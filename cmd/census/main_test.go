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
	"github.com/sandertv/gophertunnel/minecraft/nbt"
)

// buildArchive writes a one-zombie world and tars it the way the backup job
// does - `tar czf "$ARCHIVE_TMP" -C "$STAGE" .`, so "./" is member 0 and
// every directory gets an entry ahead of its files - so the binary is
// exercised end to end against the archive shape it actually receives.
func buildArchive(t *testing.T, dir string) {
	t.Helper()
	payload, err := nbt.MarshalEncoding(map[string]any{
		"identifier": "minecraft:zombie",
		"Pos":        []any{float32(1), float32(64), float32(2)},
		"UniqueID":   int64(1),
	}, nbt.LittleEndian)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	buildArchiveFromRecord(t, dir, payload)
}

// buildArchiveFromRecord builds that archive around a caller-supplied
// actorprefix value, so a test can stand in bytes the decoder will refuse.
func buildArchiveFromRecord(t *testing.T, dir string, actorRecord []byte) {
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

func TestRunRefusesToReportAWorldItCouldNotDecode(t *testing.T) {
	// Every section of a report built from nothing renders empty, which is
	// indistinguishable from a quiet world. The run must fail instead.
	dir := t.TempDir()
	buildArchiveFromRecord(t, dir, []byte{0xff, 0xff, 0xff})

	var out bytes.Buffer
	err := run(context.Background(), []string{"-backup-dir", dir}, &out)
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

func TestRunRemovesTheExtractionWhenItIsCancelled(t *testing.T) {
	// The pod can be terminated part way through a multi-minute extraction,
	// and what must not survive it is the ~570MB tree on the backup volume.
	dir := t.TempDir()
	buildArchive(t, dir)
	extractions := t.TempDir()
	t.Setenv("TMPDIR", extractions)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var out bytes.Buffer
	if err := run(ctx, []string{"-backup-dir", dir}, &out); !errors.Is(err, context.Canceled) {
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
