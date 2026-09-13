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
