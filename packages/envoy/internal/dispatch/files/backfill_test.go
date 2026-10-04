package files_test

import (
	"bytes"
	"context"
	"errors"
	"io"
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
// an upload did before the store existed, and returns the version's id. Each call's row is
// created later than the previous call's, so a pass visits them in the order they were seeded.
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
		insert into artifact_versions (artifact_id, number, content, mime, size, sha256, created_at)
		values ($1, 1, $2, 'application/octet-stream', $3, $4, coalesce((select max(created_at) from artifact_versions), now()) + interval '1 second')
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

// storedBytes reads sha whole from store, as a caller that copies a body to its end does.
func storedBytes(t *testing.T, store files.Store, sha string) []byte {
	t.Helper()
	body, err := files.ReadAll(context.Background(), store, sha)
	if err != nil {
		t.Fatalf("read %s from the store: %v", sha, err)
	}
	return body
}

// refusing is a store that refuses to write one hash and passes every other call on: a bucket
// that fails for one object.
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

// altering is a store whose object under one hash reads back with an extra byte and passes every
// other call on: an object another writer replaced, or a bucket that answered for a write it did
// not keep. The extra byte makes the verifying reader refuse the body at its end.
type altering struct {
	files.Store
	sha string
}

func (a altering) Get(ctx context.Context, sha string) (*files.Object, error) {
	object, err := a.Store.Get(ctx, sha)
	if err != nil || sha != a.sha {
		return object, err
	}
	body, err := io.ReadAll(object.Body)
	_ = object.Body.Close()
	if err != nil {
		// The inner reader verified it: take the bytes it passed through before refusing.
		body = append([]byte(nil), body...)
	}
	altered := append(body, '!')
	size := int64(len(altered))
	return &files.Object{Body: files.NewVerifyingReader(io.NopCloser(bytes.NewReader(altered)), sha, size), Size: size}, nil
}

func TestBackfillMovesEveryRowItCanAndNamesTheRowsItCannot(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	firstBody, secondBody, thirdBody := bytes.Repeat([]byte("a"), 1000), bytes.Repeat([]byte("b"), 2000), bytes.Repeat([]byte("c"), 3000)
	first := seedFileVersion(t, database, "first", firstBody)
	second := seedFileVersion(t, database, "second", secondBody)
	third := seedFileVersion(t, database, "third", thirdBody)
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

	// The bucket refuses the second row: the pass names it, moves the rows either side of it, and
	// leaves the second holding its bytes, which the server keeps serving.
	var out strings.Builder
	report, err := files.BackfillRows(ctx, database.Pool, refusing{Store: memory, sha: files.SHA256(secondBody)}, &out)
	if err != nil {
		t.Fatalf("BackfillRows against a bucket refusing one row: %v", err)
	}
	if report.Done != 2 || len(report.Failed) != 1 || !strings.Contains(report.Failed[0], second) {
		t.Fatalf("report = %+v, want two rows moved and %s named as failed", report, second)
	}
	if rowBytes(t, database, first) != nil || rowBytes(t, database, third) != nil || rowBytes(t, database, second) == nil {
		t.Fatal("want the first and third rows cleared and the second untouched")
	}
	if !strings.Contains(out.String(), "FAILED version "+second) {
		t.Errorf("output does not name the failed row:\n%s", out.String())
	}

	// The next pass moves the one row left, and a pass after that moves nothing.
	report, err = files.BackfillRows(ctx, database.Pool, memory, &out)
	if err != nil || report.Done != 1 || report.Bytes != int64(len(secondBody)) || len(report.Failed) != 0 {
		t.Fatalf("second pass: report %+v, err %v, want the second row's %d bytes alone", report, err, len(secondBody))
	}
	report, err = files.BackfillRows(ctx, database.Pool, memory, &out)
	if err != nil || report.Done != 0 || len(report.Failed) != 0 {
		t.Fatalf("third pass: report %+v, err %v, want nothing to do", report, err)
	}
	for _, body := range [][]byte{firstBody, secondBody, thirdBody} {
		if got := storedBytes(t, memory, files.SHA256(body)); !bytes.Equal(got, body) {
			t.Fatalf("the store holds %d bytes for a moved file, want its %d bytes", len(got), len(body))
		}
	}
	if !strings.Contains(out.String(), "moved version "+first) || !strings.Contains(out.String(), "moved version "+second) {
		t.Errorf("progress output does not name the moved versions:\n%s", out.String())
	}

	verified, err := files.VerifyRows(ctx, database.Pool, memory, &out)
	if err != nil || verified.Done != 3 || len(verified.Failed) != 0 {
		t.Fatalf("VerifyRows: %+v, %v, want the three files checked and the document skipped", verified, err)
	}
}

// A row whose bytes do not hash to its recorded hash is named and passed over, and keeps its bytes:
// moving it would store the bytes under a hash that is not theirs.
func TestBackfillPassesOverARowWhoseBytesDoNotMatchTheirHash(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	corrupt := seedFileVersion(t, database, "corrupt", []byte("stored bytes"))
	sound := seedFileVersion(t, database, "sound", []byte("sound bytes"))
	if _, err := database.Pool.Exec(ctx, `update artifact_versions set content = $2 where id = $1`, corrupt, []byte("other bytes")); err != nil {
		t.Fatalf("corrupt the row: %v", err)
	}
	memory := filestest.NewMemory()
	report, err := files.BackfillRows(ctx, database.Pool, memory, &strings.Builder{})
	if err != nil {
		t.Fatalf("BackfillRows: %v", err)
	}
	if report.Done != 1 || len(report.Failed) != 1 || !strings.Contains(report.Failed[0], corrupt) || !strings.Contains(report.Failed[0], "hash to") {
		t.Fatalf("report = %+v, want the sound row moved and %s named", report, corrupt)
	}
	if rowBytes(t, database, corrupt) == nil || rowBytes(t, database, sound) != nil || memory.Len() != 1 {
		t.Fatal("want the corrupt row untouched and the sound row moved")
	}
}

// The schema lets a version's sha256 and mime be null. A row with no type is moved under the
// upload route's default; a row with no hash is named and keeps its bytes.
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
	if err != nil {
		t.Fatalf("BackfillRows: %v", err)
	}
	if report.Done != 1 || len(report.Failed) != 1 || !strings.Contains(report.Failed[0], unhashed) {
		t.Fatalf("report = %+v, want the untyped row moved and %s named", report, unhashed)
	}
	if rowBytes(t, database, untyped) != nil || rowBytes(t, database, unhashed) == nil {
		t.Fatal("want the untyped row moved and the unhashed row untouched")
	}
	if mime := memory.MIME(files.SHA256([]byte("untyped bytes"))); mime != "application/octet-stream" {
		t.Fatalf("the row with no type was stored as %q, want application/octet-stream", mime)
	}
}

// A row's bytes are its only copy until the store reads them back under their hash, so a store that
// acknowledged a write it did not keep must leave the row as it was.
func TestBackfillKeepsARowsBytesUntilTheStoreReadsThemBack(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	id := seedFileVersion(t, database, "kept", []byte("the only copy"))
	store := altering{Store: filestest.NewMemory(), sha: files.SHA256([]byte("the only copy"))}
	report, err := files.BackfillRows(ctx, database.Pool, store, &strings.Builder{})
	if err != nil {
		t.Fatalf("BackfillRows: %v", err)
	}
	if report.Done != 0 || len(report.Failed) != 1 || !strings.Contains(report.Failed[0], id) || !bytes.Equal(rowBytes(t, database, id), []byte("the only copy")) {
		t.Fatalf("a write the store did not keep cleared the row: report %+v", report)
	}
}

func TestVerifyNamesARowWhoseObjectIsMissingOrWrong(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedFileVersion(t, database, "kept", []byte("kept bytes"))
	lost := seedFileVersion(t, database, "lost", []byte("lost bytes"))
	altered := seedFileVersion(t, database, "altered", []byte("altered bytes"))
	memory := filestest.NewMemory()
	if report, err := files.BackfillRows(ctx, database.Pool, memory, &strings.Builder{}); err != nil || len(report.Failed) != 0 {
		t.Fatalf("BackfillRows: %+v, %v", report, err)
	}
	memory.Delete(files.SHA256([]byte("lost bytes")))

	var out strings.Builder
	report, err := files.VerifyRows(ctx, database.Pool, altering{Store: memory, sha: files.SHA256([]byte("altered bytes"))}, &out)
	if err != nil {
		t.Fatalf("VerifyRows: %v", err)
	}
	failed := strings.Join(report.Failed, "\n")
	if report.Done != 1 || len(report.Failed) != 2 || !strings.Contains(failed, lost) || !strings.Contains(failed, altered) {
		t.Fatalf("report = %+v, want one row checked and %s and %s failed", report, lost, altered)
	}
	for _, id := range []string{lost, altered} {
		if !strings.Contains(out.String(), "FAILED version "+id) {
			t.Errorf("output does not name failed version %s:\n%s", id, out.String())
		}
	}
}

// The rollback: a restore writes every moved row's bytes back from the store, verified, so a
// server with no store serves them from the row again; a row whose object is gone is named and
// left empty; the objects stay, and a later backfill moves the rows out once more.
func TestRestoreWritesTheBytesBackIntoEveryRowWhoseObjectReadsBack(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	kept := seedFileVersion(t, database, "kept", []byte("kept bytes"))
	lost := seedFileVersion(t, database, "lost", []byte("lost bytes"))
	memory := filestest.NewMemory()
	if report, err := files.BackfillRows(ctx, database.Pool, memory, &strings.Builder{}); err != nil || report.Done != 2 {
		t.Fatalf("BackfillRows: %+v, %v", report, err)
	}
	memory.Delete(files.SHA256([]byte("lost bytes")))

	var out strings.Builder
	report, err := files.RestoreRows(ctx, database.Pool, memory, &out)
	if err != nil {
		t.Fatalf("RestoreRows: %v", err)
	}
	if report.Done != 1 || report.Bytes != int64(len("kept bytes")) || len(report.Failed) != 1 || !strings.Contains(report.Failed[0], lost) {
		t.Fatalf("report = %+v, want %s restored and %s named", report, kept, lost)
	}
	if !bytes.Equal(rowBytes(t, database, kept), []byte("kept bytes")) || rowBytes(t, database, lost) != nil {
		t.Fatal("want the kept row's bytes back and the lost row still empty")
	}
	if !strings.Contains(out.String(), "restored version "+kept) {
		t.Errorf("output does not name the restored version:\n%s", out.String())
	}
	// A second restore has nothing to do for the kept row, and the object is still in the store,
	// so a backfill moves it out again.
	if report, err := files.RestoreRows(ctx, database.Pool, memory, &strings.Builder{}); err != nil || report.Done != 0 {
		t.Fatalf("second restore: %+v, %v", report, err)
	}
	if report, err := files.BackfillRows(ctx, database.Pool, memory, &strings.Builder{}); err != nil || report.Done != 1 || rowBytes(t, database, kept) != nil {
		t.Fatalf("backfill after a restore: %+v, %v", report, err)
	}
}

// Two passes started together do not work the same rows in lockstep: the second is refused while
func TestASecondPassIsRefusedWhileOneHoldsTheLock(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	seedFileVersion(t, database, "one", []byte("one"))
	memory := filestest.NewMemory()
	// Hold the lock the way a running pass does: on a connection kept for the duration, since an
	// advisory lock is the session's and a pooled query would hand its session back.
	holder, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection: %v", err)
	}
	var held bool
	if err := holder.QueryRow(ctx, `select pg_try_advisory_lock(x'4c4547494f4e3532'::bigint)`).Scan(&held); err != nil || !held {
		t.Fatalf("hold the lock: %v (%t)", err, held)
	}
	if _, err := files.BackfillRows(ctx, database.Pool, memory, &strings.Builder{}); !errors.Is(err, files.ErrAnotherPassRunning) {
		t.Fatalf("BackfillRows while the lock is held: %v, want ErrAnotherPassRunning", err)
	}
	if _, err := files.RestoreRows(ctx, database.Pool, memory, &strings.Builder{}); !errors.Is(err, files.ErrAnotherPassRunning) {
		t.Fatalf("RestoreRows while the lock is held: %v, want ErrAnotherPassRunning", err)
	}
	if _, err := holder.Exec(ctx, `select pg_advisory_unlock_all()`); err != nil {
		t.Fatalf("release the lock: %v", err)
	}
	holder.Release()
	if report, err := files.BackfillRows(ctx, database.Pool, memory, &strings.Builder{}); err != nil || report.Done != 1 {
		t.Fatalf("BackfillRows once the lock is released: %+v, %v", report, err)
	}
}
