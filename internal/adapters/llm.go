package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
	"github.com/jdwillmsen/minecraft-server-agent/pkg/logging"
)

// Bedrock renders long chat lines badly and the server-side line limit is
// tight, so a reply is one short sentence. The question is bounded too: it
// comes straight from player-typed chat and is forwarded to a model.
const (
	MaxReplyChars    = 200
	MaxQuestionChars = 256
)

// MaxToolRounds caps how many times the model may ask for tools before it
// is made to answer. Two covers the questions this serves -- one lookup,
// occasionally two -- and an uncapped loop driven by a chat message is an
// unbounded cost per message.
const MaxToolRounds = 2

// systemPrompt is where most of this feature's safety lives.
//
// The final clause is not stylistic. minecraft-afk-bot shipped this same
// capability and had to fix loop-safety bugs afterwards: a reply ending in a
// question invites an answer, and with two automated speakers in one chat that
// is a conversation nothing terminates. Refusing to end on a question mark
// removes the invitation at the source, which is cheaper and more reliable
// than trying to detect the loop once it has started.
const systemPrompt = "You are the voice of a Minecraft Bedrock server, replying directly in its own chat. " +
	"Answer in one short, plain sentence under 200 characters. " +
	"No markdown, no roleplay asterisks, and never end your reply with a question mark."

// LLMClient calls an OpenAI-compatible chat-completions endpoint.
//
// Deliberately not an SDK. The endpoint is one POST with a fixed body shape,
// and the cluster's own vLLM is the default backend -- a vendored client
// library would be a large dependency and a second opinion about retries and
// timeouts this already has.
type LLMClient struct {
	baseURL   string
	model     string
	apiKey    string
	maxTokens int
	timeout   time.Duration
	http      *http.Client
	// log carries operational events (currently: failed tool invocations).
	// May be nil, which drops them; set at construction and never written
	// again, because AnswerWithTools is called from one goroutine per
	// answer and a setter would be a data race waiting for its first
	// concurrent caller.
	log *logging.Logger
}

// NewLLMClient builds a client. An empty baseURL disables answering: the
// caller checks Enabled rather than discovering it through a failed call. A
// nil log is supported -- the events are dropped rather than the client
// requiring one to function.
func NewLLMClient(baseURL, model, apiKey string, maxTokens int, timeout time.Duration, log *logging.Logger) *LLMClient {
	return &LLMClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		model:     model,
		apiKey:    apiKey,
		maxTokens: maxTokens,
		timeout:   timeout,
		http:      &http.Client{},
		log:       log,
	}
}

// Enabled reports whether a backend is configured at all.
func (c *LLMClient) Enabled() bool { return c.baseURL != "" }

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// ToolCalls is echoed back verbatim in the assistant turn: the backend
	// matches tool results to it by id, and dropping it makes the tool
	// messages orphans.
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	// ToolCallID is set only on role:"tool" messages.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type chatRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []chatMessage      `json:"messages"`
	Tools     []tools.Definition `json:"tools,omitempty"`
}

// headers is the one place the auth rule is decided: BuildRequest and post
// both call it, so the header a request gets never depends on which path
// built it.
func (c *LLMClient) headers() map[string]string {
	headers := map[string]string{"content-type": "application/json"}
	// Omitted rather than sent empty: the cluster's vLLM takes no auth, and
	// some OpenAI-compatible backends reject a present-but-empty header.
	if c.apiKey != "" {
		headers["authorization"] = "Bearer " + c.apiKey
	}
	return headers
}

// BuildRequest is the pure request shape, separated from the call so the
// truncation and auth-header rules are testable without a network.
func (c *LLMClient) BuildRequest(asker, question string) (url string, headers map[string]string, body chatRequest) {
	headers = c.headers()

	if len(question) > MaxQuestionChars {
		question = question[:MaxQuestionChars]
	}
	return c.baseURL + "/chat/completions",
		headers,
		chatRequest{
			Model:     c.model,
			MaxTokens: c.maxTokens,
			Messages: []chatMessage{
				{Role: "system", Content: systemPrompt},
				{Role: "user", Content: asker + " asked: " + question},
			},
		}
}

// ExtractText is the pure response parser. It returns "" for anything that is
// not a normal OpenAI-shaped completion, so a new or misbehaving backend makes
// the agent fall silent rather than error into chat.
func ExtractText(payload []byte) string {
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return ""
	}
	if len(parsed.Choices) == 0 {
		return ""
	}
	// Bedrock chat is single-line; collapse anything the model wrapped.
	text := strings.Join(strings.Fields(parsed.Choices[0].Message.Content), " ")
	if len(text) > MaxReplyChars {
		text = text[:MaxReplyChars]
	}
	return text
}

// ExtractToolCalls returns the tool calls in a completion, or nil when the
// model answered with text. Like ExtractText, it treats anything it cannot
// parse as "nothing", so an unfamiliar backend degrades to a plain answer.
func ExtractToolCalls(payload []byte) []toolCall {
	var parsed struct {
		Choices []struct {
			Message struct {
				ToolCalls []toolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil || len(parsed.Choices) == 0 {
		return nil
	}
	return parsed.Choices[0].Message.ToolCalls
}

// withNonEmptyIDs filters out tool calls with no id, in place.
func withNonEmptyIDs(calls []toolCall) []toolCall {
	out := calls[:0]
	for _, call := range calls {
		if call.ID != "" {
			out = append(out, call)
		}
	}
	return out
}

// post sends one chat-completions request and returns the raw payload. Each
// call carries its own timeout: without a per-call bound, one stalled
// request would consume the whole loop's budget.
func (c *LLMClient) post(ctx context.Context, body chatRequest) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	for k, v := range c.headers() {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("llm: post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("llm: backend returned HTTP %d", resp.StatusCode)
	}
	// Bounded: a backend answering with something enormous must not become
	// this process's memory problem.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("llm: read response: %w", err)
	}
	return payload, nil
}

// AnswerWithTools asks the model, letting it call read-only tools first.
//
// callerXUID is passed to every tool the model invokes and is never taken
// from the model's own arguments: that is what keeps waypoint_lookup from
// being talked into reading someone else's coordinates.
func (c *LLMClient) AnswerWithTools(ctx context.Context, asker, callerXUID, question string, registry *tools.Registry) (string, error) {
	if !c.Enabled() {
		return "", nil
	}

	_, _, initial := c.BuildRequest(asker, question)
	messages := initial.Messages

	for round := 0; ; round++ {
		body := chatRequest{Model: c.model, MaxTokens: c.maxTokens, Messages: messages}
		// Tools are withheld on the final pass, which is what forces text
		// out of a model that would otherwise keep calling tools forever.
		if registry.Len() > 0 && round < MaxToolRounds {
			body.Tools = registry.Definitions()
		}

		payload, err := c.post(ctx, body)
		if err != nil {
			return "", err
		}

		calls := ExtractToolCalls(payload)
		// A call with no id can't be matched back to a role:"tool" reply --
		// the backend keys the pairing on tool_call_id -- so echoing one
		// blank produces an orphaned message the backend rejects outright.
		// Dropping it here costs nothing the model can't recover from on
		// its next turn.
		calls = withNonEmptyIDs(calls)
		if len(calls) == 0 || round >= MaxToolRounds {
			return ExtractText(payload), nil
		}

		messages = append(messages, chatMessage{Role: "assistant", ToolCalls: calls})
		for _, call := range calls {
			result, err := registry.Invoke(ctx, call.Function.Name, json.RawMessage(call.Function.Arguments), callerXUID)
			if err != nil {
				if c.log != nil {
					c.log.Error("tool_invocation_failed", logging.Fields{"tool": call.Function.Name, "error": err.Error()})
				}
				// Handed back to the model rather than aborting: it can
				// answer around a missing fact, and an operator reads the
				// failure in the logs above. Fixed and generic on purpose --
				// unlike a successful result, err.Error() never passes
				// through the registry's cap or collapse, and a bridge or
				// database failure can carry a DSN or an internal address.
				// The model gets only enough to answer around the gap.
				result = "error: that lookup is unavailable right now"
			}
			messages = append(messages, chatMessage{
				Role: "tool", ToolCallID: call.ID, Content: result,
			})
		}
	}
}

// Answer asks the model and returns its reply, or "" when there is nothing
// worth saying.
//
// An empty reply is returned as empty rather than as an error, and callers
// must not broadcast it: minecraft-afk-bot shipped a bug where an empty
// completion was broadcast as a blank chat line, which reads to players as the
// server glitching.
func (c *LLMClient) Answer(ctx context.Context, asker, question string) (string, error) {
	return c.AnswerWithTools(ctx, asker, "", question, nil)
}
