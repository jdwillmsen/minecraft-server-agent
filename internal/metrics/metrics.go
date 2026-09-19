// Package metrics counts what the agent does, not just whether it is
// connected.
//
// Every series lives on the default registry, which the /metrics handler in
// internal/httpapi already serves, and is registered exactly once per process
// at package init -- so any number of tests, or of callers, can never
// double-register and panic. Callers record through the small functions here
// and never touch a Prometheus type: the metric names and label values are
// what dashboards and alerts elsewhere are written against, and keeping them
// in one file is what keeps them from drifting call site by call site.
//
// Every label value is bounded by construction. Nothing a player types, and
// nothing a model invents, reaches a label unfiltered: a label fed from chat
// is a series count any one player could inflate without limit.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/jdwillmsen/minecraft-server-agent/internal/moderation"
)

// moderationRules and moderationActions are the schema's own sets, so the
// counter's labels can never hold more than their product.
var (
	moderationRules   = []moderation.Rule{moderation.RuleTerm, moderation.RuleFlood, moderation.RuleCaps}
	moderationActions = []moderation.Action{moderation.ActionLogged, moderation.ActionWarned}
)

// Unregistered stands in for any command or tool name the registry does not
// hold, so arbitrary words collapse into one series.
const Unregistered = "unregistered"

// MentionOutcome is how one @server mention ended.
type MentionOutcome string

const (
	MentionAnswered      MentionOutcome = "answered"
	MentionFailed        MentionOutcome = "failed"
	MentionEmpty         MentionOutcome = "empty"
	MentionRateLimited   MentionOutcome = "rate_limited"
	MentionBusy          MentionOutcome = "busy"
	MentionUndeliverable MentionOutcome = "undeliverable"
	MentionSendFailed    MentionOutcome = "send_failed"
	MentionDisabled      MentionOutcome = "disabled"
)

var mentionOutcomes = []MentionOutcome{
	MentionAnswered, MentionFailed, MentionEmpty, MentionRateLimited,
	MentionBusy, MentionUndeliverable, MentionSendFailed, MentionDisabled,
}

// Delivery is how one announcement send reached its audience.
type Delivery string

const (
	DeliveryBroadcast Delivery = "broadcast"
	DeliveryWhisper   Delivery = "whisper"
	// DeliverySummary is the drain's "more are waiting" line, counted apart
	// from the announcements themselves because it is not one.
	DeliverySummary Delivery = "summary"
)

var deliveries = []Delivery{DeliveryBroadcast, DeliveryWhisper, DeliverySummary}

const (
	outcomeSent     = "sent"
	outcomeFailed   = "failed"
	outcomeOK       = "ok"
	outcomeError    = "error"
	outcomeAnswered = "answered"
)

var (
	commandsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mc_agent_commands_total",
		Help: "Command dispatches, by registered command (or unregistered) and audit outcome.",
	}, []string{"command", "outcome"})

	mentionsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mc_agent_mentions_total",
		Help: "@server mentions, by how each one ended.",
	}, []string{"outcome"})

	answerDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "mc_agent_answer_duration_seconds",
		Help: "Time the model took over one @server answer, tool rounds included.",
		// 30s is the default whole-answer budget, but LLM_TOTAL_TIMEOUT_MS
		// has no ceiling and must grow with the per-call timeout. The buckets
		// past 30 exist so a raised budget still shows up in p95, instead of
		// slow answers vanishing into +Inf with the quantile pinned at 30.
		Buckets: []float64{0.5, 1, 2, 4, 8, 15, 30, 45, 60, 90},
	}, []string{"outcome"})

	toolCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mc_agent_tool_calls_total",
		Help: "Tool invocations the model asked for, by registered tool (or unregistered) and outcome.",
	}, []string{"tool", "outcome"})

	announceDeliveriesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mc_agent_announce_deliveries_total",
		Help: "Announcement send attempts, by delivery kind and outcome.",
	}, []string{"delivery", "outcome"})

	auditWriteFailuresTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mc_agent_audit_write_failures_total",
		Help: "Command dispatches the audit trail failed to record.",
	})

	authRejectionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mc_agent_auth_rejections_total",
		Help: "Times Xbox Live rejected the account itself rather than the connection failing.",
	})

	// Counted when a flag is written, not when a rule fires, so this agrees
	// with the rows in minecraft.moderation_events.
	moderationFlagsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "mc_agent_moderation_flags_total",
		Help: "Moderation flags written to the record, by rule and the action taken.",
	}, []string{"rule", "action"})

	deathsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mc_agent_deaths_total",
		Help: "Deaths the respawner handled.",
	})

	// serverTPS and linkRTT are vectors with no labels so that neither
	// exists until it is first measured. A plain gauge exports 0 from
	// startup, and a zero TPS reads as a crashed server -- which on an agent
	// whose console bridge is down from the start would hold a low-TPS alert
	// firing over a server nobody measured.
	serverTPS = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mc_agent_server_tps",
		Help: "The last successfully measured server TPS. Never reset on a failed measurement; read its age from mc_agent_tps_last_success_timestamp_seconds.",
	}, nil)

	// Deliberately a plain gauge, unlike serverTPS: 0 here is the epoch, an
	// age no alert can mistake for fresh, so a measurement that has never
	// succeeded since startup reads as stale rather than as absent.
	tpsLastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_agent_tps_last_success_timestamp_seconds",
		Help: "Unix time of the last successful TPS measurement.",
	})

	linkRTT = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mc_agent_link_rtt_seconds",
		Help: "Round trip over the agent's Bedrock connection, as last sampled while a session existed.",
	}, nil)

	// A plain gauge, like tpsLastSuccess and for the same reason: 0 is the
	// epoch, which reads as "never" rather than as "just now".
	sessionEstablished = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "mc_agent_session_established_timestamp_seconds",
		Help: "Unix time the agent last reached spawn on a fresh Bedrock session. Proof a real account could join at that moment, which no reachability check can give.",
	})
	sessionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mc_agent_sessions_total",
		Help: "Bedrock sessions this process has taken all the way to spawn.",
	})
	sessionRecyclesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "mc_agent_session_recycles_total",
		Help: "Sessions ended deliberately on the recycle schedule, rather than by the server or a fault.",
	})
)

// Pre-initialised because increase() over a series that springs into
// existence at 1 reads as 0: without a zero to rise from, an alert on the
// first failure after a restart would never fire. The unlabelled counters
// need nothing here -- a plain counter exports 0 from registration.
func init() {
	for _, o := range mentionOutcomes {
		mentionsTotal.WithLabelValues(string(o))
	}
	for _, d := range deliveries {
		announceDeliveriesTotal.WithLabelValues(string(d), outcomeSent)
		announceDeliveriesTotal.WithLabelValues(string(d), outcomeFailed)
	}
	answerDuration.WithLabelValues(outcomeAnswered)
	answerDuration.WithLabelValues(outcomeFailed)
	// Every pair, including ones only term can reach today: which rule may
	// warn is the plugin's decision, and restating it here would be a second
	// place for it to drift.
	for _, r := range moderationRules {
		for _, a := range moderationActions {
			moderationFlagsTotal.WithLabelValues(string(r), string(a))
		}
	}
}

// InitCommands starts every command x outcome pair at zero, for the same
// reason init does for the fixed label sets. Command names come from the
// plugin registry, which only exists once main has built it, so this cannot
// run at package init.
func InitCommands(commands, outcomes []string) {
	for _, c := range commands {
		for _, o := range outcomes {
			commandsTotal.WithLabelValues(c, o)
		}
	}
}

// Command counts one dispatch. command must already be a registered name or
// Unregistered; the caller holds the registry, so it makes that call.
func Command(command, outcome string) {
	commandsTotal.WithLabelValues(command, outcome).Inc()
}

// Mention counts one @server mention.
func Mention(o MentionOutcome) {
	mentionsTotal.WithLabelValues(string(o)).Inc()
}

// Answer records how long one model call took, as answered when it returned
// without error and failed otherwise.
func Answer(took time.Duration, err error) {
	outcome := outcomeAnswered
	if err != nil {
		outcome = outcomeFailed
	}
	answerDuration.WithLabelValues(outcome).Observe(took.Seconds())
}

// ToolCall counts one tool invocation. tool must already be a registered
// name or Unregistered.
func ToolCall(tool string, err error) {
	outcome := outcomeOK
	if err != nil {
		outcome = outcomeError
	}
	toolCallsTotal.WithLabelValues(tool, outcome).Inc()
}

// AnnounceDelivery counts one send attempt, as sent when err is nil.
func AnnounceDelivery(d Delivery, err error) {
	outcome := outcomeSent
	if err != nil {
		outcome = outcomeFailed
	}
	announceDeliveriesTotal.WithLabelValues(string(d), outcome).Inc()
}

// AuditWriteFailure counts one dispatch the audit trail did not record.
func AuditWriteFailure() { auditWriteFailuresTotal.Inc() }

// AuthRejection counts one Xbox Live rejection of the account.
func AuthRejection() { authRejectionsTotal.Inc() }

// Death counts one death the respawner handled.
func Death() { deathsTotal.Inc() }

// ModerationFlag counts one flag written to the moderation record.
func ModerationFlag(rule moderation.Rule, action moderation.Action) {
	moderationFlagsTotal.WithLabelValues(string(rule), string(action)).Inc()
}

// ServerTPS records a successful TPS measurement taken at at. There is
// deliberately no way to record a failed one: the last real value beside its
// age tells the truth, where a sentinel would be a made-up reading.
func ServerTPS(tps float64, at time.Time) {
	serverTPS.WithLabelValues().Set(tps)
	tpsLastSuccess.Set(float64(at.UnixNano()) / float64(time.Second))
}

// LinkRTT records the current round trip over the Bedrock connection.
func LinkRTT(rtt time.Duration) {
	linkRTT.WithLabelValues().Set(rtt.Seconds())
}

// SessionEstablished records that a session reached spawn at at.
//
// Recorded at spawn rather than at connect: a connection that never spawns is
// not a session a player would call joining, and this gauge is read as proof
// that joining works.
func SessionEstablished(at time.Time) {
	sessionEstablished.Set(float64(at.Unix()))
	sessionsTotal.Inc()
}

// SessionRecycled counts one session ended on purpose by the schedule.
func SessionRecycled() { sessionRecyclesTotal.Inc() }
