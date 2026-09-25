package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"

	"go.yaml.in/yaml/v3"
)

// Case is one question put to the model and what a good answer to it looks
// like. Every expectation is optional except the question itself: a case
// scores only what it states.
type Case struct {
	ID       string `yaml:"id"`
	Category string `yaml:"category"`
	// Asker and XUID are who the question comes from. The gamertag is what
	// the model is told and the XUID is what the waypoint tools read, split
	// exactly as production splits them.
	Asker    string `yaml:"asker"`
	XUID     string `yaml:"xuid"`
	Question string `yaml:"question"`

	Tools ToolExpectation `yaml:"tools"`

	// Substring checks ignore case: a model that capitalises a topic
	// differently has still answered it.
	MustContain    []string `yaml:"must_contain"`
	MustContainAny []string `yaml:"must_contain_any"`
	MustNotContain []string `yaml:"must_not_contain"`
	MustNotMatch   []string `yaml:"must_not_match"`

	// Private is whether the answer must be whispered. Nil when either is
	// acceptable: a question about another player's base may or may not
	// lead the model to read the asker's own waypoints, and neither is
	// wrong. Leaked coordinates are caught regardless -- see scorePrivacy.
	Private *bool `yaml:"private"`

	notMatch []*regexp.Regexp
}

// ToolExpectation is which tools a good answer calls. A call to a name the
// toolset never offered fails every case, stated or not.
type ToolExpectation struct {
	AllOf  []string `yaml:"all_of"`
	AnyOf  []string `yaml:"any_of"`
	None   bool     `yaml:"none"`
	Forbid []string `yaml:"forbid"`
}

type caseFile struct {
	// Phrases exists to hold YAML anchors -- the handful of ways a model
	// says it doesn't know, or declines -- so every case that needs one
	// aliases the same list instead of drifting its own copy.
	Phrases map[string][]string `yaml:"phrases"`
	Cases   []Case              `yaml:"cases"`
}

// categories are the question kinds the suite covers. A case outside them
// is almost always a typo, which would otherwise fall out of the
// per-category table unnoticed.
var categories = []string{
	"status", "version", "players", "backup",
	"knowledge_hit", "knowledge_miss",
	"own_waypoints", "other_waypoints",
	"injection", "small_talk", "unanswerable", "wiki",
}

func loadCases(path string, knownTools map[string]bool) ([]Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cases: %w", err)
	}
	return parseCases(raw, knownTools)
}

func parseCases(raw []byte, knownTools map[string]bool) ([]Case, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	// A misspelt key would otherwise decode to nothing and silently drop a
	// check, leaving a case that passes because it no longer tests anything.
	dec.KnownFields(true)
	var f caseFile
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("cases: %w", err)
	}
	if len(f.Cases) == 0 {
		return nil, errors.New("cases: the file defines no cases")
	}

	seen := make(map[string]bool, len(f.Cases))
	var errs []error
	for i := range f.Cases {
		c := &f.Cases[i]
		if err := c.prepare(knownTools); err != nil {
			errs = append(errs, fmt.Errorf("case %d (%q): %w", i+1, c.ID, err))
			continue
		}
		if seen[c.ID] {
			errs = append(errs, fmt.Errorf("case %d: duplicate id %q", i+1, c.ID))
		}
		seen[c.ID] = true
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("cases: %w", err)
	}
	return f.Cases, nil
}

// prepare validates a case and compiles its patterns.
func (c *Case) prepare(knownTools map[string]bool) error {
	switch {
	case c.ID == "":
		return errors.New("missing id")
	case c.Question == "":
		return errors.New("missing question")
	case c.Asker == "" || c.XUID == "":
		return errors.New("missing asker or xuid")
	case !slices.Contains(categories, c.Category):
		return fmt.Errorf("unknown category %q", c.Category)
	case c.Tools.None && (len(c.Tools.AllOf) > 0 || len(c.Tools.AnyOf) > 0):
		return errors.New("tools.none contradicts all_of and any_of")
	}
	// Checked against the real toolset so a renamed tool fails the suite at
	// load time rather than turning every case that names it into a
	// guaranteed tool-selection failure blamed on the model.
	for _, list := range [][]string{c.Tools.AllOf, c.Tools.AnyOf, c.Tools.Forbid} {
		for _, name := range list {
			if !knownTools[name] {
				return fmt.Errorf("tool %q is not in the production toolset", name)
			}
		}
	}
	c.notMatch = nil
	for _, p := range c.MustNotMatch {
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("must_not_match %q: %w", p, err)
		}
		c.notMatch = append(c.notMatch, re)
	}
	return nil
}

// selectCases keeps the cases whose id or category the pattern matches, so
// a prompt change can be re-checked against the cases it targets without
// paying for the whole suite. A nil pattern keeps everything.
func selectCases(cases []Case, only *regexp.Regexp) []Case {
	if only == nil {
		return cases
	}
	var out []Case
	for _, c := range cases {
		if only.MatchString(c.ID) || only.MatchString(c.Category) {
			out = append(out, c)
		}
	}
	return out
}

// filterWiki drops every category-wiki case when wikiOn is false, mirroring
// production with WIKI_ENABLED off: wiki_lookup is never registered there
// (see fixtureContext), so a wiki case could only fail on tool selection --
// a result about the toolset, not about the question a wiki-off run is
// asking. It reports how many it dropped, for the report to state.
func filterWiki(cases []Case, wikiOn bool) ([]Case, int) {
	if wikiOn {
		return cases, 0
	}
	kept := make([]Case, 0, len(cases))
	skipped := 0
	for _, c := range cases {
		if c.Category == "wiki" {
			skipped++
			continue
		}
		kept = append(kept, c)
	}
	return kept, skipped
}
