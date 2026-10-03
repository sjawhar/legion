package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// Settlement repairs an ask block's server-owned attributes into the document as it stands when
// the repair is written, not into the tree settlement read before its database work: a paragraph a
// browser edits in between keeps the edit, and the stored document and the version the settlement
// writes hold both (LEGION-479).
func TestSettlementRepairKeepsAnEditMadeAfterItsRead(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)

	var edited atomic.Bool
	service.afterSettleReconcile = func(room string) {
		if room == artifactID && edited.CompareAndSwap(false, true) {
			editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if !edited.Load() {
		t.Fatal("settlement never reached the window between its read and its repair")
	}

	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext after.\n"
	waitForDocumentText(t, service, artifactID, want)
	waitForPersistedProofText(t, service.store, artifactID, want)
	requireLatestVersionMarkdown(t, service, artifactID, want)
}

// The same race on the block-id stamp: a settlement that stamped ids and then sees a peer's edit
// before it versions writes the version of the document as it stands, the edit included, rather
// than of the tree its stamp read (LEGION-479).
func TestSettlementStampVersionKeepsAnEditMadeAfterItsRead(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))

	var edited atomic.Bool
	service.afterSettleReconcile = func(room string) {
		if room == artifactID && edited.CompareAndSwap(false, true) {
			editAsPeer(t, service, artifactID, replaceRun("before", "before, edited"))
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if !edited.Load() {
		t.Fatal("settlement never reached the window between its stamp and its version")
	}

	const want = "before, edited\n\nadded\n"
	waitForDocumentText(t, service, artifactID, want)
	requireNoSuppressedSlots(t, service, artifactID, "after the stamp")
	waitForPersistedProofText(t, service.store, artifactID, want)
	requireLatestVersionMarkdown(t, service, artifactID, want)
}

// A settlement whose room is replaced while it does its database work - the room's last browser
// left, and its repair's write loaded the room again from the store - writes nothing into the
// replacement: its version is read from the document it held, which would never get the repair.
// The settlement the replacement's load arms versions the document with the repair (LEGION-479).
func TestSettlementWhoseRoomWasReplacedLeavesTheDocumentToTheReplacement(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext before.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)
	editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))

	held := service.srv.GetDoc(artifactID)
	var replaced atomic.Bool
	service.afterSettleReconcile = func(room string) {
		if room != artifactID || !replaced.CompareAndSwap(false, true) {
			return
		}
		// ygo closes a room the moment its last browser leaves, on that browser's goroutine.
		go func() { _ = service.srv.CloseRoom(room, true) }()
		deadline := time.Now().Add(5 * time.Second)
		for service.srv.GetDoc(room) == held {
			if time.Now().After(deadline) {
				t.Error("the room was never closed")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if !replaced.Load() {
		t.Fatal("settlement never reached the window between its read and its repair")
	}
	service.afterSettleReconcile = nil
	settleCurrentGeneration(t, service, artifactID)

	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n:::\n\nContext after.\n"
	waitForDocumentText(t, service, artifactID, want)
	waitForPersistedProofText(t, service.store, artifactID, want)
	requireLatestVersionMarkdown(t, service, artifactID, want)
}

// requireLatestVersionMarkdown requires the document's latest version to hold want.
func requireLatestVersionMarkdown(t *testing.T, service *Service, artifactID, want string) {
	t.Helper()
	var markdown string
	if err := service.store.Pool.QueryRow(context.Background(), `
		select markdown from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&markdown); err != nil {
		t.Fatalf("read the latest document version: %v", err)
	}
	if markdown != want {
		t.Fatalf("latest version markdown = %q, want %q", markdown, want)
	}
}

// A browser that deletes the ask block settlement is repairing, between the reconciliation and its
// write, keeps the deletion: the repair finds no block to write into, so the settlement wrote nothing
// into the room and leaves the moved document, versions included, to the settlement the deletion
// scheduled. The repair's transaction, which changed nothing, still reports an update; its slot is
// finished with that update and the room's persistence worker consumes it (LEGION-479).
func TestSettlementRepairOfAnAskAPeerDeletedWritesNothing(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n\nContext.\n")
	service.settleRoom(artifactID, 0)
	answerBlockAsk(t, service, artifactID)
	versionsBefore := latestVersionNumber(t, service, artifactID)

	var deleted atomic.Bool
	service.afterSettleReconcile = func(room string) {
		if room == artifactID && deleted.CompareAndSwap(false, true) {
			editAsPeer(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				tree.Children = tree.Children[1:]
				return tree
			})
		}
	}
	settleCurrentGeneration(t, service, artifactID)
	if !deleted.Load() {
		t.Fatal("settlement never reached the window between its read and its repair")
	}

	waitForDocumentText(t, service, artifactID, "Context.\n")
	requireNoSuppressedSlots(t, service, artifactID, "after the repair")
	waitForPersistedProofText(t, service.store, artifactID, "Context.\n")
	if versions := latestVersionNumber(t, service, artifactID); versions != versionsBefore {
		t.Fatalf("latest version = %d, want %d: the settlement versioned the ask block the peer deleted", versions, versionsBefore)
	}
}

func latestVersionNumber(t *testing.T, service *Service, artifactID string) int {
	t.Helper()
	var latest int
	if err := service.store.Pool.QueryRow(context.Background(), `
		select coalesce(max(number), 0) from artifact_versions where artifact_id = $1
	`, artifactID).Scan(&latest); err != nil {
		t.Fatalf("read the latest document version: %v", err)
	}
	return latest
}

// A settlement takes a suppression slot before it stamps block ids, and the stamp it then writes
// can find the ids already repaired - here by another writer between settlement's read and its
// write. That slot is released every time: one nothing finishes or discards holds the room's
// persistence worker at the next update it is handed, and every later update of the room with it
// (LEGION-479).
func TestSettlementReleasesTheSlotOfAStampTheDocumentNoLongerNeeds(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")
	settleCurrentGeneration(t, service, artifactID)

	var stampFirst atomic.Bool
	service.afterSettleRead = func(room string) {
		if room == artifactID && stampFirst.CompareAndSwap(true, false) {
			// Another writer identifies the block first, as an agent's edit stamps every block
			// it addresses.
			editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
				pmdoc.EnsureBlockIDs(tree)
				return tree
			})
		}
	}
	want := "before\n"
	for round := range 5 {
		paragraph := fmt.Sprintf("round %d", round)
		editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, paragraph))
		want += "\n" + paragraph + "\n"
		stampFirst.Store(true)
		settleCurrentGeneration(t, service, artifactID)
		if stampFirst.Load() {
			t.Fatalf("round %d: settlement never reached the window between its read and its stamp", round)
		}
		requireNoSuppressedSlots(t, service, artifactID, fmt.Sprintf("round %d", round))
	}
	waitForPersistedProofText(t, service.store, artifactID, want)
}

// A settlement that both stamps block ids and repairs an ask block writes two updates into the
// room, and the room's persistence worker is handed each on its own. Both are held from it and
// released once each reaches it (LEGION-479).
func TestSettlementThatStampsAndRepairsReleasesItsSlots(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n:::\n")
	service.settleRoom(artifactID, 0)
	for round := range 3 {
		answerBlockAsk(t, service, artifactID)
		paragraph := fmt.Sprintf("round %d", round)
		editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
			tree.Children[0].Attrs["state"] = "open"
			return appendUnidentifiedBlocks(t, paragraph)(tree)
		})
		settleCurrentGeneration(t, service, artifactID)
		if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs != 0 {
			t.Fatalf("round %d: settlement left %d unstamped blocks, so it never stamped", round, repairs)
		}
		if state := liveTree(t, service, artifactID).Children[0].Attrs["state"]; state != "answered" {
			t.Fatalf("round %d: ask block state = %v, so settlement never repaired it", round, state)
		}
		requireNoSuppressedSlots(t, service, artifactID, fmt.Sprintf("round %d", round))
		reopenBlockAsk(t, service, artifactID)
	}
}

// answerBlockAsk answers the document's one indexed ask in its row alone, so the block disagrees
// with it until settlement repairs the block.
func answerBlockAsk(t *testing.T, service *Service, artifactID string) model.AskAnswer {
	t.Helper()
	answer := model.AskAnswer{User: "alice", Selected: []string{}, At: time.Now().UTC()}
	answerJSON, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode answer: %v", err)
	}
	tag, err := service.store.Pool.Exec(context.Background(), `
		update asks set state = 'answered', answer = $2 where block_artifact_id = $1
	`, artifactID, answerJSON)
	if err != nil {
		t.Fatalf("answer indexed ask: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("answered %d asks, want the document's one", tag.RowsAffected())
	}
	return answer
}

// reopenBlockAsk takes the answer back off the document's indexed ask, so the next round can
// answer it again.
func reopenBlockAsk(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if _, err := service.store.Pool.Exec(context.Background(), `
		update asks set state = 'open', answer = null where block_artifact_id = $1
	`, artifactID); err != nil {
		t.Fatalf("reopen indexed ask: %v", err)
	}
}

// appendUnidentifiedBlocks returns a live edit that appends the blocks parsed from markdown
// without block ids, as a write that predates them leaves a block.
func appendUnidentifiedBlocks(t *testing.T, markdown string) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	parsed, err := pmdoc.Parse(markdown)
	if err != nil {
		t.Fatalf("parse appended blocks: %v", err)
	}
	removeBlockIDs(parsed)
	return func(tree *pmdoc.Node) *pmdoc.Node {
		tree.Children = append(tree.Children, parsed.Children...)
		return tree
	}
}

// requireNoSuppressedSlots waits for the room's persistence worker to consume every suppression
// slot queued for room. A slot still queued at the deadline is released before the test fails,
// so the worker it holds lets the test's shutdown finish.
func requireNoSuppressedSlots(t *testing.T, service *Service, room, when string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		service.suppressMu.Lock()
		queued := len(service.suppressed[room])
		service.suppressMu.Unlock()
		if queued == 0 {
			return
		}
		if time.Now().After(deadline) {
			service.purgeSuppressedPersistence(room)
			t.Fatalf("%s: %d suppression slots still queued for the room", when, queued)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// editAsPeer writes a tree change as a browser's edit reaches the room: made in a document of its
// own, with a client id of its own, synced from the room and applied to it as a peer's update.
func editAsPeer(t *testing.T, service *Service, artifactID string, edit func(*pmdoc.Node) *pmdoc.Node) {
	t.Helper()
	room := service.srv.GetDoc(artifactID)
	if room == nil {
		t.Fatal("document room is not resident")
	}
	peer := crdt.New()
	if err := crdt.ApplyUpdateV1(peer, crdt.EncodeStateAsUpdateV1(room, nil), nil); err != nil {
		t.Fatalf("sync peer from the room: %v", err)
	}
	synced := peer.StateVector()
	tree, err := treeOf(peer)
	if err != nil {
		t.Fatalf("read peer document: %v", err)
	}
	fragment := peer.GetXmlFragment(fragmentName)
	peer.Transact(func(txn *crdt.Transaction) {
		if err := pmdoc.Update(txn, fragment, edit(tree)); err != nil {
			t.Errorf("edit peer document: %v", err)
		}
	})
	if err := crdt.ApplyUpdateV1(room, crdt.EncodeStateAsUpdateV1(peer, synced), "peer"); err != nil {
		t.Fatalf("apply the peer's edit to the room: %v", err)
	}
}
