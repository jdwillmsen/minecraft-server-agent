package adapters

import (
	"context"
	"fmt"

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
func (f *BridgeFacts) PlayersOnline(ctx context.Context) (string, error) {
	resp, err := f.client.runCommand(ctx, "list")
	if err != nil {
		return "", fmt.Errorf("bridge facts: players online: %w", err)
	}
	return resp.Output, nil
}
