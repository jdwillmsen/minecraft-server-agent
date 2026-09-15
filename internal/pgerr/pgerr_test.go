package pgerr

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
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

// The distinction Unreachable draws is the one a store needs to tell a
// database that is down from a statement that is wrong: the first is a blip
// to read around, the second is a bug to report.
func TestUnreachableSeparatesSilenceFromAnAnswer(t *testing.T) {
	cases := map[string]struct {
		err  error
		want bool
	}{
		"a refused dial, wrapped as a store wraps it": {
			err:  fmt.Errorf("authcache: read token: %w", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}),
			want: true,
		},
		"a read that timed out": {
			err:  fmt.Errorf("authcache: read token: %w", &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}),
			want: true,
		},
		"a statement that outran its deadline": {
			err:  fmt.Errorf("authcache: read token: %w", context.DeadlineExceeded),
			want: true,
		},
		"a connection the driver had already closed": {
			err:  fmt.Errorf("authcache: read token: %w", pgconn.ErrConnClosed),
			want: true,
		},
		// The server answered, so whatever went wrong is not the network.
		"a missing table":       {err: &pgconn.PgError{Code: "42P01"}},
		"a constraint we broke": {err: &pgconn.PgError{Code: "23503"}},
		// This process shutting down is not the database going away.
		"a cancelled statement": {err: fmt.Errorf("authcache: read token: %w", context.Canceled)},
		"an ordinary error":     {err: errors.New("cached token has no refresh token")},
		"no error at all":       {err: nil},
	}

	for name, tc := range cases {
		if got := Unreachable(tc.err); got != tc.want {
			t.Errorf("%s: Unreachable = %v, want %v", name, got, tc.want)
		}
	}
}
