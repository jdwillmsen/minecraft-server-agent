// Command evalllm puts a fixed set of questions to the configured model
// through the agent's real answer path and scores the replies.
//
// It exists so a model, prompt or parameter change is judged on numbers
// instead of a handful of questions typed into chat. The capabilities
// behind the tools are fixtures, so two runs differ only in what the model
// did. Not part of CI: it needs a GPU endpoint and takes minutes.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/adapters"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

type options struct {
	casesPath     string
	baseURL       string
	model         string
	apiKey        string
	maxTokens     int
	timeout       time.Duration
	total         time.Duration
	latencyBudget time.Duration
	only          *regexp.Regexp
	label         string
	out           string
	trace         string
	maxToolRounds int
	wiki          bool
}

// traceLine is one case's raw exchanges. The report shows what the agent
// would have said; the trace shows why, which is what a prompt change has
// to be reasoned from.
type traceLine struct {
	ID     string  `json:"id"`
	Reply  string  `json:"reply"`
	Error  string  `json:"error,omitempty"`
	Rounds []Round `json:"rounds"`
}

// parseOptions defaults every model setting from the variable the agent
// itself reads, so a run with no flags measures what production runs.
func parseOptions(args []string, stderr io.Writer) (options, error) {
	var o options
	maxTokens, err := envInt("LLM_MAX_TOKENS", 192)
	if err != nil {
		return o, err
	}
	timeoutMs, err := envInt("LLM_TIMEOUT_MS", 8000)
	if err != nil {
		return o, err
	}
	totalMs, err := envInt("LLM_TOTAL_TIMEOUT_MS", 30000)
	if err != nil {
		return o, err
	}
	maxToolRounds, err := envInt("MAX_TOOL_ROUNDS", adapters.DefaultMaxToolRounds)
	if err != nil {
		return o, err
	}

	fs := flag.NewFlagSet("evalllm", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.casesPath, "cases", "eval/cases.yaml", "case file")
	fs.StringVar(&o.baseURL, "base-url", os.Getenv("LLM_BASE_URL"), "OpenAI-compatible base URL (LLM_BASE_URL)")
	fs.StringVar(&o.model, "model", os.Getenv("LLM_MODEL"), "model name (LLM_MODEL)")
	fs.IntVar(&o.maxTokens, "max-tokens", maxTokens, "max tokens per call (LLM_MAX_TOKENS)")
	fs.IntVar(&timeoutMs, "timeout-ms", timeoutMs, "per-call timeout (LLM_TIMEOUT_MS)")
	fs.IntVar(&totalMs, "total-timeout-ms", totalMs, "whole-answer timeout (LLM_TOTAL_TIMEOUT_MS)")
	fs.IntVar(&o.maxToolRounds, "max-tool-rounds", maxToolRounds, "tool rounds per answer (MAX_TOOL_ROUNDS)")
	budgetMs := fs.Int("latency-budget-ms", 0, "latency pass threshold per answer (default: the per-call timeout)")
	only := fs.String("only", "", "regexp: run only cases whose id or category matches")
	fs.StringVar(&o.label, "label", "evaluation", "title for the report, e.g. baseline")
	fs.StringVar(&o.out, "out", "", "also write the report to this file")
	fs.StringVar(&o.trace, "trace", "", "write every case's raw exchanges to this file as JSON lines")
	fs.BoolVar(&o.wiki, "wiki", true, "offer wiki_lookup and run wiki cases; -wiki=false measures the WIKI_ENABLED=off default")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: evalllm [flags]   runs eval/cases.yaml against the model; markdown report on stdout")
		fmt.Fprintln(stderr, "the API key is read from LLM_API_KEY only, so it never appears in a process list")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	o.apiKey = os.Getenv("LLM_API_KEY")
	o.timeout = time.Duration(timeoutMs) * time.Millisecond
	o.total = time.Duration(totalMs) * time.Millisecond
	// The per-call timeout by default: it is the operator's own figure for
	// how long one model call may take, and an answer slower than one call
	// allows has lost the moment even when it does arrive.
	o.latencyBudget = o.timeout
	if *budgetMs > 0 {
		o.latencyBudget = time.Duration(*budgetMs) * time.Millisecond
	}
	switch {
	case o.baseURL == "":
		return o, errors.New("no endpoint: set LLM_BASE_URL or -base-url")
	case o.model == "":
		return o, errors.New("no model: set LLM_MODEL or -model")
	case o.maxTokens <= 0 || timeoutMs <= 0 || totalMs <= 0:
		return o, errors.New("-max-tokens, -timeout-ms and -total-timeout-ms must be positive")
	}
	if *only != "" {
		re, err := regexp.Compile(*only)
		if err != nil {
			return o, fmt.Errorf("-only: %w", err)
		}
		o.only = re
	}
	return o, nil
}

func envInt(name string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer, got %q", name, raw)
	}
	return n, nil
}

// run returns 0 whenever the suite ran, however many cases failed: a
// failing case is the measurement, not an error in taking it.
func run(args []string, stdout, stderr io.Writer) int {
	o, err := parseOptions(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}

	knownTools := fixtureToolNames()
	cases, err := loadCases(o.casesPath, knownTools)
	if err != nil {
		fmt.Fprintf(stdout, "error: %v\n", err)
		return 1
	}
	cases = selectCases(cases, o.only)
	cases, wikiSkipped := filterWiki(cases, o.wiki)
	if len(cases) == 0 {
		fmt.Fprintf(stdout, "error: no case id or category matches -only %q\n", o.only)
		return 1
	}

	recorder := NewRecorder(o.baseURL)
	localURL, stop, err := recorder.Start()
	if err != nil {
		fmt.Fprintf(stdout, "error: start recorder: %v\n", err)
		return 1
	}
	defer stop()

	var trace *json.Encoder
	if o.trace != "" {
		f, err := os.Create(o.trace)
		if err != nil {
			fmt.Fprintf(stdout, "error: %v\n", err)
			return 1
		}
		defer f.Close()
		trace = json.NewEncoder(f)
	}

	r := runner{
		client:   adapters.NewLLMClient(localURL, o.model, o.apiKey, o.maxTokens, o.timeout, nil, adapters.WithMaxToolRounds(o.maxToolRounds)),
		recorder: recorder,
		total:    o.total,
		world:    func() *plugin.Context { return fixtureContext(o.wiki) },
	}
	lim := Limits{
		MaxReplyChars: adapters.MaxReplyChars,
		LatencyBudget: o.latencyBudget,
		KnownTools:    knownTools,
		Owners:        coordinateOwners(),
		Facts:         fixtureFacts(),
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	started := time.Now()
	var results []Result
	// Strictly one case at a time. The endpoint also serves live players,
	// and parallel cases would both slow their answers and measure a
	// contended endpoint instead of the model.
	for i, c := range cases {
		if ctx.Err() != nil {
			fmt.Fprintln(stderr, "interrupted; reporting the cases that finished")
			break
		}
		res := Score(c, r.run(ctx, c), lim)
		results = append(results, res)
		if trace != nil {
			line := traceLine{ID: c.ID, Reply: res.Obs.Reply, Rounds: res.Obs.Rounds}
			if res.Obs.Err != nil {
				line.Error = res.Obs.Err.Error()
			}
			if err := trace.Encode(line); err != nil {
				fmt.Fprintf(stderr, "trace: %v\n", err)
			}
		}
		verdict := "pass"
		if !res.Passed() {
			verdict = "FAIL"
		}
		fmt.Fprintf(stderr, "[%d/%d] %-22s %s %s\n", i+1, len(cases), c.ID, verdict, seconds(res.Obs.Latency))
	}

	host := o.baseURL
	if u, err := url.Parse(o.baseURL); err == nil && u.Host != "" {
		host = u.Host
	}
	var report bytes.Buffer
	if err := writeReport(&report, reportMeta{
		Label: o.label, Model: o.model, Host: host, Started: started,
		MaxTokens: o.maxTokens, Timeout: o.timeout, Total: o.total,
		LatencyBudget: o.latencyBudget, MaxToolRounds: r.client.MaxToolRounds(),
		Wiki: o.wiki, WikiCasesSkipped: wikiSkipped,
	}, results); err != nil {
		fmt.Fprintf(stdout, "error: render report: %v\n", err)
		return 1
	}
	_, _ = stdout.Write(report.Bytes())
	if o.out != "" {
		if err := os.WriteFile(o.out, report.Bytes(), 0o644); err != nil {
			fmt.Fprintf(stdout, "error: write %s: %v\n", o.out, err)
			return 1
		}
	}

	var failed []string
	for _, res := range results {
		if !res.Passed() {
			failed = append(failed, regexp.QuoteMeta(res.Case.ID))
		}
	}
	fmt.Fprintf(stderr, "%d of %d cases passed\n", len(results)-len(failed), len(results))
	if len(failed) > 0 {
		fmt.Fprintf(stderr, "rerun the failures: go run ./cmd/evalllm -only '^(%s)$'\n", strings.Join(failed, "|"))
	}
	return 0
}
