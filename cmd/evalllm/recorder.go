package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// maxExchangeBytes matches the production client's own response cap, so the
// recorder never accepts a payload the agent would have refused.
const maxExchangeBytes = 1 << 20

// Recorder sits between the production client and the real endpoint and
// keeps what each exchange said.
//
// AnswerWithTools returns only the final text, which is all production
// needs and all it should expose. Scoring needs more: which tools the model
// asked for, what it wrote before the agent cut it to the chat limit, and
// whether it ran out of tokens. Reading that off the wire leaves the client
// untouched, so what is measured is exactly what ships.
type Recorder struct {
	upstream string
	client   *http.Client

	mu sync.Mutex
	// gen increments on every Reset. A request that outlives its case --
	// the client gave up on it at the per-call timeout, but the upstream
	// has not yet noticed -- carries the old generation and is dropped
	// rather than landing in the next case's record.
	gen    int
	rounds []Round
}

// Round is one chat-completions exchange.
type Round struct {
	// Status is the upstream HTTP status, or 502 when it could not be
	// reached at all; Err says why in that case.
	Status int
	Err    string
	// ToolsOffered is how many tools the request carried. Zero on the final
	// pass, when the client withholds them to force a text answer.
	ToolsOffered     int
	ToolCalls        []ToolCall
	Content          string
	FinishReason     string
	CompletionTokens int
}

// ToolCall is one tool the model asked for, with its arguments as sent.
type ToolCall struct {
	Name      string
	Arguments string
}

// NewRecorder forwards to upstream, an OpenAI-compatible base URL such as
// http://host:8000/v1.
func NewRecorder(upstream string) *Recorder {
	return &Recorder{upstream: strings.TrimRight(upstream, "/"), client: &http.Client{}}
}

// Start serves the recorder on a loopback port and returns its base URL.
//
// Loopback only: the recorder forwards the API key upstream, and listening
// any wider would lend that key to whoever else can reach this machine.
func (r *Recorder) Start() (baseURL string, stop func(), err error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	srv := &http.Server{Handler: r, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	return "http://" + ln.Addr().String(), func() { _ = srv.Close() }, nil
}

// Reset starts a new case's record.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gen++
	r.rounds = nil
}

// Rounds returns the current case's exchanges, oldest first.
func (r *Recorder) Rounds() []Round {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Round(nil), r.rounds...)
}

func (r *Recorder) record(gen int, round Round) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if gen == r.gen {
		r.rounds = append(r.rounds, round)
	}
}

func (r *Recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	gen := r.gen
	r.mu.Unlock()

	body, err := io.ReadAll(io.LimitReader(req.Body, maxExchangeBytes))
	if err != nil {
		http.Error(w, "recorder: read request", http.StatusBadRequest)
		return
	}
	// The incoming context carries the client's per-call timeout, so a call
	// the client abandons is abandoned upstream too.
	out, err := http.NewRequestWithContext(req.Context(), req.Method, r.upstream+req.URL.Path, bytes.NewReader(body))
	if err != nil {
		http.Error(w, "recorder: build request", http.StatusBadGateway)
		return
	}
	for _, h := range []string{"Content-Type", "Authorization"} {
		if v := req.Header.Get(h); v != "" {
			out.Header.Set(h, v)
		}
	}

	resp, err := r.client.Do(out)
	if err != nil {
		r.record(gen, Round{Status: http.StatusBadGateway, Err: err.Error(), ToolsOffered: toolsOffered(body)})
		http.Error(w, "recorder: upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxExchangeBytes))
	if err != nil {
		r.record(gen, Round{Status: http.StatusBadGateway, Err: err.Error(), ToolsOffered: toolsOffered(body)})
		http.Error(w, "recorder: read upstream", http.StatusBadGateway)
		return
	}

	r.record(gen, parseRound(resp.StatusCode, body, payload))
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(payload)
}

func toolsOffered(request []byte) int {
	var req struct {
		Tools []json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(request, &req)
	return len(req.Tools)
}

// parseRound reads what the scorer needs from one exchange. A payload it
// cannot parse is recorded with its status and nothing else; the client
// treats the same payload as an empty answer, and the score says so.
func parseRound(status int, request, payload []byte) Round {
	round := Round{Status: status, ToolsOffered: toolsOffered(request)}
	if status != http.StatusOK {
		round.Err = strings.TrimSpace(string(payload))
		return round
	}
	var resp struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &resp); err != nil || len(resp.Choices) == 0 {
		return round
	}
	choice := resp.Choices[0]
	round.Content = choice.Message.Content
	round.FinishReason = choice.FinishReason
	round.CompletionTokens = resp.Usage.CompletionTokens
	for _, call := range choice.Message.ToolCalls {
		round.ToolCalls = append(round.ToolCalls, ToolCall{Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	return round
}
