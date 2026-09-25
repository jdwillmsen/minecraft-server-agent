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
	presenceKicksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mc_presence_kicks_total",
		Help: "Kicks sent for an actor still on the server after its park grace.",
	}, []string{"actor"})
)

// InitPresence starts every actor's kick counter at zero, for the same
// reason init does for the fixed label sets.
func InitPresence(actors []string) {
	for _, a := range actors {
		presenceKicksTotal.WithLabelValues(a)
	}
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

func PresenceKick(actor string) { presenceKicksTotal.WithLabelValues(actor).Inc() }

// ResetPresence withdraws every presence gauge when this process stops
// leading. The kick counter stays: a counter that vanished and came back
// would read as a reset to every rate() over it.
func ResetPresence() {
	presenceDesired.Reset()
	presenceObserved.Reset()
	presenceOverrideAge.Reset()
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
