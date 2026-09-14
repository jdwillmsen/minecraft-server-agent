// Package authcache keeps the Xbox Live token where every agent process can
// reach it, rather than on one pod's volume.
//
// The volume is the reason this exists. A ReadWriteOnce claim can be mounted
// by one pod, so a standby scheduled onto another node cannot read the token
// at all -- it waits in ContainerCreating until the pod it is replacing lets
// the volume go, which is the absence a warm standby is supposed to remove.
// The database is already open on this process for the profile store and the
// leader lock, so putting the token there costs no new dependency and no new
// Kubernetes object.
package authcache

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/mcauth"
)

// Table is the relation this store reads and writes, named here so the
// README can quote one source for the DDL a migration in jdwlabs/platform
// has to create.
const Table = "minecraft.auth_tokens"

// Postgres stores one row per Xbox Live account, sharing the profile store's
// pool.
//
// A file cache was protected by its 0600 mode. There is no such thing for a
// row, and the replacement is the grant: only the agent's role may select
// from or write to this table, and nothing else in the schema references it.
// That is the whole of the protection at rest, deliberately stated rather
// than implied, because a reader who expects the file's mode to have an
// equivalent here will otherwise go looking for one.
type Postgres struct {
	pool    *pgxpool.Pool
	account string
	// table is interpolated into every statement below, because a relation
	// name is an identifier and cannot be a bind parameter. It is never
	// input: NewPostgres sets it from the constant above, and only a test
	// aiming at a relation that deliberately does not exist sets it to
	// anything else.
	table string
}

var _ mcauth.Store = (*Postgres)(nil)

// NewPostgres binds a store to one account. The account is MC_USERNAME --
// the same identity the leader lock is keyed on, because the thing two
// processes cannot share is the login, not the deployment.
func NewPostgres(pool *pgxpool.Pool, account string) *Postgres {
	return &Postgres{pool: pool, account: account, table: Table}
}

func (p *Postgres) Load(ctx context.Context) (*oauth2.Token, error) {
	var encoded []byte
	err := p.pool.QueryRow(ctx,
		`SELECT token FROM `+p.table+` WHERE account = $1`,
		p.account,
	).Scan(&encoded)
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		return nil, mcauth.ErrNoToken
	case unavailable(err):
		return nil, fmt.Errorf("%w: %v", mcauth.ErrStoreUnavailable, err)
	default:
		return nil, fmt.Errorf("authcache: read token: %w", err)
	}
	return mcauth.DecodeToken(encoded)
}

func (p *Postgres) Save(ctx context.Context, tok *oauth2.Token) error {
	encoded, err := mcauth.EncodeToken(tok)
	if err != nil {
		return err
	}
	if _, err := p.pool.Exec(ctx, `
		INSERT INTO `+p.table+` (account, token, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (account) DO UPDATE
		SET token = EXCLUDED.token, updated_at = EXCLUDED.updated_at`,
		p.account, string(encoded),
	); err != nil {
		if unavailable(err) {
			return fmt.Errorf("%w: %v", mcauth.ErrStoreUnavailable, err)
		}
		return fmt.Errorf("authcache: write token: %w", err)
	}
	return nil
}

// unavailable reports the failures that mean this store cannot say whether it
// holds a token, as opposed to saying it holds none.
//
// Two ways in: the agent released ahead of its migration or ahead of the grant
// that migration owes it, and the database not answering at all. They are one
// answer here because the caller does the same thing with both -- fall through
// to the file cache, and fail loudly if that cannot answer either. What
// neither may become is ErrNoToken: an empty store is licence to print a
// device code into a pod log, and a database problem that clears itself must
// never buy one.
func unavailable(err error) bool { return pgerr.Unready(err) || pgerr.Unreachable(err) }
