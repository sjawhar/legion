package main

// loadgen pg-statements and loadgen pg-size: the readings of R6 that Postgres itself holds and
// that no other command of this package takes -- the window's slowest statements, its lock waits,
// and the database's size at the step's start and at its end. Cloud Monitoring gives the
// instance's CPU and memory from outside (cloudsqlcpu.go); these two read the server.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgStatementsExtension is the extension the window's statements come from. The load-test
// instance carries it as a database flag (pg_stat_statements.track=all), so a database that does
// not hold it is a deployment that was not made as it should have been, and the watcher says so
// by name rather than recording a step with no statement.
const pgStatementsExtension = "pg_stat_statements"

// pgLockInterval is how often the watcher reads pg_locks. The run's own ungranted-lock sampler
// reads every second (locksampler.go) and this read is the heavier of the two -- it joins
// pg_stat_activity twice, for the statement at each end of the wait -- so it reads half as often
// beside it, which still sees a wait of a few seconds and costs the instance little.
const pgLockInterval = 2 * time.Second

// pgTopStatements is how many statements the file keeps, slowest by total time first.
const pgTopStatements = 50

// pgStatementTextMax and pgLockQueryTextMax are how much of a statement's text is kept: the whole
// normalized statement for pg_stat_statements' own rows, and one line of it for the two ends of a
// lock wait, where the point is which statement waited, not its whole text.
const (
	pgStatementTextMax = 2000
	pgLockQueryTextMax = 200
)

// pgReadTimeout is how long the reads around the window are given: the reset and the sizes are
// single rows, and the statements are one indexed read of a view.
const pgReadTimeout = 30 * time.Second

// pgLockReadTimeout bounds one read of pg_locks. It is generous beside the interval: the read
// takes tens of milliseconds with tens of waiters, and a read this bound cancels is a server
// that stopped answering, not a slow window.
const pgLockReadTimeout = 10 * time.Second

// pgStatementRow is one row of pg_stat_statements as the file keeps it. The optional figures are
// pointers: a version of the extension that does not offer a column records null for it, which a
// reader tells from a measured zero.
type pgStatementRow struct {
	Query          string  `json:"query"`
	QueryID        *int64  `json:"queryid"`
	Calls          int64   `json:"calls"`
	TotalMs        float64 `json:"total_ms"`
	MeanMs         float64 `json:"mean_ms"`
	Rows           *int64  `json:"rows"`
	SharedBlksRead *int64  `json:"shared_blks_read"`
	SharedBlksHit  *int64  `json:"shared_blks_hit"`
}

// pgStatementsReset records the reset at the window's start: the call the catalogue offered, what
// it returned, and the error when it failed. A step whose reset failed holds statements that
// began before it, which is a different reading from a step with few statements, so the file says
// which it is.
type pgStatementsReset struct {
	Call       string    `json:"call"`
	ResultType string    `json:"result_type"`
	OK         bool      `json:"ok"`
	At         time.Time `json:"at"`
	Returned   string    `json:"returned"`
	Error      string    `json:"error"`
}

// pgLockWaitGroup is one (lock, waiter, blocker) of the window's lock waits, as the samples saw
// it. A wait that is over leaves no row in pg_locks, so these come from sampling: Observations is
// how many reads saw this wait, and SeenForS is how long it was in view, a lower bound on its
// length. WaitMsMax and WaitMsMean are the real thing where the server offers it --
// pg_locks.waitstart, which Postgres 14 added -- and null where it does not.
type pgLockWaitGroup struct {
	LockType     string    `json:"lock_type"`
	Target       string    `json:"target"`
	Mode         string    `json:"mode"`
	WaiterQuery  string    `json:"waiter_query"`
	BlockerQuery string    `json:"blocker_query"`
	Observations int       `json:"observations"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	SeenForS     float64   `json:"seen_for_s"`
	WaitMsMax    *float64  `json:"wait_ms_max"`
	WaitMsMean   *float64  `json:"wait_ms_mean"`
}

// pgStatementsOut is a step's pg-statements.json.
type pgStatementsOut struct {
	Database         string    `json:"database"`
	ServerVersion    string    `json:"server_version"`
	Extension        string    `json:"extension"`
	ExtensionVersion string    `json:"extension_version"`
	StartedAt        time.Time `json:"started_at"`
	EndedAt          time.Time `json:"ended_at"`
	WindowS          float64   `json:"window_s"`
	// Reset is the reset at StartedAt, so the statements below are this step's own.
	Reset pgStatementsReset `json:"reset"`
	// Columns are the columns of pg_stat_statements this server's extension offers, read from
	// the catalogue: which of them the rows carry follows from it.
	Columns    []string         `json:"columns"`
	Statements []pgStatementRow `json:"statements"`
	// The lock-wait sampling: how many reads it took, how many failed, how often it read, and
	// where each wait's length comes from.
	LockSamples        int               `json:"lock_samples"`
	LockSampleFailures int               `json:"lock_sample_failures"`
	LockIntervalS      float64           `json:"lock_interval_s"`
	LockWaitSource     string            `json:"lock_wait_source"`
	LockWaits          []pgLockWaitGroup `json:"lock_waits"`
}

// pgSizeOut is a step's pg-size.json. The end is written by the second phase, so a step whose end
// phase never ran holds null there: a reader tells that from a database that did not grow.
type pgSizeOut struct {
	Database   string     `json:"database"`
	StartAt    time.Time  `json:"start_at"`
	StartBytes int64      `json:"start_bytes"`
	EndAt      *time.Time `json:"end_at"`
	EndBytes   *int64     `json:"end_bytes"`
}

// --- what these commands read from Postgres ----------------------------------------------------

// pgResetFunc is one pg_stat_statements_reset the catalogue holds: its name as a call takes it,
// its signature, how many arguments it takes and how many of those have defaults, and what it
// returns. The extension's reset grew arguments in 1.5 and a return value in 1.11, so the call
// and the read of its answer both follow from this rather than from the version.
type pgResetFunc struct {
	Name      string
	Signature string
	NArgs     int
	NDefaults int
	Result    string
}

// callableWithNoArgs is true when the function takes no argument that has no default, which is
// how every version of the extension since 1.5 offers pg_stat_statements_reset().
func (f pgResetFunc) callableWithNoArgs() bool { return f.NArgs-f.NDefaults <= 0 }

// pgStatementColumns are the columns of pg_stat_statements this server's extension offers, as the
// read of it uses them: the extension renamed total_time and mean_time to total_exec_time and
// mean_exec_time in 1.8, and the rest are there or not.
type pgStatementColumns struct {
	Total, Mean                     string
	HasRows, HasSharedRead          bool
	HasSharedHit, HasQueryID, HasDB bool
}

// pgLockObservation is one ungranted lock a sample saw.
type pgLockObservation struct {
	LockType     string
	Mode         string
	Target       string
	WaiterQuery  string
	BlockerQuery string
	// WaitMs is how long the waiter had waited when the sample read it, from pg_locks.waitstart;
	// nil on a server whose pg_locks has no waitstart.
	WaitMs *float64
}

// pgLockSample is one read of every ungranted lock.
type pgLockSample struct {
	At    time.Time
	Waits []pgLockObservation
}

// pgStatsDB is the Postgres these collectors read. It is an interface so the commands' own
// behaviour -- the catalogue-driven choice of columns and of the reset call, the sampling, the
// summaries and every file they write -- is tested without a server (pgstats_test.go), while the
// one implementation below holds the SQL.
type pgStatsDB interface {
	database(ctx context.Context) (string, error)
	serverVersion(ctx context.Context) (string, error)
	extensionVersion(ctx context.Context, name string) (string, error)
	relationColumns(ctx context.Context, relation string) ([]string, error)
	resetFunctions(ctx context.Context) ([]pgResetFunc, error)
	callReset(ctx context.Context, f pgResetFunc) (string, error)
	topStatements(ctx context.Context, cols pgStatementColumns, top int) ([]pgStatementRow, error)
	lockWaits(ctx context.Context, waitstart bool) ([]pgLockObservation, error)
	databaseSize(ctx context.Context) (int64, error)
	close()
}

// openPGStats connects to the step's database. It is a variable so a test runs the whole command
// against a fake server.
var openPGStats = func(ctx context.Context, url string) (pgStatsDB, error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return nil, err
	}
	return &pgxStatsDB{conn: conn}, nil
}

// pgxStatsDB is pgStatsDB over one direct connection.
type pgxStatsDB struct{ conn *pgx.Conn }

func (d *pgxStatsDB) close() { d.conn.Close(context.Background()) }

func (d *pgxStatsDB) database(ctx context.Context) (string, error) {
	var name string
	err := d.conn.QueryRow(ctx, `select current_database()`).Scan(&name)
	return name, err
}

func (d *pgxStatsDB) serverVersion(ctx context.Context) (string, error) {
	var v string
	err := d.conn.QueryRow(ctx, `select current_setting('server_version')`).Scan(&v)
	return v, err
}

// extensionVersion is the installed version of the extension, or "" when the database does not
// hold it.
func (d *pgxStatsDB) extensionVersion(ctx context.Context, name string) (string, error) {
	var v string
	err := d.conn.QueryRow(ctx, `select extversion from pg_catalog.pg_extension where extname = $1`, name).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// relationColumns are a relation's column names in order, from the catalogue.
func (d *pgxStatsDB) relationColumns(ctx context.Context, relation string) ([]string, error) {
	rows, err := d.conn.Query(ctx, `select a.attname from pg_catalog.pg_attribute a
where a.attrelid = $1::regclass and a.attnum > 0 and not a.attisdropped
order by a.attnum`, relation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

// resetFunctions are the pg_stat_statements_reset functions the catalogue holds.
func (d *pgxStatsDB) resetFunctions(ctx context.Context) ([]pgResetFunc, error) {
	rows, err := d.conn.Query(ctx, `select p.oid::regproc::text, p.oid::regprocedure::text,
       p.pronargs, coalesce(p.pronargdefaults, 0), pg_catalog.pg_get_function_result(p.oid)
from pg_catalog.pg_proc p
where p.proname = 'pg_stat_statements_reset'
order by p.pronargs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fs []pgResetFunc
	for rows.Next() {
		var f pgResetFunc
		if err := rows.Scan(&f.Name, &f.Signature, &f.NArgs, &f.NDefaults, &f.Result); err != nil {
			return nil, err
		}
		fs = append(fs, f)
	}
	return fs, rows.Err()
}

// callReset calls the reset and returns what it gave back as text. A reset that returns void
// (every version before 1.11) is run as a statement and returns "": there is nothing to read, and
// a cast of void to text is an error of its own.
func (d *pgxStatsDB) callReset(ctx context.Context, f pgResetFunc) (string, error) {
	call := pgResetCall(f)
	if strings.EqualFold(strings.TrimSpace(f.Result), "void") {
		_, err := d.conn.Exec(ctx, "select "+call)
		return "", err
	}
	var returned string
	err := d.conn.QueryRow(ctx, "select ("+call+")::text").Scan(&returned)
	return returned, err
}

func (d *pgxStatsDB) topStatements(ctx context.Context, cols pgStatementColumns, top int) ([]pgStatementRow, error) {
	rows, err := d.conn.Query(ctx, pgStatementsQuery(cols), top)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pgStatementRow
	for rows.Next() {
		var r pgStatementRow
		dest := []any{&r.Query, &r.Calls, &r.TotalMs, &r.MeanMs}
		if cols.HasRows {
			dest = append(dest, &r.Rows)
		}
		if cols.HasSharedRead {
			dest = append(dest, &r.SharedBlksRead)
		}
		if cols.HasSharedHit {
			dest = append(dest, &r.SharedBlksHit)
		}
		if cols.HasQueryID {
			dest = append(dest, &r.QueryID)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *pgxStatsDB) lockWaits(ctx context.Context, waitstart bool) ([]pgLockObservation, error) {
	rows, err := d.conn.Query(ctx, pgLockWaitsQuery(waitstart), commitOrderLockClassID, commitOrderLockObjID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pgLockObservation
	for rows.Next() {
		var o pgLockObservation
		if err := rows.Scan(&o.LockType, &o.Mode, &o.Target, &o.WaiterQuery, &o.BlockerQuery, &o.WaitMs); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (d *pgxStatsDB) databaseSize(ctx context.Context) (int64, error) {
	var size int64
	err := d.conn.QueryRow(ctx, `select pg_catalog.pg_database_size(current_database())`).Scan(&size)
	return size, err
}

// --- the catalogue's answers, turned into the reads -------------------------------------------

// pgStatementColumnsFrom is what the read of pg_stat_statements may ask for, from the view's own
// columns. The extension renamed its time columns in 1.8, so neither name is assumed: the one
// this server offers is used, and a view with neither is refused by the names it does offer.
func pgStatementColumnsFrom(cols []string) (pgStatementColumns, error) {
	has := func(name string) bool { return slices.Contains(cols, name) }
	c := pgStatementColumns{
		HasRows:       has("rows"),
		HasSharedRead: has("shared_blks_read"),
		HasSharedHit:  has("shared_blks_hit"),
		HasQueryID:    has("queryid"),
		HasDB:         has("dbid"),
	}
	switch {
	case has("total_exec_time"):
		c.Total = "total_exec_time"
	case has("total_time"):
		c.Total = "total_time"
	}
	switch {
	case has("mean_exec_time"):
		c.Mean = "mean_exec_time"
	case has("mean_time"):
		c.Mean = "mean_time"
	}
	if !has("query") || !has("calls") || c.Total == "" {
		return c, fmt.Errorf("the %s view of this server offers no query, calls and total time to read (its columns: %s)",
			pgStatementsExtension, strings.Join(cols, ", "))
	}
	return c, nil
}

// pgStatementsQuery reads the slowest statements of the current database, the one that took the
// most time over the window first. $1 is how many rows to keep.
func pgStatementsQuery(c pgStatementColumns) string {
	var b strings.Builder
	fmt.Fprintf(&b, "select left(regexp_replace(query, '\\s+', ' ', 'g'), %d), calls, %s, ", pgStatementTextMax, c.Total)
	if c.Mean != "" {
		b.WriteString(c.Mean)
	} else {
		// A view with no mean column still has both figures the mean is: calls is never 0 in a
		// row pg_stat_statements kept.
		fmt.Fprintf(&b, "%s / nullif(calls, 0)", c.Total)
	}
	for _, col := range []struct {
		has  bool
		name string
	}{{c.HasRows, "rows"}, {c.HasSharedRead, "shared_blks_read"}, {c.HasSharedHit, "shared_blks_hit"}, {c.HasQueryID, "queryid"}} {
		if col.has {
			b.WriteString(", " + col.name)
		}
	}
	b.WriteString("\nfrom " + pgStatementsExtension)
	if c.HasDB {
		// The instance carries more than this step's database; the step's statements are its own.
		b.WriteString("\nwhere dbid = (select oid from pg_database where datname = current_database())")
	}
	fmt.Fprintf(&b, "\norder by %s desc\nlimit $1", c.Total)
	return b.String()
}

// pgResetCall is the call of f: with no argument where the server takes none, and with the three
// zeroes that mean "every user, every database, every statement" where it insists on them.
func pgResetCall(f pgResetFunc) string {
	if f.callableWithNoArgs() {
		return f.Name + "()"
	}
	return f.Name + "(0, 0, 0)"
}

// pgResetFuncFor is the reset this server offers: the one that takes no argument, else the one
// that takes the three the extension's 1.5 added. A server that offers neither is refused by
// name, since a step whose statistics were not reset holds statements of the steps before it.
func pgResetFuncFor(fs []pgResetFunc) (pgResetFunc, error) {
	for _, f := range fs {
		if f.callableWithNoArgs() {
			return f, nil
		}
	}
	for _, f := range fs {
		if f.NArgs == 3 {
			return f, nil
		}
	}
	if len(fs) == 0 {
		return pgResetFunc{}, fmt.Errorf("this server has no pg_stat_statements_reset function")
	}
	sigs := make([]string, 0, len(fs))
	for _, f := range fs {
		sigs = append(sigs, f.Signature)
	}
	return pgResetFunc{}, fmt.Errorf("no pg_stat_statements_reset of this server can be called with no argument: %s", strings.Join(sigs, ", "))
}

// pgLockWaitsQuery reads every ungranted heavyweight lock once, with the statement at each end of
// the wait. Like the run's own sampler it names each waiter's blocker from the same scan -- a
// granted holder of the same lock, the lowest pid -- and never calls pg_blocking_pids, which
// takes exclusive access to the lock manager's shared state (locksampler.go). $1 and $2 are the
// commit-order lock's classid and objid. waitstart says whether pg_locks has the column Postgres
// 14 added, which the catalogue answers.
func pgLockWaitsQuery(waitstart bool) string {
	wait := "null::float8"
	if waitstart {
		wait = "case when w.waitstart is null then null else extract(epoch from (clock_timestamp() - w.waitstart)) * 1000 end"
	}
	cols := ""
	if waitstart {
		cols = ", waitstart"
	}
	return fmt.Sprintf(`with l as materialized (
  select locktype, mode, pid, granted, database, relation, page, tuple, virtualxid, transactionid,
         classid, objid, objsubid%s
  from pg_locks
),
w as (
  select l.*,
    (select h.pid from l h
     where h.granted and h.pid is distinct from l.pid and h.locktype = l.locktype
       and h.database is not distinct from l.database and h.relation is not distinct from l.relation
       and h.page is not distinct from l.page and h.tuple is not distinct from l.tuple
       and h.virtualxid is not distinct from l.virtualxid
       and h.transactionid is not distinct from l.transactionid
       and h.classid is not distinct from l.classid and h.objid is not distinct from l.objid
       and h.objsubid is not distinct from l.objsubid
     order by h.pid limit 1) as blocker
  from l
  where not l.granted
)
select w.locktype, w.mode,
  case
    when w.locktype = 'advisory' and w.classid = $1 and w.objid = $2 then 'commit_order_lock'
    when w.locktype = 'advisory' then w.classid::text || ':' || w.objid::text
    when w.locktype = 'object' and w.classid = 'pg_database'::regclass and w.objid = 0 then 'notify_commit_lock'
    when w.locktype in ('relation', 'tuple') and w.database in (0, (select oid from pg_database where datname = current_database())) then w.relation::regclass::text
    when w.locktype = 'transactionid' then 'row_wait'
    else concat_ws(':', w.database, w.relation, w.page, w.tuple, w.virtualxid, w.transactionid, w.classid, w.objid, w.objsubid)
  end,
  coalesce(left(regexp_replace(a.query, '\s+', ' ', 'g'), %d), ''),
  coalesce(left(regexp_replace(b.query, '\s+', ' ', 'g'), %d), ''),
  %s
from w
left join pg_stat_activity a on a.pid = w.pid
left join pg_stat_activity b on b.pid = w.blocker`, cols, pgLockQueryTextMax, pgLockQueryTextMax, wait)
}

// summarizePGLockWaits groups the samples' waits by the lock and the two statements, longest wait
// first. A wait no sample saw is not here: a wait that is over leaves no row in pg_locks, which
// is why the watcher samples rather than reading once.
func summarizePGLockWaits(samples []pgLockSample, interval time.Duration) []pgLockWaitGroup {
	type key struct{ lockType, target, mode, waiter, blocker string }
	index := map[key]int{}
	groups := make([]pgLockWaitGroup, 0, 8)
	sums := map[int]float64{}
	counts := map[int]int{}
	for _, s := range samples {
		for _, w := range s.Waits {
			k := key{w.LockType, w.Target, w.Mode, w.WaiterQuery, w.BlockerQuery}
			i, ok := index[k]
			if !ok {
				i = len(groups)
				index[k] = i
				groups = append(groups, pgLockWaitGroup{
					LockType: w.LockType, Target: w.Target, Mode: w.Mode,
					WaiterQuery: w.WaiterQuery, BlockerQuery: w.BlockerQuery,
					FirstSeen: s.At,
				})
			}
			g := &groups[i]
			g.Observations++
			g.LastSeen = s.At
			if w.WaitMs != nil {
				sums[i] += *w.WaitMs
				counts[i]++
				if g.WaitMsMax == nil || *w.WaitMs > *g.WaitMsMax {
					g.WaitMsMax = new(*w.WaitMs)
				}
			}
		}
	}
	for i := range groups {
		g := &groups[i]
		// One sample sees a wait that lasted at least up to that read; the interval is what a
		// second sighting adds to what is known.
		g.SeenForS = g.LastSeen.Sub(g.FirstSeen).Seconds() + interval.Seconds()
		if n := counts[i]; n > 0 {
			g.WaitMsMean = new(sums[i] / float64(n))
		}
	}
	sort.SliceStable(groups, func(a, b int) bool {
		x, y := groups[a], groups[b]
		xw, yw := 0.0, 0.0
		if x.WaitMsMax != nil {
			xw = *x.WaitMsMax
		}
		if y.WaitMsMax != nil {
			yw = *y.WaitMsMax
		}
		if xw != yw {
			return xw > yw
		}
		if x.Observations != y.Observations {
			return x.Observations > y.Observations
		}
		return x.Target < y.Target
	})
	return groups
}

// --- loadgen pg-statements --------------------------------------------------------------------

// pgStatementsOptions are the watcher's settings, as its flags give them.
type pgStatementsOptions struct {
	top      int
	interval time.Duration
	window   time.Duration
	lg       *log.Logger
}

// collectPGStatements resets the statistics, samples the lock waits until ctx ends or the window
// is over, and then reads the window's statements. The refusals are the two a reader could not
// tell from a quiet step: a database with no pg_stat_statements, and a view this read cannot be
// built from. A reset that failed is recorded and the watcher runs on, so the file says the
// statements are not this step's alone rather than the step looking idle.
func collectPGStatements(ctx context.Context, db pgStatsDB, opt pgStatementsOptions) (pgStatementsOut, error) {
	out := pgStatementsOut{
		Extension:     pgStatementsExtension,
		StartedAt:     time.Now().UTC(),
		LockIntervalS: opt.interval.Seconds(),
		Statements:    []pgStatementRow{},
		LockWaits:     []pgLockWaitGroup{},
	}
	setup, cancel := context.WithTimeout(ctx, pgReadTimeout)
	defer cancel()
	var err error
	if out.Database, err = db.database(setup); err != nil {
		return out, err
	}
	if out.ServerVersion, err = db.serverVersion(setup); err != nil {
		return out, err
	}
	if out.ExtensionVersion, err = db.extensionVersion(setup, pgStatementsExtension); err != nil {
		return out, err
	}
	if out.ExtensionVersion == "" {
		return out, fmt.Errorf("the database %s has no %s extension, so this step has no slowest statements to record: the instance carries it as the database flags %s.track and %s.max, but a database it was never created in holds no view, and a database made from a template holds only what that template holds; run `create extension if not exists %s` in %s (and in the template the reset copies) before the step",
			out.Database, pgStatementsExtension, pgStatementsExtension, pgStatementsExtension, pgStatementsExtension, out.Database)
	}
	if out.Columns, err = db.relationColumns(setup, pgStatementsExtension); err != nil {
		return out, err
	}
	cols, err := pgStatementColumnsFrom(out.Columns)
	if err != nil {
		return out, err
	}
	lockCols, err := db.relationColumns(setup, "pg_locks")
	if err != nil {
		return out, err
	}
	waitstart := slices.Contains(lockCols, "waitstart")
	out.LockWaitSource = "the samples' count and span"
	if waitstart {
		out.LockWaitSource = "pg_locks.waitstart"
	}
	out.Reset = pgResetStatistics(setup, db)
	if !out.Reset.OK {
		opt.lg.Printf("the reset of %s failed, so the statements below began before this step: %s", pgStatementsExtension, out.Reset.Error)
	}

	// The window: every pgLockInterval until the step ends, which is a signal or -window.
	start := time.Now()
	until := start.Add(365 * 24 * time.Hour)
	if opt.window > 0 {
		until = start.Add(opt.window)
	}
	samples := make([]pgLockSample, 0, 1024)
	out.LockSampleFailures = poll(ctx, start, until, opt.interval, "pg-statements lock waits", opt.lg, func(context.Context) error {
		// The read has a context of its own rather than the window's: a signal that reached the
		// watcher while a query was in flight would cancel it, and pgx closes a connection whose
		// query was cancelled -- the very connection the statements are read over once the
		// window has ended. The loop still stops on the signal, at its next sleep.
		read, cancel := context.WithTimeout(context.Background(), pgLockReadTimeout)
		defer cancel()
		waits, err := db.lockWaits(read, waitstart)
		if err != nil {
			return err
		}
		samples = append(samples, pgLockSample{At: time.Now().UTC(), Waits: waits})
		return nil
	})
	out.LockSamples = len(samples)
	out.LockWaits = append(out.LockWaits, summarizePGLockWaits(samples, opt.interval)...)

	// The window has ended, so ctx is done; the last read has a context of its own.
	end, endCancel := context.WithTimeout(context.Background(), pgReadTimeout)
	defer endCancel()
	statements, err := db.topStatements(end, cols, opt.top)
	if err != nil {
		return out, err
	}
	out.Statements = append(out.Statements, statements...)
	out.EndedAt = time.Now().UTC()
	out.WindowS = out.EndedAt.Sub(out.StartedAt).Seconds()
	return out, nil
}

// pgResetStatistics resets pg_stat_statements by the call this server's catalogue offers, and
// records the attempt whether it worked or not.
func pgResetStatistics(ctx context.Context, db pgStatsDB) pgStatementsReset {
	r := pgStatementsReset{At: time.Now().UTC()}
	fs, err := db.resetFunctions(ctx)
	if err != nil {
		r.Error = fmt.Sprintf("the catalogue's pg_stat_statements_reset could not be read: %v", err)
		return r
	}
	f, err := pgResetFuncFor(fs)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	r.Call, r.ResultType = pgResetCall(f), f.Result
	r.At = time.Now().UTC()
	returned, err := db.callReset(ctx, f)
	if err != nil {
		r.Error = fmt.Sprintf("%s failed: %v", r.Call, err)
		return r
	}
	r.OK, r.Returned = true, returned
	return r
}

func runPGStatements(args []string) error {
	fs := flag.NewFlagSet("pg-statements", flag.ExitOnError)
	step := fs.String("step", "", "the step directory the file is written in (required)")
	out := fs.String("out", "", "where the JSON is written (default <step>/"+pgStatementsFileName+")")
	top := fs.Int("top", pgTopStatements, "how many statements to keep, the slowest by total time first")
	interval := fs.Duration("interval", pgLockInterval, "how often pg_locks is read for the step's lock waits")
	window := fs.Duration("window", 0, "how long to sample before writing the file; 0 samples until SIGTERM or SIGINT")
	var pg, pgFile string
	fs.StringVar(&pg, "pg", "", "direct Postgres URL of the step's database (required, or -pg-file)")
	fs.StringVar(&pgFile, "pg-file", "", "file holding the -pg value, so no password reaches the command line")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen pg-statements -step <step dir> (-pg <postgres url> | -pg-file <path>) [-top 50] [-interval 2s] [-window 0]")
		fmt.Fprintln(fs.Output(), "Resets "+pgStatementsExtension+" at the step's start, samples the lock waits of pg_locks")
		fmt.Fprintln(fs.Output(), "until SIGTERM, and then writes the window's slowest statements and those waits as "+pgStatementsFileName+".")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if err := readCredentials(credentialFile{"-pg", "-pg-file", pgFile, &pg}); err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case *step == "":
		return errors.New("-step is required: the step directory the file is written in")
	case pg == "":
		return errors.New("-pg or -pg-file is required: the step's database")
	case *top <= 0:
		return fmt.Errorf("-top must be above 0, not %d", *top)
	case *interval <= 0:
		return fmt.Errorf("-interval must be above 0, not %s", *interval)
	case *window < 0:
		return fmt.Errorf("-window must not be below 0, not %s", *window)
	}
	if _, ok := parsePG(pg); !ok {
		return fmt.Errorf("-pg %s is not a Postgres URL", redactPG(pg))
	}
	if *out == "" {
		*out = filepath.Join(*step, pgStatementsFileName)
	}
	lg := log.New(os.Stderr, "loadgen pg-statements: ", log.LstdFlags|log.LUTC)

	// The step ends the watcher with a signal, as it ends the cgroup sampler (vm/vm.sh).
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	connect, cancel := context.WithTimeout(ctx, pgReadTimeout)
	defer cancel()
	db, err := openPGStats(connect, pg)
	if err != nil {
		return err
	}
	defer db.close()

	f, err := collectPGStatements(ctx, db, pgStatementsOptions{top: *top, interval: *interval, window: *window, lg: lg})
	if err != nil {
		return err
	}
	if err := writePGJSON(*out, f); err != nil {
		return err
	}
	lg.Printf("wrote %s: %d statements and %d lock waits of %s over %.0f s, from %d samples (%d failed)",
		*out, len(f.Statements), len(f.LockWaits), f.Database, f.WindowS, f.LockSamples, f.LockSampleFailures)
	return nil
}

// --- loadgen pg-size ----------------------------------------------------------------------------

// pgSizePhaseStart and pgSizePhaseEnd are -phase's two values: the size before the load and the
// size after it, which R6 asks for as the database's size at the start and the end.
const (
	pgSizePhaseStart = "start"
	pgSizePhaseEnd   = "end"
)

func runPGSize(args []string) error {
	fs := flag.NewFlagSet("pg-size", flag.ExitOnError)
	step := fs.String("step", "", "the step directory the file is written in (required)")
	out := fs.String("out", "", "where the JSON is written (default <step>/"+pgSizeFileName+")")
	phase := fs.String("phase", "", "start, before the load, or end, after it (required)")
	var pg, pgFile string
	fs.StringVar(&pg, "pg", "", "direct Postgres URL of the step's database (required, or -pg-file)")
	fs.StringVar(&pgFile, "pg-file", "", "file holding the -pg value, so no password reaches the command line")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: loadgen pg-size -step <step dir> -phase start|end (-pg <postgres url> | -pg-file <path>)")
		fmt.Fprintln(fs.Output(), "Writes the database's size into "+pgSizeFileName+": -phase start writes start_bytes, -phase end adds end_bytes.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if err := readCredentials(credentialFile{"-pg", "-pg-file", pgFile, &pg}); err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	case *step == "":
		return errors.New("-step is required: the step directory the file is written in")
	case *phase != pgSizePhaseStart && *phase != pgSizePhaseEnd:
		return fmt.Errorf("-phase is %q; it is %s, before the load, or %s, after it", *phase, pgSizePhaseStart, pgSizePhaseEnd)
	case pg == "":
		return errors.New("-pg or -pg-file is required: the step's database")
	}
	if _, ok := parsePG(pg); !ok {
		return fmt.Errorf("-pg %s is not a Postgres URL", redactPG(pg))
	}
	if *out == "" {
		*out = filepath.Join(*step, pgSizeFileName)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pgReadTimeout)
	defer cancel()
	db, err := openPGStats(ctx, pg)
	if err != nil {
		return err
	}
	defer db.close()
	name, err := db.database(ctx)
	if err != nil {
		return err
	}
	size, err := db.databaseSize(ctx)
	if err != nil {
		return err
	}
	f, err := pgSizeRecord(*out, *phase, name, size, time.Now().UTC())
	if err != nil {
		return err
	}
	if err := writePGJSON(*out, f); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "loadgen pg-size: wrote %s: %s of %s is %d bytes (%.0f MiB)\n",
		*out, *phase, name, size, mib(float64(size)))
	return nil
}

// pgSizeRecord is the file the phase writes: the start phase begins it, and the end phase adds
// its size to the start phase's file. An end phase with no file to add to is refused by the path
// it looked in: a start_bytes of 0 is no reading, and a file holding one would read as a step
// whose database was empty when it began.
func pgSizeRecord(path, phase, database string, size int64, at time.Time) (pgSizeOut, error) {
	if phase == pgSizePhaseStart {
		return pgSizeOut{Database: database, StartAt: at, StartBytes: size}, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return pgSizeOut{}, fmt.Errorf("-phase %s has no %s to add the end size to: %w; the step runs -phase %s before the load",
			pgSizePhaseEnd, path, err, pgSizePhaseStart)
	}
	var f pgSizeOut
	if err := json.Unmarshal(b, &f); err != nil {
		return pgSizeOut{}, fmt.Errorf("%s is not a size record: %w", path, err)
	}
	if f.StartBytes <= 0 {
		return pgSizeOut{}, fmt.Errorf("%s holds no start size, so this step has no size at its start to compare the end with", path)
	}
	f.EndAt, f.EndBytes = new(at), new(size)
	return f, nil
}

// writePGJSON writes one of these files: indented JSON with a final newline, as every other file
// of a step directory is written.
func writePGJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
