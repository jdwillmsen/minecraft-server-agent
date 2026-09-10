package adapters

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
)

const (
	tpsMetric         = "mc_agent_server_tps"
	tpsSuccessMetric  = "mc_agent_tps_last_success_timestamp_seconds"
	measuredAtSeconds = 1789030800 // 2026-09-10 09:00:00 UTC
)

func measuredAt() time.Time { return time.Unix(measuredAtSeconds, 0) }

func assertTPS(t *testing.T, want float64) {
	t.Helper()
	if got := metricstest.Value(t, tpsMetric); math.Abs(got-want) > 1e-6 {
		t.Errorf("tps gauge = %v, want %v", got, want)
	}
	if got := metricstest.Value(t, tpsSuccessMetric); math.Abs(got-measuredAtSeconds) > 1e-3 {
		t.Errorf("last success = %v, want %v", got, float64(measuredAtSeconds))
	}
}

// The background sampler is what keeps the gauge fresh when nobody types
// !ping, so its own measurements have to reach it.
func TestTheBackgroundSampleRecordsAMeasuredTPS(t *testing.T) {
	p := NewServerPinger(gametimeBridge(t,
		stamped("2026-09-10 08:00:00:000", 1000),
		stamped("2026-09-10 08:00:20:000", 1300),
	), nil, quietLog)
	p.now = measuredAt

	for range 2 {
		if err := p.Sample(context.Background()); err != nil {
			t.Fatalf("Sample: %v", err)
		}
	}
	assertTPS(t, 15)
}

// A player's !ping measures too, and is often fresher than the minute-old
// background reading.
func TestAPingRecordsTheTPSItMeasured(t *testing.T) {
	p := NewServerPinger(gametimeBridge(t,
		stamped("2026-09-10 08:00:00:000", 1000),
		stamped("2026-09-10 08:00:20:000", 1400),
	), nil, quietLog)
	p.now = measuredAt

	if err := p.Sample(context.Background()); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if got := p.Ping(context.Background()); !got.TPSKnown {
		t.Fatalf("ping did not measure TPS: %v", got.TPSErr)
	}
	assertTPS(t, 20)
}

// A failed measurement leaves the last real value and its age alone. A zero
// or a sentinel would read on a dashboard as a crashed server, when all that
// is known is that nobody could ask.
func TestAFailedMeasurementNeverResetsTheGauge(t *testing.T) {
	good := NewServerPinger(gametimeBridge(t,
		stamped("2026-09-10 08:00:00:000", 1000),
		stamped("2026-09-10 08:00:20:000", 1360),
	), nil, quietLog)
	good.now = measuredAt
	for range 2 {
		if err := good.Sample(context.Background()); err != nil {
			t.Fatalf("Sample: %v", err)
		}
	}
	assertTPS(t, 18)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "console not connected", http.StatusBadGateway)
	}))
	defer srv.Close()
	down := NewServerPinger(NewBridgeClient(srv.URL, "tok", time.Second), nil, quietLog)
	down.now = func() time.Time { return measuredAt().Add(time.Hour) }
	if err := down.Sample(context.Background()); err == nil {
		t.Fatal("a refused console sampled successfully")
	}
	if got := down.Ping(context.Background()); got.TPSKnown {
		t.Fatal("a refused console measured TPS")
	}
	assertTPS(t, 18)

	// The console answering but with nothing yet to compare against is not a
	// measurement either.
	first := NewServerPinger(gametimeBridge(t, stamped("2026-09-10 10:00:00:000", 5000)), nil, quietLog)
	first.now = func() time.Time { return measuredAt().Add(2 * time.Hour) }
	if err := first.Sample(context.Background()); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	assertTPS(t, 18)
}
