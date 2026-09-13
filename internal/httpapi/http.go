// Package httpapi exposes the agent's own operational endpoints: /healthz
// for liveness, /readyz for whether this process is serving the game, and
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

// Role is what this process is doing with the one game login the account
// allows: playing, or waiting for its turn to.
type Role int32

const (
	// RoleStandby is a process that is not playing: one still starting up,
	// one waiting for the lock, or one that has just lost it. It is the zero
	// value because a process becomes live by taking the lock, and until it
	// has, claiming otherwise would let it act on a game it is not in --
	// the announcement API is served for the whole process, including the
	// startup before leadership is settled and the moment after it is gone.
	// A deployment with no database has no lock to wait for and is set live
	// explicitly instead -- see awaitLeadership.
	RoleStandby Role = iota
	// RoleLive is the process that holds the agent lock and the login.
	RoleLive
)

// Server is the agent's HTTP server.
type Server struct {
	httpServer *http.Server
	ln         net.Listener
	ready      atomic.Bool
	role       atomic.Int32
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
	// Readiness answers "is this pod doing its job", which for this workload
	// has two right answers. A live agent is doing its job when it has a
	// Bedrock session; a standby is doing its job by waiting with everything
	// else already paid for, and calling that unready would both misreport a
	// healthy pod and stall the rolling update that only removes the old pod
	// once the new one is ready -- the update the standby exists to serve.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if !s.Live() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("standby"))
			return
		}
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

// SetReady controls what /readyz reports about a live agent's session: true
// once a Bedrock session is established, false the moment it is lost, so
// /readyz reflects real session state rather than always answering ok. It
// says nothing about a standby, which has no session by design -- see
// SetRole.
func (s *Server) SetReady(ready bool) {
	s.ready.Store(ready)
}

// SetRole records whether this process is the live agent or a standby waiting
// for the lock.
//
// It also moves mc_agent_leader, rather than leaving that to a second call
// from the same place: "which pod is live" is read from the metric by alerts
// and from /readyz by Kubernetes, and two call sites is one place for those
// two answers to disagree.
func (s *Server) SetRole(role Role) {
	s.role.Store(int32(role))
	setLeader(role == RoleLive)
}

// Live reports whether this process is the one currently holding the agent
// lock, as last recorded by SetRole.
//
// Read by anything in the process that may only act once, not once per
// replica: the announcement API is mounted for the process rather than for a
// turn as the live agent, so a request landing on a standby reaches code that
// would otherwise speak into the server the live agent is playing on. This
// answers from the same atomic /readyz and mc_agent_leader answer from, so
// there is no second place for "which pod is live" to be decided.
func (s *Server) Live() bool {
	return Role(s.role.Load()) == RoleLive
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
