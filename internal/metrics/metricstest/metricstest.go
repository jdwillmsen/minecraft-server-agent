// Package metricstest reads the agent's metrics back for tests in other
// packages.
//
// It reads through the default gatherer -- the same one the /metrics handler
// serves -- rather than through the collectors themselves, for two reasons: a
// test then proves the series is actually exported, not merely incremented
// on some object, and asking whether a series exists does not create it, which
// is the whole question a label-bounding test asks.
//
// Imported only from _test files, so none of it reaches the binary.
package metricstest

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Value returns the series name{labels}: a counter's or gauge's value, or a
// histogram's observation count. labels are name, value pairs and must match
// the series' label set exactly. A series that does not exist reads as 0,
// which is what a counter that was never incremented would say.
func Value(t testing.TB, name string, labels ...string) float64 {
	t.Helper()
	v, _ := find(t, name, labels)
	return v
}

// Delta runs fn and returns how far the series name{labels} moved. Every
// series is process-global and other tests move them too, so a test asks how
// much its own action changed a value rather than what the value is.
func Delta(t testing.TB, fn func(), name string, labels ...string) float64 {
	t.Helper()
	before := Value(t, name, labels...)
	fn()
	return Value(t, name, labels...) - before
}

// Exists reports whether the series name{labels} is exported at all.
func Exists(t testing.TB, name string, labels ...string) bool {
	t.Helper()
	_, ok := find(t, name, labels)
	return ok
}

// Series returns how many series the metric name currently exports.
func Series(t testing.TB, name string) int {
	t.Helper()
	n, err := testutil.GatherAndCount(prometheus.DefaultGatherer, name)
	if err != nil {
		t.Fatalf("gather %s: %v", name, err)
	}
	return n
}

func find(t testing.TB, name string, labels []string) (float64, bool) {
	t.Helper()
	if len(labels)%2 != 0 {
		t.Fatalf("labels for %s must be name, value pairs: %v", name, labels)
	}
	want := make(map[string]string, len(labels)/2)
	for i := 0; i < len(labels); i += 2 {
		want[labels[i]] = labels[i+1]
	}

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, m := range family.GetMetric() {
			pairs := m.GetLabel()
			if len(pairs) != len(want) {
				continue
			}
			match := true
			for _, p := range pairs {
				if v, ok := want[p.GetName()]; !ok || v != p.GetValue() {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			switch {
			case m.GetCounter() != nil:
				return m.GetCounter().GetValue(), true
			case m.GetGauge() != nil:
				return m.GetGauge().GetValue(), true
			case m.GetHistogram() != nil:
				return float64(m.GetHistogram().GetSampleCount()), true
			}
		}
	}
	return 0, false
}
