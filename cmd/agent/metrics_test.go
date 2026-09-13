package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/audit"
	"github.com/jdwillmsen/minecraft-server-agent/internal/chat"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/ratelimit"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/liveness"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

const (
	commandsMetric = "mc_agent_commands_total"
	mentionsMetric = "mc_agent_mentions_total"
	answerMetric   = "mc_agent_answer_duration_seconds"
)

var quietLog = logging.New("error")

// runCommand dispatches one typed line through handleCommand against the
// standard harness.
func runCommand(t *testing.T, line string, limiter *ratelimit.PerActor, auditor audit.Store) {
	t.Helper()
	registry, pctx, _, _, _, playerRoster, permResolver := newHarness(t)
	handleCommand(context.Background(), playerXUID, chat.ParseTrigger(line), false, quietLog,
		registry, pctx, limiter, permResolver, auditor, playerRoster)
}

func TestCommandsAreCountedUnderTheirRegisteredName(t *testing.T) {
	got := metricstest.Delta(t, func() {
		runCommand(t, "!PING", unlimitedRateLimit(), &recordingAudit{})
	}, commandsMetric, "command", "ping", "outcome", "ok")
	if got != 1 {
		t.Errorf("ping/ok moved by %v, want 1", got)
	}
}

func TestADeniedCommandIsCountedWithItsOutcome(t *testing.T) {
	got := metricstest.Delta(t, func() {
		runCommand(t, "!shutdown", unlimitedRateLimit(), &recordingAudit{})
	}, commandsMetric, "command", "shutdown", "outcome", "denied")
	if got != 1 {
		t.Errorf("shutdown/denied moved by %v, want 1", got)
	}
}

// The guard on the label: a player can type any word after "!", and a word
// that reached the label would be a new series per word.
func TestAnUnregisteredCommandIsCountedAsUnregisteredNotAsTheTypedWord(t *testing.T) {
	const typed = "totallymadeupcommand"
	got := metricstest.Delta(t, func() {
		runCommand(t, "!"+typed, unlimitedRateLimit(), &recordingAudit{})
	}, commandsMetric, "command", metrics.Unregistered, "outcome", "unknown")
	if got != 1 {
		t.Errorf("unregistered/unknown moved by %v, want 1", got)
	}
	if metricstest.Exists(t, commandsMetric, "command", typed, "outcome", "unknown") {
		t.Errorf("the typed word %q became a command label", typed)
	}
}

// A rate-limited dispatch is refused before the registry is ever asked to
// run it, so it is the path most likely to leak a typed word into the label.
func TestARateLimitedUnregisteredCommandIsStillUnregistered(t *testing.T) {
	const typed = "spammedgibberish"
	got := metricstest.Delta(t, func() {
		runCommand(t, "!"+typed, ratelimit.NewPerActor(0, time.Minute), &recordingAudit{})
	}, commandsMetric, "command", metrics.Unregistered, "outcome", "rate_limited")
	if got != 1 {
		t.Errorf("unregistered/rate_limited moved by %v, want 1", got)
	}
	if metricstest.Exists(t, commandsMetric, "command", typed, "outcome", "rate_limited") {
		t.Errorf("the typed word %q became a command label", typed)
	}
}

// A dispatch happened whether or not a database is configured to record it.
func TestCommandsAreCountedWithNoAuditStore(t *testing.T) {
	got := metricstest.Delta(t, func() {
		runCommand(t, "!ping", unlimitedRateLimit(), audit.Nop{})
	}, commandsMetric, "command", "ping", "outcome", "ok")
	if got != 1 {
		t.Errorf("ping/ok moved by %v, want 1", got)
	}
}

func TestEveryRegisteredCommandStartsAtZero(t *testing.T) {
	registry := plugin.NewRegistry()
	if err := registerPlugins(t.Context(), registry, nil, newJoinTimes(), nil, quietLog); err != nil {
		t.Fatalf("registerPlugins: %v", err)
	}

	names := []string{metrics.Unregistered}
	for _, cmd := range registry.Commands() {
		names = append(names, cmd.Name)
	}
	for _, name := range names {
		for _, o := range audit.Outcomes() {
			if !metricstest.Exists(t, commandsMetric, "command", name, "outcome", string(o)) {
				t.Errorf("%s/%s is not pre-initialised", name, o)
			}
		}
	}
}

func TestAFailedAuditWriteIsCounted(t *testing.T) {
	got := metricstest.Delta(t, func() {
		runCommand(t, "!ping", unlimitedRateLimit(), &recordingAudit{err: errors.New("database on fire")})
	}, "mc_agent_audit_write_failures_total")
	if got != 1 {
		t.Errorf("audit write failures moved by %v, want 1", got)
	}
}

// The trail swallows a missing table so the log says it once, but every
// such write is still a record the trail does not hold -- and it must be
// counted exactly once, not again by the caller it was hidden from.
func TestAnUnreadyAuditTableCountsEveryLostRecordOnce(t *testing.T) {
	store := &countingAudit{err: fmt.Errorf("audit: write: %w", &pgconn.PgError{
		Code: "42P01", Message: `relation "minecraft.command_audit" does not exist`,
	})}
	trail := newAuditTrail(store, quietLog)

	got := metricstest.Delta(t, func() {
		runCommand(t, "!ping", unlimitedRateLimit(), trail)
		runCommand(t, "!help", unlimitedRateLimit(), trail)
	}, "mc_agent_audit_write_failures_total")
	if got != 2 {
		t.Errorf("audit write failures moved by %v, want 2 -- one per lost record", got)
	}
}

func TestAMentionWithAnsweringDisabledIsCounted(t *testing.T) {
	_, pctx, _, _, _, playerRoster, _ := newHarness(t)
	got := metricstest.Delta(t, func() {
		startAnswer(context.Background(), playerXUID, chat.ParseTrigger("@server hi"), false, quietLog, pctx, testAnswering(), playerRoster)
	}, mentionsMetric, "outcome", "disabled")
	if got != 1 {
		t.Errorf("disabled moved by %v, want 1", got)
	}
}

// enabledAnswering is testAnswering with a backend configured, so startAnswer
// gets past the disabled check to the refusals behind it.
func enabledAnswering(url string) answering {
	ans := testAnswering()
	ans.llm = adapters.NewLLMClient(url, "test-model", "", 192, 5*time.Second, quietLog)
	return ans
}

func TestARateLimitedMentionIsCounted(t *testing.T) {
	_, pctx, _, _, _, playerRoster, _ := newHarness(t)
	ans := enabledAnswering("http://llm.invalid")
	ans.limiter = ratelimit.NewPerActor(0, time.Minute)

	got := metricstest.Delta(t, func() {
		startAnswer(context.Background(), playerXUID, chat.ParseTrigger("@server hi"), false, quietLog, pctx, ans, playerRoster)
	}, mentionsMetric, "outcome", "rate_limited")
	if got != 1 {
		t.Errorf("rate_limited moved by %v, want 1", got)
	}
}

func TestAMentionDroppedAsBusyIsCounted(t *testing.T) {
	_, pctx, _, _, _, playerRoster, _ := newHarness(t)
	ans := enabledAnswering("http://llm.invalid")
	ans.inFlight = make(chan struct{}, 1)
	ans.inFlight <- struct{}{}

	got := metricstest.Delta(t, func() {
		startAnswer(context.Background(), playerXUID, chat.ParseTrigger("@server hi"), false, quietLog, pctx, ans, playerRoster)
	}, mentionsMetric, "outcome", "busy")
	if got != 1 {
		t.Errorf("busy moved by %v, want 1", got)
	}
}

// failingVoice refuses every send, as a bridge that is down does.
type failingVoice struct{}

func (failingVoice) Tell(context.Context, string, string) error { return errors.New("bridge down") }
func (failingVoice) Say(context.Context, string) error          { return errors.New("bridge down") }

const emptyAnswer = `{"choices":[{"message":{"content":""}}]}`

// Each case drives handleMention directly: it is synchronous, so the counts
// are settled when it returns, with no goroutine to wait for.
func TestEveryAnsweredMentionOutcomeIsCounted(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		reply     string
		voice     plugin.Voice
		noVoice   bool
		mention   string
		answerAs  string
		answerHit float64
	}{
		{name: "answered", status: http.StatusOK, reply: beaconAnswer, mention: "answered", answerAs: "answered", answerHit: 1},
		{name: "empty completion", status: http.StatusOK, reply: emptyAnswer, mention: "empty", answerAs: "answered", answerHit: 1},
		{name: "backend failure", status: http.StatusInternalServerError, reply: "{}", mention: "failed", answerAs: "failed", answerHit: 1},
		{name: "no voice", status: http.StatusOK, reply: beaconAnswer, noVoice: true, mention: "undeliverable", answerAs: "answered", answerHit: 1},
		{name: "send failed", status: http.StatusOK, reply: beaconAnswer, voice: failingVoice{}, mention: "send_failed", answerAs: "answered", answerHit: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.reply))
			}))
			defer backend.Close()

			_, pctx, _, _, _, playerRoster, _ := newHarness(t)
			if tc.voice != nil {
				pctx.Voice = tc.voice
			}
			if tc.noVoice {
				pctx.Voice = nil
			}
			ans := enabledAnswering(backend.URL)

			var mention float64
			answer := metricstest.Delta(t, func() {
				mention = metricstest.Delta(t, func() {
					handleMention(context.Background(), playerXUID, chat.ParseTrigger("@server hi"), false, quietLog, pctx, ans, playerRoster)
				}, mentionsMetric, "outcome", tc.mention)
			}, answerMetric, "outcome", tc.answerAs)

			if mention != 1 {
				t.Errorf("mentions %s moved by %v, want 1", tc.mention, mention)
			}
			if answer != tc.answerHit {
				t.Errorf("answer duration %s observations moved by %v, want %v", tc.answerAs, answer, tc.answerHit)
			}
		})
	}
}

func TestAnAuthRejectionIsCounted(t *testing.T) {
	const name = "mc_agent_auth_rejections_total"
	if got := metricstest.Delta(t, func() {
		reportSessionEnd(quietLog, "agent", abuseModeRejectionError(), true, time.Second, time.Minute)
	}, name); got != 1 {
		t.Errorf("auth rejections moved by %v, want 1", got)
	}
	if got := metricstest.Delta(t, func() {
		reportSessionEnd(quietLog, "agent", errors.New("dial: connection refused"), false, time.Second, time.Minute)
	}, name); got != 0 {
		t.Errorf("an ordinary session error moved auth rejections by %v, want 0", got)
	}
}

// discardWriter accepts every packet the respawner sends.
type discardWriter struct{}

func (discardWriter) WritePacket(packet.Packet) error { return nil }

func TestADeathIsCountedAndTheRespawnIsNot(t *testing.T) {
	const name = "mc_agent_deaths_total"
	respawner := liveness.New(42)
	var ready []bool
	setReady := func(r bool) { ready = append(ready, r) }

	if got := metricstest.Delta(t, func() {
		handleLiveness(&packet.DeathInfo{Cause: "lava"}, respawner, discardWriter{}, quietLog, setReady)
	}, name); got != 1 {
		t.Errorf("deaths moved by %v on a death, want 1", got)
	}
	if got := metricstest.Delta(t, func() {
		handleLiveness(&packet.Respawn{State: packet.RespawnStateReadyToSpawn}, respawner, discardWriter{}, quietLog, setReady)
	}, name); got != 0 {
		t.Errorf("deaths moved by %v on the respawn, want 0", got)
	}
	if len(ready) != 2 || ready[0] || !ready[1] {
		t.Errorf("readiness went %v, want [false true]: not ready while dead, ready once respawned", ready)
	}
}

// refusingPinger is a ServerPinger whose console never answers, so a test
// sees what the sampler records on its own, apart from any TPS.
func refusingPinger(t *testing.T) *adapters.ServerPinger {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "console not connected", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	return adapters.NewServerPinger(adapters.NewBridgeClient(srv.URL, "tok", time.Second), nil, quietLog)
}

func TestTheSamplerRecordsTheLinkEvenWhenTheConsoleIsDown(t *testing.T) {
	const name = "mc_agent_link_rtt_seconds"
	pinger := refusingPinger(t)
	var link linkMeter
	link.beginSession(fixedLatency(3 * time.Millisecond))

	sampleOnce(context.Background(), pinger, link.roundTrip, time.Second, quietLog)
	if got := metricstest.Value(t, name); math.Abs(got-0.006) > 1e-9 {
		t.Errorf("link rtt = %v, want 0.006: the full round trip, in seconds", got)
	}

	// With no session there is nothing to measure, and the last real reading
	// stays rather than being replaced by a zero that reads as a perfect link.
	link.endSession()
	sampleOnce(context.Background(), pinger, link.roundTrip, time.Second, quietLog)
	if got := metricstest.Value(t, name); math.Abs(got-0.006) > 1e-9 {
		t.Errorf("link rtt = %v after the session ended, want the last reading 0.006", got)
	}
}
