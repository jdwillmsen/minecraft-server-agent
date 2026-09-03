package logging

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"
)

// captureOutput runs fn against a Logger writing to an in-memory buffer -
// no pipe, no global os.Stdout swap, no fd to leak - and returns what was
// written to stdout.
func captureOutput(fn func(*Logger)) string {
	var stdout, stderr bytes.Buffer
	fn(newWithWriters("info", &stdout, &stderr))
	return stdout.String()
}

func captureOutputAtLevel(levelName string, fn func(*Logger)) string {
	var stdout, stderr bytes.Buffer
	fn(newWithWriters(levelName, &stdout, &stderr))
	return stdout.String()
}

func TestNew_LevelNameIsCaseInsensitive(t *testing.T) {
	for _, name := range []string{"debug", "DEBUG", "Debug", " debug "} {
		out := captureOutputAtLevel(name, func(l *Logger) { l.Debug("probe", nil) })
		if out == "" {
			t.Errorf("New(%q) suppressed a debug line; levelName should be case-insensitive", name)
		}
	}
}

func TestNew_NonDebugLevelSuppressesDebugLines(t *testing.T) {
	for _, name := range []string{"info", "INFO", "", "verbose"} {
		out := captureOutputAtLevel(name, func(l *Logger) { l.Debug("probe", nil) })
		if out != "" {
			t.Errorf("New(%q) emitted a debug line: %q", name, out)
		}
	}
}

func TestInfo_EmitsOneJSONLineWithFields(t *testing.T) {
	out := captureOutput(func(l *Logger) {
		l.Info("joined", Fields{"address": "example:19132"})
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
	out := captureOutput(func(l *Logger) {
		l.Info("real_event", Fields{"event": "spoofed", "level": "error"})
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

// TestLogger_ConcurrentWritesProduceIntactLines proves the mutex actually
// prevents interleaved output: without it, concurrent fmt.Fprintln calls to
// the same writer can interleave mid-line and corrupt every line involved.
func TestLogger_ConcurrentWritesProduceIntactLines(t *testing.T) {
	var stdout bytes.Buffer
	var mu sync.Mutex // guards the shared bytes.Buffer, which isn't itself concurrency-safe
	l := newWithWriters("info", syncedWriter{&stdout, &mu}, syncedWriter{&stdout, &mu})

	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.Info("concurrent", Fields{"i": i})
		}(i)
	}
	wg.Wait()

	mu.Lock()
	lines := bytes.Split(bytes.TrimRight(stdout.Bytes(), "\n"), []byte("\n"))
	mu.Unlock()

	if len(lines) != n {
		t.Fatalf("got %d lines, want %d - a corrupted/merged line means writes interleaved", len(lines), n)
	}
	for _, line := range lines {
		var decoded map[string]any
		if err := json.Unmarshal(line, &decoded); err != nil {
			t.Fatalf("line %q is not valid JSON: %v", line, err)
		}
	}
}

// syncedWriter adapts a bytes.Buffer (not itself safe for concurrent use)
// into an io.Writer safe for the concurrency test above. The Logger's own
// mutex should make this belt-and-suspenders lock redundant; keeping it
// means a failure here can only be the Logger's fault, not the test's.
type syncedWriter struct {
	buf *bytes.Buffer
	mu  *sync.Mutex
}

func (w syncedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
