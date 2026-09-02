// Package logging emits one structured JSON line per event to stdout
// (stderr for errors), matching the field shape minecraft-afk-bot already
// established so existing Loki queries keep working: {"level":...,
// "event":...,  ...fields}.
package logging

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Level controls whether Debug lines are emitted.
type Level int

const (
	LevelInfo Level = iota
	LevelDebug
)

// Logger writes structured JSON log lines.
type Logger struct {
	level Level
}

// New builds a Logger. levelName is case-insensitive; anything other than
// "debug" is treated as info.
func New(levelName string) *Logger {
	lvl := LevelInfo
	if levelName == "debug" {
		lvl = LevelDebug
	}
	return &Logger{level: lvl}
}

// Fields is a shorthand for the extra key/value pairs attached to a line.
type Fields map[string]any

func (l *Logger) write(w *os.File, level, event string, fields Fields) {
	line := make(map[string]any, len(fields)+3)
	for k, v := range fields {
		line[k] = v
	}
	line["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	line["level"] = level
	line["event"] = event

	enc, err := json.Marshal(line)
	if err != nil {
		fmt.Fprintf(os.Stderr, "{\"level\":\"error\",\"event\":\"log_marshal_failed\",\"error\":%q}\n", err.Error())
		return
	}
	fmt.Fprintln(w, string(enc))
}

// Info logs an informational event to stdout.
func (l *Logger) Info(event string, fields Fields) {
	l.write(os.Stdout, "info", event, fields)
}

// Debug logs a debug event to stdout, only when the logger's level allows it.
func (l *Logger) Debug(event string, fields Fields) {
	if l.level != LevelDebug {
		return
	}
	l.write(os.Stdout, "debug", event, fields)
}

// Error logs an error event to stderr.
func (l *Logger) Error(event string, fields Fields) {
	l.write(os.Stderr, "error", event, fields)
}
