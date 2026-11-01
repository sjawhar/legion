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
// per-migration override; raising it is a reviewed edit when a real table crosses it. The one
// exemption is by statement kind, not by migration: a migration made only of statements that
// change the catalog and read no row (catalogOnlyStatements, as Postgres's own parser reads them)
// holds its lock for a time that does not grow with the table, so the limit does not apply to it;
// its table's size is still read and reported, and its lock holders still refuse it. Census does
// not count the rows of a table above the limit: the refusal is already decided, or the migration
// reads no row, and an exact count of that much data during a deploy is load for nothing.
const CensusTableLimit int64 = 1 << 30

// CensusLongTransaction is how long a transaction holding a lock on a table a pending migration
// touches may have been open before Census refuses: the migration would give up on that lock
// after LockTimeout anyway, during the rollout, and refusing now saves it. A minute rather than
// LockTimeout, because the census runs minutes before the migration and because the nightly
// dump holds ACCESS SHARE for about that long. Transactions that old anywhere in the database,
// and those whose age the census cannot see, are reported too, without refusing.
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
	// Through is the table a foreign key reaches this one from (TouchedTable.Through); empty for a
	// table the migration names. Such a table is read for its holders and its size, and its rows
	// are not counted: the migration locks it only to check or act on the rows its foreign key
	// connects, or, for a dropped table's key, only for the catalog change.
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
	// TableSizeExemption names the statement kinds the migration is made of when every one of
	// them does its work in the catalog (tableSizeExemption) and no enabled event trigger runs on
	// them (ddlEventTriggersQuery), so CensusTableLimit does not apply to it; empty otherwise.
	TableSizeExemption string
	// exemptionVoided says, for a refusal on size, why a migration whose statements alone would be
	// exempt is not: the event triggers that run on them. Empty otherwise.
	exemptionVoided string
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
// than LongTransaction, or of an age it cannot see. A pending migration is one whose version
// versionTable does not record, the rule the runners apply it by; a database without that table
// records none. A fresh database, one that records no version and holds no table, has no row to
// refuse and no holder to wait on, so Census checks nothing there (Report.Fresh).
//
// It refuses (MigrationCensus.Refusals, not an error):
//   - a table the migration names above TableLimit, unless every statement of the migration only
//     changes the catalog (tableSizeExemption), or a table it touches that the census cannot read
//     within LockTimeout or the statement timeout;
//   - a lock holder on a touched table that would hold the migration past LockTimeout
//     (holderRefusal): a transaction at least LongTransaction old, one whose age Postgres hides
//     from the census's role or does not record (track_activities off), or an autovacuum Postgres
//     will not cancel or may not; each such refusal carries the holder's diagnosis
//     (holderDiagnosis);
//   - a census that answers non-zero, or that fails or answers anything but one integer - one that
//     names a table or column the database does not have included, even behind an earlier pending
//     migration that may create it, since the census cannot tell that from a typo without applying
//     that migration;
//   - a database that records no version but holds tables, which the runner would apply every
//     migration over, on the first pending migration.
//
// It returns an error only when the census itself could not be taken, a connection whose
// search_path names no schema that exists among them: there current_schema() is null, so neither
// versionTable nor any table is found, and a populated database would read as fresh, while the
// runner can create nothing on that connection. It writes nothing: the transaction is read-only,
// and a migration's census is sent through the extended protocol, so it cannot end that
// transaction with a statement of its own (readCensus). What the report holds is counts, sizes,
// versions, file names, session metadata, lock modes and SQLSTATEs, a refusing holder's query text
// only with its literals, numbers and comments taken out (redactedQuery), and Postgres's own
// message only where it points into the census's own text (censusCount).
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
	var schema *string
	if err := tx.QueryRow(ctx, "select current_schema()").Scan(&schema); err != nil {
		return nil, fmt.Errorf("census: read current_schema(): %w", err)
	}
	if schema == nil {
		return nil, fmt.Errorf("census: the connection's search_path names no schema that exists, so current_schema() is null: the census would find neither %s nor any table, and the runner can create neither; name an existing schema in the search_path", versionTable)
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
	// The foreign keys a migration's rows reach through: the catalog's, then each pending
	// migration's as it is read, since at boot each applies before the next.
	keys, err := foreignKeys(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("census: %w", err)
	}
	// An event trigger on create function or create trigger runs code of its own when they run,
	// which may read any table, so where one is enabled no migration is exempt from the limit.
	var ddlEventTriggers string
	if err := tx.QueryRow(ctx, ddlEventTriggersQuery).Scan(&ddlEventTriggers); err != nil {
		return nil, fmt.Errorf("census: read event triggers: %w", err)
	}
	for _, migration := range migrations {
		if recorded[migration.Version] {
			continue
		}
		entry := MigrationCensus{Migration: migration}
		if kinds := tableSizeExemption(migration.SQL); kinds != "" && ddlEventTriggers == "" {
			entry.TableSizeExemption = kinds
		} else if kinds != "" {
			entry.exemptionVoided = fmt.Sprintf("its statements are only %s, which would exempt it from the limit, but the enabled event triggers %s run code of their own on them", kinds, ddlEventTriggers)
		}
		if len(report.Pending) == 0 && len(unrecorded) > 0 {
			entry.refuse("%s records no version, yet the database holds %s: the runner would apply every migration from this one over it", versionTable, someTables(unrecorded))
		}
		bound, err := lockBoundOf(ctx, tx, &entry)
		if err != nil {
			return nil, err
		}
		text := readMigration(migration.SQL)
		tables, err := censusTables(ctx, tx, text, matchedTouches(text.code), keys)
		if err != nil {
			return nil, fmt.Errorf("census: %s: %w", migration.Name, err)
		}
		keys = append(keys, addedForeignKeys(text.code)...)
		for _, table := range tables {
			if err := censusTable(ctx, tx, table, options, bound, &entry); err != nil {
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
	// A session whose transaction's age the census cannot see - another role's without
	// pg_read_all_stats, which Postgres shows with no state, any with track_activities off, shown
	// as disabled, or a WAL sender, whose xact_start Postgres never records - shows no xact_start,
	// as one in no transaction does; the virtual transaction id every transaction locks tells the
	// two apart. Only an idle session that shows no xact_start yet locks a virtual transaction id
	// is one whose transaction began after Postgres read pg_stat_activity, within this query
	// (holderRefusal says why idle is the one state that shows it): Postgres reads
	// pg_stat_activity once per transaction and keeps it (freshActivity), so the census clears that
	// copy first and such a transaction is younger than this query.
	if err := freshActivity(ctx, tx); err != nil {
		return nil, err
	}
	report.LongTransactions, err = sessions(ctx, tx, `
		select a.pid, coalesce(a.usename, ''), coalesce(a.application_name, ''), coalesce(a.state, ''),
			clock_timestamp() - a.xact_start, coalesce(a.backend_type = 'autovacuum worker', false)
		from pg_stat_activity a
		where a.datname = current_database() and a.pid <> pg_backend_pid()
			and (clock_timestamp() - a.xact_start >= $1::interval
				or a.xact_start is null and a.state is distinct from 'idle'
					and exists (select from pg_locks l where l.pid = a.pid and l.locktype = 'virtualxid'))
		order by a.xact_start, a.pid`, options.LongTransaction)
	if err != nil {
		return nil, fmt.Errorf("census: read long transactions: %w", err)
	}
	return report, nil
}

// lockBound is how long a pending migration waits for a lock: a lock_timeout it waits under, as
// Postgres takes it, and whether Postgres cancels an autovacuum holding that lock first. Where the
// migration waits under several, it is the first Postgres would not cancel an autovacuum within,
// or the last when it would within all of them.
type lockBound struct {
	setting           string
	cancelsAutovacuum bool
}

// lockBoundOf reads entry's migration's lockBound. Postgres cancels an autovacuum holding a lock a
// session waits for once that session has waited deadlock_timeout, so it does inside a lock_timeout
// longer than deadlock_timeout, or none (0). Each lock_timeout the migration waits under
// (migrationLockTimeouts) is set in a savepoint, so Postgres reads it as the migration's own SET
// would and the census keeps its own. A value set_config refuses - one the migration would fail
// on, or SET syntax such as DEFAULT that set_config does not take - is refused: the census cannot
// judge the migration's lock holders against it.
func lockBoundOf(ctx context.Context, tx pgx.Tx, entry *MigrationCensus) (lockBound, error) {
	var bound lockBound
	for _, setting := range migrationLockTimeouts(entry.Migration.SQL) {
		bound = lockBound{setting: setting}
		nested, err := tx.Begin(ctx)
		if err != nil {
			return lockBound{}, fmt.Errorf("census: savepoint: %w", err)
		}
		_, err = nested.Exec(ctx, "select set_config('lock_timeout', $1, true)", setting)
		if err == nil {
			err = nested.QueryRow(ctx, `
				select l.setting::bigint = 0 or d.setting::bigint < l.setting::bigint
				from pg_settings l, pg_settings d
				where l.name = 'lock_timeout' and d.name = 'deadlock_timeout'
			`).Scan(&bound.cancelsAutovacuum)
		}
		if rollback := nested.Rollback(ctx); err == nil {
			err = rollback
		}
		var pgErr *pgconn.PgError
		switch {
		case errors.As(err, &pgErr):
			entry.refuse("the census could not set lock_timeout to %q, the value it sets (SQLSTATE %s), so it cannot judge the sessions holding its tables against it", setting, pgErr.Code)
			return bound, nil
		case err != nil:
			return lockBound{}, fmt.Errorf("census: %s: read its lock timeout: %w", entry.Migration.Name, err)
		case !bound.cancelsAutovacuum:
			return bound, nil
		}
	}
	return bound, nil
}

// migrationLockTimeouts are the lock_timeout settings a migration's statements wait under: Exec's
// LockTimeout until the migration's first SET [LOCAL | SESSION] lock_timeout, then each value it
// sets, as SET would give it to Postgres (a string literal's text, any other value as written). A
// migration that sets its own before any other statement waits under that alone. Comments and
// literals are read past as Postgres reads them (scanSQL), and a DO block's body is not read.
func migrationLockTimeouts(sql string) []string {
	type token struct {
		kind sqlTokenKind
		text string
	}
	var settings []string
	var statement []token
	set, defaulted := false, false
	word := func(t token, want string) bool { return t.kind == sqlWord && strings.EqualFold(t.text, want) }
	end := func() {
		words := statement
		statement = nil
		if len(words) == 0 {
			return
		}
		if len(words) > 1 && word(words[0], "set") && (word(words[1], "local") || word(words[1], "session")) {
			words = append(words[:1:1], words[2:]...)
		}
		if len(words) >= 4 && word(words[0], "set") && word(words[1], "lock_timeout") && (words[2].text == "=" || word(words[2], "to")) {
			value := words[3:]
			if len(value) == 1 && value[0].kind == sqlString && strings.HasPrefix(value[0].text, "'") {
				settings = append(settings, strings.ReplaceAll(value[0].text[1:len(value[0].text)-1], "''", "'"))
			} else {
				var written strings.Builder
				for _, part := range value {
					written.WriteString(part.text)
				}
				settings = append(settings, written.String())
			}
			set = true
			return
		}
		if !set && !defaulted {
			settings = append(settings, lockTimeoutSetting)
			defaulted = true
		}
	}
	_ = scanSQL(sql, func(t sqlToken) error {
		text := sql[t.start:t.end]
		switch {
		case t.kind == sqlSpace || t.kind == sqlComment:
		case t.kind == sqlOther && text == ";":
			end()
		default:
			statement = append(statement, token{kind: t.kind, text: text})
		}
		return nil
	})
	end()
	if len(settings) == 0 {
		return []string{lockTimeoutSetting}
	}
	return settings
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
// names above the limit is refused on its size, unless the migration is exempt from the limit
// (entry.TableSizeExemption), and either way its rows are not counted; a table a foreign key
// reaches has no limit, since the migration checks or acts on only the rows that key connects,
// or, for a dropped table's key, changes only the catalog, and its rows are never counted. Every
// refusal a holder causes carries that holder's diagnosis (holderDiagnosis).
func censusTable(ctx context.Context, tx pgx.Tx, touched TouchedTable, options CensusOptions, bound lockBound, entry *MigrationCensus) error {
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
		if reason := holderRefusal(holder, name, pastFreezeAge, bound, options.LongTransaction); reason != "" {
			entry.Refusals = append(entry.Refusals, reason+"; "+holderDiagnosis(ctx, tx, holder.PID))
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
			if entry.TableSizeExemption == "" {
				size, limit := sizesAbove(table.Bytes, options.TableLimit)
				reason := fmt.Sprintf("%s is %s, above the %s a table a migration touches may be", name, size, limit)
				if entry.exemptionVoided != "" {
					reason += "; " + entry.exemptionVoided
				}
				entry.Refusals = append(entry.Refusals, reason)
			}
			return nil
		}
		err = nested.QueryRow(ctx, "select count(*) from "+pgx.Identifier{name}.Sanitize()).Scan(&table.Rows)
	}
	// The SQLSTATE and the holders the census already read, never Postgres's message: no text a
	// row could reach is printed. The failed read aborted the savepoint, which is rolled back
	// before the holders' diagnoses are read on the transaction.
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable:
		_ = nested.Rollback(ctx)
		table.Unread = fmt.Sprintf("its lock was not granted within %s", LockTimeout)
		entry.refuse("could not read %s within %s (SQLSTATE %s); %s%s", name, LockTimeout, pgErr.Code, lockedBy(table.Holders), holderDiagnoses(ctx, tx, table.Holders))
		return nil
	case errors.As(err, &pgErr) && pgErr.Code == queryCanceled:
		_ = nested.Rollback(ctx)
		table.Unread = fmt.Sprintf("its read outlasted the %s statement timeout", options.StatementTimeout)
		entry.refuse("could not read %s within the %s statement timeout (SQLSTATE %s); %s%s", name, options.StatementTimeout, pgErr.Code, lockedBy(table.Holders), holderDiagnoses(ctx, tx, table.Holders))
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
// autovacuum holder's activity text marks it as run to prevent wraparound, which autovacuum writes
// into the text of a vacuum it launches that way; nil where Postgres hides that text from the
// census's role. The text is read in Postgres and never leaves it.
type lockHolder struct {
	Session
	antiWraparound *bool
}

// lockHolders reads the other sessions holding a granted lock on the table oid, oldest
// transaction first, with pg_stat_activity sampled afresh (freshActivity) in the query that samples
// pg_locks, rather than taken from a copy an earlier read in the transaction left. One is an autovacuum worker when
// pg_stat_activity says so, or, to a role without pg_read_all_stats, which sees no other role's
// backend_type, when it runs as no role (a client session of another role shows its role); either
// way only while every lock it holds on the table is the SHARE UPDATE EXCLUSIVE a vacuum or an
// analyze takes. A transaction's age is measured at the read (clock_timestamp()), not at the
// census's own start, which can be minutes earlier.
func lockHolders(ctx context.Context, tx pgx.Tx, oid uint32) ([]lockHolder, error) {
	if err := freshActivity(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `
		select a.pid, coalesce(a.usename, ''), coalesce(a.application_name, ''), coalesce(a.state, ''),
			clock_timestamp() - a.xact_start,
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
		err := row.Scan(&h.PID, &h.User, &h.Application, &h.State, &h.XactAge, &h.Autovacuum, &h.antiWraparound)
		return h, err
	})
}

// freshActivity discards the copy of pg_stat_activity the census's transaction holds. Postgres
// reads every session's activity the first time a transaction asks for any of it and serves that
// copy until the transaction ends (pg_stat_clear_snapshot()), while pg_locks is read anew at
// every query. The census is one transaction over every pending migration, so without this a
// later migration's holders were joined to activity read at an earlier one: a session idle then
// and holding a lock now showed its state, "idle", and no transaction start, which read as an age
// Postgres hid. It writes nothing, so the read-only transaction allows it.
func freshActivity(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, "select pg_stat_clear_snapshot()"); err != nil {
		return fmt.Errorf("census: clear the activity snapshot: %w", err)
	}
	return nil
}

// holderRefusal says why a session holding a lock on table refuses the migration, or "" when it
// does not: whether it would hold the migration's lock past the lock_timeout it waits under. An
// autovacuum worker would not, however long it has run: Postgres cancels it once the migration has
// waited deadlock_timeout for a lock it holds (bound says whether that is inside the migration's
// lock_timeout), except an anti-wraparound one, which its activity says it is, or, where its
// activity says nothing, which it may be while the table is past its freeze age (pastFreezeAge),
// refusing a vacuum launched just before the table passed it too. Any other session might, when
// its transaction is long open or the census cannot see how long.
//
// When the census's role may see a session with track_activities off, Postgres reports its state
// as disabled, its activity text empty and its transaction start unrecorded. Without the ordinary
// pg_read_all_stats visibility, those fields are hidden instead.
func holderRefusal(holder lockHolder, table string, pastFreezeAge bool, bound lockBound, long time.Duration) string {
	untracked := holder.State == untrackedState
	if holder.Autovacuum {
		antiWraparound := holder.antiWraparound
		if untracked {
			antiWraparound = nil
		}
		switch {
		case antiWraparound != nil && *antiWraparound:
			return fmt.Sprintf("%s holds a lock on %s, and it is an anti-wraparound autovacuum, which Postgres does not cancel for the migration's lock; let it finish", holder.Session, table)
		case antiWraparound == nil && pastFreezeAge:
			unknown := "granting the census's role pg_read_all_stats lets the census read which"
			if untracked {
				unknown = "Postgres records no activity for it with track_activities off, so the census cannot read which"
			}
			return fmt.Sprintf("%s holds a lock on %s, which is past its freeze age, so it may be an anti-wraparound autovacuum, which Postgres does not cancel for the migration's lock; %s", holder.Session, table, unknown)
		case !bound.cancelsAutovacuum:
			return fmt.Sprintf("%s holds a lock on %s, and the server's deadlock_timeout is not shorter than the migration's lock_timeout of %s, so the migration would give up before Postgres cancels the autovacuum", holder.Session, table, bound.setting)
		}
		return ""
	}
	switch {
	case holder.XactAge == nil && untracked:
		return fmt.Sprintf("%s holds a lock on %s, and Postgres records no transaction start for it with track_activities off, so the census cannot rule out a transaction older than %s", holder.Session, table, long)
	case holder.XactAge == nil && holder.State == "":
		// Postgres hides a session's state and transaction start together, and only from a role
		// that is neither the session's own (or a member of it) nor a member of
		// pg_read_all_stats; so here, and only here, the session runs as another role and the
		// grant is what would let the census read the age.
		return fmt.Sprintf("%s holds a lock on %s, and Postgres hides its transaction's age from the census's role, which is not the session's role, so the census cannot rule out a transaction older than %s; granting the census's role pg_read_all_stats lets the census read the age", holder.Session, table, long)
	case holder.XactAge == nil && holder.State != idleState:
		// Idle is the one state that tells a session read between its transactions, the skew the
		// next arm passes: one read while it waited for its client, before the transaction holding
		// the lock began or after it ended, reads idle (as the census's stale copy read the holder
		// TestCensusReadsTheAgeOfAHolderOfItsOwnRoleWhoseTransactionBeganDuringTheCensus sets up),
		// and a session other than a WAL sender shows xact_start whenever it holds a relation lock
		// in a transaction (xact.c sets it before the transaction runs a statement and clears it
		// once the transaction has released its locks). A WAL sender shows none, to any role
		// (pgstatfuncs.c), and a replication=database connection runs SQL, so it can hold a lock
		// in a transaction of any age; a command holding a lock of its session's between the
		// transactions it runs (CREATE INDEX CONCURRENTLY) shows none either. A session read at a
		// statement's start or end, active outside its transaction (postgres.c reports it running
		// before it starts the transaction, and until it reports it idle after the commit),
		// refuses here too: spuriously, and only at that instant, so a census taken again passes it.
		return fmt.Sprintf("%s holds a lock on %s, and Postgres records no transaction start for it though its state is %s (it records none for a WAL sender, to any role), so the census cannot rule out a transaction older than %s", holder.Session, table, holder.State, long)
	case holder.XactAge == nil:
		// Idle with no transaction start: pg_stat_activity, read afresh in the query that read
		// pg_locks (lockHolders), saw it waiting for its client between transactions, so the
		// transaction holding the lock began or ended within that one query, and is not old.
		return ""
	case *holder.XactAge >= long:
		return fmt.Sprintf("%s has held a lock on %s for %s, longer than %s", holder.Session, table, *holder.XactAge, long)
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

// diagnosisQuery reads, for one session, what an operator chasing it needs: its activity, from
// pg_stat_activity, beside every lock it holds or waits for, from pg_locks. It is printed in
// cmd/dispatch/README.md ("Pre-deploy census") for running by hand against a live database.
const diagnosisQuery = `
	select a.state, a.xact_start, a.backend_xid::text, a.wait_event_type, a.wait_event, a.query,
		l.locktype, l.mode, l.granted, l.relation::regclass::text
	from pg_stat_activity a left join pg_locks l on l.pid = a.pid
	where a.pid = $1
	order by l.granted desc, l.locktype, l.relation::regclass::text, l.mode`

// holderDiagnosis reads, read-only and in a savepoint of its own, the diagnosis of the session pid
// whose lock refuses a migration (diagnosisQuery), for the refusal to carry: by the time anyone
// looks the session has usually ended. Its query text is printed only as redactedQuery leaves it,
// with its literals, numbers and comments taken out, so the values a query carries in those do not
// reach the report. A field Postgres hides from the census's role prints as not visible. A
// diagnosis that cannot be read says so and why (a SQLSTATE, never Postgres's message), and the
// refusal it would have joined stands as it is.
func holderDiagnosis(ctx context.Context, tx pgx.Tx, pid uint32) string {
	label := fmt.Sprintf("diagnosis of pid %d", pid)
	nested, err := tx.Begin(ctx)
	if err != nil {
		return label + " not collected: " + diagnosisFailure(err)
	}
	defer nested.Rollback(ctx)
	if _, err := nested.Exec(ctx, "select pg_stat_clear_snapshot()"); err != nil {
		return label + " not collected: " + diagnosisFailure(err)
	}
	rows, err := nested.Query(ctx, diagnosisQuery, pid)
	if err != nil {
		return label + " not collected: " + diagnosisFailure(err)
	}
	defer rows.Close()
	var activity string
	var locks []string
	read := false
	for rows.Next() {
		var state, xid, waitType, waitEvent, query, lockType, mode, relation *string
		var xactStart *time.Time
		var granted *bool
		if err := rows.Scan(&state, &xactStart, &xid, &waitType, &waitEvent, &query, &lockType, &mode, &granted, &relation); err != nil {
			return label + " not collected: " + diagnosisFailure(err)
		}
		if !read {
			read = true
			// Postgres hides another role's state, transaction start and wait together from a role
			// that may not see them, and shows them null; a visible session shows them null when it
			// has none, and a WAL sender's transaction start always (pgstatfuncs.c), which prints as
			// none too. backend_xid every role sees, null only when there is none.
			absent := "none"
			if state == nil {
				absent = "not visible"
			}
			start := absent
			if xactStart != nil {
				start = xactStart.UTC().Format(time.RFC3339Nano)
			}
			wait := absent
			if waitType != nil || waitEvent != nil {
				wait = orNotVisible(waitType) + "/" + orNotVisible(waitEvent)
			}
			text := "not visible"
			if query != nil {
				text = strconv.Quote(redactedQuery(*query))
			}
			backendXID := "none"
			if xid != nil {
				backendXID = *xid
			}
			activity = fmt.Sprintf("state %s, xact_start %s, backend_xid %s, wait_event %s, query %s", orNotVisible(state), start, backendXID, wait, text)
		}
		if lockType != nil {
			lock := *lockType
			if relation != nil {
				lock += " " + *relation
			}
			lock += " " + orNotVisible(mode)
			if granted != nil && *granted {
				lock += " granted"
			} else {
				lock += " waiting"
			}
			locks = append(locks, lock)
		}
	}
	if err := rows.Err(); err != nil {
		return label + " not collected: " + diagnosisFailure(err)
	}
	if !read {
		return label + ": the session had ended when the census read it"
	}
	held := "none"
	if len(locks) > 0 {
		held = strings.Join(locks, ", ")
	}
	return label + ", read when refused: " + activity + "; its locks: " + held
}

// holderDiagnoses joins the diagnoses of holders for a refusal that names them all, each after
// "; ", or "" when there are none.
func holderDiagnoses(ctx context.Context, tx pgx.Tx, holders []Session) string {
	var out strings.Builder
	for _, holder := range holders {
		out.WriteString("; " + holderDiagnosis(ctx, tx, holder.PID))
	}
	return out.String()
}

// diagnosisFailure is why a diagnosis was not read: Postgres's SQLSTATE, or the error that is not
// Postgres's.
func diagnosisFailure(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "SQLSTATE " + pgErr.Code
	}
	return err.Error()
}

func orNotVisible(s *string) string {
	if s == nil {
		return "not visible"
	}
	return *s
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

// sessions reads the sessions query names: pid, role, application, state, how long its transaction
// has been open, and whether it is an autovacuum worker.
func sessions(ctx context.Context, tx pgx.Tx, query string, arg any) ([]Session, error) {
	rows, err := tx.Query(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.PID, &s.User, &s.Application, &s.State, &s.XactAge, &s.Autovacuum); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

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
// verdict. Counts, sizes, versions, file names, session metadata, lock modes and SQLSTATEs, and a
// refusing holder's query text only as redactedQuery leaves it.
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
			case table.Bytes > r.TableLimit && entry.TableSizeExemption != "":
				size, limit := sizesAbove(table.Bytes, r.TableLimit)
				fmt.Fprintf(w, "census: %s touches %s: %s, above the %s limit, from which the migration is exempt because its statements are only %s, which change the catalog and read no row, so its rows were not counted; locks held by other sessions: %s\n",
					name, touches, size, limit, entry.TableSizeExemption, holders)
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
	fmt.Fprintf(w, "census: transactions open longer than %s, or of an age the census cannot see: %s\n", r.LongTransaction, sessionList(r.LongTransactions))
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
