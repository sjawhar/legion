package docs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// cancelWrite is the whole of one API edit's write, run inside one request: the ops, the version
// the edit writes because the ops changed the document, and the commit, each on the request's
// context, as api's editArtifact runs them (artifacts.go:1034-1111 - s.begin, Docs.Join,
// ApplyOps, SnapshotVersion, Ledger.Commit, all given r.Context()). Only the rollback is taken
// off that context, as every rollback is: a transaction whose request has ended must still be
// ended on the connection. beforeCommit, where it is not nil, runs with the ops applied and the
// version written and before the commit, which is the window a request that ends in it must
// leave the document untouched through.
func cancelWrite(request context.Context, service *Service, artifactID, find, replacement string, beforeCommit func()) error {
	tx, err := service.store.Pool.Begin(request)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.Background())
	joined, ledger := service.Join(request, tx)
	defer ledger.Discard()
	if _, err := service.ApplyOps(joined, artifactID,
		[]model.EditOp{{Op: "replace", Find: find, With: replacement}}, grantWriter, nil); err != nil {
		return err
	}
	if _, err := service.SnapshotVersion(joined, artifactID, grantWriter); err != nil {
		return err
	}
	if beforeCommit != nil {
		beforeCommit()
	}
	return ledger.Commit(request)
}

// storedWrite is what a committed write leaves of a document in the database: the rows of its
// stored history and its versions. A write that did not commit leaves both exactly as it found
// them, and a write that committed once leaves one more of each.
type storedWrite struct{ updates, versions int }

func storedWriteOf(t *testing.T, service *Service, artifactID string) storedWrite {
	t.Helper()
	counted := storedWrite{updates: storedUpdateCount(t, service.store, artifactID)}
	if err := service.store.Pool.QueryRow(context.Background(),
		`select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&counted.versions); err != nil {
		t.Fatalf("count the document's versions: %v", err)
	}
	return counted
}

// heldCopyBound fills the whole copy-heap bound of artifactID's service, so the next live write
// of that document waits for its fork's grant (liveWrite.coverFork), and answers the release
// that frees it. The bound has room for two of this room's forks and all of it is held, which is
// the shape TestAnAPIWriteWhoseRequestEndsWhileItWaitsForTheCopyBoundIsRefusedAndChangesNothing
// holds the bound in.
func heldCopyBound(t *testing.T, service *Service, artifactID string) (heaps *weighedHeaps, free func()) {
	t.Helper()
	heaps = useCopyHeaps(t, newWeighedHeaps(copyHeapName, 2*grantRoomWeight(t, service, artifactID)))
	if !heaps.semaphore().TryAcquire(heaps.size) {
		t.Fatal("the empty bound refused all of itself")
	}
	return heaps, sync.OnceFunc(func() { heaps.semaphore().Release(heaps.size) })
}

// A write whose caller gave up before the write committed does not commit: the client that read
// a timeout of its own - the `dispatch` command cuts every call off at DISPATCH_TOOL_DEADLINE_MS,
// 60 seconds (packages/envoy-client/src/dispatch-http.ts:291-295, requestSignal, which aborts the
// fetch and so closes the connection net/http ends the request's context on) - must not have its
// edit land behind it, because the retry it makes next would then apply the same edit a second
// time. The write here waits for its fork's part of the copy-heap bound, which is where a write
// of a burst waits (liveWrite.coverFork), and its request ends there: the document holds the
// text, the stored history and the versions it held before, and the retry the caller makes once
// the bound frees applies the edit exactly once.
func TestAWriteWhoseRequestEndsWhileItWaitsForTheCopyBoundCommitsNothingAndItsRetryAppliesOnce(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.measurer.interval = time.Hour
	storeHistory(t, service, artifactID, grantTestState(t, 16<<10))
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	heaps, free := heldCopyBound(t, service, artifactID)
	defer free()
	before := storedWriteOf(t, service, artifactID)

	waiting := make(chan struct{})
	var waitOnce sync.Once
	heaps.waiting = func(int64) { waitOnce.Do(func() { close(waiting) }) }
	request, cancel := context.WithCancel(context.Background())
	refused := make(chan error, 1)
	go func() {
		refused <- cancelWrite(request, service, artifactID, grantWriteHeading, "Written", nil)
	}()
	select {
	case <-waiting:
	case err := <-refused:
		t.Fatalf("the write finished (%v) while the whole bound was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the write did not wait for the bound in 30 s")
	}
	cancel()
	var err error
	select {
	case err = <-refused:
	case <-time.After(30 * time.Second):
		t.Fatal("the write did not answer in 30 s once its request ended")
	}
	if err == nil {
		t.Fatal("the write committed after its request ended")
	}

	requireHeading(t, service, artifactID, true)
	if after := storedWriteOf(t, service, artifactID); after != before {
		t.Fatalf("the write that did not commit left %+v, want the %+v it found", after, before)
	}

	free()
	if err := cancelWrite(context.Background(), service, artifactID, grantWriteHeading, "Written", nil); err != nil {
		t.Fatalf("the caller's retry: %v", err)
	}
	requireHeading(t, service, artifactID, false)
	once := storedWrite{updates: before.updates + 1, versions: before.versions + 1}
	if after := storedWriteOf(t, service, artifactID); after != once {
		t.Fatalf("the retry left %+v, want %+v: the edit applied exactly once", after, once)
	}
}

// The same write, ended in the one window the bound's wait is not: its ops are applied and its
// version written, and its request ends before the commit. The transaction commits on the
// request's context, so it rolls back with the request and the document is as it was; the
// caller's retry applies the edit exactly once.
func TestAWriteWhoseRequestEndsBeforeItsCommitRollsBackAndItsRetryAppliesOnce(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	service.measurer.interval = time.Hour
	storeHistory(t, service, artifactID, grantTestState(t, 16<<10))
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	before := storedWriteOf(t, service, artifactID)

	request, cancel := context.WithCancel(context.Background())
	err := cancelWrite(request, service, artifactID, grantWriteHeading, "Written", cancel)
	if err == nil {
		t.Fatal("the write committed after its request ended")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("the write answered %v, want the end of its request", err)
	}

	requireHeading(t, service, artifactID, true)
	if after := storedWriteOf(t, service, artifactID); after != before {
		t.Fatalf("the write that did not commit left %+v, want the %+v it found", after, before)
	}

	if err := cancelWrite(context.Background(), service, artifactID, grantWriteHeading, "Written", nil); err != nil {
		t.Fatalf("the caller's retry: %v", err)
	}
	requireHeading(t, service, artifactID, false)
	once := storedWrite{updates: before.updates + 1, versions: before.versions + 1}
	if after := storedWriteOf(t, service, artifactID); after != once {
		t.Fatalf("the retry left %+v, want %+v: the edit applied exactly once", after, once)
	}
}
