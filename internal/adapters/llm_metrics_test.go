package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics"
	"github.com/jdwillmsen/minecraft-server-agent/internal/metrics/metricstest"
	"github.com/jdwillmsen/minecraft-server-agent/internal/tools"
)

const toolCallsMetric = "mc_agent_tool_calls_total"

// One round asking for a tool that works, one that fails, and one the model
// made up -- the three things a tool call can be.
const threeToolCalls = `{"choices":[{"message":{"content":null,"tool_calls":[` +
	`{"id":"c1","type":"function","function":{"name":"metrics_ok_tool","arguments":"{}"}},` +
	`{"id":"c2","type":"function","function":{"name":"metrics_broken_tool","arguments":"{}"}},` +
	`{"id":"c3","type":"function","function":{"name":"metrics_invented_tool","arguments":"{}"}}` +
	`]},"finish_reason":"tool_calls"}]}`

func TestEveryToolCallIsCountedUnderABoundedName(t *testing.T) {
	srv, _ := toolBackend(t, []string{threeToolCalls, textReply})
	client := NewLLMClient(srv.URL, "m", "", 192, 5*time.Second, nil)
	registry := tools.NewRegistry(
		tools.Tool{
			Name:   "metrics_ok_tool",
			Invoke: func(context.Context, json.RawMessage, string) (string, error) { return "fine", nil },
		},
		tools.Tool{
			Name: "metrics_broken_tool",
			Invoke: func(context.Context, json.RawMessage, string) (string, error) {
				return "", errors.New("bridge unreachable")
			},
		},
	)

	var broken, invented float64
	ok := metricstest.Delta(t, func() {
		broken = metricstest.Delta(t, func() {
			invented = metricstest.Delta(t, func() {
				if _, err := client.AnswerWithTools(context.Background(), "Dot", "x", "q", registry); err != nil {
					t.Fatalf("AnswerWithTools: %v", err)
				}
			}, toolCallsMetric, "tool", metrics.Unregistered, "outcome", "error")
		}, toolCallsMetric, "tool", "metrics_broken_tool", "outcome", "error")
	}, toolCallsMetric, "tool", "metrics_ok_tool", "outcome", "ok")

	if ok != 1 {
		t.Errorf("ok tool moved by %v, want 1", ok)
	}
	if broken != 1 {
		t.Errorf("failing tool moved by %v, want 1", broken)
	}
	if invented != 1 {
		t.Errorf("unregistered/error moved by %v, want 1 for the invented name", invented)
	}
	// The model chose that name; letting it through would let every answer
	// mint a new series.
	if metricstest.Exists(t, toolCallsMetric, "tool", "metrics_invented_tool", "outcome", "error") {
		t.Error("a tool name the model invented became a label")
	}
}
