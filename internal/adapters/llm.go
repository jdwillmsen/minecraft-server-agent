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
)

// Bedrock renders long chat lines badly and the server-side line limit is
// tight, so a reply is one short sentence. The question is bounded too: it
// comes straight from player-typed chat and is forwarded to a model.
const (
	MaxReplyChars    = 200
	MaxQuestionChars = 256
)

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
}

// NewLLMClient builds a client. An empty baseURL disables answering: the
// caller checks Enabled rather than discovering it through a failed call.
func NewLLMClient(baseURL, model, apiKey string, maxTokens int, timeout time.Duration) *LLMClient {
	return &LLMClient{
		baseURL:   strings.TrimRight(baseURL, "/"),
		model:     model,
		apiKey:    apiKey,
		maxTokens: maxTokens,
		timeout:   timeout,
		http:      &http.Client{},
	}
}

// Enabled reports whether a backend is configured at all.
func (c *LLMClient) Enabled() bool { return c.baseURL != "" }

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	Messages  []chatMessage `json:"messages"`
}

// BuildRequest is the pure request shape, separated from the call so the
// truncation and auth-header rules are testable without a network.
func (c *LLMClient) BuildRequest(asker, question string) (url string, headers map[string]string, body chatRequest) {
	headers = map[string]string{"content-type": "application/json"}
	// Omitted rather than sent empty: the cluster's vLLM takes no auth, and
	// some OpenAI-compatible backends reject a present-but-empty header.
	if c.apiKey != "" {
		headers["authorization"] = "Bearer " + c.apiKey
	}

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

// Answer asks the model and returns its reply, or "" when there is nothing
// worth saying.
//
// An empty reply is returned as empty rather than as an error, and callers
// must not broadcast it: minecraft-afk-bot shipped a bug where an empty
// completion was broadcast as a blank chat line, which reads to players as the
// server glitching.
func (c *LLMClient) Answer(ctx context.Context, asker, question string) (string, error) {
	if !c.Enabled() {
		return "", nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	url, headers, body := c.BuildRequest(asker, question)
	encoded, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("llm: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		return "", fmt.Errorf("llm: build request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm: post: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: backend returned HTTP %d", resp.StatusCode)
	}
	// Bounded: a backend answering with something enormous must not become
	// this process's memory problem.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("llm: read response: %w", err)
	}
	return ExtractText(payload), nil
}
