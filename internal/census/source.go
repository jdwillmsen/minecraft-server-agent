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

	dbPath, err := findDB(root, newest)
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
			return fmt.Errorf("extract %s: %w", archive, err)
		}
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", archive, err)
		}

		// A tar entry that names an absolute path or climbs out of the archive
		// root means the archive is not what it claims to be. Refuse the whole
		// extraction rather than quietly rewriting the path to something safe:
		// continuing past that is how a surprise becomes a write nobody
		// reviewed.
		cleaned := filepath.Clean(header.Name)
		if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("archive %s contains an entry escaping the extraction root: %q", archive, header.Name)
		}
		target := filepath.Join(root, cleaned)
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
func findDB(root string, archive string) (string, error) {
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
		return "", fmt.Errorf("no db directory inside archive %s", archive)
	}
	return found, nil
}
