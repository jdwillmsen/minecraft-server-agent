package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/protocol"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// gatedVoice is the console bridge with its first whisper held open, which
// is how this test stops a backlog exactly part-way through: the drain has
// spoken once and is about to speak again. Tell carries the context the
// real bridge POST carries, so a cancelled one fails instead of reaching
// the server -- but the bridge itself is a separate process that stays up,
// which is why nothing else about a dead connection stops the rest of a
// backlog on its own.
//
// The held call returns when its context ends, and that is the whole of the
// test's timing: the connection end reaches a delivery through a watcher
// goroutine, so a release on any other schedule would sometimes let the next
// message out before the cancel landed. resume exists only so a test that
// fails before the cancel arrives cannot leave the whisper parked forever.
type gatedVoice struct {
	*timelineVoice
	held     chan struct{}
	released chan struct{}
	resume   chan struct{}
	once     sync.Once
}

func newGatedVoice(t *testing.T, clock *scaledClock) *gatedVoice {
	v := &gatedVoice{
		timelineVoice: newTimelineVoice(clock),
		held:          make(chan struct{}),
		released:      make(chan struct{}),
		resume:        make(chan struct{}),
	}
	t.Cleanup(func() { close(v.resume) })
	return v
}

func (v *gatedVoice) Tell(ctx context.Context, xuid, message string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := v.timelineVoice.Tell(ctx, xuid, message)
	v.once.Do(func() {
		close(v.held)
		select {
		case <-ctx.Done():
		case <-v.resume:
		}
		close(v.released)
	})
	return err
}

// waitHeld blocks until the held whisper is in flight, so the connection is
// dropped while a send is genuinely under way rather than before or after
// one.
func (v *gatedVoice) waitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-v.held:
	case <-time.After(10 * time.Second):
		t.Fatalf("no whisper was ever attempted: %s", formatTimeline(v.spoken(), time.Time{}))
	}
}

// waitReleased blocks until the held whisper has returned, which nothing but
// the connection ending does while the test is running. Once it has, the
// cancel the drain reads is already in effect, so whether the rest of the
// backlog is spoken stops being a question of how fast this machine is.
func (v *gatedVoice) waitReleased(t *testing.T) {
	t.Helper()
	select {
	case <-v.released:
	case <-time.After(10 * time.Second):
		t.Fatalf("the end of the connection never reached the whisper in flight: %s", formatTimeline(v.spoken(), time.Time{}))
	}
}

// interruption is the agent wired as main() wires it -- roster, join clock,
// deliverer, welcome and announce-drain plugins over one event bus -- with
// a three-message backlog and a bridge that can be held mid-send. The
// scaled clock and the plugin delays are the loss scenario's, so the
// deliverer still reads the production distances between the arrival, the
// greeting and the drain.
type interruption struct {
	clock   *scaledClock
	joins   *joinTimes
	backlog *backlogStore
	voice   *gatedVoice
	bus     *bus.Bus
	roster  *roster.Roster
	log     *logging.Logger
	drainer *announce.Deliverer
}

func newInterruption(t *testing.T, ctx context.Context, joinedAt time.Time) *interruption {
	t.Helper()

	clock := newScaledClock(joinedAt)
	joins := newJoinTimes()
	joins.now = clock.now
	voice := newGatedVoice(t, clock)
	playerRoster := roster.New()
	audience := newDeliveryAudience(playerRoster, siblingBotXUIDs())
	audience.beginSession(selfXUID)
	backlog := &backlogStore{wrote: make(chan struct{}, 32), pending: []announce.Announcement{
		{ID: 2, Body: "announcement 2: the nether hub is open", TargetKind: announce.TargetPlayer, TargetValue: playerXUID, Priority: announce.PriorityNormal},
		{ID: 3, Body: "announcement 3: back up your builds", TargetKind: announce.TargetPlayer, TargetValue: playerXUID, Priority: announce.PriorityNormal},
		{ID: 4, Body: "announcement 4: spawn is being rebuilt", TargetKind: announce.TargetPlayer, TargetValue: playerXUID, Priority: announce.PriorityNormal},
	}}
	log := logging.New("info")

	deliverer := announce.NewDeliverer(backlog, voice, audience, announcePermissions{resolver: fakePermResolver(t, nil)}, log,
		announce.WithFreshJoinGrace(joins, freshJoinGrace))

	registry := plugin.NewRegistry()
	for _, p := range []plugin.Plugin{
		plugins.NewWelcome(ctx, scenarioWelcomeDelay, log),
		plugins.NewAnnounceDrain(ctx, deliverer, scenarioDrainDelay, log,
			plugins.WithConnections(joins),
			plugins.WithJitter(func(time.Duration) time.Duration { return 0 }),
		),
	} {
		if err := registry.Register(p); err != nil {
			t.Fatalf("register %s: %v", p.Name(), err)
		}
	}
	eventBus := bus.New()
	startEventDispatch(ctx, eventBus, registry, &plugin.Context{Voice: voice, Directory: registry}, log)

	return &interruption{clock: clock, joins: joins, backlog: backlog, voice: voice, bus: eventBus, roster: playerRoster, log: log, drainer: deliverer}
}

// arrive opens a connection and delivers the opening snapshot holding
// nobody but the agent, then the player's own arrival -- the two packets
// the live server sends in that order.
func (i *interruption) arrive(t *testing.T, gamertag string) {
	t.Helper()
	i.beginWatching()
	i.apply(t, addEntry(selfXUID, "ServerAgent"))
	i.apply(t, addEntry(playerXUID, gamertag))
}

// reconnectFinding is the agent coming back up with the player already
// there: beginWatching's roster reset and connected(), then one opening
// snapshot that reports both of them, which is what makes the player
// present rather than newly arrived.
func (i *interruption) reconnectFinding(t *testing.T, gamertag string) {
	t.Helper()
	i.beginWatching()
	i.apply(t, addEntry(selfXUID, "ServerAgent"), addEntry(playerXUID, gamertag))
}

func (i *interruption) beginWatching() {
	i.roster.BeginSession(time.Now(), selfXUID)
	i.joins.connected()
}

func (i *interruption) apply(t *testing.T, entries ...protocol.PlayerListEntry) {
	t.Helper()
	handlePlayerList(context.Background(), wire(t, entries...), selfXUID, siblingBotXUIDs(), i.log, i.bus, i.roster, store.Nop{}, i.joins)
}

// publishBacklog sends all three queued announcements the way a schedule or
// an !announce does: straight through the deliverer, now.
func (i *interruption) publishBacklog(t *testing.T) {
	t.Helper()
	for _, a := range i.backlog.pending {
		if _, err := i.drainer.SendNow(context.Background(), a, a.ID); err != nil {
			t.Fatalf("SendNow(%d): %v", a.ID, err)
		}
	}
}

func (i *interruption) stillOwed(t *testing.T, at time.Time) []int64 {
	t.Helper()
	left, err := i.backlog.PendingFor(context.Background(), playerXUID, "", at)
	if err != nil {
		t.Fatalf("PendingFor: %v", err)
	}
	ids := make([]int64, 0, len(left))
	for _, a := range left {
		ids = append(ids, a.ID)
	}
	return ids
}

func recordedIDs(rows []deliveryRow) []int64 {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.id)
	}
	return ids
}

// TestABacklogInterruptedByADroppedConnectionIsNotLost is the last path by
// which a join backlog could still be lost, end to end. The connection is
// dropped between one whispered announcement and the next: the rest must
// not be spoken, must not be recorded, and must not be summarised at a
// player who is mid-reconnect -- the bridge is a separate process and stays
// up, so nothing about the drop stops them on its own. What makes this the
// same permanent loss as the original incident is the delivery row: written
// for a message the player never saw, it suppresses that message for good.
// The second half is the point of stopping: the very next connection pays
// what was left owed.
func TestABacklogInterruptedByADroppedConnectionIsNotLost(t *testing.T) {
	joinedAt := time.Date(2026, 9, 11, 22, 42, 0, 0, time.UTC)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	in := newInterruption(t, ctx, joinedAt)

	in.arrive(t, "LightKing0221")
	time.Sleep(scenarioPublishAfter)
	in.publishBacklog(t)

	// Greeting, then the first announcement, which the bridge holds.
	in.voice.waitForLines(t, 2, "the backlog never reached the player")
	in.voice.waitHeld(t)

	// The Bedrock connection dies here, exactly as runConnectLoop reports
	// it when session returns, and the held whisper is freed by that and
	// nothing else -- so it returning is this test's proof the drain has
	// already read a cancelled context. Everything the drain does next, the
	// two unsent announcements and the !inbox trailer alike, is decided
	// behind that answer rather than by how fast this machine got there.
	in.joins.disconnected()
	in.voice.waitReleased(t)

	// The drain's last act on the message that did get out: a row is what
	// stops it being sent again, so the books are only closed once it is
	// written.
	in.backlog.waitForRows(t, 1, "the whisper that went out before the drop was never recorded")

	lines := in.voice.spoken()
	t.Logf("what LightKing0221's client received before the connection dropped:%s", formatTimeline(lines, joinedAt))
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want exactly 2 (greeting + the one announcement sent before the drop):%s", len(lines), formatTimeline(lines, joinedAt))
	}
	if want := "whisper to " + playerXUID + ": announcement 2: the nether hub is open"; lines[1].text != want {
		t.Errorf("line 2 = %q, want %q", lines[1].text, want)
	}

	if got := recordedIDs(in.backlog.deliveryRows()); len(got) != 1 || got[0] != 2 {
		t.Errorf("delivery rows = %v, want only announcement 2 -- a row for anything whispered into a dead connection is the permanent loss", got)
	}
	owed := in.stillOwed(t, joinedAt)
	if len(owed) != 2 || owed[0] != 3 || owed[1] != 4 {
		t.Fatalf("still owed = %v, want announcements 3 and 4 kept for the next connection", owed)
	}

	// The player reconnects and the agent's next connection finds them in
	// its opening snapshot, which is what pays the rest.
	in.reconnectFinding(t, "LightKing0221")
	lines = in.voice.waitForLines(t, 4, "the announcements held back by the drop were never re-delivered")
	t.Logf("what LightKing0221's client received across both connections:%s", formatTimeline(lines, joinedAt))

	for n, want := range []string{
		"whisper to " + playerXUID + ": announcement 3: back up your builds",
		"whisper to " + playerXUID + ": announcement 4: spawn is being rebuilt",
	} {
		if got := lines[n+2].text; got != want {
			t.Errorf("line %d = %q, want %q", n+3, got, want)
		}
	}
	if got := recordedIDs(in.backlog.deliveryRows()); len(got) != 3 {
		t.Errorf("delivery rows = %v, want one per announcement once all three have actually been seen", got)
	}
	if owed := in.stillOwed(t, joinedAt); len(owed) != 0 {
		t.Errorf("still owed = %v after the replacement delivery, want nothing", owed)
	}
}
