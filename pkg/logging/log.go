// Package logging emits one structured JSON line per event to stdout
// (stderr for errors), matching the field shape minecraft-afk-bot already
// established so existing Loki queries keep working: {"level":...,
// "event":...,  ...fields}.
package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level controls whether Debug lines are emitted.
type Level int

const (
	LevelInfo Level = iota
	LevelDebug
)

// Logger writes structured JSON log lines. Safe for concurrent use: every
// write is serialised by mu, since chat and connection-lifecycle events can
// log from multiple goroutines (the packet read loop, the HTTP server, the
// signal handler) at once, and interleaved fmt.Fprintln calls to the same
// *os.File are not otherwise guaranteed atomic.
type Logger struct {
	mu     sync.Mutex
	level  Level
	stdout io.Writer
	stderr io.Writer
}

// New builds a Logger that writes to the process's real stdout/stderr.
// levelName is case-insensitive; anything other than "debug" is treated as
// info.
func New(levelName string) *Logger {
	return newWithWriters(levelName, os.Stdout, os.Stderr)
}

// newWithWriters builds a Logger against injected writers, so tests can
// assert on output with a plain buffer instead of swapping the process's
// global os.Stdout through a pipe.
func newWithWriters(levelName string, stdout, stderr io.Writer) *Logger {
	lvl := LevelInfo
	if strings.EqualFold(strings.TrimSpace(levelName), "debug") {
		lvl = LevelDebug
	}
	return &Logger{level: lvl, stdout: stdout, stderr: stderr}
}

// Fields is a shorthand for the extra key/value pairs attached to a line.
type Fields map[string]any

func (l *Logger) write(w io.Writer, level, event string, fields Fields) {
	line := make(map[string]any, len(fields)+3)
	for k, v := range fields {
		line[k] = v
	}
	line["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	line["level"] = level
	line["event"] = event

	enc, err := json.Marshal(line)

	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		fmt.Fprintf(l.stderr, "{\"level\":\"error\",\"event\":\"log_marshal_failed\",\"error\":%q}\n", err.Error())
		return
	}
	fmt.Fprintln(w, string(enc))
}

// Info logs an informational event to stdout.
func (l *Logger) Info(event string, fields Fields) {
	l.write(l.stdout, "info", event, fields)
}

// Debug logs a debug event to stdout, only when the logger's level allows it.
func (l *Logger) Debug(event string, fields Fields) {
	if l.level != LevelDebug {
		return
	}
	l.write(l.stdout, "debug", event, fields)
}

// Warn logs a noteworthy but handled event to stdout.
//
// Stdout rather than stderr on purpose: a warning means the program adapted
// to something unexpected and carried on, so routing it to stderr would put
// it in the same stream as the failures that stop work, and inflate every
// error-rate alert built on that stream. The level was already part of the
// field shape this package inherited from the TypeScript bot, so existing
// Loki queries for it keep working.
func (l *Logger) Warn(event string, fields Fields) {
	l.write(l.stdout, "warn", event, fields)
}

// Error logs an error event to stderr.
func (l *Logger) Error(event string, fields Fields) {
	l.write(l.stderr, "error", event, fields)
}
