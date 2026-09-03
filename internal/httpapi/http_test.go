package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
