package config

import (
	"strings"
	"testing"
)

const presenceActorsJSON = `[
 {"id":"agent","gamertag":"JdwAgent","kind":"agent","groups":[],"default_state":"present"},
 {"id":"afk-bot-1","gamertag":"JdwAfk1","kind":"afk-bot","groups":["bots"],"default_state":"present"},
 {"id":"afk-bot-2","gamertag":"JdwAfk2","kind":"afk-bot","groups":["bots"],"default_state":"parked"}
]`

const presenceTokensJSON = `[
 {"name":"ops","token":"aaaaaaaaaaaaaaaa-ops","scopes":["presence:read","presence:write"]},
 {"name":"afk-bot-1","token":"bbbbbbbbbbbbbbbb-b1","scopes":["presence:read","presence:report"],"actor":"afk-bot-1"}
]`

func loadWithPresence(t *testing.T, actors, tokens, self string) (Config, error) {
	t.Helper()
	clearEnv(t)
	setRequired(t)
	t.Setenv("PRESENCE_ACTORS", actors)
	t.Setenv("PRESENCE_TOKENS", tokens)
	t.Setenv("PRESENCE_SELF_ID", self)
	return Load()
}

// Unset is the supported default: no actors, no tokens, and the agent in the
// world exactly as before presence existed.
func TestPresenceIsOffWhenUnset(t *testing.T) {
	cfg, err := loadWithPresence(t, "", "", "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.PresenceActors) != 0 || len(cfg.PresenceTokens) != 0 {
		t.Errorf("actors/tokens = %v/%v, want none", cfg.PresenceActors, cfg.PresenceTokens)
	}
	if cfg.PresenceSelfID != "agent" {
		t.Errorf("PresenceSelfID = %q, want the default %q", cfg.PresenceSelfID, "agent")
	}
}

func TestPresenceParsesActorsAndTokens(t *testing.T) {
	cfg, err := loadWithPresence(t, presenceActorsJSON, presenceTokensJSON, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.PresenceActors) != 3 {
		t.Fatalf("got %d actors, want 3", len(cfg.PresenceActors))
	}
	bot := cfg.PresenceActors[1]
	if bot.ID != "afk-bot-1" || bot.Gamertag != "JdwAfk1" || bot.Kind != "afk-bot" || bot.DefaultState != "present" || len(bot.Groups) != 1 || bot.Groups[0] != "bots" {
		t.Errorf("actor[1] = %+v", bot)
	}
	if len(cfg.PresenceTokens) != 2 || cfg.PresenceTokens[1].Actor != "afk-bot-1" {
		t.Errorf("tokens = %+v", cfg.PresenceTokens)
	}
}

// A gamertag containing a plain ASCII space is a real, valid gamertag and
// must load like any other -- only the bridge's unsafe characters are
// refused, not ordinary spacing.
func TestPresenceAllowsASpaceInAGamertag(t *testing.T) {
	actors := `[{"id":"agent","gamertag":"Jdw Agent","kind":"agent","groups":[],"default_state":"present"}]`
	cfg, err := loadWithPresence(t, actors, "", "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.PresenceActors) != 1 || cfg.PresenceActors[0].Gamertag != "Jdw Agent" {
		t.Errorf("actors = %+v, want one actor with the spaced gamertag", cfg.PresenceActors)
	}
}

func TestPresenceRefusesBadConfiguration(t *testing.T) {
	bot := func(fields string) string {
		return `[{"id":"agent","gamertag":"JdwAgent","kind":"agent","default_state":"present"},{` + fields + `}]`
	}
	cases := []struct {
		name, actors, tokens, self, wantInError string
	}{
		{"not JSON", `[{`, "", "", "PRESENCE_ACTORS"},
		{"unknown key", bot(`"id":"b","gamertag":"B","kind":"afk-bot","default-state":"present"`), "", "", "default-state"},
		{"trailing data", `[] []`, "", "", "PRESENCE_ACTORS"},
		{"bad id", bot(`"id":"Bot_1","gamertag":"B","kind":"afk-bot","default_state":"present"`), "", "", "Bot_1"},
		{"id all", bot(`"id":"all","gamertag":"B","kind":"afk-bot","default_state":"present"`), "", "", `"all"`},
		{"duplicate id", bot(`"id":"agent","gamertag":"B","kind":"afk-bot","default_state":"present"`), "", "", "twice"},
		{"blank gamertag", bot(`"id":"b","gamertag":" ","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		{"quoted gamertag", bot(`"id":"b","gamertag":"B\"x","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		// A5: mirror the bridge's BRIDGE_KICKABLE rules (kickable.go's
		// isUnsafeRune) so a gamertag config accepts is one the bridge can
		// actually kick.
		{"backslash in gamertag", bot(`"id":"b","gamertag":"B\\x","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		{"leading @ in gamertag", bot(`"id":"b","gamertag":"@Bx","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		{"control character in gamertag", bot(`"id":"b","gamertag":"B\u0007x","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		{"format character in gamertag", bot(`"id":"b","gamertag":"B` + "\u200b" + `x","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		{"non-ASCII whitespace in gamertag", bot(`"id":"b","gamertag":"B` + "\u00a0" + `x","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		{"gamertag clash by case", bot(`"id":"b","gamertag":"jdwagent","kind":"afk-bot","default_state":"present"`), "", "", "gamertag"},
		{"bad kind", bot(`"id":"b","gamertag":"B","kind":"bot","default_state":"present"`), "", "", "kind"},
		{"bad default", bot(`"id":"b","gamertag":"B","kind":"afk-bot","default_state":"away"`), "", "", "default_state"},
		{"group all", bot(`"id":"b","gamertag":"B","kind":"afk-bot","groups":["all"],"default_state":"present"`), "", "", "group"},
		{"group named like an actor", bot(`"id":"b","gamertag":"B","kind":"afk-bot","groups":["agent"],"default_state":"present"`), "", "", "group"},
		{"self missing", presenceActorsJSON, "", "server", "PRESENCE_SELF_ID"},
		{"self not an agent", presenceActorsJSON, "", "afk-bot-1", "PRESENCE_SELF_ID"},
		{"tokens without actors", "", presenceTokensJSON, "", "PRESENCE_ACTORS"},
		{"short token", presenceActorsJSON, `[{"name":"ops","token":"short","scopes":["presence:read"]}]`, "", "16"},
		{"unknown scope", presenceActorsJSON, `[{"name":"ops","token":"cccccccccccccccc","scopes":["presence:admin"]}]`, "", "presence:admin"},
		{"no scopes", presenceActorsJSON, `[{"name":"ops","token":"dddddddddddddddd","scopes":[]}]`, "", "scope"},
		{"unknown bound actor", presenceActorsJSON, `[{"name":"b","token":"eeeeeeeeeeeeeeee","scopes":["presence:read"],"actor":"afk-bot-9"}]`, "", "afk-bot-9"},
		{"report without actor", presenceActorsJSON, `[{"name":"b","token":"ffffffffffffffff","scopes":["presence:report"]}]`, "", "presence:report"},
		{"duplicate name", presenceActorsJSON, `[{"name":"a","token":"gggggggggggggggg-1","scopes":["presence:read"]},{"name":"a","token":"gggggggggggggggg-2","scopes":["presence:read"]}]`, "", "twice"},
		{"shared secret", presenceActorsJSON, `[{"name":"a","token":"hhhhhhhhhhhhhhhh","scopes":["presence:read"]},{"name":"b","token":"hhhhhhhhhhhhhhhh","scopes":["presence:read"]}]`, "", "secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWithPresence(t, tc.actors, tc.tokens, tc.self)
			if err == nil {
				t.Fatal("Load accepted it")
			}
			if !strings.Contains(err.Error(), tc.wantInError) {
				t.Errorf("error %q does not mention %q", err, tc.wantInError)
			}
		})
	}
}

// A token is a secret: whatever is wrong with the configuration, the error
// that reaches the pod log must not carry one.
func TestPresenceErrorsNeverCarryATokenValue(t *testing.T) {
	const secret = "zzzzzzzzzzzzzzzz-leak"
	_, err := loadWithPresence(t, presenceActorsJSON, `[{"name":"b","token":"`+secret+`","scopes":["presence:nope"]}]`, "")
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("error = %v, want a refusal that does not include the token", err)
	}
}
