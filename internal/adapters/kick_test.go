package adapters

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestKickSendsTheBridgesKickCommand(t *testing.T) {
	cases := map[string]string{
		"JdwAfk1":     "kick JdwAfk1",
		"Jdw Afk Two": `kick "Jdw Afk Two"`,
	}
	for gamertag, want := range cases {
		var got string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req commandRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			got = req.Command
			_ = json.NewEncoder(w).Encode(commandResponse{Rule: "kick", Output: "Kicked " + gamertag})
		}))
		err := NewBridgeClient(srv.URL, "tok", time.Second).Kick(t.Context(), gamertag)
		srv.Close()
		if err != nil {
			t.Fatalf("Kick(%q): %v", gamertag, err)
		}
		if got != want {
			t.Errorf("Kick(%q) sent %q, want %q", gamertag, got, want)
		}
	}
}

// The bridge refuses a kick for anyone not on its actor list; that refusal
// has to reach the loop as an error, not vanish.
func TestKickReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "kick is limited to actors", http.StatusForbidden)
	}))
	defer srv.Close()
	if err := NewBridgeClient(srv.URL, "tok", time.Second).Kick(t.Context(), "Steve"); err == nil {
		t.Error("a refused kick returned nil")
	}
}
