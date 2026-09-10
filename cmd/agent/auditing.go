package main

import (
	"context"
	"sync"

	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/internal/pgerr"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// auditTrail is the command audit store as this binary uses it: the same
// writes, with the one failure a deploy causes rather than a bug reported
// once instead of on every command.
//
// The agent may be released ahead of the migration that creates its table,
// or ahead of the grants on it. Either way every dispatch -- and every
// dispatch is audited -- writes an error line naming the same unchanging
// fact, which buries whatever else the log had to say for as long as the
// two are out of step. Said once, at INFO, it is a deploy-ordering notice;
// repeated at ERROR it is noise that trains an operator to ignore the level
// that means something.
type auditTrail struct {
	store audit.Store
	log   *logging.Logger
	// unready fires for the first refusal only. A deploy-ordering state
	// does not change while the process runs: either a later deploy fixes
	// it, which restarts this process, or it stays true and nothing is
	// learned by saying so again.
	unready sync.Once
}

func newAuditTrail(store audit.Store, log *logging.Logger) *auditTrail {
	return &auditTrail{store: store, log: log}
}

var _ audit.Store = (*auditTrail)(nil)

func (a *auditTrail) Enabled() bool { return a.store.Enabled() }

// Write records one dispatch, swallowing the refusals it has already
// reported.
//
// Swallowed rather than returned because the caller's only response to an
// error here is to log one -- at ERROR, per command -- which is exactly the
// noise this type exists to remove. Every other failure is passed through
// untouched: a gap in the compliance trail for any other reason is still
// something an operator has to be shouted at about.
func (a *auditTrail) Write(ctx context.Context, r audit.Record) error {
	err := a.store.Write(ctx, r)
	if pgerr.Unready(err) {
		// Counted every time even though it is logged once. The log line is
		// said once because repeating it tells a reader nothing new; the
		// counter answers a different question -- is the trail whole -- and
		// every one of these is a dispatch it does not hold. Left uncounted, a
		// release that outran its migration would lose every record while the
		// failure alert read zero. Counted here because the caller, which
		// counts every other failure, never sees this one.
		metrics.AuditWriteFailure()
		a.unready.Do(func() {
			a.log.Info("audit_store_unready", logging.Fields{"error": err.Error()})
		})
		return nil
	}
	return err
}
