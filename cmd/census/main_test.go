package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/df-mc/goleveldb/leveldb"
	"github.com/jdwillmsen/minecraft-server-agent/internal/census"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// zombieRecord is the actorprefix value of a single placed, named mob.
func zombieRecord(t *testing.T) []byte {
	t.Helper()
	payload, err := nbt.MarshalEncoding(map[string]any{
		"identifier": "minecraft:zombie",
		"Pos":        []any{float32(1), float32(64), float32(2)},
		"UniqueID":   int64(1),
	}, nbt.LittleEndian)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return payload
}

// stageWorld writes a world holding one actor record and returns the
// directory an archive of it would extract to.
func stageWorld(t *testing.T, actorRecord []byte) string {
	t.Helper()
	stage := t.TempDir()
	if err := os.MkdirAll(worldDB(stage), 0o755); err != nil {
		t.Fatalf("stage world: %v", err)
	}
	writeWorld(t, worldDB(stage), actorRecord)
	return stage
}

func worldDB(stage string) string { return filepath.Join(stage, "FWB", "db") }

// buildArchive writes a one-zombie world and tars it the way the backup job
// does - `tar czf "$ARCHIVE_TMP" -C "$STAGE" .`, so "./" is member 0 and
// every directory gets an entry ahead of its files - so the binary is
// exercised end to end against the archive shape it actually receives.
func buildArchive(t *testing.T, dir string) {
	t.Helper()
	buildArchiveFromRecord(t, dir, zombieRecord(t))
}

// buildArchiveFromRecord builds that archive around a caller-supplied
// actorprefix value, so a test can stand in bytes the decoder will refuse.
func buildArchiveFromRecord(t *testing.T, dir string, actorRecord []byte) {
	t.Helper()
	stage := stageWorld(t, actorRecord)
	tarWorld(t, stage, dir)
}

func writeWorld(t *testing.T, dbPath string, actorRecord []byte) {
	t.Helper()
	db, err := leveldb.OpenFile(dbPath, nil)
	if err != nil {
		t.Fatalf("open world: %v", err)
	}
	id := make([]byte, 8)
	binary.LittleEndian.PutUint64(id, 1)
	if err := db.Put(append([]byte("actorprefix"), id...), actorRecord, nil); err != nil {
		t.Fatalf("put actor: %v", err)
	}
	digp := append([]byte("digp"), make([]byte, 8)...)
	if err := db.Put(digp, id, nil); err != nil {
		t.Fatalf("put digp: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close world: %v", err)
	}
}

func tarWorld(t *testing.T, stage, dir string) {
	t.Helper()
	out, err := os.Create(filepath.Join(dir, "fwb-20260913T203100Z.tar.gz"))
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer out.Close()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	if err := filepath.WalkDir(stage, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(stage, path)
		if err != nil {
			return err
		}
		name := "./" + filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == "." {
				name = "./"
			} else {
				name += "/"
			}
			return tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeDir, Mode: 0o755})
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body)),
		}); err != nil {
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

	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-backup-dir", dir}, &out, &errOut); err != nil {
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
	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"-backup-dir", t.TempDir()}, &out, &errOut)
	if err == nil {
		t.Fatal("run returned nil error with no archive present")
	}
	if out.Len() != 0 {
		t.Errorf("run wrote %q to stdout on failure, want nothing", out.String())
	}
}

func TestRunRejectsUnknownFlags(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-nonsense"}, &out, &errOut); err == nil {
		t.Error("run accepted an unknown flag")
	}
}

func TestRunRefusesToReportAWorldItCouldNotDecode(t *testing.T) {
	// Every section of a report built from nothing renders empty, which is
	// indistinguishable from a quiet world. The run must fail instead.
	dir := t.TempDir()
	buildArchiveFromRecord(t, dir, []byte{0xff, 0xff, 0xff})

	var out, errOut bytes.Buffer
	err := run(context.Background(), []string{"-backup-dir", dir}, &out, &errOut)
	if err == nil {
		t.Fatal("run returned nil error for a world whose every record failed to decode")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error does not say records failed to decode: %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("run wrote %q to stdout, want no report at all", out.String())
	}
}

func TestRunRefusesToReportAWorldWhosePositionsMoved(t *testing.T) {
	// The failure a Bedrock layout change actually produces: the NBT still
	// decodes, so nothing is unparsable, but Pos is no longer three
	// float32s and not one record can be placed in a region. Every section
	// of the report renders empty and the job would otherwise exit 0.
	payload, err := nbt.MarshalEncoding(map[string]any{
		"identifier": "minecraft:zombie",
		"Pos":        []any{float64(1), float64(64), float64(2)},
		"UniqueID":   int64(1),
	}, nbt.LittleEndian)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dir := t.TempDir()
	buildArchiveFromRecord(t, dir, payload)

	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-backup-dir", dir}, &out, &errOut); err == nil {
		t.Fatal("run returned nil error for a world where no record could be placed")
	}
	if out.Len() != 0 {
		t.Errorf("run wrote %q to stdout, want no report at all", out.String())
	}
}

func TestRunRefusesToReportAWorldWhoseRecordsNameNothing(t *testing.T) {
	// The variant that escapes a placement check entirely: every record
	// decodes and places, and the report lists entities with a blank
	// identifier, no graded regions and an exit code of 0.
	payload, err := nbt.MarshalEncoding(map[string]any{
		"Pos":      []any{float32(1), float32(64), float32(2)},
		"UniqueID": int64(1),
	}, nbt.LittleEndian)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	dir := t.TempDir()
	buildArchiveFromRecord(t, dir, payload)

	var out, errOut bytes.Buffer
	if err := run(context.Background(), []string{"-backup-dir", dir}, &out, &errOut); err == nil {
		t.Fatal("run returned nil error for a world where no record named an entity")
	}
	if out.Len() != 0 {
		t.Errorf("run wrote %q to stdout, want no report at all", out.String())
	}
}

func TestRunRemovesTheExtractionWhenItIsCancelled(t *testing.T) {
	// The pod can be terminated part way through a multi-minute extraction,
	// and what must not survive it is the ~570MB tree on the backup volume.
	dir := t.TempDir()
	buildArchive(t, dir)
	extractions := t.TempDir()
	t.Setenv("TMPDIR", extractions)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out, errOut bytes.Buffer
	if err := run(ctx, []string{"-backup-dir", dir}, &out, &errOut); !errors.Is(err, context.Canceled) {
		t.Fatalf("run of a cancelled context returned %v, want context.Canceled", err)
	}
	left, err := os.ReadDir(extractions)
	if err != nil {
		t.Fatalf("read temp directory: %v", err)
	}
	if len(left) != 0 {
		t.Errorf("run left %d entries behind in the temp directory, want none", len(left))
	}
	if out.Len() != 0 {
		t.Errorf("run wrote %q to stdout, want no report at all", out.String())
	}
}

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

// stubSource stands in for the archive source so a test can control what the
// cleanup it hands back does.
type stubSource struct {
	world   census.World
	cleanup func() error
}

func (s stubSource) Open(context.Context) (census.World, func() error, error) {
	return s.world, s.cleanup, nil
}

func TestReportFromSurfacesACleanupFailureThatCancellationWouldHide(t *testing.T) {
	// Cancellation is the case the cleanup exists for: the pod is going
	// away and the ~570MB extraction has to go with it. Reporting the
	// cleanup failure only when everything else succeeded stayed silent in
	// exactly the run where it mattered.
	stage := stageWorld(t, zombieRecord(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out bytes.Buffer
	err := reportFrom(ctx, stubSource{
		world:   census.World{DBPath: worldDB(stage), Kind: "archive", Archive: "fwb-20260913T203100Z.tar.gz"},
		cleanup: func() error { return errors.New("device or resource busy") },
	}, census.DefaultReportOptions(), &out)

	if !errors.Is(err, context.Canceled) {
		t.Errorf("reportFrom of a cancelled context returned %v, want it to still wrap context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "device or resource busy") {
		t.Errorf("error hides the cleanup failure: %v", err)
	}
	if !strings.Contains(err.Error(), "fwb-20260913T203100Z.tar.gz") {
		t.Errorf("error does not name the archive left extracted: %v", err)
	}
}

func TestReportFromSurfacesACleanupFailureAfterASuccessfulRun(t *testing.T) {
	stage := stageWorld(t, zombieRecord(t))

	var out bytes.Buffer
	err := reportFrom(context.Background(), stubSource{
		world:   census.World{DBPath: worldDB(stage), Kind: "archive", Archive: "fwb-20260913T203100Z.tar.gz"},
		cleanup: func() error { return errors.New("device or resource busy") },
	}, census.DefaultReportOptions(), &out)

	if err == nil || !strings.Contains(err.Error(), "device or resource busy") {
		t.Errorf("reportFrom returned %v, want the cleanup failure", err)
	}
}
