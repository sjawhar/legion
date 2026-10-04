package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/api"
	"github.com/sjawhar/envoy/internal/dispatch/config"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/embedqueue"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// subcommand is an argument envoy-dispatch takes in place of serving. run gets the arguments
// after the name and what the settings table read.
type subcommand struct {
	name string
	run  func(ctx context.Context, args []string, env settingValues, stdout, stderr io.Writer) int
}

// subcommands are every argument envoy-dispatch takes in place of serving: runSubcommand both
// dispatches on this table and names it, in this order, when it refuses an argument.
var subcommands = []subcommand{
	{"backfill-block-ids", func(ctx context.Context, _ []string, env settingValues, stdout, _ io.Writer) int {
		return backfillBlockIDs(ctx, env.get("DATABASE_URL"), stdout)
	}},
	{"backfill-anchor-blocks", func(ctx context.Context, _ []string, env settingValues, stdout, _ io.Writer) int {
		return backfillAnchorBlocks(ctx, env.get("DATABASE_URL"), stdout)
	}},
	{"rebuild-refs", func(ctx context.Context, _ []string, env settingValues, stdout, _ io.Writer) int {
		return rebuildRefs(ctx, env.get("DATABASE_URL"), loadServerURL(env), stdout)
	}},
	{"redeliver-webhooks", func(ctx context.Context, args []string, env settingValues, stdout, _ io.Writer) int {
		return redeliverWebhooks(ctx, args, env, stdout)
	}},
	{"backfill-embeddings", func(ctx context.Context, _ []string, env settingValues, stdout, _ io.Writer) int {
		return backfillEmbeddings(ctx, env.get("DATABASE_URL"), env.get("COHERE_API_KEY"), stdout)
	}},
	{"census", func(ctx context.Context, _ []string, env settingValues, stdout, stderr io.Writer) int {
		return census(ctx, env.get("DATABASE_URL"), stdout, stderr)
	}},
	{"backfill-files", func(ctx context.Context, args []string, env settingValues, stdout, _ io.Writer) int {
		return backfillFiles(ctx, args, env.get("DATABASE_URL"), strings.TrimSpace(env.get("DISPATCH_FILE_STORE_BUCKET")), stdout)
	}},
	{"settings", func(_ context.Context, _ []string, _ settingValues, stdout, stderr io.Writer) int {
		if err := writeSettings(stdout); err != nil {
			fmt.Fprintf(stderr, "settings: %v\n", err)
			return 1
		}
		return 0
	}},
	{"routes", func(_ context.Context, _ []string, _ settingValues, stdout, stderr io.Writer) int {
		if err := api.WriteRouteIndex(stdout); err != nil {
			fmt.Fprintf(stderr, "routes: %v\n", err)
			return 1
		}
		return 0
	}},
}

// runSubcommand runs the subcommand args name and returns its exit code. A name it does not know
// is refused with exit 2 rather than falling through to the server: the server migrates the
// database at boot, so a deployment running `envoy-dispatch census` on an image that predates
// the subcommand must get a refusal, never a boot that applies the migrations the census was to
// inspect.
func runSubcommand(ctx context.Context, args []string, env settingValues, stdout, stderr io.Writer) int {
	names := make([]string, len(subcommands))
	for i, sub := range subcommands {
		if sub.name == args[0] {
			return sub.run(ctx, args[1:], env, stdout, stderr)
		}
		names[i] = sub.name
	}
	fmt.Fprintf(stderr, "envoy-dispatch: unknown subcommand %q; the subcommands are %s, and envoy-dispatch with no argument serves\n", args[0], strings.Join(names, ", "))
	return 2
}

// openMigrated opens the database a DB subcommand works on and brings it to the current
// schema, reporting each failure on out under the subcommand's label. The caller closes the
// returned store's pool.
func openMigrated(ctx context.Context, label, databaseURL string, out io.Writer) (*store.Store, bool) {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(out, label+": DATABASE_URL is required")
		return nil, false
	}
	database, err := store.Open(ctx, databaseURL)
	if err != nil {
		fmt.Fprintf(out, "%s: open database: %v\n", label, err)
		return nil, false
	}
	if err := database.Migrate(ctx); err != nil {
		fmt.Fprintf(out, "%s: migrate database: %v\n", label, err)
		database.Pool.Close()
		return nil, false
	}
	return database, true
}

func backfillBlockIDs(ctx context.Context, databaseURL string, out io.Writer) int {
	database, ok := openMigrated(ctx, "backfill-block-ids", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	service := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	defer service.Shutdown(context.Background())
	reports, err := service.BackfillBlockIDs(ctx)
	if err != nil {
		fmt.Fprintf(out, "backfill-block-ids: %v\n", err)
		return 1
	}
	exitCode := 0
	for _, report := range reports {
		if !writeBlockIDBackfillReport(out, report) {
			exitCode = 1
		}
	}
	return exitCode
}

func backfillAnchorBlocks(ctx context.Context, databaseURL string, out io.Writer) int {
	database, ok := openMigrated(ctx, "backfill-anchor-blocks", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	service := docs.New(docs.Deps{Store: database, Events: events.NewBroker()})
	defer service.Shutdown(context.Background())
	reports, err := service.BackfillBlockIDs(ctx)
	if err != nil {
		fmt.Fprintf(out, "backfill-anchor-blocks: stamp document blocks: %v\n", err)
		return 1
	}
	for _, report := range reports {
		if report.Err != nil {
			fmt.Fprintf(out, "backfill-anchor-blocks: stamp document %s: %v\n", report.ArtifactID, report.Err)
			return 1
		}
	}
	result, err := service.BackfillAnchorBlocks(ctx)
	if err != nil {
		fmt.Fprintf(out, "backfill-anchor-blocks: %v\n", err)
		return 1
	}
	writeAnchorBlockBackfillReport(out, result)
	return 0
}

// backfillFiles moves every uploaded file's bytes from Postgres into the bucket
// DISPATCH_FILE_STORE_BUCKET names (files.BackfillRows); with --verify-only it reads back every
// file already moved and checks it against its row's hash, moving nothing (files.VerifyRows); with
// --restore, the rollback, it writes every moved file's bytes back into its row from the bucket
// (files.RestoreRows). Each pass names the rows it could not do and goes on past them, exits 1
// when any failed, and can be run again at any time; the server serves a row from its bytes
// whenever it holds them.
func backfillFiles(ctx context.Context, args []string, databaseURL, bucket string, out io.Writer) int {
	flags := flag.NewFlagSet("backfill-files", flag.ContinueOnError)
	flags.SetOutput(out)
	verifyOnly := flags.Bool("verify-only", false, "read back every file already moved and check it against its row's hash, moving nothing")
	restore := flags.Bool("restore", false, "write every moved file's bytes back into its row from the bucket: the rollback")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(out, "backfill-files: unexpected argument %q; the flags are --verify-only and --restore\n", flags.Arg(0))
		return 2
	}
	if *verifyOnly && *restore {
		fmt.Fprintln(out, "backfill-files: --verify-only and --restore cannot both be set")
		return 2
	}
	if bucket == "" {
		fmt.Fprintln(out, "backfill-files: DISPATCH_FILE_STORE_BUCKET is required")
		return 1
	}
	database, ok := openMigrated(ctx, "backfill-files", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	fileStore, err := files.NewS3(ctx, bucket)
	if err != nil {
		fmt.Fprintf(out, "backfill-files: %v\n", err)
		return 1
	}
	if err := fileStore.Healthy(ctx); err != nil {
		fmt.Fprintf(out, "backfill-files: the bucket is not reachable: %v\n", err)
		return 1
	}
	pass, verb := files.BackfillRows, "moved"
	switch {
	case *verifyOnly:
		pass, verb = files.VerifyRows, "verified"
	case *restore:
		pass, verb = files.RestoreRows, "restored"
	}
	report, err := pass(ctx, database.Pool, fileStore, out)
	fmt.Fprintf(out, "backfill-files: %s=%d bytes=%d failed=%d\n", verb, report.Done, report.Bytes, len(report.Failed))
	if err != nil {
		fmt.Fprintf(out, "backfill-files: stopped: %v\n", err)
		return 1
	}
	if len(report.Failed) > 0 {
		return 1
	}
	return 0
}

// loadServerURL resolves the dashboard origin exactly as the server does, for a subcommand
// that parses reference text.
func loadServerURL(env settingValues) string {
	envoyConfig, err := loadEnvoyConfig(env, config.LoadOptions{})
	if err != nil {
		slog.Error("dispatch: load envoy config", "error", err)
		os.Exit(1)
	}
	if envoyConfig.Dispatch == nil {
		return ""
	}
	return envoyConfig.Dispatch.ServerURL
}

// rebuildRefs reparses every reference source and reconciles the refs index with it. It
// refuses an empty server URL: text.Extract recognises same-origin dashboard URLs only against
// it, so an empty value would delete every URL-form mention.
func rebuildRefs(ctx context.Context, databaseURL, serverURL string, out io.Writer) int {
	if strings.TrimSpace(databaseURL) == "" {
		fmt.Fprintln(out, "rebuild-refs: DATABASE_URL is required")
		return 1
	}
	if strings.TrimSpace(serverURL) == "" {
		fmt.Fprintln(out, "rebuild-refs: dispatch.server_url is required to recognise dashboard URLs; refusing to drop URL-form mentions")
		return 1
	}
	database, ok := openMigrated(ctx, "rebuild-refs", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	// rebuild-refs opens a transaction per source, so it marks its context like the other
	// commands: a read taken inside one is refused rather than left to deadlock the pool.
	report, err := refs.RebuildAll(store.WithTransactionTracking(ctx), database.Pool, serverURL)
	if err != nil {
		fmt.Fprintf(out, "rebuild-refs: %v\n", err)
		return 1
	}
	writeRebuildRefsReport(out, report)
	return 0
}

// backfillEmbeddings enqueues and embeds meaning-search vectors for content this Dispatch was
// already carrying before LEGION-549 (embedqueue.Backfill); a fresh write is covered by its own
// table's trigger (0054_embeddings.up.sql) the moment COHERE_API_KEY is set, so this is a one-time
// catch-up, not something the server runs itself. Resumable: rerunning it (after an interrupt, or
// to pick up a kind this Dispatch grew after an earlier run finished) continues from each kind's
// own checkpoint rather than rescanning rows it already enqueued.
func backfillEmbeddings(ctx context.Context, databaseURL, cohereAPIKey string, out io.Writer) int {
	database, ok := openMigrated(ctx, "backfill-embeddings", databaseURL, out)
	if !ok {
		return 1
	}
	defer database.Pool.Close()
	if strings.TrimSpace(cohereAPIKey) == "" {
		fmt.Fprintln(out, "backfill-embeddings: COHERE_API_KEY is required")
		return 1
	}
	report, err := embedqueue.Backfill(ctx, embedqueue.Deps{Store: database, Embedder: embed.New(cohereAPIKey)}, out)
	if err != nil {
		fmt.Fprintf(out, "backfill-embeddings: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "backfill-embeddings: done - enqueued=%v embedded=%d failed=%d\n", report.Enqueued, report.Embedded, report.Failed)
	if report.Failed > 0 {
		return 1
	}
	return 0
}

func writeRebuildRefsReport(out io.Writer, report refs.Rebuild) {
	fmt.Fprintf(out, "rebuild-refs: documents=%d asks=%d comments=%d messages=%d orphans=%d edges=%d\n",
		report.Documents, report.Asks, report.Comments, report.Messages, report.Orphans, report.Edges)
}

func writeAnchorBlockBackfillReport(out io.Writer, result docs.AnchorBlockBackfill) {
	fmt.Fprintf(out, "backfill-anchor-blocks: asks=%d comments=%d skipped=%d\n", result.Asks, result.Comments, result.Skipped)
}

func writeBlockIDBackfillReport(out io.Writer, report docs.BlockIDBackfill) bool {
	switch {
	case report.Err != nil:
		fmt.Fprintf(out, "%s error (%v)\n", report.ArtifactID, report.Err)
		return false
	case report.Skipped != "":
		fmt.Fprintf(out, "%s skipped (%s)\n", report.ArtifactID, report.Skipped)
		return true
	default:
		fmt.Fprintf(out, "%s stamped=%d\n", report.ArtifactID, report.Stamped)
		return true
	}
}
