package census

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
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
