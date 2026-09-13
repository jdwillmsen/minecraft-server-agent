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
		Help: "1 if this process is the live agent, 0 if it is a standby.",
	})
	// A second series rather than a third value of the one above, because the
	// two questions are independent and both have to stay answerable: this
	// process is the live agent (mc_agent_leader is 1, like any other live
	// agent) and it is leading without the lock that makes that exclusive. A
	// gauge rather than a counter so it stops being true when it stops being
	// true -- the agent adopts the lock if it ever frees, and an alert that
	// could not clear would outlive the condition.
	leaderUnlockedGauge = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_agent_leader_unlocked",
		Help: "1 if this process is the live agent without holding the agent lock, after the bounded wait for it expired.",
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

// SetLeaderUnlocked records whether this process is leading without the lock.
//
// Exported, unlike setLeader, because nothing about readiness changes when it
// moves: an agent leading unlocked is live and serving players, and the only
// thing that differs is the guarantee behind it -- which is a thing to alert
// on, not a thing to take a pod out of service for.
func SetLeaderUnlocked(unlocked bool) {
	if unlocked {
		leaderUnlockedGauge.Set(1)
		return
	}
	leaderUnlockedGauge.Set(0)
}

// IncReconnect records one reconnect attempt.
func IncReconnect() {
	reconnectsTotal.Inc()
}

func metricsHandler() http.Handler {
	return promhttp.Handler()
}
