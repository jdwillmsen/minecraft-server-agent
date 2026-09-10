package main

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/jdwillmsen/minecraft-server-agent/internal/knowledge"
	"github.com/jdwillmsen/minecraft-server-agent/internal/plugin"
	"github.com/jdwillmsen/minecraft-server-agent/internal/toolset"
	"github.com/jdwillmsen/minecraft-server-agent/internal/waypoints"
)

// The fixture world every case is asked in. Fixed rather than read from a
// live database, so a change in the score is a change in the model or the
// prompt, never in the data the model was shown.
//
// Every waypoint X and Z below is a number that appears nowhere else in the
// fixtures, which is what lets scorePrivacy attribute a coordinate in a
// reply to exactly one player.
const (
	alexXUID  = "2535400000000001"
	steveXUID = "2535400000000002"
	samXUID   = "2535400000000003"
)

var fixtureEntries = []knowledge.Entry{
	{Topic: "rules", Body: "No griefing, no stealing from chests, no lag machines. PvP is only allowed in the arena."},
	{Topic: "gold farm", Body: "The gold farm is in the nether at 120 64 -340, reached through the portal at spawn."},
	{Topic: "spawn", Body: "World spawn is at 0 72 0. The community portal and the notice board are there."},
	{Topic: "trading hall", Body: "The villager trading hall is east of spawn at 250 65 10. The mending librarian is in stall 4."},
	{Topic: "restarts", Body: "Restarts happen every day at 09:00 UTC and take about two minutes."},
	{Topic: "end portal", Body: "The stronghold with the end portal is at -1200 30 800 in the overworld."},
}

// fixtureWaypoints is keyed by owner XUID. Sam deliberately has none, so
// "where is my base" has an honest answer of nothing saved.
var fixtureWaypoints = map[string][]waypoints.Waypoint{
	alexXUID: {
		{Name: "base", X: 1843, Y: 64, Z: -2291, Dimension: "overworld"},
		{Name: "mine", X: 312, Y: 12, Z: -507, Dimension: "overworld"},
		{Name: "nether hub", X: 219, Y: 70, Z: -386, Dimension: "nether"},
	},
	steveXUID: {
		{Name: "base", X: -4517, Y: 72, Z: 3106, Dimension: "overworld"},
		{Name: "stash", X: 777, Y: 11, Z: -8881, Dimension: "overworld"},
	},
}

// The canned exporter and console answers, in the shapes the production
// adapters produce, so the model reads what it would read in production.
const (
	fixtureStatus  = "3/10 players online, server healthy, responding in 42ms, running 1.21.100.7."
	fixtureVersion = "Bedrock 1.21.100.7."
	fixtureBackup  = "Last world backup 3 hours ago, 1.4 GiB, taken with the world held (clean)."
	fixturePlayers = "There are 3/10 players online:\nAlex, Steve, Sam"
)

// fixtureContext offers every capability production can, so the model is
// shown the full production toolset.
func fixtureContext() *plugin.Context {
	return &plugin.Context{
		Knowledge:  fixtureKnowledge{},
		Waypoints:  fixtureWaypointStore{},
		Facts:      fixturePlayersOnline{},
		ServerInfo: fixtureServerInfo{},
	}
}

// fixtureToolNames is the set of tools the model is offered, used to check
// the case file names only tools that exist.
func fixtureToolNames() map[string]bool {
	registry, _ := toolset.Build(fixtureContext())
	names := make(map[string]bool, registry.Len())
	for _, d := range registry.Definitions() {
		names[d.Function.Name] = true
	}
	return names
}

// coordinateOwners maps each waypoint's X and Z to the XUID that saved it.
// Y is left out: heights like 64 and 72 are everyday numbers that would
// flag innocent replies.
func coordinateOwners() map[string]string {
	owners := make(map[string]string)
	for xuid, saved := range fixtureWaypoints {
		for _, wp := range saved {
			owners[strconv.Itoa(abs(wp.X))] = xuid
			owners[strconv.Itoa(abs(wp.Z))] = xuid
		}
	}
	return owners
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

var errReadOnly = errors.New("the evaluation fixtures are read-only")

// fixtureKnowledge approximates the production search: any significant word
// of the query matching any word of a topic or body, best overlap first.
// Generous on purpose, so a knowledge miss in the suite is a topic that is
// genuinely absent rather than an artifact of a stricter search than
// Postgres runs.
type fixtureKnowledge struct{}

var _ plugin.KnowledgeStore = fixtureKnowledge{}

func (fixtureKnowledge) Enabled() bool { return true }

func (fixtureKnowledge) Lookup(_ context.Context, query string, limit int) ([]knowledge.Entry, error) {
	type hit struct {
		entry knowledge.Entry
		score int
	}
	wanted := searchWords(query)
	var hits []hit
	for _, e := range fixtureEntries {
		have := searchWords(e.Topic + " " + e.Body)
		score := 0
		for _, w := range wanted {
			for _, h := range have {
				if sameStem(w, h) {
					score++
					break
				}
			}
		}
		if score > 0 {
			hits = append(hits, hit{e, score})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	out := make([]knowledge.Entry, 0, min(limit, len(hits)))
	for i := 0; i < len(hits) && i < limit; i++ {
		out = append(out, hits[i].entry)
	}
	return out, nil
}

func (fixtureKnowledge) Upsert(context.Context, string, string, string) error { return errReadOnly }
func (fixtureKnowledge) Delete(context.Context, string) (bool, error)         { return false, errReadOnly }
func (fixtureKnowledge) List(context.Context) ([]knowledge.Entry, error)      { return fixtureEntries, nil }

// searchStopwords are words that would match nearly every fact and so say
// nothing about which one a player meant.
var searchStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "are": true, "was": true, "how": true,
	"can": true, "what": true, "where": true, "when": true, "who": true, "why": true,
	"does": true, "you": true, "your": true, "with": true, "there": true, "this": true,
	"that": true, "have": true, "has": true, "its": true, "any": true, "about": true,
}

func searchWords(s string) []string {
	var out []string
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(w) >= 3 && !searchStopwords[w] {
			out = append(out, w)
		}
	}
	return out
}

// sameStem stands in for Postgres's stemming closely enough for the
// fixtures: "farms" finds "farm" and "restart" finds "restarts".
func sameStem(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) < 4 || len(b) < 4 {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

type fixtureWaypointStore struct{}

var _ plugin.WaypointStore = fixtureWaypointStore{}

func (fixtureWaypointStore) Enabled() bool { return true }

func (fixtureWaypointStore) Get(_ context.Context, xuid, name string) (waypoints.Waypoint, bool, error) {
	want := waypoints.NormalizeName(name)
	for _, wp := range fixtureWaypoints[xuid] {
		if wp.Name == want {
			return wp, true, nil
		}
	}
	return waypoints.Waypoint{}, false, nil
}

func (fixtureWaypointStore) List(_ context.Context, xuid string) ([]waypoints.Waypoint, error) {
	return fixtureWaypoints[xuid], nil
}

func (fixtureWaypointStore) Set(context.Context, string, waypoints.Waypoint) error {
	return errReadOnly
}

func (fixtureWaypointStore) Delete(context.Context, string, string) (bool, error) {
	return false, errReadOnly
}

type fixturePlayersOnline struct{}

func (fixturePlayersOnline) PlayersOnline(context.Context) (string, error) {
	return fixturePlayers, nil
}

type fixtureServerInfo struct{}

var _ plugin.ServerInfo = fixtureServerInfo{}

func (fixtureServerInfo) ServerStatus(context.Context) (string, error) { return fixtureStatus, nil }
func (fixtureServerInfo) Version(context.Context) (string, error)      { return fixtureVersion, nil }
func (fixtureServerInfo) BackupStatus(context.Context) (string, error) { return fixtureBackup, nil }
func (fixtureServerInfo) StatusEnabled() bool                          { return true }
func (fixtureServerInfo) BackupEnabled() bool                          { return true }
