package mcauth

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type stubTokenSource struct {
	mu     sync.Mutex
	tokens []*oauth2.Token
	calls  int
	// err stands for the refresh Microsoft rejects -- invalid_grant against
	// a refresh token something else has already rotated past.
	err error
}

func (s *stubTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	// The last token repeats once the script runs out, which is what a real
	// source does between refreshes: the same token until it expires.
	return s.tokens[min(s.calls-1, len(s.tokens)-1)], nil
}

func (s *stubTokenSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
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

	ts, err := TokenSource(context.Background(), store, io.Discard)
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if _, err := ts.Token(); err != nil {
		t.Fatalf("Token: %v", err)
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

// A standby has no lock, so it has no claim on the one login the account
// allows. Prompting anyway costs that pod ~15 minutes blocked on a code
// nobody is watching for, and an operator who does answer it creates a second
// grant the process holding the game knows nothing about.
func TestTokenSource_AStandbyDoesNotPromptForAColdStart(t *testing.T) {
	store := &memStore{}
	withStubLogin(t, func(ctx context.Context, out io.Writer) (*oauth2.Token, error) {
		t.Error("a standby printed a device code")
		return nil, errors.New("should not be reached")
	})

	ts, err := TokenSource(context.Background(), store, io.Discard, WithLiveGate(func() bool { return false }))
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if _, err := ts.Token(); err == nil {
		t.Fatal("a standby reported a usable token from an empty store")
	}
}

// And the login is not lost, only deferred: the moment this process is the
// one entitled to it, it runs.
func TestTokenSource_TheDeferredLoginRunsOnceTheProcessGoesLive(t *testing.T) {
	store := &memStore{}
	var live atomic.Bool
	loginCalls := 0
	withStubLogin(t, func(ctx context.Context, out io.Writer) (*oauth2.Token, error) {
		loginCalls++
		return &oauth2.Token{AccessToken: "fresh", RefreshToken: "fresh-refresh"}, nil
	})

	ts, err := TokenSource(context.Background(), store, io.Discard, WithLiveGate(live.Load))
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if _, err := ts.Token(); err == nil {
		t.Fatal("a standby reported a usable token from an empty store")
	}

	live.Store(true)
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token once live: %v", err)
	}
	if tok.AccessToken != "fresh" {
		t.Errorf("AccessToken = %q, want the login's", tok.AccessToken)
	}
	if loginCalls != 1 {
		t.Errorf("login called %d times, want 1", loginCalls)
	}
	saved, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("the token the login returned was not persisted: %v", err)
	}
	if saved.RefreshToken != "fresh-refresh" {
		t.Errorf("saved RefreshToken = %q, want the login's", saved.RefreshToken)
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
		store: store,
		out:   io.Discard,
		live:  func() bool { return true },
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
		store: store,
		out:   io.Discard,
		live:  func() bool { return true },
		inner: &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "a", RefreshToken: "r1"}}},
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

// The invariant the warm standby rests on, and the reason the gate cannot sit
// on the write alone: Microsoft retires the old refresh token as it issues
// the new one, so a standby that refreshed would revoke the credential the
// live agent is playing on -- the row would still say r1, and r1 would be
// dead. Suppressing the write does not undo that.
func TestCachingTokenSource_StandbyNeverRotatesTheSharedToken(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "live-token", RefreshToken: "r1"})

	refresher := &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "standby-refreshed", RefreshToken: "r2"}}}
	standby := &cachingTokenSource{
		store: store,
		out:   io.Discard,
		live:  func() bool { return false },
		inner: refresher,
		held:  &oauth2.Token{AccessToken: "live-token", RefreshToken: "r1"},
	}

	tok, err := standby.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if refresher.callCount() != 0 {
		t.Errorf("standby refreshed %d times, want 0", refresher.callCount())
	}
	if tok.RefreshToken != "r1" {
		t.Errorf("standby got %q, want the token the live agent is using", tok.RefreshToken)
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

// How a standby stays warm without rotating anything: when what it holds has
// expired, it re-reads the store, which the live agent keeps current. The
// round trip the handover would have paid is paid here, against the database
// instead of against Microsoft.
func TestCachingTokenSource_StandbyRewarmsFromWhatTheLiveAgentStored(t *testing.T) {
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "live-refreshed", RefreshToken: "r2", Expiry: time.Now().Add(time.Hour)})

	refresher := &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "must-not-happen", RefreshToken: "r3"}}}
	standby := &cachingTokenSource{
		store: store,
		out:   io.Discard,
		live:  func() bool { return false },
		inner: refresher,
		held:  &oauth2.Token{AccessToken: "stale", RefreshToken: "r1", Expiry: time.Now().Add(-time.Minute)},
	}

	tok, err := standby.Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if refresher.callCount() != 0 {
		t.Errorf("standby refreshed %d times, want 0", refresher.callCount())
	}
	if tok.RefreshToken != "r2" {
		t.Errorf("standby got %q, want the live agent's stored r2", tok.RefreshToken)
	}
	if _, saves := store.counts(); saves != 0 {
		t.Errorf("standby wrote %d times, want 0", saves)
	}
}

// With nothing newer to read, the standby stays cold and says so rather than
// refreshing its way out of it.
func TestCachingTokenSource_AnUnwarmableStandbyReportsItRatherThanRefreshing(t *testing.T) {
	refresher := &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "must-not-happen", RefreshToken: "r2"}}}
	standby := &cachingTokenSource{
		store: &memStore{failLoad: ErrStoreUnavailable},
		out:   io.Discard,
		live:  func() bool { return false },
		inner: refresher,
		held:  &oauth2.Token{AccessToken: "stale", RefreshToken: "r1", Expiry: time.Now().Add(-time.Minute)},
	}

	if _, err := standby.Token(); err == nil {
		t.Fatal("an unwarmable standby reported success")
	}
	if refresher.callCount() != 0 {
		t.Errorf("standby refreshed %d times, want 0", refresher.callCount())
	}
}

// A process does not necessarily hold the account's current refresh token: a
// load that reached past an unreachable database answers from the file the
// migration left behind, and that copy died the first time the live agent
// rotated. Nothing else re-reads the store, so the rejection has to, or the
// connect loop retries a dead credential for as long as the process lives
// while the recovered database holds one that works.
func TestCachingTokenSource_ARejectedRefreshReReadsTheStore(t *testing.T) {
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "current", RefreshToken: "r5", Expiry: time.Now().Add(time.Hour)})

	refresher := &stubTokenSource{err: errors.New("invalid_grant")}
	cts := &cachingTokenSource{
		store: store,
		out:   io.Discard,
		live:  func() bool { return true },
		inner: refresher,
		held:  &oauth2.Token{AccessToken: "stale", RefreshToken: "r0", Expiry: time.Now().Add(-time.Minute)},
	}

	tok, err := cts.Token()
	if err != nil {
		t.Fatalf("a rejected refresh was fatal even though the store held a usable token: %v", err)
	}
	if tok.RefreshToken != "r5" {
		t.Errorf("got %q, want the r5 the live agent stored", tok.RefreshToken)
	}
	if cts.held.RefreshToken != "r5" {
		t.Errorf("held RefreshToken = %q, want r5: the dead r0 must not be what the next refresh starts from", cts.held.RefreshToken)
	}
}

// The re-read is a second look, not a second chance: a store that agrees with
// what was just rejected has nothing to add, and swallowing the rejection
// would hide a genuinely revoked account behind a nil error.
func TestCachingTokenSource_ARejectedRefreshStandsWhenTheStoreAgrees(t *testing.T) {
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "stale", RefreshToken: "r0", Expiry: time.Now().Add(-time.Minute)})

	rejection := errors.New("invalid_grant")
	refresher := &stubTokenSource{err: rejection}
	cts := &cachingTokenSource{
		store: store,
		out:   io.Discard,
		live:  func() bool { return true },
		inner: refresher,
		held:  &oauth2.Token{AccessToken: "stale", RefreshToken: "r0", Expiry: time.Now().Add(-time.Minute)},
	}

	if _, err := cts.Token(); !errors.Is(err, rejection) {
		t.Fatalf("err = %v, want the rejection itself", err)
	}
	if refresher.callCount() != 1 {
		t.Errorf("refreshed %d times, want 1: a store that agrees is not worth a retry", refresher.callCount())
	}
}

// Adopting rebuilds the refresher, so a reload is only ever an improvement
// when it can be used. The standby reload runs with what is held already
// expired, which means the one thing a useless answer can still do is take
// over which credential a later rotation starts from -- and a fallback
// reaching past an unreachable database answers with the file's older copy.
func TestCachingTokenSource_AnUnusableStandbyReloadKeepsTheHeldToken(t *testing.T) {
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "pre-migration", RefreshToken: "r0", Expiry: time.Now().Add(-time.Hour)})

	refresher := &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "must-not-happen", RefreshToken: "r6"}}}
	standby := &cachingTokenSource{
		store: store,
		out:   io.Discard,
		live:  func() bool { return false },
		inner: refresher,
		held:  &oauth2.Token{AccessToken: "expired", RefreshToken: "r5", Expiry: time.Now().Add(-time.Minute)},
	}

	if _, err := standby.Token(); !errors.Is(err, ErrStandbyUnwarmed) {
		t.Fatalf("err = %v, want ErrStandbyUnwarmed", err)
	}
	if standby.held.RefreshToken != "r5" {
		t.Errorf("held RefreshToken = %q, want r5 kept: the reload was no better than what it replaced", standby.held.RefreshToken)
	}
	if standby.inner != oauth2.TokenSource(refresher) {
		t.Error("the refresher was rebuilt around a token that could not be used")
	}
}

// A standby that loaded nothing at all is the exception: an expired token is
// worse than a usable one and better than none, because its refresh token is
// what this process will rotate from the moment it goes live.
func TestCachingTokenSource_AStandbyHoldingNothingAdoptsAnExpiredReload(t *testing.T) {
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "expired", RefreshToken: "r2", Expiry: time.Now().Add(-time.Minute)})

	standby := &cachingTokenSource{
		store: store,
		out:   io.Discard,
		live:  func() bool { return false },
	}

	if _, err := standby.Token(); !errors.Is(err, ErrStandbyUnwarmed) {
		t.Fatalf("err = %v, want ErrStandbyUnwarmed", err)
	}
	if standby.held == nil || standby.held.RefreshToken != "r2" {
		t.Errorf("held = %+v, want the stored r2: a process with nothing has nothing to lose", standby.held)
	}
}

// Going live is what lifts the restriction, and the first refresh after it is
// written -- nothing this process held was ever recorded from here before.
func TestCachingTokenSource_RefreshesAndWritesOnceItGoesLive(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "old", RefreshToken: "r1"})

	var live atomic.Bool
	cts := &cachingTokenSource{
		store: store,
		out:   io.Discard,
		live:  live.Load,
		inner: &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "refreshed", RefreshToken: "r2"}}},
		held:  &oauth2.Token{AccessToken: "old", RefreshToken: "r1"},
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
		t.Errorf("stored RefreshToken = %q, want r2 written once live", stored.RefreshToken)
	}
}

// Both roles against one store at once, which is what a rolling release
// actually looks like. The assertion is not just that nothing races: it is
// that the stored token is only ever one the live agent put there, and that
// the standby got every token it answered with out of the live agent's row.
//
// The standby's held token is expired on purpose. A zero Expiry reads as
// "never expires" to oauth2, which would have the standby answer from memory
// every time and touch the shared store not once -- the property this is
// here for, serialised away by construction.
func TestCachingTokenSource_LiveAndStandbyShareOneStoreConcurrently(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	// The live agent's own token, since that is all this store ever holds:
	// a standby that reads before the first rotation below still reads one.
	store.seed(t, &oauth2.Token{AccessToken: "live", RefreshToken: "live-seed", Expiry: time.Now().Add(time.Hour)})

	liveTokens := make([]*oauth2.Token, 50)
	for i := range liveTokens {
		liveTokens[i] = &oauth2.Token{
			AccessToken:  "live",
			RefreshToken: "live-" + string(rune('a'+i%26)),
			Expiry:       time.Now().Add(time.Hour),
		}
	}
	standbyTokens := make([]*oauth2.Token, 50)
	for i := range standbyTokens {
		standbyTokens[i] = &oauth2.Token{AccessToken: "must-not-happen", RefreshToken: "standby-only", Expiry: time.Now().Add(time.Hour)}
	}

	liveAgent := &cachingTokenSource{
		store: store,
		out:   io.Discard,
		live:  func() bool { return true },
		inner: &stubTokenSource{tokens: liveTokens},
	}
	standbyRefresher := &stubTokenSource{tokens: standbyTokens}
	standby := &cachingTokenSource{
		store: store,
		out:   io.Discard,
		live:  func() bool { return false },
		inner: standbyRefresher,
		held:  &oauth2.Token{AccessToken: "standby", RefreshToken: "standby-held", Expiry: time.Now().Add(-time.Minute)},
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
				// The one failure a standby is allowed: nothing fresh in the
				// store and no licence to refresh its way out of that.
				if !errors.Is(err, ErrStandbyUnwarmed) {
					errCh <- err
				}
				return
			}
			// Whatever it answers with came out of the live agent's row --
			// it has no other source, having rotated nothing itself.
			if tok.AccessToken != "live" {
				errCh <- errors.New("standby answered with a token the live agent never stored")
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent Token() call: %v", err)
	}

	if standbyRefresher.callCount() != 0 {
		t.Errorf("standby refreshed %d times, want 0", standbyRefresher.callCount())
	}
	loads, saves := store.counts()
	if loads == 0 {
		t.Error("the standby never read the shared store, so neither role exercised it")
	}
	if saves != len(liveTokens) {
		t.Errorf("saves = %d, want one per live refresh (%d): only the live agent writes", saves, len(liveTokens))
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
		store: store,
		out:   io.Discard,
		live:  func() bool { return true },
		inner: &stubTokenSource{tokens: []*oauth2.Token{{AccessToken: "a", RefreshToken: "r1"}}},
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
		store: store,
		out:   io.Discard,
		live:  func() bool { return true },
		inner: &stubTokenSource{tokens: tokens},
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

// errTokenSource is a refresher that only fails, which is what Microsoft's
// does once the token it holds has been retired by a newer one.
type errTokenSource struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (e *errTokenSource) Token() (*oauth2.Token, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	return nil, e.err
}

func (e *errTokenSource) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// A promoted standby holds whatever the store said when it last read it, and
// the live agent has rotated since. Microsoft retired that token as it issued
// the replacement, so the first thing the new live agent must do with the
// login is read the row -- not write its own copy over it. Flushing leaves
// the account's only stored credential dead: this process fails at its next
// refresh, and every restart after it loads the dead one too.
func TestCachingTokenSource_PromotionReadsTheStoreBeforeWritingToIt(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "boot", RefreshToken: "r1", Expiry: time.Now().Add(time.Hour)})

	var live atomic.Bool
	cts := &cachingTokenSource{store: store, out: io.Discard, live: live.Load}
	if _, err := cts.Token(); err != nil {
		t.Fatalf("Token while standing by: %v", err)
	}

	// The live agent rotates r1 into r2 and stores it, which is what retires
	// r1 at Microsoft.
	if err := store.Save(ctx, &oauth2.Token{AccessToken: "live", RefreshToken: "r2", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("the live agent's Save: %v", err)
	}

	live.Store(true)
	tok, err := cts.Token()
	if err != nil {
		t.Fatalf("Token once promoted: %v", err)
	}
	if tok.RefreshToken != "r2" {
		t.Errorf("promoted standby answered with %q, want the stored r2", tok.RefreshToken)
	}
	stored, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if stored.RefreshToken != "r2" {
		t.Errorf("the row now holds %q: a promoted standby overwrote a newer token with a retired one", stored.RefreshToken)
	}
}

// The same handover with the standby's copy already expired, which is the
// likelier half: refreshing from the retired token answers invalid_grant, and
// the connect loop then parks on AUTH_RETRY_DELAY_MS -- the handover cost this
// design exists to remove. The store holds a usable token the whole time.
func TestCachingTokenSource_PromotionWithAnExpiredCopyTakesTheStoredTokenNotARefresh(t *testing.T) {
	ctx := context.Background()
	withStubLogin(t, func(ctx context.Context, out io.Writer) (*oauth2.Token, error) {
		t.Error("a promoted standby printed a device code with a usable token in the store")
		return nil, errors.New("should not be reached")
	})

	store := &memStore{}
	store.seed(t, &oauth2.Token{AccessToken: "boot", RefreshToken: "r1", Expiry: time.Now().Add(-time.Minute)})

	var live atomic.Bool
	refresher := &errTokenSource{err: errors.New("oauth2: invalid_grant")}
	cts := &cachingTokenSource{
		store: store,
		out:   io.Discard,
		live:  live.Load,
		inner: refresher,
		held:  &oauth2.Token{AccessToken: "boot", RefreshToken: "r1", Expiry: time.Now().Add(-time.Minute)},
	}
	if _, err := cts.Token(); err == nil {
		t.Fatal("a standby with nothing unexpired to read reported a usable token")
	}

	if err := store.Save(ctx, &oauth2.Token{AccessToken: "live", RefreshToken: "r2", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("the live agent's Save: %v", err)
	}

	live.Store(true)
	tok, err := cts.Token()
	if err != nil {
		t.Fatalf("Token once promoted: %v", err)
	}
	if tok.RefreshToken != "r2" {
		t.Errorf("promoted standby answered with %q, want the stored r2", tok.RefreshToken)
	}
	if refresher.callCount() != 0 {
		t.Errorf("refreshed %d times from a token the live agent retired, want 0", refresher.callCount())
	}
}
