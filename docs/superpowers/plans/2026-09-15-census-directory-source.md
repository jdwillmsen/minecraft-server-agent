# Census DirectorySource Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `cmd/census` read a world that something else has already snapshotted, falling back to the nightly archive when no snapshot was taken.

**Architecture:** A second `census.Source` reads a directory prepared by the census CronJob's init container, which drives Bedrock's save hold/query/resume sequence in shell. The Go side never speaks that protocol. `cmd/census` composes the two sources so an absent snapshot falls through to the archive while a malformed one fails loudly.

**Tech Stack:** Go 1.27, standard library only. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-15-census-cronjob-design.md` in the `jdw-deployments` repository (this plan implements only its "Agent-side change" section).

## Global Constraints

- Go 1.27 as pinned in `go.mod`. **No new dependencies** — `go.mod` and `go.sum` must be untouched by this whole plan.
- The snapshot directory belongs to whoever created it. `DirectorySource`'s cleanup function must be a no-op: the census must never delete a world it did not extract. `ArchiveSource` removes its own extraction because it made it; this one did not.
- A missing snapshot and a malformed snapshot are different outcomes and must stay distinguishable. Missing is routine (the snapshotter could not get a hold) and falls back. Malformed means something is broken and must surface, because silently reading yesterday's archive would bury it.
- Provenance is never invented. A snapshot with no readable timestamp is an error, not a fallback to `time.Now()` — the report's provenance line is the only thing stopping a stale census being believed.
- `run` writes nothing to stdout unless it produced a whole report. The existing tests assert this; keep it true.
- Run `gofmt -w` on every file you touch. `go build ./...`, `go vet ./...` and `go test ./...` must be clean.
- Commit messages follow the repo's conventional-commit style and end with:
  ```
  Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01G57aCPx9HY47pLzij6x1Tv
  ```

## File Structure

- `internal/census/source.go` — gains `ErrNoSnapshot`, `snapshotTakenAtFile` and `DirectorySource`, beside the existing `World`, `Source` and `ArchiveSource`. Sources are a family and live together; this file stays the one place a reader looks for "where do world bytes come from".
- `internal/census/source_test.go` — gains the `DirectorySource` cases.
- `cmd/census/main.go` — gains the `-world-dir` flag, a `stderr` parameter on `run`, and the composite source that picks between snapshot and archive.
- `cmd/census/main_test.go` — existing tests updated for the new `run` signature, plus the new selection cases.

---

### Task 1: DirectorySource

**Files:**
- Modify: `internal/census/source.go` (append after `ArchiveSource.Open`, before `extract`)
- Test: `internal/census/source_test.go` (append)

**Interfaces:**
- Consumes: `World` struct (fields `DBPath`, `TakenAt`, `Kind`, `Archive`), the `Source` interface, and `func findDB(root string, archive string) (string, error)` — all already in `internal/census/source.go`.
- Produces: `var ErrNoSnapshot error`; `const snapshotTakenAtFile = "snapshot-taken-at"`; `type DirectorySource struct { Dir string }` with `func (s DirectorySource) Open(ctx context.Context) (World, func() error, error)`.

- [ ] **Step 1: Write the failing test**

Append to `internal/census/source_test.go`:

```go
// writeSnapshot builds the directory shape the census CronJob's init
// container leaves behind: a world under the directory plus the provenance
// marker naming when the hold was taken.
func writeSnapshot(t *testing.T, takenAt string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "FWB", "db"), 0o755); err != nil {
		t.Fatalf("create snapshot world: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "FWB", "db", "CURRENT"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write CURRENT: %v", err)
	}
	if takenAt != "" {
		if err := os.WriteFile(filepath.Join(dir, snapshotTakenAtFile), []byte(takenAt), 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}
	return dir
}

func TestDirectorySourceOpensASnapshotWithItsOwnProvenance(t *testing.T) {
	dir := writeSnapshot(t, "2026-09-15T06:00:00Z\n")

	world, cleanup, err := DirectorySource{Dir: dir}.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cleanup()

	if world.Kind != "snapshot" {
		t.Errorf("Kind = %q, want %q", world.Kind, "snapshot")
	}
	want := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)
	if !world.TakenAt.Equal(want) {
		t.Errorf("TakenAt = %v, want %v read from the marker", world.TakenAt, want)
	}
	if filepath.Base(world.DBPath) != "db" {
		t.Errorf("DBPath = %q, want it to point at the db directory", world.DBPath)
	}
	if world.Archive == "" {
		t.Error("Archive is empty; an operator reading a Scan failure cannot tell which snapshot it was")
	}
}

func TestDirectorySourceCleanupDoesNotDeleteTheSnapshot(t *testing.T) {
	// The snapshot belongs to the init container that wrote it and is shared
	// through an emptyDir. Removing it would destroy a world this process
	// did not extract.
	dir := writeSnapshot(t, "2026-09-15T06:00:00Z")

	_, cleanup, err := DirectorySource{Dir: dir}.Open(context.Background())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "FWB", "db", "CURRENT")); err != nil {
		t.Errorf("cleanup removed the snapshot it did not create: %v", err)
	}
}

func TestDirectorySourceReportsAnAbsentSnapshotDistinguishably(t *testing.T) {
	// No marker means the snapshotter could not get a hold. That is routine,
	// and the caller falls back to an archive — so it must be tellable apart
	// from every other failure.
	dir := writeSnapshot(t, "")

	_, _, err := DirectorySource{Dir: dir}.Open(context.Background())
	if err == nil {
		t.Fatal("Open succeeded with no snapshot-taken-at marker")
	}
	if !errors.Is(err, ErrNoSnapshot) {
		t.Errorf("err = %v, want it to wrap ErrNoSnapshot", err)
	}
}

func TestDirectorySourceRefusesAnUnparsableTimestamp(t *testing.T) {
	// Present but wrong is not the same as absent. Falling back here would
	// bury a broken snapshotter behind a stale report.
	dir := writeSnapshot(t, "yesterday afternoon")

	_, _, err := DirectorySource{Dir: dir}.Open(context.Background())
	if err == nil {
		t.Fatal("Open accepted an unparsable snapshot-taken-at")
	}
	if errors.Is(err, ErrNoSnapshot) {
		t.Error("an unparsable marker reported as ErrNoSnapshot; it would silently fall back to an archive")
	}
}

func TestDirectorySourceRefusesASnapshotWithNoWorldInIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, snapshotTakenAtFile), []byte("2026-09-15T06:00:00Z"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	_, _, err := DirectorySource{Dir: dir}.Open(context.Background())
	if err == nil {
		t.Fatal("Open accepted a snapshot containing no db directory")
	}
	if errors.Is(err, ErrNoSnapshot) {
		t.Error("a marked snapshot with no world reported as ErrNoSnapshot; it would silently fall back")
	}
}
```

`source_test.go` already imports `context`, `os`, `path/filepath`, `testing` and `time`. It does **not** import `errors`, which these tests need for `errors.Is` — add it.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd ~/worktrees/minecraft-server-agent/directory-source && go test ./internal/census/ -run TestDirectorySource -v`
Expected: FAIL — the package does not compile, `undefined: DirectorySource`, `undefined: snapshotTakenAtFile`, `undefined: ErrNoSnapshot`.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/census/source.go`, after `ArchiveSource.Open` and before `func extract`:

```go
// snapshotTakenAtFile is the provenance marker an external snapshotter writes
// beside the world it copied, holding an RFC3339 time.
const snapshotTakenAtFile = "snapshot-taken-at"

// ErrNoSnapshot reports that a directory holds no snapshot at all.
//
// It is distinguishable because it is the one failure a caller should recover
// from: the census job's init container writes nothing when it cannot get a
// save hold, which is routine, and the caller then reads the nightly archive
// instead. Every other failure means the snapshot is broken, and falling back
// would hide that behind a stale report.
var ErrNoSnapshot = errors.New("no snapshot present")

// DirectorySource reads a world that something else has already snapshotted.
//
// Bedrock's save hold / save query / save resume sequence stays in the census
// job's init container, alongside the backup job that has run it for months.
// A second implementation of that protocol would be a second thing capable of
// leaving a live server unable to persist, so this reads what the shell left
// rather than speaking to the server itself.
type DirectorySource struct {
	Dir string
}

func (s DirectorySource) Open(ctx context.Context) (World, func() error, error) {
	// Nothing to clean up: this world belongs to whoever wrote it, and it
	// reaches us through a volume shared with that process. Removing it
	// would destroy a snapshot this process did not extract.
	noCleanup := func() error { return nil }

	marker := filepath.Join(s.Dir, snapshotTakenAtFile)
	raw, err := os.ReadFile(marker)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return World{}, nil, fmt.Errorf("%w: %s", ErrNoSnapshot, marker)
	case err != nil:
		return World{}, nil, fmt.Errorf("read %s: %w", marker, err)
	}

	// The marker is the only record of when this world was captured, and the
	// report's provenance line is what stops a stale census being believed.
	// Substituting the current time would forge exactly that.
	takenAt, err := time.Parse(time.RFC3339, strings.TrimSpace(string(raw)))
	if err != nil {
		return World{}, nil, fmt.Errorf("parse %s: %w", marker, err)
	}

	dbPath, err := findDB(s.Dir, s.Dir)
	if err != nil {
		return World{}, nil, err
	}
	return World{DBPath: dbPath, TakenAt: takenAt, Kind: "snapshot", Archive: s.Dir}, noCleanup, nil
}
```

`ctx` is unused here — a directory read needs no cancellation — but the parameter is what makes this satisfy `Source`.

**`internal/census/source.go` does not currently import `errors`.** It needs it for `errors.Is` and `errors.New` above; add it to the import block. Verified by compiling: without it the package fails with `undefined: errors` at both use sites.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/census/ -run TestDirectorySource -v`
Expected: PASS, five tests. Then `go test ./internal/census/` — the whole package must stay green.

- [ ] **Step 5: Commit**

```bash
gofmt -w internal/census/source.go internal/census/source_test.go
go vet ./internal/census/
git add internal/census/source.go internal/census/source_test.go
git commit -m "feat(census): read a world another process snapshotted"
```

---

### Task 2: Choose between the snapshot and the archive

**Files:**
- Modify: `cmd/census/main.go`
- Test: `cmd/census/main_test.go`

**Interfaces:**
- Consumes: `census.DirectorySource`, `census.ErrNoSnapshot` from Task 1; the existing `census.ArchiveSource`, `census.Source`, `census.World`, `census.ReportOptions`, `census.DefaultReportOptions()`, and the existing `reportFrom(ctx, source, opts, stdout)`.
- Produces: `func run(ctx context.Context, args []string, stdout, stderr io.Writer) error` (note the added `stderr`), and `type sourceChain struct` implementing `census.Source`.

- [ ] **Step 1: Write the failing test**

In `cmd/census/main_test.go`, every existing call to `run(...)` gains a fourth argument. There are **seven** of them, at these lines as the file currently stands:

| Line | Test |
|---|---|
| 138 | `TestRunPrintsAReportFromAnArchive` |
| 151 | `TestRunFailsLoudlyWhenThereIsNoArchive` |
| 162 | `TestRunRejectsUnknownFlags` |
| 174 | `TestRunRefusesToReportAWorldItCouldNotDecode` |
| 203 | `TestRunRefusesToReportAWorldWhosePositionsMoved` |
| 226 | `TestRunRefusesToReportAWorldWhoseRecordsNameNothing` |
| 246 | `TestRunRemovesTheExtractionWhenItIsCancelled` |

In each, declare a stderr buffer alongside the existing `out` and pass it. For example, line 138 becomes:

```go
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-backup-dir", dir}, &out, &errOut); err != nil {
```

Where the test already declares `var out bytes.Buffer` on its own line, widen that declaration to `var out, errOut bytes.Buffer` rather than adding a second one. Do not otherwise change those tests — their assertions are unrelated to this work.

The two `reportFrom(...)` call sites (in `TestReportFromSurfacesACleanupFailureThatCancellationWouldHide` and `TestReportFromSurfacesACleanupFailureAfterASuccessfulRun`) are **not** affected; `reportFrom`'s signature does not change.

Then append:

```go
// writeSnapshotDir builds the shape the census job's init container leaves:
// a world plus the provenance marker. An empty takenAt writes no marker,
// which is how the init container reports that it could not get a hold.
func writeSnapshotDir(t *testing.T, takenAt string) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "FWB", "db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatalf("stage snapshot: %v", err)
	}
	db, err := leveldb.OpenFile(dbPath, nil)
	if err != nil {
		t.Fatalf("open snapshot world: %v", err)
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
	if err := db.Put(append([]byte("digp"), make([]byte, 8)...), id, nil); err != nil {
		t.Fatalf("put digp: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close snapshot world: %v", err)
	}
	if takenAt != "" {
		if err := os.WriteFile(filepath.Join(dir, "snapshot-taken-at"), []byte(takenAt), 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}
	return dir
}

func TestRunPrefersTheSnapshotOverTheArchive(t *testing.T) {
	backupDir := t.TempDir()
	buildArchive(t, backupDir)
	snapshotDir := writeSnapshotDir(t, "2026-09-15T06:00:00Z")

	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{
		"-world-dir", snapshotDir, "-backup-dir", backupDir,
	}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "snapshot") {
		t.Errorf("report does not name the snapshot as its source\n---\n%s", got)
	}
	if !strings.Contains(got, "2026-09-15T06:00:00Z") {
		t.Errorf("report does not carry the snapshot's own timestamp\n---\n%s", got)
	}
}

func TestRunFallsBackToTheArchiveWhenNoSnapshotWasTaken(t *testing.T) {
	backupDir := t.TempDir()
	buildArchive(t, backupDir)
	snapshotDir := writeSnapshotDir(t, "") // no marker: the hold never happened

	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{
		"-world-dir", snapshotDir, "-backup-dir", backupDir,
	}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "archive") {
		t.Errorf("report does not name the archive as its source\n---\n%s", out.String())
	}
	if errOut.Len() == 0 {
		t.Error("nothing on stderr; the run's log does not record that the fresh path was attempted and missed")
	}
}

func TestRunFailsOnAMalformedSnapshotRatherThanFallingBack(t *testing.T) {
	backupDir := t.TempDir()
	buildArchive(t, backupDir)
	snapshotDir := writeSnapshotDir(t, "yesterday afternoon")

	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{
		"-world-dir", snapshotDir, "-backup-dir", backupDir,
	}, &out, &errOut)
	if err == nil {
		t.Fatal("run succeeded on a snapshot with an unparsable timestamp; a broken snapshotter would go unnoticed")
	}
	if out.Len() != 0 {
		t.Errorf("run wrote a report despite failing: %q", out.String())
	}
}

func TestRunRefusesWhenNeitherSourceIsGiven(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-backup-dir", ""}, &out, &errOut); err == nil {
		t.Fatal("run accepted having no world to read")
	}
}

func TestRunReadsTheSnapshotWithNoArchiveConfigured(t *testing.T) {
	snapshotDir := writeSnapshotDir(t, "2026-09-15T06:00:00Z")

	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{
		"-world-dir", snapshotDir, "-backup-dir", "",
	}, &out, &errOut); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out.String(), "snapshot") {
		t.Errorf("report does not name the snapshot as its source\n---\n%s", out.String())
	}
}
```

`main_test.go` already imports everything these tests need — `bytes`, `context`, `encoding/binary`, `errors`, `os`, `path/filepath`, `strings`, `testing`, plus `leveldb` and `nbt`. No import changes are required in this file. `buildArchive(t *testing.T, dir string)` also already exists here; call it, do not redefine it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/census/ -v`
Expected: FAIL — `run` takes 3 arguments, not 4; `undefined: writeSnapshotDir` resolves once added, but the signature mismatch stops compilation.

- [ ] **Step 3: Write minimal implementation**

In `cmd/census/main.go`, change `main` to pass stderr:

```go
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "census: %v\n", err)
		os.Exit(1)
	}
```

Change `run`'s signature and body. The flag block gains `-world-dir`; the help branch still prints to stdout; the tail builds the source chain:

```go
func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("census", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	worldDir := fs.String("world-dir", "", "directory holding a snapshot written by the census job's init container; preferred over -backup-dir when it holds one")
	backupDir := fs.String("backup-dir", "/backup", "directory holding fwb-<stamp>.tar.gz backup archives; read when -world-dir holds no snapshot")
	topRegions := fs.Int("top-regions", census.DefaultReportOptions().TopRegions, "how many regions to list")
	topTypes := fs.Int("top-types", census.DefaultReportOptions().TopTypes, "how many entity types to list")
	if parseErr := fs.Parse(args); parseErr != nil {
		if errors.Is(parseErr, flag.ErrHelp) {
			// -h/-help is a request for usage, not a failure: it should
			// print to stdout and exit 0 like any other well-behaved CLI.
			fs.SetOutput(stdout)
			fs.PrintDefaults()
			return nil
		}
		return fmt.Errorf("parse flags: %w", parseErr)
	}

	source, err := chooseSource(*worldDir, *backupDir, stderr)
	if err != nil {
		return err
	}
	return reportFrom(ctx, source,
		census.ReportOptions{TopRegions: *topRegions, TopTypes: *topTypes}, stdout)
}

// chooseSource assembles where the world comes from.
//
// The CronJob supplies both flags on every run: its init container writes a
// snapshot when it can get a save hold and writes nothing when it cannot, so
// having both set is the normal operating mode rather than a mistake.
func chooseSource(worldDir, backupDir string, stderr io.Writer) (census.Source, error) {
	switch {
	case worldDir == "" && backupDir == "":
		return nil, errors.New("no world to read: give -world-dir, -backup-dir, or both")
	case worldDir == "":
		return census.ArchiveSource{Dir: backupDir}, nil
	case backupDir == "":
		return census.DirectorySource{Dir: worldDir}, nil
	default:
		return sourceChain{
			snapshot: census.DirectorySource{Dir: worldDir},
			archive:  census.ArchiveSource{Dir: backupDir},
			stderr:   stderr,
		}, nil
	}
}

// sourceChain reads the fresh snapshot when there is one and last night's
// archive when there is not.
//
// Only an absent snapshot falls through. A snapshot that is present but
// malformed fails the run: reading the archive instead would leave a broken
// snapshotter producing plausible reports indefinitely.
type sourceChain struct {
	snapshot census.DirectorySource
	archive  census.ArchiveSource
	stderr   io.Writer
}

func (c sourceChain) Open(ctx context.Context) (census.World, func() error, error) {
	world, cleanup, err := c.snapshot.Open(ctx)
	if err == nil {
		return world, cleanup, nil
	}
	if !errors.Is(err, census.ErrNoSnapshot) {
		return census.World{}, nil, err
	}
	// The report's provenance line will say it read an archive, but not that
	// a fresh snapshot was attempted and missed. That difference is what
	// tells an operator the snapshotter is failing rather than disabled.
	fmt.Fprintf(c.stderr, "census: %v; reading the newest archive instead\n", err)
	return c.archive.Open(ctx)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cmd/census/ -v`
Expected: PASS — the three existing tests plus five new ones.

- [ ] **Step 5: Run the whole repo**

```bash
go build ./...
go vet ./...
go test ./...
```
Expected: build clean, vet clean, every package passing.

- [ ] **Step 6: Commit**

```bash
gofmt -w cmd/census/main.go cmd/census/main_test.go
git add cmd/census/main.go cmd/census/main_test.go
git commit -m "feat(census): prefer a fresh snapshot, fall back to the archive"
```

---

## Follow-on work, not in this plan

- The census CronJob, its snapshot init container and its RBAC, all in `jdw-deployments`. That plan depends on an agent release carrying `cmd/census` with this change.
- Postgres persistence, Prometheus metrics and leak deltas, which need the `jdwillmsen-schemas` migration in `jdwlabs/platform` first.
- The in-game chat command.
