package files

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// seedFileVersion writes an issue, a file artifact and one version holding body in its row, as
// an upload did before the store existed, and returns the version's id.
func seedFileVersion(t *testing.T, database *store.Store, slug string, body []byte) string {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('FILES', 'Files') on conflict do nothing`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, status, created_by, rank) values ('FILES-1', 'FILES', 1, 'Issue', 'todo', '{"kind":"user","id":"alice"}', 'U')
		on conflict do nothing
	`); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	var artifactID string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('FILES-1', 'FILES', $1, $1, 'file', '{"kind":"user","id":"alice"}')
		returning id::text
	`, slug).Scan(&artifactID); err != nil {
		t.Fatalf("seed artifact: %v", err)
	}
	var id string
	if err := database.Pool.QueryRow(ctx, `
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256)
		values ($1, 1, $2, 'application/octet-stream', $3, $4)
		returning id::text
	`, artifactID, body, len(body), SHA256(body)).Scan(&id); err != nil {
		t.Fatalf("seed version: %v", err)
	}
	return id
}

// rowBytes is what the version's row holds: nil once the backfill cleared it.
func rowBytes(t *testing.T, database *store.Store, id string) []byte {
	t.Helper()
	var content []byte
	if err := database.Pool.QueryRow(context.Background(), `select content from artifact_versions where id = $1`, id).Scan(&content); err != nil {
		t.Fatalf("read version %s: %v", id, err)
	}
	return content
}

func TestBackfillMovesEachRowOnceAndLeavesTheRestUntouchedWhenTheStoreFails(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	first := seedFileVersion(t, database, "first", bytes.Repeat([]byte("a"), 1000))
	second := seedFileVersion(t, database, "second", bytes.Repeat([]byte("b"), 2000))
	// A document version holds markdown and no bytes; it is never a candidate.
	if _, err := database.Pool.Exec(ctx, `
		insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
		values ('FILES-1', 'FILES', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
	`); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	memory := NewMemory()

	// The store refuses before anything moves: both rows keep their bytes.
	memory.Fail = errors.New("bucket unreachable")
	var out strings.Builder
	report, err := BackfillRows(ctx, database.Pool, memory, &out)
	if err == nil || !strings.Contains(err.Error(), "bucket unreachable") {
		t.Fatalf("BackfillRows against a failing store: report %+v, err %v", report, err)
	}
	if report.Moved != 0 || rowBytes(t, database, first) == nil || rowBytes(t, database, second) == nil {
		t.Fatalf("a failing store moved rows: report %+v", report)
	}

	memory.Fail = nil
	report, err = BackfillRows(ctx, database.Pool, memory, &out)
	if err != nil {
		t.Fatalf("BackfillRows: %v", err)
	}
	if report.Moved != 2 || report.Bytes != 3000 {
		t.Fatalf("report = %+v, want 2 rows and 3000 bytes", report)
	}
	if rowBytes(t, database, first) != nil || rowBytes(t, database, second) != nil {
		t.Fatal("moved rows still hold their bytes")
	}
	if memory.Len() != 2 {
		t.Fatalf("store holds %d objects, want 2", memory.Len())
	}
	got, err := memory.Get(ctx, SHA256(bytes.Repeat([]byte("a"), 1000)))
	if err != nil || len(got) != 1000 {
		t.Fatalf("the first file in the store: %d bytes, %v", len(got), err)
	}
	if !strings.Contains(out.String(), "moved version "+first) {
		t.Errorf("progress output names no moved version:\n%s", out.String())
	}

	// A second pass finds nothing to move and changes nothing.
	report, err = BackfillRows(ctx, database.Pool, memory, &out)
	if err != nil || report.Moved != 0 {
		t.Fatalf("second pass: report %+v, err %v", report, err)
	}
	if memory.Puts() != 2 {
		t.Errorf("the store saw %d writes, want 2", memory.Puts())
	}

	verified, err := VerifyRows(ctx, database.Pool, memory, &out)
	if err != nil || verified.Checked != 2 || len(verified.Failed) != 0 {
		t.Fatalf("VerifyRows: %+v, %v", verified, err)
	}
}

func TestBackfillStopsAtARowWhoseBytesDoNotMatchTheirHash(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	id := seedFileVersion(t, database, "corrupt", []byte("stored bytes"))
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set content = $2 where id = $1`, id, []byte("other bytes")); err != nil {
		t.Fatalf("corrupt the row: %v", err)
	}
	memory := NewMemory()
	report, err := BackfillRows(ctx, database.Pool, memory, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "hash to") {
		t.Fatalf("BackfillRows over a corrupt row: report %+v, err %v", report, err)
	}
	if rowBytes(t, database, id) == nil || memory.Len() != 0 {
		t.Fatal("a corrupt row was moved")
	}
}

func TestVerifyNamesARowWhoseObjectIsMissingOrWrong(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	kept := seedFileVersion(t, database, "kept", []byte("kept bytes"))
	lost := seedFileVersion(t, database, "lost", []byte("lost bytes"))
	memory := NewMemory()
	if _, err := BackfillRows(ctx, database.Pool, memory, &strings.Builder{}); err != nil {
		t.Fatalf("BackfillRows: %v", err)
	}
	memory.Delete(SHA256([]byte("lost bytes")))

	var out strings.Builder
	report, err := VerifyRows(ctx, database.Pool, memory, &out)
	if err != nil {
		t.Fatalf("VerifyRows: %v", err)
	}
	if report.Checked != 1 || len(report.Failed) != 1 || !strings.Contains(report.Failed[0], lost) {
		t.Fatalf("report = %+v, want %s checked and %s failed", report, kept, lost)
	}
	if !strings.Contains(out.String(), "FAILED version "+lost) {
		t.Errorf("output names no failed version:\n%s", out.String())
	}
}
