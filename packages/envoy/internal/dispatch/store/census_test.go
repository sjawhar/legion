package store

import (
	"context"
	"io/fs"
	neturl "net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/pgmigrate/pgmigratetest"
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

// refusals is every refusal in the report as it prints them after "REFUSED ": the migration's
// name, a colon, and the reason.
func refusals(report *pgmigrate.Report) []string {
	var out []string
	for _, entry := range report.Pending {
		for _, reason := range entry.Refusals {
			out = append(out, entry.Migration.Name+": "+reason)
		}
	}
	return out
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
	if !report.Refused() || len(refusals(report)) != 1 {
		t.Fatalf("refusals = %v, want one", refusals(report))
	}
	for _, want := range []string{"0002_things_kind_check.up.sql", "counts 3", "0002_things_kind_check.census.sql"} {
		if !strings.Contains(refusals(report)[0], want) {
			t.Errorf("refusal %q lacks %q", refusals(report)[0], want)
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
		t.Fatalf("refused: %v", refusals(report))
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
			if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], tc.wantCode) || !strings.Contains(got[0], "0002_things_kind_check.census.sql") {
				t.Errorf("refusals = %v, want one naming %s and the census file", got, tc.wantCode)
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

// The census runs with standard_conforming_strings on whatever the connection sets, because
// pgmigrate.Load finds a census's string literals by that syntax: with it off a backslash escapes
// the quote after it, so a literal could run on where Load read code. Under off, '\\' is one
// backslash and this census answers -1.
func TestCensusReadsItsStringLiteralsWithStandardConformingStrings(t *testing.T) {
	_, url := migratedToOne(t)
	report, err := census(context.Background(), url+"&standard_conforming_strings=off", withCheck(`select length('\\') - 2`), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 0 {
		t.Errorf("refusals = %v, want none: the census read '\\\\' as one backslash", got)
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
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "SQLSTATE 22P02") || !strings.Contains(got[0], "0002_things_kind_check.census.sql") {
		t.Errorf("refusals = %v, want one naming SQLSTATE 22P02 and the census file", got)
	}
	if strings.Contains(out, sentinel) || strings.Contains(out, "SENTINEL") {
		t.Fatalf("the report printed a row's value:\n%s", out)
	}
}

// A census that names a column the database lacks refuses, and since that error points into the
// census's own text (its position), the report prints Postgres's message, which names the column.
func TestCensusRefusesACensusNamingAMissingColumn(t *testing.T) {
	_, url := migratedToOne(t)
	report, err := census(context.Background(), url, withCheck("select count(*) from things where colour = 'bad'"), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], `SQLSTATE 42703: column "colour" does not exist`) {
		t.Errorf("refusals = %v, want one naming 42703 and the column, whose message points into the census", got)
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
			if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "0002_things_kind_check.census.sql") || !strings.Contains(got[0], tc.want) {
				t.Errorf("refusals = %v, want one naming the census file and %q", got, tc.want)
			}
		})
	}
}

// A census naming a table or column the database does not have refuses even behind an earlier
// pending migration that may be what creates it: the census cannot tell that from a typo without
// applying that migration, which takes the very locks it measures. Here the database is at 0001,
// 0002 creates others and 0003's census names it; the refusal says such a release cannot pass.
func TestCensusRefusesACensusNamingWhatOnlyAnEarlierPendingMigrationCreates(t *testing.T) {
	_, url := migratedToOne(t)
	set := fstest.MapFS{
		"migrations/0001_things.up.sql":              censusBase["migrations/0001_things.up.sql"],
		"migrations/0002_others.up.sql":              {Data: []byte("create table others (id integer)")},
		"migrations/0003_others_id_check.up.sql":     {Data: []byte("alter table others add constraint others_id_check check (id > 0)")},
		"migrations/0003_others_id_check.census.sql": {Data: []byte("select count(*) from others where id <= 0")},
	}
	report, err := census(context.Background(), url, set, pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "0003_others_id_check.up.sql") || !strings.Contains(got[0], "SQLSTATE 42P01") || !strings.Contains(got[0], "a release carrying that migration without this one deploys first") {
		t.Errorf("refusals = %v, want one for 0003 naming 42P01 and the release that must deploy first", got)
	}
	out := reportText(report)
	if report.SchemaVersion != 1 || len(report.Pending) != 2 || !strings.Contains(out, "touches others: not in the database yet") {
		t.Errorf("report:\n%s", out)
	}
}

// A fresh database, one that records no version and holds no table, has no row a migration could
// refuse and no session holding a lock on one, so the census passes it with one line: a new
// stack's first deploy takes the census like every later one.
func TestCensusPassesAFreshDatabase(t *testing.T) {
	store := openEmptyTestStore(t)
	report, err := census(context.Background(), store.Pool.Config().ConnString(), migrationFiles, pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if out := reportText(report); report.Refused() || out != "census: fresh database, nothing to check\ncensus: ok\n" {
		t.Errorf("refusals = %v, report:\n%s", refusals(report), out)
	}
}

// A database that records no version but holds tables is not fresh: the runner would apply every
// migration from the first over what it holds. The census refuses it on the first migration,
// naming the table, even where every census answers 0.
func TestCensusRefusesADatabaseHoldingTablesButRecordingNoVersion(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if _, err := store.Pool.Exec(ctx, "create table things (id integer, kind text)"); err != nil {
		t.Fatal(err)
	}
	report, err := census(ctx, store.Pool.Config().ConnString(), withCheck("select count(*) from things where kind = 'bad'"), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 1 || !strings.HasPrefix(got[0], "0001_things.up.sql: ") || !strings.Contains(got[0], "schema_migrations records no version") || !strings.Contains(got[0], "things") {
		t.Errorf("refusals = %v, want one on 0001 naming schema_migrations and things", got)
	}
}

// The same holds for a typo behind an earlier pending migration: 0002 adds a column, and 0003's
// census misspells the one it means. A census that let it pass would let 0003 fail at boot on the
// very row it exists to count.
func TestCensusRefusesAMissingColumnBehindAnEarlierPendingMigration(t *testing.T) {
	ctx := context.Background()
	store, url := migratedToOne(t)
	if _, err := store.Pool.Exec(ctx, "insert into things (id, kind) values (1, 'bad')"); err != nil {
		t.Fatal(err)
	}
	set := fstest.MapFS{
		"migrations/0001_things.up.sql":                censusBase["migrations/0001_things.up.sql"],
		"migrations/0002_things_note.up.sql":           {Data: []byte("alter table things add column note text")},
		"migrations/0003_things_kind_check.up.sql":     {Data: []byte("alter table things add constraint things_kind_check check (kind <> 'bad')")},
		"migrations/0003_things_kind_check.census.sql": {Data: []byte("select count(*) from things where knd = 'bad'")},
	}
	report, err := census(ctx, url, set, pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "0003_things_kind_check.up.sql") || !strings.Contains(got[0], "SQLSTATE 42703") {
		t.Errorf("refusals = %v, want one for 0003 naming 42703", got)
	}
}

// Pending is the runner's own rule: every version schema_migrations does not record, not every
// version above the highest it does. A database that recorded 3 out of order (by hand, or one that
// ran a branch) still applies 0002 at boot, so the census takes 0002's census too.
func TestCensusTakesEveryVersionTheRunnerHasNotRecordedAsPending(t *testing.T) {
	ctx := context.Background()
	store, url := migratedToOne(t)
	if _, err := store.Pool.Exec(ctx, "create table others (id integer)"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, "insert into schema_migrations (version) values (3)"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, "insert into things (id, kind) values (1, 'bad')"); err != nil {
		t.Fatal(err)
	}
	set := withCheck("select count(*) from things where kind = 'bad'")
	set["migrations/0003_others.up.sql"] = &fstest.MapFile{Data: []byte("create table others (id integer)")}
	report, err := census(ctx, url, set, pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	out := reportText(report)
	if len(report.Pending) != 1 || report.Pending[0].Migration.Name != "0002_things_kind_check.up.sql" || !strings.Contains(out, "pending: 0002_things_kind_check.up.sql\n") {
		t.Errorf("pending is not exactly the unrecorded 0002:\n%s", out)
	}
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "counts 1") {
		t.Errorf("refusals = %v, want 0002's count", got)
	}
}

// A census that casts a row's text to a reg* type fails in the type's input function, which
// quotes the value it was given under a class-42 SQLSTATE. The census prints Postgres's message
// only for an error that points into the census's own text (a position), never this one. The name
// regclass misses is a row's, not one a migration creates, so the refusal gives no advice about an
// earlier pending migration.
func TestCensusNeverPrintsARowValueAnInputFunctionQuotes(t *testing.T) {
	for name, tc := range map[string]struct{ census, code string }{
		"regclass": {"select count(*) from things where kind::regclass is not null", "SQLSTATE 42P01"},
		"regtype":  {"select count(*) from things where kind::regtype is not null", "SQLSTATE 42704"},
		"regproc":  {"select count(*) from things where kind::regproc is not null", "SQLSTATE 42883"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, url := migratedToOne(t)
			if _, err := store.Pool.Exec(ctx, "insert into things (id, kind) values (1, 'ghp_SENTINEL_ROW_VALUE')"); err != nil {
				t.Fatal(err)
			}
			report, err := census(ctx, url, withCheck(tc.census), pgmigrate.CensusOptions{})
			if err != nil {
				t.Fatalf("census: %v", err)
			}
			out := reportText(report)
			if strings.Contains(strings.ToLower(out), "sentinel") {
				t.Fatalf("the report printed a row's value:\n%s", out)
			}
			if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], tc.code) || strings.Contains(got[0], "pending migration") {
				t.Errorf("refusals = %v, want one naming %s and no pending migration", got, tc.code)
			}
		})
	}
}

// A lock holder of another role whose transaction's age Postgres hides from the census's role
// (no pg_read_all_stats) refuses: the census cannot rule out the long transaction it exists to
// catch. Granting that role pg_read_all_stats lets it read the age, and a young holder passes.
func TestCensusRefusesALockHolderWhoseAgeItCannotSee(t *testing.T) {
	ctx := context.Background()
	store, base := migratedToOne(t)
	suffix := randomDatabaseSuffix(t)
	censusRole, otherRole := "census_"+suffix, "other_"+suffix
	for _, role := range []string{censusRole, otherRole} {
		if _, err := store.Pool.Exec(ctx, "create role "+role+" login password 'census-test'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// The grants in this database first, then the role itself, which spans the cluster.
			for _, statement := range []string{"drop owned by " + role, "drop role " + role} {
				if _, err := store.Pool.Exec(context.Background(), statement); err != nil {
					t.Errorf("%s: %v", statement, err)
				}
			}
		})
		if _, err := store.Pool.Exec(ctx, "grant select on all tables in schema public to "+role); err != nil {
			t.Fatal(err)
		}
	}
	holderConfig := store.Pool.Config().ConnConfig.Copy()
	holderConfig.User, holderConfig.Password = otherRole, "census-test"
	holder, err := pgx.ConnectConfig(ctx, holderConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { held.Rollback(context.Background()) })
	if _, err := held.Exec(ctx, "lock table things in access share mode"); err != nil {
		t.Fatal(err)
	}
	asCensus, err := neturl.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	asCensus.User = neturl.UserPassword(censusRole, "census-test")
	pid := "pid " + strconv.FormatUint(uint64(holder.PgConn().PID()), 10)
	report, err := census(ctx, asCensus.String(), withCheck("select 0"), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], pid) || !strings.Contains(got[0], "pg_read_all_stats") {
		t.Errorf("refusals = %v, want one naming %s and pg_read_all_stats", got, pid)
	}
	if _, err := store.Pool.Exec(ctx, "grant pg_read_all_stats to "+censusRole); err != nil {
		t.Fatal(err)
	}
	report, err = census(ctx, asCensus.String(), withCheck("select 0"), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if report.Refused() {
		t.Errorf("a young holder refused once the census could read its age: %v", refusals(report))
	}
}

// A touched table above the limit refuses on its size and is not counted; the limit is injected so
// the test needs no gigabyte.
func TestCensusRefusesATouchedTableAboveTheLimit(t *testing.T) {
	_, url := migratedToOne(t)
	report, err := census(context.Background(), url, withCheck("select 0"), pgmigrate.CensusOptions{TableLimit: 1})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "things is ") || !strings.Contains(got[0], "above the 1 bytes") {
		t.Errorf("refusals = %v", got)
	}
	if out := reportText(report); !strings.Contains(out, ", above the 1 bytes limit, so its rows were not counted;") {
		t.Errorf("report:\n%s", out)
	}
}

// A read of a touched table that outlasts the statement timeout refuses that table, and the census
// still prints its report rather than exiting without one. Here the read waits on an ACCESS
// EXCLUSIVE holder under a statement timeout shorter than the lock timeout, so Postgres cancels it
// as a statement timeout (57014), as it cancels a slow count of a large table.
func TestCensusRefusesATouchedTableWhoseReadOutlastsTheStatementTimeout(t *testing.T) {
	ctx := context.Background()
	store, url := migratedToOne(t)
	pid := "pid " + strconv.FormatUint(uint64(holdLock(t, store, "things", "access exclusive")), 10)
	report, err := census(ctx, url, withCheck("select 0"), pgmigrate.CensusOptions{StatementTimeout: time.Second})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "could not read things within the 1s statement timeout (SQLSTATE 57014)") || !strings.Contains(got[0], pid) {
		t.Errorf("refusals = %v, want one naming the table, 57014 and %s", got, pid)
	}
	out := reportText(report)
	if !strings.Contains(out, "touches things: not read, its read outlasted the 1s statement timeout;") || !strings.HasSuffix(out, "census: REFUSED (1 reason)\n") {
		t.Errorf("report:\n%s", out)
	}
}

// Every census the repository ships answers one integer at the schema just before its migration:
// a database migrated through N-1, and the set cut at N, so N is the one pending migration. The
// census refuses a census naming what its schema lacks, so a shipped one that did would refuse
// every deploy carrying it; this catches it at review instead.
func TestEveryShippedCensusAnswersAtTheSchemaBeforeItsMigration(t *testing.T) {
	ctx := context.Background()
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	store := openEmptyTestStore(t)
	url := store.Pool.Config().ConnString()
	answered := 0
	for _, migration := range migrations {
		if migration.Census == "" {
			continue
		}
		migrateThrough(t, store, migration.Version-1)
		report, err := census(ctx, url, migrationsThrough(t, migration.Version), pgmigrate.CensusOptions{})
		if err != nil {
			t.Fatalf("census of %s: %v", migration.CensusName, err)
		}
		if len(report.Pending) != 1 || report.Pending[0].Count == nil || report.Refused() {
			t.Errorf("%s did not answer one integer at schema %d:\n%s", migration.CensusName, migration.Version-1, reportText(report))
		}
		answered++
	}
	if answered == 0 {
		t.Fatal("the set declares no census")
	}
}

// A census is taken before every migration of its release applies, so one that reads what an
// earlier migration of the same release creates refuses every deploy of that release. The test
// cannot know where releases split, so every census that reads what a migration from
// censusRequiredFrom on creates names that migration: the author's word that it ships first.
func TestEveryShippedCensusNamesTheNewMigrationsItReads(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	conn, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if err := pgmigratetest.CheckCensusNamesTheMigrationsItReads(ctx, conn, migrationFiles, "migrations", censusRequiredFrom); err != nil {
		t.Fatal(err)
	}
}

// The check finds the pair: 0002 adds things.note and 0003's census counts the rows its check on
// note would refuse. A release carrying both refuses every deploy, so the check names the column
// and both files until the census names 0002, and a census that reads only what 0001 made, older
// than the rule's first number, needs nothing.
func TestTheCensusPairCheckNamesACensusReadingWhatANewMigrationCreates(t *testing.T) {
	ctx := context.Background()
	set := fstest.MapFS{
		"migrations/0001_things.up.sql":          censusBase["migrations/0001_things.up.sql"],
		"migrations/0002_things_note.up.sql":     {Data: []byte("alter table things add column note text")},
		"migrations/0002_things_note.census.sql": {Data: []byte("-- a new nullable column: no row can violate it\nselect count(*) from things where kind = 'never'")},
		"migrations/0003_note_check.up.sql":      {Data: []byte("alter table things add constraint note_check check (note <> '')")},
		"migrations/0003_note_check.census.sql":  {Data: []byte("select count(*) from things where note = ''")},
	}
	check := func() error {
		store := openEmptyTestStore(t)
		conn, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close(context.Background()) })
		return pgmigratetest.CheckCensusNamesTheMigrationsItReads(ctx, conn, set, "migrations", 2)
	}
	err := check()
	if err == nil || !strings.Contains(err.Error(), "census 0003_note_check.census.sql reads things.note, which 0002_things_note.up.sql creates") {
		t.Fatalf("CheckCensusNamesTheMigrationsItReads = %v, want it to name 0003's census, things.note and 0002", err)
	}
	set["migrations/0003_note_check.census.sql"] = &fstest.MapFile{Data: []byte("-- note is 0002_things_note's, which ships in an earlier release\nselect count(*) from things where note = ''")}
	if err := check(); err != nil {
		t.Errorf("with 0002 named: %v", err)
	}
}

// migrationsThrough is the embedded set's files numbered up to version: what a binary built when
// version was the newest migration would carry.
func migrationsThrough(t *testing.T, version int) fstest.MapFS {
	t.Helper()
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	set := fstest.MapFS{}
	for _, entry := range entries {
		digits, _, _ := strings.Cut(entry.Name(), "_")
		number, err := strconv.Atoi(digits)
		if err != nil {
			t.Fatalf("%s: %v", entry.Name(), err)
		}
		if number > version {
			continue
		}
		data, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		set["migrations/"+entry.Name()] = &fstest.MapFile{Data: data}
	}
	return set
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
		t.Fatalf("a hold on an untouched table refused: %v", refusals(report))
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
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "pid "+strconv.FormatUint(uint64(pid), 10)) || !strings.Contains(got[0], "has held a lock on things for") {
		t.Errorf("refusals = %v, want one naming pid %d", got, pid)
	}
}

// A foreign key an earlier pending migration adds is not in the catalog yet, but the migrations
// apply in order at boot, so a later one's rows reach through it. Here 0002 gives things a key to
// parent that cascades, and 0003 deletes from parent: an old transaction holding things refuses
// 0003 as well as 0002, which names things itself.
func TestCensusFollowsAForeignKeyAnEarlierPendingMigrationAdds(t *testing.T) {
	ctx := context.Background()
	store, url := migratedToOne(t)
	if _, err := store.Pool.Exec(ctx, "create table parent (id integer primary key)"); err != nil {
		t.Fatal(err)
	}
	set := fstest.MapFS{
		"migrations/0001_things.up.sql":        censusBase["migrations/0001_things.up.sql"],
		"migrations/0002_things_parent.up.sql": {Data: []byte("alter table things add column parent_id integer references parent on delete cascade")},
		"migrations/0003_parent_purge.up.sql":  {Data: []byte("delete from parent where id < 0")},
	}
	pid := "pid " + strconv.FormatUint(uint64(holdLock(t, store, "things", "access share")), 10)
	time.Sleep(1100 * time.Millisecond)
	report, err := census(ctx, url, set, pgmigrate.CensusOptions{LongTransaction: time.Second})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); !slices.ContainsFunc(got, func(r string) bool {
		return strings.HasPrefix(r, "0003_parent_purge.up.sql: "+pid) && strings.Contains(r, "has held a lock on things")
	}) {
		t.Errorf("refusals = %v, want one for 0003 naming %s on things", got, pid)
	}
	if out := reportText(report); !strings.Contains(out, "census: 0003_parent_purge.up.sql touches things through a foreign key with parent: ") {
		t.Errorf("report:\n%s", out)
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
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "could not read things within 5s (SQLSTATE 55P03)") || !strings.Contains(got[0], "pid "+strconv.FormatUint(uint64(pid), 10)) {
		t.Errorf("refusals = %v, want one naming the table, 55P03 and pid %d", got, pid)
	}
	out := reportText(report)
	if !strings.Contains(out, "touches things: not read, its lock was not granted within 5s") || !strings.Contains(out, "census: transactions open longer than 1m0s: none") {
		t.Errorf("report:\n%s", out)
	}
}

// A migration that writes rows also locks the tables its foreign keys connect, though it names
// none of them: deleting an ask cascades into ask_followers and user_ask_snooze and checks
// comments and artifact_reviews for rows that still reference it. The census reads those tables
// from the catalog and checks them as it checks a table the migration names. Here a scratch 0056
// deletes an ask with a follower, against the real set at 0055: an old transaction holding a row
// of ask_followers refuses it, and so does a session holding ask_followers in ACCESS EXCLUSIVE,
// since at boot the migration's cascade would wait on either until its lock timeout.
func TestCensusChecksTheTablesAForeignKeyReachesFromARowTheMigrationWrites(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 55)
	for _, statement := range []string{
		`insert into projects (key, name) values ('CORE', 'Core')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('CORE-1', 'CORE', 1, 'Spec', '{"kind":"session","id":"s"}', 'U')`,
		`insert into asks (issue_key, author, question) values ('CORE-1', '{"kind":"session","id":"s"}', 'scratch?')`,
		`insert into ask_followers (ask_id, session_id) select id, 's' from asks`,
	} {
		if _, err := store.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	set := migrationsThrough(t, 55)
	set["migrations/0056_scratch_delete.up.sql"] = &fstest.MapFile{Data: []byte("delete from asks where question = 'scratch?';")}
	set["migrations/0056_scratch_delete.census.sql"] = &fstest.MapFile{Data: []byte("select 0")}
	url := store.Pool.Config().ConnString()

	holder, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close(context.Background()) })
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := held.Exec(ctx, "update ask_followers set since = now()"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	report, err := census(ctx, url, set, pgmigrate.CensusOptions{LongTransaction: time.Second})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	pid := "pid " + strconv.FormatUint(uint64(holder.PgConn().PID()), 10)
	if got := refusals(report); len(got) != 1 || !strings.HasPrefix(got[0], "0056_scratch_delete.up.sql: "+pid) || !strings.Contains(got[0], "has held a lock on ask_followers for") {
		t.Errorf("refusals = %v, want one for 0056 naming %s on ask_followers", got, pid)
	}
	out := reportText(report)
	for _, table := range []string{"ask_followers", "user_ask_snooze", "comments", "artifact_reviews"} {
		if !strings.Contains(out, "census: 0056_scratch_delete.up.sql touches "+table+" through a foreign key with asks: ") {
			t.Errorf("report lacks %s, reached from asks:\n%s", table, out)
		}
	}
	if err := held.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	exclusive := "pid " + strconv.FormatUint(uint64(holdLock(t, store, "ask_followers", "access exclusive")), 10)
	report, err = census(ctx, url, set, pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if got := refusals(report); len(got) != 1 || !strings.Contains(got[0], "could not read ask_followers within 5s (SQLSTATE 55P03)") || !strings.Contains(got[0], exclusive) {
		t.Errorf("refusals = %v, want one naming ask_followers, 55P03 and %s", got, exclusive)
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
