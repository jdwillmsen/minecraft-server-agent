package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// gametimeBridge answers each POST /command with the next of outputs,
// repeating the last once they run out.
func gametimeBridge(t *testing.T, outputs ...string) *BridgeClient {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req commandRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Command != gametimeCommand {
			t.Errorf("command = %q, want %q", req.Command, gametimeCommand)
		}
		i := int(calls.Add(1)) - 1
		if i >= len(outputs) {
			i = len(outputs) - 1
		}
		json.NewEncoder(w).Encode(commandResponse{Rule: "time_query", Output: outputs[i]})
	}))
	t.Cleanup(srv.Close)
	return NewBridgeClient(srv.URL, "tok", time.Second)
}

func stamped(at string, ticks int64) string {
	return fmt.Sprintf("[%s INFO] Gametime is %d\n", at, ticks)
}

func TestServerPinger_TPSFromTheServerClock(t *testing.T) {
	// The pair read off the production console while this was built: 246
	// ticks across 12.302s of server time.
	p := NewServerPinger(gametimeBridge(t,
		stamped("2026-09-10 08:27:25:221", 524220674),
		stamped("2026-09-10 08:27:37:523", 524220920),
	), nil)

	if err := p.Sample(context.Background()); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	got := p.Ping(context.Background())
	if got.TPSErr != nil || !got.TPSKnown {
		t.Fatalf("TPS not measured: known=%v err=%v", got.TPSKnown, got.TPSErr)
	}
	if want := 246 / 12.302; math.Abs(got.TPS-want) > 0.001 {
		t.Errorf("TPS = %.4f, want %.4f", got.TPS, want)
	}
}

func TestServerPinger_FirstReadingIsStillMeasuring(t *testing.T) {
	p := NewServerPinger(gametimeBridge(t, stamped("2026-09-10 08:00:00:000", 1000)), nil)

	got := p.Ping(context.Background())
	if got.TPSErr != nil {
		t.Fatalf("unexpected error: %v", got.TPSErr)
	}
	if got.TPSKnown {
		t.Errorf("TPS reported as known (%.1f) with nothing to measure against", got.TPS)
	}
}

func TestServerPinger_BaselineOutsideTheWindowIsNotUsed(t *testing.T) {
	for _, tc := range []struct {
		name, first, second string
		secondTicks         int64
	}{
		{"too recent", "2026-09-10 08:00:00:000", "2026-09-10 08:00:05:000", 1100},
		{"too old", "2026-09-10 08:00:00:000", "2026-09-10 08:03:00:000", 4600},
		{"clock went backwards", "2026-09-10 08:00:00:000", "2026-09-10 08:00:30:000", 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewServerPinger(gametimeBridge(t, stamped(tc.first, 1000), stamped(tc.second, tc.secondTicks)), nil)
			if err := p.Sample(context.Background()); err != nil {
				t.Fatalf("Sample: %v", err)
			}
			if got := p.Ping(context.Background()); got.TPSKnown {
				t.Errorf("TPS = %.1f from a baseline that should have been refused", got.TPS)
			}
		})
	}
}

// A lag spike between the two newest readings must show, not be averaged
// away against an older one.
func TestServerPinger_MeasuresAgainstTheNewestBaseline(t *testing.T) {
	p := NewServerPinger(gametimeBridge(t,
		stamped("2026-09-10 08:00:00:000", 0),
		stamped("2026-09-10 08:00:30:000", 600),
		stamped("2026-09-10 08:01:00:000", 900),
	), nil)
	for range 2 {
		if err := p.Sample(context.Background()); err != nil {
			t.Fatalf("Sample: %v", err)
		}
	}

	got := p.Ping(context.Background())
	if !got.TPSKnown || got.TPS != 10 {
		t.Errorf("TPS = %.1f (known=%v), want 10.0 over the last 30s rather than 15.0 over the minute", got.TPS, got.TPSKnown)
	}
}

func TestServerPinger_FallsBackToTheAgentClock(t *testing.T) {
	p := NewServerPinger(gametimeBridge(t, "Gametime is 1000\n", "Gametime is 1400\n"), nil)
	start := time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)
	reads := []time.Time{start, start.Add(20 * time.Second)}
	p.now = func() time.Time {
		t := reads[0]
		reads = reads[1:]
		return t
	}

	if err := p.Sample(context.Background()); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if got := p.Ping(context.Background()); !got.TPSKnown || got.TPS != 20 {
		t.Errorf("TPS = %.1f (known=%v), want 20.0", got.TPS, got.TPSKnown)
	}
}

// The server's log clock and the agent's clock are different clocks; a span
// between one of each is meaningless.
func TestServerPinger_NeverMixesClocks(t *testing.T) {
	p := NewServerPinger(gametimeBridge(t, stamped("2026-09-10 08:00:00:000", 1000), "Gametime is 1400\n"), nil)
	p.now = func() time.Time { return time.Date(2026, 9, 10, 8, 0, 20, 0, time.UTC) }

	if err := p.Sample(context.Background()); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if got := p.Ping(context.Background()); got.TPSKnown {
		t.Errorf("TPS = %.1f measured across two different clocks", got.TPS)
	}
}

func TestServerPinger_ConsoleFailuresAreReported(t *testing.T) {
	t.Run("bridge refuses", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "console not connected", http.StatusBadGateway)
		}))
		defer srv.Close()
		p := NewServerPinger(NewBridgeClient(srv.URL, "tok", time.Second), nil)

		if got := p.Ping(context.Background()); got.TPSErr == nil || got.TPSKnown {
			t.Errorf("got known=%v err=%v, want an error", got.TPSKnown, got.TPSErr)
		}
	})
	t.Run("no Gametime line captured", func(t *testing.T) {
		p := NewServerPinger(gametimeBridge(t, ""), nil)

		if got := p.Ping(context.Background()); got.TPSErr == nil || got.TPSKnown {
			t.Errorf("got known=%v err=%v, want an error", got.TPSKnown, got.TPSErr)
		}
	})
}

func TestServerPinger_ReportsTheLink(t *testing.T) {
	bridge := gametimeBridge(t, stamped("2026-09-10 08:00:00:000", 1000))

	got := NewServerPinger(bridge, func() (time.Duration, bool) { return 6 * time.Millisecond, true }).Ping(context.Background())
	if !got.LinkKnown || got.Link != 6*time.Millisecond {
		t.Errorf("link = %v (known=%v), want 6ms", got.Link, got.LinkKnown)
	}

	got = NewServerPinger(bridge, func() (time.Duration, bool) { return 0, false }).Ping(context.Background())
	if got.LinkKnown {
		t.Error("link reported as known with no session")
	}

	if got = NewServerPinger(bridge, nil).Ping(context.Background()); got.LinkKnown {
		t.Error("link reported as known with no link source")
	}
}

func TestServerPinger_HistoryIsBounded(t *testing.T) {
	p := NewServerPinger(gametimeBridge(t, stamped("2026-09-10 08:00:00:000", 1000)), nil)
	for range maxTickSamples + 8 {
		if err := p.Sample(context.Background()); err != nil {
			t.Fatalf("Sample: %v", err)
		}
	}
	if len(p.samples) != maxTickSamples {
		t.Errorf("kept %d samples, want %d", len(p.samples), maxTickSamples)
	}
}
