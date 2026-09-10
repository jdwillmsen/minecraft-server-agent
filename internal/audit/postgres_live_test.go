//go:build livedb

// Exercises the real SQL against a real PostgreSQL, as the runtime role.
//
// Everything else in this package's tests runs against Nop or pure constant
// checks, so no query in postgres.go has ever been executed against a real
// database. The six Outcome constants only matter because the schema's
// CHECK constraint on the outcome column enforces that exact set -- and a
// constraint is not something a unit test against Nop can see. This test is
// the only thing that actually inserts one row per outcome and lets
// Postgres itself decide whether the constant list still matches.
//
// Behind a build tag because it needs a database. Run it with:
//
//	MC_TEST_DSN=postgres://app:...@127.0.0.1:55432/jdwillmsen_prd go test -tags livedb ./internal/audit/
package audit

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// livePool opens a pool from the runtime role's DSN rather than reuse
// store.Postgres's pool: Task 5 has not added the accessor yet, so this is
// the only way this test can reach a real database until then.
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

func TestWriteAcceptsEveryOutcomeTheSchemaAllows(t *testing.T) {
	ctx := context.Background()
	s := NewPostgres(livePool(t))
	t.Cleanup(func() {
		_, _ = livePool(t).Exec(ctx, `DELETE FROM minecraft.command_audit WHERE xuid = '__test'`)
	})

	for _, out := range []Outcome{
		OutcomeOK, OutcomeDenied, OutcomeUnknown,
		OutcomeError, OutcomeRateLimited, OutcomeTimeout,
	} {
		if err := s.Write(ctx, Record{
			XUID: "__test", Gamertag: "__test", Permission: "visitor",
			Command: "probe", Args: "", Outcome: out, At: time.Now(),
		}); err != nil {
			t.Fatalf("outcome %q rejected by the schema: %v", out, err)
		}
	}
}
