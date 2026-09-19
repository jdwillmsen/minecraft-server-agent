package joinprobe

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

var recordedAt = time.Date(2026, 9, 19, 4, 0, 0, 0, time.UTC)

func TestRecordPublishesAJoinableServer(t *testing.T) {
	Record(Result{
		Stage: StageHandshake, Protocol: 2193, DialedProtocol: 2193,
		Version: "1.26.51", Duration: 120 * time.Millisecond,
	}, recordedAt)

	if got := testutil.ToFloat64(joinable); got != 1 {
		t.Errorf("joinable = %v, want 1", got)
	}
	if got := testutil.ToFloat64(lastJoinable); got != float64(recordedAt.Unix()) {
		t.Errorf("last joinable = %v, want %v", got, recordedAt.Unix())
	}
	// -1 rather than 0: zero is PlayStatusLoginSuccess, and this probe never
	// performs a login, so publishing it would claim something untrue.
	if got := testutil.ToFloat64(playStatus); got != -1 {
		t.Errorf("play status = %v, want -1 when the server did not refuse", got)
	}
}

func TestRecordPublishesARefusal(t *testing.T) {
	Record(Result{Stage: StageRefused, Protocol: 2169, DialedProtocol: 2193, PlayStatus: 2}, recordedAt)

	if got := testutil.ToFloat64(joinable); got != 0 {
		t.Errorf("joinable = %v, want 0 for a refused session", got)
	}
	if got := testutil.ToFloat64(playStatus); got != 2 {
		t.Errorf("play status = %v, want the server's 2", got)
	}
	if got := testutil.ToFloat64(stage); got != float64(StageRefused) {
		t.Errorf("stage = %v, want %v", got, float64(StageRefused))
	}
}

// The freshness gauge is what separates "cannot join right now" from "has
// never been able to join", so a failure must not move it.
func TestRecordLeavesTheLastJoinableTimeAloneOnFailure(t *testing.T) {
	Record(Result{Stage: StageHandshake, Version: "1.26.51"}, recordedAt)
	before := testutil.ToFloat64(lastJoinable)

	Record(Result{Stage: StageUnreachable}, recordedAt.Add(time.Hour))

	if got := testutil.ToFloat64(lastJoinable); got != before {
		t.Errorf("last joinable moved to %v on a failed probe, want it held at %v", got, before)
	}
}

// A version label that accumulates would leave the pre-upgrade version
// published at 1 forever, which is exactly the fact an upgrade is supposed to
// change.
func TestRecordReplacesTheVersionLabelOnUpgrade(t *testing.T) {
	Record(Result{Stage: StageHandshake, Version: "1.26.45"}, recordedAt)
	Record(Result{Stage: StageHandshake, Version: "1.26.51"}, recordedAt)

	got := testutil.CollectAndCount(serverInfo)
	if got != 1 {
		t.Errorf("%d version series published, want only the current one", got)
	}
	if err := testutil.CollectAndCompare(serverInfo, strings.NewReader(`
# HELP mc_joinprobe_server_info 1, labelled with the version string the server advertises.
# TYPE mc_joinprobe_server_info gauge
mc_joinprobe_server_info{version="1.26.51"} 1
`)); err != nil {
		t.Error(err)
	}
}

// Every stage has to exist from startup: a counter that springs into existence
// at 1 reads as no change to increase(), so the first refusal after a restart
// would not register.
func TestAttemptCountersExistBeforeTheFirstProbe(t *testing.T) {
	for _, s := range []Stage{StageUnreachable, StagePong, StageRefused, StageHandshake} {
		if got := testutil.ToFloat64(attempts.WithLabelValues(s.String())); got < 0 {
			t.Errorf("counter for stage %s is missing", s)
		}
	}
	if n := testutil.CollectAndCount(attempts); n != 4 {
		t.Errorf("%d attempt counters, want one per stage", n)
	}
}
