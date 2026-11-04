package store

import (
	"context"
	"testing"
)

// asksApprovalDocumentsVersion is the migration that grows an approval ask from one document to a
// list of them.
const asksApprovalDocumentsVersion = 20261102110100

// Every stored approval ask gets the documents array its four top-level fields already imply, and
// a row the release before this one writes during a rolling deploy - the four fields and no array,
// on an insert or on the update a version move makes - is normalized to the same shape, so every
// reader finds the list and the rebuilt check admits the row.
func TestMigrationGivesApprovalAsksTheirDocumentList(t *testing.T) {
	ctx := context.Background()
	store := openMigratedThrough(t, asksApprovalDocumentsVersion-1)
	const artifactID = "11111111-1111-4111-8111-111111111111"
	for _, seed := range []string{
		`insert into projects (key, name) values ('TEST', 'Test')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('TEST-1', 'TEST', 1, 'Spec', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into artifacts (id, issue_key, project_key, slug, name, kind, is_primary, created_by)
		 values ('` + artifactID + `', 'TEST-1', 'TEST', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}')`,
		`insert into asks (id, issue_key, author, question, options, multiple, urgency, kind, approval)
		 values ('22222222-2222-4222-8222-222222222222', 'TEST-1', '{"kind":"user","id":"alice"}',
		         'Approve spec.md (version 2)?', '[{"label":"Approve"},{"label":"Request changes"}]', false, 'high', 'approval',
		         '{"artifact_id":"` + artifactID + `","name":"spec.md","version":2,"requested_version":1}')`,
	} {
		if _, err := store.Pool.Exec(ctx, seed); err != nil {
			t.Fatalf("seed before the migration: %v", err)
		}
	}
	if err := store.migrate(ctx, migrationsThrough(t, asksApprovalDocumentsVersion)); err != nil {
		t.Fatalf("migrate to the document list: %v", err)
	}
	documents := func(id string) string {
		t.Helper()
		var stored string
		if err := store.Pool.QueryRow(ctx, `select (approval -> 'documents')::text from asks where id = $1`, id).Scan(&stored); err != nil {
			t.Fatalf("read the documents of %s: %v", id, err)
		}
		return stored
	}
	want := `[{"name": "spec.md", "version": 2, "artifact_id": "` + artifactID + `", "requested_version": 1}]`
	if got := documents("22222222-2222-4222-8222-222222222222"); got != want {
		t.Fatalf("backfilled documents = %s, want %s", got, want)
	}

	// The release before this one inserts the four fields alone; the trigger writes the list.
	if _, err := store.Pool.Exec(ctx, `insert into asks (id, issue_key, author, question, options, multiple, urgency, kind, approval)
		values ('33333333-3333-4333-8333-333333333333', 'TEST-1', '{"kind":"user","id":"alice"}',
		        'Approve spec.md (version 1)?', '[{"label":"Approve"},{"label":"Request changes"}]', false, 'high', 'approval',
		        '{"artifact_id":"`+artifactID+`","name":"spec.md","version":1,"requested_version":1}')`); err != nil {
		t.Fatalf("the previous release's approval insert: %v", err)
	}
	inserted := `[{"name": "spec.md", "version": 1, "artifact_id": "` + artifactID + `", "requested_version": 1}]`
	if got := documents("33333333-3333-4333-8333-333333333333"); got != inserted {
		t.Fatalf("documents of the previous release's row = %s, want %s", got, inserted)
	}
	// And its version move, which rewrites the blob without the list, is normalized the same way.
	if _, err := store.Pool.Exec(ctx, `update asks set approval = '{"artifact_id":"`+artifactID+`","name":"spec.md","version":3,"requested_version":1}'
		where id = '33333333-3333-4333-8333-333333333333'`); err != nil {
		t.Fatalf("the previous release's version move: %v", err)
	}
	moved := `[{"name": "spec.md", "version": 3, "artifact_id": "` + artifactID + `", "requested_version": 1}]`
	if got := documents("33333333-3333-4333-8333-333333333333"); got != moved {
		t.Fatalf("documents after the previous release's move = %s, want %s", got, moved)
	}

	// The rebuilt check holds every document to the shape the four fields have always been held
	// to, and holds the first of them to the document the request was opened on.
	for name, approval := range map[string]string{
		"an empty list":               `{"artifact_id":"` + artifactID + `","name":"spec.md","version":1,"requested_version":1,"documents":[]}`,
		"a document with no name":     `{"artifact_id":"` + artifactID + `","name":"spec.md","version":1,"requested_version":1,"documents":[{"artifact_id":"` + artifactID + `","version":1,"requested_version":1}]}`,
		"a version below its own":     `{"artifact_id":"` + artifactID + `","name":"spec.md","version":1,"requested_version":1,"documents":[{"artifact_id":"` + artifactID + `","name":"spec.md","version":1,"requested_version":2}]}`,
		"an id that is no uuid":       `{"artifact_id":"` + artifactID + `","name":"spec.md","version":1,"requested_version":1,"documents":[{"artifact_id":"nope","name":"spec.md","version":1,"requested_version":1}]}`,
		"a first document of its own": `{"artifact_id":"` + artifactID + `","name":"spec.md","version":1,"requested_version":1,"documents":[{"artifact_id":"44444444-4444-4444-8444-444444444444","name":"other.md","version":1,"requested_version":1}]}`,
	} {
		if _, err := store.Pool.Exec(ctx, `update asks set approval = $2 where id = $1`,
			"33333333-3333-4333-8333-333333333333", approval); err == nil {
			t.Fatalf("%s was stored, want it refused", name)
		}
	}
	// Two documents, each at its own version, are what the request that names both stores.
	both := `{"artifact_id":"` + artifactID + `","name":"spec.md","version":2,"requested_version":2,"documents":[{"artifact_id":"` + artifactID + `","name":"spec.md","version":2,"requested_version":2},{"artifact_id":"44444444-4444-4444-8444-444444444444","name":"verification.md","version":1,"requested_version":1}]}`
	if _, err := store.Pool.Exec(ctx, `update asks set approval = $2 where id = $1`,
		"33333333-3333-4333-8333-333333333333", both); err != nil {
		t.Fatalf("store a request naming two documents: %v", err)
	}
}
