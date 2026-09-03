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
)

// SetConnected records whether a Bedrock session is currently established.
func SetConnected(connected bool) {
	if connected {
		connectedGauge.Set(1)
	} else {
		connectedGauge.Set(0)
	}
}

// IncReconnect records one reconnect attempt.
func IncReconnect() {
	reconnectsTotal.Inc()
}

func metricsHandler() http.Handler {
	return promhttp.Handler()
}
