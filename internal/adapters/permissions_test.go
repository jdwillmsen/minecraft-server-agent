package adapters

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

func permsServer(t *testing.T, perms map[string]string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
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
	if *calls != 0 {
		t.Errorf("bridge was called %d times for ServerOrigin, want 0 — it's a sentinel, not a real XUID", *calls)
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

	if *calls != 1 {
		t.Errorf("bridge called %d times for two lookups within the TTL, want 1", *calls)
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

	if *calls != 2 {
		t.Errorf("bridge called %d times across an expired TTL, want 2", *calls)
	}
}
