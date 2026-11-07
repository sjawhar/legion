package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The columns pg_stat_statements offers, by version: 1.8 and later renamed total_time and
// mean_time to total_exec_time and mean_exec_time, and a build of the extension that is older
// than 1.8 still answers the old names (pg_stat_statements' own history).
var (
	pgCols18 = []string{"userid", "dbid", "queryid", "query", "calls", "total_exec_time", "min_exec_time",
		"mean_exec_time", "rows", "shared_blks_hit", "shared_blks_read"}
	pgColsOld = []string{"userid", "dbid", "queryid", "query", "calls", "total_time", "mean_time", "rows",
		"shared_blks_hit", "shared_blks_read"}
)

// pgResetVoid and pgResetTimestamp are the two shapes of pg_stat_statements_reset the catalogue
// holds: the one every version since 1.5 offers, whose three arguments all have defaults and
// which returns void, and 1.11's, which returns the time of the reset.
var (
	pgResetVoid = pgResetFunc{
		Name: "pg_stat_statements_reset", Signature: "pg_stat_statements_reset(oid,oid,bigint)",
		NArgs: 3, NDefaults: 3, Result: "void",
	}
	pgResetTimestamp = pgResetFunc{
		Name: "pg_stat_statements_reset", Signature: "pg_stat_statements_reset(oid,oid,bigint,boolean)",
		NArgs: 4, NDefaults: 4, Result: "timestamp with time zone",
	}
)

// fakePGStats is a Postgres these collectors can be run whole against: it answers the catalogue
// reads from its fields, hands out one lock-wait answer per sample, and records what was called.
type fakePGStats struct {
	mu sync.Mutex

	name, version, extension string
	statementCols, lockCols  []string
	resets                   []pgResetFunc
	statements               []pgStatementRow
	// lockSamples are handed out in order; the last one repeats once they run out.
	lockSamples [][]pgLockObservation
	// sizes are handed out in order, one per databaseSize call.
	sizes []int64

	// The failures a test asks for.
	resetErr, statementsErr, lockErr, sizeErr error

	// What the collector did.
	resetCall   string
	resetCalls  int
	lockReads   int
	sizeReads   int
	topAsked    int
	colsAsked   pgStatementColumns
	closed      bool
	waitstartOn bool
}

func (f *fakePGStats) database(context.Context) (string, error)      { return f.name, nil }
func (f *fakePGStats) serverVersion(context.Context) (string, error) { return f.version, nil }

func (f *fakePGStats) extensionVersion(_ context.Context, name string) (string, error) {
	if name != pgStatementsExtension {
		return "", fmt.Errorf("the fake was asked for %s", name)
	}
	return f.extension, nil
}

func (f *fakePGStats) relationColumns(_ context.Context, relation string) ([]string, error) {
	switch relation {
	case pgStatementsExtension:
		return f.statementCols, nil
	case "pg_locks":
		return f.lockCols, nil
	}
	return nil, fmt.Errorf("the fake holds no relation %s", relation)
}

func (f *fakePGStats) resetFunctions(context.Context) ([]pgResetFunc, error) { return f.resets, nil }

func (f *fakePGStats) callReset(_ context.Context, fn pgResetFunc) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetCalls++
	f.resetCall = pgResetCall(fn)
	if f.resetErr != nil {
		return "", f.resetErr
	}
	if strings.EqualFold(fn.Result, "void") {
		return "", nil
	}
	return "2026-11-06T09:00:00Z", nil
}

func (f *fakePGStats) topStatements(_ context.Context, cols pgStatementColumns, top int) ([]pgStatementRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.colsAsked, f.topAsked = cols, top
	if f.statementsErr != nil {
		return nil, f.statementsErr
	}
	return f.statements, nil
}

func (f *fakePGStats) lockWaits(_ context.Context, waitstart bool) ([]pgLockObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.waitstartOn = waitstart
	i := f.lockReads
	f.lockReads++
	if f.lockErr != nil {
		return nil, f.lockErr
	}
	if len(f.lockSamples) == 0 {
		return nil, nil
	}
	if i >= len(f.lockSamples) {
		i = len(f.lockSamples) - 1
	}
	return f.lockSamples[i], nil
}

func (f *fakePGStats) databaseSize(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.sizeReads
	f.sizeReads++
	if f.sizeErr != nil {
		return 0, f.sizeErr
	}
	if i >= len(f.sizes) {
		i = len(f.sizes) - 1
	}
	return f.sizes[i], nil
}

func (f *fakePGStats) close() { f.closed = true }

// The lock waits' ages below are pointers: pg_locks.waitstart gives one, and a server with no
// such column gives none.

// newFakePGStats is a seeded load-test database: the extension of a current server, two slow
// statements and two lock waits.
func newFakePGStats() *fakePGStats {
	return &fakePGStats{
		name: "dispatch", version: "16.4", extension: "1.10",
		statementCols: pgCols18,
		lockCols:      []string{"locktype", "database", "relation", "pid", "mode", "granted", "waitstart"},
		resets:        []pgResetFunc{pgResetVoid},
		statements: []pgStatementRow{
			{Query: "insert into events ($1)", Calls: 1200, TotalMs: 9000, MeanMs: 7.5},
			{Query: "select * from documents where id = $1", Calls: 40000, TotalMs: 4000, MeanMs: 0.1},
		},
		lockSamples: [][]pgLockObservation{{
			{LockType: "advisory", Target: "commit_order_lock", Mode: "ExclusiveLock",
				WaiterQuery: "insert into events ($1)", BlockerQuery: "select pg_advisory_xact_lock($1, $2)", WaitMs: new(120.0)},
			{LockType: "transactionid", Target: "row_wait", Mode: "ShareLock",
				WaiterQuery: "update documents set body = $1", BlockerQuery: "update documents set body = $1", WaitMs: new(40.0)},
		}, {
			{LockType: "advisory", Target: "commit_order_lock", Mode: "ExclusiveLock",
				WaiterQuery: "insert into events ($1)", BlockerQuery: "select pg_advisory_xact_lock($1, $2)", WaitMs: new(300.0)},
		}},
		sizes: []int64{4 << 30},
	}
}

// runPGStatementsAgainst runs the whole command against the fake, with a window short enough to
// take a few samples, and reads back what it wrote.
func runPGStatementsAgainst(t *testing.T, db pgStatsDB, extra ...string) (string, pgStatementsOut, []byte, error) {
	t.Helper()
	dir := t.TempDir()
	withFakePGStats(t, db)
	args := append([]string{
		"-step", dir, "-pg-file", credentialFileAt(t, "pg-url", "postgres://u:p@127.0.0.1:5432/dispatch\n"),
		"-window", "60ms", "-interval", "20ms",
	}, extra...)
	err := runPGStatements(args)
	out := filepath.Join(dir, pgStatementsFileName)
	var f pgStatementsOut
	b, readErr := os.ReadFile(out)
	if readErr == nil {
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("%s is not JSON: %v: %s", out, err, b)
		}
	}
	return out, f, b, err
}

// withFakePGStats makes every connection of this test reach db.
func withFakePGStats(t *testing.T, db pgStatsDB) {
	t.Helper()
	was := openPGStats
	openPGStats = func(context.Context, string) (pgStatsDB, error) { return db, nil }
	t.Cleanup(func() { openPGStats = was })
}

// TestPGStatementsWritesTheStepsStatementsAndItsLockWaits: the file holds the reset that makes
// the statements this step's own, the statements themselves, and the lock waits the samples saw,
// each with the statement at both ends and how long it had waited.
func TestPGStatementsWritesTheStepsStatementsAndItsLockWaits(t *testing.T) {
	db := newFakePGStats()
	out, f, _, err := runPGStatementsAgainst(t, db)
	if err != nil {
		t.Fatalf("pg-statements: %v", err)
	}
	if !f.Reset.OK || f.Reset.Call != "pg_stat_statements_reset()" || f.Reset.At.IsZero() {
		t.Errorf("%s records the reset as %+v, want a reset that worked, with its call and its time", out, f.Reset)
	}
	if db.resetCalls != 1 {
		t.Errorf("the watcher reset the statistics %d times, want once, at the step's start", db.resetCalls)
	}
	if f.Database != "dispatch" || f.ServerVersion != "16.4" || f.ExtensionVersion != "1.10" {
		t.Errorf("the file says %s on %s with %s %s; want the fake's database, server and extension",
			f.Database, f.ServerVersion, f.Extension, f.ExtensionVersion)
	}
	if len(f.Statements) != 2 || f.Statements[0].Query != "insert into events ($1)" || f.Statements[0].TotalMs != 9000 {
		t.Fatalf("the file holds %+v, want the two statements the view answered", f.Statements)
	}
	if db.topAsked != pgTopStatements || db.colsAsked.Total != "total_exec_time" || db.colsAsked.Mean != "mean_exec_time" {
		t.Errorf("the read asked for %d rows of %s/%s; want %d of the 1.8 column names",
			db.topAsked, db.colsAsked.Total, db.colsAsked.Mean, pgTopStatements)
	}
	if f.LockSamples < 2 || f.LockSampleFailures != 0 {
		t.Fatalf("the watcher took %d lock samples and %d failed; want several over the window and none failed",
			f.LockSamples, f.LockSampleFailures)
	}
	if !db.waitstartOn || f.LockWaitSource != "pg_locks.waitstart" {
		t.Errorf("the lock waits say their length comes from %q, want pg_locks.waitstart, which this server's pg_locks has", f.LockWaitSource)
	}
	if len(f.LockWaits) != 2 {
		t.Fatalf("the file holds %d lock waits, want the two the samples saw: %+v", len(f.LockWaits), f.LockWaits)
	}
	// The longest wait first, with the statement at each end and the length pg_locks reported.
	first := f.LockWaits[0]
	if first.Target != "commit_order_lock" || first.WaiterQuery != "insert into events ($1)" ||
		first.BlockerQuery != "select pg_advisory_xact_lock($1, $2)" {
		t.Errorf("the first lock wait is %+v, want the commit-order lock with the statement at each end", first)
	}
	if first.WaitMsMax == nil || *first.WaitMsMax != 300 || first.WaitMsMean == nil {
		t.Errorf("the first lock wait's length is %+v, want a max of 300 ms and a mean", first)
	}
	if first.Observations < 2 || first.SeenForS <= 0 {
		t.Errorf("the first lock wait was seen %d times over %g s, want every sample of the window", first.Observations, first.SeenForS)
	}
	if second := f.LockWaits[1]; second.Target != "row_wait" || second.WaitMsMax == nil || *second.WaitMsMax != 40 {
		t.Errorf("the second lock wait is %+v, want the row wait of 40 ms", second)
	}
	if f.WindowS <= 0 || f.EndedAt.Before(f.StartedAt) {
		t.Errorf("the file's window is %g s from %s to %s", f.WindowS, f.StartedAt, f.EndedAt)
	}
}

// TestPGStatementsRefusesADatabaseWithNoExtension: a database the extension was never created in
// is refused by its name, and writes no file. A step whose database holds no pg_stat_statements
// has no slowest statements to record, and an empty file would read as a step that ran no
// statement worth recording.
func TestPGStatementsRefusesADatabaseWithNoExtension(t *testing.T) {
	db := newFakePGStats()
	db.extension = ""
	out, _, _, err := runPGStatementsAgainst(t, db)
	if err == nil {
		t.Fatal("a database with no pg_stat_statements was not refused")
	}
	if !strings.Contains(err.Error(), pgStatementsExtension) || !strings.Contains(err.Error(), "dispatch") {
		t.Errorf("the refusal %q names neither the extension nor the database", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("a refused watcher wrote %s; a collector that never ran leaves no file", out)
	}
	if db.resetCalls != 0 {
		t.Errorf("the watcher reset the statistics %d times on a database with no extension", db.resetCalls)
	}
}

// TestPGStatementsRefusesAViewItCannotRead: a pg_stat_statements with no total-time column of
// either name is refused by the columns it does offer, before the step runs.
func TestPGStatementsRefusesAViewItCannotRead(t *testing.T) {
	db := newFakePGStats()
	db.statementCols = []string{"userid", "dbid", "query", "calls"}
	out, _, _, err := runPGStatementsAgainst(t, db)
	if err == nil || !strings.Contains(err.Error(), "total time") {
		t.Fatalf("want a refusal naming the missing total time, got %v", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("a refused watcher wrote %s", out)
	}
}

// TestPGStatementsRecordsAFailedReset: a reset the server refused is recorded as a failed reset
// beside the statements the window read, so a step whose statistics still hold older steps is
// not read as a step with few statements.
func TestPGStatementsRecordsAFailedReset(t *testing.T) {
	db := newFakePGStats()
	db.resetErr = errors.New("permission denied for function pg_stat_statements_reset")
	out, f, _, err := runPGStatementsAgainst(t, db)
	if err != nil {
		t.Fatalf("a failed reset stopped the watcher: %v", err)
	}
	if f.Reset.OK {
		t.Fatalf("%s records the failed reset as one that worked: %+v", out, f.Reset)
	}
	if !strings.Contains(f.Reset.Error, "permission denied") || f.Reset.Call != "pg_stat_statements_reset()" {
		t.Errorf("the record of the failed reset is %+v, want the call and the server's reason", f.Reset)
	}
	if len(f.Statements) != 2 {
		t.Errorf("a failed reset left %d statements, want the ones the view did hold", len(f.Statements))
	}
}

// TestPGStatementsRecordsAServerWithNoResetFunction: a server whose catalogue holds no
// pg_stat_statements_reset that can be called records that, rather than a reset that worked.
func TestPGStatementsRecordsAServerWithNoResetFunction(t *testing.T) {
	db := newFakePGStats()
	db.resets = nil
	_, f, _, err := runPGStatementsAgainst(t, db)
	if err != nil {
		t.Fatalf("pg-statements: %v", err)
	}
	if f.Reset.OK || !strings.Contains(f.Reset.Error, "no pg_stat_statements_reset") {
		t.Errorf("the reset record is %+v, want the missing function named", f.Reset)
	}
	if db.resetCalls != 0 {
		t.Errorf("the watcher called a reset function the catalogue does not hold")
	}
}

// TestPGStatementsWritesEmptyArraysWhenItFindsNothing: a step whose window held no lock wait and
// whose view answered no row writes both arrays empty. The check tells a collector that ran and
// found nothing from one that never ran, so the file is written and the arrays are [] and not
// null.
func TestPGStatementsWritesEmptyArraysWhenItFindsNothing(t *testing.T) {
	db := newFakePGStats()
	db.statements, db.lockSamples = nil, nil
	out, f, raw, err := runPGStatementsAgainst(t, db)
	if err != nil {
		t.Fatalf("pg-statements: %v", err)
	}
	if len(f.Statements) != 0 || len(f.LockWaits) != 0 {
		t.Fatalf("%s holds %d statements and %d lock waits, want none", out, len(f.Statements), len(f.LockWaits))
	}
	for _, want := range []string{`"statements": []`, `"lock_waits": []`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s does not hold %s:\n%s", out, want, raw)
		}
	}
	if f.LockSamples == 0 {
		t.Errorf("the file says the lock sampler took no sample; it ran and saw no wait")
	}
}

// TestPGStatementsCountsFailedLockReads: reads of pg_locks that failed are counted, not dropped,
// and the file still holds the step's statements.
func TestPGStatementsCountsFailedLockReads(t *testing.T) {
	db := newFakePGStats()
	db.lockErr = errors.New("canceling statement due to statement timeout")
	_, f, _, err := runPGStatementsAgainst(t, db)
	if err != nil {
		t.Fatalf("pg-statements: %v", err)
	}
	if f.LockSampleFailures == 0 || f.LockSamples != 0 {
		t.Errorf("the file says %d samples and %d failures, want the failed reads counted and no sample",
			f.LockSamples, f.LockSampleFailures)
	}
	if len(f.Statements) != 2 {
		t.Errorf("a failed lock read cost the step its statements: %d", len(f.Statements))
	}
}

// TestPGStatementsRefusesBadFlags: the flags and the URL are judged before any connection.
func TestPGStatementsRefusesBadFlags(t *testing.T) {
	dir := t.TempDir()
	file := credentialFileAt(t, "pg-url", "postgres://u:p@127.0.0.1:5432/dispatch\n")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-pg-file", file}, "-step is required"},
		{[]string{"-step", dir}, "-pg or -pg-file is required"},
		{[]string{"-step", dir, "-pg-file", file, "-top", "0"}, "-top must be above 0"},
		{[]string{"-step", dir, "-pg-file", file, "-interval", "0"}, "-interval must be above 0"},
		{[]string{"-step", dir, "-pg", "not-a-url"}, "is not a Postgres URL"},
		{[]string{"-step", dir, "-pg", "postgres://x/dispatch", "-pg-file", file}, "both give the same credential"},
	} {
		if err := runPGStatements(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("runPGStatements(%v): want an error naming %q, got %v", tc.args, tc.want, err)
		}
	}
}

// TestPGStatementColumnsFromTheCatalogue: the read uses the column names this server's extension
// offers, and a view it cannot be built from is refused by the columns it does offer.
func TestPGStatementColumnsFromTheCatalogue(t *testing.T) {
	c, err := pgStatementColumnsFrom(pgCols18)
	if err != nil {
		t.Fatalf("the 1.8 columns were refused: %v", err)
	}
	if c.Total != "total_exec_time" || c.Mean != "mean_exec_time" || !c.HasRows || !c.HasSharedRead || !c.HasSharedHit || !c.HasQueryID || !c.HasDB {
		t.Errorf("the 1.8 columns read as %+v", c)
	}
	if q := pgStatementsQuery(c); !strings.Contains(q, "order by total_exec_time desc") ||
		!strings.Contains(q, "where dbid = (select oid from pg_database where datname = current_database())") {
		t.Errorf("the query of the 1.8 columns is\n%s", q)
	}
	old, err := pgStatementColumnsFrom(pgColsOld)
	if err != nil {
		t.Fatalf("the pre-1.8 columns were refused: %v", err)
	}
	if old.Total != "total_time" || old.Mean != "mean_time" {
		t.Errorf("the pre-1.8 columns read as %+v, want the names that version offers", old)
	}
	if _, err := pgStatementColumnsFrom([]string{"query", "calls", "rows"}); err == nil {
		t.Error("a view with no total time at all was taken for one that can be read")
	} else if !strings.Contains(err.Error(), "query, calls, rows") {
		t.Errorf("the refusal %q does not name the columns the view does offer", err)
	}
}

// TestPGResetCallFromTheCatalogue: the call is the one this server supports -- no argument where
// every argument has a default, the three zeroes where they do not -- and a server with no reset
// function at all is refused by name.
func TestPGResetCallFromTheCatalogue(t *testing.T) {
	for _, tc := range []struct {
		name string
		fs   []pgResetFunc
		want string
	}{
		{"defaulted arguments", []pgResetFunc{pgResetVoid}, "pg_stat_statements_reset()"},
		{"a reset that returns its time", []pgResetFunc{pgResetTimestamp}, "pg_stat_statements_reset()"},
		{"arguments with no default", []pgResetFunc{{Name: "pg_stat_statements_reset", NArgs: 3, Result: "void"}}, "pg_stat_statements_reset(0, 0, 0)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := pgResetFuncFor(tc.fs)
			if err != nil {
				t.Fatalf("pgResetFuncFor: %v", err)
			}
			if got := pgResetCall(f); got != tc.want {
				t.Errorf("the call is %q, want %q", got, tc.want)
			}
		})
	}
	if _, err := pgResetFuncFor(nil); err == nil || !strings.Contains(err.Error(), "no pg_stat_statements_reset") {
		t.Errorf("a server with no reset function gave %v", err)
	}
	if _, err := pgResetFuncFor([]pgResetFunc{{Name: "pg_stat_statements_reset", Signature: "pg_stat_statements_reset(text)", NArgs: 1}}); err == nil ||
		!strings.Contains(err.Error(), "pg_stat_statements_reset(text)") {
		t.Errorf("a reset that cannot be called with no argument gave %v", err)
	}
}

// TestPGLockWaitsQueryFollowsTheCatalogue: a server whose pg_locks has waitstart (Postgres 14 and
// later) is asked for each wait's length, and an older one is not asked for a column it does not
// have.
func TestPGLockWaitsQueryFollowsTheCatalogue(t *testing.T) {
	with := pgLockWaitsQuery(true)
	if !strings.Contains(with, "w.waitstart") || !strings.Contains(with, "clock_timestamp()") {
		t.Errorf("the query of a server with waitstart does not read it:\n%s", with)
	}
	without := pgLockWaitsQuery(false)
	if strings.Contains(without, "waitstart") {
		t.Errorf("the query of a server with no waitstart reads it:\n%s", without)
	}
	if !strings.Contains(without, "null::float8") {
		t.Errorf("the query of a server with no waitstart does not say the length is unknown:\n%s", without)
	}
	for _, q := range []string{with, without} {
		if strings.Contains(q, "pg_blocking_pids") {
			t.Error("the lock read calls pg_blocking_pids, which takes exclusive access to the lock manager")
		}
	}
}

// TestSummarizePGLockWaitsCountsEachWaitOnce: the same wait in several samples is one group with
// its observations, its span and its longest length, and the longest wait comes first.
func TestSummarizePGLockWaitsCountsEachWaitOnce(t *testing.T) {
	at := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	short := pgLockObservation{LockType: "relation", Target: "events", Mode: "RowExclusiveLock",
		WaiterQuery: "insert into events ($1)", BlockerQuery: "vacuum", WaitMs: new(10.0)}
	long := pgLockObservation{LockType: "advisory", Target: "commit_order_lock", Mode: "ExclusiveLock",
		WaiterQuery: "insert into events ($1)", BlockerQuery: "commit", WaitMs: new(100.0)}
	longer := long
	longer.WaitMs = new(500.0)
	groups := summarizePGLockWaits([]pgLockSample{
		{At: at, Waits: []pgLockObservation{short, long}},
		{At: at.Add(2 * time.Second), Waits: []pgLockObservation{longer}},
	}, 2*time.Second)
	if len(groups) != 2 {
		t.Fatalf("the samples made %d groups, want 2: %+v", len(groups), groups)
	}
	first := groups[0]
	if first.Target != "commit_order_lock" || first.Observations != 2 {
		t.Errorf("the first group is %+v, want the commit-order lock seen twice", first)
	}
	if first.WaitMsMax == nil || *first.WaitMsMax != 500 || first.WaitMsMean == nil || *first.WaitMsMean != 300 {
		t.Errorf("the first group's lengths are %+v, want a max of 500 ms and a mean of 300 ms", first)
	}
	if first.SeenForS != 4 {
		t.Errorf("the first group was in view for %g s, want the 2 s between the samples plus the interval", first.SeenForS)
	}
	if groups[1].Target != "events" || groups[1].Observations != 1 {
		t.Errorf("the second group is %+v, want the relation lock seen once", groups[1])
	}
	// A server with no waitstart leaves the lengths unknown; the count and the span remain.
	none := summarizePGLockWaits([]pgLockSample{{At: at, Waits: []pgLockObservation{{LockType: "advisory", Target: "commit_order_lock"}}}}, time.Second)
	if len(none) != 1 || none[0].WaitMsMax != nil || none[0].WaitMsMean != nil || none[0].SeenForS != 1 {
		t.Errorf("a wait with no length from the server reads as %+v, want null lengths and the sampled span", none)
	}
}

// --- loadgen pg-size ----------------------------------------------------------------------------

// runPGSizeAgainst runs one phase against the fake and reads the file back.
func runPGSizeAgainst(t *testing.T, db pgStatsDB, dir, phase string) (string, pgSizeOut, error) {
	t.Helper()
	withFakePGStats(t, db)
	err := runPGSize([]string{
		"-step", dir, "-phase", phase,
		"-pg-file", credentialFileAt(t, "pg-url", "postgres://u:p@127.0.0.1:5432/dispatch\n"),
	})
	out := filepath.Join(dir, pgSizeFileName)
	var f pgSizeOut
	if b, readErr := os.ReadFile(out); readErr == nil {
		if err := json.Unmarshal(b, &f); err != nil {
			t.Fatalf("%s is not JSON: %v: %s", out, err, b)
		}
	}
	return out, f, err
}

// TestPGSizeRecordsTheStartAndTheEnd: the start phase writes the size before the load and the end
// phase adds the size after it, in the one file R6's reading is read from.
func TestPGSizeRecordsTheStartAndTheEnd(t *testing.T) {
	db := newFakePGStats()
	db.sizes = []int64{4 << 30, 5 << 30}
	dir := t.TempDir()
	out, start, err := runPGSizeAgainst(t, db, dir, pgSizePhaseStart)
	if err != nil {
		t.Fatalf("pg-size -phase start: %v", err)
	}
	if start.StartBytes != 4<<30 || start.Database != "dispatch" || start.StartAt.IsZero() {
		t.Fatalf("%s holds %+v after the start phase", out, start)
	}
	if start.EndBytes != nil {
		t.Errorf("the start phase wrote an end size of %d; the step had not run yet", *start.EndBytes)
	}
	_, end, err := runPGSizeAgainst(t, db, dir, pgSizePhaseEnd)
	if err != nil {
		t.Fatalf("pg-size -phase end: %v", err)
	}
	if end.StartBytes != 4<<30 || end.EndBytes == nil || *end.EndBytes != 5<<30 {
		t.Fatalf("%s holds %+v after the end phase, want both sizes", out, end)
	}
	if end.EndAt == nil || end.EndAt.Before(end.StartAt) {
		t.Errorf("the end phase recorded %v, want the time it read the size", end.EndAt)
	}
}

// TestPGSizeRefusesAnEndWithNoStart: the end phase with no file to add to is refused by the path
// it looked in, and writes nothing. A file holding an end size and a start of 0 would read as a
// step that began with an empty database.
func TestPGSizeRefusesAnEndWithNoStart(t *testing.T) {
	dir := t.TempDir()
	out, _, err := runPGSizeAgainst(t, newFakePGStats(), dir, pgSizePhaseEnd)
	if err == nil {
		t.Fatal("an end phase with no start was not refused")
	}
	if !strings.Contains(err.Error(), pgSizeFileName) || !strings.Contains(err.Error(), "-phase start") {
		t.Errorf("the refusal %q names neither the file nor the phase that writes it", err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Errorf("a refused end phase wrote %s", out)
	}
}

// TestPGSizeRefusesAStartOfZero: a start file whose size is 0 is no reading, and the end phase
// says so rather than adding its own size to it.
func TestPGSizeRefusesAStartOfZero(t *testing.T) {
	dir := t.TempDir()
	if err := writePGJSON(filepath.Join(dir, pgSizeFileName), pgSizeOut{Database: "dispatch"}); err != nil {
		t.Fatal(err)
	}
	_, _, err := runPGSizeAgainst(t, newFakePGStats(), dir, pgSizePhaseEnd)
	if err == nil || !strings.Contains(err.Error(), "no start size") {
		t.Fatalf("want a refusal naming the missing start size, got %v", err)
	}
}

// TestPGSizeRefusesBadFlags: the phase and the credential are judged before any connection.
func TestPGSizeRefusesBadFlags(t *testing.T) {
	dir := t.TempDir()
	file := credentialFileAt(t, "pg-url", "postgres://u:p@127.0.0.1:5432/dispatch\n")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"-phase", "start", "-pg-file", file}, "-step is required"},
		{[]string{"-step", dir, "-pg-file", file}, `-phase is ""`},
		{[]string{"-step", dir, "-phase", "middle", "-pg-file", file}, `-phase is "middle"`},
		{[]string{"-step", dir, "-phase", "start"}, "-pg or -pg-file is required"},
		{[]string{"-step", dir, "-phase", "start", "-pg", "not-a-url"}, "is not a Postgres URL"},
	} {
		if err := runPGSize(tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("runPGSize(%v): want an error naming %q, got %v", tc.args, tc.want, err)
		}
	}
}
