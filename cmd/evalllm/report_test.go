package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPercentileIsNearestRank(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 20; i++ {
		d = append(d, time.Duration(i)*time.Second)
	}
	if got := percentile(d, 0.50); got != 10*time.Second {
		t.Errorf("p50 = %s", got)
	}
	if got := percentile(d, 0.95); got != 19*time.Second {
		t.Errorf("p95 = %s", got)
	}
	if got := percentile(nil, 0.95); got != 0 {
		t.Errorf("empty p95 = %s", got)
	}
}

func TestReportShowsRatesAndFailuresWithTheActualReply(t *testing.T) {
	lim := testLimits()
	pass := Score(testCase(t, func(c *Case) { c.ID = "good" }), answered("Server is healthy."), lim)
	fail := Score(testCase(t, func(c *Case) { c.ID = "bad"; c.MustContain = []string{"1.21"} }), answered("It runs a | pipe?"), lim)
	broken := Score(testCase(t, func(c *Case) { c.ID = "down" }), Observation{Err: errors.New("llm: post: refused")}, lim)

	var b strings.Builder
	if err := writeReport(&b, reportMeta{Label: "unit", Model: "m", Host: "h:1"}, []Result{pass, fail, broken}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"**1 of 3 cases passed every check (33%).**",
		"| answered | 2 | 3 | 67% |",
		// Nothing in these cases states a content expectation except one.
		"| content | 0 | 1 | 0% |",
		`It runs a \| pipe?`,
		"no_question: ends with a question",
		"_(error: llm: post: refused)_",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestReportSaysNoneWhenNothingFailed(t *testing.T) {
	r := Score(testCase(t, nil), answered("ok."), testLimits())
	var b strings.Builder
	_ = writeReport(&b, reportMeta{}, []Result{r})
	if !strings.Contains(b.String(), "## Failures\n\nNone.") {
		t.Errorf("no explicit empty failures section:\n%s", b.String())
	}
}
