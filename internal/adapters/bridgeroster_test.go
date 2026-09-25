package adapters

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestBridgeClient_EventsAsksForEverythingAfterSince(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/events" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("since"); got != "41" {
			t.Errorf("since = %q, want 41", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		_, _ = w.Write([]byte(`[{"id":42,"type":"connect","time":"2026-09-23T10:00:00Z","player":"Steve","raw":"[2026-09-23 10:00:00:000 INFO] Player connected: Steve, xuid: 2535457893448396"}]`))
	}))
	defer srv.Close()

	got, err := NewBridgeClient(srv.URL, "tok", time.Second).Events(context.Background(), 41)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(got) != 1 || got[0].ID != 42 || got[0].Type != BridgeEventConnect || got[0].Player != "Steve" {
		t.Fatalf("Events = %+v, want the one connect event", got)
	}
	if x := got[0].XUID(); x != "2535457893448396" {
		t.Errorf("XUID = %q, want 2535457893448396", x)
	}
}

func TestBridgeClient_EventsFailureIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	if _, err := NewBridgeClient(srv.URL, "tok", time.Second).Events(context.Background(), 0); err == nil {
		t.Fatal("Events returned no error for a 401")
	}
}

func TestBridgeEventXUIDIsEmptyWhenTheLineCarriesNone(t *testing.T) {
	if x := (BridgeEvent{Raw: "Player connected: Steve"}).XUID(); x != "" {
		t.Errorf("XUID = %q, want empty", x)
	}
}

func TestBridgeClient_OnlinePlayersRunsListAndParsesIt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req commandRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Command != "list" {
			t.Errorf("command = %q, want list", req.Command)
		}
		_ = json.NewEncoder(w).Encode(commandResponse{Rule: "list", Output: "There are 2/20 players online:\nSteve, Alex\n"})
	}))
	defer srv.Close()

	got, err := NewBridgeClient(srv.URL, "tok", time.Second).OnlinePlayers(context.Background())
	if err != nil {
		t.Fatalf("OnlinePlayers: %v", err)
	}
	if want := []string{"Steve", "Alex"}; !reflect.DeepEqual(got, want) {
		t.Errorf("OnlinePlayers = %q, want %q", got, want)
	}
}

func TestParseList(t *testing.T) {
	for _, tc := range []struct {
		name   string
		output string
		want   []string
	}{
		{"names on the next line", "There are 2/20 players online:\nSteve, Alex\n", []string{"Steve", "Alex"}},
		{"names on the same line", "There are 2/20 players online: Steve, Alex", []string{"Steve", "Alex"}},
		{"log-prefixed header and CRLF", "[2026-09-23 10:00:00:000 INFO] There are 1/10 players online:\r\nSteve\r\n", []string{"Steve"}},
		{"gamertag with a space", "There are 1/10 players online:\nBig Steve\n", []string{"Big Steve"}},
		{"empty server", "There are 0/10 players online:\n", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseList(tc.output)
			if err != nil {
				t.Fatalf("ParseList: %v", err)
			}
			if got == nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseList = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// The bridge collects whatever the console prints inside its window, so a
// join landing at the same moment can sit where the names should be. A
// count the header cannot account for is a seed that would be wrong.
func TestParseListRefusesACountItCannotAccountFor(t *testing.T) {
	for _, output := range []string{
		"",
		"Unknown command: list",
		"There are 2/20 players online:\n",
		"There are 2/20 players online:\n[2026-09-23 10:00:00:000 INFO] Player connected: Sam, xuid: 3\n",
		"There are 1/20 players online:\nSteve, Alex\n",
	} {
		if got, err := ParseList(output); err == nil {
			t.Errorf("ParseList(%q) = %q, nil; want an error", output, got)
		}
	}
}

func TestBridgeEvent_DecodesTheBackfillFlag(t *testing.T) {
	var got []BridgeEvent
	body := `[{"id":1,"type":"connect","time":"2026-09-23T10:00:00Z","player":"Steve","raw":"x","backfill":true},{"id":2,"type":"connect","time":"2026-09-23T10:00:00Z","player":"Alex","raw":"y"}]`
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !got[0].Backfill || got[1].Backfill {
		t.Errorf("Backfill = %v, %v; want true for the replayed line and false for one without the flag", got[0].Backfill, got[1].Backfill)
	}
}
