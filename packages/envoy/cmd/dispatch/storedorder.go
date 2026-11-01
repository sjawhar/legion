package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// artifactFlags is the repeatable --artifact flag: each value a document's id.
type artifactFlags []string

func (a *artifactFlags) String() string { return strings.Join(*a, ",") }

func (a *artifactFlags) Set(value string) error {
	id, err := uuid.Parse(value)
	if err != nil {
		return fmt.Errorf("%q is not a document id", value)
	}
	*a = append(*a, id.String())
	return nil
}

// storedOrder is `envoy-dispatch stored-order`: whether each document's stored updates, folded in
// version order, give every stored class and every version's markdown (docs.CheckStoredOrder). It
// checks the documents --artifact names, or every document, and with --since only the updates and
// versions created at or after that time, folding the older ones unchecked. It reads in read-only
// transactions, migrates nothing and writes nothing, and prints one line per document and one per
// mismatch: counts, classes, versions, md5s and lengths, never document text. A version created
// inside the window recording an update a compaction has folded into the base is counted
// (versions_compacted) and compared with nothing. A document whose check compared no update and
// no version, one whose window versions all record such updates included, is unchecked, never
// clean: one --artifact names fails the run, and a run over every document fails when it checked
// none, so exit 0 always means the check looked. Exit 0 when every document replays, 1 on any
// mismatch, refused document, failure to check or unchecked named document, 2 on a usage error.
func storedOrder(ctx context.Context, args []string, databaseURL string, out io.Writer) int {
	flags := flag.NewFlagSet("stored-order", flag.ContinueOnError)
	flags.SetOutput(out)
	var artifacts artifactFlags
	flags.Var(&artifacts, "artifact", "a document id to check (repeatable); every document when none is given")
	since := flags.String("since", "", "an RFC 3339 time: updates and versions created before it are folded unchecked")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(out, "stored-order: unexpected argument %q; the flags are --artifact and --since\n", flags.Arg(0))
		return 2
	}
	var from time.Time
	if *since != "" {
		parsed, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			fmt.Fprintf(out, "stored-order: --since %q is not an RFC 3339 time\n", *since)
			return 2
		}
		from = parsed
	}
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(out, "stored-order: DATABASE_URL is required")
		return 1
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(out, "stored-order: open database: %v\n", err)
		return 1
	}
	defer database.Pool.Close()
	ids := []string(artifacts)
	if len(ids) == 0 {
		if ids, err = docs.StoredOrderDocuments(ctx, database.Pool); err != nil {
			fmt.Fprintf(out, "stored-order: %v\n", err)
			return 1
		}
	}
	exitCode := 0
	var clean, mismatched, refused, failed, unchecked int
	for _, id := range ids {
		report, err := docs.CheckStoredOrder(ctx, database.Pool, id, from)
		if err != nil {
			fmt.Fprintf(out, "stored-order: %s error (%v)\n", id, err)
			failed++
			exitCode = 1
			continue
		}
		writeStoredOrderReport(out, report)
		switch {
		case report.Refused != "":
			refused++
		case len(report.Mismatches) > 0:
			mismatched++
		case !report.Checked():
			unchecked++
		default:
			clean++
		}
		if !report.Clean() && (report.Checked() || report.Refused != "" || len(artifacts) > 0) {
			exitCode = 1
		}
	}
	if clean+mismatched == 0 {
		// No document had an update or version to compare: the run proves nothing, an empty
		// database included.
		exitCode = 1
	}
	fmt.Fprintf(out, "stored-order: documents=%d clean=%d mismatched=%d refused=%d failed=%d unchecked=%d\n", len(ids), clean, mismatched, refused, failed, unchecked)
	return exitCode
}

func writeStoredOrderReport(out io.Writer, report docs.StoredOrderReport) {
	fmt.Fprintf(out, "stored-order: %s base=%d updates_checked=%d updates_skipped=%d versions_checked=%d versions_skipped=%d versions_compacted=%d mismatches=%d",
		report.ArtifactID, report.Base, report.UpdatesChecked, report.UpdatesSkipped,
		report.VersionsChecked, report.VersionsSkipped, report.VersionsCompacted, len(report.Mismatches))
	if report.VersionsCompacted > 0 {
		fmt.Fprintf(out, " compacted=%q", fmt.Sprintf("%d versions record updates below the compaction base %d and were not compared",
			report.VersionsCompacted, report.Base))
	}
	if report.Refused != "" {
		fmt.Fprintf(out, " refused=%q", report.Refused)
	}
	fmt.Fprintln(out)
	for _, mismatch := range report.Mismatches {
		fmt.Fprintf(out, "stored-order: %s %s\n", report.ArtifactID, mismatch)
	}
}
