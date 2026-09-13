# Mob Census Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A `census` binary that reads a Bedrock world save and emits a reproducible population report — entity totals by dimension, regional spawn-cap saturation, name-tagged and persistent mobs, and entity clusters.

**Architecture:** A new `internal/census` package performs a two-pass scan over the world's LevelDB key space: `digp*` keys build an actor-to-dimension index, then `actorprefix*` keys are decoded from Bedrock NBT into entity records. Aggregation buckets those entities into 144x144-block regions and compares each against Bedrock's population-cap table. A `Source` interface supplies world bytes so the engine never knows whether it got a backup archive or a live snapshot. `cmd/census` is a second entrypoint in this repo, following the `cmd/evalllm` precedent.

**Tech Stack:** Go 1.27, `github.com/df-mc/goleveldb/leveldb` (new direct dependency), `github.com/sandertv/gophertunnel/minecraft/nbt` (already a direct dependency).

**Spec:** `docs/superpowers/specs/2026-09-13-mob-census-design.md`

## Global Constraints

- Go 1.27 / toolchain go1.27.1, as pinned in `go.mod`.
- Exactly one new direct dependency is authorised: `github.com/df-mc/goleveldb v1.1.9`. Do **not** add `github.com/df-mc/dragonfly` — pulling the whole server to reach `mcdb` was considered and rejected in the spec.
- NBT decoding uses `nbt.LittleEndian`. Not `nbt.NetworkLittleEndian` — that is the protocol variant and will not read a world save.
- **Entity NBT must be decoded into `map[string]any`, never into a struct.** gophertunnel's decoder is strict and aborts on any tag absent from the target struct. Verified against the live FWB world: `nbt: unexpected named tag '.Air' with type TAG_Short at offset 7: not present in struct to be decoded into`. Entity records carry dozens of varying fields, so struct decoding fails on essentially every entity.
- Concrete Go types out of a `map[string]any` decode, verified against the live world: `identifier` → `string`, `Pos` → `[]interface{}` of `float32`, `UniqueID` → `int64`, `CustomName` → `string`, `CustomNameVisible` → `uint8`, `Persistent` → `uint8`, `Health` → `int16`, `Tags` → `[]interface{}`. Type-assert accordingly; a wrong assertion yields a silent zero value, not an error.
- LevelDB is opened `ReadOnly: true`. The census never writes to a world.
- The `actorprefix` key prefix is 11 bytes; the actor UUID is the following 8 bytes.
- Commit messages follow the repo's conventional-commit style and end with:
  ```
  Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01G57aCPx9HY47pLzij6x1Tv
  ```
- Run `gofmt -w` on every file you touch before committing. `go vet ./...` must pass.

## Scope

This plan covers the census engine, the archive source, the report renderer and the `cmd/census` entrypoint — everything that lives in this repository and is testable without a cluster.

Two pieces of spec phase 1 are deliberately **not** here:

- **`LiveSource`** (the `save hold` / `save query` / `save resume` snapshot) is a `kubectl exec` wrapper whose only meaningful test is against a running cluster, and whose RBAC lives in the `jdw-deployments` chart. It belongs with that chart work.
- **The census CronJob template** lives in `jdw-deployments`, a different repository, and cannot be deployed until this repo publishes an image containing `cmd/census`.

Both land in a follow-on plan against `jdw-deployments`. The `Source` interface defined in Task 9 is the seam they attach to.

---

### Task 1: Entity record and NBT field extraction

**Files:**
- Create: `internal/census/entity.go`
- Test: `internal/census/entity_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Dimension int` with constants `Overworld`, `Nether`, `End`, `UnknownDimension`, and `func (d Dimension) String() string`. `type Entity struct` with fields `Identifier string`, `UniqueID int64`, `Dimension Dimension`, `X, Y, Z float64`, `CustomName string`, `NameVisible bool`, `Persistent bool`, `Health int`. `func EntityFromNBT(m map[string]any) (Entity, bool)` returning `ok=false` when the record has no usable position.

- [ ] **Step 1: Write the failing test**

```go
package census

import "testing"

func TestEntityFromNBTReadsTheFieldTypesBedrockActuallyWrites(t *testing.T) {
	// These concrete types were read off the live FWB world. A decode into
	// map[string]any yields float32 inside an []interface{} for Pos, and
	// uint8 for the boolean-ish flags — not float64 and not bool.
	m := map[string]any{
		"identifier":        "minecraft:zombie",
		"Pos":               []any{float32(1.5), float32(64), float32(-2.5)},
		"UniqueID":          int64(42),
		"CustomName":        "Bob",
		"CustomNameVisible": uint8(1),
		"Persistent":        uint8(1),
		"Health":            int16(20),
	}
	e, ok := EntityFromNBT(m)
	if !ok {
		t.Fatal("EntityFromNBT returned ok=false for a record with a valid Pos")
	}
	if e.Identifier != "zombie" {
		t.Errorf("Identifier = %q, want %q — the minecraft: namespace must be stripped", e.Identifier, "zombie")
	}
	if e.X != 1.5 || e.Y != 64 || e.Z != -2.5 {
		t.Errorf("Pos = (%v,%v,%v), want (1.5,64,-2.5)", e.X, e.Y, e.Z)
	}
	if e.UniqueID != 42 {
		t.Errorf("UniqueID = %d, want 42", e.UniqueID)
	}
	if e.CustomName != "Bob" || !e.NameVisible {
		t.Errorf("CustomName = %q NameVisible = %v, want %q true", e.CustomName, e.NameVisible, "Bob")
	}
	if !e.Persistent {
		t.Error("Persistent = false, want true")
	}
	if e.Health != 20 {
		t.Errorf("Health = %d, want 20", e.Health)
	}
}

func TestEntityFromNBTRejectsARecordWithNoPosition(t *testing.T) {
	if _, ok := EntityFromNBT(map[string]any{"identifier": "minecraft:zombie"}); ok {
		t.Error("ok = true for a record with no Pos, want false")
	}
	if _, ok := EntityFromNBT(map[string]any{
		"identifier": "minecraft:zombie",
		"Pos":        []any{float32(1)},
	}); ok {
		t.Error("ok = true for a Pos with one element, want false")
	}
}

func TestEntityFromNBTDefaultsAbsentOptionalFields(t *testing.T) {
	e, ok := EntityFromNBT(map[string]any{
		"identifier": "minecraft:item",
		"Pos":        []any{float32(0), float32(0), float32(0)},
	})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if e.CustomName != "" || e.NameVisible || e.Persistent || e.Health != 0 {
		t.Errorf("absent optional fields did not default: %+v", e)
	}
}

func TestEntityFromNBTPreservesNonASCIINameTags(t *testing.T) {
	// Name tags are arbitrary player text. NBT strings are length-prefixed
	// UTF-8, so a multi-byte tag must survive byte-for-byte rather than
	// being truncated at a byte boundary inside a rune.
	const name = "Señor Oink 🐖"
	e, ok := EntityFromNBT(map[string]any{
		"identifier": "minecraft:pig",
		"Pos":        []any{float32(0), float32(0), float32(0)},
		"CustomName": name,
	})
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if e.CustomName != name {
		t.Errorf("CustomName = %q, want %q", e.CustomName, name)
	}
}

func TestDimensionString(t *testing.T) {
	for d, want := range map[Dimension]string{
		Overworld: "overworld", Nether: "nether", End: "end", UnknownDimension: "unknown",
	} {
		if got := d.String(); got != want {
			t.Errorf("Dimension(%d).String() = %q, want %q", int(d), got, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/worktrees/minecraft-server-agent/mob-census && go test ./internal/census/ -run TestEntity -v`
Expected: FAIL — the package does not compile, `undefined: EntityFromNBT`.

- [ ] **Step 3: Write minimal implementation**

```go
// Package census reads a Bedrock world save and reports what lives in it.
package census

import "strings"

// Dimension identifies which of the three worlds an entity is saved in.
type Dimension int

const (
	Overworld Dimension = 0
	Nether    Dimension = 1
	End       Dimension = 2

	// UnknownDimension covers actors whose owning chunk carried no digp
	// record. They are real entities that cannot be placed, so they are
	// counted separately rather than silently attributed to the overworld.
	UnknownDimension Dimension = -1
)

func (d Dimension) String() string {
	switch d {
	case Overworld:
		return "overworld"
	case Nether:
		return "nether"
	case End:
		return "end"
	default:
		return "unknown"
	}
}

// Entity is one actor record, reduced to the fields the census reasons about.
type Entity struct {
	Identifier  string
	UniqueID    int64
	Dimension   Dimension
	X, Y, Z     float64
	CustomName  string
	NameVisible bool
	Persistent  bool
	Health      int
}

// EntityFromNBT reduces a decoded actor record. It reports false when the
// record carries no usable position: such an actor cannot be assigned to a
// region, which is the whole point of the census.
//
// The input must come from a map decode. gophertunnel's NBT decoder aborts on
// any tag missing from a target struct, and actor records carry dozens of
// varying tags, so struct decoding fails on nearly every entity.
func EntityFromNBT(m map[string]any) (Entity, bool) {
	x, y, z, ok := nbtPos(m)
	if !ok {
		return Entity{}, false
	}
	return Entity{
		Identifier:  strings.TrimPrefix(nbtString(m, "identifier"), "minecraft:"),
		UniqueID:    nbtInt64(m, "UniqueID"),
		Dimension:   UnknownDimension,
		X:           x,
		Y:           y,
		Z:           z,
		CustomName:  nbtString(m, "CustomName"),
		NameVisible: nbtFlag(m, "CustomNameVisible"),
		Persistent:  nbtFlag(m, "Persistent"),
		Health:      nbtInt(m, "Health"),
	}, true
}

func nbtString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func nbtInt64(m map[string]any, key string) int64 {
	v, _ := m[key].(int64)
	return v
}

// nbtFlag reads a TAG_Byte used as a boolean. Bedrock writes these as uint8.
func nbtFlag(m map[string]any, key string) bool {
	v, _ := m[key].(uint8)
	return v != 0
}

// nbtInt reads a small integer tag. Health is TAG_Short, so int16.
func nbtInt(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case int16:
		return int(v)
	case int32:
		return int(v)
	case uint8:
		return int(v)
	}
	return 0
}

// nbtPos reads the three-element Pos list. A map decode yields []interface{}
// holding float32, so neither []float32 nor float64 elements will assert.
func nbtPos(m map[string]any) (x, y, z float64, ok bool) {
	list, isList := m["Pos"].([]any)
	if !isList || len(list) != 3 {
		return 0, 0, 0, false
	}
	out := make([]float64, 3)
	for i, v := range list {
		f, isFloat := v.(float32)
		if !isFloat {
			return 0, 0, 0, false
		}
		out[i] = float64(f)
	}
	return out[0], out[1], out[2], true
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run 'TestEntity|TestDimension' -v`
Expected: PASS, five tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/entity.go internal/census/entity_test.go
go vet ./internal/census/
git add internal/census/entity.go internal/census/entity_test.go
git commit -m "feat(census): read actor records out of Bedrock NBT maps"
```

---

### Task 2: Spawn category classification

**Files:**
- Create: `internal/census/category.go`
- Test: `internal/census/category_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `type Category int` with constants `Monster`, `Animal`, `WaterAnimal`, `Ambient`, `Pillager`, `Uncapped`, `Ignored`; `func (c Category) String() string`; `func CategoryOf(identifier string) Category`.

- [ ] **Step 1: Write the failing test**

```go
package census

import "testing"

func TestCategoryOfClassifiesTheMobsThatDriveCaps(t *testing.T) {
	for identifier, want := range map[string]Category{
		"zombie":        Monster,
		"piglin_brute":  Monster,
		"enderman":      Monster,
		"cow":           Animal,
		"villager_v2":   Animal,
		"iron_golem":    Animal,
		"squid":         WaterAnimal,
		"bat":           Ambient,
		"pillager":      Pillager,
		"item":          Ignored,
		"xp_orb":        Ignored,
		"chest_minecart": Ignored,
		"arrow":         Ignored,
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/census/ -run TestCategory -v`
Expected: FAIL, `undefined: CategoryOf`.

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run 'TestCategory|TestIgnored' -v`
Expected: PASS, three tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/category.go internal/census/category_test.go
go vet ./internal/census/
git add internal/census/category.go internal/census/category_test.go
git commit -m "feat(census): classify entities into Bedrock spawn categories"
```

---

### Task 3: Region bucketing

**Files:**
- Create: `internal/census/region.go`
- Test: `internal/census/region_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `const RegionSize = 144`; `type RegionKey struct { Dimension Dimension; X, Z int }`; `func RegionOf(d Dimension, x, z float64) RegionKey`; `func (r RegionKey) Bounds() (minX, maxX, minZ, maxZ int)`.

- [ ] **Step 1: Write the failing test**

```go
package census

import "testing"

func TestRegionOfBucketsAt144Blocks(t *testing.T) {
	for _, tc := range []struct {
		x, z       float64
		wantX, wantZ int
	}{
		{0, 0, 0, 0},
		{143.9, 143.9, 0, 0},
		{144, 144, 1, 1},
		{468.5, 268.6, 3, 1}, // the FWB iron farm
	} {
		got := RegionOf(Overworld, tc.x, tc.z)
		if got.X != tc.wantX || got.Z != tc.wantZ {
			t.Errorf("RegionOf(%v,%v) = (%d,%d), want (%d,%d)", tc.x, tc.z, got.X, got.Z, tc.wantX, tc.wantZ)
		}
	}
}

func TestRegionOfFloorsNegativeCoordinates(t *testing.T) {
	// Integer truncation rounds toward zero, so -1 would land in region 0
	// alongside +1 and merge two regions into one. Only floor is correct.
	for _, tc := range []struct {
		x     float64
		wantX int
	}{
		{-0.5, -1},
		{-1, -1},
		{-144, -1},
		{-144.5, -2},
		{-145, -2},
	} {
		if got := RegionOf(Overworld, tc.x, 0); got.X != tc.wantX {
			t.Errorf("RegionOf(%v).X = %d, want %d", tc.x, got.X, tc.wantX)
		}
	}
}

func TestRegionKeysSeparateDimensions(t *testing.T) {
	if RegionOf(Overworld, 0, 0) == RegionOf(Nether, 0, 0) {
		t.Error("overworld and nether regions at the same coordinates compare equal")
	}
}

func TestRegionBounds(t *testing.T) {
	minX, maxX, minZ, maxZ := RegionOf(Overworld, 200, -10).Bounds()
	if minX != 144 || maxX != 287 || minZ != -144 || maxZ != -1 {
		t.Errorf("Bounds() = (%d,%d,%d,%d), want (144,287,-144,-1)", minX, maxX, minZ, maxZ)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/census/ -run TestRegion -v`
Expected: FAIL, `undefined: RegionOf`.

- [ ] **Step 3: Write minimal implementation**

```go
package census

import "math"

// RegionSize is the side of Bedrock's population-control region: 9 chunks,
// 144 blocks. Caps are counted per region, per category.
const RegionSize = 144

// RegionKey identifies one population-control region. Dimension is part of
// the key because the cap tables differ per dimension and regions at the
// same coordinates in different dimensions are unrelated.
type RegionKey struct {
	Dimension Dimension
	X, Z      int
}

// RegionOf places a position into its region. It floors rather than
// truncating: truncation rounds toward zero, which would merge the regions
// either side of an axis into one.
func RegionOf(d Dimension, x, z float64) RegionKey {
	return RegionKey{
		Dimension: d,
		X:         int(math.Floor(x / RegionSize)),
		Z:         int(math.Floor(z / RegionSize)),
	}
}

// Bounds returns the inclusive block bounds of the region, for reporting.
func (r RegionKey) Bounds() (minX, maxX, minZ, maxZ int) {
	return r.X * RegionSize, r.X*RegionSize + RegionSize - 1,
		r.Z * RegionSize, r.Z*RegionSize + RegionSize - 1
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run TestRegion -v`
Expected: PASS, four tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/region.go internal/census/region_test.go
go vet ./internal/census/
git add internal/census/region.go internal/census/region_test.go
git commit -m "feat(census): bucket entities into 144-block population regions"
```

---

### Task 4: Cap table and saturation status

**Files:**
- Create: `internal/census/caps.go`
- Test: `internal/census/caps_test.go`

**Interfaces:**
- Consumes: `Dimension`, `Category` from Tasks 1 and 2.
- Produces: `type Caps struct { Surface, Cave int }`; `func CapsFor(d Dimension, c Category) (Caps, bool)`; `type Status int` with `Headroom`, `AtRisk`, `Capped`, `StatusUnknown`; `func (s Status) String() string`; `func StatusOf(d Dimension, c Category, count int) Status`; `const GlobalCap = 200`.

- [ ] **Step 1: Write the failing test**

```go
package census

import "testing"

func TestCapsForMatchesTheBedrockTable(t *testing.T) {
	for _, tc := range []struct {
		d    Dimension
		c    Category
		want Caps
	}{
		{Overworld, Monster, Caps{Surface: 8, Cave: 16}},
		{Overworld, Animal, Caps{Surface: 4, Cave: 0}},
		{Overworld, WaterAnimal, Caps{Surface: 36, Cave: 0}},
		{Overworld, Ambient, Caps{Surface: 0, Cave: 2}},
		{Overworld, Pillager, Caps{Surface: 8, Cave: 8}},
		{Nether, Monster, Caps{Surface: 0, Cave: 16}},
		{Nether, Animal, Caps{Surface: 0, Cave: 4}},
		{End, Monster, Caps{Surface: 10, Cave: 8}},
	} {
		got, ok := CapsFor(tc.d, tc.c)
		if !ok {
			t.Errorf("CapsFor(%v,%v) ok = false, want true", tc.d, tc.c)
			continue
		}
		if got != tc.want {
			t.Errorf("CapsFor(%v,%v) = %+v, want %+v", tc.d, tc.c, got, tc.want)
		}
	}
}

func TestCapsForHasNoEntryForUncountedCategories(t *testing.T) {
	for _, c := range []Category{Ignored, Uncapped} {
		if _, ok := CapsFor(Overworld, c); ok {
			t.Errorf("CapsFor(overworld,%v) ok = true, want false", c)
		}
	}
	if _, ok := CapsFor(UnknownDimension, Monster); ok {
		t.Error("CapsFor(unknown dimension) ok = true, want false")
	}
}

func TestStatusOfReportsARangeNotAFalsePrecision(t *testing.T) {
	// Surface-versus-cave is fixed when a mob spawns and is not written to
	// the save, so the exact cap is unknowable. Overworld monsters are
	// capped somewhere in 8..16: at or below 8 there is headroom for
	// certain, above 16 the region is saturated for certain, between the
	// two it depends on how those mobs spawned.
	for _, tc := range []struct {
		count int
		want  Status
	}{
		{0, Headroom},
		{8, Headroom},
		{9, AtRisk},
		{16, AtRisk},
		{17, Capped},
	} {
		if got := StatusOf(Overworld, Monster, tc.count); got != tc.want {
			t.Errorf("StatusOf(overworld,monster,%d) = %v, want %v", tc.count, got, tc.want)
		}
	}
}

func TestStatusOfHandlesAZeroSurfaceCap(t *testing.T) {
	// Nether monsters are 0 surface / 16 cave. The lower bound is zero, so
	// any mob at all is already past it.
	if got := StatusOf(Nether, Monster, 1); got != AtRisk {
		t.Errorf("StatusOf(nether,monster,1) = %v, want AtRisk", got)
	}
	if got := StatusOf(Nether, Monster, 17); got != Capped {
		t.Errorf("StatusOf(nether,monster,17) = %v, want Capped", got)
	}
}

func TestStatusOfIsUnknownForUncountedCategories(t *testing.T) {
	if got := StatusOf(Overworld, Ignored, 5000); got != StatusUnknown {
		t.Errorf("StatusOf(ignored) = %v, want StatusUnknown", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/census/ -run 'TestCaps|TestStatus' -v`
Expected: FAIL, `undefined: CapsFor`.

- [ ] **Step 3: Write minimal implementation**

```go
package census

// GlobalCap is Bedrock's server-wide ceiling on environmental spawning. It
// is not scaled by player count. The census reports world totals against it
// as context only: the figure the game checks is over loaded chunks, which a
// save cannot show.
const GlobalCap = 200

// Caps is one cell of the population-control table. Surface and Cave are
// separate ceilings for the same category.
type Caps struct {
	Surface, Cave int
}

var capTable = map[Dimension]map[Category]Caps{
	Overworld: {
		Monster:     {Surface: 8, Cave: 16},
		Animal:      {Surface: 4, Cave: 0},
		WaterAnimal: {Surface: 36, Cave: 0},
		Ambient:     {Surface: 0, Cave: 2},
		Pillager:    {Surface: 8, Cave: 8},
	},
	Nether: {
		Monster:     {Surface: 0, Cave: 16},
		Animal:      {Surface: 0, Cave: 4},
		WaterAnimal: {Surface: 0, Cave: 0},
		Ambient:     {Surface: 0, Cave: 0},
		Pillager:    {Surface: 0, Cave: 0},
	},
	End: {
		Monster:     {Surface: 10, Cave: 8},
		Animal:      {Surface: 4, Cave: 0},
		WaterAnimal: {Surface: 36, Cave: 0},
		Ambient:     {Surface: 0, Cave: 2},
		Pillager:    {Surface: 8, Cave: 8},
	},
}

// CapsFor returns the population caps for a dimension and category. It
// reports false for categories that consume no cap and for entities whose
// dimension could not be resolved.
func CapsFor(d Dimension, c Category) (Caps, bool) {
	byCategory, ok := capTable[d]
	if !ok {
		return Caps{}, false
	}
	caps, ok := byCategory[c]
	return caps, ok
}

// Status is how close a region is to refusing new spawns.
type Status int

const (
	StatusUnknown Status = iota
	Headroom
	AtRisk
	Capped
)

func (s Status) String() string {
	switch s {
	case Headroom:
		return "headroom"
	case AtRisk:
		return "at_risk"
	case Capped:
		return "capped"
	default:
		return "unknown"
	}
}

// StatusOf grades a regional count against its cap range.
//
// Bedrock fixes whether a mob counts as a surface or a cave spawn at spawn
// time and does not write that to the save, so the exact applicable cap
// cannot be recovered. Rather than invent a single number, the count is
// graded against both bounds: at or below the lower bound there is headroom
// whichever way the mobs spawned, above the upper bound the region is
// saturated whichever way, and between them it depends on facts the save
// does not carry.
func StatusOf(d Dimension, c Category, count int) Status {
	caps, ok := CapsFor(d, c)
	if !ok {
		return StatusUnknown
	}
	lower, upper := caps.Surface, caps.Cave
	if lower > upper {
		lower, upper = upper, lower
	}
	switch {
	case count > upper:
		return Capped
	case count > lower:
		return AtRisk
	default:
		return Headroom
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run 'TestCaps|TestStatus' -v`
Expected: PASS, five tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/caps.go internal/census/caps_test.go
go vet ./internal/census/
git add internal/census/caps.go internal/census/caps_test.go
git commit -m "feat(census): grade regions against the Bedrock population cap range"
```

---

### Task 5: Dimension index from digp records

**Files:**
- Create: `internal/census/dimension.go`
- Test: `internal/census/dimension_test.go`

**Interfaces:**
- Consumes: `Dimension` from Task 1.
- Produces: `type dimensionIndex map[[8]byte]Dimension`; `func (ix dimensionIndex) addDigp(key, value []byte)`; `func (ix dimensionIndex) lookup(actorID []byte) Dimension`.

- [ ] **Step 1: Write the failing test**

```go
package census

import (
	"encoding/binary"
	"testing"
)

func digpKey(chunkX, chunkZ int32, dim *int32) []byte {
	k := append([]byte("digp"), make([]byte, 8)...)
	binary.LittleEndian.PutUint32(k[4:], uint32(chunkX))
	binary.LittleEndian.PutUint32(k[8:], uint32(chunkZ))
	if dim != nil {
		k = append(k, 0, 0, 0, 0)
		binary.LittleEndian.PutUint32(k[12:], uint32(*dim))
	}
	return k
}

func actorID(n uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, n)
	return b
}

func TestAddDigpTreatsAnEightByteSuffixAsOverworld(t *testing.T) {
	ix := dimensionIndex{}
	ix.addDigp(digpKey(1, 2, nil), actorID(7))
	if got := ix.lookup(actorID(7)); got != Overworld {
		t.Errorf("lookup = %v, want overworld", got)
	}
}

func TestAddDigpReadsTheTrailingDimensionOnATwelveByteSuffix(t *testing.T) {
	nether := int32(1)
	end := int32(2)
	ix := dimensionIndex{}
	ix.addDigp(digpKey(1, 2, &nether), actorID(8))
	ix.addDigp(digpKey(3, 4, &end), actorID(9))
	if got := ix.lookup(actorID(8)); got != Nether {
		t.Errorf("nether lookup = %v, want nether", got)
	}
	if got := ix.lookup(actorID(9)); got != End {
		t.Errorf("end lookup = %v, want end", got)
	}
}

func TestAddDigpSplitsAValueHoldingSeveralActors(t *testing.T) {
	ix := dimensionIndex{}
	value := append(actorID(11), actorID(12)...)
	ix.addDigp(digpKey(0, 0, nil), value)
	for _, id := range []uint64{11, 12} {
		if got := ix.lookup(actorID(id)); got != Overworld {
			t.Errorf("lookup(%d) = %v, want overworld", id, got)
		}
	}
}

func TestLookupOfAnUnknownActorIsUnknown(t *testing.T) {
	if got := (dimensionIndex{}).lookup(actorID(99)); got != UnknownDimension {
		t.Errorf("lookup = %v, want unknown", got)
	}
}

func TestAddDigpIgnoresMalformedKeysAndValues(t *testing.T) {
	ix := dimensionIndex{}
	ix.addDigp([]byte("digp"), actorID(1))                 // no chunk coords
	ix.addDigp(digpKey(0, 0, nil), []byte{1, 2, 3})        // value not a multiple of 8
	if len(ix) != 0 {
		t.Errorf("index has %d entries after malformed input, want 0", len(ix))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/census/ -run 'TestAddDigp|TestLookup' -v`
Expected: FAIL, `undefined: dimensionIndex`.

- [ ] **Step 3: Write minimal implementation**

```go
package census

import "encoding/binary"

// digpPrefix marks the per-chunk records that list which actors belong to a
// chunk. The key carries the chunk's coordinates, and its length is the only
// thing that says which dimension the chunk is in.
const digpPrefix = "digp"

// dimensionIndex maps an actor's 8-byte id to the dimension of the chunk
// that owns it. Actor records themselves carry no dimension.
type dimensionIndex map[[8]byte]Dimension

// addDigp records every actor listed in one digp value.
//
// The key suffix is 8 bytes (chunk X and Z) for the overworld, or 12 bytes
// with a trailing dimension int for the nether and the end. The value is a
// packed array of 8-byte actor ids. Malformed records are skipped rather
// than guessed at: a wrong dimension silently moves entities between cap
// tables.
func (ix dimensionIndex) addDigp(key, value []byte) {
	suffix := key[len(digpPrefix):]
	var dim Dimension
	switch len(suffix) {
	case 8:
		dim = Overworld
	case 12:
		dim = Dimension(int32(binary.LittleEndian.Uint32(suffix[8:])))
	default:
		return
	}
	if len(value)%8 != 0 {
		return
	}
	for i := 0; i+8 <= len(value); i += 8 {
		var id [8]byte
		copy(id[:], value[i:i+8])
		ix[id] = dim
	}
}

// lookup resolves an actor id. An actor whose chunk carried no digp record
// resolves to UnknownDimension rather than defaulting to the overworld.
func (ix dimensionIndex) lookup(actorID []byte) Dimension {
	if len(actorID) != 8 {
		return UnknownDimension
	}
	var id [8]byte
	copy(id[:], actorID)
	if d, ok := ix[id]; ok {
		return d
	}
	return UnknownDimension
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run 'TestAddDigp|TestLookup' -v`
Expected: PASS, five tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/dimension.go internal/census/dimension_test.go
go vet ./internal/census/
git add internal/census/dimension.go internal/census/dimension_test.go
git commit -m "feat(census): resolve actor dimensions from digp chunk records"
```

---

### Task 6: Scan a world into entities

**Files:**
- Create: `internal/census/scan.go`
- Create: `internal/census/fixture_test.go`
- Test: `internal/census/scan_test.go`
- Modify: `go.mod`, `go.sum` (adds `github.com/df-mc/goleveldb`)

**Interfaces:**
- Consumes: `Entity`, `EntityFromNBT`, `dimensionIndex` from Tasks 1 and 5.
- Produces: `func Scan(dbPath string) ([]Entity, ScanStats, error)`; `type ScanStats struct { Records, Decoded, Unparsable, Unplaced int; FirstUnparsableErr string }`. Test helper: `func writeFixtureWorld(t *testing.T, actors []fixtureActor) string` and `type fixtureActor struct { ID uint64; Dimension *int32; NBT map[string]any }`.

- [ ] **Step 1: Write the failing test**

Create `internal/census/fixture_test.go`:

```go
package census

import (
	"encoding/binary"
	"path/filepath"
	"testing"

	"github.com/df-mc/goleveldb/leveldb"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// fixtureActor is one entity to write into a throwaway world. Dimension nil
// writes an 8-byte digp key (overworld); non-nil writes the 12-byte form.
type fixtureActor struct {
	ID        uint64
	Dimension *int32
	NBT       map[string]any
}

// writeFixtureWorld builds a real LevelDB holding the given actors and
// returns its path. Building the world through the same encoding the game
// uses is what makes the scan test meaningful — a hand-rolled byte fixture
// would only prove the parser agrees with itself.
func writeFixtureWorld(t *testing.T, actors []fixtureActor) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "db")
	db, err := leveldb.OpenFile(path, nil)
	if err != nil {
		t.Fatalf("open fixture world: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close fixture world: %v", err)
		}
	}()

	byChunk := map[int32][]byte{}
	for _, a := range actors {
		id := make([]byte, 8)
		binary.LittleEndian.PutUint64(id, a.ID)

		payload, err := nbt.MarshalEncoding(a.NBT, nbt.LittleEndian)
		if err != nil {
			t.Fatalf("marshal actor %d: %v", a.ID, err)
		}
		if err := db.Put(append([]byte("actorprefix"), id...), payload, nil); err != nil {
			t.Fatalf("put actor %d: %v", a.ID, err)
		}

		var dim int32
		if a.Dimension != nil {
			dim = *a.Dimension
		}
		byChunk[dim] = append(byChunk[dim], id...)
	}

	for dim, ids := range byChunk {
		key := append([]byte("digp"), make([]byte, 8)...)
		binary.LittleEndian.PutUint32(key[4:], uint32(dim)) // chunk X, arbitrary
		if dim != 0 {
			key = append(key, 0, 0, 0, 0)
			binary.LittleEndian.PutUint32(key[12:], uint32(dim))
		}
		if err := db.Put(key, ids, nil); err != nil {
			t.Fatalf("put digp for dimension %d: %v", dim, err)
		}
	}
	return path
}

func pos(x, y, z float32) []any { return []any{x, y, z} }
```

Create `internal/census/scan_test.go`:

```go
package census

import "testing"

func TestScanReadsEntitiesWithTheirDimensions(t *testing.T) {
	nether := int32(1)
	path := writeFixtureWorld(t, []fixtureActor{
		{ID: 1, NBT: map[string]any{
			"identifier": "minecraft:zombie",
			"Pos":        pos(10, 64, 20),
			"UniqueID":   int64(1),
		}},
		{ID: 2, Dimension: &nether, NBT: map[string]any{
			"identifier": "minecraft:piglin_brute",
			"Pos":        pos(-5, 40, -5),
			"UniqueID":   int64(2),
			"Persistent": uint8(1),
		}},
	})

	entities, stats, err := Scan(path)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entities) != 2 {
		t.Fatalf("got %d entities, want 2", len(entities))
	}
	if stats.Decoded != 2 || stats.Unparsable != 0 {
		t.Errorf("stats = %+v, want 2 decoded and 0 unparsable", stats)
	}

	byID := map[int64]Entity{}
	for _, e := range entities {
		byID[e.UniqueID] = e
	}
	if got := byID[1]; got.Identifier != "zombie" || got.Dimension != Overworld {
		t.Errorf("actor 1 = %+v, want zombie in the overworld", got)
	}
	if got := byID[2]; got.Identifier != "piglin_brute" || got.Dimension != Nether || !got.Persistent {
		t.Errorf("actor 2 = %+v, want a persistent piglin_brute in the nether", got)
	}
}

func TestScanCountsUnplacedEntities(t *testing.T) {
	// An actor with no Pos cannot be assigned to a region. It must be
	// counted, not silently dropped — a census that quietly loses entities
	// is worse than one that says it lost them.
	path := writeFixtureWorld(t, []fixtureActor{
		{ID: 3, NBT: map[string]any{"identifier": "minecraft:zombie", "UniqueID": int64(3)}},
	})
	entities, stats, err := Scan(path)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("got %d entities, want 0", len(entities))
	}
	if stats.Unplaced != 1 {
		t.Errorf("Unplaced = %d, want 1", stats.Unplaced)
	}
	if stats.Records != 1 {
		t.Errorf("Records = %d, want 1", stats.Records)
	}
}

func TestScanRejectsAMissingWorld(t *testing.T) {
	if _, _, err := Scan(t.TempDir() + "/does-not-exist"); err == nil {
		t.Error("Scan of a missing world returned nil error")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go get github.com/df-mc/goleveldb@v1.1.9
go mod tidy
go test ./internal/census/ -run TestScan -v
```
Expected: FAIL, `undefined: Scan`.

- [ ] **Step 3: Write minimal implementation**

```go
package census

import (
	"fmt"

	"github.com/df-mc/goleveldb/leveldb"
	"github.com/df-mc/goleveldb/leveldb/opt"
	"github.com/df-mc/goleveldb/leveldb/util"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// actorPrefix marks a saved entity. The 8 bytes after it are the actor id
// that digp records refer to.
const actorPrefix = "actorprefix"

// ScanStats records what the scan saw, so a census can state how much of the
// world it failed to read instead of quietly reporting a short count.
type ScanStats struct {
	Records           int    // actorprefix keys seen
	Decoded           int    // records that became entities
	Unparsable        int    // records whose NBT would not decode
	Unplaced          int    // records decoded but carrying no usable position
	FirstUnparsableErr string // first decode failure seen, or empty if none
}

// Scan reads every entity out of a Bedrock world's LevelDB.
//
// The database is opened read-only: the census must never be able to modify
// a world, and the archive it usually reads is the only copy of that day's
// backup.
func Scan(dbPath string) ([]Entity, ScanStats, error) {
	db, err := leveldb.OpenFile(dbPath, &opt.Options{ReadOnly: true})
	if err != nil {
		return nil, ScanStats{}, fmt.Errorf("open world %s: %w", dbPath, err)
	}
	defer db.Close()

	// Two passes: digp records must all be read before any actor can be
	// placed, because an actor's dimension lives in the chunk record rather
	// than in the actor itself, and the iteration order gives no guarantee
	// that a chunk is seen before the actors it owns.
	index := dimensionIndex{}
	if err := iterate(db, []byte(digpPrefix), func(k, v []byte) {
		index.addDigp(k, v)
	}); err != nil {
		return nil, ScanStats{}, err
	}

	var (
		entities []Entity
		stats    ScanStats
	)
	if err := iterate(db, []byte(actorPrefix), func(k, v []byte) {
		stats.Records++
		var m map[string]any
		if err := nbt.UnmarshalEncoding(v, &m, nbt.LittleEndian); err != nil {
			stats.Unparsable++
			if stats.FirstUnparsableErr == "" {
				stats.FirstUnparsableErr = err.Error()
			}
			return
		}
		e, ok := EntityFromNBT(m)
		if !ok {
			stats.Unplaced++
			return
		}
		e.Dimension = index.lookup(k[len(actorPrefix):])
		stats.Decoded++
		entities = append(entities, e)
	}); err != nil {
		return nil, stats, err
	}
	return entities, stats, nil
}

// iterate seeks the prefix range and calls fn for each key in it. The callback
// must not retain k or v: the iterator reuses their backing arrays between steps.
func iterate(db *leveldb.DB, prefix []byte, fn func(k, v []byte)) error {
	it := db.NewIterator(util.BytesPrefix(prefix), nil)
	defer it.Release()
	for it.Next() {
		fn(it.Key(), it.Value())
	}
	if err := it.Error(); err != nil {
		return fmt.Errorf("iterate %s: %w", prefix, err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run TestScan -v`
Expected: PASS, four tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/scan.go internal/census/scan_test.go internal/census/fixture_test.go
go vet ./internal/census/
git add go.mod go.sum internal/census/scan.go internal/census/scan_test.go internal/census/fixture_test.go
git commit -m "feat(census): scan a Bedrock world save into placed entities"
```

---

### Task 7: Entity clustering

**Files:**
- Create: `internal/census/cluster.go`
- Test: `internal/census/cluster_test.go`

**Interfaces:**
- Consumes: `Entity` from Task 1.
- Produces: `type Cluster struct { Count int; CentreX, CentreY, CentreZ float64; MinX, MaxX, MinY, MaxY, MinZ, MaxZ float64 }`; `func ClusterEntities(entities []Entity, radius float64) []Cluster` returning clusters in deterministic order (sorted by Count descending, then by CentreX, CentreY, CentreZ, MinX, MinY, MinZ, MaxX, MaxY, MaxZ ascending).

- [ ] **Step 1: Write the failing test**

```go
package census

import (
	"fmt"
	"testing"
)

func entityAt(x, y, z float64) Entity {
	return Entity{Identifier: "item", Dimension: Overworld, X: x, Y: y, Z: z}
}

func TestClusterEntitiesGroupsNeighboursAndSeparatesDistantOnes(t *testing.T) {
	got := ClusterEntities([]Entity{
		entityAt(0, 0, 0),
		entityAt(1, 0, 0),
		entityAt(2, 0, 0),
		entityAt(500, 0, 0),
	}, 8)
	if len(got) != 2 {
		t.Fatalf("got %d clusters, want 2", len(got))
	}
	if got[0].Count != 3 {
		t.Errorf("largest cluster has %d, want 3", got[0].Count)
	}
	if got[1].Count != 1 {
		t.Errorf("second cluster has %d, want 1", got[1].Count)
	}
}

func TestClusterEntitiesChainsThroughIntermediateMembers(t *testing.T) {
	// Transitive grouping: the ends are 20 apart, further than the radius,
	// but each hop is within it, so all three are one cluster.
	got := ClusterEntities([]Entity{
		entityAt(0, 0, 0),
		entityAt(10, 0, 0),
		entityAt(20, 0, 0),
	}, 12)
	if len(got) != 1 || got[0].Count != 3 {
		t.Fatalf("got %d clusters (first count %d), want one cluster of 3", len(got), got[0].Count)
	}
}

func TestClusterEntitiesReportsCentreAndBounds(t *testing.T) {
	got := ClusterEntities([]Entity{
		entityAt(0, 10, 0),
		entityAt(4, 14, 8),
	}, 32)
	if len(got) != 1 {
		t.Fatalf("got %d clusters, want 1", len(got))
	}
	c := got[0]
	if c.CentreX != 2 || c.CentreY != 12 || c.CentreZ != 4 {
		t.Errorf("centre = (%v,%v,%v), want (2,12,4)", c.CentreX, c.CentreY, c.CentreZ)
	}
	if c.MinX != 0 || c.MaxX != 4 || c.MinZ != 0 || c.MaxZ != 8 {
		t.Errorf("bounds = x %v..%v z %v..%v, want x 0..4 z 0..8", c.MinX, c.MaxX, c.MinZ, c.MaxZ)
	}
}

func TestClusterEntitiesGroupsAcrossTheOriginOnNegativeCoordinates(t *testing.T) {
	// Points either side of an axis must still group. The nether item pile
	// that motivated this sits at negative x, so getting this wrong splits
	// the exact finding the tool exists to make.
	got := ClusterEntities([]Entity{
		entityAt(-2, 0, -2),
		entityAt(-1, 0, -1),
		entityAt(1, 0, 1),
	}, 16)
	if len(got) != 1 || got[0].Count != 3 {
		t.Fatalf("got %d clusters (first count %d), want one cluster of 3", len(got), got[0].Count)
	}
	if got[0].MinX != -2 || got[0].MaxX != 1 {
		t.Errorf("bounds x = %v..%v, want -2..1", got[0].MinX, got[0].MaxX)
	}
}

func TestClusterEntitiesOnAnEmptyInput(t *testing.T) {
	if got := ClusterEntities(nil, 16); len(got) != 0 {
		t.Errorf("got %d clusters for no entities, want 0", len(got))
	}
}

func TestClusterEntitiesOrderIsDeterministic(t *testing.T) {
	// Create several hundred entities arranged as well-separated pairs.
	// Each pair is a cluster of exactly 2 entities; pairs are far enough apart
	// that they don't merge. This produces many equal-sized clusters, exercising
	// the tie-breaking logic that must be deterministic.
	const pairs = 300
	const spacing = 1000.0
	var entities []Entity
	for i := 0; i < pairs; i++ {
		x := float64(i) * spacing
		entities = append(entities, entityAt(x, 0, 0))
		entities = append(entities, entityAt(x+1, 0, 0))
	}

	// Call ClusterEntities multiple times on the same slice.
	const runs = 50
	var fingerprints [runs]string
	for run := 0; run < runs; run++ {
		result := ClusterEntities(entities, 8)
		var fp string
		for _, c := range result {
			fp += fmt.Sprintf("(%d,%.1f,%.1f,%.1f);", c.Count, c.CentreX, c.CentreY, c.CentreZ)
		}
		fingerprints[run] = fp
	}

	// All fingerprints must be identical.
	for run := 1; run < runs; run++ {
		if fingerprints[run] != fingerprints[0] {
			t.Errorf("run %d fingerprint differs from run 0: %s != %s",
				run, fingerprints[run], fingerprints[0])
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/census/ -run TestCluster -v`
Expected: FAIL, `undefined: ClusterEntities`.

- [ ] **Step 3: Write minimal implementation**

```go
package census

import (
	"math"
	"sort"
)

// Cluster is a group of entities that sit within a chained radius of each
// other. Clusters are what turn a flat count into a location: "4,147 items"
// is a number, "4,147 items at x=-24 y=14 z=1320" is a place to go.
type Cluster struct {
	Count                      int
	CentreX, CentreY, CentreZ  float64
	MinX, MaxX                 float64
	MinY, MaxY                 float64
	MinZ, MaxZ                 float64
}

// ClusterEntities groups entities by proximity and returns the groups
// largest first. The returned order is deterministic for a given input,
// sorted by Count (descending), then by CentreX, CentreY, CentreZ, MinX,
// MinY, MinZ, MaxX, MaxY, MaxZ (all ascending).
//
// Grouping is transitive: two entities further apart than the radius still
// share a cluster if a chain of neighbours links them. A spatial hash keeps
// the comparison local, so this stays practical on the tens of thousands of
// entities a real world holds rather than degrading to every-pair.
func ClusterEntities(entities []Entity, radius float64) []Cluster {
	n := len(entities)
	if n == 0 {
		return nil
	}

	type cell struct{ x, y, z int }
	grid := map[cell][]int{}
	// Floor, not truncation. Truncating toward zero makes the cell
	// straddling an axis twice as wide as the others, which is survivable
	// here but makes the neighbour argument depend on a coincidence rather
	// than on every cell being exactly one radius across.
	cellOf := func(e Entity) cell {
		return cell{
			int(math.Floor(e.X / radius)),
			int(math.Floor(e.Y / radius)),
			int(math.Floor(e.Z / radius)),
		}
	}
	for i, e := range entities {
		c := cellOf(e)
		grid[c] = append(grid[c], i)
	}

	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(a int) int {
		for parent[a] != a {
			parent[a] = parent[parent[a]]
			a = parent[a]
		}
		return a
	}
	union := func(a, b int) {
		ra, rb := find(a), find(b)
		if ra != rb {
			parent[rb] = ra
		}
	}

	r2 := radius * radius
	for c, members := range grid {
		for dx := -1; dx <= 1; dx++ {
			for dy := -1; dy <= 1; dy++ {
				for dz := -1; dz <= 1; dz++ {
					neighbours, ok := grid[cell{c.x + dx, c.y + dy, c.z + dz}]
					if !ok {
						continue
					}
					for _, i := range members {
						for _, j := range neighbours {
							if j <= i {
								continue
							}
							ex, ey, ez := entities[i].X-entities[j].X,
								entities[i].Y-entities[j].Y,
								entities[i].Z-entities[j].Z
							if ex*ex+ey*ey+ez*ez <= r2 {
								union(i, j)
							}
						}
					}
				}
			}
		}
	}

	members := map[int][]int{}
	for i := 0; i < n; i++ {
		root := find(i)
		members[root] = append(members[root], i)
	}

	out := make([]Cluster, 0, len(members))
	for _, group := range members {
		first := entities[group[0]]
		c := Cluster{
			Count: len(group),
			MinX:  first.X, MaxX: first.X,
			MinY: first.Y, MaxY: first.Y,
			MinZ: first.Z, MaxZ: first.Z,
		}
		var sx, sy, sz float64
		for _, i := range group {
			e := entities[i]
			sx, sy, sz = sx+e.X, sy+e.Y, sz+e.Z
			c.MinX, c.MaxX = min(c.MinX, e.X), max(c.MaxX, e.X)
			c.MinY, c.MaxY = min(c.MinY, e.Y), max(c.MaxY, e.Y)
			c.MinZ, c.MaxZ = min(c.MinZ, e.Z), max(c.MaxZ, e.Z)
		}
		count := float64(len(group))
		c.CentreX, c.CentreY, c.CentreZ = sx/count, sy/count, sz/count
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if out[i].CentreX != out[j].CentreX {
			return out[i].CentreX < out[j].CentreX
		}
		if out[i].CentreY != out[j].CentreY {
			return out[i].CentreY < out[j].CentreY
		}
		if out[i].CentreZ != out[j].CentreZ {
			return out[i].CentreZ < out[j].CentreZ
		}
		if out[i].MinX != out[j].MinX {
			return out[i].MinX < out[j].MinX
		}
		if out[i].MinY != out[j].MinY {
			return out[i].MinY < out[j].MinY
		}
		if out[i].MinZ != out[j].MinZ {
			return out[i].MinZ < out[j].MinZ
		}
		if out[i].MaxX != out[j].MaxX {
			return out[i].MaxX < out[j].MaxX
		}
		if out[i].MaxY != out[j].MaxY {
			return out[i].MaxY < out[j].MaxY
		}
		return out[i].MaxZ < out[j].MaxZ
	})
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run TestCluster -v`
Expected: PASS, six tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/cluster.go internal/census/cluster_test.go
go vet ./internal/census/
git add internal/census/cluster.go internal/census/cluster_test.go
git commit -m "feat(census): group entities into located clusters"
```

---

### Task 8: Aggregate entities into a census

**Files:**
- Create: `internal/census/census.go`
- Test: `internal/census/census_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1-7.
- Produces:
  - `type Census struct { TakenAt time.Time; SourceKind string; Stats ScanStats; Totals []Total; Regions []Region; Named []Named; PersistentByIdentifier map[string]int; Concentrations []Concentration }`
  - `type Total struct { Dimension Dimension; Identifier string; Category Category; Count int }`
  - `type Region struct { Key RegionKey; Category Category; Count int; Status Status }`
  - `type Named struct { Name string; Identifier string; Dimension Dimension; X, Y, Z float64; Persistent bool }`
  - `type Concentration struct { Dimension Dimension; Identifier string; Cluster Cluster }`
  - `const ConcentrationThreshold = 25`, `const ConcentrationRadius = 24.0`
  - `func Aggregate(entities []Entity, stats ScanStats, takenAt time.Time, sourceKind string) Census`

- [ ] **Step 1: Write the failing test**

```go
package census

import (
	"fmt"
	"testing"
	"time"
)

func TestAggregateCountsTotalsPerDimensionAndIdentifier(t *testing.T) {
	c := Aggregate([]Entity{
		{Identifier: "zombie", Dimension: Overworld, X: 0, Z: 0},
		{Identifier: "zombie", Dimension: Overworld, X: 1, Z: 1},
		{Identifier: "zombie", Dimension: Nether, X: 0, Z: 0},
	}, ScanStats{Decoded: 3}, time.Unix(0, 0), "archive")

	counts := map[Dimension]int{}
	for _, tot := range c.Totals {
		if tot.Identifier != "zombie" {
			t.Fatalf("unexpected identifier %q", tot.Identifier)
		}
		if tot.Category != Monster {
			t.Errorf("category = %v, want monster", tot.Category)
		}
		counts[tot.Dimension] = tot.Count
	}
	if counts[Overworld] != 2 || counts[Nether] != 1 {
		t.Errorf("counts = %v, want overworld 2 and nether 1", counts)
	}
}

func TestAggregateGradesRegionsAndExcludesUncountedCategories(t *testing.T) {
	var entities []Entity
	for i := 0; i < 17; i++ {
		entities = append(entities, Entity{Identifier: "zombie", Dimension: Overworld, X: float64(i), Z: 0})
	}
	// Items share the region but must not appear as a graded row.
	entities = append(entities, Entity{Identifier: "item", Dimension: Overworld, X: 0, Z: 0})

	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")
	if len(c.Regions) != 1 {
		t.Fatalf("got %d graded regions, want 1", len(c.Regions))
	}
	r := c.Regions[0]
	if r.Category != Monster || r.Count != 17 {
		t.Fatalf("region = %+v, want 17 monsters", r)
	}
	if r.Status != Capped {
		t.Errorf("status = %v, want capped (17 is above the overworld monster upper bound of 16)", r.Status)
	}
}

func TestAggregateRanksRegionsWorstFirst(t *testing.T) {
	var entities []Entity
	for i := 0; i < 3; i++ {
		entities = append(entities, Entity{Identifier: "zombie", Dimension: Overworld, X: float64(i), Z: 0})
	}
	for i := 0; i < 20; i++ {
		entities = append(entities, Entity{Identifier: "zombie", Dimension: Overworld, X: 200 + float64(i), Z: 0})
	}
	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")
	if len(c.Regions) != 2 {
		t.Fatalf("got %d regions, want 2", len(c.Regions))
	}
	if c.Regions[0].Count != 20 {
		t.Errorf("first region count = %d, want the worst region (20) first", c.Regions[0].Count)
	}
}

func TestAggregateCollectsNamedAndPersistentEntities(t *testing.T) {
	c := Aggregate([]Entity{
		{Identifier: "cat", Dimension: Overworld, X: 1, Y: 2, Z: 3, CustomName: "OJ", Persistent: true},
		{Identifier: "piglin_brute", Dimension: Nether, Persistent: true},
		{Identifier: "zombie", Dimension: Overworld},
	}, ScanStats{}, time.Unix(0, 0), "archive")

	if len(c.Named) != 1 {
		t.Fatalf("got %d named entities, want 1", len(c.Named))
	}
	if c.Named[0].Name != "OJ" || c.Named[0].Identifier != "cat" || c.Named[0].X != 1 {
		t.Errorf("named = %+v, want OJ the cat at x=1", c.Named[0])
	}
	if c.PersistentByIdentifier["piglin_brute"] != 1 || c.PersistentByIdentifier["cat"] != 1 {
		t.Errorf("persistent counts = %v, want one brute and one cat", c.PersistentByIdentifier)
	}
	if _, ok := c.PersistentByIdentifier["zombie"]; ok {
		t.Error("a non-persistent zombie appears in the persistent counts")
	}
}

func TestAggregateFindsConcentrationsAndLocatesThem(t *testing.T) {
	// A pile of one entity type in one spot is the shape of every entity
	// leak found so far. A count alone says there is a problem; the cluster
	// centre says where to stand to fix it.
	var entities []Entity
	for i := 0; i < 40; i++ {
		entities = append(entities, Entity{
			Identifier: "item", Dimension: Nether,
			X: -24 + float64(i%4), Y: 14, Z: 1320 + float64(i%4),
		})
	}
	// Scattered singletons of the same type must not become a concentration.
	for i := 0; i < 5; i++ {
		entities = append(entities, Entity{
			Identifier: "item", Dimension: Nether,
			X: float64(i) * 5000, Y: 60, Z: 0,
		})
	}
	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")

	if len(c.Concentrations) != 1 {
		t.Fatalf("got %d concentrations, want 1", len(c.Concentrations))
	}
	got := c.Concentrations[0]
	if got.Identifier != "item" || got.Dimension != Nether {
		t.Errorf("concentration = %+v, want nether items", got)
	}
	if got.Cluster.Count != 40 {
		t.Errorf("cluster count = %d, want 40", got.Cluster.Count)
	}
	if got.Cluster.CentreX > -20 || got.Cluster.CentreX < -28 {
		t.Errorf("centre x = %v, want it near -24", got.Cluster.CentreX)
	}
}

func TestAggregateIgnoresTypesBelowTheConcentrationThreshold(t *testing.T) {
	var entities []Entity
	for i := 0; i < ConcentrationThreshold-1; i++ {
		entities = append(entities, Entity{Identifier: "cow", Dimension: Overworld, X: float64(i), Z: 0})
	}
	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")
	if len(c.Concentrations) != 0 {
		t.Errorf("got %d concentrations below the threshold, want 0", len(c.Concentrations))
	}
}

func TestAggregateCarriesProvenance(t *testing.T) {
	when := time.Unix(1757800000, 0).UTC()
	c := Aggregate(nil, ScanStats{Records: 7}, when, "archive")
	if !c.TakenAt.Equal(when) || c.SourceKind != "archive" || c.Stats.Records != 7 {
		t.Errorf("provenance = %v/%q/%+v, want it carried through unchanged", c.TakenAt, c.SourceKind, c.Stats)
	}
}

func TestAggregateIsDeterministicOverTiedRows(t *testing.T) {
	// Build input that ties on the old comparator keys:
	// - Same identifier and same count in different dimensions (ties Totals and Regions)
	// - Concentrations of equal size in well-separated locations in different dimensions
	var entities []Entity

	// 5 zombies in Overworld, all in region (0, 0)
	for i := 0; i < 5; i++ {
		entities = append(entities, Entity{Identifier: "zombie", Dimension: Overworld, X: float64(i), Z: 0})
	}

	// 5 zombies in Nether, all in region (0, 0) [also ties Regions]
	for i := 0; i < 5; i++ {
		entities = append(entities, Entity{Identifier: "zombie", Dimension: Nether, X: float64(i), Z: 0})
	}

	// 25 items in Overworld at one location (concentration cluster 1)
	for i := 0; i < 25; i++ {
		entities = append(entities, Entity{Identifier: "item", Dimension: Overworld, X: 100 + float64(i%3), Z: 100 + float64(i%3)})
	}

	// 25 items in Nether at a different location (concentration cluster 2, same count as cluster 1)
	for i := 0; i < 25; i++ {
		entities = append(entities, Entity{Identifier: "item", Dimension: Nether, X: 5000 + float64(i%3), Z: 5000 + float64(i%3)})
	}

	// Fingerprint a census by converting its structured output to a string
	fingerprint := func(c Census) string {
		var fp string
		for _, tot := range c.Totals {
			fp += fmt.Sprintf("T:%d:%s:%d:%d,", tot.Dimension, tot.Identifier, tot.Category, tot.Count)
		}
		for _, reg := range c.Regions {
			fp += fmt.Sprintf("R:%d:%d:%d:%d:%d,", reg.Key.Dimension, reg.Key.X, reg.Key.Z, reg.Category, reg.Count)
		}
		for _, nam := range c.Named {
			fp += fmt.Sprintf("N:%s:%s:%d,", nam.Name, nam.Identifier, nam.Dimension)
		}
		for _, con := range c.Concentrations {
			fp += fmt.Sprintf("C:%d:%s:%d,", con.Dimension, con.Identifier, con.Cluster.Count)
		}
		return fp
	}

	// Run Aggregate 50 times and collect fingerprints
	var firstFP string
	var diffSection string
	for run := 0; run < 50; run++ {
		c := Aggregate(entities, ScanStats{}, time.Unix(0, 0), "archive")
		fp := fingerprint(c)
		if run == 0 {
			firstFP = fp
		} else if fp != firstFP {
			// Find which section differed
			if len(c.Totals) != len(c.Totals) {
				diffSection = "Totals"
			} else if len(c.Regions) != len(c.Regions) {
				diffSection = "Regions"
			} else if len(c.Concentrations) != len(c.Concentrations) {
				diffSection = "Concentrations"
			}
			if diffSection == "" {
				diffSection = "ordering within a section"
			}
			t.Fatalf("run %d produced different output than run 0 (differed in %s): %q vs %q", run, diffSection, fp, firstFP)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/census/ -run TestAggregate -v`
Expected: FAIL, `undefined: Aggregate` (before implementation); once implemented, TestAggregateIsDeterministicOverTiedRows must FAIL on the old comparators.

- [ ] **Step 3: Write minimal implementation**

```go
package census

import (
	"sort"
	"time"
)

// Total is one entity type's count within one dimension.
type Total struct {
	Dimension  Dimension
	Identifier string
	Category   Category
	Count      int
}

// Region is one population-control region's occupancy for one category,
// graded against that category's cap range.
type Region struct {
	Key      RegionKey
	Category Category
	Count    int
	Status   Status
}

// Named is a name-tagged entity. Name tags force persistence, so each of
// these permanently occupies a slot in its region's cap.
type Named struct {
	Name       string
	Identifier string
	Dimension  Dimension
	X, Y, Z    float64
	Persistent bool
}

// Concentration is the largest cluster of one entity type in one dimension.
// It is what turns "6,515 items in the nether" into a place to stand.
type Concentration struct {
	Dimension  Dimension
	Identifier string
	Cluster    Cluster
}

// ConcentrationThreshold is how many of one type in one dimension it takes
// before the census bothers clustering them, and ConcentrationRadius is the
// chaining distance used. Below the threshold a "pile" is just a herd.
// ConcentrationsPerType bounds how many clusters of one type are kept.
// Keeping only the largest hides the second pile, and the second pile is
// often the interesting one: on FWB the base village outranks the iron farm,
// so reporting one cluster per type would have made the farm invisible.
const (
	ConcentrationThreshold = 25
	ConcentrationRadius    = 24.0
	ConcentrationsPerType  = 3
)

// Census is one complete reading of a world.
type Census struct {
	// TakenAt is when the world data was captured, not when the scan ran.
	// Every consumer renders it: a report that cannot say how old it is
	// will eventually be believed when it should not be.
	TakenAt    time.Time
	SourceKind string
	Stats      ScanStats

	Totals                 []Total
	Regions                []Region
	Named                  []Named
	PersistentByIdentifier map[string]int
	Concentrations         []Concentration
}

// Aggregate turns scanned entities into the census.
//
// Only categories that consume a cap are graded into regions. Items,
// projectiles and vehicles are counted in the totals — they matter for tick
// cost and for spotting leaks — but they occupy no spawn slot, so grading
// them would invent pressure that does not exist.
func Aggregate(entities []Entity, stats ScanStats, takenAt time.Time, sourceKind string) Census {
	c := Census{
		TakenAt:                takenAt,
		SourceKind:             sourceKind,
		Stats:                  stats,
		PersistentByIdentifier: map[string]int{},
	}

	type totalKey struct {
		dimension  Dimension
		identifier string
	}
	totals := map[totalKey]int{}
	type regionCategory struct {
		key      RegionKey
		category Category
	}
	regions := map[regionCategory]int{}

	// Entities are retained per type so the largest concentration of each
	// can be located afterwards. Only types that pass the threshold are
	// clustered, so this never runs the pairwise pass over a long tail of
	// singletons.
	byType := map[totalKey][]Entity{}

	for _, e := range entities {
		category := CategoryOf(e.Identifier)
		key := totalKey{e.Dimension, e.Identifier}
		totals[key]++
		byType[key] = append(byType[key], e)

		if e.Persistent {
			c.PersistentByIdentifier[e.Identifier]++
		}
		if e.CustomName != "" {
			c.Named = append(c.Named, Named{
				Name:       e.CustomName,
				Identifier: e.Identifier,
				Dimension:  e.Dimension,
				X:          e.X, Y: e.Y, Z: e.Z,
				Persistent: e.Persistent,
			})
		}
		if _, counted := CapsFor(e.Dimension, category); counted {
			regions[regionCategory{RegionOf(e.Dimension, e.X, e.Z), category}]++
		}
	}

	for k, count := range totals {
		c.Totals = append(c.Totals, Total{
			Dimension:  k.dimension,
			Identifier: k.identifier,
			Category:   CategoryOf(k.identifier),
			Count:      count,
		})
	}
	sort.Slice(c.Totals, func(i, j int) bool {
		if c.Totals[i].Count != c.Totals[j].Count {
			return c.Totals[i].Count > c.Totals[j].Count
		}
		if c.Totals[i].Identifier != c.Totals[j].Identifier {
			return c.Totals[i].Identifier < c.Totals[j].Identifier
		}
		return c.Totals[i].Dimension < c.Totals[j].Dimension
	})

	for k, count := range regions {
		c.Regions = append(c.Regions, Region{
			Key:      k.key,
			Category: k.category,
			Count:    count,
			Status:   StatusOf(k.key.Dimension, k.category, count),
		})
	}
	sort.Slice(c.Regions, func(i, j int) bool {
		if c.Regions[i].Count != c.Regions[j].Count {
			return c.Regions[i].Count > c.Regions[j].Count
		}
		if c.Regions[i].Key.Dimension != c.Regions[j].Key.Dimension {
			return c.Regions[i].Key.Dimension < c.Regions[j].Key.Dimension
		}
		if c.Regions[i].Key.X != c.Regions[j].Key.X {
			return c.Regions[i].Key.X < c.Regions[j].Key.X
		}
		if c.Regions[i].Key.Z != c.Regions[j].Key.Z {
			return c.Regions[i].Key.Z < c.Regions[j].Key.Z
		}
		return c.Regions[i].Category < c.Regions[j].Category
	})

	for key, group := range byType {
		if len(group) < ConcentrationThreshold {
			continue
		}
		kept := 0
		for _, cluster := range ClusterEntities(group, ConcentrationRadius) {
			if cluster.Count < ConcentrationThreshold || kept >= ConcentrationsPerType {
				break // clusters are sorted largest first
			}
			c.Concentrations = append(c.Concentrations, Concentration{
				Dimension:  key.dimension,
				Identifier: key.identifier,
				Cluster:    cluster,
			})
			kept++
		}
	}
	sort.Slice(c.Concentrations, func(i, j int) bool {
		if c.Concentrations[i].Cluster.Count != c.Concentrations[j].Cluster.Count {
			return c.Concentrations[i].Cluster.Count > c.Concentrations[j].Cluster.Count
		}
		if c.Concentrations[i].Identifier != c.Concentrations[j].Identifier {
			return c.Concentrations[i].Identifier < c.Concentrations[j].Identifier
		}
		if c.Concentrations[i].Dimension != c.Concentrations[j].Dimension {
			return c.Concentrations[i].Dimension < c.Concentrations[j].Dimension
		}
		if c.Concentrations[i].Cluster.CentreX != c.Concentrations[j].Cluster.CentreX {
			return c.Concentrations[i].Cluster.CentreX < c.Concentrations[j].Cluster.CentreX
		}
		return c.Concentrations[i].Cluster.CentreZ < c.Concentrations[j].Cluster.CentreZ
	})

	sort.Slice(c.Named, func(i, j int) bool {
		if c.Named[i].Name != c.Named[j].Name {
			return c.Named[i].Name < c.Named[j].Name
		}
		if c.Named[i].Identifier != c.Named[j].Identifier {
			return c.Named[i].Identifier < c.Named[j].Identifier
		}
		if c.Named[i].Dimension != c.Named[j].Dimension {
			return c.Named[i].Dimension < c.Named[j].Dimension
		}
		if c.Named[i].X != c.Named[j].X {
			return c.Named[i].X < c.Named[j].X
		}
		if c.Named[i].Y != c.Named[j].Y {
			return c.Named[i].Y < c.Named[j].Y
		}
		return c.Named[i].Z < c.Named[j].Z
	})
	return c
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run TestAggregate -v`
Expected: PASS, eight tests (seven original aggregate tests plus TestAggregateIsDeterministicOverTiedRows).

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/census.go internal/census/census_test.go
go vet ./internal/census/
git add internal/census/census.go internal/census/census_test.go
git commit -m "feat(census): aggregate entities into totals, graded regions and name tags"
```

---

### Task 9: Source interface and archive source

**Files:**
- Create: `internal/census/source.go`
- Test: `internal/census/source_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `type World struct { DBPath string; TakenAt time.Time; Kind string }`; `type Source interface { Open(ctx context.Context) (World, func() error, error) }`; `type ArchiveSource struct { Dir string }`; `func (s ArchiveSource) Open(ctx context.Context) (World, func() error, error)`.

- [ ] **Step 1: Write the failing test**

```go
package census

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeArchive(t *testing.T, dir, name string, files map[string]string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatalf("tar body %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return path
}

func TestArchiveSourceExtractsTheNewestArchive(t *testing.T) {
	dir := t.TempDir()
	writeArchive(t, dir, "fwb-20260901T000000Z.tar.gz", map[string]string{"FWB/db/CURRENT": "old"})
	writeArchive(t, dir, "fwb-20260913T000000Z.tar.gz", map[string]string{"FWB/db/CURRENT": "new"})

	world, cleanup, err := ArchiveSource{Dir: dir}.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()

	body, err := os.ReadFile(filepath.Join(world.DBPath, "CURRENT"))
	if err != nil {
		t.Fatalf("read extracted world: %v", err)
	}
	if string(body) != "new" {
		t.Errorf("extracted %q, want the newest archive's contents", body)
	}
	if world.Kind != "archive" {
		t.Errorf("Kind = %q, want %q", world.Kind, "archive")
	}
	want := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	if !world.TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v parsed from the archive name", world.TakenAt, want)
	}
}

func TestArchiveSourceCleanupRemovesTheExtraction(t *testing.T) {
	dir := t.TempDir()
	writeArchive(t, dir, "fwb-20260913T000000Z.tar.gz", map[string]string{"FWB/db/CURRENT": "x"})
	world, cleanup, err := ArchiveSource{Dir: dir}.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(world.DBPath); !os.IsNotExist(err) {
		t.Errorf("extraction still present after cleanup: %v", err)
	}
}

func TestArchiveSourceRefusesAnEmptyDirectory(t *testing.T) {
	if _, _, err := (ArchiveSource{Dir: t.TempDir()}).Open(context.Background()); err == nil {
		t.Error("Open of a directory with no archives returned nil error")
	}
}

func TestArchiveSourceRefusesPathsEscapingTheExtractionRoot(t *testing.T) {
	// A tar entry naming ../ must never be written outside the temporary
	// directory. The archive is trusted today, but a path-traversal write
	// running as the census would be a real foothold.
	dir := t.TempDir()
	writeArchive(t, dir, "fwb-20260913T000000Z.tar.gz", map[string]string{"../escaped": "x"})
	if _, _, err := (ArchiveSource{Dir: dir}).Open(context.Background()); err == nil {
		t.Error("Open accepted an archive containing a ../ path")
	}
	if _, err := os.Stat(filepath.Join(dir, "..", "escaped")); err == nil {
		t.Error("a ../ tar entry was written outside the extraction root")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/census/ -run TestArchiveSource -v`
Expected: FAIL, `undefined: ArchiveSource`.

- [ ] **Step 3: Write minimal implementation**

```go
package census

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// World is an extracted, readable world plus where it came from.
type World struct {
	// DBPath is the directory holding the LevelDB files.
	DBPath string
	// TakenAt is when the world data was captured.
	TakenAt time.Time
	// Kind names the source, for the report's provenance line.
	Kind string
}

// Source supplies world bytes. The engine above never learns which
// implementation it was given, so a census reads the same whether it came
// from last night's backup or a snapshot taken a moment ago.
//
// Open returns a cleanup function the caller must invoke.
type Source interface {
	Open(ctx context.Context) (World, func() error, error)
}

// ArchiveSource reads the newest backup archive in a directory.
//
// The backup job that writes these archives already performs Bedrock's
// save hold / save query / save resume sequence, so the bytes are
// internally consistent. Reading them costs nothing on the running server
// and cannot touch the world.
type ArchiveSource struct {
	Dir string
}

// archiveName matches the backup job's own naming, and the timestamp in it
// is the only record of when the world was captured.
var archiveName = regexp.MustCompile(`^fwb-(\d{8}T\d{6}Z)\.tar\.gz$`)

func (s ArchiveSource) Open(ctx context.Context) (World, func() error, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return World{}, nil, fmt.Errorf("read backup directory %s: %w", s.Dir, err)
	}
	var names []string
	for _, e := range entries {
		if archiveName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return World{}, nil, fmt.Errorf("no fwb-<stamp>.tar.gz archive in %s", s.Dir)
	}
	// The stamp is fixed-width and zero-padded, so lexical order is
	// chronological order.
	sort.Strings(names)
	newest := names[len(names)-1]

	stamp, err := time.Parse("20060102T150405Z", archiveName.FindStringSubmatch(newest)[1])
	if err != nil {
		return World{}, nil, fmt.Errorf("parse timestamp from %s: %w", newest, err)
	}

	root, err := os.MkdirTemp("", "census-world-")
	if err != nil {
		return World{}, nil, fmt.Errorf("create extraction directory: %w", err)
	}
	cleanup := func() error { return os.RemoveAll(root) }

	if err := extract(ctx, filepath.Join(s.Dir, newest), root); err != nil {
		_ = cleanup()
		return World{}, nil, err
	}

	dbPath, err := findDB(root)
	if err != nil {
		_ = cleanup()
		return World{}, nil, err
	}
	return World{DBPath: dbPath, TakenAt: stamp, Kind: "archive"}, cleanup, nil
}

func extract(ctx context.Context, archive, root string) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("open archive %s: %w", archive, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gunzip %s: %w", archive, err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", archive, err)
		}

		// Refuse any entry that would land outside the extraction root
		// rather than sanitising it. A traversal entry means the archive is
		// not what it claims to be, and continuing past that is how a
		// surprise becomes a write to somebody's home directory.
		target := filepath.Join(root, filepath.Clean("/"+header.Name))
		if !strings.HasPrefix(target, filepath.Clean(root)+string(os.PathSeparator)) {
			return fmt.Errorf("archive %s contains an entry escaping the extraction root: %q", archive, header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fmt.Errorf("create %s: %w", filepath.Dir(target), err)
			}
			out, err := os.Create(target)
			if err != nil {
				return fmt.Errorf("create %s: %w", target, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return fmt.Errorf("write %s: %w", target, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("close %s: %w", target, err)
			}
		}
	}
}

// findDB locates the LevelDB directory inside an extracted archive. The
// archive's internal layout has changed before, so this searches rather than
// assuming a fixed path.
func findDB(root string) (string, error) {
	var found string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "db" && found == "" {
			found = path
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("search extracted archive: %w", err)
	}
	if found == "" {
		return "", fmt.Errorf("no db directory inside the extracted archive at %s", root)
	}
	return found, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run TestArchiveSource -v`
Expected: PASS, four tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/source.go internal/census/source_test.go
go vet ./internal/census/
git add internal/census/source.go internal/census/source_test.go
git commit -m "feat(census): read worlds from the newest backup archive"
```

---

### Task 10: Report renderer

**Files:**
- Create: `internal/census/report.go`
- Test: `internal/census/report_test.go`

**Interfaces:**
- Consumes: `Census` from Task 8.
- Produces: `func Render(c Census, opts ReportOptions) string`; `type ReportOptions struct { TopRegions, TopTypes int }`; `func DefaultReportOptions() ReportOptions`.

- [ ] **Step 1: Write the failing test**

```go
package census

import (
	"strings"
	"testing"
	"time"
)

func sampleCensus() Census {
	return Aggregate([]Entity{
		{Identifier: "zombie", Dimension: Overworld, X: 0, Y: 64, Z: 0},
		{Identifier: "zombie", Dimension: Overworld, X: 1, Y: 64, Z: 0},
		{Identifier: "piglin_brute", Dimension: Nether, X: 600, Y: 40, Z: 10, Persistent: true},
		{Identifier: "cat", Dimension: Overworld, X: 173, Y: 66, Z: 263, CustomName: "OJ", Persistent: true},
	}, ScanStats{Records: 4, Decoded: 4}, time.Date(2026, 9, 13, 20, 31, 0, 0, time.UTC), "archive")
}

func TestRenderStatesProvenanceProminently(t *testing.T) {
	out := Render(sampleCensus(), DefaultReportOptions())
	if !strings.Contains(out, "2026-09-13T20:31:00Z") {
		t.Error("report does not state the world timestamp it was taken from")
	}
	if !strings.Contains(out, "archive") {
		t.Error("report does not state its source kind")
	}
}

func TestRenderShowsTotalsPerDimension(t *testing.T) {
	out := Render(sampleCensus(), DefaultReportOptions())
	for _, want := range []string{"overworld", "nether", "zombie", "piglin_brute"} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q", want)
		}
	}
}

func TestRenderNamesTaggedMobsAndTheirCost(t *testing.T) {
	out := Render(sampleCensus(), DefaultReportOptions())
	if !strings.Contains(out, "OJ") || !strings.Contains(out, "cat") {
		t.Error("report does not list the name-tagged cat")
	}
}

func TestRenderReportsUnreadableRecordsRatherThanHidingThem(t *testing.T) {
	c := sampleCensus()
	c.Stats.Unparsable = 3
	c.Stats.Unplaced = 2
	out := Render(c, DefaultReportOptions())
	if !strings.Contains(out, "3") || !strings.Contains(out, "unparsable") {
		t.Error("report does not surface unparsable records")
	}
	if !strings.Contains(out, "unplaced") {
		t.Error("report does not surface unplaced records")
	}
}

func TestRenderHonoursTopLimits(t *testing.T) {
	var entities []Entity
	for i := 0; i < 40; i++ {
		entities = append(entities, Entity{
			Identifier: "zombie", Dimension: Overworld,
			X: float64(i * RegionSize), Z: 0,
		})
	}
	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0).UTC(), "archive")
	out := Render(c, ReportOptions{TopRegions: 5, TopTypes: 5})
	if got := strings.Count(out, "x "); got > 6 {
		t.Errorf("report rendered %d region rows, want at most 5 plus a header", got)
	}
}

func TestRenderLocatesConcentrations(t *testing.T) {
	var entities []Entity
	for i := 0; i < 40; i++ {
		entities = append(entities, Entity{
			Identifier: "item", Dimension: Nether,
			X: -24, Y: 14, Z: 1320,
		})
	}
	c := Aggregate(entities, ScanStats{}, time.Unix(0, 0).UTC(), "archive")
	out := Render(c, DefaultReportOptions())
	if !strings.Contains(out, "concentrations") {
		t.Fatal("report has no concentrations section")
	}
	for _, want := range []string{"item", "z=1320"} {
		if !strings.Contains(out, want) {
			t.Errorf("concentration section is missing %q\n---\n%s", want, out)
		}
	}
}

func TestRenderAnEmptyCensusDoesNotPanic(t *testing.T) {
	out := Render(Census{PersistentByIdentifier: map[string]int{}}, DefaultReportOptions())
	if out == "" {
		t.Error("Render returned an empty string for an empty census")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/census/ -run TestRender -v`
Expected: FAIL, `undefined: Render`.

- [ ] **Step 3: Write minimal implementation**

```go
package census

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// ReportOptions bounds the report. A world holds thousands of regions and
// dozens of entity types; a report nobody can read gets skimmed instead.
type ReportOptions struct {
	TopRegions int
	TopTypes   int
}

func DefaultReportOptions() ReportOptions {
	return ReportOptions{TopRegions: 15, TopTypes: 20}
}

// Render writes the census as plain text.
func Render(c Census, opts ReportOptions) string {
	var b strings.Builder

	taken := "unknown"
	if !c.TakenAt.IsZero() {
		taken = c.TakenAt.UTC().Format(time.RFC3339)
	}
	fmt.Fprintf(&b, "FWB mob census\n")
	fmt.Fprintf(&b, "world taken at %s via %s\n", taken, sourceKindOrUnknown(c.SourceKind))
	fmt.Fprintf(&b, "records %d, decoded %d, unparsable %d, unplaced %d\n\n",
		c.Stats.Records, c.Stats.Decoded, c.Stats.Unparsable, c.Stats.Unplaced)

	byDimension := map[Dimension]int{}
	for _, t := range c.Totals {
		byDimension[t.Dimension] += t.Count
	}
	fmt.Fprintf(&b, "entities by dimension\n")
	for _, d := range []Dimension{Overworld, Nether, End, UnknownDimension} {
		if n, ok := byDimension[d]; ok {
			fmt.Fprintf(&b, "  %-10s %6d\n", d, n)
		}
	}

	fmt.Fprintf(&b, "\ntop entity types\n")
	for i, t := range c.Totals {
		if i >= opts.TopTypes {
			break
		}
		fmt.Fprintf(&b, "  %-10s %-24s %-12s %6d\n", t.Dimension, t.Identifier, t.Category, t.Count)
	}

	fmt.Fprintf(&b, "\nregions closest to their spawn cap (%d blocks square)\n", RegionSize)
	fmt.Fprintf(&b, "  surface and cave caps differ and the save does not record which applies,\n")
	fmt.Fprintf(&b, "  so each region is graded against the range rather than one number\n")
	// Ranked within each dimension rather than globally. The End is full of
	// end-city shulkers whose counts dwarf everything else, and a single
	// global ranking buries the overworld and nether regions a player can
	// actually do something about.
	for _, d := range []Dimension{Overworld, Nether, End, UnknownDimension} {
		shown := 0
		for _, r := range c.Regions {
			if r.Key.Dimension != d {
				continue
			}
			if shown >= opts.TopRegions {
				break
			}
			if shown == 0 {
				fmt.Fprintf(&b, "  %s\n", d)
			}
			minX, maxX, minZ, maxZ := r.Key.Bounds()
			caps, _ := CapsFor(r.Key.Dimension, r.Category)
			lower, upper := caps.Surface, caps.Cave
			if lower > upper {
				lower, upper = upper, lower
			}
			fmt.Fprintf(&b, "    x %6d..%-6d z %6d..%-6d %-12s %4d / %d..%d  %s\n",
				minX, maxX, minZ, maxZ, r.Category, r.Count, lower, upper, r.Status)
			shown++
		}
	}

	fmt.Fprintf(&b, "\nname-tagged entities (%d)\n", len(c.Named))
	fmt.Fprintf(&b, "  a name tag forces persistence, so each of these holds a cap slot forever\n")
	for _, n := range c.Named {
		fmt.Fprintf(&b, "  %-24s %-20s %-10s x=%.0f y=%.0f z=%.0f\n",
			n.Name, n.Identifier, n.Dimension, n.X, n.Y, n.Z)
	}

	if len(c.Concentrations) > 0 {
		fmt.Fprintf(&b, "\nlargest concentrations (%d or more of one type within %.0f blocks)\n",
			ConcentrationThreshold, ConcentrationRadius)
		for i, con := range c.Concentrations {
			if i >= opts.TopTypes {
				break
			}
			fmt.Fprintf(&b, "  %6d  %-22s %-10s x=%.0f y=%.0f z=%.0f\n",
				con.Cluster.Count, con.Identifier, con.Dimension,
				con.Cluster.CentreX, con.Cluster.CentreY, con.Cluster.CentreZ)
		}
	}

	fmt.Fprintf(&b, "\npersistent entities by type\n")
	type kv struct {
		identifier string
		count      int
	}
	var persistent []kv
	for id, n := range c.PersistentByIdentifier {
		persistent = append(persistent, kv{id, n})
	}
	sort.Slice(persistent, func(i, j int) bool {
		if persistent[i].count != persistent[j].count {
			return persistent[i].count > persistent[j].count
		}
		return persistent[i].identifier < persistent[j].identifier
	})
	for i, p := range persistent {
		if i >= opts.TopTypes {
			break
		}
		fmt.Fprintf(&b, "  %-24s %6d\n", p.identifier, p.count)
	}

	return b.String()
}

func sourceKindOrUnknown(kind string) string {
	if kind == "" {
		return "unknown source"
	}
	return kind
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run TestRender -v`
Expected: PASS, seven tests.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/report.go internal/census/report_test.go
go vet ./internal/census/
git add internal/census/report.go internal/census/report_test.go
git commit -m "feat(census): render the census as an operator report"
```

---

### Task 11: The census binary

**Files:**
- Create: `cmd/census/main.go`
- Test: `cmd/census/main_test.go`

**Interfaces:**
- Consumes: `ArchiveSource`, `Scan`, `Aggregate`, `Render` from Tasks 6 and 8-10.
- Produces: `func run(ctx context.Context, args []string, stdout io.Writer) error` — the testable body, with `main` a thin wrapper around it.

- [ ] **Step 1: Write the failing test**

```go
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/df-mc/goleveldb/leveldb"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// buildArchive writes a one-zombie world and tars it the way the backup job
// does, so the binary is exercised end to end rather than from a stub.
func buildArchive(t *testing.T, dir string) {
	t.Helper()
	stage := t.TempDir()
	dbPath := filepath.Join(stage, "FWB", "db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("stage world: %v", err)
	}
	db, err := leveldb.OpenFile(dbPath, nil)
	if err != nil {
		t.Fatalf("open world: %v", err)
	}
	id := make([]byte, 8)
	binary.LittleEndian.PutUint64(id, 1)
	payload, err := nbt.MarshalEncoding(map[string]any{
		"identifier": "minecraft:zombie",
		"Pos":        []any{float32(1), float32(64), float32(2)},
		"UniqueID":   int64(1),
	}, nbt.LittleEndian)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := db.Put(append([]byte("actorprefix"), id...), payload, nil); err != nil {
		t.Fatalf("put actor: %v", err)
	}
	digp := append([]byte("digp"), make([]byte, 8)...)
	if err := db.Put(digp, id, nil); err != nil {
		t.Fatalf("put digp: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close world: %v", err)
	}

	out, err := os.Create(filepath.Join(dir, "fwb-20260913T203100Z.tar.gz"))
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	if err := filepath.WalkDir(stage, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(stage, path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return err
		}
		_, err = tw.Write(body)
		return err
	}); err != nil {
		t.Fatalf("tar world: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
}

func TestRunPrintsAReportFromAnArchive(t *testing.T) {
	dir := t.TempDir()
	buildArchive(t, dir)

	var out bytes.Buffer
	if err := run(context.Background(), []string{"-backup-dir", dir}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := out.String()
	for _, want := range []string{"FWB mob census", "zombie", "2026-09-13T20:31:00Z", "archive"} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q\n---\n%s", want, got)
		}
	}
}

func TestRunFailsLoudlyWhenThereIsNoArchive(t *testing.T) {
	var out bytes.Buffer
	err := run(context.Background(), []string{"-backup-dir", t.TempDir()}, &out)
	if err == nil {
		t.Fatal("run returned nil error with no archive present")
	}
	if out.Len() != 0 {
		t.Errorf("run wrote %q to stdout on failure, want nothing", out.String())
	}
}

func TestRunRejectsUnknownFlags(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-nonsense"}, &out); err == nil {
		t.Error("run accepted an unknown flag")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/census/ -v`
Expected: FAIL — no non-test Go files in `cmd/census`, `undefined: run`.

- [ ] **Step 3: Write minimal implementation**

```go
// Command census reads a Bedrock world save and prints a population report.
//
// It runs as a CronJob beside the server rather than inside the agent: the
// scan is a batch job over hundreds of megabytes, and the agent's own pod is
// the one answering players in chat.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/jdwillmsen/minecraft-server-agent/internal/census"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "census: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable body. It writes nothing to stdout unless it produced a
// whole report: a truncated report is worse than none, because it looks like
// an answer.
func run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("census", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	backupDir := fs.String("backup-dir", "/backup", "directory holding fwb-<stamp>.tar.gz backup archives")
	topRegions := fs.Int("top-regions", census.DefaultReportOptions().TopRegions, "how many regions to list")
	topTypes := fs.Int("top-types", census.DefaultReportOptions().TopTypes, "how many entity types to list")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	source := census.ArchiveSource{Dir: *backupDir}
	world, cleanup, err := source.Open(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	entities, stats, err := census.Scan(world.DBPath)
	if err != nil {
		return err
	}

	report := census.Render(
		census.Aggregate(entities, stats, world.TakenAt, world.Kind),
		census.ReportOptions{TopRegions: *topRegions, TopTypes: *topTypes},
	)
	if _, err := io.WriteString(stdout, report); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/census/ -v`
Expected: PASS, three tests.

- [ ] **Step 5: Run the whole suite and build**

```bash
go build ./...
go vet ./...
go test ./...
```
Expected: build clean, vet clean, all packages pass.

- [ ] **Step 6: Verify against the real world**

Run the binary against a real backup archive to confirm it reproduces the hand-run audit's shape:

```bash
go run ./cmd/census -backup-dir <directory holding a real fwb-*.tar.gz>
```

Expected: a report naming the nether regions saturated with piglins and brutes, the overworld base regions far over the animal cap, the five name-tagged mobs, and the 4,147-item nether concentration near x=-24 y=14 z=1320.

This whole plan was executed once against the live FWB world before being written, so these are observed outputs rather than predictions. Two things to expect:

- **The entity total depends on whether the write-ahead log has been replayed, not on which reader you use.** A world extracted fresh from an archive reports 22,622 records; the same world after any process opens it read-write reports 22,616, because a read-write open replays and compacts the WAL. This was confirmed by running this very `Scan` over both states of the same world: the reader is identical, the directory is not. Always read a freshly extracted archive, read-only, or the number is not reproducible — which is the entire point of this tool. A difference of hundreds is a bug; a difference of single digits between a pristine and a previously-opened copy is this effect.
- **The report is only useful if it agrees with a careful manual count.** If the nether piglin regions, the overworld animal saturation, or the item concentration are absent or wildly different, stop and investigate rather than committing — reproducing that audit is the entire point of the tool.

- [ ] **Step 7: Commit**

```bash
gofmt -w cmd/census/main.go cmd/census/main_test.go
git add cmd/census/main.go cmd/census/main_test.go
git commit -m "feat(census): add the census command"
```

---

## Follow-on work, not in this plan

- `LiveSource` and the census CronJob template, both in `jdw-deployments`, once this repo publishes an image carrying `cmd/census`.
- Schema migration in `jdwlabs/platform`, then Postgres persistence, Prometheus metrics and run-to-run leak deltas.
- The in-game chat command.
- A block-palette scanner for beds and workstations, which the iron-farm diagnosis needs and no entity scan can answer.
