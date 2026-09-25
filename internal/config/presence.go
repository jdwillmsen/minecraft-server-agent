package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode"

	"github.com/jdwillmsen/minecraft-server-agent/internal/text"
)

// PresenceActor is one entry of PRESENCE_ACTORS: an account that puts a
// player into the world.
type PresenceActor struct {
	ID           string   `json:"id"`
	Gamertag     string   `json:"gamertag"`
	Kind         string   `json:"kind"`
	Groups       []string `json:"groups"`
	DefaultState string   `json:"default_state"`
}

// PresenceToken is one entry of PRESENCE_TOKENS.
type PresenceToken struct {
	Name   string   `json:"name"`
	Token  string   `json:"token"`
	Scopes []string `json:"scopes"`
	Actor  string   `json:"actor"`
}

// presenceID is the shape of an actor id, a group and a token name. All
// three are printed into set_by, audit rows and metric labels.
var presenceID = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// minPresenceToken is short enough for any generated secret and long enough
// that a placeholder like "changeme" is refused rather than deployed.
const minPresenceToken = 16

var (
	presenceKinds  = map[string]bool{"agent": true, "afk-bot": true}
	presenceStates = map[string]bool{"present": true, "parked": true}
	presenceScopes = map[string]bool{"presence:read": true, "presence:write": true, "presence:report": true}
)

func loadPresence() ([]PresenceActor, []PresenceToken, string, error) {
	self := stringDefault("PRESENCE_SELF_ID", "agent")
	var actors []PresenceActor
	if err := strictJSON("PRESENCE_ACTORS", &actors); err != nil {
		return nil, nil, "", err
	}
	var tokens []PresenceToken
	if err := strictJSON("PRESENCE_TOKENS", &tokens); err != nil {
		return nil, nil, "", err
	}
	if err := checkPresenceActors(actors, self); err != nil {
		return nil, nil, "", err
	}
	if err := checkPresenceTokens(tokens, actors); err != nil {
		return nil, nil, "", err
	}
	return actors, tokens, self, nil
}

// strictJSON decodes name's value into v, leaving v alone when it is unset.
// Unknown keys are refused: "default-state" would otherwise silently give an
// actor the zero default. The decoder's messages name offsets and keys, never
// values, so a malformed token list cannot print a secret.
func strictJSON(name string, v any) error {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("environment variable %s is not valid: %w", name, err)
	}
	if dec.More() {
		return fmt.Errorf("environment variable %s has data after its JSON value", name)
	}
	return nil
}

func checkPresenceActors(actors []PresenceActor, self string) error {
	if len(actors) == 0 {
		return nil
	}
	ids := make(map[string]bool, len(actors))
	tags := make(map[string]bool, len(actors))
	for i, a := range actors {
		switch {
		case !presenceID.MatchString(a.ID):
			return fmt.Errorf("PRESENCE_ACTORS[%d]: id %q must match %s", i, a.ID, presenceID)
		case a.ID == "all":
			return fmt.Errorf(`PRESENCE_ACTORS[%d]: id "all" names every actor and cannot be one`, i)
		case ids[a.ID]:
			return fmt.Errorf("PRESENCE_ACTORS: id %q is listed twice", a.ID)
		}
		ids[a.ID] = true
		if err := checkPresenceGamertag(a.Gamertag); err != nil {
			return fmt.Errorf("PRESENCE_ACTORS[%d] (%s): %w", i, a.ID, err)
		}
		// Case-insensitive because the server's list and chat are, and
		// folded the same way the bridge folds a kick target so the two
		// agree on which gamertag is which.
		folded := text.FoldASCII(a.Gamertag)
		if tags[folded] {
			return fmt.Errorf("PRESENCE_ACTORS[%d] (%s): gamertag %q belongs to another actor", i, a.ID, a.Gamertag)
		}
		tags[folded] = true
		if !presenceKinds[a.Kind] {
			return fmt.Errorf("PRESENCE_ACTORS[%d] (%s): kind %q must be agent or afk-bot", i, a.ID, a.Kind)
		}
		if !presenceStates[a.DefaultState] {
			return fmt.Errorf("PRESENCE_ACTORS[%d] (%s): default_state %q must be present or parked", i, a.ID, a.DefaultState)
		}
	}
	for i, a := range actors {
		for _, g := range a.Groups {
			// A group spelled like an actor would make "!park agent" mean two
			// things; "all" is implicit and listing it would add nothing.
			if !presenceID.MatchString(g) || g == "all" || ids[g] {
				return fmt.Errorf("PRESENCE_ACTORS[%d] (%s): group %q must match %s and be neither all nor an actor id", i, a.ID, g, presenceID)
			}
		}
	}
	for _, a := range actors {
		if a.ID == self {
			if a.Kind != "agent" {
				return fmt.Errorf("PRESENCE_SELF_ID %q is an %s, not the agent", self, a.Kind)
			}
			return nil
		}
	}
	return fmt.Errorf("PRESENCE_SELF_ID %q is not in PRESENCE_ACTORS", self)
}

// checkPresenceGamertag mirrors mc-console-bridge's BRIDGE_KICKABLE parsing
// (kickable.go's ParseKickable/isUnsafeRune): an actor's gamertag is kicked
// by the bridge under this exact string, so anything the bridge's console
// command could not pass through safely is refused here too, at startup,
// rather than failing a kick later. A quote or backslash could end a quoted
// name early, a leading @ is a selector rather than a player, a control or
// format character can hide inside the name invisibly, and non-ASCII
// whitespace can pass for a plain space without being one. A plain ASCII
// space stays allowed, since gamertags can contain them.
func checkPresenceGamertag(gamertag string) error {
	switch {
	case gamertag == "" || strings.TrimSpace(gamertag) != gamertag:
		return fmt.Errorf("gamertag %q must be set and unpadded", gamertag)
	case strings.HasPrefix(gamertag, "@"), strings.ContainsAny(gamertag, `"\`), strings.ContainsFunc(gamertag, isUnsafeGamertagRune):
		return fmt.Errorf("gamertag %q cannot be passed to kick safely", gamertag)
	}
	return nil
}

// isUnsafeGamertagRune reports whether r has no place in a gamertag: a
// control character, a format character (invisible but not a control, such
// as a zero-width space or a bidi override), or any non-ASCII whitespace
// (which can pass for a plain space without being one). A plain ASCII space
// is none of these, so it stays allowed.
func isUnsafeGamertagRune(r rune) bool {
	if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
		return true
	}
	return r > 0x7F && unicode.IsSpace(r)
}

func checkPresenceTokens(tokens []PresenceToken, actors []PresenceActor) error {
	if len(tokens) == 0 {
		return nil
	}
	if len(actors) == 0 {
		return errors.New("PRESENCE_TOKENS is set but PRESENCE_ACTORS is empty: there is nothing for a token to act on")
	}
	known := make(map[string]bool, len(actors))
	for _, a := range actors {
		known[a.ID] = true
	}
	names := make(map[string]bool, len(tokens))
	secrets := make(map[string]string, len(tokens))
	for i, tk := range tokens {
		switch {
		case !presenceID.MatchString(tk.Name):
			return fmt.Errorf("PRESENCE_TOKENS[%d]: name %q must match %s", i, tk.Name, presenceID)
		case names[tk.Name]:
			return fmt.Errorf("PRESENCE_TOKENS: name %q is listed twice", tk.Name)
		case len(strings.TrimSpace(tk.Token)) < minPresenceToken:
			return fmt.Errorf("PRESENCE_TOKENS[%d] (%s): token must be at least %d characters", i, tk.Name, minPresenceToken)
		}
		names[tk.Name] = true
		if other, dup := secrets[tk.Token]; dup {
			return fmt.Errorf("PRESENCE_TOKENS: %s and %s share a secret, so neither could be told apart", other, tk.Name)
		}
		secrets[tk.Token] = tk.Name
		if len(tk.Scopes) == 0 {
			return fmt.Errorf("PRESENCE_TOKENS[%d] (%s): at least one scope is required", i, tk.Name)
		}
		report := false
		for _, s := range tk.Scopes {
			if !presenceScopes[s] {
				return fmt.Errorf("PRESENCE_TOKENS[%d] (%s): unknown scope %q", i, tk.Name, s)
			}
			report = report || s == "presence:report"
		}
		if tk.Actor != "" && !known[tk.Actor] {
			return fmt.Errorf("PRESENCE_TOKENS[%d] (%s): actor %q is not in PRESENCE_ACTORS", i, tk.Name, tk.Actor)
		}
		// A status report is only ever about the reporter itself, so a token
		// that may report must say which actor it is.
		if report && tk.Actor == "" {
			return fmt.Errorf("PRESENCE_TOKENS[%d] (%s): presence:report needs an actor", i, tk.Name)
		}
	}
	return nil
}
