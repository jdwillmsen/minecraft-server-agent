package mcauth

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"golang.org/x/oauth2"
)

type stubTokenSource struct {
	mu     sync.Mutex
	tokens []*oauth2.Token
	calls  int
}

func (s *stubTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The last token repeats once the script runs out, which is what a real
	// source does between refreshes: the same token until it expires.
	tok := s.tokens[min(s.calls, len(s.tokens)-1)]
	s.calls++
	return tok, nil
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

func TestTokenSource_ColdStartWithNothingCachedLogsIn(t *testing.T) {
	store := &memStore{}
	loginCalls := 0
	withStubLogin(t, func(ctx context.Context, out io.Writer) (*oauth2.Token, error) {
		loginCalls++
		return &oauth2.Token{AccessToken: "fresh", RefreshToken: "fresh-refresh"}, nil
	})

	if _, err := TokenSource(context.Background(), store, io.Discard); err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if loginCalls != 1 {
		t.Errorf("login called %d times, want 1 for an empty store", loginCalls)
	}

	saved, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("the token the login returned was not persisted: %v", err)
	}
	if saved.AccessToken != "fresh" {
		t.Errorf("saved AccessToken = %q, want fresh", saved.AccessToken)
	}
}

func TestTokenSource_CorruptCacheIsAHardErrorNotALoginPrompt(t *testing.T) {
	store := &memStore{failLoad: errors.New("cached token is not valid JSON")}

	loginCalls := 0
	withStubLogin(t, func(ctx context.Context, out io.Writer) (*oauth2.Token, error) {
		loginCalls++
		t.Error("interactive login must not be attempted for a corrupt cache")
		return nil, errors.New("should not be reached")
	})

	if _, err := TokenSource(context.Background(), store, io.Discard); err == nil {
		t.Fatal("expected TokenSource to fail hard on a corrupt cache")
	}
	if loginCalls != 0 {
		t.Errorf("login called %d times, want 0", loginCalls)
	}
}

// A store that cannot be reached is the state a pod is in when it has been
// released ahead of the migration that gives it its table, and it is the one
// most worth getting right: a device code printed into a pod log that nobody
// is watching blocks the agent for as long as the code lasts, on a database
// problem that would have cleared itself.
func TestTokenSource_UnavailableStoreIsAHardErrorNotALoginPrompt(t *testing.T) {
	store := &memStore{failLoad: ErrStoreUnavailable}

	withStubLogin(t, func(ctx context.Context, out io.Writer) (*oauth2.Token, error) {
		t.Error("interactive login must not be attempted when the store cannot be read")
		return nil, errors.New("should not be reached")
	})

	_, err := TokenSource(context.Background(), store, io.Discard)
	if err == nil {
		t.Fatal("expected TokenSource to fail hard on an unavailable store")
	}
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("error = %v, want it to carry ErrStoreUnavailable", err)
	}
}

func TestTokenSource_RejectsANilStore(t *testing.T) {
	if _, err := TokenSource(context.Background(), nil, io.Discard); err == nil {
		t.Fatal("expected an error for a nil store")
	}
}

func TestCachingTokenSource_PersistsEachRefresh(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "first", RefreshToken: "r1"})

	cts := &cachingTokenSource{
		store:   store,
		allowed: func() bool { return true },
		inner: &stubTokenSource{tokens: []*oauth2.Token{
			{AccessToken: "first", RefreshToken: "r1"},
			{AccessToken: "second", RefreshToken: "r2"},
		}},
	}

	if _, err := cts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	got, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load after first call: %v", err)
	}
	if got.AccessToken != "first" {
		t.Errorf("AccessToken = %q, want first", got.AccessToken)
	}

	if _, err := cts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	got, err = store.Load(ctx)
	if err != nil {
		t.Fatalf("Load after second call: %v", err)
	}
	if got.AccessToken != "second" {
		t.Errorf("AccessToken = %q, want second after refresh", got.AccessToken)
	}
}

func TestCachingTokenSource_UnchangedTokenIsNotRewritten(t *testing.T) {
	store := &memStore{}
	cts := &cachingTokenSource{
		store:   store,
		allowed: func() bool { return true },
		inner:   &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "a", RefreshToken: "r1"}}},
	}

	for i := 0; i < 5; i++ {
		if _, err := cts.Token(); err != nil {
			t.Fatalf("Token: %v", err)
		}
	}
	if _, saves := store.counts(); saves != 1 {
		t.Errorf("saves = %d, want 1: a token that has not rotated is not news", saves)
	}
}

// The invariant the warm standby rests on: two processes hold the same
// account's token, and only the live one may rotate what is stored.
func TestCachingTokenSource_StandbyRefreshesWithoutWriting(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "live-token", RefreshToken: "r1"})

	standby := &cachingTokenSource{
		store:   store,
		allowed: func() bool { return false },
		inner:   &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "standby-refreshed", RefreshToken: "r2"}}},
	}

	tok, err := standby.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "standby-refreshed" {
		t.Errorf("a standby got %q, want its own refreshed token", tok.AccessToken)
	}
	if _, saves := store.counts(); saves != 0 {
		t.Errorf("standby wrote %d times, want 0", saves)
	}
	stored, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.RefreshToken != "r1" {
		t.Errorf("stored RefreshToken = %q, want the live agent's r1 left untouched", stored.RefreshToken)
	}
}

// A standby that refreshed while it waited holds a token nothing has
// recorded. Going live has to write it, or a restart would fall back to a
// refresh token Microsoft has already rotated away from.
func TestCachingTokenSource_StandbyWritesWhatItRefreshedOnceItGoesLive(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "old", RefreshToken: "r1"})

	var live atomic.Bool
	cts := &cachingTokenSource{
		store:   store,
		allowed: live.Load,
		inner:   &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "refreshed", RefreshToken: "r2"}}},
	}

	if _, err := cts.Token(); err != nil {
		t.Fatalf("Token as standby: %v", err)
	}
	if _, saves := store.counts(); saves != 0 {
		t.Fatalf("standby wrote %d times, want 0", saves)
	}

	live.Store(true)
	if _, err := cts.Token(); err != nil {
		t.Fatalf("Token as leader: %v", err)
	}
	stored, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.RefreshToken != "r2" {
		t.Errorf("stored RefreshToken = %q, want r2 flushed on going live", stored.RefreshToken)
	}
}

// Both roles against one store at once, which is what a rolling release
// actually looks like. The assertion is not just that nothing races: it is
// that the stored token is only ever one the live agent put there.
func TestCachingTokenSource_LiveAndStandbyShareOneStoreConcurrently(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "seed", RefreshToken: "live-0"})

	liveTokens := make([]*oauth2.Token, 50)
	for i := range liveTokens {
		liveTokens[i] = &oauth2.Token{AccessToken: "live", RefreshToken: "live-" + string(rune('a'+i%26))}
	}
	standbyTokens := make([]*oauth2.Token, 50)
	for i := range standbyTokens {
		standbyTokens[i] = &oauth2.Token{AccessToken: "standby", RefreshToken: "standby-only"}
	}

	liveAgent := &cachingTokenSource{
		store:   store,
		allowed: func() bool { return true },
		inner:   &stubTokenSource{tokens: liveTokens},
	}
	standby := &cachingTokenSource{
		store:   store,
		allowed: func() bool { return false },
		inner:   &stubTokenSource{tokens: standbyTokens},
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 2*len(liveTokens))
	for i := 0; i < len(liveTokens); i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := liveAgent.Token(); err != nil {
				errCh <- err
			}
		}()
		go func() {
			defer wg.Done()
			tok, err := standby.Token()
			if err != nil {
				errCh <- err
				return
			}
			// The standby's whole reason to exist: it holds a usable token
			// of its own the entire time, without a volume and without
			// touching the live agent's.
			if tok.AccessToken != "standby" {
				errCh <- errors.New("standby did not get its own token")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Token() call: %v", err)
	}

	stored, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.RefreshToken == "standby-only" {
		t.Error("the standby's refresh token reached the store")
	}
	if stored.AccessToken != "live" {
		t.Errorf("stored AccessToken = %q, want the live agent's", stored.AccessToken)
	}
}

// A store that has gone away costs the next restart a re-authentication. It
// must not cost this process its connection, which is the one thing keeping
// the agent in the game.
func TestCachingTokenSource_SaveFailureDoesNotFailTheCall(t *testing.T) {
	store := &memStore{failSave: ErrStoreUnavailable}
	cts := &cachingTokenSource{
		store:   store,
		allowed: func() bool { return true },
		inner:   &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "a", RefreshToken: "r1"}}},
	}

	tok, err := cts.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok.AccessToken != "a" {
		t.Errorf("AccessToken = %q, want a", tok.AccessToken)
	}

	// And the failure is not remembered as a success: the next call tries
	// again rather than assuming the store holds what it does not.
	if _, err := cts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if _, saves := store.counts(); saves != 2 {
		t.Errorf("saves = %d, want 2 attempts", saves)
	}
}

func TestCachingTokenSource_TokenIsSafeForConcurrentUse(t *testing.T) {
	store := &memStore{}
	tokens := make([]*oauth2.Token, 50)
	for i := range tokens {
		tokens[i] = &oauth2.Token{AccessToken: "tok", RefreshToken: "refresh"}
	}
	cts := &cachingTokenSource{
		store:   store,
		allowed: func() bool { return true },
		inner:   &stubTokenSource{tokens: tokens},
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(tokens))
	for range tokens {
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
