package census

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	if world.Archive != "fwb-20260913T000000Z.tar.gz" {
		t.Errorf("Archive = %q, want the newest archive's filename", world.Archive)
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
	testCases := []string{
		"../escaped",
		"../../etc/passwd",
		"/etc/passwd",
	}

	for _, payload := range testCases {
		t.Run(payload, func(t *testing.T) {
			dir := t.TempDir()
			// Include a valid world so findDB succeeds; the ONLY reason
			// Open should fail is the traversal check.
			writeArchive(t, dir, "fwb-20260913T000000Z.tar.gz", map[string]string{
				"FWB/db/CURRENT": "valid",
				payload:          "x",
			})
			_, cleanup, err := (ArchiveSource{Dir: dir}).Open(context.Background())
			if err == nil {
				cleanup()
				t.Errorf("Open accepted an archive containing %q", payload)
			}
			if !strings.Contains(err.Error(), "escaping the extraction root") {
				t.Errorf("error did not mention escaping: %v", err)
			}
		})
	}
}

func TestArchiveSourceAcceptsBenignArchives(t *testing.T) {
	// Prove that valid archives pass the traversal check.
	dir := t.TempDir()
	writeArchive(t, dir, "fwb-20260913T000000Z.tar.gz", map[string]string{
		"FWB/db/CURRENT": "valid",
	})
	_, cleanup, err := (ArchiveSource{Dir: dir}).Open(context.Background())
	if err != nil {
		t.Fatalf("Open of benign archive failed: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// writeBackupArchive writes an archive with the member shape the backup job
// actually produces. `tar czf "$ARCHIVE_TMP" -C "$STAGE" .` emits "./" as
// member 0 and a directory entry for every directory ahead of its files;
// both GNU tar and the busybox tar in the backup image do. An archive built
// from regular-file members alone never exercises those entries.
func writeBackupArchive(t *testing.T, dir, name string, files map[string]string) string {
	t.Helper()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	headers := []*tar.Header{{Name: "./", Typeflag: tar.TypeDir, Mode: 0o755}}
	seen := map[string]bool{"./": true}
	for _, p := range paths {
		segments := strings.Split(p, "/")
		prefix := "./"
		for _, segment := range segments[:len(segments)-1] {
			prefix += segment + "/"
			if seen[prefix] {
				continue
			}
			seen[prefix] = true
			headers = append(headers, &tar.Header{Name: prefix, Typeflag: tar.TypeDir, Mode: 0o755})
		}
		headers = append(headers, &tar.Header{
			Name: "./" + p, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(files[p])),
		})
	}

	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create archive: %v", err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, header := range headers {
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("tar header %s: %v", header.Name, err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if _, err := tw.Write([]byte(files[strings.TrimPrefix(header.Name, "./")])); err != nil {
			t.Fatalf("tar body %s: %v", header.Name, err)
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

func TestArchiveSourceAcceptsTheShapeTheBackupJobWrites(t *testing.T) {
	dir := t.TempDir()
	writeBackupArchive(t, dir, "fwb-20260913T000000Z.tar.gz", map[string]string{"FWB/db/CURRENT": "valid"})

	world, cleanup, err := ArchiveSource{Dir: dir}.Open(context.Background())
	if err != nil {
		t.Fatalf("Open of a real backup archive failed: %v", err)
	}
	defer cleanup()

	body, err := os.ReadFile(filepath.Join(world.DBPath, "CURRENT"))
	if err != nil {
		t.Fatalf("read extracted world: %v", err)
	}
	if string(body) != "valid" {
		t.Errorf("extracted %q, want %q", body, "valid")
	}
}

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

func TestDirectorySourceTreatsAMissingDirectoryAsMalformed(t *testing.T) {
	// A volume that never mounted, a renamed path or a typo in the chart is
	// not a snapshotter declining to take a hold. Reported as absent it
	// would fall back to the archive and stay green forever.
	missing := filepath.Join(t.TempDir(), "never-mounted")

	_, _, err := DirectorySource{Dir: missing}.Open(context.Background())
	if err == nil {
		t.Fatal("Open succeeded on a directory that does not exist")
	}
	if errors.Is(err, ErrNoSnapshot) {
		t.Error("a missing directory reported as ErrNoSnapshot; it is indistinguishable from a routine missed hold")
	}
}

func TestDirectorySourceTreatsAFileInPlaceOfTheDirectoryAsMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "world")
	if err := os.WriteFile(path, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, _, err := DirectorySource{Dir: path}.Open(context.Background())
	if err == nil {
		t.Fatal("Open succeeded on a path that is not a directory")
	}
	if errors.Is(err, ErrNoSnapshot) {
		t.Error("a non-directory reported as ErrNoSnapshot; it would silently fall back")
	}
}

func TestDirectorySourceReportsAnUnreadableMarkerDistinguishably(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: file modes do not deny access")
	}
	dir := writeSnapshot(t, "2026-09-15T06:00:00Z")
	if err := os.Chmod(filepath.Join(dir, snapshotTakenAtFile), 0); err != nil {
		t.Fatalf("chmod marker: %v", err)
	}

	_, _, err := DirectorySource{Dir: dir}.Open(context.Background())
	if err == nil {
		t.Fatal("Open succeeded on a marker it could not read")
	}
	if errors.Is(err, ErrNoSnapshot) {
		t.Error("an unreadable marker reported as ErrNoSnapshot; a broken volume would fall back silently")
	}
}

func TestDirectorySourceRefusesASnapshotHoldingMoreThanOneWorld(t *testing.T) {
	// The snapshot directory is a volume another process owns, not a tree
	// this process just built, so a leftover world can sit beside the fresh
	// one. Taking the first by name order would report the leftover's mobs
	// under the fresh marker's timestamp.
	dir := writeSnapshot(t, "2026-09-15T06:00:00Z")
	if err := os.MkdirAll(filepath.Join(dir, "AAA-stale", "db"), 0o755); err != nil {
		t.Fatalf("create leftover world: %v", err)
	}

	_, _, err := DirectorySource{Dir: dir}.Open(context.Background())
	if err == nil {
		t.Fatal("Open chose one of two worlds instead of refusing")
	}
	if errors.Is(err, ErrNoSnapshot) {
		t.Error("two worlds reported as ErrNoSnapshot; it would silently fall back")
	}
	for _, want := range []string{"AAA-stale", "FWB"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name the %s world: %v", want, err)
		}
	}
}

func TestDirectorySourceStopsOnACancelledContext(t *testing.T) {
	// The walk for the world is over an operator-supplied directory of
	// unknown size, and it is the one unbounded step on this path.
	dir := writeSnapshot(t, "2026-09-15T06:00:00Z")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := DirectorySource{Dir: dir}.Open(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Open of a cancelled context returned %v, want it to wrap context.Canceled", err)
	}
}
