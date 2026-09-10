package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// countingAudit is an audit.Store that fails every write with a fixed error
// and counts how many it was asked for.
type countingAudit struct {
	mu     sync.Mutex
	writes int
	err    error
}

var _ audit.Store = (*countingAudit)(nil)

func (c *countingAudit) Write(context.Context, audit.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writes++
	return c.err
}

func (c *countingAudit) Enabled() bool { return true }

func (c *countingAudit) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

// captureAgentStdout redirects the process's real stdout for the duration of
// fn, so a test can assert on a *logging.Logger's line without a writer seam.
func captureAgentStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

func auditRecord() audit.Record {
	return audit.Record{XUID: playerXUID, Gamertag: "Steve", Permission: "visitor",
		Command: "ping", Outcome: audit.OutcomeOK, At: time.Now()}
}

// An agent released ahead of its migration audits every command against a
// table that is not there. Reported once, at INFO, so the wrong deploy order
// is a notice rather than an error line per command for as long as the two
// are out of step.
func TestAuditTrailReportsAMissingTableOnceAndNotAsAFailure(t *testing.T) {
	store := &countingAudit{err: fmt.Errorf("audit: write: %w", &pgconn.PgError{
		Code: "42P01", Message: `relation "minecraft.command_audit" does not exist`,
	})}

	var errs []error
	out := captureAgentStdout(t, func() {
		// Built inside the capture: *logging.Logger resolves os.Stdout at
		// construction.
		trail := newAuditTrail(store, logging.New("info"))
		for i := 0; i < 3; i++ {
			errs = append(errs, trail.Write(context.Background(), auditRecord()))
		}
	})

	for i, err := range errs {
		if err != nil {
			t.Errorf("write %d returned %v, want the refusal swallowed after it was reported", i, err)
		}
	}
	if store.count() != 3 {
		t.Errorf("underlying store saw %d writes, want 3 -- the wrapper must not stop trying", store.count())
	}
	if got := strings.Count(out, `"event":"audit_store_unready"`); got != 1 {
		t.Errorf("stdout carried %d audit_store_unready events, want exactly 1:\n%s", got, out)
	}
}

// A grant that was never made is the same class of deploy mistake as a table
// that was never created, and the failure this project has actually had.
func TestAuditTrailReportsAMissingGrantTheSameWay(t *testing.T) {
	store := &countingAudit{err: fmt.Errorf("audit: write: %w", &pgconn.PgError{
		Code: "42501", Message: "permission denied for table command_audit",
	})}

	var err error
	out := captureAgentStdout(t, func() {
		err = newAuditTrail(store, logging.New("info")).Write(context.Background(), auditRecord())
	})

	if err != nil {
		t.Errorf("Write = %v, want the refusal swallowed after it was reported", err)
	}
	if !strings.Contains(out, `"event":"audit_store_unready"`) {
		t.Errorf("stdout = %q, want an audit_store_unready event", out)
	}
}

// Everything else still reaches the caller, which logs it at ERROR: a
// compliance gap for any other reason is not a deploy notice.
func TestAuditTrailPassesEveryOtherFailureThrough(t *testing.T) {
	store := &countingAudit{err: errors.New("connection refused")}
	trail := newAuditTrail(store, logging.New("error"))

	if err := trail.Write(context.Background(), auditRecord()); err == nil {
		t.Error("an ordinary write failure was swallowed as if it were a deploy notice")
	}
}
