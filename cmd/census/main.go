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

	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "census: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable body. It writes nothing to stdout unless it produced a
// whole report: a truncated report is worse than none, because it looks like
// an answer.
func run(ctx context.Context, args []string, stdout io.Writer) (err error) {
	fs := flag.NewFlagSet("census", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	backupDir := fs.String("backup-dir", "/backup", "directory holding fwb-<stamp>.tar.gz backup archives")
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

	source := census.ArchiveSource{Dir: *backupDir}
	world, cleanup, err := source.Open(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// A silent RemoveAll failure here repeats every scheduled run and
		// slowly fills the volume with ~570MB extractions, so surface it -
		// but never let a cleanup failure mask a scan or render error that
		// already explains why the run failed.
		if cleanupErr := cleanup(); cleanupErr != nil && err == nil {
			err = fmt.Errorf("clean up extracted world %s: %w", world.Archive, cleanupErr)
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

	report := census.Render(
		census.Aggregate(entities, stats, world.TakenAt, world.Kind),
		census.ReportOptions{TopRegions: *topRegions, TopTypes: *topTypes},
	)
	if _, err := io.WriteString(stdout, report); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
