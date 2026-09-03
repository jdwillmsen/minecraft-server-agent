package adapters

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeNames is a NameResolver over a fixed, in-test map.
type fakeNames map[string]string

func (f fakeNames) NameFor(xuid string) (string, bool) {
	name, ok := f[xuid]
	return name, ok
}

func TestBridgeVoice_Tell_Success(t *testing.T) {
	var gotCommand string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req commandRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotCommand = req.Command
		json.NewEncoder(w).Encode(commandResponse{Rule: "tellraw"})
	}))
	defer srv.Close()

	v := NewBridgeVoice(NewBridgeClient(srv.URL, "tok", time.Second), fakeNames{"111": "Steve"})
	if err := v.Tell(context.Background(), "111", "hello there"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.HasPrefix(gotCommand, `tellraw @a[name="Steve"] `) {
		t.Errorf("command = %q, want a tellraw targeted at Steve", gotCommand)
	}
	payloadJSON := strings.TrimPrefix(gotCommand, `tellraw @a[name="Steve"] `)
	var payload tellrawPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("payload is not valid JSON: %v (%s)", err, payloadJSON)
	}
	if len(payload.RawText) != 1 || payload.RawText[0].Text != "hello there" {
		t.Errorf("payload = %+v, want one run with text %q", payload, "hello there")
	}
}

func TestBridgeVoice_Tell_UnknownXUIDFailsWithoutCallingBridge(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		json.NewEncoder(w).Encode(commandResponse{})
	}))
	defer srv.Close()

	v := NewBridgeVoice(NewBridgeClient(srv.URL, "tok", time.Second), fakeNames{})
	err := v.Tell(context.Background(), "unknown-xuid", "hi")
	if err == nil {
		t.Fatal("expected an error for an XUID not in the roster")
	}
	if called {
		t.Error("bridge was called despite no known name to target — should fail before any HTTP call")
	}
}

func TestBridgeVoice_Tell_MessageWithSpecialCharactersStaysValidJSON(t *testing.T) {
	var gotCommand string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req commandRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotCommand = req.Command
		json.NewEncoder(w).Encode(commandResponse{})
	}))
	defer srv.Close()

	v := NewBridgeVoice(NewBridgeClient(srv.URL, "tok", time.Second), fakeNames{"111": "Steve"})
	message := `she said "hi" and left <a note>` + "\nline two"
	if err := v.Tell(context.Background(), "111", message); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.ContainsAny(gotCommand, "\n\r") {
		t.Error("command contains a raw newline/CR — the bridge's allowlist refuses this outright")
	}
	payloadJSON := strings.TrimPrefix(gotCommand, `tellraw @a[name="Steve"] `)
	var payload tellrawPayload
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		t.Fatalf("payload is not valid JSON: %v (%s)", err, payloadJSON)
	}
	if payload.RawText[0].Text != message {
		t.Errorf("round-tripped text = %q, want %q", payload.RawText[0].Text, message)
	}
}

func TestBridgeVoice_Tell_NameWithQuoteRefused(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	v := NewBridgeVoice(NewBridgeClient(srv.URL, "tok", time.Second), fakeNames{"111": `Ste"ve`})
	if err := v.Tell(context.Background(), "111", "hi"); err == nil {
		t.Fatal("expected an error for a gamertag containing a double quote")
	}
	if called {
		t.Error("bridge was called with an unvalidated quoted name")
	}
}

func TestBridgeVoice_Tell_BridgeRefusalPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "command not allowed", http.StatusForbidden)
	}))
	defer srv.Close()

	v := NewBridgeVoice(NewBridgeClient(srv.URL, "tok", time.Second), fakeNames{"111": "Steve"})
	if err := v.Tell(context.Background(), "111", "hi"); err == nil {
		t.Fatal("expected the bridge's refusal to surface as an error")
	}
}

func TestBridgeVoice_Say_Success(t *testing.T) {
	var gotCommand string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req commandRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotCommand = req.Command
		json.NewEncoder(w).Encode(commandResponse{Rule: "say"})
	}))
	defer srv.Close()

	v := NewBridgeVoice(NewBridgeClient(srv.URL, "tok", time.Second), fakeNames{})
	if err := v.Say(context.Background(), "server restarting soon"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCommand != "say server restarting soon" {
		t.Errorf("command = %q, want %q", gotCommand, "say server restarting soon")
	}
}

func TestBridgeVoice_Say_DeliveryFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "console not connected", http.StatusBadGateway)
	}))
	defer srv.Close()

	v := NewBridgeVoice(NewBridgeClient(srv.URL, "tok", time.Second), fakeNames{})
	if err := v.Say(context.Background(), "hi"); err == nil {
		t.Fatal("expected the delivery failure to surface as an error")
	}
}

func TestBridgeVoice_Tell_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		json.NewEncoder(w).Encode(commandResponse{})
	}))
	defer srv.Close()

	v := NewBridgeVoice(NewBridgeClient(srv.URL, "tok", 5*time.Millisecond), fakeNames{"111": "Steve"})
	if err := v.Tell(context.Background(), "111", "hi"); err == nil {
		t.Fatal("expected a timeout error")
	}
}
