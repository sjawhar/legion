package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// envoy-dispatch stored-order exits 2 on a usage error, 0 when every document's stored history
// replays and 1 once one does not, names a mismatch without the document's text, and counts a
// named document whose versions all record updates below its compaction base as unchecked.
func TestStoredOrderSubcommandExitCodes(t *testing.T) {
	ctx := context.Background()
	run := func(env map[string]string, args ...string) (int, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := runSubcommand(ctx, append([]string{"stored-order"}, args...), envGetter(env), &stdout, &stderr)
		return code, stdout.String() + stderr.String()
	}
	for _, args := range [][]string{{"--since", "yesterday"}, {"--artifact", "not-a-document"}, {"--everything"}, {"extra"}} {
		if code, out := run(nil, args...); code != 2 {
			t.Errorf("stored-order %v: exit %d, want 2:\n%s", args, code, out)
		}
	}

	database := storetest.Open(t)
	env := map[string]string{"DATABASE_URL": database.Pool.Config().ConnString()}
	if code, out := run(env); code != 1 || !strings.Contains(out, "documents=0") {
		t.Fatalf("stored-order on an empty database: exit %d, want 1, since it checked nothing:\n%s", code, out)
	}
	var artifactID string
	for _, statement := range []string{
		`insert into projects (key, name) values ('DOC', 'Documents')`,
		`insert into issues (key, project_key, number, title, status, created_by, rank) values ('DOC-1', 'DOC', 1, 'Issue', 'todo', '{"kind":"user","id":"alice"}', 'U')`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('DOC-1', 'DOC', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}') returning id::text
	`).Scan(&artifactID); err != nil {
		t.Fatalf("seed the document: %v", err)
	}
	// The document's one stored update is its base, a paragraph; its first version predates it and
	// is skipped.
	tree, err := pmdoc.Parse("Before.\n")
	if err != nil {
		t.Fatalf("parse the document: %v", err)
	}
	markdown, err := pmdoc.Render(tree)
	if err != nil {
		t.Fatalf("render the document: %v", err)
	}
	base := crdt.New()
	fragment := base.GetXmlFragment("prosemirror")
	var written error
	base.Transact(func(txn *crdt.Transaction) { written = pmdoc.Update(txn, fragment, tree) })
	if written != nil {
		t.Fatalf("write the document: %v", written)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed) values ($1, 1, $2, true)
	`, artifactID, crdt.EncodeStateAsUpdateV1(base, nil)); err != nil {
		t.Fatalf("seed the document's update: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, doc_update_version) values ($1, 1, 'Before.', 0)
	`, artifactID); err != nil {
		t.Fatalf("seed the document's first version: %v", err)
	}
	if code, out := run(env, "--artifact", artifactID); code != 1 || !strings.Contains(out, "updates_checked=0") || !strings.Contains(out, "unchecked=1") {
		t.Fatalf("stored-order on a named document it compared nothing of: exit %d, want 1 and the document unchecked:\n%s", code, out)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, doc_update_version) values ($1, 2, $2, 1)
	`, artifactID, markdown); err != nil {
		t.Fatalf("seed a version the fold renders: %v", err)
	}
	if code, out := run(env, "--artifact", artifactID); code != 0 || !strings.Contains(out, "versions_checked=1") || !strings.Contains(out, "clean=1") {
		t.Fatalf("stored-order on a document that replays: exit %d, want 0:\n%s", code, out)
	}
	const text = "Words the document never held."
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, doc_update_version) values ($1, 3, $2, 1)
	`, artifactID, text); err != nil {
		t.Fatalf("seed a version the fold does not render: %v", err)
	}
	code, out := run(env, "--since", "2000-01-01T00:00:00Z")
	if code != 1 || !strings.Contains(out, "version number=3 doc_update_version=1") || strings.Contains(out, text) {
		t.Fatalf("stored-order on a document that does not replay: exit %d, want 1 naming version 3 without its text:\n%s", code, out)
	}

	// A document a compaction left with its base above every version it holds: update 3 after the
	// base is checked, no version is compared with the fold, and the run that names it fails.
	var compactedID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('DOC-1', 'DOC', 'compacted', 'compacted.md', 'doc', '{"kind":"user","id":"alice"}') returning id::text
	`).Scan(&compactedID); err != nil {
		t.Fatalf("seed the compacted document: %v", err)
	}
	folded := crdt.New()
	foldedFragment := folded.GetXmlFragment("prosemirror")
	folded.Transact(func(txn *crdt.Transaction) { written = pmdoc.Update(txn, foldedFragment, tree) })
	if written != nil {
		t.Fatalf("write the compacted document: %v", written)
	}
	// Update 2 is the row a compaction folded the history into, the document's base.
	foldedBase := crdt.EncodeStateAsUpdateV1(folded, nil)
	after, err := pmdoc.Parse("Before, edited.\n")
	if err != nil {
		t.Fatalf("parse the compacted document's edit: %v", err)
	}
	var edit []byte
	unsubscribe := folded.OnUpdate(func(update []byte, _ any) { edit = append([]byte(nil), update...) })
	folded.Transact(func(txn *crdt.Transaction) { written = pmdoc.Update(txn, foldedFragment, after) })
	unsubscribe()
	if written != nil || edit == nil {
		t.Fatalf("edit the compacted document: %v", written)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into doc_updates (artifact_id, version, update, content_changed) values ($1, 2, $2, true), ($1, 3, $3, true)
	`, compactedID, foldedBase, edit); err != nil {
		t.Fatalf("seed the compacted document's updates: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into artifact_versions (artifact_id, number, markdown, doc_update_version) values ($1, 1, 'Before.', 1)
	`, compactedID); err != nil {
		t.Fatalf("seed the compacted document's version: %v", err)
	}
	if code, out := run(env, "--artifact", compactedID); code != 1 || !strings.Contains(out, "versions_compacted=1") || !strings.Contains(out, "unchecked=1") {
		t.Fatalf("stored-order on a document whose only version is below its compaction base: exit %d, want 1 and the document unchecked:\n%s", code, out)
	}
}
