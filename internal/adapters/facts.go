package adapters

import (
	"context"
	"fmt"
	"strings"

	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
)

// BridgeFacts is the mc-console-bridge-backed Facts implementation.
type BridgeFacts struct {
	client *BridgeClient
}

// NewBridgeFacts builds a BridgeFacts on top of client.
func NewBridgeFacts(client *BridgeClient) *BridgeFacts {
	return &BridgeFacts{client: client}
}

var _ plugin.Facts = (*BridgeFacts)(nil)

// PlayersOnline runs `list` through the bridge and returns its raw output
// untouched — see the Facts interface doc for why this isn't parsed into a
// structured name slice.
//
// Blank output is a failure, not a result: the bridge collects console
// output in a fixed time window with no request/response correlation, so a
// busy or stalled console yields an empty string on an otherwise
// successful call. Relaying that would answer the player with a blank
// whisper and record no failure anywhere.
func (f *BridgeFacts) PlayersOnline(ctx context.Context) (string, error) {
	resp, err := f.client.runCommand(ctx, "list")
	if err != nil {
		return "", fmt.Errorf("bridge facts: players online: %w", err)
	}
	if strings.TrimSpace(resp.Output) == "" {
		return "", fmt.Errorf("bridge facts: players online: the bridge captured no console output for `list`")
	}
	return resp.Output, nil
}
