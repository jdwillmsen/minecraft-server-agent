package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/mcauth"
)

// sharedStore stands in for the database-backed store, which is the only
// thing about it that matters here: two processes reach the same one.
type sharedStore struct {
	mu    sync.Mutex
	token *oauth2.Token
	saves int
}

func (s *sharedStore) Load(context.Context) (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == nil {
		return nil, mcauth.ErrNoToken
	}
	return s.token, nil
}

func (s *sharedStore) Save(_ context.Context, tok *oauth2.Token) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves++
	s.token = tok
	return nil
}

// storable is a token the stores will take back out again: mcauth refuses one
// that has no refresh token to rotate with, or no expiry to rotate at.
func storable(access, refresh string) *oauth2.Token {
	return &oauth2.Token{AccessToken: access, RefreshToken: refresh, Expiry: time.Now().Add(time.Hour)}
}

func authConfig(dir string) config.Config {
	return config.Config{AuthCacheDir: dir, MCUsername: "agent-one"}
}

func quietLogger() *logging.Logger { return logging.New("error") }

func TestOpenTokenStore_WithoutADatabaseUsesTheFileCache(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	got, err := openTokenStore(authConfig(dir), nil, quietLogger())
	if err != nil {
		t.Fatalf("openTokenStore: %v", err)
	}
	if err := got.Save(ctx, storable("a", "r")); err != nil {
		t.Fatalf("Save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("cache dir holds %d entries, want the one token file", len(entries))
	}
}

func TestOpenTokenStore_PrefersTheDatabaseAndReadsTheFileThrough(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// The state of a pod on the first release after this change: the token
	// is still on the volume and nothing has written it to the database.
	file, err := mcauth.NewFileStore(dir, "agent-one")
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	if err := file.Save(ctx, storable("a", "on-the-volume")); err != nil {
		t.Fatalf("file Save: %v", err)
	}

	shared := &sharedStore{}
	got, err := openTokenStore(authConfig(dir), shared, quietLogger())
	if err != nil {
		t.Fatalf("openTokenStore: %v", err)
	}

	tok, err := got.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if tok.RefreshToken != "on-the-volume" {
		t.Errorf("RefreshToken = %q, want the file's", tok.RefreshToken)
	}

	// And the first write goes to the database, which is what retires the
	// volume without anyone reading a refresh token out of a pod.
	if err := got.Save(ctx, storable("b", "rotated")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if shared.saves != 1 {
		t.Errorf("database saves = %d, want 1", shared.saves)
	}
	tok, err = got.Load(ctx)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if tok.RefreshToken != "rotated" {
		t.Errorf("RefreshToken = %q, want the database's after it was written", tok.RefreshToken)
	}
	onDisk, err := file.Load(ctx)
	if err != nil {
		t.Fatalf("file Load: %v", err)
	}
	if onDisk.RefreshToken != "on-the-volume" {
		t.Errorf("the file was rewritten to %q: one rotating token, one writer", onDisk.RefreshToken)
	}
}

// Once the volume is gone there is no cache directory to create, and that
// is the expected state rather than a failure -- as long as the database is
// there to take its place.
func TestOpenTokenStore_UnusableCacheDirIsFatalOnlyWithoutADatabase(t *testing.T) {
	ctx := context.Background()
	blocked := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := authConfig(filepath.Join(blocked, "auth"))

	shared := &sharedStore{}
	got, err := openTokenStore(cfg, shared, quietLogger())
	if err != nil {
		t.Fatalf("openTokenStore with a database: %v", err)
	}
	if err := got.Save(ctx, storable("a", "r")); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if shared.saves != 1 {
		t.Errorf("database saves = %d, want 1", shared.saves)
	}

	if _, err := openTokenStore(cfg, nil, quietLogger()); err == nil {
		t.Fatal("expected an error when the file cache is all there is and cannot be made")
	}
}

func TestOpenTokenStore_RejectsABlankUsernameWithNoDatabase(t *testing.T) {
	cfg := config.Config{AuthCacheDir: t.TempDir(), MCUsername: "  "}
	if _, err := openTokenStore(cfg, nil, quietLogger()); err == nil {
		t.Fatal("expected an error for a blank username")
	}
}

func TestTokenLiveGate_ClosedUntilThisProcessIsLive(t *testing.T) {
	var g tokenLiveGate
	if g.isOpen() {
		t.Error("a process that has not won a turn may not rotate the token")
	}
	g.open()
	if !g.isOpen() {
		t.Error("the live agent must be able to refresh and write")
	}
	g.close()
	if g.isOpen() {
		t.Error("a process that handed over may not still be rotating")
	}
}

// The gate is read from gophertunnel's refresh goroutine while the main loop
// opens and closes it around each turn.
func TestTokenLiveGate_IsSafeForConcurrentUse(t *testing.T) {
	var g tokenLiveGate
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); g.open(); g.close() }()
		go func() { defer wg.Done(); _ = g.isOpen() }()
	}
	wg.Wait()
}

// A store that cannot be read must not be mistaken for a cold start, and
// that holds all the way through the wiring this file builds: an unavailable
// database with the volume already gone is the ordinary state of a pod
// released ahead of its migration.
func TestTokenSourceOverAnUnavailableStoreFailsRatherThanPrompting(t *testing.T) {
	unavailable := unavailableStore{}
	_, err := mcauth.TokenSource(context.Background(), unavailable, os.Stdout)
	if err == nil {
		t.Fatal("expected an error rather than a device-code prompt")
	}
	if !errors.Is(err, mcauth.ErrStoreUnavailable) {
		t.Errorf("error = %v, want it to carry ErrStoreUnavailable", err)
	}
}

func TestTokenSourceOverAnUnavailableDatabaseAndNoVolumeStillFailsLoudly(t *testing.T) {
	cfg := authConfig(t.TempDir())
	store, err := openTokenStore(cfg, unavailableStore{}, quietLogger())
	if err != nil {
		t.Fatalf("openTokenStore: %v", err)
	}

	_, err = mcauth.TokenSource(context.Background(), store, os.Stdout)
	if err == nil {
		t.Fatal("an unavailable database with an empty cache dir was taken for a cold start")
	}
	if !errors.Is(err, mcauth.ErrStoreUnavailable) {
		t.Errorf("error = %v, want it to carry ErrStoreUnavailable", err)
	}
	if errors.Is(err, mcauth.ErrNoToken) {
		t.Error("reported as an empty store, which is licence to print a device code")
	}
}

type unavailableStore struct{}

func (unavailableStore) Load(context.Context) (*oauth2.Token, error) {
	return nil, mcauth.ErrStoreUnavailable
}

func (unavailableStore) Save(context.Context, *oauth2.Token) error {
	return mcauth.ErrStoreUnavailable
}

// flakyStore cannot answer for its first failures loads, then holds a token
// like any other store: a database that was still coming up when the pod did.
type flakyStore struct {
	mu       sync.Mutex
	failures int
	loads    int
	token    *oauth2.Token
}

func (f *flakyStore) Load(context.Context) (*oauth2.Token, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.loads <= f.failures {
		return nil, mcauth.ErrStoreUnavailable
	}
	if f.token == nil {
		return nil, mcauth.ErrNoToken
	}
	return f.token, nil
}

func (f *flakyStore) Save(_ context.Context, tok *oauth2.Token) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token = tok
	return nil
}

func (f *flakyStore) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

func withRetryDelay(t *testing.T, d time.Duration) {
	t.Helper()
	orig := tokenStoreRetryDelay
	tokenStoreRetryDelay = d
	t.Cleanup(func() { tokenStoreRetryDelay = orig })
}

// With the volume dropped the database is the only store there is, so a
// two-second blip while the pod starts would otherwise be an exit and a crash
// loop -- for exactly the outage the agent is supposed to survive.
func TestNewTokenSource_WaitsOutAStoreThatCannotAnswerYet(t *testing.T) {
	withRetryDelay(t, time.Millisecond)
	store := &flakyStore{failures: 2, token: storable("a", "r")}

	ts, err := newTokenSource(context.Background(), store, io.Discard, quietLogger())
	if err != nil {
		t.Fatalf("newTokenSource: %v", err)
	}
	if ts == nil {
		t.Fatal("no token source")
	}
	if store.loadCount() != 3 {
		t.Errorf("loaded %d times, want the two failures and the answer", store.loadCount())
	}
}

// Bounded, because the other way a store cannot answer is a release that
// landed ahead of its migration, and that does not clear on its own: it has
// to end as a failure an operator can see.
func TestNewTokenSource_GivesUpOnAStoreThatStaysUnavailable(t *testing.T) {
	withRetryDelay(t, time.Millisecond)
	store := &flakyStore{failures: 1000}

	if _, err := newTokenSource(context.Background(), store, io.Discard, quietLogger()); !errors.Is(err, mcauth.ErrStoreUnavailable) {
		t.Fatalf("newTokenSource = %v, want ErrStoreUnavailable", err)
	}
	if store.loadCount() != tokenStoreAttempts {
		t.Errorf("loaded %d times, want %d", store.loadCount(), tokenStoreAttempts)
	}
}

// And nothing else is retried: a corrupt entry reads the same on every
// attempt, and waiting on it only delays the operator who has to delete it.
func TestNewTokenSource_DoesNotWaitOnAFailureThatCannotClear(t *testing.T) {
	withRetryDelay(t, time.Hour)
	store := &corruptStore{}

	if _, err := newTokenSource(context.Background(), store, io.Discard, quietLogger()); err == nil {
		t.Fatal("a corrupt store was accepted")
	}
}

type corruptStore struct{}

func (corruptStore) Load(context.Context) (*oauth2.Token, error) {
	return nil, errors.New("mcauth: cached token is not valid JSON")
}

func (corruptStore) Save(context.Context, *oauth2.Token) error { return nil }
