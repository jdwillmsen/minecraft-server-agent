package adapters

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestBridgeClient_RunCommand_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/command" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		var req commandRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Command != "say hello" {
			t.Errorf("command = %q, want %q", req.Command, "say hello")
		}
		json.NewEncoder(w).Encode(commandResponse{Rule: "say", Output: ""})
	}))
	defer srv.Close()

	c := NewBridgeClient(srv.URL, "test-token", time.Second)
	resp, err := c.runCommand(context.Background(), "say hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Rule != "say" {
		t.Errorf("Rule = %q, want say", resp.Rule)
	}
}

func TestBridgeClient_RunCommand_Refused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "command not allowed: no rule matched", http.StatusForbidden)
	}))
	defer srv.Close()

	c := NewBridgeClient(srv.URL, "tok", time.Second)
	_, err := c.runCommand(context.Background(), "op Steve")
	if err == nil {
		t.Fatal("expected an error for a refused command")
	}
}

func TestBridgeClient_RunCommand_DeliveryFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "console not connected", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := NewBridgeClient(srv.URL, "tok", time.Second)
	_, err := c.runCommand(context.Background(), "list")
	if err == nil {
		t.Fatal("expected an error when the bridge cannot deliver the command")
	}
}

func TestBridgeClient_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		json.NewEncoder(w).Encode(commandResponse{})
	}))
	defer srv.Close()

	c := NewBridgeClient(srv.URL, "tok", 5*time.Millisecond)
	_, err := c.runCommand(context.Background(), "list")
	if err == nil {
		t.Fatal("expected a timeout error")
	}
}

func TestBridgeClient_MalformedResponseBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := NewBridgeClient(srv.URL, "tok", time.Second)
	_, err := c.runCommand(context.Background(), "list")
	if err == nil {
		t.Fatal("expected an error for a malformed response body")
	}
}

func TestBridgeClient_GetPermissions_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/permissions" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]string{"111": "operator", "222": "visitor"})
	}))
	defer srv.Close()

	c := NewBridgeClient(srv.URL, "tok", time.Second)
	perms, err := c.getPermissions(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if perms["111"] != "operator" || perms["222"] != "visitor" {
		t.Errorf("perms = %+v, want 111=operator, 222=visitor", perms)
	}
}

func TestBridgeClient_TrimsTrailingSlashFromBaseURL(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		json.NewEncoder(w).Encode(commandResponse{})
	}))
	defer srv.Close()

	c := NewBridgeClient(srv.URL+"/", "tok", time.Second)
	if _, err := c.runCommand(context.Background(), "list"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotPath != "/command" {
		t.Errorf("path = %q, want /command (no double slash)", gotPath)
	}
}
