package pgmigrate

import (
	"context"
	"errors"
	"fmt"
	"io"
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
// per-migration override; raising it is a reviewed edit when a real table crosses it. Census does
// not count the rows of a table above it: the refusal is already decided, and an exact count of
// that much data during a deploy is load for nothing.
const CensusTableLimit int64 = 1 << 30

// CensusLongTransaction is how long a transaction holding a lock on a table a pending migration
// touches may have been open before Census refuses: the migration would give up on that lock
// after LockTimeout anyway, during the rollout, and refusing now saves it. A minute rather than
// LockTimeout, because the census runs minutes before the migration and because the nightly
// dump holds ACCESS SHARE for about that long. Transactions that old anywhere in the database
// are reported too, without refusing.
const CensusLongTransaction = time.Minute

// CensusStatementTimeout bounds each statement the census runs, the migrations' own censuses
// included. A read of a touched table that outlasts it refuses that table, like one that waits
// out LockTimeout.
const CensusStatementTimeout = time.Minute

// The SQLSTATEs the census tells apart, beside lock.go's lockNotAvailable.
const (
	undefinedTable  = "42P01"
	undefinedColumn = "42703"
	queryCanceled   = "57014"
)

// CensusOptions are the census's bounds. A zero field takes its package constant
// (CensusTableLimit, CensusLongTransaction, CensusStatementTimeout); tests set smaller ones.
type CensusOptions struct {
	TableLimit       int64
	LongTransaction  time.Duration
	StatementTimeout time.Duration
}

// TableCensus is one table a pending migration touches.
type TableCensus struct {
	Name string
	// Through is the table a foreign key reaches this one from (CensusTable.Through); empty for a
	// table the migration names. Such a table is read for its holders and its size, and its rows
	// are not counted: the migration locks it only to check or act on the rows its foreign key
	// connects.
	Through string
	Exists  bool
	// Bytes is its pg_total_relation_size; Rows is its exact row count, read only when Bytes is
	// within the limit.
	Bytes int64
	Rows  int64
	// Unread says why the census gave up reading the table (a lock every read waits behind, or a
	// read that outlasted the statement timeout); empty when it read it.
	Unread string
	// Holders are the other sessions holding a granted relation lock on the table.
	Holders []Session
}

// MigrationCensus is the census of one pending migration.
type MigrationCensus struct {
	Migration Migration
	Tables    []TableCensus
	// Count is what the migration's census answered; nil when it declares none or the census
	// failed.
	Count *int64
	// Refusals are why the migration may not be applied yet, each a reason the report prints
	// under the migration's name.
	Refusals []string
}

func (m *MigrationCensus) refuse(format string, args ...any) {
	m.Refusals = append(m.Refusals, fmt.Sprintf(format, args...))
}

// Report is what Census found, with the bounds it applied.
type Report struct {
	VersionTable string
	// Fresh is a database that records no version and holds no table: nothing a migration could
	// refuse or wait on, so Census checks nothing else.
	Fresh bool
	// SchemaVersion is the highest version VersionTable records, 0 when it records none.
	SchemaVersion    int
	TableLimit       int64
	LongTransaction  time.Duration
	Pending          []MigrationCensus
	LongTransactions []Session
}

// refusals counts every reason in the report.
func (r *Report) refusals() int {
	n := 0
	for _, entry := range r.Pending {
		n += len(entry.Refusals)
	}
	return n
}

// Refused reports whether anything refuses the pending migrations.
func (r *Report) Refused() bool { return r.refusals() > 0 }

// Census reads, in one repeatable-read read-only transaction on conn, what a deployment must know
// before the migrations the database has not applied run: for each pending migration the tables
// it locks above ACCESS SHARE (CensusTables), with their total size, row count and the sessions
// holding locks on them, and the count its own census answers; and every transaction open longer
// than LongTransaction. A pending migration is one whose version versionTable does not record,
// the rule the runners apply it by; a database without that table records none. A fresh database,
// one that records no version and holds no table, has no row to refuse and no holder to wait on,
// so Census checks nothing there (Report.Fresh).
//
// It refuses (MigrationCensus.Refusals, not an error):
//   - a table the migration names above TableLimit, or a table it touches that the census cannot
//     read within LockTimeout or the statement timeout;
//   - a lock holder on a touched table that would hold the migration past LockTimeout
//     (holderRefusal): a transaction at least LongTransaction old, one whose age Postgres hides
//     from the census's role, or an autovacuum Postgres will not cancel;
//   - a census that answers non-zero, or that fails or answers anything but one integer - one that
//     names a table or column the database does not have included, even behind an earlier pending
//     migration that may create it, since the census cannot tell that from a typo without applying
//     that migration;
//   - a database that records no version but holds tables, which the runner would apply every
//     migration over, on the first pending migration.
//
// It returns an error only when the census itself could not be taken. It writes nothing: the
// transaction is read-only, and a migration's census is sent through the extended protocol, so it
// cannot end that transaction with a statement of its own (readCensus). What the report holds is
// counts, sizes, versions, file names, session metadata and SQLSTATEs; Postgres's own message only
// where it points into the census's own text (censusCount).
func Census(ctx context.Context, conn *pgx.Conn, migrations []Migration, versionTable string, options CensusOptions) (*Report, error) {
	if options.TableLimit == 0 {
		options.TableLimit = CensusTableLimit
	}
	if options.LongTransaction == 0 {
		options.LongTransaction = CensusLongTransaction
	}
	if options.StatementTimeout == 0 {
		options.StatementTimeout = CensusStatementTimeout
	}
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("census: begin read-only transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	// standard_conforming_strings on, whatever the server or the connection sets, makes Postgres
	// find the census's string literals where Load's check found them (censusTokens): off, a
	// backslash in '…' escapes the quote after it, and a literal can run on over a call.
	if _, err := tx.Exec(ctx, "select set_config('statement_timeout', $1, true), set_config('lock_timeout', $2, true), set_config('standard_conforming_strings', 'on', true)",
		strconv.FormatInt(options.StatementTimeout.Milliseconds(), 10)+"ms", lockTimeoutSetting); err != nil {
		return nil, fmt.Errorf("census: set timeouts and string syntax: %w", err)
	}
	report := &Report{VersionTable: versionTable, TableLimit: options.TableLimit, LongTransaction: options.LongTransaction}
	recorded, err := recordedVersions(ctx, tx, versionTable)
	if err != nil {
		return nil, err
	}
	for version := range recorded {
		report.SchemaVersion = max(report.SchemaVersion, version)
	}
	var unrecorded []string
	if len(recorded) == 0 {
		if unrecorded, err = tablesHeld(ctx, tx, versionTable); err != nil {
			return nil, err
		}
		if len(unrecorded) == 0 {
			report.Fresh = true
			return report, nil
		}
	}
	// Postgres cancels an autovacuum that holds a lock another session waits for, once that
	// session has waited deadlock_timeout; the migration waits LockTimeout at most.
	var cancelsAutovacuum bool
	if err := tx.QueryRow(ctx, "select current_setting('deadlock_timeout')::interval < $1::interval", lockTimeoutSetting).Scan(&cancelsAutovacuum); err != nil {
		return nil, fmt.Errorf("census: read deadlock_timeout: %w", err)
	}
	for _, migration := range migrations {
		if recorded[migration.Version] {
			continue
		}
		entry := MigrationCensus{Migration: migration}
		if len(report.Pending) == 0 && len(unrecorded) > 0 {
			entry.refuse("%s records no version, yet the database holds %s: the runner would apply every migration from this one over it", versionTable, someTables(unrecorded))
		}
		tables, err := CensusTables(ctx, tx, migration.SQL)
		if err != nil {
			return nil, fmt.Errorf("census: %s: %w", migration.Name, err)
		}
		for _, table := range tables {
			if err := censusTable(ctx, tx, table, options, cancelsAutovacuum, &entry); err != nil {
				return nil, err
			}
		}
		if migration.Census != "" {
			if err := censusCount(ctx, tx, &entry); err != nil {
				return nil, err
			}
		}
		report.Pending = append(report.Pending, entry)
	}
	report.LongTransactions, err = sessions(ctx, tx, `
		select pid, coalesce(usename, ''), coalesce(application_name, ''), coalesce(state, ''),
			extract(epoch from now() - xact_start)::bigint, coalesce(backend_type = 'autovacuum worker', false)
		from pg_stat_activity
		where datname = current_database() and pid <> pg_backend_pid()
			and xact_start is not null and now() - xact_start >= $1::interval
		order by xact_start`, strconv.FormatInt(options.LongTransaction.Milliseconds(), 10)+" milliseconds")
	if err != nil {
		return nil, fmt.Errorf("census: read long transactions: %w", err)
	}
	return report, nil
}

// recordedVersions reads every version versionTable records; none when the table does not exist.
func recordedVersions(ctx context.Context, tx pgx.Tx, versionTable string) (map[int]bool, error) {
	recorded := map[int]bool{}
	var exists bool
	if err := tx.QueryRow(ctx, "select to_regclass($1) is not null", versionTable).Scan(&exists); err != nil {
		return nil, fmt.Errorf("census: look for %s: %w", versionTable, err)
	}
	if !exists {
		return recorded, nil
	}
	rows, err := tx.Query(ctx, "select version from "+pgx.Identifier{versionTable}.Sanitize())
	if err != nil {
		return nil, fmt.Errorf("census: read %s: %w", versionTable, err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return nil, fmt.Errorf("census: read %s: %w", versionTable, err)
	}
	for _, version := range versions {
		recorded[version] = true
	}
	return recorded, nil
}

// tablesHeld names, sorted, the tables of the schema the runner creates its tables in
// (current_schema()), versionTable aside.
func tablesHeld(ctx context.Context, tx pgx.Tx, versionTable string) ([]string, error) {
	rows, err := tx.Query(ctx, "select relname::text from pg_class where relnamespace = current_schema()::regnamespace and relkind in ('r', 'p') and relname <> $1 order by 1", versionTable)
	if err != nil {
		return nil, fmt.Errorf("census: list the database's tables: %w", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("census: list the database's tables: %w", err)
	}
	return names, nil
}

// someTables names tables for a sentence: one by name, a few in a list, many by count and the first.
func someTables(names []string) string {
	switch {
	case len(names) == 1:
		return "table " + names[0]
	case len(names) <= 3:
		return fmt.Sprintf("%d tables (%s)", len(names), strings.Join(names, ", "))
	}
	return fmt.Sprintf("%d tables (%s, …)", len(names), strings.Join(names[:3], ", "))
}

// censusTable reads one touched table onto entry, with what it refuses. Its holders come from
// pg_locks, which takes no lock. Its size and its rows each take ACCESS SHARE on it, so both wait
// behind a session holding ACCESS EXCLUSIVE (another migration, an ALTER) and run in a savepoint:
// a wait that outlasts LockTimeout, or a read that outlasts the statement timeout, leaves the
// transaction usable and is a refusal naming the holders, not an error. A table the migration
// names above the limit is refused on its size, and its rows are not counted; a table a foreign
// key reaches has no limit, since the migration checks or acts on only the rows that key connects,
// and its rows are never counted.
func censusTable(ctx context.Context, tx pgx.Tx, touched CensusTable, options CensusOptions, cancelsAutovacuum bool, entry *MigrationCensus) error {
	name := touched.Name
	table := TableCensus{Name: name, Through: touched.Through}
	defer func() { entry.Tables = append(entry.Tables, table) }()
	var oid uint32
	var pastFreezeAge bool
	err := tx.QueryRow(ctx, tableQuery, name).Scan(&oid, &pastFreezeAge)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("census: look for %s: %w", name, err)
	}
	table.Exists = true
	holders, err := lockHolders(ctx, tx, oid)
	if err != nil {
		return fmt.Errorf("census: read lock holders of %s: %w", name, err)
	}
	for _, holder := range holders {
		table.Holders = append(table.Holders, holder.Session)
		if reason := holderRefusal(holder, name, pastFreezeAge, cancelsAutovacuum, options.LongTransaction); reason != "" {
			entry.Refusals = append(entry.Refusals, reason)
		}
	}
	nested, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("census: savepoint: %w", err)
	}
	defer nested.Rollback(ctx)
	err = nested.QueryRow(ctx, "select pg_total_relation_size($1::oid)", oid).Scan(&table.Bytes)
	if err == nil && touched.Through == "" {
		if table.Bytes > options.TableLimit {
			size, limit := sizesAbove(table.Bytes, options.TableLimit)
			entry.refuse("%s is %s, above the %s a table a migration touches may be", name, size, limit)
			return nil
		}
		err = nested.QueryRow(ctx, "select count(*) from "+pgx.Identifier{name}.Sanitize()).Scan(&table.Rows)
	}
	// The SQLSTATE and the holders the census already read, never Postgres's message: no text a
	// row could reach is printed.
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable:
		table.Unread = fmt.Sprintf("its lock was not granted within %s", LockTimeout)
		entry.refuse("could not read %s within %s (SQLSTATE %s); %s", name, LockTimeout, pgErr.Code, lockedBy(table.Holders))
		return nil
	case errors.As(err, &pgErr) && pgErr.Code == queryCanceled:
		table.Unread = fmt.Sprintf("its read outlasted the %s statement timeout", options.StatementTimeout)
		entry.refuse("could not read %s within the %s statement timeout (SQLSTATE %s); %s", name, options.StatementTimeout, pgErr.Code, lockedBy(table.Holders))
		return nil
	}
	return fmt.Errorf("census: read %s: %w", name, err)
}

// tableQuery finds a touched table by name: its oid, and whether it is past its freeze age, so
// that autovacuum launches its vacuums of it to prevent wraparound and Postgres does not cancel
// one for a lock another session waits for. The table's own reloptions can lower either age, never
// raise it.
const tableQuery = `
	select c.oid,
		age(c.relfrozenxid) >= least(current_setting('autovacuum_freeze_max_age')::bigint, coalesce(
			(select option_value::bigint from pg_options_to_table(c.reloptions) where option_name = 'autovacuum_freeze_max_age'), 2147483647))
		or mxid_age(c.relminmxid) >= least(current_setting('autovacuum_multixact_freeze_max_age')::bigint, coalesce(
			(select option_value::bigint from pg_options_to_table(c.reloptions) where option_name = 'autovacuum_multixact_freeze_max_age'), 2147483647))
	from pg_class c where c.oid = to_regclass($1)`

// lockHolder is a session holding a lock on a touched table. antiWraparound says whether an
// autovacuum holder runs to prevent wraparound, which autovacuum writes into the activity text of
// a vacuum it launches that way; nil where Postgres hides that text from the census's role, which
// then judges by the table's freeze age, refusing a vacuum launched just before the table passed
// it too. The text is read in Postgres and never leaves it.
type lockHolder struct {
	Session
	antiWraparound *bool
}

// lockHolders reads the other sessions holding a granted lock on the table oid, oldest
// transaction first. One is an autovacuum worker when pg_stat_activity says so, or, to a role
// without pg_read_all_stats, which sees no other role's backend_type, when it runs as no role (a
// client session of another role shows its role); either way only while every lock it holds on
// the table is the SHARE UPDATE EXCLUSIVE a vacuum or an analyze takes.
func lockHolders(ctx context.Context, tx pgx.Tx, oid uint32) ([]lockHolder, error) {
	rows, err := tx.Query(ctx, `
		select a.pid, coalesce(a.usename, ''), coalesce(a.application_name, ''), coalesce(a.state, ''),
			extract(epoch from now() - a.xact_start)::bigint,
			coalesce(a.backend_type = 'autovacuum worker', a.usesysid is null) and bool_and(l.mode = 'ShareUpdateExclusiveLock'),
			case when a.backend_type is not null then a.query like 'autovacuum: % (to prevent wraparound)' end
		from pg_locks l join pg_stat_activity a on a.pid = l.pid
		where l.granted and l.locktype = 'relation' and l.relation = $1
			and l.database = (select oid from pg_database where datname = current_database())
			and l.pid <> pg_backend_pid()
		group by a.pid, a.usename, a.application_name, a.state, a.xact_start, a.backend_type, a.usesysid, a.query
		order by 5 desc nulls last, 1`, oid)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (lockHolder, error) {
		var h lockHolder
		err := row.Scan(&h.PID, &h.User, &h.Application, &h.State, &h.XactSeconds, &h.Autovacuum, &h.antiWraparound)
		return h, err
	})
}

// holderRefusal says why a session holding a lock on table refuses the migration, or "" when it
// does not: whether it would hold the migration's lock past LockTimeout. An autovacuum worker
// would not, however long it has run: Postgres cancels it once the migration has waited
// deadlock_timeout for a lock it holds (cancelsAutovacuum says that is inside LockTimeout), except
// an anti-wraparound one, which its activity says it is, or, where Postgres hides that, which it
// may be while the table is past its freeze age (pastFreezeAge). Any other session might, when its
// transaction is long open or the census cannot see how long.
func holderRefusal(holder lockHolder, table string, pastFreezeAge, cancelsAutovacuum bool, long time.Duration) string {
	if holder.Autovacuum {
		switch {
		case holder.antiWraparound != nil && *holder.antiWraparound:
			return fmt.Sprintf("%s holds a lock on %s, and it is an anti-wraparound autovacuum, which Postgres does not cancel for the migration's lock; let it finish", holder.Session, table)
		case holder.antiWraparound == nil && pastFreezeAge:
			return fmt.Sprintf("%s holds a lock on %s, which is past its freeze age, so it may be an anti-wraparound autovacuum, which Postgres does not cancel for the migration's lock; granting the census's role pg_read_all_stats lets the census read which", holder.Session, table)
		case !cancelsAutovacuum:
			return fmt.Sprintf("%s holds a lock on %s, and the server's deadlock_timeout is not shorter than the migration's %s lock timeout, so the migration would give up before Postgres cancels the autovacuum", holder.Session, table, LockTimeout)
		}
		return ""
	}
	switch {
	case holder.XactSeconds == nil:
		return fmt.Sprintf("%s holds a lock on %s, and Postgres hides its transaction's age from the census's role, so the census cannot rule out a transaction older than %s; granting that role pg_read_all_stats lets the census read the age", holder.Session, table, long)
	case time.Duration(*holder.XactSeconds)*time.Second >= long:
		return fmt.Sprintf("%s has held a lock on %s for %s, longer than %s", holder.Session, table, seconds(*holder.XactSeconds), long)
	}
	return ""
}

// lockedBy names the sessions holding a lock on a table the census could not read, for a refusal.
func lockedBy(holders []Session) string {
	if len(holders) == 0 {
		return "no other session holds a lock on it"
	}
	return "locks on it are held by " + sessionList(holders)
}

// censusCount runs the migration's census inside a savepoint and records what it answered.
//
// What a failure prints is bounded, because the report goes to logs with a wider audience than
// the database's: the census file's name and the SQLSTATE always, and Postgres's message only
// when the error points into the census's own text (a position, which a parse or analysis error
// carries), since then it quotes that text and its identifiers. An error raised while the census
// runs carries no position, and its message can quote a row's value: a data exception's quotes
// the value that failed to cast, and a reg* input function's (regclass, regtype, regproc) quotes
// the text it was given under a class-42 SQLSTATE. A name the census's own text gives that the
// database does not have (42P01, 42703, with a position) refuses like any other failure, with what
// to do if an earlier pending migration creates it: that release cannot pass, and one carrying the
// earlier migration alone deploys first. A missing name with no position is a row's value (a
// regclass cast), which no migration creates, and gets no such advice. An error that is not
// Postgres's (the connection dropped) is returned: the census could not be taken.
func censusCount(ctx context.Context, tx pgx.Tx, entry *MigrationCensus) error {
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
			entry.refuse("its census %s (%s); a census answers one integer", shape, migration.CensusName)
		case count == nil:
			entry.refuse("its census answered no row (%s); a census answers one integer", migration.CensusName)
		default:
			entry.Count = count
			if *count != 0 {
				entry.refuse("its census counts %d (%s); the migration would refuse or rewrite what those rows hold", *count, migration.CensusName)
			}
		}
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return fmt.Errorf("census: run %s: %w", migration.CensusName, err)
	}
	failure := fmt.Sprintf("%s, SQLSTATE %s", migration.CensusName, pgErr.Code)
	unprinted := ""
	if pgErr.Position > 0 {
		failure += ": " + pgErr.Message
	} else {
		unprinted = "; Postgres's message is not printed, since it does not point into the census's text and can quote a row's value"
	}
	if (pgErr.Code == undefinedTable || pgErr.Code == undefinedColumn) && pgErr.Position > 0 {
		entry.refuse("its census names something the database does not have (%s); if an earlier pending migration creates it, this release cannot pass: a release carrying that migration without this one deploys first", failure)
		return nil
	}
	entry.refuse("its census failed (%s)%s", failure, unprinted)
	return nil
}

// readCensus runs one census and reads what it answered: the count, or the shape that makes it
// no count, or the error the statement met. The statement runs with QueryExecModeDescribeExec
// whatever the connection's default is: through the extended protocol Postgres refuses a text
// holding more than one statement (42601), so a census cannot end the read-only transaction with a
// `commit` of its own, which the simple protocol would let it do. The rows are read before
// anything is judged about their shape: in this exec mode pgx sends no Describe for the portal and
// fills the field descriptions from the first DataRow, so a statement that fails at execution with
// no row (a data exception, a write the read-only transaction refuses) shows zero columns and its
// error only once the rows are closed. The column count is read from each row's values instead.
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

// sessions reads the sessions query names: pid, role, application, state, transaction age in
// seconds, and whether it is an autovacuum worker.
func sessions(ctx context.Context, tx pgx.Tx, query string, arg any) ([]Session, error) {
	rows, err := tx.Query(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.PID, &s.User, &s.Application, &s.State, &s.XactSeconds, &s.Autovacuum); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func seconds(n int64) string { return (time.Duration(n) * time.Second).String() }

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}

// sizesAbove writes a size above limit and the limit so that the two never read as one figure: as
// humanBytes writes them, or in bytes where it rounds them to the same one.
func sizesAbove(n, limit int64) (string, string) {
	if size, bound := humanBytes(n), humanBytes(limit); size != bound {
		return size, bound
	}
	return thousands(n) + " bytes", thousands(limit) + " bytes"
}

// thousands writes n with , separators: the counts a human compares against a row estimate.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// Write prints the report, one census: line per fact, every refusal under its migration, ending
// with census: ok or census: REFUSED (<n> reason(s)); a fresh database's is one line and the
// verdict. Counts, sizes, versions, file names, session metadata and SQLSTATEs only.
func (r *Report) Write(w io.Writer) {
	if r.Fresh {
		fmt.Fprintln(w, "census: fresh database, nothing to check")
		fmt.Fprintln(w, "census: ok")
		return
	}
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
			touches := table.Name
			if table.Through != "" {
				touches += " through a foreign key with " + table.Through
			}
			holders := sessionList(table.Holders)
			switch {
			case !table.Exists:
				fmt.Fprintf(w, "census: %s touches %s: not in the database yet\n", name, touches)
			case table.Unread != "":
				fmt.Fprintf(w, "census: %s touches %s: not read, %s; locks held by other sessions: %s\n", name, touches, table.Unread, holders)
			case table.Through != "":
				fmt.Fprintf(w, "census: %s touches %s: %s; locks held by other sessions: %s\n", name, touches, humanBytes(table.Bytes), holders)
			case table.Bytes > r.TableLimit:
				size, limit := sizesAbove(table.Bytes, r.TableLimit)
				fmt.Fprintf(w, "census: %s touches %s: %s, above the %s limit, so its rows were not counted; locks held by other sessions: %s\n",
					name, touches, size, limit, holders)
			default:
				rows := "rows"
				if table.Rows == 1 {
					rows = "row"
				}
				fmt.Fprintf(w, "census: %s touches %s: %s %s, %s (limit %s); locks held by other sessions: %s\n",
					name, touches, thousands(table.Rows), rows, humanBytes(table.Bytes), humanBytes(r.TableLimit), holders)
			}
		}
		switch {
		case entry.Migration.Census == "":
			fmt.Fprintf(w, "census: %s declares no census\n", name)
		case entry.Count != nil && *entry.Count == 0:
			fmt.Fprintf(w, "census: %s census (%s): 0\n", name, entry.Migration.CensusName)
		}
		for _, refusal := range entry.Refusals {
			fmt.Fprintf(w, "census: REFUSED %s: %s\n", name, refusal)
		}
	}
	fmt.Fprintf(w, "census: transactions open longer than %s: %s\n", r.LongTransaction, sessionList(r.LongTransactions))
	if n := r.refusals(); n > 0 {
		reasons := "reasons"
		if n == 1 {
			reasons = "reason"
		}
		fmt.Fprintf(w, "census: REFUSED (%d %s)\n", n, reasons)
		return
	}
	fmt.Fprintln(w, "census: ok")
}
