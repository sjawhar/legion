package docs

import (
	"context"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// Room sync runs each handler on a lane of its own, at the same time as the others (roomSyncLane).
// What one lane does on the pool must not refuse another's: a catch-up holding a pooled connection
// - a transaction, an open cursor - is no reason for the issue-open lane's read of an issue to be
// taken for a second connection of the same caller. Refused, that read loses the close or reopen
// it was handling, so the process's rooms keep the scope the issue no longer has: a reader keeps
// typing into a closed issue, or stays read-only on a reopened one.
//
// Here the catch-up lane holds a transaction while the issue closes. The close must still reach
// the room, and alice's browser must be closed.
func TestAnIssueCloseReachesTheRoomsWhileAnotherRoomSyncLaneHoldsAConnection(t *testing.T) {
	database := storetest.Open(t)
	artifactID := createDocument(t, database, "before")
	task, server := newTestServiceInstance(t, database)
	seedServiceText(t, task, artifactID, "before")
	waitForLandedAppends(t, task, artifactID)
	runRoomSync(t, newRoomSync(task))
	alice := connectWithScope(t, server.URL, artifactID, "alice", "read-write", "before\n")

	task.catchUps.mu.Lock()
	lane := task.catchUps.lane
	task.catchUps.mu.Unlock()
	if lane == nil {
		t.Fatal("room sync gave the catch-up no lane")
	}
	holding := make(chan struct{})
	release := make(chan struct{})
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	t.Cleanup(releaseOnce)
	lane.push(func(ctx context.Context) {
		tx, err := task.store.Pool.Begin(ctx)
		if err != nil {
			t.Errorf("the catch-up lane's transaction: %v", err)
			close(holding)
			return
		}
		close(holding)
		<-release
		_ = tx.Rollback(context.Background())
	})
	<-holding

	writeIssueClosed(t, task.store, "DOC-1", true)
	requireEnds(t, alice, "closing the issue while room sync's catch-up lane held a pooled connection left alice's browser connected")
	releaseOnce()
}
