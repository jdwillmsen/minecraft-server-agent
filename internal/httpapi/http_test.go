package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestServer_Healthz(t *testing.T) {
	srv := New(":0")
	go func() { _ = srv.ListenAndServe() }()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	// New(":0") binds an ephemeral port we can't easily discover through
	// http.Server, so exercise the handler directly instead of over the
	// network.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)

	handlerServer := New(":0")
	handlerServer.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != "ok" {
		t.Errorf("body = %q, want ok", rec.Body.String())
	}
}

func TestServer_Metrics(t *testing.T) {
	srv := New(":0")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)

	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

func TestServer_ShutdownIsClean(t *testing.T) {
	srv := New("127.0.0.1:0")
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	// Give the listener a moment to start before shutting it down.
	time.Sleep(50 * time.Millisecond)

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
