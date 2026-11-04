package store

import (
	"context"
	"testing"
)

// artifactDocumentRolesVersion is the migration that gives an issue's Verification document and
// its plan their role.
const artifactDocumentRolesVersion = 20261102110000

// The role column starts null on every stored document, and the backfill gives it to the issue
// documents already slugged verification-md and plan-md, so the tabs that read a role find a plan
// written before this deploy. Nothing else takes one: a project document of the same slug, a
// picture, and another issue document each keep null.
func TestMigrationGivesStoredIssueDocumentsTheirRole(t *testing.T) {
	ctx := context.Background()
	store := openMigratedThrough(t, artifactDocumentRolesVersion-1)
	for _, seed := range []string{
		`insert into projects (key, name) values ('TEST', 'Test')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('TEST-1', 'TEST', 1, 'Spec', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		 values ('TEST-1', 'TEST', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')`,
		`insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		 values ('TEST-1', 'TEST', 'verification-md', 'verification.md', 'doc', '{"kind":"user","id":"alice"}')`,
		`insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		 values ('TEST-1', 'TEST', 'plan-md', 'plan.md', 'doc', '{"kind":"user","id":"alice"}')`,
		`insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		 values ('TEST-1', 'TEST', 'notes-md', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')`,
		`insert into artifacts (project_key, slug, name, kind, created_by)
		 values ('TEST', 'plan-md', 'plan.md', 'doc', '{"kind":"user","id":"alice"}')`,
	} {
		if _, err := store.Pool.Exec(ctx, seed); err != nil {
			t.Fatalf("seed before the migration: %v", err)
		}
	}
	if err := store.migrate(ctx, migrationsThrough(t, artifactDocumentRolesVersion)); err != nil {
		t.Fatalf("migrate to the roles: %v", err)
	}
	for _, want := range []struct{ refKey, role string }{
		{"TEST-1/spec", ""},
		{"TEST-1/verification-md", "verification"},
		{"TEST-1/plan-md", "plan"},
		{"TEST-1/notes-md", ""},
		{"TEST/plan-md", ""},
	} {
		var role string
		if err := store.Pool.QueryRow(ctx, `select coalesce(role, '') from artifacts where ref_key = $1`, want.refKey).Scan(&role); err != nil {
			t.Fatalf("read the role of %s: %v", want.refKey, err)
		}
		if role != want.role {
			t.Fatalf("role of %s = %q, want %q", want.refKey, role, want.role)
		}
	}
	// The release before this one goes on writing documents with no role, which the schema admits.
	if _, err := store.Pool.Exec(ctx, `insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('TEST-1', 'TEST', 'design-md', 'design.md', 'doc', '{"kind":"user","id":"alice"}')`); err != nil {
		t.Fatalf("the previous release's document write: %v", err)
	}
	// An issue holds one document per role, and a role belongs to an issue's markdown document.
	for name, write := range map[string]string{
		"a second plan on one issue": `insert into artifacts (issue_key, project_key, slug, name, kind, role, created_by)
			values ('TEST-1', 'TEST', 'plan-2-md', 'plan 2.md', 'doc', 'plan', '{"kind":"user","id":"alice"}')`,
		"a role on a project document": `insert into artifacts (project_key, slug, name, kind, role, created_by)
			values ('TEST', 'verification-md', 'verification.md', 'doc', 'verification', '{"kind":"user","id":"alice"}')`,
		"a role nobody plays": `insert into artifacts (issue_key, project_key, slug, name, kind, role, created_by)
			values ('TEST-1', 'TEST', 'other-md', 'other.md', 'doc', 'other', '{"kind":"user","id":"alice"}')`,
	} {
		if _, err := store.Pool.Exec(ctx, write); err == nil {
			t.Fatalf("%s was stored, want it refused", name)
		}
	}
}
