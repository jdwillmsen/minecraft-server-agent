// Package adapters holds the implementations of the plugin package's
// capability interfaces (Voice, Facts, ...). Stage 1 ships only a logging
// stand-in for Voice; the real mc-console-bridge-backed implementation
// lands in Stage 2.
package adapters

import (
	"context"

	"github.com/jdwillmsen/minecraft-server-agent/internal/logging"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// NoopVoice logs what it would have said instead of actually reaching the
// console bridge. Used until Stage 2 wires a real bridge client, so Stage 1
// can exercise the full command dispatch path end-to-end.
type NoopVoice struct {
	log *logging.Logger
}

// NewNoopVoice builds a NoopVoice that logs through log.
func NewNoopVoice(log *logging.Logger) NoopVoice {
	return NoopVoice{log: log}
}

var _ plugin.Voice = NoopVoice{}

func (v NoopVoice) Tell(ctx context.Context, xuid, message string) error {
	v.log.Info("voice_tell_noop", logging.Fields{"xuid": xuid, "message": message})
	return nil
}

func (v NoopVoice) Say(ctx context.Context, message string) error {
	v.log.Info("voice_say_noop", logging.Fields{"message": message})
	return nil
}
