package announce

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The distinction NotMigrated draws is the whole point of it: "the tables
// are not there yet" is an operator's deploy-ordering mistake that a command
// can answer for, and everything else is a failure a command must not
// disguise as one.
func TestNotMigratedRecognisesOnlyAMissingTable(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"undefined table, wrapped as the store wraps it": {
			err:  fmt.Errorf("announce: insert: %w", &pgconn.PgError{Code: "42P01"}),
			want: true,
		},
		"undefined table, bare": {
			err:  &pgconn.PgError{Code: "42P01"},
			want: true,
		},
		"a different postgres error": {
			err:  fmt.Errorf("announce: insert: %w", &pgconn.PgError{Code: "23503"}),
			want: false,
		},
		"an ordinary error": {err: errors.New("connection refused"), want: false},
		"no error at all":   {err: nil, want: false},
	}

	for name, tc := range cases {
		if got := NotMigrated(tc.err); got != tc.want {
			t.Errorf("%s: NotMigrated = %v, want %v", name, got, tc.want)
		}
	}
}

// Pinned to the literal for the same reason the target and priority
// constants are: this string is Postgres's, not ours, and a typo in it would
// leave every test green while the guard never fires in production.
func TestUndefinedTableIsPostgresOwnCode(t *testing.T) {
	if undefinedTable != "42P01" {
		t.Errorf("undefinedTable = %q, want %q", undefinedTable, "42P01")
	}
}
