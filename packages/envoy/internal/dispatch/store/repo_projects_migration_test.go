package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/pgmigrate"
)

// repoProjectsDefaultVersion is the migration that adds a project's default repository and an
// issue's own repository.
const repoProjectsDefaultVersion = 113

// The migration adds the project's default repository and an issue's own repository. At most one
// mapping of a project is the default; an issue's repository must be a mapping of its own project;
// a mapping an issue names cannot be deleted or moved to another project.
func TestMigrationAddsADefaultMappingAndTheIssueRepository(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, repoProjectsDefaultVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", repoProjectsDefaultVersion-1, err)
	}
	if _, err := store.Pool.Exec(ctx, `
		insert into projects (key, name) values ('CORE', 'Core'), ('OTHER', 'Other');
		insert into repo_projects (repo, project, created_by) values
			('acme/widgets', 'CORE', '{"kind":"user","id":"alice"}'),
			('acme/gadgets', 'CORE', '{"kind":"user","id":"alice"}');
		insert into issues (key, project_key, number, title, created_by, rank)
			values ('CORE-1', 'CORE', 1, 'One', '{"kind":"user","id":"alice"}', 'U');
	`); err != nil {
		t.Fatalf("seed before the migration: %v", err)
	}
	if err := store.migrate(ctx, migrationsThrough(t, repoProjectsDefaultVersion)); err != nil {
		t.Fatalf("migrate to %d: %v", repoProjectsDefaultVersion, err)
	}
	var defaults int
	if err := store.Pool.QueryRow(ctx, `select count(*) from repo_projects where is_default`).Scan(&defaults); err != nil || defaults != 0 {
		t.Fatalf("defaults after the migration = %d (%v), want none: no mapping is the default until a person sets one", defaults, err)
	}
	if _, err := store.Pool.Exec(ctx, `update repo_projects set is_default = true`); !isSQLState(err, "23505") {
		t.Fatalf("two defaults in CORE: %v, want unique_violation", err)
	}
	if _, err := store.Pool.Exec(ctx, `update issues set repo = 'acme/unmapped' where key = 'CORE-1'`); !isSQLState(err, "23503") {
		t.Fatalf("an unmapped repository on CORE-1: %v, want foreign_key_violation", err)
	}
	if _, err := store.Pool.Exec(ctx, `update issues set repo = 'acme/widgets' where key = 'CORE-1'`); err != nil {
		t.Fatalf("a mapped repository on CORE-1: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `update repo_projects set project = 'OTHER' where repo = 'acme/widgets'`); !isSQLState(err, "23503") {
		t.Fatalf("moving a mapping CORE-1 names: %v, want foreign_key_violation", err)
	}
	if _, err := store.Pool.Exec(ctx, `delete from repo_projects where repo = 'acme/widgets'`); !isSQLState(err, "23503") {
		t.Fatalf("deleting a mapping CORE-1 names: %v, want foreign_key_violation", err)
	}
	if _, err := store.Pool.Exec(ctx, `delete from repo_projects where repo = 'acme/gadgets'`); err != nil {
		t.Fatalf("deleting a mapping no issue names: %v", err)
	}
}

// seedChain inserts project's issues 1..length, each the parent of the next: issue n sits n-1
// links below issue 1. A release before the migration let a create build any depth.
func seedChain(t *testing.T, store *Store, project string, length int) {
	t.Helper()
	if _, err := store.Pool.Exec(context.Background(), `
		insert into projects (key, name) values ($1, $1);
	`, project); err != nil {
		t.Fatalf("create project %s: %v", project, err)
	}
	if _, err := store.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, parent_key, created_by, rank)
		select $1 || '-' || n, $1, n, 'Link ' || n, case when n = 1 then null else $1 || '-' || (n - 1) end,
		       '{"kind":"user","id":"alice"}', lpad(n::text, 8, '0')
		from generate_series(1, $2::int) n
		order by n
	`, project, length); err != nil {
		t.Fatalf("insert %s's %d-issue chain: %v", project, length, err)
	}
}

// censusCount takes the pre-deploy census of the migration and answers its count.
func censusCount(t *testing.T, store *Store) int64 {
	t.Helper()
	report, err := census(context.Background(), store.Pool.Config().ConnString(), migrationsThrough(t, repoProjectsDefaultVersion), pgmigrate.CensusOptions{})
	if err != nil {
		t.Fatalf("census: %v", err)
	}
	if len(report.Pending) != 1 || report.Pending[0].Count == nil {
		t.Fatalf("census did not answer one count:\n%s", reportText(report))
	}
	return *report.Pending[0].Count
}

// An issue more than parentDepthCap (32) links below its root, which this release neither creates
// nor reads, refuses the migration: the census counts it, so the pre-deploy gate refuses, and the
// migration itself names the first 20 and the repair (detach a chain on the release still running)
// and applies nothing.
func TestMigrationRefusesAnIssueMoreThan32LinksBelowItsRoot(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, repoProjectsDefaultVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", repoProjectsDefaultVersion-1, err)
	}
	seedChain(t, store, "DEEP", 34) // DEEP-34 is 33 links below DEEP-1
	if got := censusCount(t, store); got != 1 {
		t.Fatalf("census = %d, want 1: DEEP-34 alone sits more than 32 links below its root", got)
	}
	err := store.migrate(ctx, migrationsThrough(t, repoProjectsDefaultVersion))
	if err == nil {
		t.Fatal("the migration applied over an issue 33 links below its root")
	}
	t.Logf("refusal: %v", err)
	for _, want := range []string{"1 issue", "more than 32 links", "DEEP-34", `{"parent": null}`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	if got := schemaVersion(t, store); got != repoProjectsDefaultVersion-1 {
		t.Fatalf("schema version after the refusal = %d, want %d", got, repoProjectsDefaultVersion-1)
	}
	// 25 issues past the cap: the refusal names the first 20 by project and number, and counts the rest.
	seedChain(t, store, "WIDE", 57) // WIDE-34 through WIDE-57 sit more than 32 links down
	if got := censusCount(t, store); got != 25 {
		t.Fatalf("census = %d, want 25", got)
	}
	err = store.migrate(ctx, migrationsThrough(t, repoProjectsDefaultVersion))
	if err == nil || !strings.Contains(err.Error(), "25 issue(s)") || !strings.Contains(err.Error(), "DEEP-34, WIDE-34, WIDE-35") ||
		!strings.Contains(err.Error(), "WIDE-52 and 5 more") || strings.Contains(err.Error(), "WIDE-53") {
		t.Fatalf("refusal over 25 issues = %v, want DEEP-34 and WIDE-34 through WIDE-52 named and 5 more counted", err)
	}
}

// A chain whose deepest issue sits exactly 32 links below its root is within the cap: the census
// answers 0 and the migration applies.
func TestMigrationAppliesOverAChain32LinksDeep(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, repoProjectsDefaultVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", repoProjectsDefaultVersion-1, err)
	}
	seedChain(t, store, "EDGE", 33) // EDGE-33 is 32 links below EDGE-1
	if got := censusCount(t, store); got != 0 {
		t.Fatalf("census = %d, want 0", got)
	}
	if err := store.migrate(ctx, migrationsThrough(t, repoProjectsDefaultVersion)); err != nil {
		t.Fatalf("migrate to %d over a 32-link chain: %v", repoProjectsDefaultVersion, err)
	}
}

func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// awaitMigrationLockWait waits until a backend of store's database other than the pool's own
// poll waits on a lock: the migration, queued behind a writer's open transaction.
func awaitMigrationLockWait(t *testing.T, store *Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := store.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and pid <> pg_backend_pid()
		`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the migration never waited on the writer's lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A legacy release's create that commits while 0113 waits for its tables is counted: the
// migration takes its tables' locks before it counts, so a row committed while it waits is in the
// count, and an issue 33 links down refuses the migration as it would have before it began.
func TestMigrationCountsAnOverDeepIssueCommittedWhileItWaits(t *testing.T) {
	ctx := context.Background()
	store := openEmptyTestStore(t)
	if err := store.migrate(ctx, migrationsThrough(t, repoProjectsDefaultVersion-1)); err != nil {
		t.Fatalf("migrate through %d: %v", repoProjectsDefaultVersion-1, err)
	}
	seedChain(t, store, "EDGE", 33) // EDGE-33 is 32 links below EDGE-1, within the cap
	if got := censusCount(t, store); got != 0 {
		t.Fatalf("census = %d, want 0", got)
	}
	writer, err := store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the writer: %v", err)
	}
	defer writer.Rollback(ctx)
	if _, err := writer.Exec(ctx, `
		insert into issues (key, project_key, number, title, parent_key, created_by, rank)
		values ('EDGE-34', 'EDGE', 34, 'Link 34', 'EDGE-33', '{"kind":"user","id":"alice"}', '00000034')
	`); err != nil {
		t.Fatalf("the writer's insert 33 links down: %v", err)
	}
	applied := make(chan error, 1)
	go func() { applied <- store.migrate(ctx, migrationsThrough(t, repoProjectsDefaultVersion)) }()
	awaitMigrationLockWait(t, store)
	if err := writer.Commit(ctx); err != nil {
		t.Fatalf("commit the writer: %v", err)
	}
	select {
	case err := <-applied:
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" || !strings.Contains(err.Error(), "EDGE-34") {
			t.Fatalf("migrate after a writer committed EDGE-34 33 links down: %v, want 0113 refusing it (23514)", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the migration never finished after the writer committed")
	}
	if got := schemaVersion(t, store); got != repoProjectsDefaultVersion-1 {
		t.Fatalf("schema version = %d, want %d", got, repoProjectsDefaultVersion-1)
	}
}
