package pgerr

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The distinction these predicates draw is the whole point of them: a
// missing table and a missing grant are deploy mistakes a command can
// answer for, and everything else is a failure a command must not disguise
// as one.
func TestPredicatesRecogniseOnlyTheirOwnCode(t *testing.T) {
	cases := map[string]struct {
		err                              error
		notMigrated, notGranted, unready bool
	}{
		"undefined table, wrapped as a store wraps it": {
			err:         fmt.Errorf("announce: insert: %w", &pgconn.PgError{Code: "42P01"}),
			notMigrated: true,
			unready:     true,
		},
		"undefined table, bare": {
			err:         &pgconn.PgError{Code: "42P01"},
			notMigrated: true,
			unready:     true,
		},
		"insufficient privilege, wrapped": {
			err:        fmt.Errorf("audit: write: %w", &pgconn.PgError{Code: "42501"}),
			notGranted: true,
			unready:    true,
		},
		"a different postgres error": {
			err: fmt.Errorf("announce: insert: %w", &pgconn.PgError{Code: "23503"}),
		},
		"an ordinary error": {err: errors.New("connection refused")},
		"no error at all":   {err: nil},
	}

	for name, tc := range cases {
		if got := NotMigrated(tc.err); got != tc.notMigrated {
			t.Errorf("%s: NotMigrated = %v, want %v", name, got, tc.notMigrated)
		}
		if got := NotGranted(tc.err); got != tc.notGranted {
			t.Errorf("%s: NotGranted = %v, want %v", name, got, tc.notGranted)
		}
		if got := Unready(tc.err); got != tc.unready {
			t.Errorf("%s: Unready = %v, want %v", name, got, tc.unready)
		}
	}
}

// Pinned to the literals for the same reason the announcement target and
// priority constants are: these strings are Postgres's, not ours, and a
// typo in one would leave every test green while the guard never fires in
// production.
func TestCodesArePostgresOwn(t *testing.T) {
	if undefinedTable != "42P01" {
		t.Errorf("undefinedTable = %q, want %q", undefinedTable, "42P01")
	}
	if insufficientPrivilege != "42501" {
		t.Errorf("insufficientPrivilege = %q, want %q", insufficientPrivilege, "42501")
	}
}
