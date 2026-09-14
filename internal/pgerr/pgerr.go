// Package pgerr recognises the failures a configured Postgres returns for a
// reason a deploy is responsible for rather than a bug: the tables are not
// there yet, or the role the agent connects as was never granted access to
// them. Both are states an operator can be told about and can fix, and
// neither is a reason to leave the person who typed a command with nothing.
//
// Shared rather than defined per store because every store in this repo
// borrows the same pool and fails these same two ways, and because the
// second one is not hypothetical: a migration that created tables the app
// role could not read is an incident this project has already had.
//
// It also recognises the failure where the database says nothing at all --
// see Unreachable -- because the alternative is for each store to mistake a
// database that is down for a statement that is wrong.
package pgerr

import (
	"errors"
	"net"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// undefinedTable is Postgres's SQLSTATE for "relation does not exist".
	undefinedTable = "42P01"
	// insufficientPrivilege is Postgres's SQLSTATE for a role missing the
	// grant a statement needs.
	insufficientPrivilege = "42501"
)

// code reports the SQLSTATE err carries, if it carries one at all. Wrapped
// errors are unwrapped: every store here wraps what pgx returns.
func code(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "", false
	}
	return pgErr.Code, true
}

// NotMigrated reports whether err is a statement against a table that does
// not exist yet -- the agent released ahead of its migration.
func NotMigrated(err error) bool {
	c, ok := code(err)
	return ok && c == undefinedTable
}

// NotGranted reports whether err is a statement the connecting role is not
// privileged to run -- the migration ran, the grants did not.
func NotGranted(err error) bool {
	c, ok := code(err)
	return ok && c == insufficientPrivilege
}

// Unready reports either, for the callers that only need to know the
// database cannot serve them yet and not which of the two ways it means.
func Unready(err error) bool { return NotMigrated(err) || NotGranted(err) }

// Unreachable reports whether the database never answered at all: the host
// refused the connection, the dial timed out, the socket went away
// mid-statement.
//
// Separate from Unready, which is the database answering that it is not set
// up yet. An unreachable one carries no SQLSTATE, so a caller that only asks
// for a code cannot tell a database that is merely down from a statement
// that is wrong -- and those demand opposite reactions. The pool connects
// lazily, so a database that is down when the process starts surfaces here
// on the first statement rather than when the pool is built.
func Unreachable(err error) bool {
	if err == nil {
		return false
	}
	if _, answered := code(err); answered {
		return false
	}
	var connErr *pgconn.ConnectError
	if errors.As(err, &connErr) {
		return true
	}
	// net.Error covers both the dial and read/write failures pgconn wraps
	// and context.DeadlineExceeded, which reports Timeout. Cancellation
	// deliberately does not match: that is this process shutting down, not
	// the database being gone.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, pgconn.ErrConnClosed)
}
