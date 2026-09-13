package main

import (
	"slices"
	"strconv"
	"testing"
)

// The committed case file is loaded by the normal test run so a typo in it
// fails here, offline, rather than halfway through a GPU run.
func TestCommittedCaseFileCoversTheSpec(t *testing.T) {
	cases, err := loadCases("../../eval/cases.yaml", fixtureToolNames())
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 25 {
		t.Errorf("%d cases, want at least 25", len(cases))
	}

	perCategory := map[string]int{}
	for _, c := range cases {
		perCategory[c.Category]++
	}
	for _, cat := range categories {
		if perCategory[cat] == 0 {
			t.Errorf("no case covers %s", cat)
		}
	}
	// The two categories where a failure is a security problem rather than
	// a poor answer get more than a token case each.
	for _, cat := range []string{"injection", "other_waypoints"} {
		if perCategory[cat] < 3 {
			t.Errorf("%s has %d cases, want at least 3", cat, perCategory[cat])
		}
	}
}

// A case whose asker and XUID disagree would test the wrong player's
// waypoints while reading as correct.
func TestCommittedCasesUseFixturePlayers(t *testing.T) {
	cases, err := loadCases("../../eval/cases.yaml", fixtureToolNames())
	if err != nil {
		t.Fatal(err)
	}
	players := map[string]string{"Alex": alexXUID, "Steve": steveXUID, "Sam": samXUID}
	for _, c := range cases {
		if want, ok := players[c.Asker]; !ok || want != c.XUID {
			t.Errorf("%s: asker %q with xuid %q is not a fixture player", c.ID, c.Asker, c.XUID)
		}
	}
}

func TestParseCasesRejectsWhatWouldSilentlyWeakenACase(t *testing.T) {
	valid := "cases:\n  - {id: a, category: status, asker: Alex, xuid: \"1\", question: q}\n"
	if _, err := parseCases([]byte(valid), fixtureToolNames()); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}

	bad := map[string]string{
		"misspelt key":     "cases:\n  - {id: a, category: status, asker: Alex, xuid: \"1\", question: q, must_contian: [x]}\n",
		"unknown tool":     "cases:\n  - {id: a, category: status, asker: Alex, xuid: \"1\", question: q, tools: {any_of: [announce]}}\n",
		"unknown category": "cases:\n  - {id: a, category: stats, asker: Alex, xuid: \"1\", question: q}\n",
		"bad pattern":      "cases:\n  - {id: a, category: status, asker: Alex, xuid: \"1\", question: q, must_not_match: ['(']}\n",
		"duplicate id":     valid + "  - {id: a, category: status, asker: Alex, xuid: \"1\", question: q}\n",
		"none with any_of": "cases:\n  - {id: a, category: status, asker: Alex, xuid: \"1\", question: q, tools: {none: true, any_of: [server_status]}}\n",
		"no question":      "cases:\n  - {id: a, category: status, asker: Alex, xuid: \"1\"}\n",
		"empty":            "cases: []\n",
	}
	for name, raw := range bad {
		if _, err := parseCases([]byte(raw), fixtureToolNames()); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// scorePrivacy attributes a number in a reply to the one player whose
// waypoint it belongs to. That only holds while no waypoint number also
// appears in a public fact or canned answer, where it would flag an honest
// reply as a leak.
func TestWaypointNumbersAppearNowhereElseInTheFixtures(t *testing.T) {
	text := publicFixtureText()

	seen := map[string]string{}
	for xuid, saved := range fixtureWaypoints {
		for _, wp := range saved {
			for _, n := range []int{abs(wp.X), abs(wp.Z)} {
				key := strconv.Itoa(n)
				if other, dup := seen[key]; dup && other != xuid {
					t.Errorf("%s belongs to two players", key)
				}
				seen[key] = xuid
			}
		}
	}
	for _, tok := range numberToken.FindAllString(text, -1) {
		if _, clash := seen[tok]; clash {
			t.Errorf("waypoint number %s also appears in public fixture text", tok)
		}
	}
}

func TestFixtureToolsetIsTheFullProductionSurface(t *testing.T) {
	want := []string{"knowledge_lookup", "waypoint_lookup", "waypoint_list", "players_online", "server_status", "server_version", "backup_status"}
	got := fixtureToolNames()
	if len(got) != len(want) {
		t.Errorf("fixture offers %d tools, want %d", len(got), len(want))
	}
	for _, name := range want {
		if !got[name] {
			t.Errorf("fixture does not offer %s", name)
		}
	}
}

func TestFixtureKnowledgeFindsHitsAndMisses(t *testing.T) {
	store := fixtureKnowledge{}
	hits, _ := store.Lookup(t.Context(), "wheres the gold farm?", 3)
	if len(hits) == 0 || hits[0].Topic != "gold farm" {
		t.Errorf("gold farm lookup = %+v", hits)
	}
	if misses, _ := store.Lookup(t.Context(), "discord link", 3); len(misses) != 0 {
		t.Errorf("discord lookup found %+v, want nothing", misses)
	}
}

// The facts a reply is held to are read out of the fixtures rather than
// restated, so this checks the reading, not a copy of it.
func TestFixtureFactsComeFromTheCannedAnswers(t *testing.T) {
	facts := fixtureFacts()
	if len(facts.Versions) != 1 || facts.Versions[0] != "1.21.100.7" {
		t.Errorf("versions = %v, want just the server's own", facts.Versions)
	}
	for _, want := range []string{"3", "10"} {
		if !slices.Contains(facts.Counts, want) {
			t.Errorf("counts = %v, want %s among them", facts.Counts, want)
		}
	}
}
