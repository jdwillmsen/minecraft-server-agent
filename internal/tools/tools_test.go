package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func testTool(name string, out string, err error) Tool {
	return Tool{
		Name:        name,
		Description: "test tool",
		Schema:      json.RawMessage(`{"type":"object","properties":{}}`),
		Invoke: func(context.Context, json.RawMessage, string) (string, error) {
			return out, err
		},
	}
}

func TestDefinitionsAreWireShaped(t *testing.T) {
	r := NewRegistry(testTool("players_online", "two players", nil))
	defs := r.Definitions()
	if len(defs) != 1 {
		t.Fatalf("Definitions len = %d, want 1", len(defs))
	}
	encoded, err := json.Marshal(defs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, want := range []string{`"type":"function"`, `"name":"players_online"`, `"parameters"`} {
		if !strings.Contains(string(encoded), want) {
			t.Errorf("definition JSON %s missing %s", encoded, want)
		}
	}
}

func TestInvokeUnknownTool(t *testing.T) {
	r := NewRegistry(testTool("known", "ok", nil))
	if _, err := r.Invoke(context.Background(), "nope", nil, "xuid"); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("Invoke unknown err = %v, want ErrUnknownTool", err)
	}
}

func TestInvokePassesCallerAndTruncates(t *testing.T) {
	var gotCaller string
	r := NewRegistry(Tool{
		Name:   "echo_caller",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`),
		Invoke: func(_ context.Context, _ json.RawMessage, caller string) (string, error) {
			gotCaller = caller
			return strings.Repeat("x", MaxToolResultChars+50), nil
		},
	})
	out, err := r.Invoke(context.Background(), "echo_caller", nil, "2535417035391439")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if gotCaller != "2535417035391439" {
		t.Errorf("caller = %q, want the XUID the loop injected", gotCaller)
	}
	if len(out) != MaxToolResultChars {
		t.Errorf("result len = %d, want it truncated to %d", len(out), MaxToolResultChars)
	}
}

func TestInvokeTruncatesOnRuneBoundary(t *testing.T) {
	// Place a two-byte rune ("é") straddling the cap so a byte-index slice
	// lands on its second byte: prefix fills bytes 0-398, the rune's first
	// byte sits at 399, its second byte at 400 -- exactly where the old
	// out[:400] slice cut.
	prefix := strings.Repeat("x", MaxToolResultChars-1)
	out := prefix + "é" + strings.Repeat("y", 50)
	r := NewRegistry(testTool("multibyte", out, nil))
	got, err := r.Invoke(context.Background(), "multibyte", nil, "xuid")
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("result is not valid UTF-8: %q", got)
	}
	if len(got) > MaxToolResultChars {
		t.Errorf("result len = %d, want <= %d", len(got), MaxToolResultChars)
	}
}

func TestRegistrySkipsMalformedTools(t *testing.T) {
	r := NewRegistry(
		testTool("", "unnamed", nil),
		Tool{Name: "no_invoke", Schema: json.RawMessage(`{}`)},
		testTool("good", "ok", nil),
	)
	if r.Len() != 1 {
		t.Fatalf("Len = %d, want 1 -- unnamed and nil-Invoke tools must be dropped", r.Len())
	}
}
