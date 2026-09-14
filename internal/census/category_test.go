package census

import "testing"

func TestCategoryOfClassifiesTheMobsThatDriveCaps(t *testing.T) {
	for identifier, want := range map[string]Category{
		"zombie":         Monster,
		"piglin_brute":   Monster,
		"enderman":       Monster,
		"cow":            Animal,
		"villager_v2":    Animal,
		"iron_golem":     Animal,
		"squid":          WaterAnimal,
		"bat":            Ambient,
		"pillager":       Pillager,
		"item":           Ignored,
		"xp_orb":         Ignored,
		"chest_minecart": Ignored,
		"arrow":          Ignored,
	} {
		if got := CategoryOf(identifier); got != want {
			t.Errorf("CategoryOf(%q) = %v, want %v", identifier, got, want)
		}
	}
}

func TestCategoryOfTreatsUnknownIdentifiersAsUncapped(t *testing.T) {
	// A mob added by a future Bedrock release must not be silently counted
	// against a cap it may not belong to.
	if got := CategoryOf("minecraft_mob_from_the_future"); got != Uncapped {
		t.Errorf("CategoryOf(unknown) = %v, want Uncapped", got)
	}
}

func TestIgnoredIsDistinctFromUncapped(t *testing.T) {
	// Ignored means "never counts toward a cap" (items, projectiles).
	// Uncapped means "we do not know". Collapsing them hides new mobs.
	if Ignored == Uncapped {
		t.Fatal("Ignored and Uncapped must be different values")
	}
}
