package store

import (
	"context"
	"os"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run Postgres store tests")
	}
	store, err := Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(store.Pool.Close)
	return store
}

func TestMigrateCreatesSchemaAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	expectedTables := []string{
		"users",
		"projects",
		"issues",
		"issue_external_links",
		"artifacts",
		"artifact_versions",
		"doc_updates",
		"doc_snapshots",
		"doc_checkpoints",
		"asks",
		"comments",
		"messages",
		"refs",
		"events",
		"user_issue_state",
	}
	rows, err := store.Pool.Query(ctx, `
		select tablename
		from pg_tables
		where schemaname = current_schema()
	`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatalf("scan table: %v", err)
		}
		tables[table] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate tables: %v", err)
	}
	for _, table := range expectedTables {
		if !tables[table] {
			t.Errorf("migration did not create %s", table)
		}
	}

	var migrations int
	if err := store.Pool.QueryRow(ctx, "select count(*) from schema_migrations").Scan(&migrations); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if migrations != 1 {
		t.Errorf("recorded migrations: got %d, want 1", migrations)
	}
}

func TestMigrationVersionParsesNumericFilenamePrefix(t *testing.T) {
	for _, tc := range []struct {
		filename string
		want     int
	}{
		{filename: "migrations/0001_init.up.sql", want: 1},
		{filename: "migrations/0012_add_events.up.sql", want: 12},
	} {
		got, err := migrationVersion(tc.filename)
		if err != nil {
			t.Errorf("%s: %v", tc.filename, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.filename, got, tc.want)
		}
	}
}
