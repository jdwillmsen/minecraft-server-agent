package adapters

import (
	"context"
	"fmt"
	"strings"
)

// Kick removes gamertag from the server through the bridge. The bridge takes
// it only for the actors on its own list, so a leaked bridge token cannot be
// turned on a real player.
//
// Quoted only when the name has a space: that is the one case the console
// cannot parse bare, and the bridge accepts exactly these two forms.
func (c *BridgeClient) Kick(ctx context.Context, gamertag string) error {
	cmd := "kick " + gamertag
	if strings.Contains(gamertag, " ") {
		cmd = `kick "` + gamertag + `"`
	}
	if _, err := c.runCommand(ctx, cmd); err != nil {
		return fmt.Errorf("bridge: kick %s: %w", gamertag, err)
	}
	return nil
}
