package files_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/files"
	"github.com/sjawhar/envoy/internal/dispatch/files/filestest"
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
	`, artifactID, body, len(body), files.SHA256(body)).Scan(&id); err != nil {
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

// refusing is a store that refuses to write one hash and passes every other call on: a bucket
// that fails partway through a run.
type refusing struct {
	files.Store
	sha string
}

func (r refusing) Put(ctx context.Context, sha, mime string, body []byte) error {
	if sha == r.sha {
		return errors.New("bucket refused the write")
	}
	return r.Store.Put(ctx, sha, mime, body)
}

func TestBackfillMovesEachRowOnceAndResumesWhereAFailedRunStopped(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	firstBody, secondBody := bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 2000)
	first := seedFileVersion(t, database, "first", firstBody)
	second := seedFileVersion(t, database, "second", secondBody)
	// Oldest first: the first row is the one a run moves before it reaches the second.
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set created_at = created_at - interval '1 hour' where id = $1`, first); err != nil {
		t.Fatalf("age the first row: %v", err)
	}
	// A document version holds markdown and no bytes; it is never a candidate.
	if _, err := database.Pool.Exec(ctx, `
		with doc as (
			insert into artifacts (issue_key, project_key, slug, name, kind, created_by)
			values ('FILES-1', 'FILES', 'notes', 'notes.md', 'doc', '{"kind":"user","id":"alice"}')
			returning id
		)
		insert into artifact_versions (artifact_id, number, markdown) select id, 1, '# Notes' from doc
	`); err != nil {
		t.Fatalf("seed document: %v", err)
	}
	memory := filestest.NewMemory()

	// The bucket refuses the second row: the run stops there with the first row moved and the
	// second still holding its bytes, which the server keeps serving.
	var out strings.Builder
	report, err := files.BackfillRows(ctx, database.Pool, refusing{Store: memory, sha: files.SHA256(secondBody)}, &out)
	if err == nil || !strings.Contains(err.Error(), second) {
		t.Fatalf("BackfillRows against a bucket refusing the second row: report %+v, err %v, want an error naming %s", report, err, second)
	}
	if report.Moved != 1 || rowBytes(t, database, first) != nil || rowBytes(t, database, second) == nil {
		t.Fatalf("a run stopped at the second row: report %+v, want the first row moved and the second untouched", report)
	}

	// The next run picks up at the second row and moves only it.
	report, err = files.BackfillRows(ctx, database.Pool, memory, &out)
	if err != nil {
		t.Fatalf("BackfillRows: %v", err)
	}
	if report.Moved != 1 || report.Bytes != int64(len(secondBody)) {
		t.Fatalf("resumed run: report %+v, want the second row's %d bytes alone", report, len(secondBody))
	}
	if rowBytes(t, database, second) != nil {
		t.Fatal("the resumed run left the second row's bytes")
	}
	for _, body := range [][]byte{firstBody, secondBody} {
		if got, err := memory.Get(ctx, files.SHA256(body)); err != nil || !bytes.Equal(got, body) {
			t.Fatalf("the store holds %d bytes for a moved file (%v), want its %d bytes", len(got), err, len(body))
		}
	}
	if !strings.Contains(out.String(), "moved version "+first) || !strings.Contains(out.String(), "moved version "+second) {
		t.Errorf("progress output does not name both moved versions:\n%s", out.String())
	}

	// A third run finds nothing to move.
	report, err = files.BackfillRows(ctx, database.Pool, memory, &out)
	if err != nil || report.Moved != 0 {
		t.Fatalf("third run: report %+v, err %v", report, err)
	}

	verified, err := files.VerifyRows(ctx, database.Pool, memory, &out)
	if err != nil || verified.Checked != 2 || len(verified.Failed) != 0 {
		t.Fatalf("VerifyRows: %+v, %v, want the two files checked and the document skipped", verified, err)
	}
}

func TestBackfillStopsAtARowWhoseBytesDoNotMatchTheirHash(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	id := seedFileVersion(t, database, "corrupt", []byte("stored bytes"))
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set content = $2 where id = $1`, id, []byte("other bytes")); err != nil {
		t.Fatalf("corrupt the row: %v", err)
	}
	memory := filestest.NewMemory()
	report, err := files.BackfillRows(ctx, database.Pool, memory, &strings.Builder{})
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
	seedFileVersion(t, database, "kept", []byte("kept bytes"))
	lost := seedFileVersion(t, database, "lost", []byte("lost bytes"))
	altered := seedFileVersion(t, database, "altered", []byte("altered bytes"))
	memory := filestest.NewMemory()
	if _, err := files.BackfillRows(ctx, database.Pool, memory, &strings.Builder{}); err != nil {
		t.Fatalf("BackfillRows: %v", err)
	}
	memory.Delete(files.SHA256([]byte("lost bytes")))

	var out strings.Builder
	report, err := files.VerifyRows(ctx, database.Pool, altering{Store: memory, sha: files.SHA256([]byte("altered bytes"))}, &out)
	if err != nil {
		t.Fatalf("VerifyRows: %v", err)
	}
	failed := strings.Join(report.Failed, "\n")
	if report.Checked != 1 || len(report.Failed) != 2 || !strings.Contains(failed, lost) || !strings.Contains(failed, altered) {
		t.Fatalf("report = %+v, want one row checked and %s and %s failed", report, lost, altered)
	}
	for _, id := range []string{lost, altered} {
		if !strings.Contains(out.String(), "FAILED version "+id) {
			t.Errorf("output does not name failed version %s:\n%s", id, out.String())
		}
	}
}

// The schema lets a version's sha256 and mime be null. A row with no type is moved under the
// upload route's default; a row with no hash stops the pass naming that row, rather than with a
// scan error that names none, and keeps its bytes.
func TestBackfillNamesARowWithNoHashAndMovesARowWithNoType(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	untyped := seedFileVersion(t, database, "untyped", []byte("untyped bytes"))
	unhashed := seedFileVersion(t, database, "unhashed", []byte("unhashed bytes"))
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set mime = null where id = $1`, untyped); err != nil {
		t.Fatalf("clear the type: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set sha256 = null where id = $1`, unhashed); err != nil {
		t.Fatalf("clear the hash: %v", err)
	}
	memory := filestest.NewMemory()
	report, err := files.BackfillRows(ctx, database.Pool, memory, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), unhashed) {
		t.Fatalf("BackfillRows over a row with no hash: report %+v, err %v, want an error naming %s", report, err, unhashed)
	}
	if report.Moved != 1 || rowBytes(t, database, untyped) != nil {
		t.Fatalf("the row with no type was not moved: report %+v", report)
	}
	if mime := memory.MIME(files.SHA256([]byte("untyped bytes"))); mime != "application/octet-stream" {
		t.Fatalf("the row with no type was stored as %q, want application/octet-stream", mime)
	}
	if rowBytes(t, database, unhashed) == nil {
		t.Fatal("the row with no hash lost its bytes")
	}
}

// altering is a store that reads back other bytes than it was given under one hash and passes
// every other call on: an object another writer replaced, or a bucket that answered for a write
// it did not keep.
type altering struct {
	files.Store
	sha string
}

func (a altering) Get(ctx context.Context, sha string) ([]byte, error) {
	body, err := a.Store.Get(ctx, sha)
	if err != nil || sha != a.sha {
		return body, err
	}
	return append(body, '!'), nil
}

// A row's bytes are its only copy until the store reads them back under their hash, so a store that
// acknowledged a write it did not keep must leave the row as it was.
func TestBackfillKeepsARowsBytesUntilTheStoreReadsThemBack(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	id := seedFileVersion(t, database, "kept", []byte("the only copy"))
	store := altering{Store: filestest.NewMemory(), sha: files.SHA256([]byte("the only copy"))}
	report, err := files.BackfillRows(ctx, database.Pool, store, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), id) {
		t.Fatalf("BackfillRows against a store that keeps other bytes: report %+v, err %v, want an error naming %s", report, err, id)
	}
	if report.Moved != 0 || !bytes.Equal(rowBytes(t, database, id), []byte("the only copy")) {
		t.Fatalf("a write the store did not keep cleared the row: report %+v", report)
	}
}
