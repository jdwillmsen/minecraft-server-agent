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

	if err := pg.RecordLeave(ctx, xuid, start.Add(30*time.Minute)); err != nil {
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

// A gamertag change must not create a second player: identity is the XUID.
func TestGamertagChangeKeepsOneIdentity(t *testing.T) {
	pg := liveStore(t)
	ctx := t.Context()
	xuid := "2535400000000002"

	if _, err := pg.RecordJoin(ctx, xuid, "OldName", time.Now().UTC().Add(-time.Hour)); err != nil {
		t.Fatalf("join as OldName: %v", err)
	}
	if err := pg.RecordLeave(ctx, xuid, time.Now().UTC().Add(-50*time.Minute)); err != nil {
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
	if err := pg.RecordLeave(ctx, xuid, time.Now().UTC()); err != nil {
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
