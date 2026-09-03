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
