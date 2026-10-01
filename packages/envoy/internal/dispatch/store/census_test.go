package store

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

var censusBase = fstest.MapFS{
	"migrations/0001_things.up.sql": {Data: []byte("create table things (id integer, kind text)")},
}

// withCheck is censusBase plus a pending 0002 that adds a check over things, declaring census as
// its census when census is not empty.
func withCheck(census string) fstest.MapFS {
	set := fstest.MapFS{}
	for name, file := range censusBase {
		set[name] = file
	}
	set["migrations/0002_things_kind_check.up.sql"] = &fstest.MapFile{Data: []byte("alter table things add constraint things_kind_check check (kind <> 'bad')")}
	if census != "" {
		set["migrations/0002_things_kind_check.census.sql"] = &fstest.MapFile{Data: []byte(census)}
	}
	return set
}

// migratedToOne is a fresh database at version 1 of censusBase, and its URL.
func migratedToOne(t *testing.T) (*Store, string) {
	t.Helper()
	store := openEmptyTestStore(t)
	if err := store.migrate(context.Background(), censusBase); err != nil {
		t.Fatalf("migrate to 0001: %v", err)
	}
	return store, store.Pool.Config().ConnString()
}

func schemaVersion(t *testing.T, store *Store) int {
	t.Helper()
	var version int
	if err := store.Pool.QueryRow(context.Background(), "select coalesce(max(version), 0) from schema_migrations").Scan(&version); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return version
}

func countThings(t *testing.T, store *Store) int {
	t.Helper()
	var rows int
	if err := store.Pool.QueryRow(context.Background(), "select count(*) from things").Scan(&rows); err != nil {
		t.Fatalf("count things: %v", err)
	}
	return rows
}

func reportText(report *pgmigrate.Report) string {
	var out strings.Builder
	report.Write(&out)
	return out.String()
}

// A pending migration whose census counts rows is refused, naming the migration, the count and the
// census file, and the database is left as it was: the version table does not move and the rows
// are still there.
func TestCensusRefusesAPendingMigrationWhoseCensusCountsRowsAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	store, url := migratedToOne(t)
	if _, err := store.Pool.Exec(ctx, "insert into things (id, kind) values (1, 'bad'), (2, 'bad'), (3, 'bad'), (4, 'good')"); err != nil {
		t.Fatal(err)
	}
	report, err := census(ctx, url, withCheck("select count(*) from things where kind = 'bad'"), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if !report.Refused() || len(report.Refusals) != 1 {
		t.Fatalf("refusals = %v, want one", report.Refusals)
	}
	for _, want := range []string{"0002_things_kind_check.up.sql", "counts 3", "0002_things_kind_check.census.sql"} {
		if !strings.Contains(report.Refusals[0], want) {
			t.Errorf("refusal %q lacks %q", report.Refusals[0], want)
		}
	}
	out := reportText(report)
	if !strings.Contains(out, "census: REFUSED 0002_things_kind_check.up.sql: its census counts 3") || !strings.HasSuffix(strings.TrimSpace(out), "census: REFUSED (1 reason)") {
		t.Errorf("report:\n%s", out)
	}
	if got := schemaVersion(t, store); got != 1 {
		t.Errorf("schema_migrations moved to %d", got)
	}
	if got := countThings(t, store); got != 4 {
		t.Errorf("things has %d rows, want the 4 that were there", got)
	}
}

// Zero violators pass, and the report names the table the migration touches with its count and size.
func TestCensusPassesAtZeroAndNamesTheTouchedTable(t *testing.T) {
	ctx := context.Background()
	store, url := migratedToOne(t)
	if _, err := store.Pool.Exec(ctx, "insert into things (id, kind) values (1, 'good'), (2, 'good')"); err != nil {
		t.Fatal(err)
	}
	report, err := census(ctx, url, withCheck("select count(*) from things where kind = 'bad'"), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Fatalf("refused: %v", report.Refusals)
	}
	out := reportText(report)
	for _, want := range []string{
		"census: schema version 1 (schema_migrations); pending: 0002_things_kind_check.up.sql",
		"census: 0002_things_kind_check.up.sql touches things: 2 rows, ",
		"census: 0002_things_kind_check.up.sql census (0002_things_kind_check.census.sql): 0",
		"census: ok",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

// The census runs read-only, and its statement goes through the extended protocol whatever the
// connection string asks for: a write inside a CTE is refused by Postgres (25006, a class the
// report names by SQLSTATE alone), and a census of several statements ending the read-only
// transaction with its own `commit` is refused as more than one statement (42601) even over a
// URL that asks for the simple protocol, under which pgx would otherwise send it as one text and
// Postgres would run every statement. Either way the table is unchanged.
func TestCensusRefusesACensusThatWrites(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct{ census, urlSuffix, wantCode string }{
		"a write in a CTE": {
			census:   "with w as (insert into things (id, kind) values (9, 'x') returning id) select count(*) from w",
			wantCode: "SQLSTATE 25006",
		},
		// Without the pin, under the simple protocol pgx sends this as one Query message: `commit`
		// ends the read-only transaction and the insert lands. With the pin Postgres refuses the
		// text at Parse.
		"several statements over the simple protocol": {
			census:    "select 0; commit; insert into things (id, kind) values (9, 'x'); select 0",
			urlSuffix: "&default_query_exec_mode=simple_protocol",
			wantCode:  "SQLSTATE 42601",
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, url := migratedToOne(t)
			if _, err := store.Pool.Exec(ctx, "insert into things (id, kind) values (1, 'good')"); err != nil {
				t.Fatal(err)
			}
			report, err := census(ctx, url+tc.urlSuffix, withCheck(tc.census), pgmigrate.CensusOptions{})
			if err != nil {
				t.Fatalf("census: %v", err)
			}
			if len(report.Refusals) != 1 || !strings.Contains(report.Refusals[0], tc.wantCode) || !strings.Contains(report.Refusals[0], "0002_things_kind_check.census.sql") {
				t.Errorf("refusals = %v, want one naming %s and the census file", report.Refusals, tc.wantCode)
			}
			if got := countThings(t, store); got != 1 {
				t.Errorf("things has %d rows, want the 1 that was there: the census wrote", got)
			}
			if got := schemaVersion(t, store); got != 1 {
				t.Errorf("schema_migrations moved to %d", got)
			}
		})
	}
}

// A failed census never prints a row's value. A data exception's message quotes the value that
// failed to cast (`invalid input syntax for type integer: "<value>"`), and Dispatch's tables hold
// plain-text secrets, so the report names the census file and the SQLSTATE and nothing of the
// message.
func TestCensusNeverPrintsRowContentWhenACensusFails(t *testing.T) {
	ctx := context.Background()
	store, url := migratedToOne(t)
	const sentinel = "ghp_SENTINEL_NEVER_PRINTED_7f3a"
	if _, err := store.Pool.Exec(ctx, "insert into things (id, kind) values (1, $1)", sentinel); err != nil {
		t.Fatal(err)
	}
	report, err := census(ctx, url, withCheck("select (select kind from things limit 1)::int"), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	out := reportText(report)
	if len(report.Refusals) != 1 || !strings.Contains(report.Refusals[0], "SQLSTATE 22P02") || !strings.Contains(report.Refusals[0], "0002_things_kind_check.census.sql") {
		t.Errorf("refusals = %v, want one naming SQLSTATE 22P02 and the census file", report.Refusals)
	}
	if strings.Contains(out, sentinel) || strings.Contains(out, "SENTINEL") {
		t.Fatalf("the report printed a row's value:\n%s", out)
	}
}

// A census that names a column the database lacks refuses when its migration is the first pending
// one: at that schema the census was written against exactly this database, so the name is a typo.
func TestCensusRefusesAMissingColumnInTheFirstPendingMigrationsCensus(t *testing.T) {
	_, url := migratedToOne(t)
	report, err := census(context.Background(), url, withCheck("select count(*) from things where colour = 'bad'"), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(report.Refusals) != 1 || !strings.Contains(report.Refusals[0], "SQLSTATE 42703") || !strings.Contains(report.Refusals[0], "first pending migration") {
		t.Errorf("refusals = %v, want one naming 42703 and that it is the first pending migration", report.Refusals)
	}
	if report.Pending[0].NotAnswerable != "" {
		t.Errorf("the typo was reported as not answerable (%q) instead of refused", report.Pending[0].NotAnswerable)
	}
}

// A census that answers anything but one integer in one row is a defect and refuses.
func TestCensusRefusesAMalformedCensus(t *testing.T) {
	for name, tc := range map[string]struct{ census, want string }{
		"two columns": {"select 1, 2", "answered 2 columns"},
		// A bare select answers one row of no columns; reading its first value would panic.
		"no column": {"select", "answered 0 columns"},
		"no row":    {"select 0 where false", "answered no row"},
		"text":      {"select 'x'", "answered a string, not an integer"},
		"null":      {"select null::integer", "answered NULL, not an integer"},
		"two rows":  {"select 0 union all select 0", "more than one row"},
		// Class 42 is the one class whose message is printed: it quotes the census's own text.
		"syntax": {"select count(*) frm things", `SQLSTATE 42601: syntax error at or near "things"`},
	} {
		t.Run(name, func(t *testing.T) {
			_, url := migratedToOne(t)
			report, err := census(context.Background(), url, withCheck(tc.census), pgmigrate.CensusOptions{})
			if err != nil {
				t.Fatalf("census: %v", err)
			}
			if len(report.Refusals) != 1 || !strings.Contains(report.Refusals[0], "0002_things_kind_check.census.sql") || !strings.Contains(report.Refusals[0], tc.want) {
				t.Errorf("refusals = %v, want one naming the census file and %q", report.Refusals, tc.want)
			}
		})
	}
}

// A census naming a table or column the database does not have yet refuses nothing when an
// earlier pending migration exists, since that migration may be what creates it: here 0001, with
// no census of its own, is pending ahead of 0002, whose census names 0001's table. The same
// census as the FIRST pending migration refuses (the test above).
func TestCensusReportsACensusItCannotAnswerAtThisSchemaWithoutRefusing(t *testing.T) {
	store := openEmptyTestStore(t)
	url := store.Pool.Config().ConnString()
	set := withCheck("select count(*) from things where kind = 'bad'") // nothing applied: things does not exist yet
	report, err := census(context.Background(), url, set, pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Fatalf("refused on an empty database: %v", report.Refusals)
	}
	if report.SchemaVersion != 0 || len(report.Pending) != 2 || report.Pending[1].NotAnswerable == "" {
		t.Errorf("report = version %d, %d pending, not answerable %q", report.SchemaVersion, len(report.Pending), report.Pending[1].NotAnswerable)
	}
	out := reportText(report)
	if !strings.Contains(out, "is not answerable at schema 0, so it refuses nothing") || !strings.Contains(out, "touches things: not in the database yet") {
		t.Errorf("report:\n%s", out)
	}
}

// A touched table above the limit refuses; the limit is injected so the test needs no gigabyte.
func TestCensusRefusesATouchedTableAboveTheLimit(t *testing.T) {
	_, url := migratedToOne(t)
	report, err := census(context.Background(), url, withCheck("select 0"), pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(report.Refusals) != 1 || !strings.Contains(report.Refusals[0], "things is ") || !strings.Contains(report.Refusals[0], "above the 1 bytes") {
		t.Errorf("refusals = %v", report.Refusals)
	}
}

// holdLock opens a connection of its own to store's database and takes mode on table in a
// transaction that lasts until the test ends, returning the holder's backend pid.
func holdLock(t *testing.T, store *Store, table, mode string) uint32 {
	t.Helper()
	ctx := context.Background()
	holder, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Rollback(context.Background()) })
	if _, err := held.Exec(ctx, "lock table "+table+" in "+mode+" mode"); err != nil {
		t.Fatal(err)
	}
	return holder.PgConn().PID()
}

// A session that has held a lock on a touched table for longer than the bound refuses, naming the
// session; the same hold on a table no pending migration touches does not.
func TestCensusRefusesALockHeldOnATouchedTableByAnOldTransaction(t *testing.T) {
	ctx := context.Background()
	store, url := migratedToOne(t)
	if _, err := store.Pool.Exec(ctx, "create table untouched (id integer)"); err != nil {
		t.Fatal(err)
	}
	holdLock(t, store, "untouched", "access share")
	options := pgmigrate.CensusOptions{LongTransaction: time.Second}
	time.Sleep(1100 * time.Millisecond)
	report, err := census(ctx, url, withCheck("select 0"), options)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Fatalf("a hold on an untouched table refused: %v", report.Refusals)
	}
	if len(report.LongTransactions) != 1 {
		t.Errorf("long transactions = %v, want the holder reported", report.LongTransactions)
	}
	pid := holdLock(t, store, "things", "access share")
	time.Sleep(1100 * time.Millisecond)
	report, err = census(ctx, url, withCheck("select 0"), options)
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(report.Refusals) != 1 || !strings.Contains(report.Refusals[0], "pid "+strconv.FormatUint(uint64(pid), 10)) || !strings.Contains(report.Refusals[0], "has held a lock on things for") {
		t.Errorf("refusals = %v, want one naming pid %d", report.Refusals, pid)
	}
}

// A touched table the census cannot read within the lock timeout - another session holds ACCESS
// EXCLUSIVE on it, which every read of its size or rows waits behind - refuses naming the table,
// the SQLSTATE and the holder, and the census still finishes its report: the wait gave up inside a
// savepoint, so it left the transaction usable.
func TestCensusRefusesATouchedTableItCannotReadWithinTheLockTimeout(t *testing.T) {
	ctx := context.Background()
	store, url := migratedToOne(t)
	pid := holdLock(t, store, "things", "access exclusive")
	started := time.Now()
	report, err := census(ctx, url, withCheck("select 0"), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v (after %s)", err, time.Since(started).Round(time.Millisecond))
	}
	if len(report.Refusals) != 1 || !strings.Contains(report.Refusals[0], "could not read things within 5s (SQLSTATE 55P03)") || !strings.Contains(report.Refusals[0], "pid "+strconv.FormatUint(uint64(pid), 10)) {
		t.Errorf("refusals = %v, want one naming the table, 55P03 and pid %d", report.Refusals, pid)
	}
	out := reportText(report)
	if !strings.Contains(out, "touches things: not read, its lock was not granted within 5s") || !strings.Contains(out, "census: transactions open longer than 1m0s: none") {
		t.Errorf("report:\n%s", out)
	}
}

// On a database carrying every migration the census has nothing pending and passes.
func TestCensusOnAMigratedDatabaseHasNothingPending(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	report, err := Census(ctx, store.Pool.Config().ConnString())
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	out := reportText(report)
	if report.Refused() || len(report.Pending) != 0 || !strings.Contains(out, "no pending migration") || !strings.HasSuffix(strings.TrimSpace(out), "census: ok") {
		t.Errorf("report:\n%s", out)
	}
}
