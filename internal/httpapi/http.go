// Package httpapi exposes the agent's own operational endpoints: /healthz
// for liveness and /metrics for Prometheus scraping. Feature-specific
// metrics get registered here starting with Stage 6; the registry is wired
// up now so there's a stable place to add them later.
package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server is the agent's HTTP server.
type Server struct {
	httpServer *http.Server
}

// New builds a Server listening on addr, with /healthz and /metrics wired.
func New(addr string) *Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.Handle("/metrics", promhttp.Handler())

	return &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
	}
}

// ListenAndServe blocks serving HTTP until the server is shut down or
// fails to start.
func (s *Server) ListenAndServe() error {
	err := s.httpServer.ListenAndServe()
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
