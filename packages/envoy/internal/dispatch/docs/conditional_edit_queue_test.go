package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The cap on a document's queued conditional edits holds across envoy-dispatch processes (plan D9,
// migration 0150): each process writes how many it has admitted to doc_conditional_edit_queue,
// and an admission sums the counts of the member processes under the document's queue lock. The
// tests here run two services on one database, each on a pool of its own and each with its room
// sync running, so each is a member, as two envoy-dispatch processes are.

// queuedAcrossProcesses is the sum of every process's row of artifactID's queue.
func queuedAcrossProcesses(t *testing.T, database *store.Store, artifactID string) (queued, rows int) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(), `
		select coalesce(sum(queued), 0)::int, count(*)::int from doc_conditional_edit_queue where artifact_id = $1
	`, artifactID).Scan(&queued, &rows); err != nil {
		t.Fatalf("read the document's conditional edit queue: %v", err)
	}
	return queued, rows
}

// 32 conditional edits admitted across two processes fill the document's queue: the first holds
// task A's turn, and the other 31, alternating between task A and task B, are admitted and wait
// (task B's first holds its own turn). The 33rd is refused on either process, though neither
// process's own queue is near 32. Once every edit has left, no row remains.
func TestTheConditionalEditQueueIsCappedAcrossProcesses(t *testing.T) {
	pair := newProcessPair(t)
	pair.runRoomSync(t)
	database, artifactID, taskA, taskB := pair.database, pair.artifactID, pair.taskA, pair.taskB
	first, err := taskA.AcquireConditionalEdit(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("acquire the first conditional edit on task A: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type acquired struct {
		release func()
		err     error
	}
	results := make(chan acquired, maxQueuedConditionalEdits-1)
	for index := range maxQueuedConditionalEdits - 1 {
		service := taskA
		if index%2 == 0 {
			service = taskB
		}
		go func() {
			release, err := service.AcquireConditionalEdit(ctx, artifactID)
			results <- acquired{release: release, err: err}
		}()
	}
	var held []func()
	waitFor(t, roomWait(t), "32 conditional edits admitted across both processes", func() bool {
		select {
		case result := <-results:
			// Task B's first edit takes its turn at once; every other one waits for a turn.
			if result.err != nil {
				t.Fatalf("a conditional edit within the cap: %v", result.err)
			}
			held = append(held, result.release)
		default:
		}
		queued, _ := queuedAcrossProcesses(t, database, artifactID)
		return queued == maxQueuedConditionalEdits
	})
	if queued, rows := queuedAcrossProcesses(t, database, artifactID); queued != maxQueuedConditionalEdits || rows != 2 {
		t.Fatalf("the queue holds %d edits in %d rows, want %d in one row per process", queued, rows, maxQueuedConditionalEdits)
	}
	for name, service := range map[string]*Service{"task A": taskA, "task B": taskB} {
		if _, err := service.AcquireConditionalEdit(context.Background(), artifactID); !errors.Is(err, ErrPreconditionBusy) {
			t.Fatalf("the 33rd conditional edit, on %s = %v, want ErrPreconditionBusy", name, err)
		}
	}

	cancel()
	first()
	for _, release := range held {
		release()
	}
	for range maxQueuedConditionalEdits - 1 - len(held) {
		result := <-results
		if result.err == nil {
			// A waiter whose turn came as its context ended may take the turn.
			result.release()
		} else if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("a waiting conditional edit ended with %v, want its context's cancellation", result.err)
		}
	}
	if queued, rows := queuedAcrossProcesses(t, database, artifactID); queued != 0 || rows != 0 {
		t.Fatalf("after every edit left, the queue holds %d edits in %d rows, want none", queued, rows)
	}
	for name, service := range map[string]*Service{"task A": taskA, "task B": taskB} {
		if _, exists := service.conditionalGates.Load(artifactID); exists {
			t.Fatalf("%s kept the document's idle conditional edit gate", name)
		}
	}
}

// A row of a process that holds no room-sync connection - one that died, or whose listener is
// down - counts for nothing: a full queue written by such a process does not refuse an edit, and
// the admission deletes its row. A member's row counts.
func TestANonMembersConditionalEditsCountForNothingAndAreDeleted(t *testing.T) {
	pair := newProcessPair(t)
	pair.runRoomSync(t)
	database, artifactID, taskA, taskB := pair.database, pair.artifactID, pair.taskA, pair.taskB
	gone := uuid.NewString()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into doc_conditional_edit_queue (artifact_id, process_id, queued) values ($1, $2, $3), ($1, $4, $5)
	`, artifactID, gone, maxQueuedConditionalEdits, taskB.ProcessID(), maxQueuedConditionalEdits-1); err != nil {
		t.Fatalf("write the queue rows: %v", err)
	}
	release, err := taskA.AcquireConditionalEdit(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("a conditional edit beside a dead process's full queue and 31 of task B's = %v, want it admitted", err)
	}
	var left int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from doc_conditional_edit_queue where artifact_id = $1 and process_id = $2
	`, artifactID, gone).Scan(&left); err != nil {
		t.Fatalf("read the dead process's row: %v", err)
	}
	if left != 0 {
		t.Fatal("the admission left the dead process's row in place")
	}
	if _, err := taskA.AcquireConditionalEdit(context.Background(), artifactID); !errors.Is(err, ErrPreconditionBusy) {
		t.Fatalf("a conditional edit with task B's 31 and task A's 1 queued = %v, want ErrPreconditionBusy", err)
	}
	release()
	if queued, rows := queuedAcrossProcesses(t, database, artifactID); queued != maxQueuedConditionalEdits-1 || rows != 1 {
		t.Fatalf("after task A's edit left, the queue holds %d edits in %d rows, want task B's %d alone", queued, rows, maxQueuedConditionalEdits-1)
	}
}

// A retry storm on one document holds one of the process's shared connections, not one per
// request: while another session holds the document's queue lock, twenty admissions on one
// process wait for it through one connection, the rest waiting in memory, and an admission whose
// context ends while it waits there leaves. Once the lock is released every admission completes.
func TestConditionalEditAdmissionsOfADocumentWaitOnItsQueueLockThroughOneConnection(t *testing.T) {
	const storm = 20
	// pileUpWindow is how long the test leaves the admissions to pile onto the lock: far longer
	// than twenty goroutines take to reach Begin, and it is only the passing case that waits it out.
	const pileUpWindow = time.Second
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "# First")
	// The service's pool is its own, so its acquired connections are the admissions' alone.
	service, _ := newTestServiceInstance(t, storetest.SecondProcess(t, database))
	holder, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the queue lock's holder: %v", err)
	}
	defer holder.Rollback(context.Background())
	if _, err := holder.Exec(context.Background(), conditionalEditQueueLock, conditionalEditQueueLockNamespace, artifactID); err != nil {
		t.Fatalf("hold the document's conditional edit queue lock: %v", err)
	}

	type acquired struct {
		release func()
		err     error
	}
	results := make(chan acquired, storm)
	for range storm {
		go func() {
			release, err := service.AcquireConditionalEdit(context.Background(), artifactID)
			results <- acquired{release: release, err: err}
		}()
	}
	if waiting := storetest.WaitForLockWaiters(t, holder, 1, roomWait(t)); waiting < 1 {
		t.Fatal("no admission came to wait on the document's queue lock")
	}
	if waiting := storetest.WaitForLockWaiters(t, holder, storm, pileUpWindow); waiting != 1 {
		t.Fatalf("%d of the service's connections wait on the document's queue lock, want 1", waiting)
	}
	if inUse := service.store.Pool.Stat().AcquiredConns(); inUse != 1 {
		t.Fatalf("%d admissions hold a shared connection, want 1", inUse)
	}

	leaving, leave := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() {
		_, err := service.AcquireConditionalEdit(leaving, artifactID)
		left <- err
	}()
	leave()
	select {
	case err := <-left:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("an admission whose context ended while it waited = %v, want its context's cancellation", err)
		}
	case <-time.After(roomWait(t)):
		t.Fatal("an admission whose context ended stayed queued behind the document's queue lock")
	}

	if err := holder.Commit(context.Background()); err != nil {
		t.Fatalf("release the document's conditional edit queue lock: %v", err)
	}
	for range storm {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("a conditional edit within the cap: %v", result.err)
			}
			result.release()
		case <-time.After(roomWait(t)):
			t.Fatal("an admission did not complete once the document's queue lock was released")
		}
	}
	if queued, rows := queuedAcrossProcesses(t, database, artifactID); queued != 0 || rows != 0 {
		t.Fatalf("after every edit left, the queue holds %d edits in %d rows, want none", queued, rows)
	}
}
