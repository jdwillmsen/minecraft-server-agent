package main

import (
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

// abuseModeRejectionError reproduces the exact error shape the incident
// produced: an *oauth2.RetrieveError joined with a plain status-line error
// (the way go-xsapi's token refresher actually returns a rejected refresh),
// then wrapped with %w up through xal, a gophertunnel net.OpError, and
// finally this package's own "dial: %w" in session. Constructing it this
// way, rather than a hand-picked string, is what proves the classifier
// survives the real wrapping rather than a convenient approximation of it.
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
	delay, rejected := reconnectDelay(abuseModeRejectionError(), time.Second, 200*time.Second, min, max, authDelay)
	if !rejected {
		t.Fatal("reconnectDelay reported rejected = false for an auth rejection")
	}
	if delay != authDelay {
		t.Errorf("delay = %v, want the auth floor %v", delay, authDelay)
	}
}

func TestReconnectDelay_RepeatedRejectionsDoNotClimb(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second
	authDelay := 900 * time.Second

	delay := min
	for i := 0; i < 5; i++ {
		var rejected bool
		delay, rejected = reconnectDelay(abuseModeRejectionError(), time.Second, delay, min, max, authDelay)
		if !rejected || delay != authDelay {
			t.Fatalf("iteration %d: delay=%v rejected=%v, want %v/true every time", i, delay, rejected, authDelay)
		}
	}
}

func TestReconnectDelay_TransientErrorFollowsTheExistingLadder(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second
	authDelay := 900 * time.Second
	transient := errors.New("disconnect: You have been kicked: Please reconnect")

	delay, rejected := reconnectDelay(transient, time.Second, 10*time.Second, min, max, authDelay)
	if rejected {
		t.Error("reconnectDelay reported rejected = true for a transient error")
	}
	if want := nextDelay(10*time.Second, time.Second, min, max); delay != want {
		t.Errorf("delay = %v, want nextDelay's %v", delay, want)
	}
}

func TestReconnectDelay_TransientErrorStillResetsOnAStableSession(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second
	authDelay := 900 * time.Second
	transient := errors.New("disconnect: server closed the connection")

	delay, rejected := reconnectDelay(transient, stableSessionThreshold, 120*time.Second, min, max, authDelay)
	if rejected {
		t.Error("reconnectDelay reported rejected = true for a transient error")
	}
	if delay != min {
		t.Errorf("delay after a stable session = %v, want the min %v", delay, min)
	}
}

func TestReconnectDelay_NoErrorFollowsTheExistingLadder(t *testing.T) {
	min := 5 * time.Second
	max := 300 * time.Second
	authDelay := 900 * time.Second

	delay, rejected := reconnectDelay(nil, time.Second, 10*time.Second, min, max, authDelay)
	if rejected {
		t.Error("reconnectDelay reported rejected = true for a clean disconnect")
	}
	if want := nextDelay(10*time.Second, time.Second, min, max); delay != want {
		t.Errorf("delay = %v, want nextDelay's %v", delay, want)
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
