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
package pgerr

import (
	"errors"

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
