package store

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/migrationid"

	"github.com/sjawhar/legion/daemon/internal/store/migrations"
)

// A migration added after the numbered series is named for the UTC moment its
// author created it, so two branches open at once cannot claim one identifier.
// t1 was created first and t2 a day later; whichever merges first, both run
// exactly once on every database.
const (
	t1Version = 20261030120000
	t2Version = 20261031120000
)

// craftedMigration is a timestamped migration that leaves a mark a second run
// cannot hide: it creates its table, which a rerun would fail on, and inserts
// one row, so the row count says how many times it ran.
func craftedMigration(version int, slug, table string) migrations.Migration {
	return migrations.Migration{
		Version: version,
		Name:    fmt.Sprintf("%d_%s.up.sql", version, slug),
		SQL: fmt.Sprintf(`create table %s (applied_at timestamptz not null default now());
insert into %s default values;`, table, table),
	}
}

// numberedWith is the set these tests migrate: the daemon's numbered
// migrations — the embedded ones at or below the number migrationid says its
// series closed at — and the extras, in version order, as migrations.Load
// returns. The daemon's own timestamped migrations are left out, and the
// closing number is read rather than written down here so that adding one
// changes neither: a shipped timestamp would otherwise stand above t2 as the
// set's highest version, or widen schema_version.version before any crafted
// migration reached it, and these tests would be reading the daemon's
// migrations rather than the ones they applied.
func numberedWith(t *testing.T, extra ...migrations.Migration) []migrations.Migration {
	t.Helper()
	all, err := migrations.All()
	if err != nil {
		t.Fatalf("read the embedded migrations: %v", err)
	}
	versions := make(map[int]string, len(all))
	for _, m := range all {
		versions[m.Version] = m.Name
	}
	closedAt, err := migrationid.ClosedAt(versions)
	if err != nil {
		t.Fatalf("find the number the daemon's numbered series closed at: %v", err)
	}
	set := make([]migrations.Migration, 0, len(all)+len(extra))
	for _, m := range all {
		if m.Version <= closedAt {
			set = append(set, m)
		}
	}
	set = append(set, extra...)
	slices.SortFunc(set, func(a, b migrations.Migration) int { return cmp.Compare(a.Version, b.Version) })
	return set
}

func countRows(t *testing.T, store *Store, table string) int {
	t.Helper()
	var count int
	if err := store.pool.QueryRow(context.Background(), "select count(*) from "+table).Scan(&count); err != nil {
		t.Fatalf("count rows in %s: %v", table, err)
	}
	return count
}

// timesRecorded is how many schema_version rows name a version: a migration
// that ran twice would have two, and the primary key would have refused the
// second.
func timesRecorded(t *testing.T, store *Store, version int) int {
	t.Helper()
	var count int
	if err := store.pool.QueryRow(context.Background(),
		"select count(*) from schema_version where version = $1", version).Scan(&count); err != nil {
		t.Fatalf("count schema_version rows for %d: %v", version, err)
	}
	return count
}

func schemaVersionColumnType(t *testing.T, store *Store) string {
	t.Helper()
	var dataType string
	if err := store.pool.QueryRow(context.Background(), `select data_type from information_schema.columns
		where table_schema = current_schema() and table_name = 'schema_version' and column_name = 'version'`).Scan(&dataType); err != nil {
		t.Fatalf("read the type of schema_version.version: %v", err)
	}
	return dataType
}

// The proof the identifiers are worth their width: a database migrated by a
// daemon carrying only t2 — the later timestamp, merged first — takes t1 when
// the daemon that carries it arrives, and t2 does not run a second time.
func TestMigrateAppliesATimestampedMigrationMergedAfterALaterOne(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	t1 := craftedMigration(t1Version, "first_written", "timestamped_first_written")
	t2 := craftedMigration(t2Version, "merged_first", "timestamped_merged_first")

	withT2 := numberedWith(t, t2)
	applied, err := store.migrate(ctx, withT2)
	if err != nil {
		t.Fatalf("migrate with the later timestamp alone: %v", err)
	}
	if applied != len(withT2) {
		t.Fatalf("the first migrate applied %d migrations, want all %d", applied, len(withT2))
	}

	withBoth := numberedWith(t, t1, t2)
	applied, err = store.migrate(ctx, withBoth)
	if err != nil {
		t.Fatalf("migrate with the earlier timestamp added: %v", err)
	}
	if applied != 1 {
		t.Errorf("the second migrate applied %d migrations, want only the one it lacked", applied)
	}
	for _, m := range []struct {
		migration migrations.Migration
		table     string
	}{{t1, "timestamped_first_written"}, {t2, "timestamped_merged_first"}} {
		if recorded := timesRecorded(t, store, m.migration.Version); recorded != 1 {
			t.Errorf("schema_version has %d rows for %s, want 1", recorded, m.migration.Name)
		}
		if rows := countRows(t, store, m.table); rows != 1 {
			t.Errorf("%s has %d rows, want the one its migration inserts", m.table, rows)
		}
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read the schema version: %v", err)
	}
	if version != t2Version {
		t.Errorf("schema version = %d, want the highest timestamp %d", version, t2Version)
	}

	applied, err = store.migrate(ctx, withBoth)
	if err != nil {
		t.Fatalf("third migrate: %v", err)
	}
	if applied != 0 {
		t.Errorf("the third migrate applied %d migrations, want 0", applied)
	}
}

func TestMigrateAppliesEveryTimestampedMigrationOnceOnAFreshDatabase(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	t1 := craftedMigration(t1Version, "first_written", "timestamped_first_written")
	t2 := craftedMigration(t2Version, "merged_first", "timestamped_merged_first")
	all := numberedWith(t, t1, t2)

	applied, err := store.migrate(ctx, all)
	if err != nil {
		t.Fatalf("migrate a fresh database: %v", err)
	}
	if applied != len(all) {
		t.Fatalf("migrate applied %d migrations, want all %d", applied, len(all))
	}
	for _, m := range []struct {
		version int
		table   string
	}{{t1Version, "timestamped_first_written"}, {t2Version, "timestamped_merged_first"}} {
		if recorded := timesRecorded(t, store, m.version); recorded != 1 {
			t.Errorf("schema_version has %d rows for %d, want 1", recorded, m.version)
		}
		if rows := countRows(t, store, m.table); rows != 1 {
			t.Errorf("%s has %d rows, want the one its migration inserts", m.table, rows)
		}
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read the schema version: %v", err)
	}
	if version != t2Version {
		t.Errorf("schema version = %d, want the highest timestamp %d", version, t2Version)
	}
}

// 0001_init made schema_version.version an integer, which a 14-digit timestamp
// is past. The first timestamped migration a database takes widens the column
// inside its own transaction, and a database that already took one widens
// nothing.
func TestTheFirstTimestampedMigrationWidensTheVersionColumn(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	if _, err := store.migrate(ctx, numberedWith(t)); err != nil {
		t.Fatalf("migrate through the numbered set: %v", err)
	}
	if dataType := schemaVersionColumnType(t, store); dataType != "integer" {
		t.Fatalf("schema_version.version is %s on a database migrated through the numbered set, want integer", dataType)
	}

	t1 := craftedMigration(t1Version, "first_written", "timestamped_first_written")
	applied, err := store.migrate(ctx, numberedWith(t, t1))
	if err != nil {
		t.Fatalf("migrate with a timestamped migration: %v", err)
	}
	if applied != 1 {
		t.Fatalf("migrate applied %d migrations, want only the timestamped one", applied)
	}
	if dataType := schemaVersionColumnType(t, store); dataType != "bigint" {
		t.Errorf("schema_version.version is %s after a timestamped migration, want bigint", dataType)
	}
	version, err := store.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read the schema version: %v", err)
	}
	if version != t1Version {
		t.Errorf("schema version = %d, want the timestamp %d", version, t1Version)
	}

	t2 := craftedMigration(t2Version, "merged_first", "timestamped_merged_first")
	applied, err = store.migrate(ctx, numberedWith(t, t1, t2))
	if err != nil {
		t.Fatalf("migrate with a second timestamped migration: %v", err)
	}
	if applied != 1 {
		t.Errorf("migrate applied %d migrations, want only the second timestamped one", applied)
	}
	if dataType := schemaVersionColumnType(t, store); dataType != "bigint" {
		t.Errorf("schema_version.version is %s after a second timestamped migration, want bigint", dataType)
	}
}
