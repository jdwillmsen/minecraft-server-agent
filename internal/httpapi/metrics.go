package httpapi

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics worth alerting on for an unattended 24/7 connection: whether the
// Bedrock session is currently up, and how often it's had to reconnect.
// Declared at package level (not inside New) and registered exactly once
// per process, so multiple Server instances - as tests create - never
// double-register and panic.
var (
	connectedGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_agent_connected",
		Help: "1 if the agent currently has an established Bedrock session, 0 otherwise.",
	})
	reconnectsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mc_agent_reconnects_total",
		Help: "Number of times the agent has had to reconnect to the Bedrock server.",
	})
	// One account allows one login, so the sum of this series across pods is
	// the invariant worth alerting on in both directions: two live agents are
	// two processes kicking each other out of the game, and none is a server
	// with nobody answering it.
	leaderGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_agent_leader",
		Help: "1 if this process holds the agent lock and is the live agent, 0 if it is a standby.",
	})
)

// SetConnected records whether a Bedrock session is currently established.
func SetConnected(connected bool) {
	if connected {
		connectedGauge.Set(1)
	} else {
		connectedGauge.Set(0)
	}
}

// setLeader records whether this process is the live agent. Unexported
// because it must move with the role the rest of the process acts on -- see
// Server.SetRole.
func setLeader(leader bool) {
	if leader {
		leaderGauge.Set(1)
		return
	}
	leaderGauge.Set(0)
}

// IncReconnect records one reconnect attempt.
func IncReconnect() {
	reconnectsTotal.Inc()
}

func metricsHandler() http.Handler {
	return promhttp.Handler()
}
