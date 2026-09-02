package logging

import (
	"encoding/json"
	"io"
	"os"
	"testing"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything written to it.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	defer func() { os.Stdout = orig }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read pipe: %v", err)
	}
	return string(out)
}

func TestNew_LevelNameIsCaseInsensitive(t *testing.T) {
	for _, name := range []string{"debug", "DEBUG", "Debug", " debug "} {
		out := captureStdout(t, func() { New(name).Debug("probe", nil) })
		if out == "" {
			t.Errorf("New(%q) suppressed a debug line; levelName should be case-insensitive", name)
		}
	}
}

func TestNew_NonDebugLevelSuppressesDebugLines(t *testing.T) {
	for _, name := range []string{"info", "INFO", "", "verbose"} {
		out := captureStdout(t, func() { New(name).Debug("probe", nil) })
		if out != "" {
			t.Errorf("New(%q) emitted a debug line: %q", name, out)
		}
	}
}

func TestInfo_EmitsOneJSONLineWithFields(t *testing.T) {
	out := captureStdout(t, func() {
		New("info").Info("joined", Fields{"address": "example:19132"})
	})

	var line map[string]any
	if err := json.Unmarshal([]byte(out), &line); err != nil {
		t.Fatalf("log line is not JSON (%q): %v", out, err)
	}
	if line["level"] != "info" {
		t.Errorf("level = %v, want info", line["level"])
	}
	if line["event"] != "joined" {
		t.Errorf("event = %v, want joined", line["event"])
	}
	if line["address"] != "example:19132" {
		t.Errorf("address = %v, want example:19132", line["address"])
	}
	if _, ok := line["timestamp"]; !ok {
		t.Error("line has no timestamp")
	}
}

func TestWrite_ReservedKeysAreNotOverriddenByFields(t *testing.T) {
	out := captureStdout(t, func() {
		New("info").Info("real_event", Fields{"event": "spoofed", "level": "error"})
	})

	var line map[string]any
	if err := json.Unmarshal([]byte(out), &line); err != nil {
		t.Fatalf("log line is not JSON (%q): %v", out, err)
	}
	if line["event"] != "real_event" {
		t.Errorf("event = %v, want real_event", line["event"])
	}
	if line["level"] != "info" {
		t.Errorf("level = %v, want info", line["level"])
	}
}
