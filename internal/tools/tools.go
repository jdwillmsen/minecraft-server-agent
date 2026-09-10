// Package tools is the read-only capability surface the LLM may call.
//
// Every tool here answers a question; none of them change anything. That is
// the whole security model of the answer path: the input is untrusted
// player-typed chat, so a prompt-injection attempt to rewrite the rules or
// move someone's waypoint has nothing to call. Writes live in ! commands,
// which carry a real actor and a permission check.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/text"
)

// MaxToolResultChars bounds what one tool feeds back into the model's
// context. The reply itself is capped at 200 characters, so a tool result
// larger than this buys nothing and costs prompt tokens on a small local
// model with a finite window.
const MaxToolResultChars = 400

// Tool is one capability the model may call.
type Tool struct {
	Name        string
	Description string
	// Schema is the JSON Schema for the arguments object, as the backend
	// expects it under function.parameters.
	Schema json.RawMessage
	// Invoke runs the tool. caller is the asking player's XUID, injected by
	// the loop and never supplied by the model -- which is what stops a tool
	// from being asked for another player's data.
	Invoke func(ctx context.Context, args json.RawMessage, caller string) (string, error)
}

// FunctionDefinition and Definition are the OpenAI-compatible wire shapes.
type FunctionDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type Definition struct {
	Type     string             `json:"type"`
	Function FunctionDefinition `json:"function"`
}

// ErrUnknownTool is returned when the model names a tool that is not
// registered -- which happens: models invent plausible names.
var ErrUnknownTool = errors.New("unknown tool")

// Registry holds the tools offered for one answer.
type Registry struct {
	order  []string
	byName map[string]Tool
}

// NewRegistry builds a registry, silently dropping tools that could not be
// called anyway (no name, no Invoke, or a name an earlier tool already
// claimed). A malformed tool is a wiring bug, and offering the model a name
// that panics on call -- or that resolves to whichever duplicate happens to
// win -- is worse than not offering it.
func NewRegistry(list ...Tool) *Registry {
	r := &Registry{byName: make(map[string]Tool, len(list))}
	for _, t := range list {
		name := strings.TrimSpace(t.Name)
		if name == "" || t.Invoke == nil {
			continue
		}
		if _, exists := r.byName[name]; exists {
			continue
		}
		t.Name = name
		r.byName[name] = t
		r.order = append(r.order, name)
	}
	return r
}

func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.order)
}

// Definitions returns what is sent to the model, in registration order.
func (r *Registry) Definitions() []Definition {
	if r == nil {
		return nil
	}
	out := make([]Definition, 0, len(r.order))
	for _, name := range r.order {
		t := r.byName[name]
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, Definition{
			Type: "function",
			Function: FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  schema,
			},
		})
	}
	return out
}

// Has reports whether name is a registered tool, matched the way Invoke
// matches it.
func (r *Registry) Has(name string) bool {
	if r == nil {
		return false
	}
	_, ok := r.byName[strings.TrimSpace(name)]
	return ok
}

// Invoke runs one tool call and returns its result, truncated.
func (r *Registry) Invoke(ctx context.Context, name string, args json.RawMessage, caller string) (string, error) {
	if r == nil {
		return "", fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	t, ok := r.byName[strings.TrimSpace(name)]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownTool, name)
	}
	out, err := t.Invoke(ctx, args, caller)
	if err != nil {
		return "", err
	}
	return text.Truncate(strings.Join(strings.Fields(out), " "), MaxToolResultChars), nil
}
