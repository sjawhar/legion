package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// seedFinishPoll is how often seed-finish reads a count it waits on.
const seedFinishPoll = 2 * time.Second

// The embedding gate's own numbers. It waits on Dispatch's embed queue rather than on a count
// Postgres settles by itself, which is work of another order: the loop seed writes every issue,
// document, comment and ask in minutes, and the queue embeds them a batch of up to 96 texts at a
// time, paced by two thirds of the deployment's token quota
// (internal/dispatch/embed/azure.go:NewAzure).
const (
	// seedEmbedTimeout is the gate's default bound: eight hours, which is the loop seed's own
	// shape, not a round number. Its 334,536 comment rows and 891 proposals' documents are more
	// text than any step writes, and a deployment that embeds them at a thousand texts a minute
	// needs about six hours; the bound leaves room above that rather than failing a seed that is
	// still making progress.
	seedEmbedTimeout = 8 * time.Hour
	// seedEmbedPoll is how often the gate reads the coverage report. The report is one statement
	// over the searchable rows, so a read a few times a minute costs the deployment nothing
	// measurable, and a seed that finishes embedding is noticed within it.
	seedEmbedPoll = 15 * time.Second
	// seedEmbedLogEvery is how often the gate writes a progress line. Every poll of an
	// eight-hour wait would be 1,920 lines; the record file (-embed-progress) holds every poll,
	// so the log holds the first and one a quarter hour.
	seedEmbedLogEvery = 15 * time.Minute
)

// searchCoveragePath is Dispatch's report of what meaning search covers
// (internal/dispatch/api/routes_table.go:280). It takes a person, a cookie or a bearer, so the
// gate reads it with the deployment's agent bearer like every other call the harness makes.
const searchCoveragePath = "/api/v1/search/coverage"

// The queries seed-finish runs. The block-index tables do not exist in a build older than
// migration 0121, which owes nothing; Postgres resolves every table a statement names before it
// runs, so the existence check is its own query.
const (
	sqlUnpublishedEvents = "select count(*) from events where published_at is null"
	sqlBlockIndexTables  = "select to_regclass('public.document_block_versions') is not null and to_regclass('public.block_grammar_owed') is not null"
	sqlBlockIndexOwed    = "select (select count(*) from document_block_versions where version is null) + (select count(*) from block_grammar_owed)"
	sqlVacuumAnalyze     = "vacuum analyze"
	// seed-refs writes under idempotency keys, so Dispatch keeps one request_keys row per write.
	// They protect retries during the seed only. Left in the template, Dispatch's sweep of keys
	// older than a day would delete them inside the window of every step on a seed older than
	// that, and not on a younger one. A build older than migration 0137 has no such table.
	sqlRequestKeysTable = "select to_regclass('public.request_keys') is not null"
	sqlRequestKeysDel   = "delete from request_keys"
	sqlRequestKeysCount = "select count(*) from request_keys"
)

// pgSession is what seed-finish uses of a Postgres connection; *pgx.Conn is one.
type pgSession interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// coverageReader is what the embedding gate reads meaning-search coverage through, as pgSession
// is what the waits above read Postgres through: a test fakes the reader rather than serving the
// report over HTTP.
type coverageReader interface {
	readCoverage(ctx context.Context) (model.SearchCoverage, error)
}

// dispatchCoverage reads the report over Dispatch's API with the harness's own client (api.go),
// so the gate sends the agent bearer and times a call out exactly as every other call does. A
// non-200 answer and a body that does not decode are both returned as errors: to the gate they
// are the same transient condition, a deployment that is not answering for now.
type dispatchCoverage struct{ a *api }

func (c dispatchCoverage) readCoverage(ctx context.Context) (model.SearchCoverage, error) {
	var coverage model.SearchCoverage
	res := c.a.call(ctx, http.MethodGet, searchCoveragePath, who{}, nil)
	switch {
	case res.err != nil:
		return coverage, fmt.Errorf("GET %s: %w", searchCoveragePath, res.err)
	case res.status != http.StatusOK:
		return coverage, fmt.Errorf("GET %s: HTTP %d %s", searchCoveragePath, res.status, trunc(res.body))
	}
	if err := json.Unmarshal(res.body, &coverage); err != nil {
		return coverage, fmt.Errorf("GET %s: %w", searchCoveragePath, err)
	}
	return coverage, nil
}

// seedFinisher runs the steps after `loadgen seed`, while Dispatch still runs: each wait polls its
// count every poll and fails once it has waited timeout; now and sleep are the clock it waits on.
// The embedding gate (waitEmbedded) keeps a bound of its own, embedTimeout, since it waits on
// hours of the embed queue's work rather than on a count Postgres settles in minutes; it reads
// coverage every embedPoll and writes one entry per poll to embedProgress. A nil coverage leaves
// that gate out, which is what a run given no deployment URL does.
type seedFinisher struct {
	timeout, poll           time.Duration
	embedTimeout, embedPoll time.Duration
	coverage                coverageReader
	embedProgress           string
	now                     func() time.Time
	sleep                   func(context.Context, time.Duration) bool
	lg                      *log.Logger
}

func runSeedFinish(args []string) error {
	fs := flag.NewFlagSet("seed-finish", flag.ExitOnError)
	pg := fs.String("pg", "", "direct Postgres URL of the seeded dispatch database (required)")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long each wait on a Postgres count may take before it fails")
	dispatchURL := fs.String("url", "", "Dispatch base URL; with it, seed-finish also waits for meaning search to embed the seed")
	token := fs.String("token", "", "Dispatch agent bearer, required with -url")
	embedTimeout := fs.Duration("embed-timeout", seedEmbedTimeout, "how long the wait for meaning search may take before it fails")
	embedProgress := fs.String("embed-progress", "", "where that wait's poll record is written (default $LOADTEST_ROOT/seedfinish.embed.json)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen seed-finish -pg <postgres url> [-timeout 15m] [-url <dispatch url> -token <bearer> [-embed-timeout 8h] [-embed-progress <path>]]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case *pg == "":
		return fmt.Errorf("-pg is required")
	case *timeout <= 0:
		return fmt.Errorf("-timeout must be more than 0, not %s", *timeout)
	case *embedTimeout <= 0:
		return fmt.Errorf("-embed-timeout must be more than 0, not %s", *embedTimeout)
	case *dispatchURL == "" && *token != "":
		return fmt.Errorf("-token names no deployment to read: it needs -url")
	case *dispatchURL != "" && *token == "":
		return fmt.Errorf("-token is required with -url: the coverage report takes the deployment's agent bearer")
	}
	if *dispatchURL != "" {
		u, err := url.Parse(*dispatchURL)
		if err != nil || !u.IsAbs() || u.Host == "" {
			return fmt.Errorf("-url %q is not an absolute URL with a host", *dispatchURL)
		}
	}
	if *embedProgress == "" {
		*embedProgress = filepath.Join(loadtestRoot(), "seedfinish.embed.json")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, *pg)
	if err != nil {
		return fmt.Errorf("connect to -pg: %w", err)
	}
	defer conn.Close(context.Background())
	f := seedFinisher{
		timeout: *timeout, poll: seedFinishPoll,
		embedTimeout: *embedTimeout, embedPoll: seedEmbedPoll, embedProgress: *embedProgress,
		now:   time.Now,
		sleep: func(ctx context.Context, d time.Duration) bool { return sleepUntil(ctx, time.Now().Add(d)) },
		lg:    log.New(os.Stderr, "loadgen seed-finish: ", 0),
	}
	if *dispatchURL != "" {
		f.coverage = dispatchCoverage{a: newAPI(*dispatchURL, *token, 2)}
	}
	return f.finish(ctx, conn)
}

// finish waits until the outbox has published every seed event, then, when the build has the
// block-index tables, until Dispatch's block-index backfill owes nothing: a seed that still owed
// it would hand every step about 100 s of that backfill inside its window, and production in
// steady state owes none. Then, with a deployment to read (-url), it waits until meaning search
// has embedded the seed (waitEmbedded). Then, when the build has request_keys, it deletes the
// seed's idempotency keys and checks that none is left; then it runs vacuum analyze and installs
// the commit-time trigger (commitTriggerSQL). The vacuum comes last so it reads the table the
// embed queue has finished writing, not one it is still filling.
func (f seedFinisher) finish(ctx context.Context, db pgSession) error {
	f.lg.Print("waiting for the outbox to publish every seed event")
	if err := f.waitZero(ctx, db, sqlUnpublishedEvents, "the outbox still holds %d unpublished events"); err != nil {
		return err
	}
	var indexed bool
	if err := db.QueryRow(ctx, sqlBlockIndexTables).Scan(&indexed); err != nil {
		return fmt.Errorf("%s: %w", sqlBlockIndexTables, err)
	}
	if indexed {
		f.lg.Print("waiting for Dispatch's block-index backfill to finish")
		if err := f.waitZero(ctx, db, sqlBlockIndexOwed, "Dispatch still owes %d block-index entries"); err != nil {
			return err
		}
	} else {
		f.lg.Print("no block-index tables (a build older than migration 0121): no backfill to wait for")
	}
	if err := f.waitEmbedded(ctx); err != nil {
		return err
	}
	var keyed bool
	if err := db.QueryRow(ctx, sqlRequestKeysTable).Scan(&keyed); err != nil {
		return fmt.Errorf("%s: %w", sqlRequestKeysTable, err)
	}
	if keyed {
		f.lg.Print("deleting the seed's idempotency keys (request_keys)")
		if _, err := db.Exec(ctx, sqlRequestKeysDel); err != nil {
			return fmt.Errorf("%s: %w", sqlRequestKeysDel, err)
		}
		var left int64
		if err := db.QueryRow(ctx, sqlRequestKeysCount).Scan(&left); err != nil {
			return fmt.Errorf("%s: %w", sqlRequestKeysCount, err)
		}
		if left != 0 {
			return fmt.Errorf("request_keys still holds %d rows after the delete; the template would carry them", left)
		}
	} else {
		f.lg.Print("no request_keys table (a build older than migration 0137): no idempotency keys to delete")
	}
	f.lg.Print("vacuum analyze")
	if _, err := db.Exec(ctx, sqlVacuumAnalyze); err != nil {
		return fmt.Errorf("%s: %w", sqlVacuumAnalyze, err)
	}
	f.lg.Print("installing the commit-time trigger (loadtest.event_commits)")
	if _, err := db.Exec(ctx, commitTriggerSQL); err != nil {
		return fmt.Errorf("install the commit-time trigger (commit-trigger.sql): %w", err)
	}
	f.lg.Print("done")
	return nil
}

// waitZero reads the count query until it is 0. Once it has waited f.timeout and the count is
// still above 0, it fails with pending (a format of that count) and how long it waited.
func (f seedFinisher) waitZero(ctx context.Context, db pgSession, query, pending string) error {
	deadline := f.now().Add(f.timeout)
	for {
		var n int64
		if err := db.QueryRow(ctx, query).Scan(&n); err != nil {
			return fmt.Errorf("%s: %w", query, err)
		}
		if n == 0 {
			return nil
		}
		if !f.now().Before(deadline) {
			return fmt.Errorf(pending+" after %s", n, seconds(f.timeout))
		}
		if !f.sleep(ctx, f.poll) {
			return ctx.Err()
		}
	}
}

// seconds is d as whole seconds, "900 s".
func seconds(d time.Duration) string { return fmt.Sprintf("%.0f s", d.Seconds()) }

// coverageRecord is what the embedding gate writes beside the seed's other outputs
// (-embed-progress): the embedder it waited on, one entry per poll, and how the wait ended, so a
// seed that waited hours can be read afterwards without the log. WaitedSeconds is the whole wait,
// TimeoutSeconds the bound it was given and PollSeconds how often it read.
type coverageRecord struct {
	Identity       *string        `json:"identity"`
	Place          *string        `json:"place"`
	Model          *string        `json:"model"`
	TimeoutSeconds int64          `json:"timeout_seconds"`
	PollSeconds    int64          `json:"poll_seconds"`
	WaitedSeconds  int64          `json:"waited_seconds"`
	Polls          []coveragePoll `json:"polls"`
	Error          string         `json:"error,omitempty"`
}

// coveragePoll is one read of the coverage report: when it was read and the body's total counts
// (model.SearchCoverageCounts: items, current, lacking, failed and how long the oldest lacking
// item has waited). A poll Dispatch did not answer, or answered with a body that does not
// decode, carries Error and no counts at all, since it saw none.
type coveragePoll struct {
	At    time.Time                   `json:"at"`
	Total *model.SearchCoverageCounts `json:"total"`
	Error string                      `json:"error,omitempty"`
}

// waitEmbedded waits until meaning search covers the seed: every kind the coverage report counts
// (issue, document, comment, ask, message) lacks no vector. The seed writes rows far faster than
// the embed queue embeds them, so a step started the moment the seed ends would measure a
// deployment still embedding its own fixture, and a meaning search over it would rank a fraction
// of the records; the gate makes the end of the seed the point where the database is what a step
// is meant to run against.
//
// It refuses rather than waits wherever waiting cannot help. A deployment with no embedder
// answers identity, place and model as null and never embeds anything
// (internal/dispatch/model/model.go:397-407), so that is refused at the first poll rather than
// eight hours later. A kind with rows the queue gave up on (failed) is refused as soon as it is
// read, since those rows do not clear by themselves. A poll Dispatch does not answer, or answers
// with a body that does not decode, is neither: a deployment restarting under the seed is
// expected, so the gate logs it and reads again until the bound.
func (f seedFinisher) waitEmbedded(ctx context.Context) (err error) {
	if f.coverage == nil {
		f.lg.Print("no -url: not waiting for meaning search to embed the seed")
		return nil
	}
	f.lg.Printf("waiting up to %s for meaning search to embed the seed", seconds(f.embedTimeout))
	start := f.now()
	record := &coverageRecord{TimeoutSeconds: int64(f.embedTimeout.Seconds()), PollSeconds: int64(f.embedPoll.Seconds())}
	defer func() {
		if err != nil {
			record.Error = err.Error()
		}
		record.WaitedSeconds = int64(f.now().Sub(start).Seconds())
		if f.embedProgress != "" {
			writeJSONFile(f.embedProgress, record, 0o644, f.lg)
		}
	}()
	deadline := start.Add(f.embedTimeout)
	var lastLog time.Time
	for {
		coverage, readErr := f.coverage.readCoverage(ctx)
		at := f.now()
		// The first poll is always logged, and one more every seedEmbedLogEvery after it.
		loud := lastLog.IsZero() || !at.Before(lastLog.Add(seedEmbedLogEvery))
		if loud {
			lastLog = at
		}
		if readErr != nil {
			record.Polls = append(record.Polls, coveragePoll{At: at, Error: readErr.Error()})
			if loud {
				f.lg.Printf("meaning search's coverage is unreadable for now (%v); reading again until %s have passed", readErr, seconds(f.embedTimeout))
			}
		} else {
			total := coverage.Total
			record.Polls = append(record.Polls, coveragePoll{At: at, Total: &total})
			record.Identity, record.Place, record.Model = coverage.Identity, coverage.Place, coverage.Model
			if coverage.Identity == nil {
				return fmt.Errorf("the deployment has no embedder: the coverage report answers identity, place and model as null, so nothing it holds is ever embedded (DISPATCH_EMBEDDER and the DISPATCH_AZURE_* settings; `loadgen stub-embed` stands in for Azure)")
			}
			if failed := coverageFailed(coverage.Kinds); failed != "" {
				return fmt.Errorf("Dispatch gave up embedding rows meaning search ranks: %s", failed)
			}
			lacking := coverageLacking(coverage.Kinds)
			if lacking == "" {
				f.lg.Printf("meaning search covers the seed: %d items current, after %s", total.Current, seconds(at.Sub(start)))
				return nil
			}
			if loud {
				f.lg.Printf("%d items current, %d lacking (oldest %s), after %s of %s", total.Current, total.Lacking, oldestLacking(total.OldestLackingSeconds), seconds(at.Sub(start)), seconds(f.embedTimeout))
			}
		}
		if !at.Before(deadline) {
			if readErr != nil {
				return fmt.Errorf("meaning search's coverage was unreadable for %s: %w", seconds(f.embedTimeout), readErr)
			}
			return fmt.Errorf("meaning search still lacks vectors after %s: %s", seconds(f.embedTimeout), coverageLacking(coverage.Kinds))
		}
		if !f.sleep(ctx, f.embedPoll) {
			return ctx.Err()
		}
	}
}

// coverageKind is one kind the coverage report counts, under the name the report gives it, so a
// refusal names what the operator reads in the body.
type coverageKind struct {
	name   string
	counts model.SearchCoverageCounts
}

// coverageKinds is every kind of a coverage body, in the order model.SearchCoverageKinds declares
// them (internal/dispatch/model/model.go:410-416).
func coverageKinds(kinds model.SearchCoverageKinds) []coverageKind {
	return []coverageKind{
		{"issue", kinds.Issue}, {"document", kinds.Document}, {"comment", kinds.Comment},
		{"ask", kinds.Ask}, {"message", kinds.Message},
	}
}

// coverageFailed names each kind holding rows the queue gave up on, with how many, "issue 4,
// comment 1"; empty when no kind holds any.
func coverageFailed(kinds model.SearchCoverageKinds) string {
	var named []string
	for _, kind := range coverageKinds(kinds) {
		if kind.counts.Failed > 0 {
			named = append(named, fmt.Sprintf("%s %d", kind.name, kind.counts.Failed))
		}
	}
	return strings.Join(named, ", ")
}

// coverageLacking names each kind still waiting for vectors, with how many and how long its
// oldest has waited, "issue 12 (oldest 900 s)"; empty when every kind is covered, which is what
// the gate waits for.
func coverageLacking(kinds model.SearchCoverageKinds) string {
	var named []string
	for _, kind := range coverageKinds(kinds) {
		if kind.counts.Lacking > 0 {
			named = append(named, fmt.Sprintf("%s %d (oldest %s)", kind.name, kind.counts.Lacking, oldestLacking(kind.counts.OldestLackingSeconds)))
		}
	}
	return strings.Join(named, ", ")
}

// oldestLacking is how long the longest-waiting lacking item has waited, as a line of the gate's
// says it; the report answers null when nothing lacks, which reads "unknown" here.
func oldestLacking(waited *int64) string {
	if waited == nil {
		return "unknown"
	}
	return fmt.Sprintf("%d s", *waited)
}
