package main

import (
	"context"
	"sync"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/moderation"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/internal/roster"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// moderationPruneInterval is how often flags older than the retention
// period are deleted. Daily is plenty for a 90-day limit, and a restart
// prunes at startup anyway.
const moderationPruneInterval = 24 * time.Hour

// moderationPruneTimeout bounds one prune. It is longer than a command's
// budget because nobody is waiting on it, and a first prune after a long
// outage may have weeks of rows to delete.
const moderationPruneTimeout = 30 * time.Second

// moderationLog is the moderation store as this binary uses it: every error
// still reaches the caller, and the one a deploy causes is also said once.
//
// Errors are passed through rather than swallowed, unlike auditTrail's.
// The plugin decides from a failed write that operators should not be
// pointed at a flag that is not there, and !modlog answers a missing table
// to the operator who asked. What this adds is the line an operator reads
// when a release looks wrong, said once, for the reason outbox gives.
type moderationLog struct {
	store   moderation.Store
	log     *logging.Logger
	unready sync.Once
}

func newModerationLog(store moderation.Store, log *logging.Logger) *moderationLog {
	return &moderationLog{store: store, log: log}
}

var _ moderation.Store = (*moderationLog)(nil)

func (m *moderationLog) noteUnready(op string, err error) {
	if !pgerr.Unready(err) {
		return
	}
	m.unready.Do(func() {
		m.log.Info("moderation_store_unready", logging.Fields{"op": op, "error": err.Error()})
	})
}

func (m *moderationLog) Record(ctx context.Context, e moderation.Event) error {
	err := m.store.Record(ctx, e)
	m.noteUnready("record", err)
	return err
}

func (m *moderationLog) Recent(ctx context.Context, xuid string, limit int) ([]moderation.Event, error) {
	events, err := m.store.Recent(ctx, xuid, limit)
	m.noteUnready("recent", err)
	return events, err
}

func (m *moderationLog) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	n, err := m.store.Prune(ctx, cutoff)
	m.noteUnready("prune", err)
	return n, err
}

func (m *moderationLog) Enabled() bool { return m.store.Enabled() }

// pruneModerationLog deletes flags past moderation.Retention now and then
// every interval, until ctx ends. It runs for the process, not the session:
// the database has nothing to do with the Bedrock connection.
//
// The retention promise is only true if this runs, so a failure other than
// the deploy-ordering one is logged at ERROR every time rather than once.
func pruneModerationLog(ctx context.Context, store moderation.Store, interval time.Duration, log *logging.Logger) {
	if !store.Enabled() {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		pruneCtx, cancel := context.WithTimeout(ctx, moderationPruneTimeout)
		n, err := store.Prune(pruneCtx, time.Now().Add(-moderation.Retention))
		cancel()
		switch {
		case err != nil && (pgerr.Unready(err) || ctx.Err() != nil):
		case err != nil:
			log.Error("moderation_prune_failed", logging.Fields{"error": err.Error()})
		case n > 0:
			log.Info("moderation_pruned", logging.Fields{"rows": n})
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// rosterName is the gamertag the live roster holds for xuid, or xuid itself
// when it holds none. It is the same resolution handleCommand and
// handleMention make, from the one place an XUID becomes a name.
func rosterName(r *roster.Roster, xuid string) string {
	if r != nil {
		if name, ok := r.NameFor(xuid); ok && name != "" {
			return name
		}
	}
	return xuid
}
