// Package httpapi exposes the agent's own operational endpoints: /healthz
// for liveness, /readyz for whether the Bedrock session is actually up, and
// /metrics for Prometheus scraping.
package httpapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Server is the agent's HTTP server.
type Server struct {
	httpServer *http.Server
	ln         net.Listener
	ready      atomic.Bool
	// mux is kept so a route that depends on configuration -- the
	// announcement API -- can be mounted after New, or not at all.
	mux *http.ServeMux
}

// New binds addr and builds a Server with /healthz, /readyz, and /metrics
// wired. The listener is bound synchronously here (rather than lazily
// inside ListenAndServe) so a bad address or an already-used port fails
// loudly at startup instead of being discovered later by a background
// goroutine whose error might go unnoticed.
func New(addr string) (*Server, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("httpapi: listen on %s: %w", addr, err)
	}

	mux := http.NewServeMux()
	s := &Server{ln: ln, mux: mux}

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready"))
	})
	mux.Handle("/metrics", metricsHandler())

	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	return s, nil
}

// Addr reports the address the server is actually bound to - useful in
// tests that bind an ephemeral port (":0").
func (s *Server) Addr() net.Addr {
	return s.ln.Addr()
}

// SetReady controls what /readyz reports. The connect loop calls this: true
// once a Bedrock session is established, false the moment it's lost, so
// /readyz reflects real session state rather than always answering ok.
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

// ListenAndServe blocks serving HTTP on the listener bound by New, until the
// server is shut down or the listener fails.
func (s *Server) ListenAndServe() error {
	err := s.httpServer.Serve(s.ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
