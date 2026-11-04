package pgclone

// The pool's own tests, against a schema of this file's own: two tables, a standalone sequence
// and a trigger function. The pool reads every table, column, sequence and constraint it works
// on from the template at prepare time, so a four-relation schema exercises it as a harness's
// ninety-table one does, and the file costs the clones it takes rather than anybody's migrations.
//
// Each test is one promise the two storetest harnesses rest on: a database a test only wrote
// rows into comes back emptied and is handed on, a database a test changed the schema of is
// dropped instead, and a fresh database is nobody's leftovers. Every one of the schema changes
// below has shipped past a fingerprint that did not look for it.
//
// No top-level test here calls t.Parallel: each reads back the database it was handed by name,
// which the free list only answers while nothing else is taking from it. The schema cases do run
// together, as subtests of one test: what each of them observes is its own database's fate
// (release), not a count, and seven create database and seven drop database in a row are seven
// waits on the one checkpointer every package on the server queues behind (dispatch://INFRA-1075).
//
// The tests are in package pgclone rather than pgclone_test because what they observe - the free
// list, the refusal count - is the pool's decision about a database, which it exports no reading
// of. An accessor for them would be a second surface to keep, and the harnesses would never call
// it.
//
// Main reports what the tests cost it on standard error, so a passing run prints a line naming
// the databases this file's schema cases had dropped. That line is the file working.

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool hands these tests their databases, as shared does in either storetest. The server is
// DISPATCH_TEST_DATABASE_URL's, the variable internal/testpg reuses rather than doubles for the
// same reason: it is how every Postgres-backed test of this module is already told which server
// its lane provides, and a second name would have to be set beside it on every job.
var testPool = New(Config{
	Name:           "pgclone's own tests",
	DatabaseURLEnv: "DISPATCH_TEST_DATABASE_URL",
	SkipMessage:    "DISPATCH_TEST_DATABASE_URL must be set to run the pgclone tests",
	DatabasePrefix: "pgclone_test_",
	TemplatePrefix: "pgclone_test_template_",
	SeedSchema:     "pgclone_test_seed",
	Migrate:        migrateTestTemplate,
})

func TestMain(m *testing.M) { os.Exit(testPool.Main(m)) }

// templateSchema is what this file's migrations are. widgets is seeded and events is not, so one
// test reads what the reset puts back and what it empties; the identity sequence and the
// standalone batches sequence are the two kinds the reset has to restore, since truncate ...
// restart identity reaches the first and only the setval list reaches the second. note_widget is
// here rather than in the trigger case so that adding a trigger adds nothing but a trigger.
const templateSchema = `
create table widgets (
	id bigint generated always as identity primary key,
	label text not null
);
create table events (
	id bigserial primary key,
	body text
);
create sequence batches;
create function note_widget() returns trigger language plpgsql as $$ begin return new; end $$;
insert into widgets (label) values ('alpha'), ('beta');
select nextval('batches')`

// migrateTestTemplate builds the template every clone of these tests comes from. The analyze is
// what makes a fresh clone carry statistics at all, which is what the reset's own analyze is
// measured against (TestTheResetLeavesTheStatisticsOfAFreshClone); a harness's migrations analyse
// nothing, so this stands in for the autovacuum that would have.
func migrateTestTemplate(ctx context.Context, dsn string) (Querier, func(), error) {
	template, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open the template database: %w", err)
	}
	for _, statement := range []string{templateSchema, "analyze"} {
		if _, err := template.Exec(ctx, statement); err != nil {
			template.Close()
			return nil, nil, fmt.Errorf("migrate the template database: %w", err)
		}
	}
	return template, template.Close, nil
}

// A test that only writes rows is every test the harnesses have but a handful, and the free list
// is why the two packages do not pay a create database and a drop database each. Broken, this
// reads either "was dropped" - the pool refusing a database nothing is wrong with, which is the
// whole saving gone and silently, since a dropped database is silent by design - or a reused
// database still holding the rows, the sequence or the missing seed rows of the test before.
func TestADatabaseATestOnlyWroteRowsIntoIsReusedEmptied(t *testing.T) {
	ctx := context.Background()
	name := testPool.Take(t, true)
	conn := connect(t, name)
	exec(t, conn, `
		insert into widgets (label) values ('written by the test');
		insert into events (body) values ('one'), ('two');
		select setval('batches', 50)`)

	if outcome := release(t, name, func() { conn.Close(ctx) }); outcome != reused {
		t.Fatalf("the database of a test that only wrote rows was %s, not put back on the free list", outcome)
	}
	again := testPool.Take(t, true)
	if again != name {
		t.Fatalf("the next test was handed %s, not the emptied %s the free list held", again, name)
	}
	back := connect(t, again)
	defer release(t, again, func() { back.Close(ctx) })

	if held := probe(t, back, contentQuery); held != templateContent {
		t.Fatalf("the reused database holds %s; a fresh clone of the template holds %s", held, templateContent)
	}
	if next := probe(t, back, sequenceQuery); next != templateSequences {
		t.Fatalf("the reused database hands out %s; a fresh clone of the template hands out %s", next, templateSequences)
	}
}

// One case per term the fingerprint carries, because each was added to it after the change
// before had slipped past: a column default (CI run 41729266538, 67 api tests failing on a
// duplicate messages_pkey) and a constraint (the architecture importer's check, which a reviewer
// found and no test did) were both shipped missing. Broken - a term dropped from
// fingerprintQuery - the case reads "was reused", and the next test to take that database is
// handed a schema it never wrote: a default that hands out one id, a constraint that refuses
// rows, a trigger that runs code.
//
// A relation is one case rather than three: an index adds nothing but a pg_class row, so it is
// the one of the three that no other term of the fingerprint would catch anyway. A table also
// adds columns and a sequence is read back by the reset's setval list.
//
// Every case here is a right this pod's role has: it owns the database it was handed and
// everything in it. Nothing is left out.
func TestADatabaseWhoseSchemaATestChangedIsDropped(t *testing.T) {
	for _, change := range []struct {
		name      string
		statement func(database string) string
	}{
		{"a column's default altered", func(string) string {
			return "alter table events alter column body set default 'written by the test'"
		}},
		{"a constraint added", func(string) string {
			return "alter table widgets add constraint widgets_label_short check (length(label) < 64)"
		}},
		{"a relation added", func(string) string {
			return "create index widgets_by_label on widgets (label)"
		}},
		{"a function added", func(string) string {
			return "create function added_by_the_test() returns int language sql as 'select 1'"
		}},
		{"a trigger added", func(string) string {
			return "create trigger widgets_noted before insert on widgets for each row execute function note_widget()"
		}},
		{"a schema added", func(string) string {
			return "create schema added_by_the_test"
		}},
		{"a database setting set", func(database string) string {
			return "alter database " + database + " set search_path = public"
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			// Each case reads its own database's fate by name, so the cases wait on the
			// server's one checkpointer together rather than one after another.
			t.Parallel()
			ctx := context.Background()
			name := testPool.Take(t, true)
			conn := connect(t, name)
			exec(t, conn, change.statement(name))
			if outcome := release(t, name, func() { conn.Close(ctx) }); outcome != refused {
				t.Fatalf("a database with %s was %s; the test after it would be handed a schema it never wrote", change.name, outcome)
			}
		})
	}
}

// The schemas of a temporary table stay in the catalog after the session that made them ends, so
// a fingerprint that counted them would drop a database for a test that wrote one scratch table -
// and go on dropping, since every test that takes a clone makes another. Broken, this reads "was
// dropped", and a package whose tests use temporary tables pays a create database and a drop
// database for each of them.
func TestATemporaryTableDoesNotCostADatabase(t *testing.T) {
	ctx := context.Background()
	name := testPool.Take(t, true)
	conn := connect(t, name)
	exec(t, conn, "create temporary table scratch (note text) ; insert into scratch values ('written by the test')")
	// The schema's own name, read while the session that owns it is still open, is what the
	// check below holds the catalog to: without it this case would pass on a server that had
	// quietly stopped leaving the schema behind, and prove nothing.
	temporary := probe(t, conn, "select pg_my_temp_schema()::regnamespace::text")

	if outcome := release(t, name, func() { conn.Close(ctx) }); outcome != reused {
		t.Fatalf("the database of a test that made one temporary table was %s", outcome)
	}
	again := testPool.Take(t, true)
	if again != name {
		t.Fatalf("the next test was handed %s, not the emptied %s the free list held", again, name)
	}
	back := connect(t, again)
	defer release(t, again, func() { back.Close(ctx) })
	left := probe(t, back, "select count(*)::text from pg_namespace where nspname = '"+temporary+"'")
	if left != "1" {
		t.Fatalf("the temporary schema %s left the catalog with its session, so this test holds the fingerprint to nothing", temporary)
	}
}

// A truncate empties a table and leaves pg_statistic describing the rows that were in it, so a
// reused database would plan the next test's queries on the row counts, the distinct values and
// the common values of the test before - which is a test that reads one answer on a clone and
// another on a recycled database. The reset analyses what it emptied, and this is that line's
// test: broken (the analyze cut from resetStatements), it reads n_distinct=1 and
// most_common_vals={hot}, the 5000 rows this test wrote, where a fresh clone reads the two the
// template seeded.
//
// It is widgets, a table the template seeds, because the reset analyses after it has put the
// template's rows back, and PostgreSQL's ANALYZE of a table that holds no row at that moment
// replaces no statistics at all (verified on the server this runs against, PostgreSQL 18): a
// table the template leaves empty is where this behaviour cannot be seen.
func TestTheResetLeavesTheStatisticsOfAFreshClone(t *testing.T) {
	ctx := context.Background()
	fresh := testPool.Take(t, false)
	freshConn := connect(t, fresh)
	want := probe(t, freshConn, statisticsQuery)
	freshConn.Close(ctx)
	testPool.Drop(fresh)

	name := testPool.Take(t, true)
	conn := connect(t, name)
	exec(t, conn, "insert into widgets (label) select 'hot' from generate_series(1, 5000) ; analyze widgets")
	left := probe(t, conn, statisticsQuery)
	if left == want {
		t.Fatalf("the 5000 rows this test wrote left the statistics a fresh clone carries (%s), so it holds the reset to nothing", want)
	}

	if outcome := release(t, name, func() { conn.Close(ctx) }); outcome != reused {
		t.Fatalf("the database of a test that only wrote rows was %s", outcome)
	}
	again := testPool.Take(t, true)
	if again != name {
		t.Fatalf("the next test was handed %s, not the emptied %s the free list held", again, name)
	}
	back := connect(t, again)
	defer release(t, again, func() { back.Close(ctx) })
	if got := probe(t, back, statisticsQuery); got != want {
		t.Fatalf("the reused database plans widgets.label on %s, the statistics of the test before (%s); a fresh clone plans it on %s",
			got, left, want)
	}
}

// OpenFresh's half of the pool: Take(t, false) is for a test that counts what a sweep or a
// server wrote across the whole of a table, where a row an earlier test left in a table the reset
// does not reach would change the answer, and for one that leaves a session of its own behind.
// Broken - Take taking from the free list whatever it was asked for - it reads "handed back the
// free list's", and those tests read another test's rows. The drop is the other half: a database
// a test held a session on past its cleanup must leave the server rather than wait on the free
// list for a test that will find it occupied.
func TestAFreshDatabaseIsNobodysLeftoversAndIsDropped(t *testing.T) {
	ctx := context.Background()
	warm := testPool.Take(t, true)
	warmConn := connect(t, warm)
	exec(t, warmConn, "insert into events (body) values ('written by the test')")
	if outcome := release(t, warm, func() { warmConn.Close(ctx) }); outcome != reused {
		t.Fatalf("the database of a test that only wrote rows was %s, so the free list is empty and this test holds Take to nothing", outcome)
	}

	fresh := testPool.Take(t, false)
	if fresh == warm {
		t.Fatalf("a fresh database was handed back the free list's %s", warm)
	}
	if !onFreeList(warm) {
		t.Fatalf("a fresh database took %s off the free list, which the next ordinary test is owed", warm)
	}
	conn := connect(t, fresh)
	if held := probe(t, conn, contentQuery); held != templateContent {
		t.Fatalf("a fresh database holds %s, not the template's own %s", held, templateContent)
	}
	conn.Close(ctx)

	testPool.Drop(fresh)
	waitDropped(t, fresh)
	if onFreeList(fresh) {
		t.Fatalf("the dropped %s is on the free list, where the next test would take a database the server has not got", fresh)
	}
}

// contentQuery and templateContent are the rows of the two tables as the next test reads them:
// the template's seeded widgets, and the events no migration wrote.
const (
	contentQuery = `select format('widgets=%s events=%s',
		(select coalesce(string_agg(label, ',' order by id), 'none') from widgets),
		(select count(*) from events))`
	templateContent = "widgets=alpha,beta events=0"
)

// sequenceQuery and templateSequences are where the template left its two sequences: the next
// identity widgets hands out, after the two rows the template seeded, and the next value of the
// standalone sequence the template called once. It is the two sequences the reset has to put
// back by different means, and asking for the next value of each is what the test after a reset
// meets - an identity left at 1 is the duplicate key the next insert fails on. It advances them,
// so it is the last thing a test asks of a database.
const (
	sequenceQuery = `select format('widgets.id=%s batches=%s',
		nextval(pg_get_serial_sequence('widgets', 'id')), nextval('batches'))`
	templateSequences = "widgets.id=3 batches=2"
)

// statisticsQuery is what the planner reads about widgets.label. pg_stats rather than
// pg_statistic: the catalog is readable by a superuser alone, and the view is what a test role
// sees of its own tables.
const statisticsQuery = `select coalesce((
	select format('n_distinct=%s most_common_vals=%s', n_distinct, coalesce(most_common_vals::text, 'none'))
	from pg_stats where schemaname = 'public' and tablename = 'widgets' and attname = 'label'), 'none')`

// outcome is what a reset worker did with a database Release handed it.
type outcome string

const (
	reused      outcome = "put back on the free list"
	refused     outcome = "dropped: it is not the template's copy any more"
	resetFailed outcome = "dropped: its reset failed"
)

// releaseWait bounds how long a release waits for its worker. A reset is a handful of statements
// on a database the test has let go of, so this is a deadline for a pool that has stopped
// answering rather than for a slow server: nothing waits anywhere near it.
const releaseWait = 60 * time.Second

// release hands name back the way a harness's cleanup does and answers what the pool decided.
// Every release ends in one of the three: the database goes on the free list, or the pool will
// not reuse it and drops it, or its reset failed and it is dropped for that. The first two are
// read by name - the free list holds it, or the server no longer does - so releases running
// beside each other are each read for themselves; only the third, which no test here expects,
// is read from the pool's count, and its log line says a reset failed rather than naming this
// database's.
func release(t *testing.T, name string, close func()) outcome {
	t.Helper()
	_, wereFailed := refusals()
	testPool.Release(name, close)
	for deadline := time.Now().Add(releaseWait); ; time.Sleep(time.Millisecond) {
		if onFreeList(name) {
			return reused
		}
		if dropped(t, name) {
			if _, nowFailed := refusals(); nowFailed > wereFailed {
				testPool.resetMu.Lock()
				first := testPool.resetFirst
				testPool.resetMu.Unlock()
				t.Logf("a reset failed while %s was released: %s", name, first)
				return resetFailed
			}
			return refused
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reset worker took %s back within %s", name, releaseWait)
		}
	}
}

// refusals is how many databases the pool has refused to reuse and how many resets have failed.
func refusals() (changed, failed int) {
	testPool.resetMu.Lock()
	defer testPool.resetMu.Unlock()
	return testPool.changed, testPool.resetFails
}

// onFreeList reports whether name is waiting for the next test that asks for a database.
func onFreeList(name string) bool {
	testPool.freeMu.Lock()
	defer testPool.freeMu.Unlock()
	return slices.Contains(testPool.free, name)
}

// dropped reports whether the server no longer holds name, which is where a database the pool
// would not reuse ends: the reset's decision carries no name, and this does.
func dropped(t *testing.T, name string) bool {
	t.Helper()
	var left int
	if err := testPool.Admin().QueryRow(context.Background(), "select count(*) from pg_database where datname = $1", name).Scan(&left); err != nil {
		t.Fatalf("read whether %s is still on the server: %v", name, err)
	}
	return left == 0
}

// waitDropped waits for the drop Drop started in the background. It reads the server rather than
// the pool's wait group, which only Main may wait on.
func waitDropped(t *testing.T, name string) {
	t.Helper()
	for deadline := time.Now().Add(releaseWait); ; time.Sleep(time.Millisecond) {
		if dropped(t, name) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was still on the server %s after it was dropped", name, releaseWait)
		}
	}
}

// connect opens the one session a test holds on its database, as a harness's store does.
func connect(t *testing.T, name string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), testPool.URL(name))
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	return conn
}

// exec runs what a test does to its database. Several statements at once are one round trip
// under the simple protocol, which is how the reset runs its own.
func exec(t *testing.T, conn *pgx.Conn, statements string) {
	t.Helper()
	if _, err := conn.Exec(context.Background(), statements); err != nil {
		t.Fatalf("run %s: %v", statements, err)
	}
}

// probe reads one line about a database. Every observation in this file is one string, so a
// failure names what the next test would meet rather than a column of it.
func probe(t *testing.T, conn *pgx.Conn, query string) string {
	t.Helper()
	var answer string
	if err := conn.QueryRow(context.Background(), query).Scan(&answer); err != nil {
		t.Fatalf("run %s: %v", query, err)
	}
	return answer
}
