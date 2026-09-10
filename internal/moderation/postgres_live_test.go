//go:build livedb

// Exercises the real SQL against a real PostgreSQL, as the runtime role.
//
// The rule and action constants only matter because the schema's CHECK
// constraints enforce that exact set, the newest-first read leans on a
// tie-breaker a fake would simply agree with, and the prune is a DELETE whose
// comparison is the kind that reads fine the wrong way round. None of that is
// visible to a unit test against Nop.
//
// Behind a build tag because it needs a database, and never pointed at
// production: it writes and deletes rows. Run it with:
//
//	MC_TEST_DSN=postgres://app:...@127.0.0.1:55432/jdwillmsen_prd go test -tags livedb ./internal/moderation/
package moderation

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testXUID = "__test"

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
	t.Cleanup(pool.Close)
	return pool
}

func cleanup(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clear := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM minecraft.moderation_events WHERE xuid = $1`, testXUID)
	}
	clear()
	t.Cleanup(clear)
}

func TestRecordAcceptsEveryRuleAndActionTheSchemaAllows(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	cleanup(t, pool)
	s := NewPostgres(pool)

	for _, rule := range []Rule{RuleTerm, RuleFlood, RuleCaps} {
		for _, action := range []Action{ActionLogged, ActionWarned} {
			if err := s.Record(ctx, Event{
				XUID: testXUID, Gamertag: "__test", Message: "probe",
				Rule: rule, Detail: "probe", Action: action, OccurredAt: time.Now(),
			}); err != nil {
				t.Fatalf("rule %q action %q rejected by the schema: %v", rule, action, err)
			}
		}
	}
}

func TestRecentIsNewestFirstLimitedAndFiltered(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	cleanup(t, pool)
	s := NewPostgres(pool)

	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i, msg := range []string{"first", "second", "third"} {
		if err := s.Record(ctx, Event{
			XUID: testXUID, Gamertag: "__test", Message: msg,
			Rule: RuleCaps, Detail: "d", Action: ActionLogged, OccurredAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	// Same timestamp as "third": the tie must still come back newest-inserted first.
	if err := s.Record(ctx, Event{
		XUID: testXUID, Gamertag: "__test", Message: "third-tie",
		Rule: RuleTerm, Detail: "d", Action: ActionWarned, OccurredAt: base.Add(2 * time.Minute),
	}); err != nil {
		t.Fatalf("record: %v", err)
	}

	got, err := s.Recent(ctx, testXUID, 3)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	want := []string{"third-tie", "third", "second"}
	if len(got) != len(want) {
		t.Fatalf("Recent returned %d rows, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Message != want[i] {
			t.Errorf("row %d = %q, want %q", i, got[i].Message, want[i])
		}
	}
	if got[0].Rule != RuleTerm || got[0].Action != ActionWarned || got[0].ID == 0 {
		t.Errorf("row 0 scanned as %+v", got[0])
	}

	if other, err := s.Recent(ctx, "__nobody", 10); err != nil || len(other) != 0 {
		t.Errorf("Recent for another xuid = (%d rows, %v), want none", len(other), err)
	}
	if all, err := s.Recent(ctx, "", 1); err != nil || len(all) != 1 {
		t.Errorf("unfiltered Recent = (%d rows, %v), want 1", len(all), err)
	}
}

func TestPruneDeletesOnlyWhatIsOlderThanTheCutoff(t *testing.T) {
	ctx := context.Background()
	pool := livePool(t)
	cleanup(t, pool)
	s := NewPostgres(pool)

	now := time.Now()
	for _, e := range []Event{
		{Message: "old", OccurredAt: now.Add(-Retention - time.Hour)},
		{Message: "kept", OccurredAt: now.Add(-Retention + time.Hour)},
	} {
		e.XUID, e.Gamertag, e.Rule, e.Action = testXUID, "__test", RuleFlood, ActionLogged
		if err := s.Record(ctx, e); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	if _, err := s.Prune(ctx, now.Add(-Retention)); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	got, err := s.Recent(ctx, testXUID, 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	if len(got) != 1 || got[0].Message != "kept" {
		t.Errorf("after prune = %+v, want only the row inside retention", got)
	}
}
