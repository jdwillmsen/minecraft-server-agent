package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Presence series are labelled by actor id, which comes from the configured
// actor registry and never from chat, so the label set is bounded by the
// Helm values that list the actors.
var (
	presenceDesired = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mc_presence_desired",
		Help: "1 if the actor's effective presence is present, 0 if parked. Exported by the leader only.",
	}, []string{"actor"})
	presenceObserved = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mc_presence_observed",
		Help: "1 if the actor is connected by its own recent report, 0 otherwise. Exported by the leader only.",
	}, []string{"actor"})
	presenceOverrideAge = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mc_presence_override_age_seconds",
		Help: "Age of the actor's override when it has no expiry; absent otherwise.",
	}, []string{"actor"})
	// A vec with no labels only so ResetPresence can withdraw it: a plain
	// gauge always exports its one series.
	presenceTickSuccess = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mc_presence_tick_success_timestamp_seconds",
		Help: "Unix time of the leader's last policy tick that read both the overrides and the actor statuses; 0 until the first. Exported by the leader only.",
	}, nil)
	presenceKicksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mc_presence_kicks_total",
		Help: "Kicks sent for an actor still on the server after its park grace.",
	}, []string{"actor"})
)

// InitPresence starts every actor's kick counter at zero, for the same
// reason init does for the fixed label sets. The tick stamp starts at the
// epoch so a leader that never ticks cleanly reads as stale rather than
// absent, but only if unset: Prime may already have stamped this turn.
func InitPresence(actors []string) {
	for _, a := range actors {
		presenceKicksTotal.WithLabelValues(a)
	}
	presenceTickSuccess.WithLabelValues()
}

func PresenceDesired(actor string, present bool) {
	presenceDesired.WithLabelValues(actor).Set(boolValue(present))
}

func PresenceObserved(actor string, connected bool) {
	presenceObserved.WithLabelValues(actor).Set(boolValue(connected))
}

// PresenceOverrideAge records how old an actor's open-ended override is.
// open false withdraws the series: an override with an expiry, or none at
// all, has no age worth alerting on.
func PresenceOverrideAge(actor string, age time.Duration, open bool) {
	if !open {
		presenceOverrideAge.DeleteLabelValues(actor)
		return
	}
	presenceOverrideAge.WithLabelValues(actor).Set(age.Seconds())
}

// PresenceTickSuccess stamps a tick that read both the overrides and the
// statuses. A failing store otherwise leaves the per-actor gauges looking
// current, since a failed tick keeps desired and the override age as they
// were; the stamp's age is what says they have stopped moving.
func PresenceTickSuccess(at time.Time) {
	presenceTickSuccess.WithLabelValues().Set(float64(at.UnixNano()) / float64(time.Second))
}

func PresenceKick(actor string) { presenceKicksTotal.WithLabelValues(actor).Inc() }

// ResetPresence withdraws every presence gauge when this process stops
// leading. The kick counter stays: a counter that vanished and came back
// would read as a reset to every rate() over it.
func ResetPresence() {
	presenceDesired.Reset()
	presenceObserved.Reset()
	presenceOverrideAge.Reset()
	presenceTickSuccess.Reset()
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
