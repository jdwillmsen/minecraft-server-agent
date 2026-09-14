package census

// Category is a Bedrock spawn category. Population caps are counted per
// category per region, so classification decides which cap an entity
// consumes.
type Category int

const (
	// Uncapped is the zero value on purpose: an identifier nobody has
	// classified must not land in a real category by accident.
	Uncapped Category = iota
	Monster
	Animal
	WaterAnimal
	Ambient
	Pillager

	// Ignored is for entities that tick but never count toward a spawn
	// cap: dropped items, orbs, projectiles, vehicles.
	Ignored
)

func (c Category) String() string {
	switch c {
	case Monster:
		return "monster"
	case Animal:
		return "animal"
	case WaterAnimal:
		return "water_animal"
	case Ambient:
		return "ambient"
	case Pillager:
		return "pillager"
	case Ignored:
		return "ignored"
	default:
		return "uncapped"
	}
}

// categories is the single source of truth for classification. Identifiers
// are the save's own, with the minecraft: namespace already stripped.
var categories = map[string]Category{}

func init() {
	for _, id := range []string{
		"zombie", "husk", "drowned", "zombie_villager_v2", "skeleton", "stray",
		"bogged", "creeper", "spider", "cave_spider", "enderman", "endermite",
		"witch", "slime", "magma_cube", "phantom", "silverfish", "guardian",
		"elder_guardian", "shulker", "blaze", "ghast", "wither_skeleton",
		"zombie_pigman", "piglin", "piglin_brute", "hoglin", "zoglin",
		"vindicator", "evocation_illager", "ravager", "warden", "breeze",
		"creaking", "vex",
	} {
		categories[id] = Monster
	}
	for _, id := range []string{
		"cow", "mooshroom", "pig", "sheep", "chicken", "rabbit", "horse",
		"donkey", "mule", "llama", "trader_llama", "wandering_trader", "wolf",
		"cat", "ocelot", "fox", "panda", "polar_bear", "turtle", "goat", "bee",
		"villager_v2", "iron_golem", "snow_golem", "strider", "camel",
		"sniffer", "armadillo", "parrot", "frog", "skeleton_horse",
		"zombie_horse", "happy_ghast", "allay",
	} {
		categories[id] = Animal
	}
	for _, id := range []string{
		"squid", "glow_squid", "dolphin", "cod", "salmon", "pufferfish",
		"tropicalfish", "axolotl", "tadpole",
	} {
		categories[id] = WaterAnimal
	}
	categories["bat"] = Ambient
	categories["pillager"] = Pillager
	for _, id := range []string{
		"item", "xp_orb", "arrow", "thrown_trident", "snowball", "egg",
		"ender_pearl", "splash_potion", "lingering_potion", "fireball",
		"small_fireball", "dragon_fireball", "wither_skull",
		"wither_skull_dangerous", "shulker_bullet", "llama_spit",
		"evocation_fang", "eye_of_ender_signal", "fishing_hook", "boat",
		"chest_boat", "minecart", "chest_minecart", "hopper_minecart",
		"tnt_minecart", "command_block_minecart", "armor_stand", "painting",
		"item_frame", "glow_item_frame", "leash_knot", "falling_block",
		"lightning_bolt", "area_effect_cloud", "tnt", "tripod_camera",
		"agent", "npc",
	} {
		categories[id] = Ignored
	}
}

// CategoryOf classifies an entity identifier. Identifiers nobody has
// classified return Uncapped rather than being folded into a real category,
// so a mob added by a future release shows up as unclassified instead of
// quietly distorting a cap count.
func CategoryOf(identifier string) Category {
	if c, ok := categories[identifier]; ok {
		return c
	}
	return Uncapped
}
