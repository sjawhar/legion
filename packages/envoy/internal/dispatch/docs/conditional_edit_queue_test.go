package docs

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The cap on a document's queued conditional edits holds across envoy-dispatch processes (plan D9,
// migration 0137): each process writes how many it has admitted to doc_conditional_edit_queue,
// and an admission sums the counts of the member processes under the document's queue lock. The
// tests here run two services on one database, each on a pool of its own and each with its room
// sync running, so each is a member, as two envoy-dispatch processes are.

// newQueueProcesses is one document served by two member services.
func newQueueProcesses(t *testing.T) (database *store.Store, artifactID string, taskA, taskB *Service) {
	t.Helper()
	database = storetest.Open(t)
	artifactID = createDocument(t, database, "# First")
	taskA, _ = newTestServiceInstance(t, database)
	taskB, _ = newTestServiceInstance(t, storetest.SecondProcess(t, database))
	runRoomSync(t, newRoomSync(taskA))
	runRoomSync(t, newRoomSync(taskB))
	return database, artifactID, taskA, taskB
}

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
	database, artifactID, taskA, taskB := newQueueProcesses(t)
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
	database, artifactID, taskA, taskB := newQueueProcesses(t)
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
