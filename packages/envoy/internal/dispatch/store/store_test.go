package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"net/url"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	databaseURL := os.Getenv("DISPATCH_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DISPATCH_TEST_DATABASE_URL must be set to run Postgres store tests")
	}
	return databaseURL
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(context.Background(), testDatabaseURL(t))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(store.Pool.Close)
	return store
}

func openEmptyTestStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	baseURL, err := url.Parse(testDatabaseURL(t))
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	adminURL := *baseURL
	adminURL.Path = "/postgres"
	admin, err := pgxpool.New(ctx, adminURL.String())
	if err != nil {
		t.Fatalf("open admin database: %v", err)
	}
	t.Cleanup(admin.Close)

	databaseName := "dispatch_test_" + randomDatabaseSuffix(t)
	if _, err := admin.Exec(ctx, "create database "+databaseName); err != nil {
		t.Fatalf("create isolated database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "drop database "+databaseName+" with (force)"); err != nil {
			t.Errorf("drop isolated database: %v", err)
		}
	})

	testURL := *baseURL
	testURL.Path = "/" + databaseName
	store, err := Open(ctx, testURL.String())
	if err != nil {
		t.Fatalf("open isolated database: %v", err)
	}
	t.Cleanup(store.Pool.Close)
	return store
}

func randomDatabaseSuffix(t *testing.T) string {
	t.Helper()
	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatalf("random database suffix: %v", err)
	}
	return hex.EncodeToString(bytes[:])
}

func TestMigrateCreatesEmptySchemaAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	expectedTables := []string{
		"users",
		"projects",
		"repo_projects",
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
	assertDatabaseObjects(t, ctx, store.Pool, `
		select tablename
		from pg_tables
		where schemaname = current_schema()
	`, expectedTables)

	expectedIndexes := []string{
		"issue_external_links_url",
		"artifacts_one_primary",
		"asks_open",
		"comments_ask_id",
		"refs_to",
		"events_unpublished",
		"issues_search",
		"comments_search",
		"asks_search",
		"messages_search",
	}
	assertDatabaseObjects(t, ctx, store.Pool, `
		select indexname
		from pg_indexes
		where schemaname = current_schema()
	`, expectedIndexes)

	assertDatabaseObjects(t, ctx, store.Pool, `
		select table_name
		from information_schema.columns
		where table_schema = current_schema() and column_name = 'search' and is_generated = 'ALWAYS'
	`, []string{"issues", "artifact_versions", "comments", "asks", "messages"})

	expectedConstraints := []string{
		"users_pkey",
		"projects_pkey",
		"projects_key_check",
		"repo_projects_pkey",
		"repo_projects_project_fkey",
		"issues_pkey",
		"issues_project_key_fkey",
		"issues_project_key_number_key",
		"issues_parent_key_fkey",
		"issue_external_links_pkey",
		"issue_external_links_issue_key_fkey",
		"artifacts_pkey",
		"artifacts_issue_key_fkey",
		"artifacts_issue_key_slug_key",
		"artifacts_kind_check",
		"artifacts_primary_is_doc",
		"artifact_versions_pkey",
		"artifact_versions_artifact_id_fkey",
		"artifact_versions_artifact_id_number_key",
		"doc_updates_pkey",
		"doc_updates_artifact_id_fkey",
		"doc_snapshots_pkey",
		"doc_snapshots_artifact_id_fkey",
		"doc_checkpoints_pkey",
		"doc_checkpoints_artifact_id_fkey",
		"asks_pkey",
		"asks_issue_key_fkey",
		"asks_state_check",
		"asks_resolution_state_check",
		"asks_answer_state_check",
		"comments_pkey",
		"comments_issue_key_fkey",
		"comments_reply_to_fkey",
		"comments_ask_id_fkey",
		"messages_pkey",
		"messages_issue_key_fkey",
		"refs_pkey",
		"events_pkey",
		"events_issue_key_fkey",
		"events_issue_key_seq_key",
		"user_issue_state_pkey",
		"user_issue_state_issue_key_fkey",
	}
	assertDatabaseObjects(t, ctx, store.Pool, `
		select conname
		from pg_constraint
		where connamespace = current_schema()::regnamespace
	`, expectedConstraints)

	files, err := fs.Glob(migrationFiles, "migrations/*.up.sql")
	if err != nil {
		t.Fatalf("list embedded migrations: %v", err)
	}
	var recordedMigrations int
	if err := store.Pool.QueryRow(ctx, "select count(*) from schema_migrations").Scan(&recordedMigrations); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if recordedMigrations != len(files) {
		t.Errorf("recorded migrations: got %d, want %d", recordedMigrations, len(files))
	}
}

func assertDatabaseObjects(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, expected []string) {
	t.Helper()
	rows, err := pool.Query(ctx, query)
	if err != nil {
		t.Fatalf("list database objects: %v", err)
	}
	defer rows.Close()
	objects := map[string]bool{}
	for rows.Next() {
		var object string
		if err := rows.Scan(&object); err != nil {
			t.Fatalf("scan database object: %v", err)
		}
		objects[object] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate database objects: %v", err)
	}
	for _, object := range expected {
		if !objects[object] {
			t.Errorf("migration did not create %s", object)
		}
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

func TestMigrateMarksLegacyNonNotifyingEventsPublished(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// A database that stopped at the notify-only outbox: forget the backfill, then record
	// an event of the kind that outbox never published.
	if _, err := store.Pool.Exec(ctx, "delete from schema_migrations where version = 4"); err != nil {
		t.Fatalf("forget backfill migration: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by)
			values ('CORE-1', 'CORE', 1, 'Legacy', '{"kind":"session","id":"s"}');
		insert into events (issue_key, seq, type, actor, payload, notify)
			values ('CORE-1', 1, 'issue.created', '{"kind":"session","id":"s"}', '{}', false),
			       ('CORE-1', 2, 'comment.created', '{"kind":"user","id":"alice"}', '{}', true);
	`); err != nil {
		t.Fatalf("seed legacy events: %v", err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate again: %v", err)
	}
	var legacyPublished, notifyingPublished bool
	if err := store.Pool.QueryRow(ctx, `
		select
			bool_and(published_at is not null) filter (where not notify),
			bool_and(published_at is not null) filter (where notify)
		from events
	`).Scan(&legacyPublished, &notifyingPublished); err != nil {
		t.Fatalf("read events: %v", err)
	}
	if !legacyPublished {
		t.Error("legacy non-notifying event is still unpublished; the widened outbox scan would replay it")
	}
	if notifyingPublished {
		t.Error("notifying event was marked published by the backfill; it must still reach the outbox")
	}
}
