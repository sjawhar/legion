package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

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
		"asks_open_artifact",
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
		"artifacts_kind_check",
		"artifacts_primary_is_doc",
		"artifacts_project_key_fkey",
		"artifacts_ref_key_key",
		"artifacts_primary_has_issue",
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
		"asks_artifact_id_fkey",
		"asks_one_owner",
		"comments_pkey",
		"comments_issue_key_fkey",
		"comments_reply_to_fkey",
		"comments_ask_id_fkey",
		"messages_pkey",
		"comments_artifact_id_fkey",
		"comments_one_owner",
		"messages_issue_key_fkey",
		"refs_pkey",
		"events_pkey",
		"events_issue_key_fkey",
		"events_issue_key_seq_key",
		"events_artifact_id_fkey",
		"events_one_owner",
		"events_artifact_id_seq_key",
		"user_issue_state_pkey",
		"user_issue_state_issue_key_fkey",
	}
	assertDatabaseObjects(t, ctx, store.Pool, `
		select conname
		from pg_constraint
		where connamespace = current_schema()::regnamespace
	`, expectedConstraints)
	assertDatabaseObjectsAbsent(t, ctx, store.Pool, `
		select conname
		from pg_constraint
		where connamespace = current_schema()::regnamespace
	`, []string{"artifacts_issue_key_slug_key"})

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

func assertDatabaseObjectsAbsent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, query string, unexpected []string) {
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
	for _, object := range unexpected {
		if objects[object] {
			t.Errorf("migration left obsolete %s", object)
		}
	}
}

func migrateThrough(t *testing.T, store *Store, maxVersion int) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Pool.Exec(ctx, `
		create table if not exists schema_migrations (
			version integer primary key,
			applied_at timestamptz not null default now()
		)
	`); err != nil {
		t.Fatalf("create schema migrations table: %v", err)
	}
	files, err := fs.Glob(migrationFiles, "migrations/*.up.sql")
	if err != nil {
		t.Fatalf("list migrations: %v", err)
	}
	for _, filename := range files {
		version, err := migrationVersion(filename)
		if err != nil {
			t.Fatalf("parse %s: %v", filename, err)
		}
		if version > maxVersion {
			continue
		}
		contents, err := migrationFiles.ReadFile(filename)
		if err != nil {
			t.Fatalf("read %s: %v", filename, err)
		}
		if err := store.applyMigration(ctx, version, string(contents)); err != nil {
			t.Fatalf("apply %s: %v", filename, err)
		}
	}
}

func seedGraphAt0007(t *testing.T, store *Store) string {
	t.Helper()
	ctx := context.Background()
	const commentID = "8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b"
	for _, statement := range []string{
		`insert into projects (key, name) values ('CORE', 'Core'), ('OPS', 'Operations')`,
		`insert into issues (key, project_key, number, title, created_by) values
			('CORE-1', 'CORE', 1, 'Core one', '{"kind":"user","id":"alice"}'),
			('CORE-2', 'CORE', 2, 'Core two', '{"kind":"user","id":"alice"}'),
			('OPS-1', 'OPS', 1, 'Operations one', '{"kind":"user","id":"alice"}')`,
		`insert into artifacts (id, issue_key, slug, name, kind, is_primary, created_by) values
			('3348cb25-ef50-432c-9aed-381326ebb1f3', 'CORE-1', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}'),
			('56d2b0bd-9ae7-47c9-8522-a13dfddcb5d5', 'CORE-2', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}'),
			('35bc0556-5ec1-4e58-9d3d-fec6ec1a9309', 'OPS-1', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}'),
			('6bd48de1-8b27-40e5-8a81-56b36dbd9661', 'CORE-2', 'diagram-png', 'diagram.png', 'image', false, '{"kind":"user","id":"alice"}')`,
		`insert into asks (id, issue_key, author, question) values
			('5a660655-04ad-4ce0-8a9b-93dd03c412b7', 'CORE-1', '{"kind":"session","id":"session-asker"}', 'Ship it?')`,
		`insert into comments (id, issue_key, author, body) values
			('8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b', 'CORE-1', '{"kind":"user","id":"alice"}', 'Looks good')`,
		`insert into events (issue_key, seq, type, actor, payload, notify) values
			('CORE-1', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{}', false),
			('CORE-2', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{}', false),
			('OPS-1', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{}', false)`,
		`insert into refs (from_kind, from_id, to_kind, to_id) values
			('comment', '8f14e45f-ceea-467a-9c1e-1b4d9a3f1c2b', 'artifact', 'CORE-2/diagram-png'),
			('artifact', '3348cb25-ef50-432c-9aed-381326ebb1f3', 'artifact', 'CORE-2/spec'),
			('ask', '5a660655-04ad-4ce0-8a9b-93dd03c412b7', 'issue', 'CORE-2')`,
	} {
		if _, err := store.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed 0007 graph: %v", err)
		}
	}
	return commentID
}

func TestMigrate0009BackfillsProjectAndRefKeyFrom0007(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 7)
	seedGraphAt0007(t, store)

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through 0009: %v", err)
	}
	for _, check := range []struct {
		name  string
		query string
	}{
		{
			name:  "artifact projects",
			query: `select count(*) from artifacts a join issues i on i.key = a.issue_key where a.project_key <> i.project_key`,
		},
		{
			name:  "artifact reference keys",
			query: `select count(*) from artifacts where ref_key <> issue_key || '/' || slug`,
		},
		{
			name:  "artifact references",
			query: `select count(*) from refs r left join artifacts a on a.ref_key = r.to_id where r.to_kind = 'artifact' and a.id is null`,
		},
	} {
		t.Run(check.name, func(t *testing.T) {
			var count int
			if err := store.Pool.QueryRow(ctx, check.query).Scan(&count); err != nil {
				t.Fatalf("run check: %v", err)
			}
			if count != 0 {
				t.Errorf("migrated rows violating %s = %d, want 0", check.name, count)
			}
		})
	}

	var appliedAt time.Time
	if err := store.Pool.QueryRow(ctx, `select applied_at from schema_migrations where version = 9`).Scan(&appliedAt); err != nil {
		t.Fatalf("read 0009 migration record: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through 0009 again: %v", err)
	}
	var appliedAgain time.Time
	if err := store.Pool.QueryRow(ctx, `select applied_at from schema_migrations where version = 9`).Scan(&appliedAgain); err != nil {
		t.Fatalf("read 0009 migration record after second boot: %v", err)
	}
	if !appliedAgain.Equal(appliedAt) {
		t.Errorf("0009 applied_at changed from %s to %s on second boot", appliedAt, appliedAgain)
	}
}

func TestMigrate0009FailsBootOnMalformedArtifactReference(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 7)
	commentID := seedGraphAt0007(t, store)
	if _, err := store.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id)
		values ('comment', $1, 'artifact', 'not-a-ref-key')
	`, commentID); err != nil {
		t.Fatalf("seed malformed artifact reference: %v", err)
	}

	err := store.Migrate(ctx)
	if err == nil {
		t.Fatal("migration succeeded with malformed artifact reference")
	}
	if !strings.Contains(err.Error(), "not-a-ref-key") || !strings.Contains(err.Error(), commentID) {
		t.Fatalf("migration error = %q, want malformed reference and source row", err)
	}
	var count int
	if err := store.Pool.QueryRow(ctx, `select count(*) from schema_migrations where version = 9`).Scan(&count); err != nil {
		t.Fatalf("count 0009 migration records: %v", err)
	}
	if count != 0 {
		t.Errorf("0009 migration record count = %d, want 0", count)
	}
	if err := store.Pool.QueryRow(ctx, `
		select count(*)
		from information_schema.columns
		where table_schema = current_schema() and table_name = 'artifacts' and column_name = 'project_key'
	`).Scan(&count); err != nil {
		t.Fatalf("inspect artifact project column: %v", err)
	}
	if count != 0 {
		t.Errorf("artifacts.project_key exists after failed migration")
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
