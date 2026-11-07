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

// heldLoadSlots gives the process a load concurrency of one slot and holds it, so the next live
// write waits for its fork's slot (liveWrite.coverFork), and answers the release that frees it.
// It is the shape TestAnAPIWriteWhoseRequestEndsWhileItWaitsForASlotIsRefusedAndChangesNothing
// holds the slots in.
func heldLoadSlots(t *testing.T) (slots *loadSlots, free func()) {
	t.Helper()
	slots = useLoadSlots(t, newLoadSlots(1))
	holder := newLoadSlot()
	if !holder.tryTake() {
		t.Fatal("the empty concurrency refused its only slot")
	}
	return slots, sync.OnceFunc(holder.release)
}

// A write whose caller gave up before the write committed does not commit: the client that read
// a timeout of its own - the `dispatch` command cuts every call off at DISPATCH_TOOL_DEADLINE_MS,
// 60 seconds (packages/envoy-client/src/dispatch-http.ts:291-295, requestSignal, which aborts the
// fetch and so closes the connection net/http ends the request's context on) - must not have its
// edit land behind it, because the retry it makes next would then apply the same edit a second
// time. The write here waits for its fork's slot of the pod's load concurrency, which is where a
// write of a burst waits (liveWrite.coverFork), and its request ends there: the document holds
// the text, the stored history and the versions it held before, and the retry the caller makes
// once a slot is free applies the edit exactly once.
func TestAWriteWhoseRequestEndsWhileItWaitsForASlotCommitsNothingAndItsRetryAppliesOnce(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	storeHistory(t, service, artifactID, grantTestState(t, 16<<10))
	if err := loadRoomOf(service, artifactID); err != nil {
		t.Fatalf("load the room: %v", err)
	}
	slots, free := heldLoadSlots(t)
	defer free()
	before := storedWriteOf(t, service, artifactID)

	waiting := make(chan struct{})
	var waitOnce sync.Once
	slots.waiting = func() { waitOnce.Do(func() { close(waiting) }) }
	request, cancel := context.WithCancel(context.Background())
	refused := make(chan error, 1)
	go func() {
		refused <- cancelWrite(request, service, artifactID, grantWriteHeading, "Written", nil)
	}()
	select {
	case <-waiting:
	case err := <-refused:
		t.Fatalf("the write finished (%v) while the only slot was held", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the write did not wait for a slot in 30 s")
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

// The same write, ended in the one window the slot's wait is not: its ops are applied and its
// version written, and its request ends before the commit. The transaction commits on the
// request's context, so it rolls back with the request and the document is as it was; the
// caller's retry applies the edit exactly once.
func TestAWriteWhoseRequestEndsBeforeItsCommitRollsBackAndItsRetryAppliesOnce(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
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
