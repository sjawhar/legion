package pgmigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// CensusTableLimit is the largest pg_total_relation_size (table, indexes and TOAST) of an
// existing table a pending migration may touch before Census refuses it. A migration's
// statements lock the tables they name for as long as they run - a validating constraint scans
// the whole table under ACCESS EXCLUSIVE - so a table this large is a migration that needs
// another shape (CONCURRENTLY, batches), which is what the refusal forces. One constant, no
// per-migration override; raising it is a reviewed edit when a real table crosses it.
const CensusTableLimit int64 = 1 << 30

// CensusLongTransaction is how long a transaction holding a lock on a table a pending migration
// touches may have been open before Census refuses: the migration would give up on that lock
// after LockTimeout anyway, during the rollout, and refusing now saves it. A minute rather than
// LockTimeout, because the census runs minutes before the migration and because the nightly
// dump holds ACCESS SHARE for about that long. Transactions that old anywhere in the database
// are reported too, without refusing.
const CensusLongTransaction = time.Minute

// CensusStatementTimeout bounds each statement the census runs, the migrations' own censuses
// included.
const CensusStatementTimeout = time.Minute

// CensusOptions configures Census. VersionTable is required; a zero TableLimit or
// LongTransaction takes the package constant.
type CensusOptions struct {
	VersionTable    string
	TableLimit      int64
	LongTransaction time.Duration
}

// Session is a database session the census reports: never its query text (the rule LockHolder
// states). XactSeconds is how long its transaction has been open, nil when Postgres hides it:
// pg_stat_activity shows another role's xact_start only to superusers and pg_read_all_stats,
// and the census runs as the service's own role, which sees its own sessions.
type Session struct {
	PID         uint32
	User        string
	Application string
	State       string
	XactSeconds *int64
}

// TableCensus is one table a pending migration touches.
type TableCensus struct {
	Name   string
	Exists bool
	// Unread is a table whose size and rows the census gave up reading after LockTimeout, since
	// another session holds a lock every read waits behind; the refusal names the holders.
	Unread bool
	Rows   int64
	Bytes  int64
	Size   string
	// Holders are the other sessions holding a granted relation lock on the table.
	Holders []Session
}

// MigrationCensus is the census of one pending migration.
type MigrationCensus struct {
	Migration Migration
	Tables    []TableCensus
	// Count is what the migration's census answered; nil when it declares none, could not be
	// answered at this schema, or failed.
	Count *int64
	// NotAnswerable is Postgres's message when the census named a table or column the database
	// does not have yet behind an earlier pending migration, so the census refuses nothing: the
	// rows it would count cannot exist yet.
	NotAnswerable string
}

// Report is what Census found, with the bounds it applied.
type Report struct {
	VersionTable     string
	SchemaVersion    int
	TableLimit       int64
	LongTransaction  time.Duration
	Pending          []MigrationCensus
	LongTransactions []Session
	// Refusals each begin with the name of the migration they refuse and a colon.
	Refusals []string
}

// Refused reports whether anything refuses the pending migrations.
func (r *Report) Refused() bool { return len(r.Refusals) > 0 }

func (r *Report) refuse(format string, args ...any) {
	r.Refusals = append(r.Refusals, fmt.Sprintf(format, args...))
}

// Census reads, in one repeatable-read read-only transaction on conn, what a deployment must know
// before the migrations the database has not applied run: for each pending migration the tables
// its statements touch with their row count, total size and the sessions holding locks on them,
// and the count its own census answers; and every transaction open longer than
// LongTransaction. A pending migration is one whose version is above the highest in
// VersionTable; a database without that table is at version 0. It refuses (Report.Refusals, not an
// error) a touched table above TableLimit or one it cannot read within LockTimeout, a lock holder
// whose transaction is at least LongTransaction old, a census that answers non-zero, and a census
// that fails or answers anything but one integer - except one behind an earlier pending migration
// that names a table or column the database does not have yet, which is reported as not
// answerable. It returns an error only when the census itself could not be taken. It writes
// nothing: the transaction is read-only, and a migration's census is sent through the extended
// protocol, so it cannot end that transaction with a statement of its own (censusCount).
//
// What the report holds is counts, sizes, versions, file names, session metadata and SQLSTATEs.
// Postgres's own message is kept only where it cannot quote a row (censusCount).
func Census(ctx context.Context, conn *pgx.Conn, migrations []Migration, options CensusOptions) (*Report, error) {
	if options.VersionTable == "" {
		return nil, errors.New("census: VersionTable required")
	}
	if options.TableLimit == 0 {
		options.TableLimit = CensusTableLimit
	}
	if options.LongTransaction == 0 {
		options.LongTransaction = CensusLongTransaction
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("census: begin read-only transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "select set_config('statement_timeout', $1, true), set_config('lock_timeout', $2, true)",
		strconv.FormatInt(CensusStatementTimeout.Milliseconds(), 10)+"ms", lockTimeoutSetting); err != nil {
		return nil, fmt.Errorf("census: set timeouts: %w", err)
	}
	report := &Report{VersionTable: options.VersionTable, TableLimit: options.TableLimit, LongTransaction: options.LongTransaction}
	var versionTableExists bool
	if err := tx.QueryRow(ctx, "select to_regclass($1) is not null", options.VersionTable).Scan(&versionTableExists); err != nil {
		return nil, fmt.Errorf("census: look for %s: %w", options.VersionTable, err)
	}
	if versionTableExists {
		if err := tx.QueryRow(ctx, "select coalesce(max(version), 0) from "+pgx.Identifier{options.VersionTable}.Sanitize()).Scan(&report.SchemaVersion); err != nil {
			return nil, fmt.Errorf("census: read %s: %w", options.VersionTable, err)
		}
	}
	for _, migration := range migrations {
		if migration.Version <= report.SchemaVersion {
			continue
		}
		entry := MigrationCensus{Migration: migration}
		tables := TouchedTables(migration.SQL)
		for _, index := range TouchedIndexes(migration.SQL) {
			var table string
			err := tx.QueryRow(ctx, "select indrelid::regclass::text from pg_index where indexrelid = to_regclass($1)", index).Scan(&table)
			if errors.Is(err, pgx.ErrNoRows) {
				continue // created by a pending migration, whose statement names its table
			}
			if err != nil {
				return nil, fmt.Errorf("census: resolve index %s: %w", index, err)
			}
			tables = append(tables, strings.TrimPrefix(table, "public."))
		}
		slices.Sort(tables)
		tables = slices.Compact(tables)
		for _, name := range tables {
			table, err := censusTable(ctx, tx, name, migration.Name, report)
			if err != nil {
				return nil, err
			}
			entry.Tables = append(entry.Tables, table)
			if table.Exists && !table.Unread && table.Bytes > options.TableLimit {
				report.refuse("%s: %s is %s, above the %s a table a migration touches may be", migration.Name, name, table.Size, humanBytes(options.TableLimit))
			}
			for _, holder := range table.Holders {
				if holder.XactSeconds != nil && time.Duration(*holder.XactSeconds)*time.Second >= options.LongTransaction {
					report.refuse("%s: %s has held a lock on %s for %s, longer than %s", migration.Name, holder, name, seconds(*holder.XactSeconds), options.LongTransaction)
				}
			}
		}
		if migration.Census != "" {
			// A census may name what an earlier pending migration creates, so a missing relation or
			// column is tolerated only behind one; for the first pending migration it is a defect.
			if err := censusCount(ctx, tx, &entry, report, len(report.Pending) > 0); err != nil {
				return nil, err
			}
		}
		report.Pending = append(report.Pending, entry)
	}
	report.LongTransactions, err = sessions(ctx, tx, `
		select pid, coalesce(usename, ''), coalesce(application_name, ''), coalesce(state, ''),
			extract(epoch from now() - xact_start)::bigint
		from pg_stat_activity
		where datname = current_database() and pid <> pg_backend_pid()
			and xact_start is not null and now() - xact_start >= $1::interval
		order by xact_start`, strconv.FormatInt(options.LongTransaction.Milliseconds(), 10)+" milliseconds")
	if err != nil {
		return nil, fmt.Errorf("census: read long transactions: %w", err)
	}
	return report, nil
}

// censusTable reads one touched table. Its holders come from pg_locks, which takes no lock; its
// size and its rows each take ACCESS SHARE on it, so both wait behind a session holding ACCESS
// EXCLUSIVE (another migration, an ALTER) and run in a savepoint: a wait that outlasts LockTimeout
// leaves the transaction usable and is a refusal naming the holders, not an error.
func censusTable(ctx context.Context, tx pgx.Tx, name, migration string, report *Report) (TableCensus, error) {
	table := TableCensus{Name: name}
	var oid *uint32
	if err := tx.QueryRow(ctx, "select to_regclass($1)::oid", name).Scan(&oid); err != nil {
		return table, fmt.Errorf("census: look for %s: %w", name, err)
	}
	if oid == nil {
		return table, nil
	}
	table.Exists = true
	holders, err := sessions(ctx, tx, `
		select distinct a.pid, coalesce(a.usename, ''), coalesce(a.application_name, ''), coalesce(a.state, ''),
			extract(epoch from now() - a.xact_start)::bigint
		from pg_locks l join pg_stat_activity a on a.pid = l.pid
		where l.granted and l.locktype = 'relation' and l.relation = $1
			and l.database = (select oid from pg_database where datname = current_database())
			and l.pid <> pg_backend_pid()
		order by 5 desc nulls last, 1`, *oid)
	if err != nil {
		return table, fmt.Errorf("census: read lock holders of %s: %w", name, err)
	}
	table.Holders = holders
	nested, err := tx.Begin(ctx)
	if err != nil {
		return table, fmt.Errorf("census: savepoint: %w", err)
	}
	defer nested.Rollback(ctx)
	err = nested.QueryRow(ctx, "select pg_total_relation_size($1::oid), pg_size_pretty(pg_total_relation_size($1::oid))", *oid).Scan(&table.Bytes, &table.Size)
	if err == nil {
		err = nested.QueryRow(ctx, "select count(*) from "+pgx.Identifier{name}.Sanitize()).Scan(&table.Rows)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable {
		// The SQLSTATE and the holders the census already read, never Postgres's message: no text
		// a row could reach is printed.
		table.Unread, table.Rows, table.Bytes, table.Size = true, 0, 0, ""
		report.refuse("%s: could not read %s within %s (SQLSTATE %s); locks on it are held by %s", migration, name, LockTimeout, pgErr.Code, sessionList(table.Holders))
		return table, nil
	}
	if err != nil {
		return table, fmt.Errorf("census: read %s: %w", name, err)
	}
	return table, nil
}

// censusCount runs the migration's census inside a savepoint and records what it answered. The
// statement runs with QueryExecModeDescribeExec whatever the connection's default is: through the
// extended protocol Postgres refuses a text holding more than one statement (42601), so a census
// cannot end the read-only transaction with a `commit` of its own, which the simple protocol
// would let it do.
//
// What a failure prints is bounded, because the report goes to logs with a wider audience than
// the database's: the census file's name and the SQLSTATE always; Postgres's message only for
// class 42 (syntax and access-rule errors, which quote the census's own text and identifiers),
// which includes the two "does not exist" codes (identifiers only). A data exception's message
// quotes the row value that failed to cast, and is never printed. A missing relation or column
// (42P01, 42703) refuses nothing when tolerateMissing says an earlier pending migration exists,
// since the census may name what it creates; for the first pending migration it is a defect. An
// error that is not Postgres's (the connection dropped) is returned: the census could not be
// taken.
func censusCount(ctx context.Context, tx pgx.Tx, entry *MigrationCensus, report *Report, tolerateMissing bool) error {
	migration := entry.Migration
	nested, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("census: savepoint for %s: %w", migration.CensusName, err)
	}
	defer nested.Rollback(ctx)
	count, shape, err := readCensus(ctx, nested, migration.Census)
	if err == nil {
		switch {
		case shape != "":
			report.refuse("%s: its census %s (%s); a census answers one integer", migration.Name, shape, migration.CensusName)
		case count == nil:
			report.refuse("%s: its census answered no row (%s); a census answers one integer", migration.Name, migration.CensusName)
		default:
			entry.Count = count
			if *count != 0 {
				report.refuse("%s: its census counts %d (%s); the migration would refuse or rewrite what those rows hold", migration.Name, *count, migration.CensusName)
			}
		}
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("census: run %s: %w", migration.CensusName, err)
	}
	missing := pgErr.Code == "42P01" || pgErr.Code == "42703"
	switch {
	case missing && tolerateMissing:
		entry.NotAnswerable = pgErr.Message
	case missing:
		report.refuse("%s: its census names something the database does not have (%s, SQLSTATE %s: %s); it is the first pending migration, so the census was written against this schema and this is a defect", migration.Name, migration.CensusName, pgErr.Code, pgErr.Message)
	case strings.HasPrefix(pgErr.Code, "42"):
		report.refuse("%s: its census failed (%s, SQLSTATE %s: %s)", migration.Name, migration.CensusName, pgErr.Code, pgErr.Message)
	default:
		report.refuse("%s: its census failed (%s, SQLSTATE %s); the message is not printed, since for this class it can quote a row's value", migration.Name, migration.CensusName, pgErr.Code)
	}
	return nil
}

// readCensus runs one census and reads what it answered: the count, or the shape that makes it
// no count, or the error the statement met. The rows are read before anything is judged about
// their shape: in this exec mode pgx sends no Describe for the portal and fills the field
// descriptions from the first DataRow, so a statement that fails at execution with no row (a data
// exception, a write the read-only transaction refuses) shows zero columns and its error only once
// the rows are closed. The column count is read from each row's values instead.
func readCensus(ctx context.Context, tx pgx.Tx, census string) (count *int64, shape string, err error) {
	rows, err := tx.Query(ctx, census, pgx.QueryExecModeDescribeExec)
	if err != nil {
		return nil, "", err
	}
	for shape == "" && rows.Next() {
		values, valuesErr := rows.Values()
		if valuesErr != nil {
			rows.Close()
			return nil, "", valuesErr
		}
		switch {
		case len(values) != 1:
			shape = fmt.Sprintf("answered %d columns", len(values))
		case values[0] == nil:
			shape = "answered NULL, not an integer"
		case count != nil:
			shape = "answered more than one row"
		default:
			n, ok := integer(values[0])
			if !ok {
				shape = fmt.Sprintf("answered a %T, not an integer", values[0])
				break
			}
			count = &n
		}
	}
	// Closing reads what is left of the result, so an error a later row meets is not missed.
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return count, shape, nil
}

func integer(value any) (int64, bool) {
	switch v := value.(type) {
	case int64:
		return v, true
	case int32:
		return int64(v), true
	case int16:
		return int64(v), true
	}
	return 0, false
}

func sessions(ctx context.Context, tx pgx.Tx, query string, arg any) ([]Session, error) {
	rows, err := tx.Query(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.PID, &s.User, &s.Application, &s.State, &s.XactSeconds); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// String is the session as the report prints it.
func (s Session) String() string {
	age := "age not visible"
	if s.XactSeconds != nil {
		age = "transaction open " + seconds(*s.XactSeconds)
	}
	return fmt.Sprintf("pid %d (%s, %q, %s, %s)", s.PID, s.User, s.Application, s.State, age)
}

func sessionList(sessions []Session) string {
	if len(sessions) == 0 {
		return "none"
	}
	parts := make([]string, len(sessions))
	for i, s := range sessions {
		parts[i] = s.String()
	}
	return strings.Join(parts, "; ")
}

func seconds(n int64) string { return (time.Duration(n) * time.Second).String() }

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

// thousands writes n with , separators: the counts a human compares against a row estimate.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Write prints the report, one census: line per fact, ending with census: ok or
// census: REFUSED (<n> reason(s)). Counts, sizes, versions, file names, session metadata and
// SQLSTATEs only.
func (r *Report) Write(w io.Writer) {
	names := make([]string, len(r.Pending))
	for i, entry := range r.Pending {
		names[i] = entry.Migration.Name
	}
	pending := "no pending migration"
	if len(names) > 0 {
		pending = "pending: " + strings.Join(names, ", ")
	}
	fmt.Fprintf(w, "census: schema version %d (%s); %s\n", r.SchemaVersion, r.VersionTable, pending)
	for _, entry := range r.Pending {
		name := entry.Migration.Name
		if len(entry.Tables) == 0 {
			fmt.Fprintf(w, "census: %s touches no existing table\n", name)
		}
		for _, table := range entry.Tables {
			switch {
			case !table.Exists:
				fmt.Fprintf(w, "census: %s touches %s: not in the database yet\n", name, table.Name)
			case table.Unread:
				fmt.Fprintf(w, "census: %s touches %s: not read, its lock was not granted within %s; locks held by other sessions: %s\n",
					name, table.Name, LockTimeout, sessionList(table.Holders))
			default:
				rows := "rows"
				if table.Rows == 1 {
					rows = "row"
				}
				fmt.Fprintf(w, "census: %s touches %s: %s %s, %s (limit %s); locks held by other sessions: %s\n",
					name, table.Name, thousands(table.Rows), rows, table.Size, humanBytes(r.TableLimit), sessionList(table.Holders))
			}
		}
		switch {
		case entry.Migration.Census == "":
			fmt.Fprintf(w, "census: %s declares no census\n", name)
		case entry.NotAnswerable != "":
			fmt.Fprintf(w, "census: %s census (%s) is not answerable at schema %d, so it refuses nothing: %s\n", name, entry.Migration.CensusName, r.SchemaVersion, entry.NotAnswerable)
		case entry.Count != nil && *entry.Count == 0:
			fmt.Fprintf(w, "census: %s census (%s): 0\n", name, entry.Migration.CensusName)
		}
		for _, refusal := range r.Refusals {
			if strings.HasPrefix(refusal, name+":") {
				fmt.Fprintf(w, "census: REFUSED %s\n", refusal)
			}
		}
	}
	fmt.Fprintf(w, "census: transactions open longer than %s: %s\n", r.LongTransaction, sessionList(r.LongTransactions))
	if r.Refused() {
		reasons := "reasons"
		if len(r.Refusals) == 1 {
			reasons = "reason"
		}
		fmt.Fprintf(w, "census: REFUSED (%d %s)\n", len(r.Refusals), reasons)
		return
	}
	fmt.Fprintln(w, "census: ok")
}

var touchedTablePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)\balter\s+table\s+(?:if\s+exists\s+)?(?:only\s+)?([\w."]+)`),
	regexp.MustCompile(`(?is)\bcreate\s+(?:unique\s+)?index\s+(?:concurrently\s+)?(?:if\s+not\s+exists\s+)?[\w."]+\s+on\s+(?:only\s+)?([\w."]+)`),
	regexp.MustCompile(`(?is)\bdrop\s+table\s+(?:if\s+exists\s+)?([\w."]+(?:\s*,\s*[\w."]+)*)`),
	regexp.MustCompile(`(?is)\btruncate\s+(?:table\s+)?(?:only\s+)?([\w."]+(?:\s*,\s*[\w."]+)*)`),
	regexp.MustCompile(`(?is)\bcreate\s+(?:or\s+replace\s+)?(?:constraint\s+)?trigger\s+[\w."]+.*?\bon\s+([\w."]+)`),
	regexp.MustCompile(`(?is)\bupdate\s+(?:only\s+)?([\w."]+)\s+set\b`),
	regexp.MustCompile(`(?is)\bdelete\s+from\s+(?:only\s+)?([\w."]+)`),
	regexp.MustCompile(`(?is)\binsert\s+into\s+([\w."]+)`),
	regexp.MustCompile(`(?is)\block\s+(?:table\s+)?(?:only\s+)?([\w."]+(?:\s*,\s*[\w."]+)*)`),
	// A foreign key takes SHARE ROW EXCLUSIVE on the table it references, which holds that
	// table's writes for as long as the migration runs.
	regexp.MustCompile(`(?is)\breferences\s+(?:only\s+)?([\w."]+)`),
}

var touchedIndexPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?is)\bdrop\s+index\s+(?:concurrently\s+)?(?:if\s+exists\s+)?([\w."]+)`),
	regexp.MustCompile(`(?is)\balter\s+index\s+(?:if\s+exists\s+)?([\w."]+)`),
}

// TouchedTables names, sorted and once each, the tables a migration's statements take a lock on:
// the targets of alter table, create index … on, drop table, truncate, create trigger … on,
// update, delete from, insert into and lock table, and the table a foreign key references. It
// reads the SQL textually with its comments removed (DO blocks included), lowercases and strips a
// public. qualifier and quotes; the repository's migrations use plain lowercase names, which
// pgmigratetest.CheckTouchedTablesAreKnown holds every set to, so a form this misreads is caught
// there. A table the migration creates itself is reported by Census as not in the database yet.
func TouchedTables(sql string) []string {
	return matchNames(stripSQLComments(sql), touchedTablePatterns)
}

// TouchedIndexes names the indexes a migration drops or alters; Census resolves each to its table.
func TouchedIndexes(sql string) []string {
	return matchNames(stripSQLComments(sql), touchedIndexPatterns)
}

func matchNames(sql string, patterns []*regexp.Regexp) []string {
	var names []string
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllStringSubmatch(sql, -1) {
			for _, raw := range strings.Split(match[1], ",") {
				name := strings.ToLower(strings.Trim(strings.TrimSpace(raw), `"`))
				name = strings.TrimPrefix(name, "public.")
				if name != "" {
					names = append(names, name)
				}
			}
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}
