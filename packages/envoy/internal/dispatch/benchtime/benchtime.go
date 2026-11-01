// Package benchtime times one Dispatch statement against another on the seeded measurement
// database that DISPATCH_BENCH_DATABASE_URL names, and binds the rows those statements read.
//
// It is INFRA-325's protocol 1, the committed harness: a named pair of statement texts - a
// before text and an after text - whose difference is SQL text alone, run alternately on one
// connection, each run's time read as planning plus execution from the EXPLAIN output's own two
// lines, the median of runs 2 to 12 reported, both plans printed, and the pair failed when the
// after text's median is not below the before text's. The details are the measurement: one
// connection for the whole pair, so the cold cache is paid once and by both sides; alternating
// runs, so a drift in the machine moves both sides together; the first run discarded, so a cache
// fill is not counted and eleven readings decide the median; and the plan printed beside the
// number, because A8 asks for the plan as well as the time.
//
// A before text is the text the caller measures against: for every pair committed here, the text
// the release before migration 20261101182419 ran, which each package already holds as its
// equality oracle.
//
// A statement whose gain needs an index or a table this change adds has no such pair: no one
// schema holds both sides of it. Head measures the after side of those alone, and their before
// side is the same measurement run against a database migrated from the release before this one,
// whose migrations stop at 0157 where this change's run from 20261101182419 to 20261101182522;
// that is protocol 2.
//
// It is an ordinary package rather than a _test.go file because all three statement-owning
// packages (api, refs, docs) run it and Go cannot share test-only code across packages, which is
// why store/storetest is one too. CI never runs a measurement: every entry point skips while
// DISPATCH_BENCH_DATABASE_URL is unset, the gate api/search_bench_test.go and
// api/related_recall_test.go already use.
package benchtime

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// DatabaseURLEnv names the seeded measurement database (cmd/dispatch-seed). It is deliberately
// not DISPATCH_TEST_DATABASE_URL: a measurement must never run against the cluster the Go suites
// template their databases from, whose durability is off and whose cache is 128 MB.
const DatabaseURLEnv = "DISPATCH_BENCH_DATABASE_URL"

const (
	// runs is how many times each side of a pair is explained, and discarded how many of those
	// the median leaves out: runs 2 to 12, eleven readings, the first paying whatever of the
	// statement's pages the cache did not hold.
	runs      = 12
	discarded = 1
)

// Statement is one statement text with the arguments one call binds to it. The two sides of a
// pair take their own arguments, since a before text and an after text need not have the same
// parameter list: the listing's paged text takes a limit and an offset the text it replaced did
// not.
type Statement struct {
	SQL  string
	Args []any
	// Setup are the `set local` statements the caller runs the statement under, applied in the
	// measuring transaction before the EXPLAIN. The related-item check's meaning query raises
	// hnsw.ef_search to 400 for its own transaction, and a measurement under pgvector's default
	// 40 would be of a statement production never runs.
	Setup []string
}

// Runner holds the one connection every measurement of a run shares and the subjects it bound.
// One connection is what makes a pair a comparison rather than two numbers: both sides see the
// same session settings, the same backend and the same local buffers, and whichever side runs
// first pays for the pages the other then finds cached.
type Runner struct {
	conn     *pgx.Conn
	subjects Subjects
}

// Open connects to the measurement database, records the server settings every number is read
// beside, and binds the subjects. databaseURL is what DatabaseURLEnv names, read by the caller:
// a non-test file of internal/dispatch may not read the process environment, which
// cmd/dispatch's TestNoReaderBypassesTheSettingsTable holds, so each _test.go caller passes
// os.Getenv(benchtime.DatabaseURLEnv) in. The gate itself stays here: an empty URL skips t,
// which is how CI never runs a measurement, and the three callers share that one sentence
// rather than each keeping a copy of it.
func Open(t *testing.T, databaseURL string) *Runner {
	t.Helper()
	if databaseURL == "" {
		t.Skip(DatabaseURLEnv + " must name the seeded measurement database (cmd/dispatch-seed); CI leaves it unset")
	}
	config, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("read %s: %v", DatabaseURLEnv, err)
	}
	// Every run re-plans. pgx's default mode names a prepared statement and caches it, and
	// Postgres switches such a statement to a generic plan from its sixth execution, whose
	// planning time is near zero and whose plan is not the one the measured call gets; the
	// median of runs 2 to 12 would then be eleven readings of two different things.
	config.DefaultQueryExecMode = pgx.QueryExecModeDescribeExec
	ctx := context.Background()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to the measurement database: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	runner := &Runner{conn: conn}
	runner.logSettings(t)
	runner.subjects = bindSubjects(t, conn)
	t.Logf("measurement subjects: %s", runner.subjects)
	return runner
}

// Subjects are the rows the statements read.
func (r *Runner) Subjects() Subjects { return r.subjects }

// Conn is the measurement connection, for a subject a single package reads and no other does:
// the mark ids of a document, the ask ids of an issue. A caller reads with it, and never writes.
func (r *Runner) Conn() *pgx.Conn { return r.conn }

// beforeSide and afterSide name the two sides of a pair in every line this package logs and in
// the error a pair fails with, which points a reader back at those lines by name. They are
// written once here so the two always read the same.
const (
	beforeSide = "the before text"
	afterSide  = "the after text"
)

// Pair measures a statement's before text against its after text under protocol 1 and fails when
// the after text's median is not below the before text's. Each pair is a subtest of its own, so a
// pair that does not improve names itself and leaves the rest of the run to report their own
// numbers.
func (r *Runner) Pair(t *testing.T, name string, before, after Statement) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		beforeRuns := make([]reading, 0, runs)
		afterRuns := make([]reading, 0, runs)
		for range runs {
			beforeRuns = append(beforeRuns, r.explain(t, beforeSide, before))
			afterRuns = append(afterRuns, r.explain(t, afterSide, after))
		}
		beforeMedian := median(beforeRuns[discarded:])
		afterMedian := median(afterRuns[discarded:])
		t.Logf("%s: %s, planning plus execution %v (median of runs %d to %d), its median run's plan:\n%s",
			name, beforeSide, beforeMedian.took, discarded+1, runs, beforeMedian.plan)
		t.Logf("%s: %s, planning plus execution %v (median of runs %d to %d), its median run's plan:\n%s",
			name, afterSide, afterMedian.took, discarded+1, runs, afterMedian.plan)
		if afterMedian.took >= beforeMedian.took {
			t.Errorf("%s: the after text's median is %v, not below the before text's %v; "+
				"both plans are logged above this line, under %q and %q",
				name, afterMedian.took, beforeMedian.took,
				name+": "+beforeSide, name+": "+afterSide)
		}
	})
}

// Head measures the head side of a statement that has no protocol 1 pair, because its gain needs
// an index or a table this change adds and one schema cannot hold both sides of that. It sets no
// bound: the before side of such a statement is this same measurement, run against a database
// migrated from the release before this one, whose migrations stop at 0157, and seam 7's
// two-commit run is what puts the two together.
func (r *Runner) Head(t *testing.T, name string, head Statement) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		readings := make([]reading, 0, runs)
		for range runs {
			readings = append(readings, r.explain(t, "the head's text", head))
		}
		taken := median(readings[discarded:])
		t.Logf("%s: planning plus execution %v (median of runs %d to %d), its median run's plan:\n%s",
			name, taken.took, discarded+1, runs, taken.plan)
	})
}

// reading is one EXPLAIN (ANALYZE, BUFFERS) run: planning plus execution as that run's own
// output reports them, and the plan text it reported them in.
type reading struct {
	took time.Duration
	plan string
}

// explain runs one statement under EXPLAIN (ANALYZE, BUFFERS) inside a transaction it rolls
// back. The rollback is what lets the anchor reads be measured at all: two of them end in `for
// share`, and the row locks an analysed run takes are held until the transaction ends.
func (r *Runner) explain(t *testing.T, side string, statement Statement) reading {
	t.Helper()
	ctx := context.Background()
	tx, err := r.conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin a measurement of %s: %v", side, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, setting := range statement.Setup {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s before a measurement of %s: %v", setting, side, err)
		}
	}
	rows, err := tx.Query(ctx, "explain (analyze, buffers) "+statement.SQL, statement.Args...)
	if err != nil {
		t.Fatalf("explain %s: %v", side, err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read the plan of %s: %v", side, err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("roll back a measurement of %s: %v", side, err)
	}
	plan := strings.Join(lines, "\n")
	took, err := planTime(plan)
	if err != nil {
		t.Fatalf("%s: %v\n%s", side, err, plan)
	}
	return reading{took: took, plan: plan}
}

// planningLine and executionLine read the two times EXPLAIN (ANALYZE) reports in its own output.
// The time of record is their sum: a statement whose plan costs more than it saves is no faster,
// and the issue list's paged text is planned on every call.
var (
	planningLine  = regexp.MustCompile(`(?m)^\s*Planning Time: ([0-9.]+) ms\s*$`)
	executionLine = regexp.MustCompile(`(?m)^\s*Execution Time: ([0-9.]+) ms\s*$`)
)

// planTime is a plan's planning plus execution time. A plan missing either line is an error
// rather than a zero: a missing time would read as the fastest run there is and would win every
// median it is in.
func planTime(plan string) (time.Duration, error) {
	var total time.Duration
	for _, line := range []struct {
		what    string
		pattern *regexp.Regexp
	}{{"Planning Time", planningLine}, {"Execution Time", executionLine}} {
		match := line.pattern.FindStringSubmatch(plan)
		if match == nil {
			return 0, fmt.Errorf("the plan carries no %s line", line.what)
		}
		milliseconds, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			return 0, fmt.Errorf("read the %s line %q: %w", line.what, match[0], err)
		}
		total += time.Duration(milliseconds * float64(time.Millisecond))
	}
	return total, nil
}

// median is the middle reading of an odd number of them, the whole reading and not its time
// alone, so the plan printed beside a number is the run that gave that number.
func median(readings []reading) reading {
	sorted := slices.Clone(readings)
	slices.SortFunc(sorted, func(a, b reading) int { return cmp.Compare(a.took, b.took) })
	return sorted[len(sorted)/2]
}

// settingsRecorded are the server settings every measurement is recorded beside: the plan asks
// for the measurement cluster's durability and memory with each number, and a plan read under
// another shared_buffers or another effective_cache_size is a different measurement.
var settingsRecorded = []string{
	"server_version", "shared_buffers", "work_mem", "maintenance_work_mem", "effective_cache_size",
	"fsync", "synchronous_commit", "full_page_writes", "random_page_cost", "jit",
	"max_parallel_workers_per_gather",
}

func (r *Runner) logSettings(t *testing.T) {
	t.Helper()
	rows, err := r.conn.Query(context.Background(),
		`select name, setting, coalesce(unit, '') from pg_settings where name = any($1) order by name`,
		settingsRecorded)
	if err != nil {
		t.Fatalf("read the server settings: %v", err)
	}
	settings, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var name, setting, unit string
		if err := row.Scan(&name, &setting, &unit); err != nil {
			return "", err
		}
		return name + "=" + setting + unit, nil
	})
	if err != nil {
		t.Fatalf("read the server settings: %v", err)
	}
	t.Logf("measurement cluster: %s", strings.Join(settings, " "))
}
