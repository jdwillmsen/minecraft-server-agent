package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

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
	if err := got.Save(ctx, &oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
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
	if err := file.Save(ctx, &oauth2.Token{AccessToken: "a", RefreshToken: "on-the-volume"}); err != nil {
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
	if err := got.Save(ctx, &oauth2.Token{AccessToken: "b", RefreshToken: "rotated"}); err != nil {
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
	if err := got.Save(ctx, &oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
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

func TestTokenWriteGate_ClosedUntilThisProcessIsLive(t *testing.T) {
	var g tokenWriteGate
	if g.isOpen() {
		t.Error("a process that has not won a turn may not write a token")
	}
	g.open()
	if !g.isOpen() {
		t.Error("the live agent must be able to write")
	}
	g.close()
	if g.isOpen() {
		t.Error("a process that handed over may not still be writing")
	}
}

// The gate is read from gophertunnel's refresh goroutine while the main loop
// opens and closes it around each turn.
func TestTokenWriteGate_IsSafeForConcurrentUse(t *testing.T) {
	var g tokenWriteGate
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); g.open(); g.close() }()
		go func() { defer wg.Done(); _ = g.isOpen() }()
	}
	wg.Wait()
}

// A standby that cannot read the store must not print a device code into a
// pod log nobody is watching, and must not be mistaken for a cold start.
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

type unavailableStore struct{}

func (unavailableStore) Load(context.Context) (*oauth2.Token, error) {
	return nil, mcauth.ErrStoreUnavailable
}

func (unavailableStore) Save(context.Context, *oauth2.Token) error {
	return mcauth.ErrStoreUnavailable
}
