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
