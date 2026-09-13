package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugins"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// The reported incident, in the timings it was reported in: a player joined
// at 22:42:00, two queued announcements were whispered and recorded 0.812s
// later, and the player saw neither -- their client was still loading, and
// the delivery rows stopped anything from ever retrying.
//
// Reproduced at a tenth of the clock: scenarioScale maps one real
// millisecond onto ten of the deliverer's, so the plugins' own waits stay
// short while the join clock the deliverer reads still reports the
// production distances -- 0.8s from the arrival to the publish, 5s to the
// greeting, 8s to the drain, against the real freshJoinGrace of 7s. Scaling
// the clock rather than stepping it by hand also means a slow machine only
// ever reports *more* elapsed time to the deliverer, never less.
const scenarioScale = 10

const (
	scenarioPublishAfter = 80 * time.Millisecond  // 0.8s: when the announcements landed
	scenarioWelcomeDelay = 500 * time.Millisecond // welcomeDelay
	scenarioDrainDelay   = 800 * time.Millisecond // announceDrainDelay
)

// scaledClock reports a fixed start plus scenarioScale times however long
// this test has really been running.
type scaledClock struct {
	start   time.Time
	realRef time.Time
}

func newScaledClock(start time.Time) *scaledClock {
	return &scaledClock{start: start, realRef: time.Now()}
}

func (c *scaledClock) now() time.Time {
	return c.start.Add(time.Since(c.realRef) * scenarioScale)
}

// backlogStore is an announce.Store over one fixed backlog. PendingFor
// reports only what has no delivery row yet, which is what makes a row here
// the same permanent suppression it is in Postgres: once written, that
// message is never handed to that player again.
type backlogStore struct {
	mu      sync.Mutex
	pending []announce.Announcement
	rows    []deliveryRow
}

type deliveryRow struct {
	id   int64
	xuid string
	at   time.Time
}

var _ announce.Store = (*backlogStore)(nil)

func (s *backlogStore) Enabled() bool { return true }

func (s *backlogStore) Insert(context.Context, announce.Announcement) (int64, error) {
	return 0, nil
}

func (s *backlogStore) PendingFor(_ context.Context, xuid, _ string, _ time.Time) ([]announce.Announcement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []announce.Announcement
	for _, a := range s.pending {
		delivered := false
		for _, r := range s.rows {
			if r.id == a.ID && r.xuid == xuid {
				delivered = true
			}
		}
		if !delivered {
			out = append(out, a)
		}
	}
	return out, nil
}

func (s *backlogStore) MarkDelivered(_ context.Context, id int64, xuid string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, deliveryRow{id: id, xuid: xuid, at: at})
	return nil
}

func (s *backlogStore) deliveryRows() []deliveryRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]deliveryRow(nil), s.rows...)
}

// timelineVoice is the console bridge as the player experiences it: every
// whisper and broadcast, in order, stamped with when the server thought it
// said it.
type timelineVoice struct {
	mu    sync.Mutex
	clock *scaledClock
	lines []spoken
	said  chan struct{}
}

type spoken struct {
	at   time.Time
	text string
}

func newTimelineVoice(clock *scaledClock) *timelineVoice {
	return &timelineVoice{clock: clock, said: make(chan struct{}, 32)}
}

func (v *timelineVoice) Tell(_ context.Context, xuid, message string) error {
	return v.record(fmt.Sprintf("whisper to %s: %s", xuid, message))
}

func (v *timelineVoice) Say(_ context.Context, message string) error {
	return v.record("chat (everyone): " + message)
}

func (v *timelineVoice) record(text string) error {
	v.mu.Lock()
	v.lines = append(v.lines, spoken{at: v.clock.now(), text: text})
	v.mu.Unlock()
	select {
	case v.said <- struct{}{}:
	default:
	}
	return nil
}

func (v *timelineVoice) spoken() []spoken {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]spoken(nil), v.lines...)
}

// waitForLines blocks until the voice has said at least n things, so the
// test orders itself against the plugins' background goroutines rather than
// sleeping and hoping.
func (v *timelineVoice) waitForLines(t *testing.T, n int, why string) []spoken {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		if lines := v.spoken(); len(lines) >= n {
			return lines
		}
		select {
		case <-v.said:
		case <-deadline:
			t.Fatalf("%s: only %d lines were ever spoken: %s", why, len(v.spoken()), formatTimeline(v.spoken(), time.Time{}))
			return nil
		}
	}
}

// formatTimeline renders what the player's client received, as a reviewer
// reads a chat log: seconds since the arrival, then the line.
func formatTimeline(lines []spoken, joinedAt time.Time) string {
	out := ""
	for _, l := range lines {
		if joinedAt.IsZero() {
			out += fmt.Sprintf("\n    %s  %s", l.at.Format("15:04:05.000"), l.text)
			continue
		}
		out += fmt.Sprintf("\n    %s (join+%5.2fs)  %s", l.at.Format("15:04:05.000"), l.at.Sub(joinedAt).Seconds(), l.text)
	}
	return out
}

// incident is the agent wired the way main() wires it -- roster, join clock,
// deliverer, welcome and announce-drain plugins over one event bus -- with
// the packet loop's own entry point, handlePlayerList, as the only way a
// join gets in.
type incident struct {
	clock   *scaledClock
	joins   *joinTimes
	backlog *backlogStore
	voice   *timelineVoice
	bus     *bus.Bus
	roster  *roster.Roster
	log     *logging.Logger
	drainer *announce.Deliverer
}

// grace is the deliverer's freshJoinGrace; zero leaves the join clock
// unwired, which is the agent as it behaved when the incident was reported.
func newIncident(t *testing.T, ctx context.Context, joinedAt time.Time, grace time.Duration) *incident {
	t.Helper()

	clock := newScaledClock(joinedAt)
	joins := newJoinTimes()
	joins.now = clock.now
	voice := newTimelineVoice(clock)
	playerRoster := roster.New()
	audience := newDeliveryAudience(playerRoster, siblingBotXUIDs())
	audience.beginSession(selfXUID)
	backlog := &backlogStore{pending: []announce.Announcement{
		{ID: 2, Body: "announcement 2: the nether hub is open", TargetKind: announce.TargetPlayer, TargetValue: playerXUID, Priority: announce.PriorityNormal},
		{ID: 3, Body: "announcement 3: back up your builds", TargetKind: announce.TargetPlayer, TargetValue: playerXUID, Priority: announce.PriorityNormal},
	}}
	log := logging.New("info")

	var opts []announce.Option
	if grace > 0 {
		opts = append(opts, announce.WithFreshJoinGrace(joins, grace))
	}
	deliverer := announce.NewDeliverer(backlog, voice, audience, announcePermissions{resolver: fakePermResolver(t, nil)}, log, opts...)

	registry := plugin.NewRegistry()
	for _, p := range []plugin.Plugin{
		plugins.NewWelcome(ctx, scenarioWelcomeDelay, log),
		plugins.NewAnnounceDrain(ctx, deliverer, scenarioDrainDelay, log,
			plugins.WithConnections(joins),
			// Fixed so the transcript is the same every run; production
			// spreads a snapshot's worth of drains at random.
			plugins.WithJitter(func(time.Duration) time.Duration { return 0 }),
		),
	} {
		if err := registry.Register(p); err != nil {
			t.Fatalf("register %s: %v", p.Name(), err)
		}
	}
	eventBus := bus.New()
	startEventDispatch(ctx, eventBus, registry, &plugin.Context{Voice: voice, Directory: registry}, log)

	return &incident{clock: clock, joins: joins, backlog: backlog, voice: voice, bus: eventBus, roster: playerRoster, log: log, drainer: deliverer}
}

// connect opens a connection and delivers the opening snapshot, which holds
// nobody but the agent, then the arrival itself -- the two packets the live
// server sends in that order.
func (i *incident) connect(t *testing.T, gamertag string) {
	t.Helper()
	i.joins.connected()
	handlePlayerList(context.Background(), wire(t, addEntry(selfXUID, "ServerAgent")), selfXUID, siblingBotXUIDs(), i.log, i.bus, i.roster, store.Nop{}, i.joins)
	handlePlayerList(context.Background(), wire(t, addEntry(playerXUID, gamertag)), selfXUID, siblingBotXUIDs(), i.log, i.bus, i.roster, store.Nop{}, i.joins)
}

// publishBacklog sends both queued announcements the way a schedule or an
// !announce does: straight through the deliverer, now, at whatever moment
// they happen to land in.
func (i *incident) publishBacklog(t *testing.T) {
	t.Helper()
	for _, a := range i.backlog.pending {
		if _, err := i.drainer.SendNow(context.Background(), a, a.ID); err != nil {
			t.Fatalf("SendNow(%d): %v", a.ID, err)
		}
	}
}

// TestAFreshArrivalsBacklogSurvivesTheJoinMoment is the reported loss end to
// end: the announcements that went missing are instead held back while the
// client loads, and reach the player after the greeting, still recorded exactly
// once. The second case is the incident as reported, reproduced by taking
// the join clock away -- the whisper goes out into a loading client and the
// row is written, so the message is gone for good.
func TestAFreshArrivalsBacklogSurvivesTheJoinMoment(t *testing.T) {
	joinedAt := time.Date(2026, 9, 11, 22, 42, 0, 0, time.UTC)

	t.Run("held back while the client loads, then whispered after the greeting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		in := newIncident(t, ctx, joinedAt, freshJoinGrace)

		in.connect(t, "LightKing0221")
		time.Sleep(scenarioPublishAfter)
		in.publishBacklog(t)

		if rows := in.backlog.deliveryRows(); len(rows) != 0 {
			t.Fatalf("wrote %d delivery rows in the join moment, want 0 -- a row here is the permanent loss: %+v", len(rows), rows)
		}

		// Greeting first, then the backlog: three lines is the whole
		// transcript, and the drain wakes a scaled 8s after the arrival.
		lines := in.voice.waitForLines(t, 3, "the backlog never reached the player")
		t.Logf("what LightKing0221's client received:%s", formatTimeline(lines, joinedAt))

		if len(lines) != 3 {
			t.Fatalf("got %d lines, want exactly 3 (greeting + two announcements):%s", len(lines), formatTimeline(lines, joinedAt))
		}
		if want := "chat (everyone): Welcome, LightKing0221!"; lines[0].text != want {
			t.Errorf("line 1 = %q, want the greeting %q -- the greeting owns the join moment", lines[0].text, want)
		}
		for n, want := range []string{
			"whisper to " + playerXUID + ": announcement 2: the nether hub is open",
			"whisper to " + playerXUID + ": announcement 3: back up your builds",
		} {
			if got := lines[n+1].text; got != want {
				t.Errorf("line %d = %q, want %q", n+2, got, want)
			}
		}
		if since := lines[1].at.Sub(joinedAt); since < freshJoinGrace {
			t.Errorf("the backlog was whispered %s after the arrival, inside the %s grace: that client is still loading", since, freshJoinGrace)
		}

		rows := in.backlog.deliveryRows()
		if len(rows) != 2 || rows[0].id != 2 || rows[1].id != 3 || rows[0].xuid != playerXUID {
			t.Errorf("delivery rows = %+v, want one per announcement, both for the player", rows)
		}
		left, err := in.backlog.PendingFor(ctx, playerXUID, "", joinedAt)
		if err != nil {
			t.Fatalf("PendingFor: %v", err)
		}
		if len(left) != 0 {
			t.Errorf("%d announcements still owed after the drain, want 0", len(left))
		}
	})

	t.Run("the reported incident: no join clock, so the whisper is spent on a loading client", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		in := newIncident(t, ctx, joinedAt, 0)

		in.connect(t, "LightKing0221")
		time.Sleep(scenarioPublishAfter)
		in.publishBacklog(t)

		lines := in.voice.waitForLines(t, 2, "the announcements were not even attempted")
		t.Logf("what LightKing0221's client received (pre-fix wiring):%s", formatTimeline(lines, joinedAt))
		if since := lines[0].at.Sub(joinedAt); since >= freshJoinGrace {
			t.Fatalf("the first whisper landed %s after the arrival: this case only reproduces the incident inside the %s grace", since, freshJoinGrace)
		}
		if rows := in.backlog.deliveryRows(); len(rows) != 2 {
			t.Fatalf("delivery rows = %+v, want both written at the join moment -- that is the loss being reproduced", rows)
		}

		// The drain wakes a scaled 8s later and finds nothing owed: the
		// rows written above have suppressed both messages for good.
		time.Sleep(scenarioDrainDelay + 200*time.Millisecond)
		final := in.voice.spoken()
		t.Logf("after the drain woke (pre-fix wiring):%s", formatTimeline(final, joinedAt))
		for _, l := range final[2:] {
			if l.text == lines[0].text || l.text == lines[1].text {
				t.Errorf("an announcement was whispered a second time: %q", l.text)
			}
		}
		left, err := in.backlog.PendingFor(ctx, playerXUID, "", joinedAt)
		if err != nil {
			t.Fatalf("PendingFor: %v", err)
		}
		if len(left) != 0 {
			t.Fatalf("%d announcements still pending, want 0: the incident is that the rows were written, so nothing retries", len(left))
		}
	})
}
