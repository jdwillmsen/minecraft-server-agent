package joinprobe

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// The gauges are deliberately plain rather than vectors with an address label.
// One probe process watches one server; labelling by address would invite a
// second process to publish the same series for a different target and leave
// an alert matching whichever scrape landed last.
var (
	joinable = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_joinprobe_joinable",
		Help: "1 when the last probe reached the server's network settings, which is the last step before a client sends credentials. 0 when it did not.",
	})
	stage = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_joinprobe_stage",
		Help: "How far the last probe got: 0 unreachable, 1 answered the ping, 2 refused the session, 3 completed the handshake.",
	})
	lastJoinable = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_joinprobe_last_joinable_timestamp_seconds",
		Help: "Unix time a probe last completed the handshake. 0 until one has.",
	})
	// A counter rather than a gauge, because the question an operator asks
	// after an outage is "how long was it refusing", and that is a rate over
	// attempts rather than a state at a moment.
	attempts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mc_joinprobe_attempts_total",
		Help: "Probe attempts by the stage they reached.",
	}, []string{"stage"})
	duration = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_joinprobe_duration_seconds",
		Help: "How long the last probe took, ping and handshake together.",
	})
	serverProtocol = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_joinprobe_server_protocol",
		Help: "Protocol number the server advertised in its pong. 0 when it did not answer.",
	})
	dialedProtocol = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_joinprobe_dialed_protocol",
		Help: "Protocol number the last handshake announced, which is the advertised one unless a probe is pinned to a version.",
	})
	// -1 rather than absent when the server did not refuse: a series that
	// disappears cannot be joined against, and 0 is PlayStatusLoginSuccess,
	// which would read as a successful login this probe never performs.
	playStatus = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_joinprobe_play_status",
		Help: "The play status a refusing server sent (1 client too old, 2 server too old, 7 server full). -1 when the server did not refuse.",
	})
	serverInfo = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mc_joinprobe_server_info",
		Help: "1, labelled with the version string the server advertises.",
	}, []string{"version"})
)

// Pre-initialised so a rate over the refusal counter has a zero to rise from.
// Without it the first refusal after a restart springs into existence at 1 and
// increase() reads it as no change at all.
func init() {
	for _, s := range []Stage{StageUnreachable, StagePong, StageRefused, StageHandshake} {
		attempts.WithLabelValues(s.String())
	}
	playStatus.Set(-1)
}

// Record publishes one probe result.
func Record(r Result, at time.Time) {
	attempts.WithLabelValues(r.Stage.String()).Inc()
	stage.Set(float64(r.Stage))
	duration.Set(r.Duration.Seconds())
	serverProtocol.Set(float64(r.Protocol))
	dialedProtocol.Set(float64(r.DialedProtocol))

	if r.Stage == StageRefused {
		playStatus.Set(float64(r.PlayStatus))
	} else {
		playStatus.Set(-1)
	}

	if r.Version != "" {
		// Reset first: the version is a label, so an upgrade would otherwise
		// leave the old one published at 1 forever beside the new one.
		serverInfo.Reset()
		serverInfo.WithLabelValues(r.Version).Set(1)
	}

	if r.Joinable() {
		joinable.Set(1)
		lastJoinable.Set(float64(at.Unix()))
		return
	}
	joinable.Set(0)
}
