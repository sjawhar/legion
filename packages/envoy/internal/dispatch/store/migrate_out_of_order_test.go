package store

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
)

// The crafted set these tests apply: a numbered migration as every database already has, and two
// named for the UTC moment their authors created them. T1 was written before T2 and merged after
// it, which is what two pull requests open at once do, and which no sequence of numbers allows.
const (
	oooNumbered = "0001_numbered"
	oooT1       = "20261030120000_t1"
	oooT2       = "20261031120000_t2"
)

// oooSet is a set holding the named migrations and nothing else. Each creates a table of its own
// and inserts one row, so a migration that ran twice shows up twice over: its create fails the
// second run, and a table of its that held two rows would say the insert ran again.
func oooSet(stems ...string) fstest.MapFS {
	set := make(fstest.MapFS, len(stems))
	for _, stem := range stems {
		table := oooTable(stem)
		set["migrations/"+stem+".up.sql"] = &fstest.MapFile{Data: []byte(fmt.Sprintf(
			"create table %s (id integer primary key);\ninsert into %s (id) values (1);", table, table))}
	}
	return set
}

func oooTable(stem string) string { return "ooo_" + stem }

func oooVersion(t *testing.T, stem string) int64 {
	t.Helper()
	digits, _, _ := strings.Cut(stem, "_")
	version, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		t.Fatalf("the crafted migration %s does not begin with a version: %v", stem, err)
	}
	return version
}

// assertAppliedOnce checks that the database records exactly the stems' versions, each once, and
// that each migration's table holds the one row its insert made.
func assertAppliedOnce(t *testing.T, store *Store, stems ...string) {
	t.Helper()
	ctx := context.Background()
	rows, err := store.Pool.Query(ctx, "select version from schema_migrations order by version")
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	recorded, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	want := make([]int64, 0, len(stems))
	for _, stem := range stems {
		want = append(want, oooVersion(t, stem))
	}
	slices.Sort(want)
	if !slices.Equal(recorded, want) {
		t.Errorf("schema_migrations records %v, want %v, each version once", recorded, want)
	}
	for _, stem := range stems {
		var count int
		if err := store.Pool.QueryRow(ctx, "select count(*) from "+oooTable(stem)).Scan(&count); err != nil {
			t.Fatalf("count the rows %s inserted: %v", stem, err)
		}
		if count != 1 {
			t.Errorf("%s left %d rows in %s, want the 1 its insert makes: it applied %d times", stem, count, oooTable(stem), count)
		}
	}
}

// appliedAt is when the database recorded a migration's version.
func appliedAt(t *testing.T, store *Store, stem string) time.Time {
	t.Helper()
	var recordedAt time.Time
	if err := store.Pool.QueryRow(context.Background(),
		"select applied_at from schema_migrations where version = $1::bigint", oooVersion(t, stem)).Scan(&recordedAt); err != nil {
		t.Fatalf("read when %s was applied: %v", stem, err)
	}
	return recordedAt
}

// databaseFromBeforeTheWidening gives the store's database the schema_migrations table
// createVersionTable created before the versions became timestamps — version an integer — with
// the numbered migration such a database had already applied recorded in it.
func databaseFromBeforeTheWidening(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Pool.Exec(ctx, `
		create table schema_migrations (
			version integer primary key,
			applied_at timestamptz not null default now()
		)
	`); err != nil {
		t.Fatalf("create the version table as it was: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, "insert into schema_migrations (version) values ($1)", oooVersion(t, oooNumbered)); err != nil {
		t.Fatalf("record the numbered migration that database already applied: %v", err)
	}
}

// A migration that merges below one a database has already applied still runs, exactly once, at
// the next boot: the runner applies every version schema_migrations does not record, not only
// those above the highest it does. That is what lets two authors take timestamps and merge in
// either order.
func TestMigrateAppliesAMigrationThatArrivedBelowOneAlreadyApplied(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, oooSet(oooNumbered, oooT2)); err != nil {
		t.Fatalf("migrate the set T2 merged into: %v", err)
	}
	assertAppliedOnce(t, store, oooNumbered, oooT2)
	t2At := appliedAt(t, store, oooT2)

	// T1 merges after T2 and takes the lower version, so the next boot reads a set holding a
	// migration below the highest the database records.
	if err := store.migrate(ctx, oooSet(oooNumbered, oooT1, oooT2)); err != nil {
		t.Fatalf("migrate the set T1 merged into: %v", err)
	}
	assertAppliedOnce(t, store, oooNumbered, oooT1, oooT2)
	if again := appliedAt(t, store, oooT2); again != t2At {
		t.Errorf("T2 was recorded again at %s, having been applied at %s; a migration runs once", again, t2At)
	}
	t1At := appliedAt(t, store, oooT1)

	// A boot with the same set applies nothing: both rows keep the moments they were recorded at.
	if err := store.migrate(ctx, oooSet(oooNumbered, oooT1, oooT2)); err != nil {
		t.Fatalf("migrate the set a third time: %v", err)
	}
	assertAppliedOnce(t, store, oooNumbered, oooT1, oooT2)
	if again := appliedAt(t, store, oooT1); again != t1At {
		t.Errorf("T1 was recorded again at %s, having been applied at %s", again, t1At)
	}
	if again := appliedAt(t, store, oooT2); again != t2At {
		t.Errorf("T2 was recorded again at %s, having been applied at %s", again, t2At)
	}
}

// A database that sees both timestamped migrations for the first time applies each once, in
// version order, so it ends where the database that took them one merge at a time ended.
func TestMigrateAppliesBothTimestampedMigrationsOnceOnAFreshDatabase(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, oooSet(oooNumbered, oooT1, oooT2)); err != nil {
		t.Fatalf("migrate a fresh database: %v", err)
	}
	assertAppliedOnce(t, store, oooNumbered, oooT1, oooT2)
	if t1At, t2At := appliedAt(t, store, oooT1), appliedAt(t, store, oooT2); t1At.After(t2At) {
		t.Errorf("T1 was applied at %s and T2 at %s, want T1 first: the runner applies in version order", t1At, t2At)
	}
}

// A database migrated before the versions became timestamps has schema_migrations.version as the
// integer the runner used to create, which holds no 14-digit version. The next boot widens it to
// bigint under the runner's own lock, keeping the versions already recorded, and then records a
// timestamped one; a boot after that finds it widened and alters nothing.
func TestMigrateWidensAnIntegerVersionColumnAndRecordsATimestamp(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	databaseFromBeforeTheWidening(t, store)

	if err := store.migrate(ctx, oooSet(oooNumbered, oooT1)); err != nil {
		t.Fatalf("migrate a database whose version column is an integer: %v", err)
	}
	if got := versionColumnType(t, store); got != "bigint" {
		t.Fatalf("schema_migrations.version is %s after the migrate, want bigint", got)
	}
	var recorded int64
	if err := store.Pool.QueryRow(ctx, "select version from schema_migrations where version > $1::bigint", oooVersion(t, oooNumbered)).Scan(&recorded); err != nil {
		t.Fatalf("read the timestamped version: %v", err)
	}
	if want := oooVersion(t, oooT1); recorded != want {
		t.Errorf("schema_migrations records %d, want %d", recorded, want)
	}
	// The numbered version the database had kept its row, and the timestamped migration ran once.
	var inserted int
	if err := store.Pool.QueryRow(ctx, "select count(*) from "+oooTable(oooT1)).Scan(&inserted); err != nil {
		t.Fatalf("count the rows %s inserted: %v", oooT1, err)
	}
	if inserted != 1 {
		t.Errorf("%s left %d rows in %s, want the 1 its insert makes", oooT1, inserted, oooTable(oooT1))
	}
	var count int
	if err := store.Pool.QueryRow(ctx, "select count(*) from schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count the recorded versions: %v", err)
	}
	if count != 2 {
		t.Errorf("schema_migrations holds %d versions, want the 2 the database took", count)
	}

	if err := store.migrate(ctx, oooSet(oooNumbered, oooT1)); err != nil {
		t.Fatalf("migrate a database whose version column is already bigint: %v", err)
	}
	if got := versionColumnType(t, store); got != "bigint" {
		t.Errorf("schema_migrations.version is %s after the second migrate, want bigint", got)
	}
}

func versionColumnType(t *testing.T, store *Store) string {
	t.Helper()
	var dataType string
	if err := store.Pool.QueryRow(context.Background(), `select data_type from information_schema.columns
		where table_schema = current_schema() and table_name = 'schema_migrations' and column_name = 'version'`).Scan(&dataType); err != nil {
		t.Fatalf("read the type of schema_migrations.version: %v", err)
	}
	return dataType
}

// A health probe that read the version before the widening keeps answering after it. The probe's
// connection prepared its statement while the column was an integer, and a widened column changes
// that statement's result type: an uncast read fails once on every such connection with "cached
// plan must not change result type", which is a /healthz failure on every running process that a
// deploy's first boot of this change widens under.
func TestHealthyAnswersAcrossTheWideningOfTheVersionColumn(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	databaseFromBeforeTheWidening(t, store)
	if got, err := store.Pool.Healthy(ctx); err != nil || got != int(oooVersion(t, oooNumbered)) {
		t.Fatalf("Healthy before the widening = %d, %v; want %d", got, err, oooVersion(t, oooNumbered))
	}

	if err := store.migrate(ctx, oooSet(oooNumbered, oooT1)); err != nil {
		t.Fatalf("migrate a database whose version column is an integer: %v", err)
	}
	got, err := store.Pool.Healthy(ctx)
	if err != nil {
		t.Fatalf("Healthy after the widening: %v", err)
	}
	if want := int(oooVersion(t, oooT1)); got != want {
		t.Errorf("Healthy after the widening = %d, want %d", got, want)
	}
}

// A Dispatch process of the release before this change keeps serving while the first pod of this
// change boots, and its health probe reads `select coalesce(max(version), 0) from
// schema_migrations`, uncast, on a connection that prepared the read while the column was an
// integer. A boot whose set holds no timestamped migration records no version past an integer, so
// it must leave the column's type as it is: widening it there fails that probe once on every old
// process ("cached plan must not change result type"), a 503 from /healthz on each one mid-roll.
// The column is widened when the first timestamped migration applies, by which time every running
// process reads it cast.
func TestMigrateWithNoTimestampedMigrationKeepsThePreviousReleasesHealthProbeAnswering(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	databaseFromBeforeTheWidening(t, store)
	// One connection of a separate pool, as the previous release's health probe holds one, with
	// that release's read prepared on it.
	separate, err := store.Pool.Rooms()
	if err != nil {
		t.Fatalf("open a separate pool: %v", err)
	}
	conn, err := separate.Acquire(ctx)
	if err != nil {
		t.Fatalf("hold a connection of the separate pool: %v", err)
	}
	defer conn.Release()
	const previousReleasesProbe = "select coalesce(max(version), 0) from schema_migrations"
	var before int
	if err := conn.QueryRow(ctx, previousReleasesProbe).Scan(&before); err != nil {
		t.Fatalf("the previous release's health probe before the boot: %v", err)
	}

	if err := store.migrate(ctx, oooSet(oooNumbered)); err != nil {
		t.Fatalf("migrate with a set that holds no timestamped migration: %v", err)
	}
	var after int
	if err := conn.QueryRow(ctx, previousReleasesProbe).Scan(&after); err != nil {
		t.Fatalf("the previous release's health probe after a boot that applied no timestamped migration: %v", err)
	}
	if after != before {
		t.Errorf("the previous release's health probe reads %d after the boot, want the %d it read before", after, before)
	}
}
