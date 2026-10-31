package docs

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A read of a room whose replica holds no document copies the room and hands the copy to the
// replica to start from (holdLive, startFromRead), so a room that is read and not written is
// copied once rather than once a read. The copy stands for the room only while the room has
// committed nothing the replica has yet to take, and the replica's own state is what says so:
// no document and an empty log.
//
// A recover leaves exactly that state and does not mean it. Here a commit lands between the
// copy and the hand-over, the replica cannot place it, and recover empties the log and drops
// the document (renderedReplica.recover): installed then, the copy is the room without that
// commit, and every later read of the room answers from it - the room's next commit records a
// fresh base the observer passes over, since the replica holds a document again, and the
// updates built on the lost commit sit in the copy's pending set for as long as the room
// stands. The replica must refuse a copy taken before a recover it did not see.
func TestAReadStartsNoReplicaBehindACommitARecoverTookAway(t *testing.T) {
	bob := model.Actor{Kind: "user", ID: "bob"}
	carol := model.Actor{Kind: "user", ID: "carol"}
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n")
	settleCurrentGeneration(t, service, artifactID)
	// A browser edit the room's observer classifies starts the replica from the log's base, so
	// the read below meets a replica that has run.
	editAsConnectedPeer(t, service, artifactID, 1, bob, appendBlocks(t, "Warm.\n"))
	waitForLandedAppends(t, service, artifactID)
	live := service.srv.GetDoc(artifactID)
	replica := service.replicaOf(live)
	if replica == nil || replica.doc == nil {
		t.Fatal("the warm-up edit left no started replica, so nothing here is tested")
	}

	// A held observer leaves its own update in the log, which is how a read comes to copy the
	// room rather than hold the replica (hold).
	release := holdPeerEdit(t, service, artifactID, &service.beforeObserveUpdate, appendBlocks(t, "Held.\n"))
	if replica.log.Pending() == 0 {
		t.Fatal("the held edit left nothing in the room's update log, so the read below would hold the replica")
	}

	const lost = "A paragraph the read must not lose."
	var (
		once          sync.Once
		epochAtCopy   uint64
		epochAfterLog uint64
	)
	service.afterReadCopy = func(room string) {
		if room != artifactID {
			return
		}
		once.Do(func() {
			epochAtCopy = replica.epoch.Load()
			// What a replica that cannot place the room's next commit holds: an apply that
			// failed leaves no document (start, observe).
			replica.mu.Lock()
			replica.doc = nil
			replica.mu.Unlock()
			// The commit the copy was taken before. Its observer drains the log, finds no
			// document to apply it to and recovers: the log is reset and the replica starts
			// again at a fresh epoch, which leaves it holding no document and no pending
			// update - the state the copy is taken for.
			editAsPeer(t, service, artifactID, appendBlocks(t, lost+"\n"))
			epochAfterLog = replica.epoch.Load()
		})
	}
	// The read itself answers from its copy, which is the room as of the moment it was taken;
	// what it must not do is leave that copy behind as the room's replica.
	if _, err := service.liveTree(artifactID, live); err != nil {
		t.Fatalf("read the room: %v", err)
	}
	service.afterReadCopy = nil
	if epochAfterLog == epochAtCopy {
		t.Fatalf("the replica never recovered under the read (epoch %d throughout), so nothing here is tested", epochAtCopy)
	}
	if replica.log.Pending() != 0 {
		t.Fatal("the recover left updates in the log, so the read's copy was refused for the log alone and nothing here is tested")
	}
	release()

	if markdown := currentLiveMarkdown(t, service, artifactID); !strings.Contains(markdown, lost) {
		t.Errorf("a read of the room answers\n%s\nwhich does not hold %q, the paragraph committed while the read's copy was in flight: the copy was installed as the room's replica and every read of the room answers from it until the room unloads",
			markdown, lost)
	}
	// The room's own document holds the commit either way: what the replica answers is the only
	// thing at stake.
	editAsConnectedPeer(t, service, artifactID, 2, carol, appendBlocks(t, "After.\n"))
	waitForLandedAppends(t, service, artifactID)
	markdown := currentLiveMarkdown(t, service, artifactID)
	if !strings.Contains(markdown, lost) || !strings.Contains(markdown, "After.") {
		t.Errorf("after a further commit a read of the room answers\n%s\nwhich does not hold both %q and %q", markdown, lost, "After.")
	}
}
