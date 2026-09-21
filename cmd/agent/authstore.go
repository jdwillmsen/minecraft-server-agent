package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"

	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/mcauth"
)

// tokenStoreAttempts bounds how many times the token store is asked for the
// cached token before the process gives up and exits.
const tokenStoreAttempts = 5

// tokenStoreRetryDelay is how long to wait between those attempts. A var so a
// test need not spend the real one.
var tokenStoreRetryDelay = 2 * time.Second

// errTokenStoreNeedsDatabase is the startup failure of an agent configured
// with a database it could not open: the database is where the Xbox token
// lives, so without it there is no login.
var errTokenStoreNeedsDatabase = errors.New("no token store: the Xbox token is kept in Postgres, which could not be opened at startup")

// openTokenStore decides where this process caches its Xbox Live token.
//
// shared is the database-backed store, or nil when there is no database --
// the same condition that leaves the agent with no lock and no persistence.
// It is preferred whenever it exists, because it is the only one a standby
// on another node can read: the file cache lives on a ReadWriteOnce volume
// that one pod at a time may mount.
//
// The file cache stays behind it as a read-through fallback rather than
// being dropped, and does two jobs there. It is the whole store for local
// development, where there is no database at all. And in the cluster it is
// where the token still is on the first run after this change: the database
// row does not exist yet, the load falls through to the file, and the first
// refresh the live agent writes lands in the database. From then on the file
// is never read again and the volume can go.
//
// A cache directory that cannot be created is not fatal when a database
// store exists -- once the volume is gone that is the expected state, not a
// failure -- and is fatal when it is all there is.
//
// With PG_HOST set, shared is nil only because the database could not be
// opened, and that is the cause worth reporting: the file cache failing
// behind it is the expected state of a pod with no volume, and naming only
// that sends an operator looking at a directory nobody meant to exist.
func openTokenStore(cfg config.Config, shared mcauth.Store, log *logging.Logger) (mcauth.Store, error) {
	file, err := mcauth.NewFileStore(cfg.AuthCacheDir, cfg.MCUsername)
	if err != nil {
		if shared == nil && cfg.PGHost != "" {
			return nil, fmt.Errorf("%w (see store_open_failed); the file cache under AUTH_CACHE_DIR is no substitute: %w", errTokenStoreNeedsDatabase, err)
		}
		if shared == nil {
			return nil, err
		}
		log.Info("auth_store", logging.Fields{"store": "postgres", "file_cache_error": err.Error()})
		return shared, nil
	}
	if shared == nil {
		log.Info("auth_store", logging.Fields{"store": "file", "path": file.Path()})
		return file, nil
	}
	log.Info("auth_store", logging.Fields{"store": "postgres", "fallback_path": file.Path()})
	return mcauth.NewFallback(shared, file), nil
}

// tokenLiveGate says whether this process currently holds the account's Xbox
// Live login, and so may refresh it, persist the result, and answer a
// first-run device code.
//
// Microsoft retires the refresh token as it issues the replacement, so a
// second process that refreshes revokes the credential the first one is
// playing on -- the damage is the refresh, not the write. Exactly one
// process may do it, and that is the one holding the lock, so the gate opens
// with the turn and closes with it. A standby stays warm by re-reading what
// the live agent stored, not by rotating anything of its own.
type tokenLiveGate struct {
	live atomic.Bool
}

func (g *tokenLiveGate) open()  { g.live.Store(true) }
func (g *tokenLiveGate) close() { g.live.Store(false) }

func (g *tokenLiveGate) isOpen() bool { return g.live.Load() }

// newTokenSource builds the agent's token source, waiting out a store that
// cannot answer yet.
//
// Once the volume is dropped the database is the only store there is, and a
// database that is briefly unreachable is the ordinary cost of a Postgres
// failover or a node moving. Exiting on the first one turns a two-second blip
// at pod start into CrashLoopBackOff, which is the outage this agent is built
// to ride out, not to join.
//
// Bounded, because the other way a store cannot answer is a release that
// landed ahead of the migration that gives it its table -- and that does not
// clear on its own. Waiting forever would hide it; a handful of attempts
// leaves it as a failure an operator sees. Nothing else is retried: a corrupt
// entry reads the same every time.
func newTokenSource(ctx context.Context, store mcauth.Store, out io.Writer, log *logging.Logger, opts ...mcauth.Option) (oauth2.TokenSource, error) {
	for attempt := 1; ; attempt++ {
		ts, err := mcauth.TokenSource(ctx, store, out, opts...)
		if err == nil {
			return ts, nil
		}
		if !errors.Is(err, mcauth.ErrStoreUnavailable) || attempt == tokenStoreAttempts {
			return nil, err
		}
		log.Info("auth_store_unavailable_retrying", logging.Fields{"attempt": attempt})
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(tokenStoreRetryDelay):
		}
	}
}
