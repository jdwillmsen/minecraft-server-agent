package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
)

// fakeBridgeFeed is the console bridge as the roster follower reads it: a
// log of numbered events that starts again when the bridge restarts, and
// whatever `list` currently answers.
type fakeBridgeFeed struct {
	mu      sync.Mutex
	nextID  int64
	events  []adapters.BridgeEvent
	online  []string
	listErr error
	lists   int
}

var _ bridgeFeed = (*fakeBridgeFeed)(nil)

func (f *fakeBridgeFeed) Events(_ context.Context, since int64) ([]adapters.BridgeEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []adapters.BridgeEvent
	for _, e := range f.events {
		if e.ID > since {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeBridgeFeed) OnlinePlayers(context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]string{}, f.online...), nil
}

// logged appends a connect or disconnect line the way Bedrock prints it.
func (f *fakeBridgeFeed) logged(kind, name, xuid string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log(kind, name, xuid)
}

func (f *fakeBridgeFeed) log(kind, name, xuid string) {
	verb := "connected"
	if kind == adapters.BridgeEventDisconnect {
		verb = "disconnected"
	}
	f.nextID++
	f.events = append(f.events, adapters.BridgeEvent{
		ID:     f.nextID,
		Type:   kind,
		Player: name,
		Raw:    "[2026-09-23 10:00:00:000 INFO] Player " + verb + ": " + name + ", xuid: " + xuid,
	})
}

func (f *fakeBridgeFeed) listAnswers(names []string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.online, f.listErr = names, err
}

// restart is the bridge process coming back: its event IDs begin again.
func (f *fakeBridgeFeed) restart(online ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID, f.events, f.online = 0, nil, online
}

// evictAndLog is the bridge's buffer filling while the follower is behind:
// every event it holds is dropped, the IDs carry on from where they were,
// and a connect lands after the eviction. `list` answers online.
func (f *fakeBridgeFeed) evictAndLog(name, xuid string, online ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events, f.online = nil, online
	f.log(adapters.BridgeEventConnect, name, xuid)
}

func (f *fakeBridgeFeed) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

type runningBridgeRoster struct {
	roster *roster.Roster
	joins  *joinTimes
	stop   func()
}

// startBridgeRoster runs a follower on fast timings until the test stops it
// or ends.
func startBridgeRoster(t *testing.T, feed bridgeFeed, archive nameArchive, reseed time.Duration) runningBridgeRoster {
	t.Helper()
	return startBridgeRosterWithJoins(t, feed, archive, reseed, newJoinTimes())
}

// startBridgeRosterWithJoins is startBridgeRoster with a join clock the test
// has already set up, such as one reading a controlled time.
func startBridgeRosterWithJoins(t *testing.T, feed bridgeFeed, archive nameArchive, reseed time.Duration, j *joinTimes) runningBridgeRoster {
	t.Helper()
	r := roster.New()
	b := newBridgeRoster(feed, r, j, archive, quiet())
	b.poll = time.Millisecond
	b.reseed = reseed
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.run(ctx)
	}()
	stop := func() {
		cancel()
		<-done
	}
	t.Cleanup(stop)
	return runningBridgeRoster{roster: r, joins: j, stop: stop}
}

func TestBridgeRosterSeedsFromListResolvingXUIDs(t *testing.T) {
	feed := &fakeBridgeFeed{}
	// Steve's XUID is in the bridge's own backlog, Alex's only in the
	// profile store, and Sam's nowhere.
	feed.logged(adapters.BridgeEventConnect, "Steve", "111")
	feed.listAnswers([]string{"Steve", "Alex", "Sam"}, nil)
	got := startBridgeRoster(t, feed, recordedNames{byName: map[string]string{"Alex": "222"}}, time.Hour)

	waitUntil(t, got.roster.Knows, "the roster never learned who is online")
	if !got.roster.IsOnline("111") || !got.roster.IsOnline("222") {
		t.Errorf("Online() = %v, want Steve (111) and Alex (222)", got.roster.Online())
	}
	if n := len(got.roster.Online()); n != 2 {
		t.Errorf("Online() holds %d players, want 2: a player with no known XUID cannot be keyed", n)
	}
	if _, ok := got.joins.SinceConnect(); !ok {
		t.Error("SinceConnect() reports no connection: the seed must stand in for everyone it found")
	}
}

func TestBridgeRosterFollowsJoinsAndLeaves(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.listAnswers(nil, nil)
	got := startBridgeRoster(t, feed, recordedNames{}, time.Hour)
	waitUntil(t, got.roster.Knows, "the roster never learned who is online")

	feed.logged(adapters.BridgeEventConnect, "Alex", "222")
	waitUntil(t, func() bool { return got.roster.IsOnline("222") }, "a connect line never put Alex on the roster")
	if _, ok := got.joins.SinceJoin("222"); !ok {
		t.Error("no arrival recorded for Alex: an announcement could be recorded against a client still loading")
	}

	feed.logged(adapters.BridgeEventDisconnect, "Alex", "222")
	waitUntil(t, func() bool { return !got.roster.IsOnline("222") }, "a disconnect line never took Alex off the roster")
	if _, ok := got.joins.SinceJoin("222"); ok {
		t.Error("Alex's arrival is still remembered after they left")
	}
}

func TestBridgeRosterReseedsWhenTheBridgeRestarts(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.logged(adapters.BridgeEventConnect, "Steve", "111")
	feed.listAnswers([]string{"Steve"}, nil)
	got := startBridgeRoster(t, feed, recordedNames{byName: map[string]string{"Alex": "222"}}, time.Hour)
	waitUntil(t, func() bool { return got.roster.IsOnline("111") }, "the first seed never landed")

	feed.restart("Alex")

	waitUntil(t, func() bool {
		return got.roster.IsOnline("222") && !got.roster.IsOnline("111")
	}, "a restarted bridge was never noticed: the roster still holds the old population")
}

// The bridge keeps a bounded log. A follower that falls behind finds the
// event at its cursor gone while the IDs carry on, and what it missed can
// only be recovered from `list`.
func TestBridgeRosterReseedsWhenItsCursorWasEvicted(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.logged(adapters.BridgeEventConnect, "Steve", "111")
	feed.listAnswers([]string{"Steve"}, nil)
	got := startBridgeRoster(t, feed, recordedNames{byName: map[string]string{"Alex": "222"}}, time.Hour)
	waitUntil(t, func() bool { return got.roster.IsOnline("111") }, "the first seed never landed")
	seeds := feed.listCalls()

	// Steve's leave was among the evicted lines, so only a seed can take
	// him off; Kai's connect is ID 2, after the eviction.
	feed.evictAndLog("Kai", "444", "Alex", "Kai")

	waitUntil(t, func() bool {
		return got.roster.IsOnline("222") && got.roster.IsOnline("444") && !got.roster.IsOnline("111")
	}, "an evicted cursor was never noticed: the roster still holds a player whose leave was lost")
	if feed.listCalls() <= seeds {
		t.Error("the roster changed without `list` being read again")
	}
}

func TestBridgeRosterRetriesASeedTheBridgeCouldNotAnswer(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.logged(adapters.BridgeEventConnect, "Steve", "111")
	feed.listAnswers(nil, errors.New("the names do not match the count the server gave"))
	got := startBridgeRoster(t, feed, recordedNames{}, time.Hour)

	waitUntil(t, func() bool { return feed.listCalls() >= 2 }, "a failed seed was never retried")
	if got.roster.Knows() {
		t.Fatal("Knows() = true with no seed ever landing")
	}

	feed.listAnswers([]string{"Steve"}, nil)
	waitUntil(t, func() bool { return got.roster.IsOnline("111") }, "the seed never landed once list answered")
}

// Anything the bridge missed, or a player resolved only later, is corrected
// by a periodic seed rather than waiting for a bridge restart.
func TestBridgeRosterReseedsPeriodically(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.listAnswers([]string{"Steve"}, nil)
	archive := recordedNames{byName: map[string]string{"Steve": "111", "Alex": "222"}}
	got := startBridgeRoster(t, feed, archive, 20*time.Millisecond)
	waitUntil(t, func() bool { return got.roster.IsOnline("111") }, "the first seed never landed")

	feed.listAnswers([]string{"Steve", "Alex"}, nil)
	waitUntil(t, func() bool { return got.roster.IsOnline("222") }, "no later seed picked up a player the events never mentioned")
}

// A periodic seed refreshes a server the follower already knows. Starting a
// new connection instead would forget every arrival and re-arm the
// fresh-join grace for everyone online on every reseed.
func TestBridgeRosterPeriodicReseedKeepsArrivals(t *testing.T) {
	clock := &gapClock{at: time.Date(2026, 9, 23, 10, 0, 0, 0, time.UTC)}
	joins := newJoinTimes()
	joins.now = clock.now
	feed := &fakeBridgeFeed{}
	feed.listAnswers([]string{"Steve", "Sam"}, nil)
	archive := recordedNames{byName: map[string]string{"Alex": "222"}}
	got := startBridgeRosterWithJoins(t, feed, archive, 20*time.Millisecond, joins)
	waitUntil(t, got.roster.Knows, "the roster never learned who is online")
	generation := joins.Generation()

	feed.logged(adapters.BridgeEventConnect, "Steve", "111")
	feed.logged(adapters.BridgeEventConnect, "Sam", "333")
	waitUntil(t, func() bool { return got.roster.IsOnline("111") && got.roster.IsOnline("333") },
		"the connect lines never reached the roster")
	clock.advance(time.Minute)

	// Sam's leave never reached the bridge's log; Alex arrived without a
	// line the bridge parsed. Only a seed can see either.
	feed.listAnswers([]string{"Steve", "Alex"}, nil)
	seeds := feed.listCalls()
	waitUntil(t, func() bool { return feed.listCalls() >= seeds+2 }, "no periodic seed ran after list changed")

	if got := joins.Generation(); got != generation {
		t.Errorf("Generation() = %d after a periodic seed, want %d: it ended deliveries for a connection that never dropped", got, generation)
	}
	if since, ok := joins.SinceJoin("111"); !ok || since != time.Minute {
		t.Errorf("SinceJoin(Steve) = %v, %v, want 1m, true: a periodic seed reset an arrival it already knew", since, ok)
	}
	if !got.roster.IsOnline("222") {
		t.Fatal("a periodic seed never put Alex on the roster")
	}
	if since, ok := joins.SinceJoin("222"); !ok || since != 0 {
		t.Errorf("SinceJoin(Alex) = %v, %v, want 0, true: a player first seen at a seed has only just arrived", since, ok)
	}
	if got.roster.IsOnline("333") {
		t.Error("a periodic seed left Sam on the roster after list stopped reporting him")
	}
	if _, ok := joins.SinceJoin("333"); ok {
		t.Error("Sam's arrival is still remembered after a seed found him gone")
	}
}

func TestBridgeRosterLeavesTheRosterUnknowingWhenItStops(t *testing.T) {
	feed := &fakeBridgeFeed{}
	feed.logged(adapters.BridgeEventConnect, "Steve", "111")
	feed.listAnswers([]string{"Steve"}, nil)
	got := startBridgeRoster(t, feed, recordedNames{}, time.Hour)
	waitUntil(t, got.roster.Knows, "the roster never learned who is online")
	before := got.joins.Generation()

	got.stop()

	if got.roster.Knows() || got.roster.IsOnline("111") {
		t.Error("the roster still claims to know who is online after the follower stopped")
	}
	if got.joins.Generation() == before {
		t.Error("the join clock's connection did not end: a delivery scheduled under the follower would still speak")
	}
}
