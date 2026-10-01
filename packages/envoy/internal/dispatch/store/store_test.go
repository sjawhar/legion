package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/pgmigrate"
	"github.com/sjawhar/envoy/internal/pgmigrate/pgmigratetest"
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

// A connection string that asks for a pool size is refused rather than quietly overridden,
// and the question is put to pgx's own parser rather than to the string. Every row here is a
// shape pgx accepts: a keyword may be separated from its "=" by whitespace, and a value may be
// quoted or backslash-escaped and may contain the parameter's own name. A scanner gets the
// first wrong in the dangerous direction - the pool is silently pinned and the refusal never
// fires - and the last two wrong in the worse one, refusing to boot on a valid password.
func TestOpenRefusesAConnectionStringThatSetsThePoolSize(t *testing.T) {
	base := testDatabaseURL(t)
	withPoolSize, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse test database URL: %v", err)
	}
	query := withPoolSize.Query()
	query.Set(poolSizeParam, "4")
	withPoolSize.RawQuery = query.Encode()

	// The rows that must not be refused point at a closed port: the observable is whether the
	// refusal fired, not whether the credentials are real.
	unreachable := "host=127.0.0.1 port=1 "
	for name, tc := range map[string]struct {
		databaseURL string
		refused     bool
	}{
		"url query":            {withPoolSize.String(), true},
		"keyword value":        {"host=127.0.0.1 " + poolSizeParam + "=4", true},
		"keyword value spaced": {"host=127.0.0.1 " + poolSizeParam + " = 4", true},
		"quoted value containing the parameter": {
			unreachable + "password='a " + poolSizeParam + "=1 b'", false,
		},
		"escaped value containing the parameter": {
			unreachable + `password=a\ ` + poolSizeParam + `=1`, false,
		},
	} {
		t.Run(name, func(t *testing.T) {
			store, err := Open(context.Background(), tc.databaseURL)
			if err == nil {
				store.Pool.Close()
			}
			refused := err != nil &&
				strings.Contains(err.Error(), poolSizeParam) &&
				strings.Contains(err.Error(), strconv.Itoa(sharedPoolSize))
			if refused != tc.refused {
				t.Fatalf("refused = %v, want %v; open returned %v", refused, tc.refused, err)
			}
			// A row that must not be refused has to reach the dial. Reading `refused` off
			// the message alone would pass a detector that rejected a valid quoted
			// password as unparseable, which is the same boot outage under another name.
			if !tc.refused && err != nil && strings.Contains(err.Error(), "parse Postgres URL") {
				t.Fatalf("connection string rejected at the parse, want it to reach the dial: %v", err)
			}
		})
	}

	// The refusal is specific: the same connection string without the parameter still opens.
	store, err := Open(context.Background(), base)
	if err != nil {
		t.Fatalf("open without %s: %v", poolSizeParam, err)
	}
	t.Cleanup(store.Pool.Close)
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
		"message_deliveries",
		"refs",
		"events",
		"user_issue_state",
		"user_agent_state",
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
		"messages_in_reply_to",
		"issues_parent_key",
		"artifacts_issue_key",
		"asks_anchor_artifact",
		"comments_anchor_artifact",
		"ask_followers_ask_id_text",
	}
	assertDatabaseObjects(t, ctx, store.Pool, `
		select indexname
		from pg_indexes
		where schemaname = current_schema()
	`, expectedIndexes)
	assertDatabaseObjects(t, ctx, store.Pool, `
		select viewname
		from pg_views
		where schemaname = current_schema()
	`, []string{"graph_edges"})

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
		"messages_in_reply_to_fkey",
		"comments_artifact_id_fkey",
		"comments_one_owner",
		"messages_issue_key_fkey",
		"messages_issue_or_target",
		"message_deliveries_pkey",
		"message_deliveries_message_id_fkey",
		"message_deliveries_reply_id_fkey",
		"message_deliveries_delivery_check",
		"message_deliveries_state_check",
		"refs_pkey",
		"events_pkey",
		"events_issue_key_fkey",
		"events_issue_key_seq_key",
		"events_artifact_id_fkey",
		"events_one_owner",
		"events_artifact_id_seq_key",
		"user_issue_state_pkey",
		"user_issue_state_issue_key_fkey",
		"user_agent_state_pkey",
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

	assertRecordsEveryVersion(t, ctx, store)
}

// Dispatch's set is numbered 1 to N with none missing; pgmigratetest.CheckNumberedOneToN says why.
func TestMigrationSetIsNumberedOneToN(t *testing.T) {
	if err := pgmigratetest.CheckNumberedOneToN(migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// Every file in Dispatch's migrations directory reaches the runner; pgmigratetest.CheckEmbedsEveryFile
// says why that needs a test.
func TestEveryMigrationFileIsEmbedded(t *testing.T) {
	if err := pgmigratetest.CheckEmbedsEveryFile(migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// censusRequiredFrom is the first migration number that must declare a census (LEGION-459): one
// more than the highest migration on main when the census landed, so no migration written before
// the rule is caught by it. Every migration below it that declares one is a worked example. Read
// from origin/main on 2026-10-01, where the set ended at 0055.
const censusRequiredFrom = 56

// Every migration from censusRequiredFrom on declares a census: the count a deployment reads before
// it applies the migration. One that cannot refuse or rewrite any row declares `select 0` and says why.
func TestEveryMigrationFromTheCensusRuleOnDeclaresACensus(t *testing.T) {
	if err := pgmigratetest.CheckCensusDeclaredFrom(migrationFiles, "migrations", censusRequiredFrom); err != nil {
		t.Fatal(err)
	}
}

// The census's textual reading of which tables a migration touches is held to the real set: every
// name it finds is a table the set itself creates.
func TestTouchedTablesOfEveryMigrationAreTablesTheSetCreates(t *testing.T) {
	if err := pgmigratetest.CheckTouchedTablesAreKnown(migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// The census's reading of which tables a migration touches is held to what every real migration
// locks, applied in order to an empty database (pgmigratetest.CheckTouchedTablesAgainstLocks): a
// statement form the patterns miss fails here, which no reading of names alone can catch.
func TestTouchedTablesOfEveryMigrationAreTheTablesItLocks(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	conn, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if err := pgmigratetest.CheckTouchedTablesAgainstLocks(ctx, conn, migrationFiles, "migrations"); err != nil {
		t.Fatal(err)
	}
}

// The lock audit fails both ways: on a table a migration locks that the reading misses (reindex
// is a form the patterns do not know), and on a table the reading names that the migration never
// locks (a quoted identifier holding a statement's text, which the patterns read as that
// statement).
func TestTheLockAuditRefusesAMissedTableAndAMisreadOne(t *testing.T) {
	for name, tc := range map[string]struct{ second, want string }{
		"a form the patterns do not know": {
			second: "reindex table things",
			want:   "migration 0002_second.up.sql locks things above ACCESS SHARE, which pgmigrate.CensusTables does not read from it",
		},
		"a name the patterns misread": {
			second: `select 1 as "delete from things"`,
			want:   "migration 0002_second.up.sql names things, which pgmigrate.CensusTables reads from it, but takes no lock on it",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := auditTwoMigrations(t, "create table things (id integer, kind text)", tc.second)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckTouchedTablesAgainstLocks = %v, want it to say %q", err, tc.want)
			}
		})
	}
}

// The lock audit holds pgmigrate.CensusTables, the one reading Census takes too, to what each
// migration locks: the table behind an index the migration drops, which no statement of it names;
// the tables a foreign key reaches from rows it writes, which an earlier migration of the set
// seeds, since on an empty table a foreign key locks nothing; a function's body, which the
// migration defines and does not run, so the table it writes is not the migration's; and a write a
// DO block makes only when a table holds rows, which on the audit's database it does not, so the
// table it would write is left unlocked there and is no misread.
func TestTheLockAuditAcceptsWhatTheCensusReads(t *testing.T) {
	const parents = `create table parent (id integer primary key);
		create table child (id integer primary key, parent_id integer references parent on delete cascade);
		create table grandchild (id integer primary key, child_id integer references child on delete cascade);
		create table watcher (id integer primary key, parent_id integer references parent);
		create table follower (id integer primary key, parent_id integer references parent on delete set null);
		insert into parent values (1), (2);
		insert into child values (1, 1);
		insert into grandchild values (1, 1);
		insert into follower values (1, 1);`
	for name, tc := range map[string]struct{ first, second string }{
		"an index the migration drops": {
			first:  "create table things (id integer, kind text); create index things_kind on things (kind)",
			second: "drop index things_kind",
		},
		// child and follower take ROW EXCLUSIVE (a cascade and a set null), grandchild the cascade
		// from child's row, and watcher ROW SHARE: the check that no row of it still references 1.
		"the tables a deleted row's foreign keys reach": {
			first:  parents,
			second: "delete from parent where id = 1",
		},
		// Each row child gains or changes is checked against parent: ROW SHARE on parent.
		"the table an inserted or updated row's foreign key checks": {
			first:  parents,
			second: "update child set parent_id = 2; insert into child values (2, 1)",
		},
		// An upsert's do update can change the key, which every table referencing it is checked
		// for (child, watcher, follower: ROW SHARE).
		"the tables an upsert that changes a key reaches": {
			first:  parents,
			second: "insert into parent values (2) on conflict (id) do update set id = 5",
		},
		"a function whose body writes a table": {
			first:  "create table things (id integer, kind text)",
			second: "create function note_thing() returns trigger language plpgsql as $$ begin insert into things (id) values (1); return new; end $$",
		},
		"a write a DO block makes only when a table holds rows": {
			first:  "create table things (id integer, kind text); create table others (id integer)",
			second: "do $$ begin if exists (select 1 from things) then update others set id = 2; end if; end $$",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := auditTwoMigrations(t, tc.first, tc.second); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// auditTwoMigrations runs the lock audit over a set of first and second on a database of its own.
func auditTwoMigrations(t *testing.T, first, second string) error {
	t.Helper()
	ctx := context.Background()
	store := openEmptyTestStore(t)
	conn, err := pgx.ConnectConfig(ctx, store.Pool.Config().ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	set := fstest.MapFS{
		"migrations/0001_first.up.sql":  {Data: []byte(first)},
		"migrations/0002_second.up.sql": {Data: []byte(second)},
	}
	return pgmigratetest.CheckTouchedTablesAgainstLocks(ctx, conn, set, "migrations")
}

func TestMigrateSkipsVersionRecordedOutsideTheRunner(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 7)
	// Production recorded version 8 from Go (the one-time legacy document
	// conversion) before 0008_legacy_documents.up.sql existed.
	if _, err := store.Pool.Exec(ctx, `insert into schema_migrations (version) values (8)`); err != nil {
		t.Fatalf("record version 8 by hand: %v", err)
	}
	var recordedAt time.Time
	if err := store.Pool.QueryRow(ctx, `select applied_at from schema_migrations where version = 8`).Scan(&recordedAt); err != nil {
		t.Fatalf("read hand-recorded version 8: %v", err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate past a hand-recorded version: %v", err)
	}
	assertRecordsEveryVersion(t, ctx, store)
	var appliedAt time.Time
	if err := store.Pool.QueryRow(ctx, `select applied_at from schema_migrations where version = 8`).Scan(&appliedAt); err != nil {
		t.Fatalf("read version 8 after migrate: %v", err)
	}
	if !appliedAt.Equal(recordedAt) {
		t.Errorf("version 8 applied_at changed from %s to %s; the runner re-recorded it", recordedAt, appliedAt)
	}
}

// assertRecordsEveryVersion checks that schema_migrations holds exactly the versions of the
// embedded set.
func assertRecordsEveryVersion(t *testing.T, ctx context.Context, store *Store) {
	t.Helper()
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	rows, err := store.Pool.Query(ctx, "select version from schema_migrations order by version")
	if err != nil {
		t.Fatalf("list recorded migrations: %v", err)
	}
	recorded, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatalf("read recorded migrations: %v", err)
	}
	want := make([]int, len(migrations))
	for i, migration := range migrations {
		want[i] = migration.Version
	}
	if !reflect.DeepEqual(recorded, want) {
		t.Errorf("recorded versions = %v, want every embedded version %v", recorded, want)
	}
}

func assertDatabaseObjects(t *testing.T, ctx context.Context, pool *Pool, query string, expected []string) {
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

func assertDatabaseObjectsAbsent(t *testing.T, ctx context.Context, pool *Pool, query string, unexpected []string) {
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
	migrations, err := pgmigrate.Load(migrationFiles, "migrations")
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	for _, migration := range migrations {
		if migration.Version > maxVersion {
			break
		}
		if err := store.applyMigration(ctx, migration); err != nil {
			t.Fatalf("apply %s: %v", migration.Name, err)
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

func TestMigrate0015BackfillsIssueRanksByProjectCreationOrder(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 13)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core'), ('OPS', 'Operations');
		insert into issues (key, project_key, number, title, created_by, created_at) values
			('CORE-1', 'CORE', 1, 'Later', '{"kind":"user","id":"alice"}', '2026-09-12T12:00:00Z'),
			('CORE-2', 'CORE', 2, 'Earlier', '{"kind":"user","id":"alice"}', '2026-09-12T11:00:00Z'),
			('OPS-1', 'OPS', 1, 'Other project', '{"kind":"user","id":"alice"}', '2026-09-12T10:00:00Z');
	`); err != nil {
		t.Fatalf("seed pre-rank issues: %v", err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through 0015: %v", err)
	}

	rows, err := store.Pool.Query(ctx, `select project_key, key, rank from issues order by project_key, rank`)
	if err != nil {
		t.Fatalf("read ranked issues: %v", err)
	}
	defer rows.Close()
	var ranked []struct {
		Project string
		Key     string
		Rank    string
	}
	for rows.Next() {
		var issue struct {
			Project string
			Key     string
			Rank    string
		}
		if err := rows.Scan(&issue.Project, &issue.Key, &issue.Rank); err != nil {
			t.Fatalf("scan ranked issue: %v", err)
		}
		ranked = append(ranked, issue)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ranked issues: %v", err)
	}
	want := []struct {
		Project string
		Key     string
		Rank    string
	}{
		{Project: "CORE", Key: "CORE-2", Rank: "00000000000000000001"},
		{Project: "CORE", Key: "CORE-1", Rank: "00000000000000000002"},
		{Project: "OPS", Key: "OPS-1", Rank: "00000000000000000001"},
	}
	if !reflect.DeepEqual(ranked, want) {
		t.Fatalf("rank backfill = %#v, want %#v", ranked, want)
	}
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

func TestMigrate0009DropsMalformedArtifactReferences(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 7)
	commentID := seedGraphAt0007(t, store)
	const malformed = "CORE-2/diagram-png``"
	if _, err := store.Pool.Exec(ctx, `
		insert into refs (from_kind, from_id, to_kind, to_id)
		values ('comment', $1, 'artifact', $2)
	`, commentID, malformed); err != nil {
		t.Fatalf("seed malformed artifact reference: %v", err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate malformed artifact reference: %v", err)
	}
	var count int
	if err := store.Pool.QueryRow(ctx, `
		select count(*) from refs
		where from_kind = 'comment' and from_id = $1 and to_kind = 'artifact' and to_id = $2
	`, commentID, malformed).Scan(&count); err != nil {
		t.Fatalf("count malformed artifact reference: %v", err)
	}
	if count != 0 {
		t.Errorf("malformed artifact reference rows = %d, want 0", count)
	}
	if err := store.Pool.QueryRow(ctx, `select count(*) from schema_migrations where version = 9`).Scan(&count); err != nil {
		t.Fatalf("count 0009 migration records: %v", err)
	}
	if count != 1 {
		t.Errorf("0009 migration record count = %d, want 1", count)
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
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Legacy', '{"kind":"session","id":"s"}', 'U');
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

// Rows and ask.* event payloads written before options were normalised on input carry JSON
// null where the wire shape is an array. 0031 rewrites them to [] and the check constraint
// keeps any later writer from putting a null back.
func TestMigrate0031RewritesNullAskOptionsToEmptyArrays(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 30)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Null options', '{"kind":"session","id":"s"}', 'U');
		insert into asks (id, issue_key, author, question, options)
			values ('5a660655-04ad-4ce0-8a9b-93dd03c412b7', 'CORE-1', '{"kind":"session","id":"s"}', 'Ship?', 'null'::jsonb);
		insert into events (issue_key, seq, type, actor, payload, notify) values
			('CORE-1', 1, 'ask.opened', '{"kind":"session","id":"s"}',
			 '{"id":"5a660655-04ad-4ce0-8a9b-93dd03c412b7","question":"Ship?","options":null}', true),
			('CORE-1', 2, 'ask.edited', '{"kind":"session","id":"s"}',
			 '{"id":"5a660655-04ad-4ce0-8a9b-93dd03c412b7","question":"Ship it?","options":[],"previous":{"question":"Ship?","options":null}}', true);
	`); err != nil {
		t.Fatalf("seed null options: %v", err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through 0031: %v", err)
	}
	var rowOptions, openedOptions, previousOptions string
	if err := store.Pool.QueryRow(ctx, `
		select a.options::text,
		       (select payload->'options' from events where type = 'ask.opened')::text,
		       (select payload->'previous'->'options' from events where type = 'ask.edited')::text
		from asks a
	`).Scan(&rowOptions, &openedOptions, &previousOptions); err != nil {
		t.Fatalf("read migrated options: %v", err)
	}
	if rowOptions != "[]" || openedOptions != "[]" || previousOptions != "[]" {
		t.Fatalf("options after 0031: row=%s opened=%s previous=%s, want [] each", rowOptions, openedOptions, previousOptions)
	}
	_, err := store.Pool.Exec(ctx, `update asks set options = 'null'::jsonb where id = '5a660655-04ad-4ce0-8a9b-93dd03c412b7'`)
	if err == nil || !strings.Contains(err.Error(), "asks_options_array") {
		t.Fatalf("null options update error = %v, want asks_options_array violation", err)
	}
}

// Asks written as the removed `action` kind stored their Done / Can't options and their
// answers on the row like any question. 0035 relabels the rows and the ask.* event payloads
// that carried the old kind, and the check constraint refuses any new action row.
func TestMigrate0035FoldsActionAsksIntoQuestions(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	migrateThrough(t, store, 34)
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Deploy', '{"kind":"session","id":"s"}', 'U');
		insert into asks (id, issue_key, author, question, options, kind, state, answer)
			values ('5a660655-04ad-4ce0-8a9b-93dd03c412b7', 'CORE-1', '{"kind":"session","id":"s"}', 'Confirm the deploy.',
			        '[{"label":"Done"},{"label":"Can''t"}]'::jsonb, 'action', 'answered',
			        '{"user":"alice","selected":["Can''t"],"text":"No access.","at":"2026-09-01T00:00:00Z"}'::jsonb);
		insert into events (issue_key, seq, type, actor, payload, notify) values
			('CORE-1', 1, 'ask.opened', '{"kind":"session","id":"s"}',
			 '{"id":"5a660655-04ad-4ce0-8a9b-93dd03c412b7","kind":"action","question":"Confirm the deploy.","options":[{"label":"Done"},{"label":"Can''t"}]}', true),
			('CORE-1', 2, 'ask.answered', '{"kind":"user","id":"alice"}',
			 '{"id":"5a660655-04ad-4ce0-8a9b-93dd03c412b7","kind":"action","question":"Confirm the deploy.","answer":{"selected":["Can''t"],"text":"No access."}}', true),
			('CORE-1', 3, 'ask.follower_added', '{"kind":"session","id":"s"}',
			 '{"ask_id":"5a660655-04ad-4ce0-8a9b-93dd03c412b7","session_id":"s","by":{"kind":"session","id":"s"}}', false);
	`); err != nil {
		t.Fatalf("seed action ask: %v", err)
	}

	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through 0035: %v", err)
	}
	var kind, options, answer string
	var eventKinds []string
	if err := store.Pool.QueryRow(ctx, `
		select a.kind, a.options::text, a.answer::text,
		       (select array_agg(coalesce(payload->>'kind', '-') order by seq) from events where issue_key = 'CORE-1')
		from asks a
	`).Scan(&kind, &options, &answer, &eventKinds); err != nil {
		t.Fatalf("read migrated ask: %v", err)
	}
	if kind != "question" || options != `[{"label": "Done"}, {"label": "Can't"}]` {
		t.Fatalf("ask after 0035: kind=%s options=%s, want a question keeping its Done / Can't options", kind, options)
	}
	if !strings.Contains(answer, `"selected": ["Can't"]`) || !strings.Contains(answer, `"text": "No access."`) {
		t.Fatalf("answer after 0035 = %s, want the recorded Can't answer intact", answer)
	}
	if !reflect.DeepEqual(eventKinds, []string{"question", "question", "-"}) {
		t.Fatalf("event payload kinds after 0035 = %v, want the ask events relabelled and other payloads untouched", eventKinds)
	}
	_, err := store.Pool.Exec(ctx, `update asks set kind = 'action' where id = '5a660655-04ad-4ce0-8a9b-93dd03c412b7'`)
	if err == nil || !strings.Contains(err.Error(), "asks_kind_check") {
		t.Fatalf("action kind update error = %v, want asks_kind_check violation", err)
	}
}

// 0053 pairs an ask's kind with its approval: an approval ask carries an approval whose known keys
// hold the types model.AskApproval decodes and name a document version, and no other kind carries
// one, not even the JSON null. A hand-written row that breaks the pairing either way is refused at
// insert, and an approval the route writes is stored at every version a document can have. The
// rows named in residual are shapes 0053 admits and ScanAsk still fails on. They are
// skipped, which go test reports only under -v, and dispatch://LEGION-429 deletes their entries
// when it adds the check that refuses them.
func TestMigrate0053RefusesAnApprovalOnAnyAskButAnApprovalAsk(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Spec', '{"kind":"session","id":"s"}', 'U');
	`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	const approval = `{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":1}`
	residual := map[string]bool{
		"an approval ask repeating version under another case as a fraction":     true,
		"an approval ask repeating name under another case as a number":          true,
		"an approval ask repeating version with a long s as a fraction":          true,
		"an approval ask carrying a value nested past the decoder's depth limit": true,
	}
	for _, row := range []struct {
		name     string
		kind     string
		approval *string
		refused  bool
	}{
		{"an approval ask naming no document", "approval", nil, true},
		// The JSON value null is what encoding a nil *model.AskApproval writes (encodeJSON, as the
		// approval-request route writes the column), and it names no document either: ScanAsk
		// reads it as an approval with no artifact, and answering the ask fails on it.
		{"an approval ask whose approval is the JSON null", "approval", new("null"), true},
		// What ScanAsk reads the JSON null as. It separates this check from one that asks only for
		// an object.
		{"an approval ask whose document id is empty, as the JSON null reads back", "approval", new(`{"artifact_id":"","name":"","version":0}`), true},
		// An id that is not a uuid fails the cast answering the ask makes on it (22P02), and one in
		// capitals is never found by docs.ApprovalAskAt, which compares the id as text with the
		// lowercase text Postgres writes, so a new version would never retract the ask.
		{"an approval ask whose document id is empty at a real version", "approval", new(`{"artifact_id":"","name":"spec.md","version":1}`), true},
		{"an approval ask whose document id is in capitals", "approval", new(`{"artifact_id":"7C1E8A52-3F4B-4D6E-9A0B-1C2D3E4F5A6B","name":"spec.md","version":1}`), true},
		// A character before or after the uuid, or a letter past f in it, also fails that cast, and
		// docs.ApprovalAskAt never finds the ask. Each row is refused only while its anchor, or the
		// pattern's hex class, is as written.
		{"an approval ask whose document id has a digit after the uuid", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b0","name":"spec.md","version":1}`), true},
		{"an approval ask whose document id has a digit before the uuid", "approval", new(`{"artifact_id":"07c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":1}`), true},
		{"an approval ask whose document id has a letter that is not hex", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6g","name":"spec.md","version":1}`), true},
		// A missing key reads as its zero value, which no writer writes. These are the rows a check
		// with its coalesce around one conjunct alone would store, since a CHECK that evaluates to
		// null passes.
		{"an approval ask naming no version", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md"}`), true},
		{"an approval ask naming no document name", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","version":1}`), true},
		// ScanAsk decodes the approval into model.AskApproval, whose version is an int and whose name
		// is a string, so a number that is no int, or a name that is no string, fails every read of
		// the ask: the ask itself, its issue, the inbox, answering it, and each new version of its
		// document, which reads every open approval ask on the document to retract it.
		{"an approval ask whose version is not a whole number", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":1.5}`), true},
		{"an approval ask whose version is written with a fraction", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":1.0}`), true},
		{"an approval ask whose version is past what an int holds", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":9223372036854775808}`), true},
		{"an approval ask whose name is not a string", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":5,"version":1}`), true},
		// jsonb stores 1e30 as a 31-digit integer, which a check on the digits alone has to bound.
		{"an approval ask whose version is 1e30", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":1e30}`), true},
		// The first version past ten digits, which no integer version reaches; it is refused only
		// while the digit pattern's bound is ten.
		{"an approval ask whose version has eleven digits", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":10000000000}`), true},
		// The digit pattern matches the text of the string "1" too; only the number test refuses it,
		// and a string does not decode into the int.
		{"an approval ask whose version is a string", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":"1"}`), true},
		// Version 0 decodes, but no version has that number and the approval-request route never
		// writes it.
		{"an approval ask at version 0", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":0}`), true},
		// encoding/json matches an object's keys to model.AskApproval's fields without regard to
		// case, Unicode folding included, so a key the check does not name can still land on a
		// field and fail its decode; and it scans every value, so one nested past its depth limit
		// fails the decode whatever its key. Either fails every read of the ask as the rows above do.
		{"an approval ask repeating version under another case as a fraction", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":1,"Version":1.5}`), true},
		{"an approval ask repeating name under another case as a number", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","Name":5,"version":1}`), true},
		{"an approval ask repeating version with a long s as a fraction", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":1,"verſion":1.5}`), true},
		{"an approval ask carrying a value nested past the decoder's depth limit", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":1,"x":` + strings.Repeat("[", 10001) + strings.Repeat("]", 10001) + `}`), true},
		{"a question naming a document", "question", new(approval), true},
		// A check keyed on the approval being an object, (kind = 'approval') = (jsonb_typeof(approval)
		// = 'object'), would store this, and ScanAsk would then put an approval on the question.
		{"a question carrying the JSON null", "question", new("null"), true},
		{"an approval ask naming its document", "approval", new(approval), false},
		// The route writes every version a document has, and a check that admitted too few would
		// refuse its own insert. artifact_versions.number is an integer: 10 is the first version
		// with two digits, and 2147483647 the highest it holds.
		{"an approval ask at version 10", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":10}`), false},
		{"an approval ask at the highest version a document can have", "approval", new(`{"artifact_id":"7c1e8a52-3f4b-4d6e-9a0b-1c2d3e4f5a6b","name":"spec.md","version":2147483647}`), false},
		{"a question naming none", "question", nil, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if residual[row.name] {
				t.Skip("0053 admits this shape and ScanAsk fails on it; dispatch://LEGION-429 refuses it and deletes this entry")
			}
			_, err := store.Pool.Exec(ctx, `
				insert into asks (issue_key, author, question, options, kind, approval)
				values ('CORE-1', '{"kind":"session","id":"s"}', 'Approve spec.md (version 1)?',
					'[{"label":"Approve"},{"label":"Request changes"}]', $1, $2::jsonb)
			`, row.kind, row.approval)
			if !row.refused {
				if err != nil {
					t.Fatalf("insert: %v", err)
				}
				return
			}
			var refusal *pgconn.PgError
			if !errors.As(err, &refusal) || refusal.Code != "23514" || refusal.ConstraintName != "asks_approval_kind_check" {
				t.Fatalf("insert error = %v, want SQLSTATE 23514 naming asks_approval_kind_check", err)
			}
		})
	}
}

// 0043 moves the callback replies that carried a bare reply_to into the ask thread they
// belong to and gives them the turn 0028 gave every other ask reply. 0044 re-runs the same
// statements, byte for byte, from a later release, for the rows a pre-0043 task wrote while
// 0043's image was still rolling out. One contract, so one table: each case migrates to the
// version before its own file, seeds the threads that file has to repair, and applies that
// file - which is what puts the migration under test into every failure message. Seeding
// after `migrateThrough` is what gives 0044's walk anything to do: once 0043 has run, every
// comment inside an ask thread already carries its ask_id, so the walk selects an empty set.
var askReplyBackfills = []struct {
	name    string
	through int
	file    string
}{
	{"0043", 42, "0043_comment_reply_ask_id.up.sql"},
	{"0044", 43, "0044_comment_reply_ask_id_redo.up.sql"},
}

// The rows both backfills are judged on. Every id is its own literal, so a uuid out of a
// failure message or a psql dump names exactly one row.
const (
	backfillAskID             = "11111111-1111-4111-8111-111111111111"
	backfillReopenedAskID     = "22222222-2222-4222-8222-222222222222"
	backfillResolvedAskID     = "44444444-4444-4444-8444-444444444444"
	backfillMentionID         = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	backfillCallbackID        = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	backfillGrandchildID      = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	backfillWhileClosedID     = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	backfillSettledID         = "55555555-5555-4555-8555-555555555555"
	backfillClosedMentionID   = "0a0a0a0a-0a0a-4a0a-8a0a-0a0a0a0a0a0a"
	backfillClosedReplyID     = "0b0b0b0b-0b0b-4b0b-8b0b-0b0b0b0b0b0b"
	backfillAnsweredAskID     = "66666666-6666-4666-8666-666666666666"
	backfillAnsweredMentionID = "0c0c0c0c-0c0c-4c0c-8c0c-0c0c0c0c0c0c"
	backfillAnsweredReplyID   = "0d0d0d0d-0d0d-4d0d-8d0d-0d0d0d0d0d0d"
	backfillDocArtifactID     = "77777777-7777-4777-8777-777777777777"
	backfillDocAskID          = "88888888-8888-4888-8888-888888888888"
	backfillDocMentionID      = "0e0e0e0e-0e0e-4e0e-8e0e-0e0e0e0e0e0e"
	backfillDocReplyID        = "0f0f0f0f-0f0f-4f0f-8f0f-0f0f0f0f0f0f"
	backfillPlainRootID       = "99999999-9999-4999-8999-999999999999"
	backfillPlainReplyID      = "01010101-0101-4101-8101-010101010101"
)

// seedAskThreads writes the ask threads a server that has not yet stopped writing bare
// reply_to leaves behind, plus the rows that pin what the backfill may not touch. Both of the
// statement's real mechanics are here: a reply two hops below the ask-bearing row, which only
// the recursion reaches, and a thread under a resolved ask, which moves but takes no turn.
// The other three states the walk has to get right are here too, because each is a mutation
// the rest of the seed cannot kill: an ask that is answered rather than open or resolved, an
// ask owned by a document rather than an issue, and a thread that reaches no ask at all.
func seedAskThreads(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'Deploy', '{"kind":"user","id":"alice"}', 'U');
		insert into artifacts (id, project_key, slug, name, kind, is_primary, created_by)
			values ('`+backfillDocArtifactID+`', 'CORE', 'spec', 'spec.md', 'doc', false,
			 '{"kind":"user","id":"alice"}');
		insert into asks (id, issue_key, artifact_id, author, question, state, resolution, answer) values
			('`+backfillAskID+`', 'CORE-1', null, '{"kind":"session","id":"s1"}', 'Which approach?',
			 'open', null, null),
			('`+backfillReopenedAskID+`', 'CORE-1', null, '{"kind":"session","id":"s1"}', 'Ship it?',
			 'open', null, null),
			('`+backfillResolvedAskID+`', 'CORE-1', null, '{"kind":"session","id":"s1"}', 'Roll back?',
			 'resolved', '{"by":{"kind":"user","id":"alice"},"at":"2026-09-01T00:00:00Z"}'::jsonb, null),
			('`+backfillAnsweredAskID+`', 'CORE-1', null, '{"kind":"session","id":"s1"}', 'Which region?',
			 'answered', null, '{"user":"alice","selected":["eu"]}'::jsonb),
			('`+backfillDocAskID+`', null, '`+backfillDocArtifactID+`', '{"kind":"session","id":"s1"}',
			 'Reword this?', 'open', null, null);
		insert into comments (id, issue_key, author, body, ask_id, reply_to, turn) values
			('`+backfillMentionID+`', 'CORE-1', '{"kind":"user","id":"alice"}', 'Say more, @session:s1.',
			 '`+backfillAskID+`', null, 'agent'),
			('`+backfillCallbackID+`', 'CORE-1', '{"kind":"session","id":"s1"}', 'The second approach.',
			 null, '`+backfillMentionID+`', null),
			('`+backfillGrandchildID+`', 'CORE-1', '{"kind":"user","id":"alice"}', 'And the deadline?',
			 null, '`+backfillCallbackID+`', null),
			('`+backfillWhileClosedID+`', 'CORE-1', '{"kind":"user","id":"alice"}', 'Noted while it was resolved.',
			 '`+backfillReopenedAskID+`', null, null),
			('`+backfillSettledID+`', 'CORE-1', '{"kind":"session","id":"s1"}', 'Already in its thread.',
			 '`+backfillAskID+`', null, 'human'),
			('`+backfillClosedMentionID+`', 'CORE-1', '{"kind":"user","id":"alice"}', 'Anyone, @session:s1?',
			 '`+backfillResolvedAskID+`', null, null),
			('`+backfillClosedReplyID+`', 'CORE-1', '{"kind":"session","id":"s1"}', 'Rolled back already.',
			 null, '`+backfillClosedMentionID+`', null),
			('`+backfillAnsweredMentionID+`', 'CORE-1', '{"kind":"user","id":"alice"}', 'Which, @session:s1?',
			 '`+backfillAnsweredAskID+`', null, null),
			('`+backfillAnsweredReplyID+`', 'CORE-1', '{"kind":"session","id":"s1"}', 'Answered already.',
			 null, '`+backfillAnsweredMentionID+`', null),
			('`+backfillPlainRootID+`', 'CORE-1', '{"kind":"user","id":"alice"}', 'Unrelated thread.',
			 null, null, null),
			('`+backfillPlainReplyID+`', 'CORE-1', '{"kind":"session","id":"s1"}', 'Still unrelated.',
			 null, '`+backfillPlainRootID+`', null);
		insert into comments (id, artifact_id, author, body, ask_id, reply_to, turn) values
			('`+backfillDocMentionID+`', '`+backfillDocArtifactID+`', '{"kind":"user","id":"alice"}',
			 'Here, @session:s1?', '`+backfillDocAskID+`', null, null),
			('`+backfillDocReplyID+`', '`+backfillDocArtifactID+`', '{"kind":"session","id":"s1"}',
			 'Reworded.', null, '`+backfillDocMentionID+`', null);
	`); err != nil {
		t.Fatalf("seed ask threads: %v", err)
	}
}

// assertAskThreadsRepaired names the migration that ran, so a failure points at the file that
// was edited rather than at whichever migration the runner happened to start from.
func assertAskThreadsRepaired(t *testing.T, store *Store, migration string) {
	t.Helper()
	ctx := context.Background()
	for _, want := range []struct {
		name, id, askID string
		turn            *string
	}{
		{"callback reply", backfillCallbackID, backfillAskID, new("human")},
		// Two hops from the ask-bearing row, so only the recursion reaches it, and a human's
		// reply takes the other side of 0028's turn expression.
		{"reply under the callback reply", backfillGrandchildID, backfillAskID, new("agent")},
		// A thread under a resolved ask moves too - it belongs to that ask either way - but a
		// closed ask has no turn to hold.
		{"reply under the resolved ask", backfillClosedReplyID, backfillResolvedAskID, nil},
		// The row that pins the backfill's scope: already in its ask's thread, and the only
		// seeded comment whose recorded turn disagrees with what the turn expression computes.
		// A backfill reaching past the rows it moves would hand it one.
		{"reply posted while its ask was resolved", backfillWhileClosedID, backfillReopenedAskID, nil},
		// Already in its thread, so the backfill leaves the turn its own write recorded.
		{"reply that never needed moving", backfillSettledID, backfillAskID, new("human")},
		// An answered ask is neither open nor resolved: the thread moves, and the turn gate
		// still refuses it, because a turn is set on a reply to an open ask.
		{"reply under the answered ask", backfillAnsweredReplyID, backfillAnsweredAskID, nil},
		// An ask a document owns, so the walk is not implicitly scoped to issue comments.
		{"reply under the document ask", backfillDocReplyID, backfillDocAskID, new("human")},
	} {
		var gotAsk, gotReplyTo, gotTurn *string
		if err := store.Pool.QueryRow(ctx, `
			select ask_id::text, reply_to::text, turn from comments where id = $1
		`, want.id).Scan(&gotAsk, &gotReplyTo, &gotTurn); err != nil {
			t.Fatalf("read %s after %s: %v", want.name, migration, err)
		}
		if gotAsk == nil || *gotAsk != want.askID || gotReplyTo != nil {
			t.Fatalf(
				"%s after %s: ask_id=%s reply_to=%s, want %s and no reply_to",
				want.name, migration, nullableText(gotAsk), nullableText(gotReplyTo), want.askID,
			)
		}
		if !reflect.DeepEqual(gotTurn, want.turn) {
			t.Fatalf(
				"%s turn after %s = %s, want %s",
				want.name, migration, nullableText(gotTurn), nullableText(want.turn),
			)
		}
	}
	// A thread that reaches no ask keeps its reply_to: the backfill moves comments inside an
	// ask thread and touches nothing else in the table.
	var plainAsk, plainParent *string
	if err := store.Pool.QueryRow(ctx, `
		select ask_id::text, reply_to::text from comments where id = $1
	`, backfillPlainReplyID).Scan(&plainAsk, &plainParent); err != nil {
		t.Fatalf("read the reply outside every ask thread after %s: %v", migration, err)
	}
	if plainAsk != nil || plainParent == nil || *plainParent != backfillPlainRootID {
		t.Fatalf(
			"reply outside every ask thread after %s: ask_id=%s reply_to=%s, want none and %s",
			migration, nullableText(plainAsk), nullableText(plainParent), backfillPlainRootID,
		)
	}
}

// Each backfill moves the replies that carried a bare reply_to and only those, and running it
// a second time moves nothing: the walk selects only comments whose ask_id is null and whose
// ancestry reaches an ask, so the second pass selects an empty set and the turn update, which
// reads that same set, writes no row.
func TestAskReplyBackfillsMoveOnlyTheRepliesTheyOwn(t *testing.T) {
	for _, backfill := range askReplyBackfills {
		t.Run(backfill.name, func(t *testing.T) {
			store := openEmptyTestStore(t)
			migrateThrough(t, store, backfill.through)
			seedAskThreads(t, store)
			applyMigrationFile(t, store, backfill.file)
			assertAskThreadsRepaired(t, store, backfill.name)

			before := askThreadShape(t, store)
			applyMigrationFile(t, store, backfill.file)
			if after := askThreadShape(t, store); !reflect.DeepEqual(before, after) {
				t.Fatalf("second run of %s moved rows: before=%v after=%v", backfill.name, before, after)
			}
		})
	}
}

// nullableText renders a nullable comment column for a failure message, since a *string
// prints as an address.
func nullableText(value *string) string {
	if value == nil {
		return "none"
	}
	return strconv.Quote(*value)
}

// commentThreadRow is a comment's place in its thread, rendered for comparison and for a
// failure message.
type commentThreadRow struct{ askID, replyTo, turn string }

// askThreadShape is every comment's place in its thread, for comparing a table before and
// after a migration that must move nothing.
func askThreadShape(t *testing.T, store *Store) map[string]commentThreadRow {
	t.Helper()
	rows, err := store.Pool.Query(context.Background(), `
		select id::text, ask_id::text, reply_to::text, turn from comments
	`)
	if err != nil {
		t.Fatalf("read comment thread shape: %v", err)
	}
	defer rows.Close()
	shape := map[string]commentThreadRow{}
	for rows.Next() {
		var id string
		var askID, replyTo, turn *string
		if err := rows.Scan(&id, &askID, &replyTo, &turn); err != nil {
			t.Fatalf("scan comment thread shape: %v", err)
		}
		shape[id] = commentThreadRow{nullableText(askID), nullableText(replyTo), nullableText(turn)}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read comment thread shape: %v", err)
	}
	return shape
}

// applyMigrationFile runs one migration's statements the way the runner does: one transaction,
// so an `on commit drop` temporary table lives exactly that long.
func applyMigrationFile(t *testing.T, store *Store, name string) {
	t.Helper()
	statements, err := migrationFiles.ReadFile("migrations/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	ctx := context.Background()
	tx, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin %s: %v", name, err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, string(statements)); err != nil {
		t.Fatalf("apply %s: %v", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit %s: %v", name, err)
	}
}

// comments.reply_to carries no acyclicity constraint, so a comment can point at itself. The
// walk down the thread has to treat that as a visited row rather than spin: it runs inside the
// transaction holding the migration advisory lock, so a spin means no Dispatch process boots.
// The cyclic row seeds the walk (it carries the ask) and an ordinary reply hangs below it, so
// the walk both loops and has real work to do.
func TestAskReplyBackfillsTerminateOnACyclicReplyChain(t *testing.T) {
	for _, backfill := range askReplyBackfills {
		t.Run(backfill.name, func(t *testing.T) {
			ctx := context.Background()
			store := openEmptyTestStore(t)
			migrateThrough(t, store, backfill.through)
			const (
				askID    = "33333333-3333-4333-8333-333333333333"
				cyclicID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
				replyID  = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
			)
			if _, err := store.Pool.Exec(ctx, `
				insert into projects (key, name) values ('CORE', 'Core');
				insert into issues (key, project_key, number, title, created_by, rank)
					values ('CORE-1', 'CORE', 1, 'Deploy', '{"kind":"user","id":"alice"}', 'U');
				insert into asks (id, issue_key, author, question, state)
					values ('`+askID+`', 'CORE-1', '{"kind":"session","id":"s1"}', 'Which approach?', 'open');
				insert into comments (id, issue_key, author, body, ask_id, reply_to) values
					('`+cyclicID+`', 'CORE-1', '{"kind":"user","id":"alice"}', 'Say more, @session:s1.',
					 '`+askID+`', '`+cyclicID+`'),
					('`+replyID+`', 'CORE-1', '{"kind":"session","id":"s1"}', 'The second approach.',
					 null, '`+cyclicID+`');
			`); err != nil {
				t.Fatalf("seed a cyclic ask thread: %v", err)
			}
			// A bound on the statement, so a walk that does spin fails the test in seconds
			// instead of running until the test binary is killed. Database-level, because the
			// migration takes its connection from the pool: only a session opened after this
			// carries it.
			if _, err := store.Pool.Exec(ctx,
				"alter database "+store.Pool.Config().ConnConfig.Database+" set statement_timeout = '20s'",
			); err != nil {
				t.Fatalf("bound the migration statement: %v", err)
			}
			bounded, err := Open(ctx, store.Pool.Config().ConnString())
			if err != nil {
				t.Fatalf("reopen the bounded database: %v", err)
			}
			t.Cleanup(bounded.Pool.Close)

			applyMigrationFile(t, bounded, backfill.file)
			var movedAsk, movedReplyTo, movedTurn *string
			if err := bounded.Pool.QueryRow(ctx, `
				select ask_id::text, reply_to::text, turn from comments where id = $1
			`, replyID).Scan(&movedAsk, &movedReplyTo, &movedTurn); err != nil {
				t.Fatalf("read the reply below the cycle: %v", err)
			}
			if movedAsk == nil || *movedAsk != askID || movedReplyTo != nil || movedTurn == nil || *movedTurn != "human" {
				t.Fatalf(
					"reply below the cycle after %s: ask_id=%s reply_to=%s turn=%s, want the ask, no reply_to and human",
					backfill.name, nullableText(movedAsk), nullableText(movedReplyTo), nullableText(movedTurn),
				)
			}
		})
	}
}
