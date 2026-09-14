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
// allows: still getting ready for it, waiting its turn at it, or playing.
type Role int32

const (
	// RoleStarting is a process that has bound this server but has not yet
	// paid the startup every role shares -- the Xbox token above all, which
	// can take seconds and, with an unusable auth cache, can block on a
	// device-code login indefinitely.
	//
	// The zero value, because it is what a process is from the moment the
	// listener answers, and because neither of the other two is safe to
	// assume there. Live would let a process act on a game it is not in;
	// standby would report a pod ready before it can take over, which is
	// the readiness a rolling update removes the live agent on.
	RoleStarting Role = iota
	// RoleStandby is a process that has finished every part of its startup
	// that does not need the login, and is waiting for the lock. A
	// deployment with no database has no lock to wait for and goes live
	// straight out of starting instead -- see awaitLeadership.
	RoleStandby
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
	//
	// A process still starting is neither, and is the one case that must not
	// answer ready: it cannot take over yet, so a rolling update that
	// believed it could would remove the live agent and leave the server
	// with no agent at all until the successor finishes authenticating.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		switch role := Role(s.role.Load()); {
		case role == RoleStandby:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("standby"))
		case role == RoleLive && s.ready.Load():
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("not ready"))
		}
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

// SetRole records which of the three roles this process is in: still
// starting, a standby waiting for the lock, or the live agent holding it.
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
// lock, as last recorded by SetRole. False while it is still starting, which
// is what keeps a process that has not won leadership from acting as though
// it had.
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
