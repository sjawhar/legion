package store

import (
	"context"
	"testing"
	"time"
)

// document_edit_notices holds one pending notice per document: a row reads back as it was written,
// a held row names no last editor and has no due time, the two together, and deleting its document
// takes the row with it, so an artifact delete of either release keeps working.
func TestDocumentEditNotices(t *testing.T) {
	ctx := context.Background()
	store := migratedEmptyStore(t)
	var artifactID string
	if err := store.Pool.QueryRow(ctx, `
		with p as (insert into projects (key, name) values ('CORE', 'Core') returning key),
		i as (insert into issues (key, project_key, number, title, created_by, rank) select 'CORE-1', key, 1, 'Spec', '{"kind":"user","id":"alice"}', 'U' from p returning key),
		a as (insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by) select key, 'CORE', 'spec', 'spec.md', 'doc', true, '{"kind":"user","id":"alice"}' from i returning id)
		select id::text from a
	`).Scan(&artifactID); err != nil {
		t.Fatalf("seed a document: %v", err)
	}
	due := time.Date(2026, 10, 24, 12, 0, 30, 0, time.UTC)
	if _, err := store.Pool.Exec(ctx, `
		insert into document_edit_notices (artifact_id, from_version, to_version, editors, last_editor, due_at)
		values ($1, 4, 7, '["alice@example.com","bob@example.com"]', '{"kind":"user","id":"bob@example.com"}', $2)
	`, artifactID, due); err != nil {
		t.Fatalf("insert a notice: %v", err)
	}
	var from, to int
	var editors, lastEditor string
	var dueAt time.Time
	if err := store.Pool.QueryRow(ctx, `
		select from_version, to_version, editors::text, last_editor::text, due_at
		from document_edit_notices where artifact_id = $1
	`, artifactID).Scan(&from, &to, &editors, &lastEditor, &dueAt); err != nil {
		t.Fatalf("read the notice back: %v", err)
	}
	if from != 4 || to != 7 || editors != `["alice@example.com", "bob@example.com"]` ||
		lastEditor != `{"id": "bob@example.com", "kind": "user"}` || !dueAt.Equal(due) {
		t.Fatalf("notice = from %d to %d editors %s last %s due %s; want the row as written", from, to, editors, lastEditor, dueAt)
	}
	if _, err := store.Pool.Exec(ctx, `
		update document_edit_notices set last_editor = null, due_at = null where artifact_id = $1
	`, artifactID); err != nil {
		t.Fatalf("hold the notice: %v", err)
	}
	if _, err := store.Pool.Exec(ctx, `
		update document_edit_notices set due_at = now() where artifact_id = $1
	`, artifactID); err == nil {
		t.Fatal("a notice due with no last editor was stored, want the check to refuse it")
	}
	if _, err := store.Pool.Exec(ctx, `delete from artifacts where id = $1`, artifactID); err != nil {
		t.Fatalf("delete the document: %v", err)
	}
	var left int
	if err := store.Pool.QueryRow(ctx, `select count(*) from document_edit_notices`).Scan(&left); err != nil {
		t.Fatalf("count notices: %v", err)
	}
	if left != 0 {
		t.Fatalf("notices after the document's delete = %d, want 0", left)
	}
}
