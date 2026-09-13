//go:build livedb

// Exercises the real SQL against a real PostgreSQL, as the runtime role.
//
// Everything else in this package's tests runs against Nop or pure helpers, so
// until now no query in postgres.go had ever been executed by the test suite.
// The permissions bug that left minecraft.players empty in production hid
// behind exactly that gap: the SQL was never run, so nothing could tell
// whether it was even correct.
//
// Behind a build tag because it needs a database. Run it with:
//
//	MC_TEST_DSN=postgres://app:...@127.0.0.1:55432/jdwillmsen_prd go test -tags livedb ./internal/store/
package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func liveStore(t *testing.T) *Postgres {
	t.Helper()
	dsn := os.Getenv("MC_TEST_DSN")
	if dsn == "" {
		t.Skip("MC_TEST_DSN unset")
	}
	pg, err := Open(context.Background(), dsn, 10*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pg.Close)
	return pg
}

// The whole write path in the order the agent drives it: a first arrival, a
// departure, then a return. Asserted through the store's own return values,
// since that is what the welcome plugin reads.
func TestJoinLeaveRejoinAgainstRealPostgres(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000001"
	start := time.Now().UTC().Add(-2 * time.Hour)

	first, err := pg.RecordJoin(ctx, xuid, "FirstTimer", start)
	if err != nil {
		t.Fatalf("first join: %v", err)
	}
	if !first.New() {
		t.Errorf("a player's first join reported as returning: %+v", first)
	}

	if _, err := pg.RecordLeave(ctx, xuid, time.Time{}, start.Add(30*time.Minute)); err != nil {
		t.Fatalf("leave: %v", err)
	}

	second, err := pg.RecordJoin(ctx, xuid, "FirstTimer", start.Add(time.Hour))
	if err != nil {
		t.Fatalf("second join: %v", err)
	}
	if second.New() {
		t.Error("a returning player reported as new; the greeting would be wrong")
	}
	// JoinCount counts the arrival just recorded rather than the state before
	// it -- RecordJoin increments it deliberately, so the greeting can say
	// "visit number N" and mean this one. Every other field is the prior
	// state; this is the single exception and the one worth pinning.
	if second.JoinCount != 2 {
		t.Errorf("JoinCount = %d on the second arrival, want 2 (this visit's number)", second.JoinCount)
	}
	if !second.LastSeen.Before(start.Add(time.Hour)) {
		t.Error("LastSeen advanced to this arrival; it must describe the previous one for AwayFor to work")
	}
}

// Milestones are read off the two totals RecordLeave returns, so they have
// to be the real before-and-after of the session it closed. Asserted
// relative to each other rather than as absolutes: the fixed XUID keeps its
// history across runs, and only the difference is this run's to know.
func TestRecordLeaveReportsTheTotalsAroundTheSession(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000010"
	start := time.Now().UTC().Add(-12 * time.Hour)

	if _, err := pg.RecordJoin(ctx, xuid, "Milestoner", start); err != nil {
		t.Fatalf("first join: %v", err)
	}
	first, err := pg.RecordLeave(ctx, xuid, time.Time{}, start.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("first leave: %v", err)
	}
	if first.After-first.Before != 2*time.Hour {
		t.Errorf("first leave added %v, want the 2h session it closed", first.After-first.Before)
	}
	if first.Gamertag != "Milestoner" {
		t.Errorf("gamertag = %q, want the session's own", first.Gamertag)
	}

	if _, err := pg.RecordJoin(ctx, xuid, "Milestoner", start.Add(3*time.Hour)); err != nil {
		t.Fatalf("second join: %v", err)
	}
	second, err := pg.RecordLeave(ctx, xuid, time.Time{}, start.Add(11*time.Hour))
	if err != nil {
		t.Fatalf("second leave: %v", err)
	}
	if second.Before != first.After {
		t.Errorf("second leave's before = %v, want the first's after %v", second.Before, first.After)
	}
	if second.After-second.Before != 8*time.Hour {
		t.Errorf("second leave added %v, want 8h", second.After-second.Before)
	}

	// Nothing open any more: a departure that changed nothing.
	again, err := pg.RecordLeave(ctx, xuid, time.Time{}, start.Add(12*time.Hour))
	if err != nil {
		t.Fatalf("repeated leave: %v", err)
	}
	if again.Before != again.After || again.Gamertag != "" {
		t.Errorf("repeated leave = %+v, want equal totals and no gamertag", again)
	}
}

// A session the agent never saw end should already be closed by the time the
// player arrives again -- every connection starts by closing them -- but if
// that close failed, the next arrival must close it at zero length -- the
// player was not playing through their absence -- and neither playtime total
// may grow from it. If it did, a player back after three days would be broadcast
// as having played 72 hours more than they had.
func TestAStaleSessionIsClosedAtZeroLengthAndCountsForNothing(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000011"
	t0 := time.Now().UTC().Add(-100 * time.Hour).Truncate(time.Second)

	// Reports the history this fixed XUID already has, after closing anything
	// an interrupted earlier run left open.
	base, err := pg.RecordLeave(ctx, xuid, time.Time{}, t0.Add(-time.Hour))
	if err != nil {
		t.Fatalf("baseline leave: %v", err)
	}

	if _, err := pg.RecordJoin(ctx, xuid, "Wanderer", t0); err != nil {
		t.Fatalf("join: %v", err)
	}
	// No leave: the agent reconnected and never saw one. Three days later:
	back := t0.Add(72 * time.Hour)
	profile, err := pg.RecordJoin(ctx, xuid, "Wanderer", back)
	if err != nil {
		t.Fatalf("return after the gap: %v", err)
	}

	var duration int64
	var reason string
	if err := pg.pool.QueryRow(ctx,
		`SELECT duration_seconds, ended_reason FROM minecraft.sessions WHERE xuid = $1 AND joined_at = $2`,
		xuid, t0,
	).Scan(&duration, &reason); err != nil {
		t.Fatalf("read the stale session: %v", err)
	}
	if duration != 0 || reason != "unknown" {
		t.Errorf("stale session closed as (%ds, %s), want (0s, unknown): the absence was credited as playtime", duration, reason)
	}
	if want := int64(base.After / time.Second); profile.TotalSeconds != want {
		t.Errorf("welcome total = %ds, want %ds: the unobserved session counted", profile.TotalSeconds, want)
	}

	pt, err := pg.RecordLeave(ctx, xuid, time.Time{}, back.Add(time.Hour))
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if pt.Before != base.After || pt.After != base.After+time.Hour {
		t.Errorf("totals = (%v, %v), want (%v, %v): only the observed hour counts", pt.Before, pt.After, base.After, base.After+time.Hour)
	}
}

// The reconnect sequence as the agent drives it: a player joins at T0, the
// agent loses its connection, the player leaves and comes back unseen, and
// the agent reconnects at T4 to find them in the opening snapshot. Only
// T4..T5 was watched, so only that hour may count -- 72 hours of absence
// credited here would be announced as a playtime milestone.
func TestAReconnectCreditsOnlyTheTimeWatchedSinceIt(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000013"
	t0 := time.Now().UTC().Add(-300 * time.Hour).Truncate(time.Second)

	base, err := pg.RecordLeave(ctx, xuid, time.Time{}, t0.Add(-time.Hour))
	if err != nil {
		t.Fatalf("baseline leave: %v", err)
	}
	if _, err := pg.RecordJoin(ctx, xuid, "Reconnected", t0); err != nil {
		t.Fatalf("join: %v", err)
	}
	var joinsBefore int
	if err := pg.pool.QueryRow(ctx, `SELECT join_count FROM minecraft.players WHERE xuid = $1`, xuid).Scan(&joinsBefore); err != nil {
		t.Fatalf("read join count: %v", err)
	}

	t4 := t0.Add(72 * time.Hour)
	if _, err := pg.CloseOrphans(ctx, t4); err != nil {
		t.Fatalf("close orphans at reconnect: %v", err)
	}
	if _, err := pg.ResumeSession(ctx, xuid, "Reconnected", t4); err != nil {
		t.Fatalf("resume from the snapshot: %v", err)
	}

	pt, err := pg.RecordLeave(ctx, xuid, t4, t4.Add(time.Hour))
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if pt.Before != base.After || pt.After != base.After+time.Hour {
		t.Errorf("totals = (%v, %v), want (%v, %v): only the hour since the reconnect was watched", pt.Before, pt.After, base.After, base.After+time.Hour)
	}
	if pt.Gamertag != "Reconnected" {
		t.Errorf("gamertag = %q, want the resumed session's", pt.Gamertag)
	}

	var joinsAfter int
	if err := pg.pool.QueryRow(ctx, `SELECT join_count FROM minecraft.players WHERE xuid = $1`, xuid).Scan(&joinsAfter); err != nil {
		t.Fatalf("read join count: %v", err)
	}
	if joinsAfter != joinsBefore {
		t.Errorf("join_count went %d -> %d: resuming a session is not an arrival", joinsBefore, joinsAfter)
	}
}

// The reconnect sequence with both of its writes failing: the close every
// connection starts with, and the snapshot's fresh session. The session
// from before the gap is still open when the player leaves, and because it
// began before the connection did, the leave closes it as unobserved rather
// than crediting the absence it spans.
func TestALeaveNeverCreditsASessionOlderThanTheConnection(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000014"
	t0 := time.Now().UTC().Add(-400 * time.Hour).Truncate(time.Second)

	base, err := pg.RecordLeave(ctx, xuid, time.Time{}, t0.Add(-time.Hour))
	if err != nil {
		t.Fatalf("baseline leave: %v", err)
	}
	if _, err := pg.RecordJoin(ctx, xuid, "Unwatched", t0); err != nil {
		t.Fatalf("join: %v", err)
	}

	t4 := t0.Add(72 * time.Hour)
	pt, err := pg.RecordLeave(ctx, xuid, t4, t4.Add(time.Hour))
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if pt.Before != base.After || pt.After != base.After {
		t.Errorf("totals = (%v, %v), want both %v: a session from before the reconnect was credited", pt.Before, pt.After, base.After)
	}
	if pt.Gamertag != "" {
		t.Errorf("gamertag = %q, want blank: the leave credited nothing", pt.Gamertag)
	}

	var duration int64
	var reason string
	if err := pg.pool.QueryRow(ctx,
		`SELECT duration_seconds, ended_reason FROM minecraft.sessions WHERE xuid = $1 AND joined_at = $2`,
		xuid, t0,
	).Scan(&duration, &reason); err != nil {
		t.Fatalf("read the stale session: %v", err)
	}
	if duration != 0 || reason != "unknown" {
		t.Errorf("stale session closed as (%ds, %s), want (0s, unknown)", duration, reason)
	}
}

// A player the agent has never recorded can be in a snapshot; resuming
// their session must create the row the session's foreign key needs.
func TestResumeSessionForAnUnrecordedPlayer(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := fmt.Sprintf("25354%011d", time.Now().UnixNano()%1e11)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)

	if err := pg.EnsurePlayer(ctx, xuid, "Stranger", at); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	firstSeen, err := pg.ResumeSession(ctx, xuid, "Stranger", at)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !firstSeen {
		t.Error("resume reported a player with only a bare row as seen before; nobody would hear of them")
	}
	pt, err := pg.RecordLeave(ctx, xuid, time.Time{}, at.Add(time.Hour))
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if pt.Before != 0 || pt.After != time.Hour {
		t.Errorf("totals = (%v, %v), want (0s, 1h0m0s)", pt.Before, pt.After)
	}
	profile, err := pg.RecordJoin(ctx, xuid, "Stranger", at.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if profile.JoinCount != 1 {
		t.Errorf("JoinCount = %d on the first observed arrival, want 1", profile.JoinCount)
	}
	if profile.Sessions != 1 {
		t.Errorf("Sessions = %d before the first observed arrival, want the resumed one; the player would be announced a second time", profile.Sessions)
	}
	again, err := pg.ResumeSession(ctx, xuid, "Stranger", at.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if again {
		t.Error("a second resume reported a first sight again")
	}
}

// Rows written before stale sessions were closed at zero length carry the
// player's whole absence as an 'unknown' duration, and production still has
// them. Neither the welcome's total nor the milestone totals may read them.
func TestUnknownSessionsWithADurationCountForNothing(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000012"
	t0 := time.Now().UTC().Add(-200 * time.Hour).Truncate(time.Second)

	if err := pg.EnsurePlayer(ctx, xuid, "LegacyRow", t0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	base, err := pg.RecordLeave(ctx, xuid, time.Time{}, t0)
	if err != nil {
		t.Fatalf("baseline leave: %v", err)
	}
	if _, err := pg.pool.Exec(ctx, `
		INSERT INTO minecraft.sessions (xuid, gamertag, joined_at, left_at, ended_reason)
		VALUES ($1, 'LegacyRow', $2, $3, 'unknown')`,
		xuid, t0, t0.Add(72*time.Hour),
	); err != nil {
		t.Fatalf("insert a legacy inflated session: %v", err)
	}

	pt, err := pg.RecordLeave(ctx, xuid, time.Time{}, t0.Add(80*time.Hour))
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if pt.Before != base.Before || pt.After != base.Before {
		t.Errorf("totals = (%v, %v), want both %v: a 72h 'unknown' row was counted", pt.Before, pt.After, base.Before)
	}
	profile, err := pg.RecordJoin(ctx, xuid, "LegacyRow", t0.Add(90*time.Hour))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if want := int64(base.Before / time.Second); profile.TotalSeconds != want {
		t.Errorf("welcome total = %ds, want %ds, the same total the milestones read", profile.TotalSeconds, want)
	}
}

// A gamertag change must not create a second player: identity is the XUID.
func TestGamertagChangeKeepsOneIdentity(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000002"

	if _, err := pg.RecordJoin(ctx, xuid, "OldName", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("join as OldName: %v", err)
	}
	if _, err := pg.RecordLeave(ctx, xuid, time.Time{}, time.Now().UTC().Add(-50*time.Minute)); err != nil {
		t.Fatalf("leave: %v", err)
	}
	profile, err := pg.RecordJoin(ctx, xuid, "NewName", time.Now().UTC())
	if err != nil {
		t.Fatalf("join as NewName: %v", err)
	}
	if profile.New() {
		t.Error("a renamed player reported as new; identity must follow the XUID, not the gamertag")
	}
	if profile.JoinCount != 2 {
		t.Errorf("JoinCount = %d, want 2; the rename started a second identity", profile.JoinCount)
	}
}

// A rename must not erase who they used to be: player_names is the history
// that makes an old server log line still resolvable to a current player.
func TestBothGamertagsSurviveARename(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000004"

	for _, name := range []string{"BeforeRename", "AfterRename"} {
		if _, err := pg.RecordJoin(ctx, xuid, name, time.Now().UTC()); err != nil {
			t.Fatalf("join as %s: %v", name, err)
		}
	}

	rows, err := pg.pool.Query(ctx, `SELECT gamertag FROM minecraft.player_names WHERE xuid = $1 ORDER BY gamertag`, xuid)
	if err != nil {
		t.Fatalf("read name history: %v", err)
	}
	defer rows.Close()
	var seen []string
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen = append(seen, g)
	}
	if len(seen) != 2 || seen[0] != "AfterRename" || seen[1] != "BeforeRename" {
		t.Errorf("name history is %v, want both names recorded", seen)
	}
}

// The lookup !announce leans on, against the real SQL: a player who has
// logged out is still resolvable, and so is the name they used to answer to
// -- neither of which the live roster can say anything about.
func TestXUIDForNameResolvesOfflineAndRenamedPlayers(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000007"

	if _, err := pg.RecordJoin(ctx, xuid, "WasCalledThis", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("join under the old name: %v", err)
	}
	if _, err := pg.RecordJoin(ctx, xuid, "CalledThisNow", time.Now().UTC()); err != nil {
		t.Fatalf("join under the new name: %v", err)
	}
	if _, err := pg.RecordLeave(ctx, xuid, time.Time{}, time.Now().UTC()); err != nil {
		t.Fatalf("leave: %v", err)
	}

	for _, name := range []string{"CalledThisNow", "WasCalledThis"} {
		got, ok, err := pg.XUIDForName(ctx, name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		if !ok || got != xuid {
			t.Errorf("XUIDForName(%s) = (%q, %v), want %q -- an offline player is still a known one", name, got, ok, xuid)
		}
	}

	if got, ok, err := pg.XUIDForName(ctx, "NobodyHasEverBeenCalledThis"); err != nil || ok || got != "" {
		t.Errorf("XUIDForName(unknown) = (%q, %v, %v), want an empty not-found", got, ok, err)
	}
}

// A gamertag freed by a rename can be taken by a different account, so the
// player answering to it now must win over the one who merely used to.
// Getting this backwards aims a private message at the wrong person.
func TestXUIDForNamePrefersTheCurrentHolderOfAName(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	const shared = "SharedName"
	previous, current := "2535400000000008", "2535400000000009"

	if _, err := pg.RecordJoin(ctx, previous, shared, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		t.Fatalf("first holder joins: %v", err)
	}
	if _, err := pg.RecordJoin(ctx, previous, "RenamedAway", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("first holder renames: %v", err)
	}
	if _, err := pg.RecordJoin(ctx, current, shared, time.Now().UTC()); err != nil {
		t.Fatalf("second holder joins: %v", err)
	}

	got, ok, err := pg.XUIDForName(ctx, shared)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !ok || got != current {
		t.Errorf("XUIDForName(%s) = (%q, %v), want %q -- the player who answers to it now", shared, got, ok, current)
	}
}

// Sessions left open by a previous run are the agent's normal state after any
// restart, so this runs on every startup and was the one place the missing
// grant actually surfaced.
func TestCloseOrphansClosesOnlyOpenSessions(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000003"

	if _, err := pg.RecordJoin(ctx, xuid, "Abandoned", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("join: %v", err)
	}
	n, err := pg.CloseOrphans(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("close orphans: %v", err)
	}
	if n < 1 {
		t.Errorf("closed %d sessions, want at least the one just opened", n)
	}
	again, err := pg.CloseOrphans(ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("second close orphans: %v", err)
	}
	if again != 0 {
		t.Errorf("closed %d sessions on a second pass, want 0 -- closing is not idempotent", again)
	}
}

// EnsurePlayer exists so an announcement can be recorded as delivered to
// someone the agent never watched arrive. What it must not do is look like
// an arrival: the greeting a player gets on their next real join is read
// from a row this may have created first.
func TestEnsurePlayerCreatesARowWithoutCountingAJoin(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000005"
	at := time.Now().UTC()

	if err := pg.EnsurePlayer(ctx, xuid, "AlreadyOnline", at); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	var joins int
	if err := pg.pool.QueryRow(ctx, `SELECT join_count FROM minecraft.players WHERE xuid = $1`, xuid).Scan(&joins); err != nil {
		t.Fatalf("read join_count: %v", err)
	}
	if joins != 0 {
		t.Errorf("join_count = %d after an ensure, want 0; a delivery is not an arrival", joins)
	}

	profile, err := pg.RecordJoin(ctx, xuid, "AlreadyOnline", at.Add(time.Minute))
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if !profile.New() {
		t.Error("the first observed join of an ensured player reported as a return; they would be greeted as a regular")
	}
}

// A bare row the outbox wrote for a brand-new joiner, before the join itself
// was recorded, is not a sighting: the join must still read as the player's
// first.
func TestAJoinAfterAnEnsureStillReadsAsUnseen(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := fmt.Sprintf("25355%011d", time.Now().UnixNano()%1e11)
	at := time.Now().UTC().Truncate(time.Second)

	if err := pg.EnsurePlayer(ctx, xuid, "RaceLoser", at); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	profile, err := pg.RecordJoin(ctx, xuid, "RaceLoser", at)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if profile.Sessions != 0 {
		t.Errorf("Sessions = %d before the first join, want 0: the first-join notice would be skipped", profile.Sessions)
	}
}

// Ensuring is idempotent and never rewrites a real profile: a returning
// player's history must survive an announcement being delivered to them.
func TestEnsurePlayerLeavesAnExistingProfileAlone(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000006"

	if _, err := pg.RecordJoin(ctx, xuid, "Regular", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("join: %v", err)
	}
	if err := pg.EnsurePlayer(ctx, xuid, "SomethingElse", time.Now().UTC()); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	var gamertag string
	var joins int
	if err := pg.pool.QueryRow(ctx,
		`SELECT current_gamertag, join_count FROM minecraft.players WHERE xuid = $1`, xuid,
	).Scan(&gamertag, &joins); err != nil {
		t.Fatalf("read player: %v", err)
	}
	if gamertag != "Regular" || joins != 1 {
		t.Errorf("player is (%q, %d) after an ensure, want (\"Regular\", 1)", gamertag, joins)
	}
}

// A release hands the agent over from one process to another, and the
// departing one is the only thing that knows who it was watching. Closing
// those sessions as it goes is what keeps a deploy from costing every player
// online the time they had already played.
func TestHandoverClosesWatchedSessionsAndCreditsThem(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := freshXUID()
	connected := time.Now().UTC().Add(-time.Hour)

	if _, err := pg.RecordJoin(ctx, xuid, "Handover", connected); err != nil {
		t.Fatalf("join: %v", err)
	}

	closed, err := pg.CloseForHandover(ctx, connected, connected.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("handover: %v", err)
	}
	// At least, rather than exactly: the handover closes every session this
	// agent was watching, and a shared test database holds other players'
	// rows from other tests. What must be exact is this player's own row.
	if closed < 1 {
		t.Errorf("closed %d sessions, want at least the one being watched", closed)
	}

	reason, duration := sessionEnd(t, pg, xuid)
	if reason != "agent_restart" {
		t.Errorf("ended_reason = %q, want agent_restart", reason)
	}
	if duration != 1800 {
		t.Errorf("duration_seconds = %d, want 1800: the handover credits the time actually watched", duration)
	}

	// And the player is credited: a handover is a split session, not a lost
	// one, so the next greeting reads the same total it would have read had
	// nobody deployed anything.
	profile, err := pg.RecordJoin(ctx, xuid, "Handover", connected.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if profile.TotalSeconds != 1800 {
		t.Errorf("TotalSeconds = %d, want 1800: a handover's half of a session must still count", profile.TotalSeconds)
	}
	// The distinction the schema draws, and the one an operator reads: an
	// unclean session is time nobody watched. This was watched, to the
	// second.
	if profile.UncleanSessions != 0 {
		t.Errorf("UncleanSessions = %d, want 0: a clean handover is not an unwatched session", profile.UncleanSessions)
	}
}

// The same rule RecordLeave applies: a session older than the current
// connection was open across a gap the agent never saw, so the handover
// cannot honestly credit it either.
func TestHandoverLeavesASessionFromBeforeTheConnectionUnobserved(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := freshXUID()
	old := time.Now().UTC().Add(-48 * time.Hour)

	if _, err := pg.RecordJoin(ctx, xuid, "LongGone", old); err != nil {
		t.Fatalf("join: %v", err)
	}

	connected := time.Now().UTC()
	if _, err := pg.CloseForHandover(ctx, connected, connected.Add(time.Minute)); err != nil {
		t.Fatalf("handover: %v", err)
	}

	reason, duration := sessionEnd(t, pg, xuid)
	if reason != "unknown" {
		t.Errorf("ended_reason = %q, want unknown for a session open across a gap", reason)
	}
	if duration != 0 {
		t.Errorf("duration_seconds = %d, want 0: two days of absence is not playtime", duration)
	}
}

// The handover's whole point, end to end: a player online across a release
// keeps the time they played on both sides of it.
func TestPlaytimeSurvivesAHandoverMidSession(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := freshXUID()
	joined := time.Now().UTC().Add(-2 * time.Hour)
	handover := joined.Add(30 * time.Minute)

	if _, err := pg.RecordJoin(ctx, xuid, "StayedOnline", joined); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := pg.CloseForHandover(ctx, joined, handover); err != nil {
		t.Fatalf("handover: %v", err)
	}
	// The successor finds them in its opening roster snapshot and resumes
	// watching, exactly as it does after any reconnect.
	if _, err := pg.ResumeSession(ctx, xuid, "StayedOnline", handover); err != nil {
		t.Fatalf("resume: %v", err)
	}

	playtime, err := pg.RecordLeave(ctx, xuid, handover, handover.Add(20*time.Minute))
	if err != nil {
		t.Fatalf("leave: %v", err)
	}
	if playtime.Before != 30*time.Minute {
		t.Errorf("playtime before the leave = %v, want 30m (the half the handover closed)", playtime.Before)
	}
	if playtime.After != 50*time.Minute {
		t.Errorf("playtime after the leave = %v, want 50m: both halves of one visit", playtime.After)
	}
}

// sessionEnd reads how the player's most recent session ended.
func sessionEnd(t *testing.T, pg *Postgres, xuid string) (reason string, duration int) {
	t.Helper()
	err := pg.pool.QueryRow(t.Context(), `
		SELECT ended_reason, COALESCE(duration_seconds, -1)
		FROM minecraft.sessions WHERE xuid = $1
		ORDER BY session_id DESC LIMIT 1`, xuid,
	).Scan(&reason, &duration)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	return reason, duration
}

// freshXUID keeps these tests independent of each other and of every
// previous run: the totals they assert are sums over one player's whole
// history, so a fixed id would accumulate.
func freshXUID() string {
	return fmt.Sprintf("25356%011d", time.Now().UnixNano()%1e11)
}
