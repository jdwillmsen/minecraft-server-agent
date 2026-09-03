package adapters

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBridgeFacts_PlayersOnline_Success(t *testing.T) {
	var gotCommand string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req commandRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotCommand = req.Command
		json.NewEncoder(w).Encode(commandResponse{Rule: "list", Output: "There are 2/20 players online: Steve, Alex"})
	}))
	defer srv.Close()

	f := NewBridgeFacts(NewBridgeClient(srv.URL, "tok", time.Second))
	out, err := f.PlayersOnline(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotCommand != "list" {
		t.Errorf("command = %q, want list", gotCommand)
	}
	if out != "There are 2/20 players online: Steve, Alex" {
		t.Errorf("output = %q, want the bridge's raw output relayed unchanged", out)
	}
}

// TestBridgeFacts_PlayersOnline_BlankOutputIsAnError covers the bridge
// answering 200 with nothing captured: its collect window can close before
// a busy console has echoed anything. Relaying that as a successful answer
// would whisper an empty line at the player and record no failure.
func TestBridgeFacts_PlayersOnline_BlankOutputIsAnError(t *testing.T) {
	for _, output := range []string{"", "  \n"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(commandResponse{Rule: "list", Output: output})
		}))

		f := NewBridgeFacts(NewBridgeClient(srv.URL, "tok", time.Second))
		got, err := f.PlayersOnline(context.Background())
		srv.Close()

		if err == nil {
			t.Errorf("PlayersOnline with output %q = (%q, nil), want an error", output, got)
		}
	}
}

func TestBridgeFacts_PlayersOnline_MultiLineOutputRelayedUnchanged(t *testing.T) {
	raw := "There are 2/20 players online:\nSteve\nAlex\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(commandResponse{Rule: "list", Output: raw})
	}))
	defer srv.Close()

	f := NewBridgeFacts(NewBridgeClient(srv.URL, "tok", time.Second))
	out, err := f.PlayersOnline(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != raw {
		t.Errorf("output = %q, want the console's exact text %q", out, raw)
	}
}

func TestBridgeFacts_PlayersOnline_DeliveryFailurePropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "console not connected", http.StatusBadGateway)
	}))
	defer srv.Close()

	f := NewBridgeFacts(NewBridgeClient(srv.URL, "tok", time.Second))
	if _, err := f.PlayersOnline(context.Background()); err == nil {
		t.Fatal("expected an error when the bridge cannot run list")
	}
}
