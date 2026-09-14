package main

import (
	"sync/atomic"

	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/mcauth"
)

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
func openTokenStore(cfg config.Config, shared mcauth.Store, log *logging.Logger) (mcauth.Store, error) {
	file, err := mcauth.NewFileStore(cfg.AuthCacheDir, cfg.MCUsername)
	if err != nil {
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

// tokenWriteGate says whether this process may persist a refreshed token.
//
// Microsoft rotates the refresh token on every refresh, so the copy a
// standby leaves behind after refreshing is one the live agent can no longer
// use. Only one of the two processes may write, and the live agent is the
// one whose token is being used to hold the login -- so the gate opens with
// the turn and closes with the handover.
//
// A standby still refreshes: paying that round trip before it is needed is
// the point of a warm standby. It simply keeps the result to itself, and
// writes it the moment it goes live.
type tokenWriteGate struct {
	allowed atomic.Bool
}

func (g *tokenWriteGate) open()  { g.allowed.Store(true) }
func (g *tokenWriteGate) close() { g.allowed.Store(false) }

func (g *tokenWriteGate) isOpen() bool { return g.allowed.Load() }
