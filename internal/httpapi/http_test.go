package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestServer_Healthz(t *testing.T) {
	srv, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.ln.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}

func TestServer_Readyz_NotReadyUntilSetReady(t *testing.T) {
	srv, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.ln.Close()

	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		return rec
	}

	if rec := get(); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("before SetReady(true): status = %d, want 503", rec.Code)
	}

	srv.SetReady(true)
	if rec := get(); rec.Code != http.StatusOK {
		t.Errorf("after SetReady(true): status = %d, want 200", rec.Code)
	}

	srv.SetReady(false)
	if rec := get(); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("after SetReady(false): status = %d, want 503", rec.Code)
	}
}

func TestServer_Metrics(t *testing.T) {
	srv, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.ln.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestNew_FailsLoudlyOnAnAlreadyBoundAddress(t *testing.T) {
	first, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer first.ln.Close()

	if _, err := New(first.Addr().String()); err == nil {
		t.Fatal("expected New to fail binding an address already in use")
	}
}

func TestServer_ShutdownIsClean(t *testing.T) {
	srv, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	// The listener is bound synchronously by New above, so there is nothing
	// to wait on before Shutdown - no sleep-based race with the Serve
	// goroutine's startup.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("ListenAndServe returned %v, want nil after graceful shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after Shutdown")
	}
}

// A standby has done everything a pod can do without the game login: it has
// refreshed its Xbox token, opened its database, loaded its knowledge and
// bound this very server. Reporting it unready would make Kubernetes treat a
// correctly waiting pod as a broken one -- and, with a rolling update that
// will not remove the old pod until the new one is ready, would deadlock the
// rollout the standby exists to make fast.
func TestServer_Readyz_AStandbyIsReadyWithoutASession(t *testing.T) {
	srv, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = srv.ln.Close() }()

	srv.SetRole(RoleStandby)
	rec := readyz(srv)
	if rec.Code != http.StatusOK {
		t.Errorf("a standby's status = %d, want 200", rec.Code)
	}
	// Distinguishable from a live agent, because the two states call for
	// different reactions from whoever is reading: one is serving players,
	// the other is waiting for its turn.
	if rec.Body.String() != "standby" {
		t.Errorf("a standby's body = %q, want standby", rec.Body.String())
	}
}

func TestServer_Readyz_TheLiveAgentStillAnswersForItsSession(t *testing.T) {
	srv, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = srv.ln.Close() }()

	srv.SetRole(RoleLive)
	if rec := readyz(srv); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("live with no session: status = %d, want 503", rec.Code)
	}
	srv.SetReady(true)
	if rec := readyz(srv); rec.Code != http.StatusOK || rec.Body.String() != "ready" {
		t.Errorf("live with a session: (%d, %q), want (200, ready)", rec.Code, rec.Body.String())
	}
}

// Handing the lock on is not a failure, and the moment after it the process
// is a standby again: it has no session, and it must not report the 503 that
// would say it is broken.
func TestServer_Readyz_AnAgentThatGaveUpTheLockIsAStandbyAgain(t *testing.T) {
	srv, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = srv.ln.Close() }()

	srv.SetRole(RoleLive)
	srv.SetReady(true)
	srv.SetReady(false)
	srv.SetRole(RoleStandby)

	if rec := readyz(srv); rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a demoted agent waiting again", rec.Code)
	}
}

// The gauge exists so "exactly one agent is live" is answerable from
// outside: two of these at 1 is the two-logins-one-account failure, and zero
// of them is nobody playing.
func TestSetRoleMovesTheLeaderGauge(t *testing.T) {
	srv, err := New("127.0.0.1:0")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = srv.ln.Close() }()

	srv.SetRole(RoleLive)
	if got := testutil.ToFloat64(leaderGauge); got != 1 {
		t.Errorf("mc_agent_leader = %v while live, want 1", got)
	}
	srv.SetRole(RoleStandby)
	if got := testutil.ToFloat64(leaderGauge); got != 0 {
		t.Errorf("mc_agent_leader = %v while standby, want 0", got)
	}
}

func readyz(srv *Server) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	return rec
}
