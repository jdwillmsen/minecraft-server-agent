package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/internal/store"
)

// flakyDatabase refuses its first `failures` opens with err, then connects.
type flakyDatabase struct {
	failures int32
	err      error
	calls    atomic.Int32
}

func (d *flakyDatabase) open(context.Context, string, time.Duration) (*store.Postgres, error) {
	if d.calls.Add(1) <= d.failures {
		return nil, d.err
	}
	return &store.Postgres{}, nil
}

func storeConfig() config.Config {
	return config.Config{PGHost: "pg.invalid", PGPort: 5432, PGDatabase: "app", PGConnectTimeoutMs: 10}
}

func fastStoreRetries(t *testing.T, window time.Duration) {
	t.Helper()
	origWindow, origFirst, origMax := storeOpenWindow, storeRetryFirstWait, storeRetryMaxWait
	storeOpenWindow, storeRetryFirstWait, storeRetryMaxWait = window, time.Millisecond, 4*time.Millisecond
	t.Cleanup(func() {
		storeOpenWindow, storeRetryFirstWait, storeRetryMaxWait = origWindow, origFirst, origMax
	})
}

// The failure this exists for: one pod briefly unable to reach a healthy
// database. A single failed ping used to settle the agent on no store, and
// with the token kept in that store, on no login either.
func TestOpenStore_RetriesAFailedConnect(t *testing.T) {
	fastStoreRetries(t, time.Minute)
	db := &flakyDatabase{failures: 1, err: errors.New("store: ping: context deadline exceeded")}

	got := openStore(context.Background(), storeConfig(), quietLogger(), db.open)

	if _, ok := got.(*store.Postgres); !ok {
		t.Fatalf("openStore = %T, want *store.Postgres after one failed attempt", got)
	}
	if n := db.calls.Load(); n != 2 {
		t.Errorf("open called %d times, want 2", n)
	}
}

// Bounded: a database that is really gone must become a failure an operator
// sees, not a process that waits forever.
func TestOpenStore_GivesUpAfterTheWindow(t *testing.T) {
	fastStoreRetries(t, 50*time.Millisecond)
	db := &flakyDatabase{failures: 1 << 30, err: errors.New("store: ping: connection refused")}

	start := time.Now()
	got := openStore(context.Background(), storeConfig(), quietLogger(), db.open)

	if _, ok := got.(store.Nop); !ok {
		t.Fatalf("openStore = %T, want store.Nop once the window is spent", got)
	}
	if n := db.calls.Load(); n < 3 {
		t.Errorf("open called %d times, want several within the window", n)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("openStore took %v, want it bounded by the 50ms window", elapsed)
	}
}

func TestOpenStore_DoesNotRetryAMalformedDSN(t *testing.T) {
	fastStoreRetries(t, time.Minute)
	db := &flakyDatabase{failures: 1 << 30, err: fmt.Errorf("%w: bad port", store.ErrInvalidDSN)}

	got := openStore(context.Background(), storeConfig(), quietLogger(), db.open)

	if _, ok := got.(store.Nop); !ok {
		t.Fatalf("openStore = %T, want store.Nop", got)
	}
	if n := db.calls.Load(); n != 1 {
		t.Errorf("open called %d times, want 1: a malformed DSN reads the same every time", n)
	}
}

func TestOpenStore_StopsWaitingOnShutdown(t *testing.T) {
	fastStoreRetries(t, time.Hour)
	storeRetryFirstWait, storeRetryMaxWait = time.Hour, time.Hour
	db := &flakyDatabase{failures: 1 << 30, err: errors.New("store: ping: connection refused")}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan any)
	go func() { done <- openStore(ctx, storeConfig(), quietLogger(), db.open) }()
	cancel()

	select {
	case got := <-done:
		if _, ok := got.(store.Nop); !ok {
			t.Fatalf("openStore = %T, want store.Nop", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("openStore kept waiting after the context was cancelled")
	}
}

func TestOpenStore_WithoutPGHostDoesNotTry(t *testing.T) {
	db := &flakyDatabase{}

	got := openStore(context.Background(), config.Config{}, quietLogger(), db.open)

	if _, ok := got.(store.Nop); !ok {
		t.Fatalf("openStore = %T, want store.Nop", got)
	}
	if n := db.calls.Load(); n != 0 {
		t.Errorf("open called %d times, want 0", n)
	}
}

// With a database configured and not opened, the cache directory failing is
// the expected state of a pod with no volume. The error has to lead with the
// database, or an operator goes looking at the directory.
func TestOpenTokenStore_WithAnUnopenedDatabaseNamesTheDatabase(t *testing.T) {
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := authConfig(filepath.Join(blocked, "auth"))
	cfg.PGHost = "pg.invalid"

	_, err := openTokenStore(cfg, nil, quietLogger())

	if !errors.Is(err, errTokenStoreNeedsDatabase) {
		t.Fatalf("openTokenStore error = %v, want it to name the database", err)
	}
	if !strings.HasPrefix(err.Error(), errTokenStoreNeedsDatabase.Error()) {
		t.Errorf("error %q does not lead with the database", err)
	}
}
