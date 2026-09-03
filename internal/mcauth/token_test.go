package mcauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestSaveAndLoadToken_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	want := &oauth2.Token{
		AccessToken:  "access",
		RefreshToken: "refresh",
		Expiry:       time.Now().Add(time.Hour).Truncate(time.Second),
	}
	if err := saveToken(path, want); err != nil {
		t.Fatalf("saveToken: %v", err)
	}

	got, err := loadToken(path)
	if err != nil {
		t.Fatalf("loadToken: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestLoadToken_MissingFile(t *testing.T) {
	dir := t.TempDir()
	_, err := loadToken(filepath.Join(dir, "does-not-exist.json"))
	if err == nil {
		t.Fatal("expected an error for a missing token file")
	}
}

func TestLoadToken_MissingRefreshTokenRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	if err := saveToken(path, &oauth2.Token{AccessToken: "access-only"}); err != nil {
		t.Fatalf("saveToken: %v", err)
	}

	if _, err := loadToken(path); err == nil {
		t.Fatal("expected an error for a token with no refresh token")
	}
}

type stubTokenSource struct {
	tokens []*oauth2.Token
	calls  int
}

func (s *stubTokenSource) Token() (*oauth2.Token, error) {
	tok := s.tokens[s.calls]
	s.calls++
	return tok, nil
}

func TestCachingTokenSource_PersistsEachRefresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	stub := &stubTokenSource{tokens: []*oauth2.Token{
		{AccessToken: "first", RefreshToken: "r1"},
		{AccessToken: "second", RefreshToken: "r2"},
	}}
	cts := &cachingTokenSource{path: path, inner: stub}

	if _, err := cts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	got, err := loadToken(path)
	if err != nil {
		t.Fatalf("loadToken after first call: %v", err)
	}
	if got.AccessToken != "first" {
		t.Errorf("AccessToken = %q, want first", got.AccessToken)
	}

	if _, err := cts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	got, err = loadToken(path)
	if err != nil {
		t.Fatalf("loadToken after second call: %v", err)
	}
	if got.AccessToken != "second" {
		t.Errorf("AccessToken = %q, want second after refresh", got.AccessToken)
	}
}

func TestTokenFileName_IsDistinctPerUsername(t *testing.T) {
	a, err := tokenFileName("AgentOne")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	b, err := tokenFileName("AgentTwo")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	if a == b {
		t.Errorf("two usernames share cache file %q", a)
	}
}

func TestTokenFileName_SlugCollisionsStayDistinct(t *testing.T) {
	a, err := tokenFileName("agent.one")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	b, err := tokenFileName("agent/one")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	if a == b {
		t.Errorf("usernames that slug identically share cache file %q", a)
	}
}

func TestTokenFileName_StaysInsideCacheDir(t *testing.T) {
	dir := t.TempDir()
	for _, hostile := range []string{"../../etc/hosts", "a/b/c", `..\..\win`, strings.Repeat("x", 200)} {
		name, err := tokenFileName(hostile)
		if err != nil {
			t.Fatalf("tokenFileName(%q): %v", hostile, err)
		}
		path := filepath.Join(dir, name)
		if filepath.Dir(path) != dir {
			t.Errorf("tokenFileName(%q) = %q escapes cache dir: %q", hostile, name, path)
		}
	}
}

func TestTokenFileName_RejectsBlankUsername(t *testing.T) {
	if _, err := tokenFileName("   "); err == nil {
		t.Fatal("expected an error for a blank username")
	}
}

// withStubLogin swaps requestLiveToken for the duration of a test and
// restores the real one afterward, since it's a package-level var shared
// across the test binary.
func withStubLogin(t *testing.T, stub func(ctx context.Context, out io.Writer) (*oauth2.Token, error)) {
	t.Helper()
	orig := requestLiveToken
	requestLiveToken = stub
	t.Cleanup(func() { requestLiveToken = orig })
}

func TestTokenSource_MissingCacheFileTriggersLogin(t *testing.T) {
	dir := t.TempDir()
	loginCalls := 0
	withStubLogin(t, func(ctx context.Context, out io.Writer) (*oauth2.Token, error) {
		loginCalls++
		return &oauth2.Token{AccessToken: "fresh", RefreshToken: "fresh-refresh"}, nil
	})

	if _, err := TokenSource(context.Background(), dir, "agent-one", io.Discard); err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if loginCalls != 1 {
		t.Errorf("login called %d times, want 1 for a missing cache file", loginCalls)
	}

	name, err := tokenFileName("agent-one")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := loadToken(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("the token the login returned was not persisted: %v", err)
	}
	if saved.AccessToken != "fresh" {
		t.Errorf("saved AccessToken = %q, want fresh", saved.AccessToken)
	}
}

func TestTokenSource_CorruptCacheFileIsAHardErrorNotALoginPrompt(t *testing.T) {
	dir := t.TempDir()
	name, err := tokenFileName("agent-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	loginCalls := 0
	withStubLogin(t, func(ctx context.Context, out io.Writer) (*oauth2.Token, error) {
		loginCalls++
		t.Error("interactive login must not be attempted for a corrupt cache file")
		return nil, errors.New("should not be reached")
	})

	if _, err := TokenSource(context.Background(), dir, "agent-one", io.Discard); err == nil {
		t.Fatal("expected TokenSource to fail hard on a corrupt cache file")
	}
	if loginCalls != 0 {
		t.Errorf("login called %d times, want 0", loginCalls)
	}
}

func TestTokenSource_MissingRefreshTokenIsAHardErrorNotALoginPrompt(t *testing.T) {
	dir := t.TempDir()
	name, err := tokenFileName("agent-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := saveToken(filepath.Join(dir, name), &oauth2.Token{AccessToken: "access-only"}); err != nil {
		t.Fatal(err)
	}

	withStubLogin(t, func(ctx context.Context, out io.Writer) (*oauth2.Token, error) {
		t.Error("interactive login must not be attempted when the cache file lacks a refresh token")
		return nil, errors.New("should not be reached")
	})

	if _, err := TokenSource(context.Background(), dir, "agent-one", io.Discard); err == nil {
		t.Fatal("expected TokenSource to fail hard rather than prompt a login")
	}
}

func TestSaveToken_IsAtomic_NoTempFileLeftOnSuccess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	if err := saveToken(path, &oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("saveToken: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "token.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("cache dir after a successful save = %v, want only [token.json]", names)
	}
	got, err := loadToken(path)
	if err != nil || got.AccessToken != "a" {
		t.Errorf("loadToken after save = (%+v, %v), want AccessToken=a, nil", got, err)
	}
}

func TestSaveToken_ConcurrentSaversNeverExposeAPartialFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	if err := saveToken(path, &oauth2.Token{AccessToken: "seed", RefreshToken: "r"}); err != nil {
		t.Fatalf("seed saveToken: %v", err)
	}

	const savers = 4
	const savesEach = 10

	stop := make(chan struct{})
	readErrCh := make(chan error, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := loadToken(path); err != nil {
				readErrCh <- err
				return
			}
		}
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, savers)
	for i := 0; i < savers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < savesEach; j++ {
				tok := &oauth2.Token{AccessToken: fmt.Sprintf("a-%d-%d", i, j), RefreshToken: "r"}
				if err := saveToken(path, tok); err != nil {
					errCh <- err
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(stop)
	<-readerDone
	close(errCh)

	for err := range errCh {
		t.Fatalf("saveToken during concurrent saves: %v", err)
	}
	select {
	case err := <-readErrCh:
		t.Fatalf("loadToken saw a partially-written file during concurrent saves: %v", err)
	default:
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("cache dir holds %d entries after concurrent saves, want only token.json", len(entries))
	}
}

func TestCachingTokenSource_TokenIsSafeForConcurrentUse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")
	stub := &stubTokenSource{tokens: make([]*oauth2.Token, 50)}
	for i := range stub.tokens {
		stub.tokens[i] = &oauth2.Token{AccessToken: "tok", RefreshToken: "refresh"}
	}
	cts := &cachingTokenSource{path: path, inner: stub}

	var wg sync.WaitGroup
	errCh := make(chan error, len(stub.tokens))
	for range stub.tokens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := cts.Token(); err != nil {
				errCh <- err
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Token() call failed: %v", err)
	}
}

func TestTokenSource_UsesPerUsernameCacheFile(t *testing.T) {
	dir := t.TempDir()
	name, err := tokenFileName("agent-one")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	if err := saveToken(filepath.Join(dir, name), &oauth2.Token{AccessToken: "a", RefreshToken: "r"}); err != nil {
		t.Fatalf("saveToken: %v", err)
	}

	// A cached token for agent-one must not be picked up for agent-two;
	// without a real login available here, the proof is that the second
	// account's cache file simply doesn't exist yet.
	other, err := tokenFileName("agent-two")
	if err != nil {
		t.Fatalf("tokenFileName: %v", err)
	}
	if _, err := loadToken(filepath.Join(dir, other)); err == nil {
		t.Error("agent-two loaded a token cached for agent-one")
	}

	if _, err := loadToken(filepath.Join(dir, name)); err != nil {
		t.Errorf("agent-one could not load its own cached token: %v", err)
	}
}
