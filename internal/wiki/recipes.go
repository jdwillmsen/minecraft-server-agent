package wiki

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	wikiLink   = regexp.MustCompile(`\[\[(?:[^|\]]*\|)?([^\]]*)\]\]`)
	gridCell   = regexp.MustCompile(`^([ABC])([123])$`)
	columnName = map[byte]string{'A': "left", 'B': "middle", 'C': "right"}
	rowName    = map[byte]string{'1': "top", '2': "middle", '3': "bottom"}
)

// sliceWikitext returns the body under the heading path, up to the next
// heading of the same or a higher level.
func sliceWikitext(wikitext string, path []string) string {
	lines := strings.Split(wikitext, "\n")
	depth, level, start := 0, 0, -1
	for i, line := range lines {
		m := headingLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		l := len(m[1])
		if start >= 0 {
			if l <= level {
				return strings.Join(lines[start:i], "\n")
			}
			continue
		}
		if depth < len(path) && strings.EqualFold(m[2], path[depth]) {
			depth++
			if depth == len(path) {
				level, start = l, i+1
			}
		}
	}
	if start < 0 {
		return ""
	}
	return strings.Join(lines[start:], "\n")
}

// renderRecipes turns the recipe templates in a wikitext fragment into one
// readable line each. Templates are read, never echoed: an unknown one is
// dropped rather than handed to the model as "{{...}}" markup it might
// repeat in chat.
func renderRecipes(fragment string) []string {
	var out []string
	for _, tpl := range templates(fragment) {
		name, positional, named := splitTemplate(tpl)
		switch strings.ToLower(name) {
		case "crafting":
			if line := renderCrafting(positional, named); line != "" {
				out = append(out, line)
			}
		case "smelting":
			if len(positional) >= 2 {
				out = append(out, fmt.Sprintf("Smelting: %s makes %s", positional[0], positional[1]))
			}
		}
	}
	return out
}

func renderCrafting(positional []string, named map[string]string) string {
	product := named["output"]
	count := ""
	if name, n, ok := strings.Cut(product, ","); ok {
		product, count = strings.TrimSpace(name), strings.TrimSpace(n)
	}
	if product == "" {
		return ""
	}
	made := product
	if count != "" && count != "1" {
		made = count + " " + product
	}
	if len(positional) > 0 {
		return fmt.Sprintf("Crafting (any arrangement): %s makes %s", strings.Join(positional, ", "), made)
	}
	var cells []string
	for k := range named {
		if gridCell.MatchString(strings.ToUpper(k)) && named[k] != "" {
			cells = append(cells, strings.ToUpper(k))
		}
	}
	// Row-major, so the sentence reads top to bottom the way the grid does.
	sort.Slice(cells, func(i, j int) bool {
		if cells[i][1] != cells[j][1] {
			return cells[i][1] < cells[j][1]
		}
		return cells[i][0] < cells[j][0]
	})
	parts := make([]string, 0, len(cells))
	for _, cell := range cells {
		item := strings.ReplaceAll(named[strings.ToLower(cell)], "; ", " or ")
		parts = append(parts, item+" "+position(cell))
	}
	if len(parts) == 0 {
		return ""
	}
	return fmt.Sprintf("Crafting: %s makes %s", strings.Join(parts, ", "), made)
}

func position(cell string) string {
	if cell == "B2" {
		return "in the center"
	}
	return "at " + rowName[cell[1]] + " " + columnName[cell[0]]
}

// templates returns the top-level {{...}} bodies in order, respecting
// nesting so a template inside a cell does not end its parent early.
func templates(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i+1 < len(s); i++ {
		switch s[i : i+2] {
		case "{{":
			if depth == 0 {
				start = i + 2
			}
			depth++
			i++
		case "}}":
			if depth > 0 {
				depth--
				if depth == 0 {
					out = append(out, s[start:i])
				}
			}
			i++
		}
	}
	return out
}

// splitTemplate splits only on pipes at the top level: a pipe inside a
// nested template or a link belongs to that inner markup. Nested templates
// (edition notes such as {{only|java}}) are dropped from the values, so no
// "{{" or "}}" reaches the model.
func splitTemplate(body string) (string, []string, map[string]string) {
	var fields []string
	var cur strings.Builder
	braces, brackets := 0, 0
	for i := 0; i < len(body); i++ {
		switch {
		case strings.HasPrefix(body[i:], "{{"):
			braces++
			i++
			continue
		case braces > 0 && strings.HasPrefix(body[i:], "}}"):
			braces--
			i++
			continue
		case braces > 0:
			continue
		case strings.HasPrefix(body[i:], "[["):
			brackets++
			cur.WriteString("[[")
			i++
			continue
		case brackets > 0 && strings.HasPrefix(body[i:], "]]"):
			brackets--
			cur.WriteString("]]")
			i++
			continue
		case brackets == 0 && body[i] == '|':
			fields = append(fields, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(body[i])
	}
	fields = append(fields, cur.String())
	for i, f := range fields {
		fields[i] = wikiLink.ReplaceAllString(f, "$1")
	}
	name := strings.TrimSpace(fields[0])
	var positional []string
	named := make(map[string]string)
	for _, f := range fields[1:] {
		if k, v, ok := strings.Cut(f, "="); ok {
			named[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
			continue
		}
		if v := strings.TrimSpace(f); v != "" {
			positional = append(positional, v)
		}
	}
	return name, positional, named
}
