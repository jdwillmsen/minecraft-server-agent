//go:build livedb

// Exercises the real SQL against the tables plan 2's V8 migration creates,
// as the runtime role. The version rules are enforced by these statements
// and nothing else, so only a real database can say they hold.
//
//	MC_TEST_DSN=postgres://app:...@127.0.0.1:55432/presence_test?sslmode=disable \
//	  go test -tags livedb ./internal/presence/
package presence

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jdwillmsen/minecraft-server-agent/presenceapi"
)

// Ids no real deployment uses, cleaned up before and after each test.
var liveIDs = []string{"zz-test-a", "zz-test-b", "zz-test-agent"}

func livePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("MC_TEST_DSN")
	if dsn == "" {
		t.Skip("MC_TEST_DSN unset")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	clean := func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM minecraft.presence_overrides WHERE actor_id = ANY($1)`, liveIDs)
		_, _ = pool.Exec(ctx, `DELETE FROM minecraft.presence_status WHERE actor_id = ANY($1)`, liveIDs)
	}
	clean()
	t.Cleanup(func() { clean(); pool.Close() })
	return pool
}

func liveOverride(state presenceapi.State) presenceapi.Override {
	until := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	return presenceapi.Override{
		State: state, Until: &until, WakeOn: &presenceapi.WakeOn{Players: []string{"Steve"}},
		Reason: "live test", SetBy: "api:test", SetAt: time.Now().UTC().Truncate(time.Microsecond),
	}
}

func TestSetFollowsTheVersionRules(t *testing.T) {
	ctx := t.Context()
	s := NewPostgres(livePool(t))

	first, err := s.Set(ctx, "zz-test-a", liveOverride(presenceapi.StateParked), 0)
	if err != nil {
		t.Fatalf("first set: %v", err)
	}
	if first.Prev != nil || first.Now == nil || first.Now.Version != 1 {
		t.Fatalf("first set = %+v, want a new row at version 1", first)
	}

	stale, err := s.Set(ctx, "zz-test-a", liveOverride(presenceapi.StatePresent), 0)
	if !errors.Is(err, ErrConflict) || stale.Prev == nil || stale.Prev.Version != 1 {
		t.Fatalf("set expecting no row = %+v, %v; want ErrConflict carrying version 1", stale, err)
	}

	second, err := s.Set(ctx, "zz-test-a", liveOverride(presenceapi.StatePresent), 1)
	if err != nil {
		t.Fatalf("second set: %v", err)
	}
	if second.Prev == nil || second.Prev.State != presenceapi.StateParked || second.Now.Version != 2 {
		t.Fatalf("second set = %+v, want parked version 1 replaced by version 2", second)
	}

	got, err := s.Overrides(ctx)
	if err != nil {
		t.Fatalf("Overrides: %v", err)
	}
	row := got["zz-test-a"]
	if row.State != presenceapi.StatePresent || row.Until == nil || row.WakeOn == nil || len(row.WakeOn.Players) != 1 || row.SetBy != "api:test" {
		t.Errorf("stored row = %+v, want every field back", row)
	}

	if ok, err := s.Remove(ctx, "zz-test-a", 1); err != nil || ok {
		t.Errorf("Remove at a stale version = %v, %v; want nothing removed", ok, err)
	}
	if ok, err := s.Remove(ctx, "zz-test-a", 2); err != nil || !ok {
		t.Errorf("Remove at the current version = %v, %v; want removed", ok, err)
	}
	again, err := s.Set(ctx, "zz-test-a", liveOverride(presenceapi.StateParked), 0)
	if err != nil || again.Now.Version != 1 {
		t.Errorf("set after removal = %+v, %v; want a fresh row at version 1", again, err)
	}
}

func TestSetManyAndClearAreOneTransactionEach(t *testing.T) {
	ctx := t.Context()
	s := NewPostgres(livePool(t))
	both := map[string]presenceapi.Override{"zz-test-a": liveOverride(presenceapi.StateParked), "zz-test-b": liveOverride(presenceapi.StateParked)}

	if _, err := s.SetMany(ctx, both); err != nil {
		t.Fatalf("SetMany: %v", err)
	}
	changes, err := s.SetMany(ctx, both)
	if err != nil {
		t.Fatalf("SetMany again: %v", err)
	}
	for _, c := range changes {
		if c.Prev == nil || c.Now.Version != 2 {
			t.Errorf("second SetMany change = %+v, want version 2 over a prior row", c)
		}
	}
	cleared, err := s.Clear(ctx, []string{"zz-test-a", "zz-test-b", "zz-test-agent"})
	if err != nil || len(cleared) != 2 {
		t.Fatalf("Clear = %d changes, %v; want the two rows that existed", len(cleared), err)
	}
	if cleared[0].Now != nil || cleared[0].Prev == nil {
		t.Errorf("clear change = %+v, want the removed row and nothing after", cleared[0])
	}
}

func TestPutStatusUpserts(t *testing.T) {
	ctx := t.Context()
	s := NewPostgres(livePool(t))
	seen := time.Now().UTC().Truncate(time.Microsecond)
	for _, connected := range []bool{true, false} {
		if err := s.PutStatus(ctx, "zz-test-b", presenceapi.Status{Connected: connected, ObservedState: presenceapi.StatePresent, LastSeen: seen, ProcessVersion: "1.4.0"}); err != nil {
			t.Fatalf("PutStatus(%v): %v", connected, err)
		}
	}
	got, err := s.Statuses(ctx)
	if err != nil {
		t.Fatalf("Statuses: %v", err)
	}
	st := got["zz-test-b"]
	if st.Connected || !st.LastSeen.Equal(seen) || st.ProcessVersion != "1.4.0" {
		t.Errorf("status = %+v, want the second report", st)
	}
}
