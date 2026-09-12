package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"github.com/jdwillmsen/minecraft-server-agent/internal/bus"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// uuidFor gives each test player a distinct, stable UUID, the identity a
// removal record is keyed by on the wire.
func uuidFor(xuid string) [16]byte {
	return md5.Sum([]byte(xuid))
}

func addEntry(xuid, username string) protocol.PlayerListEntry {
	return protocol.PlayerListEntry{ActionType: protocol.PlayerListActionAdd, UUID: uuidFor(xuid), XUID: xuid, Username: username}
}

func removeEntry(xuid string) protocol.PlayerListEntry {
	return protocol.PlayerListEntry{ActionType: protocol.PlayerListActionRemove, UUID: uuidFor(xuid)}
}

// wire round-trips entries through gophertunnel's own PlayerList encoding,
// so handlePlayerList sees exactly the shape the live connection decodes
// rather than whatever fields a helper happened to fill in.
func wire(t *testing.T, entries ...protocol.PlayerListEntry) *packet.PlayerList {
	t.Helper()
	var buf bytes.Buffer
	(&packet.PlayerList{Entries: entries}).Marshal(protocol.NewWriter(&buf, 0))
	var decoded packet.PlayerList
	decoded.Marshal(protocol.NewReader(&buf, 0, false))
	return &decoded
}

type leaveRecorder struct {
	store.Nop
	leaves []string
}

func (s *leaveRecorder) RecordLeave(_ context.Context, xuid string, _, _ time.Time) (store.Playtime, error) {
	s.leaves = append(s.leaves, xuid)
	return store.Playtime{}, nil
}

// drainJoins collects every join already delivered to events. Publish is
// synchronous into a buffered channel, so everything handlePlayerList
// emitted is readable by the time it returns.
func drainJoins(t *testing.T, events <-chan bus.Event) []roster.JoinEvent {
	t.Helper()
	var joins []roster.JoinEvent
	for {
		select {
		case ev := <-events:
			join, ok := ev.(roster.JoinEvent)
			if !ok {
				t.Fatalf("got event %T on the join channel, want roster.JoinEvent", ev)
			}
			joins = append(joins, join)
		default:
			return joins
		}
	}
}

// TestPlayerListFlow drives the same function the live packet loop calls,
// so each case is the end-user behaviour: player list packets in, who the
// welcome plugin would end up greeting out.
func TestPlayerListFlow(t *testing.T) {
	log := logging.New("info")
	siblings := map[string]struct{}{siblingBot: {}}

	// The server reports every already-connected player as an add record
	// immediately after login. Greeting those is a welcome storm at every
	// agent restart, to players who did not just arrive.
	t.Run("opening snapshot greets nobody", func(t *testing.T) {
		eventBus := bus.New()
		events, _ := eventBus.Subscribe(roster.JoinKind, 8)
		playerRoster := roster.New()

		handlePlayerList(context.Background(), wire(t,
			addEntry(selfXUID, "Agent"),
			addEntry(playerXUID, "Steve"),
			addEntry("2535411111111111", "Alex"),
		), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, newJoinTimes())

		if joins := drainJoins(t, events); len(joins) != 0 {
			t.Errorf("got %d joins from the opening snapshot, want 0: %+v", len(joins), joins)
		}
		if name, ok := playerRoster.NameFor(playerXUID); !ok || name != "Steve" {
			t.Errorf("NameFor(player) = (%q, %v), want (Steve, true) — the snapshot must still populate the roster", name, ok)
		}
	})

	t.Run("arrival after the snapshot is published as a join", func(t *testing.T) {
		eventBus := bus.New()
		events, _ := eventBus.Subscribe(roster.JoinKind, 8)
		playerRoster := roster.New()

		handlePlayerList(context.Background(), wire(t,
			addEntry(selfXUID, "Agent"),
		), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, newJoinTimes())
		handlePlayerList(context.Background(), wire(t,
			addEntry(playerXUID, "Steve"),
		), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, newJoinTimes())

		joins := drainJoins(t, events)
		if len(joins) != 1 {
			t.Fatalf("got %d joins, want 1: %+v", len(joins), joins)
		}
		if joins[0].XUID != playerXUID || joins[0].Username != "Steve" {
			t.Errorf("join = %+v, want Steve's arrival", joins[0])
		}
	})

	t.Run("self and sibling arrivals are never published", func(t *testing.T) {
		eventBus := bus.New()
		events, _ := eventBus.Subscribe(roster.JoinKind, 8)
		playerRoster := roster.New()

		handlePlayerList(context.Background(), wire(t,
			addEntry(playerXUID, "Steve"),
		), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, newJoinTimes())
		handlePlayerList(context.Background(), wire(t,
			addEntry(selfXUID, "Agent"),
			addEntry(siblingBot, "AfkBot"),
		), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, newJoinTimes())

		if joins := drainJoins(t, events); len(joins) != 0 {
			t.Errorf("got %d joins, want 0 — the agent and its sibling bots must never be greeted: %+v", len(joins), joins)
		}
	})

	t.Run("leave then rejoin is greeted once", func(t *testing.T) {
		eventBus := bus.New()
		events, _ := eventBus.Subscribe(roster.JoinKind, 8)
		playerRoster := roster.New()
		playerStore := &leaveRecorder{}

		handlePlayerList(context.Background(), wire(t,
			addEntry(selfXUID, "Agent"),
			addEntry(playerXUID, "Steve"),
		), selfXUID, siblings, log, eventBus, playerRoster, playerStore, newJoinTimes())
		handlePlayerList(context.Background(), wire(t,
			removeEntry(playerXUID),
		), selfXUID, siblings, log, eventBus, playerRoster, playerStore, newJoinTimes())
		handlePlayerList(context.Background(), wire(t,
			addEntry(playerXUID, "Steve"),
		), selfXUID, siblings, log, eventBus, playerRoster, playerStore, newJoinTimes())

		if len(playerStore.leaves) != 1 || playerStore.leaves[0] != playerXUID {
			t.Errorf("RecordLeave calls = %v, want exactly [%s] — the departure must close Steve's session", playerStore.leaves, playerXUID)
		}
		joins := drainJoins(t, events)
		if len(joins) != 1 {
			t.Fatalf("got %d joins across leave+rejoin, want 1: %+v", len(joins), joins)
		}
		if joins[0].XUID != playerXUID {
			t.Errorf("join = %+v, want Steve's rejoin", joins[0])
		}
	})

	// A reconnect must not replay the population as arrivals, and must not
	// keep claiming a player who left while the agent was disconnected.
	t.Run("reconnect greets nobody and forgets who left in the gap", func(t *testing.T) {
		eventBus := bus.New()
		events, _ := eventBus.Subscribe(roster.JoinKind, 8)
		playerRoster := roster.New()

		handlePlayerList(context.Background(), wire(t,
			addEntry(selfXUID, "Agent"),
			addEntry(playerXUID, "Steve"),
		), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, newJoinTimes())

		playerRoster.BeginSession(time.Now())
		handlePlayerList(context.Background(), wire(t,
			addEntry(selfXUID, "Agent"),
			addEntry("2535411111111111", "Alex"),
		), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, newJoinTimes())

		if joins := drainJoins(t, events); len(joins) != 0 {
			t.Errorf("got %d joins after reconnecting, want 0: %+v", len(joins), joins)
		}
		if _, ok := playerRoster.NameFor(playerXUID); ok {
			t.Error("NameFor for a player absent from the reconnect snapshot = ok, want not-ok — a tellraw aimed at them would reach nobody")
		}
	})
}

// sessionCalls records the session writes handlePlayerList and beginWatching
// make, in order, as "method:xuid". A join opens its session in the welcome
// plugin, off the bus, so it shows up here only as a published join.
type sessionCalls struct {
	store.Nop
	calls []string
	// failOpening fails CloseOrphans and ResumeSession, the two writes a
	// connection starts with.
	failOpening bool
	closedAt    []time.Time
	leaveSince  []time.Time
}

func (s *sessionCalls) RecordLeave(_ context.Context, xuid string, since, _ time.Time) (store.Playtime, error) {
	s.calls = append(s.calls, "leave:"+xuid)
	s.leaveSince = append(s.leaveSince, since)
	return store.Playtime{}, nil
}

func (s *sessionCalls) ResumeSession(_ context.Context, xuid, _ string, _ time.Time) (bool, error) {
	s.calls = append(s.calls, "resume:"+xuid)
	if s.failOpening {
		return false, errors.New("database unreachable")
	}
	return false, nil
}

func (s *sessionCalls) CloseOrphans(_ context.Context, at time.Time) (int, error) {
	s.calls = append(s.calls, "close-orphans")
	s.closedAt = append(s.closedAt, at)
	if s.failOpening {
		return 0, errors.New("database unreachable")
	}
	return 0, nil
}

// A player seen joining before a disconnect may have left and returned
// while the agent was away, so their open session cannot be trusted to
// measure anything. The reconnect closes it before the snapshot is read,
// the snapshot opens a fresh one, and the eventual leave closes that --
// never the session from before the gap. Nobody is greeted for being in
// the snapshot.
func TestReconnectRestartsTheSessionsOfPlayersStillOnline(t *testing.T) {
	log := logging.New("info")
	siblings := map[string]struct{}{siblingBot: {}}
	eventBus := bus.New()
	events, _ := eventBus.Subscribe(roster.JoinKind, 8)
	playerRoster := roster.New()
	profiles := &sessionCalls{}

	beginWatching(context.Background(), playerRoster, profiles, newJoinTimes(), log)
	handlePlayerList(context.Background(), wire(t,
		addEntry(selfXUID, "Agent"),
	), selfXUID, siblings, log, eventBus, playerRoster, profiles, newJoinTimes())
	handlePlayerList(context.Background(), wire(t,
		addEntry(playerXUID, "Steve"),
	), selfXUID, siblings, log, eventBus, playerRoster, profiles, newJoinTimes())
	if joins := drainJoins(t, events); len(joins) != 1 {
		t.Fatalf("got %d joins before the disconnect, want Steve's", len(joins))
	}

	beginWatching(context.Background(), playerRoster, profiles, newJoinTimes(), log)
	handlePlayerList(context.Background(), wire(t,
		addEntry(selfXUID, "Agent"),
		addEntry(siblingBot, "AfkBot"),
		addEntry(playerXUID, "Steve"),
	), selfXUID, siblings, log, eventBus, playerRoster, profiles, newJoinTimes())
	handlePlayerList(context.Background(), wire(t,
		removeEntry(playerXUID),
	), selfXUID, siblings, log, eventBus, playerRoster, profiles, newJoinTimes())

	want := []string{
		"close-orphans",
		"close-orphans",
		"resume:" + playerXUID,
		"leave:" + playerXUID,
	}
	if !slices.Equal(profiles.calls, want) {
		t.Errorf("session writes = %v, want %v", profiles.calls, want)
	}
	if joins := drainJoins(t, events); len(joins) != 0 {
		t.Errorf("got %d joins from the reconnect snapshot, want 0: %+v", len(joins), joins)
	}
}

// When the close a connection starts with and the snapshot's fresh session
// both fail, the session from before the gap is still open when the player
// leaves. The leave must carry when this connection began, so the store can
// tell that session was never watched through and credit none of it.
func TestALeaveAfterFailedReconnectWritesCarriesTheConnectionStart(t *testing.T) {
	log := logging.New("info")
	siblings := map[string]struct{}{siblingBot: {}}
	eventBus := bus.New()
	playerRoster := roster.New()
	profiles := &sessionCalls{}

	beginWatching(context.Background(), playerRoster, profiles, newJoinTimes(), log)
	handlePlayerList(context.Background(), wire(t,
		addEntry(selfXUID, "Agent"),
		addEntry(playerXUID, "Steve"),
	), selfXUID, siblings, log, eventBus, playerRoster, profiles, newJoinTimes())

	profiles.failOpening = true
	beginWatching(context.Background(), playerRoster, profiles, newJoinTimes(), log)
	handlePlayerList(context.Background(), wire(t,
		addEntry(selfXUID, "Agent"),
		addEntry(playerXUID, "Steve"),
	), selfXUID, siblings, log, eventBus, playerRoster, profiles, newJoinTimes())
	profiles.failOpening = false
	handlePlayerList(context.Background(), wire(t,
		removeEntry(playerXUID),
	), selfXUID, siblings, log, eventBus, playerRoster, profiles, newJoinTimes())

	if len(profiles.closedAt) != 2 || len(profiles.leaveSince) != 1 {
		t.Fatalf("session writes = %v, want two connection starts and one leave", profiles.calls)
	}
	if got, want := profiles.leaveSince[0], profiles.closedAt[1]; !got.Equal(want) {
		t.Errorf("leave since = %v, want the reconnect's start %v", got, want)
	}
}

// One PlayerList may carry a removal and a re-add for the same player. The
// departure has to be applied before the arrival, or it erases the arrival
// the same packet just reported -- and the deliverer then reads a client
// that is mid-load as settled, which is the loss the join clock exists for.
func TestSamePacketRejoinKeepsTheArrival(t *testing.T) {
	log := logging.New("info")
	siblings := map[string]struct{}{siblingBot: {}}
	eventBus := bus.New()
	events, _ := eventBus.Subscribe(roster.JoinKind, 8)
	playerRoster := roster.New()
	joinClock := newJoinTimes()

	beginWatching(context.Background(), playerRoster, store.Nop{}, joinClock, log)
	handlePlayerList(context.Background(), wire(t,
		addEntry(selfXUID, "Agent"),
		addEntry(playerXUID, "Steve"),
	), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, joinClock)
	if _, ok := joinClock.SinceJoin(playerXUID); ok {
		t.Fatal("a player in the opening snapshot was recorded as an arrival")
	}

	handlePlayerList(context.Background(), wire(t,
		removeEntry(playerXUID),
		addEntry(playerXUID, "Steve"),
	), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, joinClock)

	if _, ok := joinClock.SinceJoin(playerXUID); !ok {
		t.Error("the rejoin's arrival was erased by the departure in the same packet")
	}
	if got := drainJoins(t, events); len(got) != 1 {
		t.Errorf("got %d joins from the rejoin packet, want 1", len(got))
	}
}

func drainPresent(t *testing.T, events <-chan bus.Event) []roster.PresentEvent {
	t.Helper()
	var present []roster.PresentEvent
	for {
		select {
		case ev := <-events:
			p, ok := ev.(roster.PresentEvent)
			if !ok {
				t.Fatalf("got event %T on the present channel, want roster.PresentEvent", ev)
			}
			present = append(present, p)
		default:
			return present
		}
	}
}

// A player the opening snapshot reports is announced as present, never as a
// join: they must not be greeted for reappearing, but whoever owes them a
// delayed delivery has to hear about them on this connection, since a
// reconnect may have left one stranded. The event carries the connection
// that reported it, so a delivery scheduled by the previous one can tell it
// has been replaced.
func TestOpeningSnapshotReportsPlayersPresentWithTheirConnection(t *testing.T) {
	log := logging.New("info")
	siblings := map[string]struct{}{siblingBot: {}}
	eventBus := bus.New()
	presentEvents, _ := eventBus.Subscribe(roster.PresentKind, 8)
	joinEvents, _ := eventBus.Subscribe(roster.JoinKind, 8)
	playerRoster := roster.New()
	joinClock := newJoinTimes()

	beginWatching(context.Background(), playerRoster, store.Nop{}, joinClock, log)
	handlePlayerList(context.Background(), wire(t,
		addEntry(selfXUID, "Agent"),
		addEntry(siblingBot, "AfkBot"),
		addEntry(playerXUID, "Steve"),
	), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, joinClock)

	present := drainPresent(t, presentEvents)
	if len(present) != 1 || present[0].XUID != playerXUID {
		t.Fatalf("present events = %+v, want only Steve: the agent and its siblings are not players", present)
	}
	first := present[0].Generation
	if got := drainJoins(t, joinEvents); len(got) != 0 {
		t.Errorf("got %d joins from the opening snapshot, want 0: nobody in it arrived", len(got))
	}
	if _, ok := joinClock.SinceJoin(playerXUID); ok {
		t.Error("the snapshot recorded an arrival: the clock must keep answering that honestly")
	}

	beginWatching(context.Background(), playerRoster, store.Nop{}, joinClock, log)
	handlePlayerList(context.Background(), wire(t,
		addEntry(selfXUID, "Agent"),
		addEntry(playerXUID, "Steve"),
	), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, joinClock)

	present = drainPresent(t, presentEvents)
	if len(present) != 1 {
		t.Fatalf("present events after the reconnect = %+v, want Steve reported again", present)
	}
	if present[0].Generation == first {
		t.Errorf("generation = %d on both connections: a delivery scheduled by the first cannot tell it was replaced", first)
	}
}

// A genuine arrival carries the connection it happened on, which is what
// lets a drain scheduled by it abandon itself if that connection ends first.
func TestAJoinCarriesItsConnection(t *testing.T) {
	log := logging.New("info")
	siblings := map[string]struct{}{siblingBot: {}}
	eventBus := bus.New()
	events, _ := eventBus.Subscribe(roster.JoinKind, 8)
	playerRoster := roster.New()
	joinClock := newJoinTimes()

	beginWatching(context.Background(), playerRoster, store.Nop{}, joinClock, log)
	handlePlayerList(context.Background(), wire(t,
		addEntry(selfXUID, "Agent"),
	), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, joinClock)
	handlePlayerList(context.Background(), wire(t,
		addEntry(playerXUID, "Steve"),
	), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, joinClock)

	joins := drainJoins(t, events)
	if len(joins) != 1 {
		t.Fatalf("got %d joins, want Steve's", len(joins))
	}
	if joins[0].Generation != joinClock.Generation() {
		t.Errorf("join generation = %d, want the live connection %d", joins[0].Generation, joinClock.Generation())
	}
}

// The mirror of the rejoin packet: one PlayerList that adds a player and
// then removes them again. They are gone by the end of it, so nothing greets
// them and nothing schedules a delivery to a client that is not there --
// Apply's two slices cannot say which record came first, but the roster it
// leaves behind can.
func TestSamePacketJoinAndLeaveGreetsNobody(t *testing.T) {
	log := logging.New("info")
	siblings := map[string]struct{}{siblingBot: {}}
	eventBus := bus.New()
	events, _ := eventBus.Subscribe(roster.JoinKind, 8)
	playerRoster := roster.New()
	joinClock := newJoinTimes()

	beginWatching(context.Background(), playerRoster, store.Nop{}, joinClock, log)
	handlePlayerList(context.Background(), wire(t,
		addEntry(selfXUID, "Agent"),
	), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, joinClock)

	handlePlayerList(context.Background(), wire(t,
		addEntry(playerXUID, "Steve"),
		removeEntry(playerXUID),
	), selfXUID, siblings, log, eventBus, playerRoster, store.Nop{}, joinClock)

	if got := drainJoins(t, events); len(got) != 0 {
		t.Errorf("got %d joins for a player who left in the same packet, want 0: %+v", len(got), got)
	}
	if _, ok := joinClock.SinceJoin(playerXUID); ok {
		t.Error("recorded an arrival for a player who is gone")
	}
}
