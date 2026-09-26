package wiki

import (
	"regexp"
	"strings"
)

type section struct {
	Title    string
	Level    int
	Text     string
	Children []*section
}

var headingLine = regexp.MustCompile(`^(={2,6})\s*(.*?)\s*={2,6}\s*$`)

// excluded headings answer nothing a player asks in chat, and a few of them
// (History above all) are long enough to crowd out the section that does.
var excluded = map[string]bool{
	"history": true, "gallery": true, "trivia": true, "videos": true,
	"sounds": true, "data values": true, "issues": true,
	"achievements": true, "advancements": true, "references": true,
	"navigation": true, "see also": true, "external links": true,
}

// synonyms maps what players say to the headings the wiki uses.
var synonyms = map[string]string{
	"recipe": "crafting", "recipes": "crafting", "craft": "crafting", "make": "crafting",
	"smelt": "smelting", "cook": "smelting",
	"spawn": "spawning", "spawns": "spawning", "find": "spawning",
	"drop": "drops", "loot": "drops",
	"use": "usage", "uses": "usage",
	"breed": "breeding", "tame": "taming",
}

// parseSections reads the heading tree out of an extract fetched with
// exsectionformat=wiki, where each heading is a "== Title ==" line.
func parseSections(extract string) *section {
	root := &section{Level: 1}
	stack := []*section{root}
	var body []string
	flush := func() {
		top := stack[len(stack)-1]
		top.Text = strings.TrimSpace(strings.Join(body, "\n"))
		body = body[:0]
	}
	for _, line := range strings.Split(extract, "\n") {
		m := headingLine.FindStringSubmatch(line)
		if m == nil {
			body = append(body, line)
			continue
		}
		flush()
		s := &section{Title: m[2], Level: len(m[1])}
		for len(stack) > 1 && stack[len(stack)-1].Level >= s.Level {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]
		parent.Children = append(parent.Children, s)
		stack = append(stack, s)
	}
	flush()
	return root
}

func topSectionNames(root *section) []string {
	var names []string
	for _, c := range root.Children {
		if !excluded[strings.ToLower(c.Title)] {
			names = append(names, c.Title)
		}
	}
	return names
}

// selectSection finds the heading best matching aspect: an exact heading,
// then the heading a synonym names, then the most shared words. Excluded
// headings are never candidates. Among equal scores the first heading in
// document order wins, whatever its depth: a subsection that comes earlier
// beats a same-named top-level section further down.
func selectSection(root *section, aspect string) (*section, []string, bool) {
	want := strings.ToLower(strings.TrimSpace(aspect))
	if want == "" {
		return nil, nil, false
	}
	if s, ok := synonyms[want]; ok {
		want = s
	}
	var best *section
	var bestPath []string
	bestScore := 0
	var walk func(s *section, path []string)
	walk = func(s *section, path []string) {
		for _, c := range s.Children {
			title := strings.ToLower(c.Title)
			if excluded[title] {
				continue
			}
			p := append(append([]string(nil), path...), c.Title)
			score := overlap(want, title)
			if title == want {
				score = 1000
			}
			if score > bestScore {
				best, bestPath, bestScore = c, p, score
			}
			walk(c, p)
		}
	}
	walk(root, nil)
	return best, bestPath, best != nil
}

// fillerWords carry no topic, so sharing one says nothing about whether a
// heading answers the aspect: "how to get" must not pick "Trading to
// villagers".
var fillerWords = map[string]bool{
	"the": true, "and": true, "how": true, "get": true, "for": true,
	"with": true, "from": true, "into": true, "what": true,
}

func overlap(a, b string) int {
	n := 0
	for _, w := range strings.Fields(a) {
		if len(w) < 3 || fillerWords[w] {
			continue
		}
		for _, h := range strings.Fields(b) {
			if strings.TrimSuffix(w, "s") == strings.TrimSuffix(h, "s") {
				n++
				break
			}
		}
	}
	return n
}

// renderSection flattens a subtree to text, dropping a "Java Edition"
// heading wherever a "Bedrock Edition" sibling exists: this server is
// Bedrock, and giving the model both invites it to state the wrong one.
func renderSection(node *section) string {
	var b strings.Builder
	var walk func(s *section)
	walk = func(s *section) {
		if s.Text != "" {
			b.WriteString(s.Text)
			b.WriteString("\n")
		}
		hasBedrock := false
		for _, c := range s.Children {
			if strings.EqualFold(c.Title, "Bedrock Edition") {
				hasBedrock = true
			}
		}
		for _, c := range s.Children {
			title := strings.ToLower(c.Title)
			if excluded[title] || (hasBedrock && title == "java edition") {
				continue
			}
			walk(c)
		}
	}
	walk(node)
	return strings.TrimSpace(b.String())
}
