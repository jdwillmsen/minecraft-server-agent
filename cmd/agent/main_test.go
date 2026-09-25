package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"reflect"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/jdwillmsen/minecraft-server-agent/internal/config"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

func TestNextDelay_ResetsAfterAStableSession(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second

	got := nextDelay(120*time.Second, stableSessionThreshold, min, max)
	if got != min {
		t.Errorf("nextDelay after a stable session = %v, want %v", got, min)
	}
}

func TestNextDelay_DoublesAfterAShortSession(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second

	got := nextDelay(10*time.Second, stableSessionThreshold-time.Nanosecond, min, max)
	if got != 20*time.Second {
		t.Errorf("nextDelay = %v, want 20s", got)
	}
}

func TestNextDelay_ClampsToMax(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second

	got := nextDelay(200*time.Second, time.Second, min, max)
	if got != max {
		t.Errorf("nextDelay = %v, want the max %v", got, max)
	}
	if got := nextDelay(max, time.Second, min, max); got != max {
		t.Errorf("nextDelay already at max = %v, want %v", got, max)
	}
}

func TestNextDelay_NeverDropsBelowMin(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second

	if got := nextDelay(0, time.Second, min, max); got != min {
		t.Errorf("nextDelay from a zero delay = %v, want the min %v", got, min)
	}
}

func TestNextDelay_ReachesMaxWithoutOvershooting(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second

	delay := min
	for i := 0; i < 50; i++ {
		delay = nextDelay(delay, time.Second, min, max)
		if delay < min || delay > max {
			t.Fatalf("iteration %d: delay %v outside [%v, %v]", i, delay, min, max)
		}
	}
	if delay != max {
		t.Errorf("delay after 50 failed sessions = %v, want the max %v", delay, max)
	}
}

func TestJitter_StaysWithinHalfToFullRange(t *testing.T) {
	d := 100 * time.Second
	for i := 0; i < 1000; i++ {
		got := jitter(d)
		if got < d/2 || got > d {
			t.Fatalf("jitter(%v) = %v, want within [%v, %v]", d, got, d/2, d)
		}
	}
}

func TestJitter_NonPositiveIsReturnedUnchanged(t *testing.T) {
	if got := jitter(0); got != 0 {
		t.Errorf("jitter(0) = %v, want 0", got)
	}
	if got := jitter(-time.Second); got != -time.Second {
		t.Errorf("jitter(-1s) = %v, want -1s", got)
	}
}

func TestJitter_SubNanosecondDurationDoesNotPanic(t *testing.T) {
	// half == 0 would make rand.Int63n(0) panic if the bound weren't +1.
	if got := jitter(1); got != 0 && got != 1 {
		t.Errorf("jitter(1ns) = %v, want 0 or 1ns", got)
	}
}

func TestJitter_VariesAcrossCalls(t *testing.T) {
	d := time.Hour
	first := jitter(d)
	for i := 0; i < 100; i++ {
		if jitter(d) != first {
			return
		}
	}
	t.Errorf("jitter(%v) returned %v on 101 consecutive calls; backoff is not randomised", d, first)
}

// abuseModeRejectionError reproduces the exact error text the incident
// produced: an *oauth2.RetrieveError wrapped with %w up through xal, a
// gophertunnel net.OpError, and finally this package's own "dial: %w" in
// session -- every real layer in that chain wraps with %w and returns the
// RetrieveError bare, with no errors.Join anywhere in it. The errors.Join
// of a status-line error around it here is a deliberate superset, not a
// claim about how the vendored libraries behave: it proves errors.As finds
// the RetrieveError even through a join, which is a strictly harder case
// than the real chain, not a different one.
func abuseModeRejectionError() error {
	retrieveErr := &oauth2.RetrieveError{
		Response:         &http.Response{Status: "400 Bad Request", StatusCode: http.StatusBadRequest},
		ErrorCode:        "invalid_grant",
		ErrorDescription: "User account is found to be in service abuse mode.",
	}
	statusErr := errors.New("POST https://login.live.com/oauth20_token.srf: 400 Bad Request")
	joined := errors.Join(statusErr, retrieveErr)

	err := fmt.Errorf("xal/sisu: request access token for authorization: %w", joined)
	err = fmt.Errorf("authorize: %w", err)
	err = fmt.Errorf("request XSTS token: %w", err)
	err = &net.OpError{Op: "dial", Net: "minecraft", Err: fmt.Errorf("login to xbox live: %w", err)}
	return fmt.Errorf("dial: %w", err)
}

func TestAbuseModeRejectionError_MatchesTheIncidentText(t *testing.T) {
	want := "dial: dial minecraft: login to xbox live: request XSTS token: authorize: xal/sisu: request access token for authorization: POST https://login.live.com/oauth20_token.srf: 400 Bad Request\noauth2: \"invalid_grant\" \"User account is found to be in service abuse mode.\""
	if got := abuseModeRejectionError().Error(); got != want {
		t.Errorf("error text =\n%q\nwant\n%q", got, want)
	}
}

func TestIsAuthRejection_RecognisesTheWrappedIncidentChain(t *testing.T) {
	err := abuseModeRejectionError()
	if !isAuthRejection(err) {
		t.Errorf("isAuthRejection(%v) = false, want true", err)
	}

	var retrieveErr *oauth2.RetrieveError
	if !errors.As(err, &retrieveErr) {
		t.Fatal("errors.As found no *oauth2.RetrieveError in the wrapped chain -- the classifier would have fallen back to the string match alone")
	}
	if retrieveErr.ErrorCode != "invalid_grant" {
		t.Errorf("ErrorCode = %q, want invalid_grant", retrieveErr.ErrorCode)
	}
}

func TestIsAuthRejection_StringFallbackCatchesAnUntypedInvalidGrant(t *testing.T) {
	// No *oauth2.RetrieveError anywhere in this chain -- exercises the
	// fallback path directly, standing in for a layer that formatted the
	// rejection into a plain string instead of wrapping it with %w.
	err := fmt.Errorf("dial: login to xbox live: %w", errors.New(`oauth2: "invalid_grant" "User account is found to be in service abuse mode."`))
	if !isAuthRejection(err) {
		t.Errorf("isAuthRejection(%v) = false, want true", err)
	}
}

func TestIsAuthRejection_NetworkErrorIsTransient(t *testing.T) {
	err := fmt.Errorf("dial: %w", &net.OpError{Op: "dial", Net: "minecraft",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: connection refused")}})
	if isAuthRejection(err) {
		t.Errorf("isAuthRejection(%v) = true, want false", err)
	}
}

func TestIsAuthRejection_ProtocolKickIsTransient(t *testing.T) {
	err := errors.New("disconnect: You have been kicked: Please reconnect")
	if isAuthRejection(err) {
		t.Errorf("isAuthRejection(%v) = true, want false", err)
	}
}

func TestIsAuthRejection_NilErrorIsFalse(t *testing.T) {
	if isAuthRejection(nil) {
		t.Error("isAuthRejection(nil) = true, want false")
	}
}

func TestReconnectDelay_AuthRejectionUsesTheFlatFloorNotTheLadder(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second
	authDelay := 900 * time.Second

	// current is deliberately already near max: an auth rejection must not
	// inherit or extend the doubling ladder's position.
	wait, _, rejected := reconnectDelay(abuseModeRejectionError(), time.Second, 200*time.Second, min, max, authDelay)
	if !rejected {
		t.Fatal("reconnectDelay reported rejected = false for an auth rejection")
	}
	if wait != authDelay {
		t.Errorf("wait = %v, want the auth floor %v", wait, authDelay)
	}
}

func TestReconnectDelay_RepeatedRejectionsDoNotClimb(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second
	authDelay := 900 * time.Second

	ladder := min
	for i := 0; i < 5; i++ {
		var wait time.Duration
		var rejected bool
		wait, ladder, rejected = reconnectDelay(abuseModeRejectionError(), time.Second, ladder, min, max, authDelay)
		if !rejected || wait != authDelay {
			t.Fatalf("iteration %d: wait=%v rejected=%v, want %v/true every time", i, wait, rejected, authDelay)
		}
	}
	if ladder != min {
		t.Errorf("ladder position after 5 rejections = %v, want it to stay at %v", ladder, min)
	}
}

// TestReconnectDelay_LadderResumesWhereItLeftOffAfterARejection is the
// regression test for the bug where reconnectDelay's rejection branch fed
// authDelay back in as the caller's next current: nextDelay's invariant is
// that current stays within [min, max], and authDelay is chosen
// independently of both bounds, so a rejection followed by an ordinary
// failure computed nextDelay(authDelay, ...) instead of resuming from
// wherever the ladder actually was. It must fail if reconnectDelay ever
// starts returning the rejection floor as the next ladder position again.
func TestReconnectDelay_LadderResumesWhereItLeftOffAfterARejection(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second
	authDelay := 900 * time.Second
	ladderBeforeRejection := 40 * time.Second

	_, ladder, rejected := reconnectDelay(abuseModeRejectionError(), time.Second, ladderBeforeRejection, min, max, authDelay)
	if !rejected {
		t.Fatal("reconnectDelay reported rejected = false for an auth rejection")
	}

	transient := errors.New("disconnect: You have been kicked: Please reconnect")
	wait, _, rejected := reconnectDelay(transient, time.Second, ladder, min, max, authDelay)
	if rejected {
		t.Fatal("reconnectDelay reported rejected = true for a transient error")
	}

	want := nextDelay(ladderBeforeRejection, time.Second, min, max)
	if wait != want {
		t.Errorf("wait after the rejection episode = %v, want %v (nextDelay(%v, ...), as if the rejection had never happened)", wait, want, ladderBeforeRejection)
	}
}

func TestReconnectDelay_TransientErrorFollowsTheExistingLadder(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second
	authDelay := 900 * time.Second
	transient := errors.New("disconnect: You have been kicked: Please reconnect")

	wait, ladder, rejected := reconnectDelay(transient, time.Second, 10*time.Second, min, max, authDelay)
	if rejected {
		t.Error("reconnectDelay reported rejected = true for a transient error")
	}
	want := nextDelay(10*time.Second, time.Second, min, max)
	if wait != want {
		t.Errorf("wait = %v, want nextDelay's %v", wait, want)
	}
	if ladder != want {
		t.Errorf("ladder = %v, want it to match wait (%v) for a non-rejection", ladder, want)
	}
}

func TestReconnectDelay_TransientErrorStillResetsOnAStableSession(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second
	authDelay := 900 * time.Second
	transient := errors.New("disconnect: server closed the connection")

	wait, _, rejected := reconnectDelay(transient, stableSessionThreshold, 120*time.Second, min, max, authDelay)
	if rejected {
		t.Error("reconnectDelay reported rejected = true for a transient error")
	}
	if wait != min {
		t.Errorf("wait after a stable session = %v, want the min %v", wait, min)
	}
}

func TestReconnectDelay_NoErrorFollowsTheExistingLadder(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second
	authDelay := 900 * time.Second

	wait, _, rejected := reconnectDelay(nil, time.Second, 10*time.Second, min, max, authDelay)
	if rejected {
		t.Error("reconnectDelay reported rejected = true for a clean disconnect")
	}
	if want := nextDelay(10*time.Second, time.Second, min, max); wait != want {
		t.Errorf("wait = %v, want nextDelay's %v", wait, want)
	}
}

func TestWaitOrShutdown_ReturnsPromptlyOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	// A wait far longer than the incident's own 15-minute floor: this must
	// not actually be waited out just because shutdown was requested early.
	if waitOrShutdown(ctx, time.Hour) {
		t.Error("waitOrShutdown = true, want false when ctx is cancelled first")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("waitOrShutdown took %v to notice cancellation, want well under a second", elapsed)
	}
}

func TestWaitOrShutdown_ReturnsTrueWhenTheWaitElapsesFirst(t *testing.T) {
	if !waitOrShutdown(context.Background(), time.Millisecond) {
		t.Error("waitOrShutdown = false, want true when the wait elapses before ctx is ever cancelled")
	}
}

// The client keeps its logger optional so every test can build one without
// wiring, which makes the production omission silent: a client with no
// logger drops every tool_invocation_failed event, and the only symptom is
// an answer that quietly lacks a fact. Reaching for the unexported field is
// the point -- nothing exported reports whether the logger was attached.
func TestProductionLLMClientLogsToolFailures(t *testing.T) {
	client := newLLMClient(config.Config{LLMBaseURL: "http://llm.invalid"}, logging.New("info"))

	if reflect.ValueOf(client).Elem().FieldByName("log").IsNil() {
		t.Error("the production LLM client has no logger: failed tool invocations would be dropped")
	}
}

// newWiki must never return a nil plugin.Wiki -- plugin.Context.Wiki is a
// repo invariant enforced by TestEveryPluginContextFieldIsWired, and the
// disabled implementation is what keeps wiki_lookup out of the toolset
// (toolset.Build asks Enabled), not a nil interface a caller could panic on.
func TestNewWiki_DisabledReturnsAnUnusableButNonNilWiki(t *testing.T) {
	w := newWiki(config.Config{WikiEnabled: false}, logging.New("error"))

	if w == nil {
		t.Fatal("newWiki(disabled) returned nil, want wiki.Nop")
	}
	if w.Enabled() {
		t.Error("newWiki(disabled).Enabled() = true, want false")
	}
}

func TestNewWiki_EnabledReturnsAWorkingClient(t *testing.T) {
	w := newWiki(config.Config{WikiEnabled: true, WikiBaseURL: "http://wiki.invalid/api.php"}, logging.New("error"))

	if !w.Enabled() {
		t.Error("newWiki(enabled).Enabled() = false, want true")
	}
}
