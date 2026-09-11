package main

import (
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// reportMeta is what a reader needs to know to compare two reports: the
// same cases against a different model, prompt or setting.
type reportMeta struct {
	Label         string
	Model         string
	Host          string
	Started       time.Time
	MaxTokens     int
	Timeout       time.Duration
	Total         time.Duration
	LatencyBudget time.Duration
	MaxToolRounds int
}

func writeReport(w io.Writer, meta reportMeta, results []Result) error {
	var b strings.Builder

	fmt.Fprintf(&b, "# Model evaluation: %s\n\n", meta.Label)
	b.WriteString("| | |\n|---|---|\n")
	fmt.Fprintf(&b, "| Model | `%s` |\n", meta.Model)
	fmt.Fprintf(&b, "| Endpoint | `%s` |\n", meta.Host)
	fmt.Fprintf(&b, "| Run | %s |\n", meta.Started.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "| Settings | max_tokens %d, per-call timeout %s, total %s, %d tool rounds, latency budget %s |\n",
		meta.MaxTokens, seconds(meta.Timeout), seconds(meta.Total), meta.MaxToolRounds, seconds(meta.LatencyBudget))
	fmt.Fprintf(&b, "| Cases | %d |\n\n", len(results))

	passed := 0
	for _, r := range results {
		if r.Passed() {
			passed++
		}
	}
	b.WriteString("## Summary\n\n")
	fmt.Fprintf(&b, "**%d of %d cases passed every check (%s).**\n\n", passed, len(results), pct(passed, len(results)))
	b.WriteString("| Dimension | Passed | Scored | Rate |\n|---|---:|---:|---:|\n")
	for _, d := range dimensions {
		p, s := tally(results, d)
		fmt.Fprintf(&b, "| %s | %d | %d | %s |\n", d, p, s, pct(p, s))
	}

	lat := make([]time.Duration, 0, len(results))
	calls := 0
	for _, r := range results {
		lat = append(lat, r.Obs.Latency)
		calls += len(r.Obs.Rounds)
	}
	slices.Sort(lat)
	meanCalls := 0.0
	if len(results) > 0 {
		meanCalls = float64(calls) / float64(len(results))
	}
	fmt.Fprintf(&b, "\nLatency of the whole answer, tool rounds included: p50 %s, p95 %s, max %s. Backend calls per answer: mean %.1f.\n\n",
		seconds(percentile(lat, 0.50)), seconds(percentile(lat, 0.95)), seconds(percentile(lat, 1)), meanCalls)

	b.WriteString("## By category\n\n| Category | Passed | Cases |\n|---|---:|---:|\n")
	for _, cat := range categories {
		p, n := 0, 0
		for _, r := range results {
			if r.Case.Category == cat {
				n++
				if r.Passed() {
					p++
				}
			}
		}
		if n > 0 {
			fmt.Fprintf(&b, "| %s | %d | %d |\n", cat, p, n)
		}
	}

	b.WriteString("\n## Failures\n\n")
	if passed == len(results) {
		b.WriteString("None.\n")
	} else {
		b.WriteString("| Case | Category | Failed checks | Tools called | Reply |\n|---|---|---|---|---|\n")
		for _, r := range results {
			failed := r.Failed()
			if len(failed) == 0 {
				continue
			}
			parts := make([]string, 0, len(failed))
			for _, c := range failed {
				parts = append(parts, fmt.Sprintf("%s: %s", c.Dim, c.Detail))
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n",
				r.Case.ID, r.Case.Category, cell(strings.Join(parts, "; ")), toolsCell(r.Obs), replyCell(r.Obs))
		}
	}

	b.WriteString("\n## All cases\n\n| Case | Result | Tools called | Whispered | Latency | Reply |\n|---|---|---|---|---:|---|\n")
	for _, r := range results {
		result := "pass"
		if !r.Passed() {
			result = "FAIL"
		}
		whispered := "no"
		if r.Obs.Private {
			whispered = "yes"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n",
			r.Case.ID, result, toolsCell(r.Obs), whispered, seconds(r.Obs.Latency), replyCell(r.Obs))
	}

	_, err := io.WriteString(w, b.String())
	return err
}

func tally(results []Result, d Dimension) (passed, scored int) {
	for _, r := range results {
		for _, c := range r.Checks {
			if c.Dim != d || !c.Scored {
				continue
			}
			scored++
			if c.Pass {
				passed++
			}
		}
	}
	return passed, scored
}

// pct says "n/a" for a dimension nothing scored, rather than a 0% that
// reads as every case failing it.
func pct(n, of int) string {
	if of == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(n)/float64(of))
}

// percentile is nearest-rank over sorted durations: with a few dozen
// cases, an interpolated p95 would describe an answer that never happened.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

var cellEscaper = strings.NewReplacer("|", `\|`, "\r", " ", "\n", " ")

func cell(s string) string { return cellEscaper.Replace(s) }

func toolsCell(o Observation) string {
	called := o.ToolsCalled()
	if len(called) == 0 {
		return "none"
	}
	return cell(strings.Join(called, ", "))
}

func replyCell(o Observation) string {
	switch {
	case o.Err != nil:
		return "_(error: " + cell(o.Err.Error()) + ")_"
	case o.Reply == "":
		return "_(no reply)_"
	}
	return cell(o.Reply)
}
