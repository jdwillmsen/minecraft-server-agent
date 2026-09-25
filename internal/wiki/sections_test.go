package wiki

import (
	"slices"
	"strings"
	"testing"
)

// ironGolemExtract mirrors the heading shape minecraft.wiki returned for
// Iron Golem on 2026-09-25: Bedrock detail nested under a topic, not a
// top-level "Bedrock Edition" section.
const ironGolemExtract = `An iron golem is a buildable neutral mob.

== Spawning ==
Iron golems spawn in villages.

=== Villages ===

==== Java Edition ====
Java needs villagers to gossip.

==== Bedrock Edition ====
Bedrock needs 20 beds and 10 villagers.

=== Creation ===
Place four iron blocks in a T and a carved pumpkin on top.

== Drops ==
Iron ingots and poppies.

== History ==
Added in Beta 1.9.`

func TestParseSectionsKeepsTheIntroAndNesting(t *testing.T) {
	root := parseSections(ironGolemExtract)
	if !strings.Contains(root.Text, "buildable neutral mob") {
		t.Errorf("intro = %q", root.Text)
	}
	if got := topSectionNames(root); !slices.Equal(got, []string{"Spawning", "Drops"}) {
		t.Errorf("top sections = %v, want [Spawning Drops] (History is excluded)", got)
	}
}

func TestSelectSectionPrefersBedrockOverJava(t *testing.T) {
	node, path, ok := selectSection(parseSections(ironGolemExtract), "spawning")
	if !ok {
		t.Fatal("spawning matched nothing")
	}
	if !slices.Equal(path, []string{"Spawning"}) {
		t.Errorf("path = %v", path)
	}
	out := renderSection(node)
	if !strings.Contains(out, "20 beds") {
		t.Errorf("render lost the Bedrock rule: %q", out)
	}
	if strings.Contains(out, "gossip") {
		t.Errorf("render kept the Java rule next to a Bedrock one: %q", out)
	}
}

func TestSelectSectionUsesSynonymsAndSubsections(t *testing.T) {
	root := parseSections(ironGolemExtract)
	if _, path, ok := selectSection(root, "loot"); !ok || !slices.Equal(path, []string{"Drops"}) {
		t.Errorf("loot -> %v %v, want Drops", path, ok)
	}
	if _, path, ok := selectSection(root, "creation"); !ok || !slices.Equal(path, []string{"Spawning", "Creation"}) {
		t.Errorf("creation -> %v %v, want Spawning > Creation", path, ok)
	}
}

func TestSelectSectionNeverPicksExcludedHeadings(t *testing.T) {
	node, _, ok := selectSection(parseSections(ironGolemExtract), "history")
	if ok {
		t.Errorf("history was selected: %q", renderSection(node))
	}
}

func TestRenderKeepsJavaWhenThereIsNoBedrockSibling(t *testing.T) {
	root := parseSections("Intro.\n\n== Farming ==\n\n=== Java Edition ===\nOnly Java text here.")
	node, _, _ := selectSection(root, "farming")
	if out := renderSection(node); !strings.Contains(out, "Only Java text") {
		t.Errorf("render dropped the only edition present: %q", out)
	}
}
