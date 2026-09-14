package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/protocol"

	"github.com/jdwillmsen/minecraft-server-agent/internal/announce"
	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/httpapi"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// gapClock is the join clock's sense of time, advanced by hand. The publish
// this test cares about has to land well past freshJoinGrace, or the
// deliverer withholds it for a client it believes is still loading and the
// test proves nothing about the roster; waiting that out in real time would
// be slower and no more certain.
type gapClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *gapClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *gapClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// connectionGap is the agent wired as main() wires it for delivery -- roster,
// the bot filter over it, join clock, deliverer and a one-message backlog --
// reaching the dead-connection state through connectionEnded, the same call
// runConnectLoop makes when session returns.
//
// No plugins are registered: the join drain and the greeting answer bus
// events on their own timers, and what is under test here is what a publish
// does in the gap. The drain's turn is taken explicitly at the end, which is
// what the plugin does once its wait is up.
type connectionGap struct {
	clock    *gapClock
	joins    *joinTimes
	backlog  *backlogStore
	voice    *timelineVoice
	bus      *bus.Bus
	roster   *roster.Roster
	audience *deliveryAudience
	log      *logging.Logger
	drainer  *announce.Deliverer
}

// gapAnnouncementID is the one queued announcement this test follows across
// the gap, whispered to the player because it is addressed to them.
const gapAnnouncementID = 9

func newConnectionGap(t *testing.T, at time.Time) *connectionGap {
	t.Helper()

	clock := &gapClock{at: at}
	joins := newJoinTimes()
	joins.now = clock.now
	voice := newTimelineVoice(newScaledClock(at))
	playerRoster := roster.New()
	audience := newDeliveryAudience(playerRoster, siblingBotXUIDs())
	audience.beginSession(selfXUID)
	backlog := &backlogStore{pending: []announce.Announcement{
		{ID: gapAnnouncementID, Body: "announcement 9: the nether hub is open", TargetKind: announce.TargetPlayer, TargetValue: playerXUID, Priority: announce.PriorityNormal},
	}}
	log := logging.New("info")

	deliverer := announce.NewDeliverer(backlog, voice, audience, announcePermissions{resolver: fakePermResolver(t, nil)}, log,
		announce.WithFreshJoinGrace(joins, freshJoinGrace))

	return &connectionGap{
		clock: clock, joins: joins, backlog: backlog, voice: voice,
		bus: bus.New(), roster: playerRoster, audience: audience, log: log, drainer: deliverer,
	}
}

// arrive opens a connection and delivers the opening snapshot holding nobody
// but the agent, then the player's own arrival -- the two packets the live
// server sends in that order.
func (g *connectionGap) arrive(t *testing.T, gamertag string) {
	t.Helper()
	g.beginWatching()
	g.apply(t, addEntry(selfXUID, "ServerAgent"))
	g.apply(t, addEntry(playerXUID, gamertag))
}

// reconnectFinding is the next connection coming up with the player already
// there: one opening snapshot reporting both of them, which makes the player
// present rather than newly arrived.
func (g *connectionGap) reconnectFinding(t *testing.T, gamertag string) {
	t.Helper()
	g.beginWatching()
	g.apply(t, addEntry(selfXUID, "ServerAgent"), addEntry(playerXUID, gamertag))
}

func (g *connectionGap) beginWatching() {
	g.roster.BeginSession(g.clock.now(), selfXUID)
	g.joins.connected()
}

func (g *connectionGap) apply(t *testing.T, entries ...protocol.PlayerListEntry) {
	t.Helper()
	handlePlayerList(context.Background(), wire(t, entries...), selfXUID, siblingBotXUIDs(), g.log, g.bus, g.roster, store.Nop{}, g.joins)
}

// quit is the player leaving: one removal record, the shape the live server
// sends.
func (g *connectionGap) quit(t *testing.T) {
	t.Helper()
	g.apply(t, removeEntry(playerXUID))
}

// publish sends the queued announcement the way a schedule, an event source
// or the HTTP API does: straight through the deliverer, now. Those three run
// for the process rather than for a session, which is how a publish reaches
// the gap at all.
func (g *connectionGap) publish(t *testing.T) {
	t.Helper()
	a := g.backlog.pending[0]
	if _, err := g.drainer.SendNow(context.Background(), a, a.ID); err != nil {
		t.Fatalf("SendNow(%d): %v", a.ID, err)
	}
}

func (g *connectionGap) stillOwed(t *testing.T) []int64 {
	t.Helper()
	left, err := g.backlog.PendingFor(context.Background(), playerXUID, "", g.clock.now())
	if err != nil {
		t.Fatalf("PendingFor: %v", err)
	}
	ids := make([]int64, 0, len(left))
	for _, a := range left {
		ids = append(ids, a.ID)
	}
	return ids
}

// TestAPublishInTheConnectionGapIsNotRecordedAgainstWhoWasThere is
// JDWLABS-543 end to end. A player is watched through one connection, the
// connection dies, and a publish lands in the gap before the next one opens.
// Nobody is being watched then, so that announcement must reach nobody and be
// recorded against nobody: the bridge is a separate process and still
// answers, so a whisper sent now is accepted by a server the player may
// already have left, and the delivery row is the permanent loss -- nothing
// retries a recorded delivery. The second half is the point of holding it:
// the very next connection pays it.
//
// This test fails if connectionEnded stops calling playerRoster.EndSession().
// The roster then still names the player for the whole gap, so the publish
// finds them "online", whispers into the dead connection and writes the row,
// and the announcement is gone -- which is the incident, not a near miss.
func TestAPublishInTheConnectionGapIsNotRecordedAgainstWhoWasThere(t *testing.T) {
	at := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	g := newConnectionGap(t, at)

	g.arrive(t, "LightKing0221")
	// Settled in: past the grace, so nothing but the dead connection can
	// hold this delivery back.
	g.clock.advance(freshJoinGrace + time.Minute)
	if online := g.audience.Online(); len(online) != 1 || online[0] != playerXUID {
		t.Fatalf("audience = %v while connected, want just the player -- this test proves nothing if they were never there", online)
	}

	connectionEnded(g.roster, g.joins)

	g.publish(t)

	if lines := g.voice.spoken(); len(lines) != 0 {
		t.Errorf("spoke into the gap, want silence:%s", formatTimeline(lines, at))
	}
	if rows := g.backlog.deliveryRows(); len(rows) != 0 {
		t.Errorf("delivery rows = %+v, want none -- a row written in the gap suppresses that announcement for good", rows)
	}
	if owed := g.stillOwed(t); len(owed) != 1 || owed[0] != gapAnnouncementID {
		t.Fatalf("still owed = %v, want announcement %d kept for the connection that follows", owed, gapAnnouncementID)
	}

	// The agent reconnects and finds the player in its opening snapshot,
	// which is what schedules their drain. Its wait is taken on the clock
	// rather than slept, then the drain itself is run the way the plugin runs
	// it when it wakes.
	g.reconnectFinding(t, "LightKing0221")
	g.clock.advance(announceDrainDelay + time.Second)
	delivered, remaining, err := g.drainer.DrainForJoin(context.Background(), playerXUID, g.clock.now())
	if err != nil {
		t.Fatalf("DrainForJoin: %v", err)
	}
	if delivered != 1 || remaining != 0 {
		t.Fatalf("drain delivered %d with %d remaining, want 1 and 0 -- the announcement held back by the gap must be paid by the next connection", delivered, remaining)
	}

	lines := g.voice.spoken()
	t.Logf("what LightKing0221's client received across the gap:%s", formatTimeline(lines, at))
	want := "whisper to " + playerXUID + ": announcement 9: the nether hub is open"
	if len(lines) != 1 || lines[0].text != want {
		t.Errorf("transcript = %+v, want exactly one line %q", lines, want)
	}
	if got := recordedIDs(g.backlog.deliveryRows()); len(got) != 1 || got[0] != gapAnnouncementID {
		t.Errorf("delivery rows = %v, want one for announcement %d, written only once it was actually seen", got, gapAnnouncementID)
	}
	if owed := g.stillOwed(t); len(owed) != 0 {
		t.Errorf("still owed = %v after the replacement delivery, want nothing", owed)
	}
}

// TestADrainForAPlayerWhoQuitIsNotWhisperedOrRecorded is the other end of
// the same permanent loss, inside a live session rather than across a gap. A
// drain is scheduled by an arrival and fires seconds later; a player who
// quits inside that wait is gone, and mc-console-bridge answers a tellraw
// that matches nobody with success, so the backlog would be recorded as
// delivered and never offered again.
//
// It fails if the announcement whisper paths take a resolvable gamertag as
// evidence of presence. Names are deliberately kept after a player leaves so
// a reply already in flight can still be addressed -- which the second half
// of this test holds to -- and that is exactly why presence has to be asked
// of the roster's online list instead.
func TestADrainForAPlayerWhoQuitIsNotWhisperedOrRecorded(t *testing.T) {
	at := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	g := newConnectionGap(t, at)

	g.arrive(t, "LightKing0221")
	// Their drain is scheduled here, a full wait ahead of now.
	g.quit(t)
	g.clock.advance(announceDrainDelay + time.Second)

	delivered, remaining, err := g.drainer.DrainForJoin(context.Background(), playerXUID, g.clock.now())
	if err != nil {
		t.Fatalf("DrainForJoin: %v", err)
	}
	if lines := g.voice.spoken(); len(lines) != 0 {
		t.Errorf("whispered to a player who has quit:%s", formatTimeline(lines, at))
	}
	if rows := g.backlog.deliveryRows(); len(rows) != 0 {
		t.Errorf("delivery rows = %+v, want none — a recorded delivery is never offered again", rows)
	}
	if delivered != 0 || remaining != 1 {
		t.Errorf("DrainForJoin = (%d, %d), want (0, 1) — the announcement is still owed", delivered, remaining)
	}
	if owed := g.stillOwed(t); len(owed) != 1 || owed[0] != gapAnnouncementID {
		t.Fatalf("still owed = %v, want announcement %d kept for their next join", owed, gapAnnouncementID)
	}

	// The half that has to keep working: the same player is still nameable,
	// so an @server answer that was still being written when they left is
	// addressed rather than thrown away.
	if name, ok := g.roster.NameFor(playerXUID); !ok || name != "LightKing0221" {
		t.Errorf("NameFor after they quit = (%q, %v), want (LightKing0221, true)", name, ok)
	}
	if g.roster.IsOnline(playerXUID) {
		t.Error("IsOnline after they quit = true, want false")
	}
}

// TestAProcessThatHasNotTakenTheLockDoesNotBroadcast wires the Deliverer's
// leadership signal to a real HTTP server the way main does, and publishes
// through both states that are not leadership -- still starting, then
// waiting for the lock. The announcement API is mounted for the whole
// process and answers through both, so a pod that is in no game can be asked
// to speak into one.
func TestAProcessThatHasNotTakenTheLockDoesNotBroadcast(t *testing.T) {
	srv, err := httpapi.New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	at := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	voice := newTimelineVoice(newScaledClock(at))
	backlog := &backlogStore{}
	playerRoster := roster.New()
	joins := newJoinTimes()
	d := announce.NewDeliverer(backlog, voice,
		newDeliveryAudience(playerRoster, siblingBotXUIDs()),
		announcePermissions{resolver: fakePermResolver(t, nil)}, logging.New("info"),
		announce.WithFreshJoinGrace(joins, freshJoinGrace),
		announce.WithLeadership(srv))

	a := announce.Announcement{ID: 1, Body: "server restarting in 5 minutes", TargetKind: announce.TargetOnlineOnly}
	if _, err := d.SendNow(context.Background(), a, a.ID); err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	if lines := voice.spoken(); len(lines) != 0 {
		t.Errorf("spoke while still starting, want silence:%s", formatTimeline(lines, at))
	}

	// Startup paid, waiting for the lock: the pod is ready to be rolled onto
	// and still must not speak into the game the leader is playing.
	srv.SetRole(httpapi.RoleStandby)
	if _, err := d.SendNow(context.Background(), a, a.ID); err != nil {
		t.Fatalf("SendNow as a standby: %v", err)
	}
	if lines := voice.spoken(); len(lines) != 0 {
		t.Errorf("spoke as a standby, want silence:%s", formatTimeline(lines, at))
	}

	// Once it is the live agent, the same publish in the same gap is heard.
	srv.SetRole(httpapi.RoleLive)
	if _, err := d.SendNow(context.Background(), a, a.ID); err != nil {
		t.Fatalf("SendNow after taking the lock: %v", err)
	}
	if lines := voice.spoken(); len(lines) != 1 {
		t.Errorf("lines = %+v, want exactly one once this process holds the lock", lines)
	}
}

// broadcastNow publishes an everyone-targeted announcement through the
// deliverer, the way a schedule or the HTTP API does, and reports whether the
// server was spoken to.
func (g *connectionGap) broadcastNow(t *testing.T, body string) bool {
	t.Helper()
	before := len(g.voice.spoken())
	a := announce.Announcement{ID: 99, Body: body, TargetKind: announce.TargetOnlineOnly}
	if _, err := g.drainer.SendNow(context.Background(), a, a.ID); err != nil {
		t.Fatalf("SendNow: %v", err)
	}
	return len(g.voice.spoken()) > before
}

// TestABlindBroadcastTellsNotKnownApartFromNobody walks the three states an
// empty roster can mean, which the deliverer must not treat alike. Two of
// them are "who is here is not known", where the console bridge is a separate
// process that still reaches the server and an online-only announcement has
// no second chance; the third is a roster that has been told who is here and
// names nobody, where the message would be a console line no player could
// hear.
//
// This test fails if the not-known states are read from the connection alone.
// The agent is connected the instant it begins watching, a full packet before
// its first roster arrives, so a publish landing there would be suppressed as
// an idle server while the world may be full.
func TestABlindBroadcastTellsNotKnownApartFromNobody(t *testing.T) {
	at := time.Date(2026, 9, 13, 9, 0, 0, 0, time.UTC)
	g := newConnectionGap(t, at)

	g.arrive(t, "LightKing0221")
	g.clock.advance(freshJoinGrace + time.Minute)
	connectionEnded(g.roster, g.joins)

	if !g.broadcastNow(t, "one: the gap between connections") {
		t.Error("silent in the gap — an online-only announcement has no second chance")
	}

	// Connected, watching, and not yet told: BeginSession has emptied the
	// roster and the opening PlayerList has not arrived.
	g.beginWatching()
	if !g.broadcastNow(t, "two: connected, before the first roster packet") {
		t.Error("silent before the opening roster arrived — the server may be full and the agent simply not told yet")
	}

	// Told, and the only name on it is the agent's own, which the audience
	// filters out: genuinely nobody to hear it.
	g.apply(t, addEntry(selfXUID, "ServerAgent"))
	if online := g.audience.Online(); len(online) != 0 {
		t.Fatalf("audience = %v, want nobody — this case proves nothing if someone is on", online)
	}
	if g.broadcastNow(t, "three: watching an empty server") {
		t.Error("spoke to an empty server the agent is watching — the roster is right, there is nobody there")
	}
}
