package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// permsServer serves perms and counts how many times it was asked, so a
// test can assert the cache actually spared the bridge a call. The counter
// is atomic because the concurrent-cache test drives many requests at once.
func permsServer(t *testing.T, perms map[string]string) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		json.NewEncoder(w).Encode(perms)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestPermissionResolver_KnownOperatorAllowed(t *testing.T) {
	srv, _ := permsServer(t, map[string]string{"111": "operator"})
	r := NewPermissionResolver(NewBridgeClient(srv.URL, "tok", time.Second), time.Minute, logging.New("info"))

	if got := r.Resolve(context.Background(), "111"); got != plugin.PermissionOperator {
		t.Errorf("Resolve(111) = %v, want operator", got)
	}
}

func TestPermissionResolver_KnownVisitorDeniedOperatorLevel(t *testing.T) {
	srv, _ := permsServer(t, map[string]string{"222": "visitor"})
	r := NewPermissionResolver(NewBridgeClient(srv.URL, "tok", time.Second), time.Minute, logging.New("info"))

	got := r.Resolve(context.Background(), "222")
	if got != plugin.PermissionVisitor {
		t.Errorf("Resolve(222) = %v, want visitor", got)
	}
	if got >= plugin.PermissionOperator {
		t.Error("a visitor must never meet an operator-level command's requirement")
	}
}

func TestPermissionResolver_UnknownXUIDResolvesToVisitor(t *testing.T) {
	srv, _ := permsServer(t, map[string]string{"111": "operator"})
	r := NewPermissionResolver(NewBridgeClient(srv.URL, "tok", time.Second), time.Minute, logging.New("info"))

	if got := r.Resolve(context.Background(), "unknown-999"); got != plugin.PermissionVisitor {
		t.Errorf("Resolve(unknown) = %v, want visitor", got)
	}
}

func TestPermissionResolver_ServerOriginIsOperatorWithoutCallingBridge(t *testing.T) {
	srv, calls := permsServer(t, map[string]string{})
	r := NewPermissionResolver(NewBridgeClient(srv.URL, "tok", time.Second), time.Minute, logging.New("info"))

	if got := r.Resolve(context.Background(), chat.ServerOrigin); got != plugin.PermissionOperator {
		t.Errorf("Resolve(ServerOrigin) = %v, want operator", got)
	}
	if calls.Load() != 0 {
		t.Errorf("bridge was called %d times for ServerOrigin, want 0 — it's a sentinel, not a real XUID", calls.Load())
	}
}

func TestPermissionResolver_BridgeFailureFailsClosedToVisitor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	r := NewPermissionResolver(NewBridgeClient(srv.URL, "tok", time.Second), time.Minute, logging.New("info"))
	if got := r.Resolve(context.Background(), "111"); got != plugin.PermissionVisitor {
		t.Errorf("Resolve on bridge failure = %v, want visitor (fail closed)", got)
	}
}

func TestPermissionResolver_UnrecognisedLevelStringFailsClosedToVisitor(t *testing.T) {
	srv, _ := permsServer(t, map[string]string{"111": "wizard"})
	r := NewPermissionResolver(NewBridgeClient(srv.URL, "tok", time.Second), time.Minute, logging.New("info"))

	if got := r.Resolve(context.Background(), "111"); got != plugin.PermissionVisitor {
		t.Errorf("Resolve for an unrecognised level string = %v, want visitor", got)
	}
}

func TestPermissionResolver_CacheAvoidsSecondCallWithinTTL(t *testing.T) {
	srv, calls := permsServer(t, map[string]string{"111": "operator"})
	r := NewPermissionResolver(NewBridgeClient(srv.URL, "tok", time.Second), time.Minute, logging.New("info"))

	start := time.Now()
	r.now = func() time.Time { return start }

	r.Resolve(context.Background(), "111")
	r.Resolve(context.Background(), "111")

	if calls.Load() != 1 {
		t.Errorf("bridge called %d times for two lookups within the TTL, want 1", calls.Load())
	}
}

func TestPermissionResolver_CacheRefetchesAfterTTLExpires(t *testing.T) {
	srv, calls := permsServer(t, map[string]string{"111": "operator"})
	r := NewPermissionResolver(NewBridgeClient(srv.URL, "tok", time.Second), time.Minute, logging.New("info"))

	tick := time.Now()
	r.now = func() time.Time { return tick }

	r.Resolve(context.Background(), "111")
	tick = tick.Add(2 * time.Minute)
	r.Resolve(context.Background(), "111")

	if calls.Load() != 2 {
		t.Errorf("bridge called %d times across an expired TTL, want 2", calls.Load())
	}
}

// TestPermissionResolver_ConcurrentResolvesShareTheCache drives the
// resolver the way the running agent does — many actors' commands landing
// at once — and is the test the -race detector needs to prove the cache's
// locking. Beyond staying race-free it asserts the cache is genuinely
// shared: once warm, no goroutine reaches the bridge again, and every one
// of them still gets its own actor's correct level rather than another
// actor's.
func TestPermissionResolver_ConcurrentResolvesShareTheCache(t *testing.T) {
	perms := map[string]string{"111": "operator", "222": "member", "333": "visitor"}
	srv, calls := permsServer(t, perms)
	r := NewPermissionResolver(NewBridgeClient(srv.URL, "tok", time.Second), time.Minute, logging.New("info"))

	start := time.Now()
	r.now = func() time.Time { return start }

	want := map[string]plugin.Permission{
		"111":     plugin.PermissionOperator,
		"222":     plugin.PermissionMember,
		"333":     plugin.PermissionVisitor,
		"unknown": plugin.PermissionVisitor,
	}

	if got := r.Resolve(context.Background(), "111"); got != plugin.PermissionOperator {
		t.Fatalf("warm-up Resolve(111) = %v, want operator", got)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("bridge called %d times for the warm-up lookup, want 1", n)
	}

	const goroutines = 64
	var wg sync.WaitGroup
	errs := make(chan string, goroutines*len(want))
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for xuid, wantLevel := range want {
				if got := r.Resolve(context.Background(), xuid); got != wantLevel {
					errs <- fmt.Sprintf("Resolve(%s) = %v, want %v", xuid, got, wantLevel)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)

	for msg := range errs {
		t.Error(msg)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("bridge called %d times, want 1 — every concurrent lookup should have been served from the warm cache", n)
	}
}

// TestPermissionResolver_ConcurrentColdStartResolvesCorrectly is the same
// race check without the warm-up: nothing serialises the first fetch, so
// several goroutines may each call the bridge, but none may observe a
// half-written cache or a wrong level.
func TestPermissionResolver_ConcurrentColdStartResolvesCorrectly(t *testing.T) {
	srv, calls := permsServer(t, map[string]string{"111": "operator"})
	r := NewPermissionResolver(NewBridgeClient(srv.URL, "tok", time.Second), time.Minute, logging.New("info"))

	const goroutines = 32
	var wg sync.WaitGroup
	results := make(chan plugin.Permission, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- r.Resolve(context.Background(), "111")
		}()
	}
	wg.Wait()
	close(results)

	for got := range results {
		if got != plugin.PermissionOperator {
			t.Errorf("concurrent cold-start Resolve(111) = %v, want operator", got)
		}
	}
	if n := calls.Load(); n < 1 {
		t.Errorf("bridge called %d times, want at least 1", n)
	}
}
