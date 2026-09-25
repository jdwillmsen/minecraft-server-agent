package wiki

import (
	"slices"
	"strings"
	"testing"
)

// torchWikitext is the Crafting section minecraft.wiki served for Torch on
// 2026-09-25, whose plain-text extract is empty.
const torchWikitext = `== Obtaining ==
=== Breaking ===
Torches can be broken instantly.
=== Crafting ===
{{Crafting
|B2 = Coal; Charcoal
|B3 = Stick
|C3 =
|Output = Torch, 4
|type = Decoration block
}}
=== Natural generation ===
In mineshafts.`

func TestSliceWikitextTakesOnlyTheHeading(t *testing.T) {
	got := sliceWikitext(torchWikitext, []string{"Obtaining", "Crafting"})
	if !strings.Contains(got, "{{Crafting") || strings.Contains(got, "mineshafts") || strings.Contains(got, "instantly") {
		t.Errorf("slice = %q", got)
	}
}

func TestRenderCraftingTemplate(t *testing.T) {
	got := renderRecipes(sliceWikitext(torchWikitext, []string{"Obtaining", "Crafting"}))
	want := []string{"Crafting: Coal or Charcoal in the center, Stick at bottom middle makes 4 Torch"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderShapelessAndSmelting(t *testing.T) {
	got := renderRecipes("{{Crafting|Iron Ingot|Iron Ingot|Output=Shears|shapeless=1}}\n{{Smelting|Raw Iron|Iron Ingot|0.7}}\n{{Some other template|x}}")
	want := []string{
		"Crafting (any arrangement): Iron Ingot, Iron Ingot makes Shears",
		"Smelting: Raw Iron makes Iron Ingot",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRenderStripsWikiLinks(t *testing.T) {
	got := renderRecipes("{{Smelting|[[Raw Iron|raw iron]]|[[Iron Ingot]]}}")
	if !slices.Equal(got, []string{"Smelting: raw iron makes Iron Ingot"}) {
		t.Errorf("got %q", got)
	}
}
