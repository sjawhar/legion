package store

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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

func isSQLState(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}
