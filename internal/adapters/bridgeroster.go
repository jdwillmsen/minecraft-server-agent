package adapters

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Event types the roster follows. The bridge also reports content errors and
// crashes, which say nothing about who is online.
const (
	BridgeEventConnect    = "connect"
	BridgeEventDisconnect = "disconnect"
)

// BridgeEvent is one entry of mc-console-bridge's GET /events. IDs are only
// unique within one bridge process: they start again at 1 when it restarts.
type BridgeEvent struct {
	ID     int64     `json:"id"`
	Type   string    `json:"type"`
	Time   time.Time `json:"time"`
	Player string    `json:"player,omitempty"`
	Raw    string    `json:"raw"`
	// Backfill marks a line the bridge replayed from the server's log
	// history when it connected. It is stamped with when it was read, not
	// when it happened, so it says nothing about who just arrived.
	Backfill bool `json:"backfill,omitempty"`
}

var eventXUID = regexp.MustCompile(`xuid: (\d+)`)

// XUID is the account the server logged the line for. The bridge parses only
// the gamertag out of it, and the roster keys on the XUID, so it is read from
// the raw line here.
func (e BridgeEvent) XUID() string {
	m := eventXUID.FindStringSubmatch(e.Raw)
	if m == nil {
		return ""
	}
	return m[1]
}

// Events returns every event the bridge still holds with an ID above since,
// oldest first.
func (c *BridgeClient) Events(ctx context.Context, since int64) ([]BridgeEvent, error) {
	var out []BridgeEvent
	if err := c.do(ctx, http.MethodGet, "/events?since="+strconv.FormatInt(since, 10), nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// OnlinePlayers runs `list` through the bridge and returns the gamertags it
// names.
func (c *BridgeClient) OnlinePlayers(ctx context.Context) ([]string, error) {
	resp, err := c.runCommand(ctx, "list")
	if err != nil {
		return nil, fmt.Errorf("bridge: online players: %w", err)
	}
	names, err := ParseList(resp.Output)
	if err != nil {
		return nil, fmt.Errorf("bridge: online players: %w", err)
	}
	return names, nil
}

var listHeader = regexp.MustCompile(`There are (\d+)/\d+ players online:`)

var (
	errNoListHeader = errors.New("no \"There are N/M players online:\" line in the console output")
	errListCount    = errors.New("the names do not match the count the server gave")
)

// ParseList reads Bedrock's answer to `list`. The server prints the count on
// one line and the names, comma separated, on the same line or the next one.
//
// Strict on purpose. The bridge returns whatever the console printed inside a
// fixed window, so another log line can sit where the names belong. A roster
// seeded from that would be wrong about everyone until the next seed, which is
// worse than one that waits a poll for a clean answer.
func ParseList(output string) ([]string, error) {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	for i, line := range lines {
		loc := listHeader.FindStringSubmatchIndex(line)
		if loc == nil {
			continue
		}
		want, err := strconv.Atoi(line[loc[2]:loc[3]])
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errNoListHeader, err)
		}
		names := []string{}
		if want == 0 {
			return names, nil
		}
		rest := strings.TrimSpace(line[loc[1]:])
		if rest == "" {
			for _, next := range lines[i+1:] {
				if rest = strings.TrimSpace(next); rest != "" {
					break
				}
			}
		}
		for _, n := range strings.Split(rest, ",") {
			n = strings.TrimSpace(n)
			if n == "" {
				continue
			}
			// A log line in the names' place splits into as many "names" as
			// it has commas, which can match the count by accident; its
			// stamp and fields cannot be a gamertag.
			if strings.ContainsAny(n, ":[") {
				return nil, fmt.Errorf("%w: %q is not a gamertag", errListCount, n)
			}
			names = append(names, n)
		}
		if len(names) != want {
			return nil, fmt.Errorf("%w: expected %d, found %d in %q", errListCount, want, len(names), rest)
		}
		return names, nil
	}
	return nil, errNoListHeader
}
