// Command census reads a Bedrock world save and prints a population report.
//
// It runs as a CronJob beside the server rather than inside the agent: the
// scan is a batch job over hundreds of megabytes, and the agent's own pod is
// the one answering players in chat.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/jdwillmsen/minecraft-server-agent/internal/census"
)

func main() {
	// A CronJob pod is terminated with SIGTERM, and the extraction and scan
	// together run for minutes. Without this the process dies where it
	// stands, before the deferred cleanup can remove the ~570MB it
	// extracted.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "census: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable body. It writes nothing to stdout unless it produced a
// whole report: a truncated report is worse than none, because it looks like
// an answer.
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

// reportFrom takes the census from an opened source. Splitting it from flag
// parsing is what lets a test supply a source whose cleanup fails.
func reportFrom(ctx context.Context, source census.Source, opts census.ReportOptions, stdout io.Writer) (err error) {
	world, cleanup, err := source.Open(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// A silent RemoveAll failure here repeats every scheduled run and
		// slowly fills the volume with ~570MB extractions, so surface it -
		// joined to whatever the run already failed with rather than
		// replacing it, because cancellation is both the likeliest reason
		// the run failed and the case the cleanup exists for.
		if cleanupErr := cleanup(); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("clean up extracted world %s: %w", world.Archive, cleanupErr))
		}
	}()

	entities, stats, scanErr := census.Scan(ctx, world.DBPath)
	if scanErr != nil {
		return fmt.Errorf("scan archive %s: %w", world.Archive, scanErr)
	}

	// Every section of a report built from records that yielded no entity
	// renders empty, and an empty report reads exactly like a quiet world.
	// Exit non-zero with the counts instead, so the CronJob goes red rather
	// than publishing a world with no mobs in it.
	if stats.Unreadable() {
		unusable := fmt.Errorf("archive %s: %d of %d actor records did not decode into a usable entity (%d unparsable, %d unplaced, %d unidentified), over the %.0f%% limit",
			world.Archive, stats.Unusable(), stats.Records,
			stats.Unparsable, stats.Unplaced, stats.Unidentified, census.MaxUnusableRatio*100)
		if stats.FirstUnparsableErr == "" {
			return unusable
		}
		return fmt.Errorf("%w; first decode failure: %s", unusable, stats.FirstUnparsableErr)
	}

	report := census.Render(census.Aggregate(entities, stats, world.TakenAt, world.Kind), opts)
	if _, err := io.WriteString(stdout, report); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
