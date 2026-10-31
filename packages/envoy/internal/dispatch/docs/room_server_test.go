package docs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A room's close gate leaves with the last close or repair that held it: the server keeps none
// for a room nothing is closing or repairing, however many documents were repaired, evicted, or
// closed with their issue without ever loading.
func TestCloseGatesDoNotOutliveTheirRooms(t *testing.T) {
	service, first := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	generation := owedStamp(t, service, first)
	service.settleRoom(first, generation) // a repair (applySuppressed)
	if err := service.srv.CloseRoom(first, true); err != nil {
		t.Fatalf("close the settled room: %v", err)
	}
	var evicted []string
	for number := 2; number <= 26; number++ { // rooms the service evicts
		room := createIssueDocument(t, service.store, number, "before")
		seedServiceText(t, service, room, "before")
		liveTree(t, service, room)
		if service.srv.GetDoc(room) == nil {
			t.Fatalf("document %d is not resident after its live read", number)
		}
		if err := service.Evict(ctx, room); err != nil {
			t.Fatalf("evict document %d: %v", number, err)
		}
		evicted = append(evicted, room)
	}
	for number := 27; number <= 51; number++ { // closed issues, documents never loaded
		createIssueDocument(t, service.store, number, "before")
		service.SetIssueClosed(ctx, fmt.Sprintf("DOC-%d", number), true)
	}
	live := 0
	for _, room := range append([]string{first}, evicted...) {
		if service.srv.GetDoc(room) != nil {
			live++
		}
	}
	if gates := closeGates(service); gates > live {
		t.Fatalf("%d close gates kept for %d live rooms after 51 documents were repaired, evicted or closed", gates, live)
	}
}

// A repair that meets a close of its room under way writes nothing and returns rather than waiting
// for the close: the close waits for repairs already committing, and a settlement writing a repair
// holds the document's lock, which a failed room's close compacts under (roomServer).
func TestARepairRefusesRatherThanWaitsForACloseUnderWay(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	generation := owedStamp(t, service, artifactID)

	closing := service.srv.acquireGate(artifactID)
	closing.Lock()
	released := false
	release := func() {
		if !released {
			released = true
			closing.Unlock()
			service.srv.releaseGate(artifactID, closing)
		}
	}
	t.Cleanup(release)
	settled := make(chan struct{})
	go func() {
		defer close(settled)
		service.settleRoom(artifactID, generation)
	}()
	select {
	case <-settled:
	case <-time.After(10 * time.Second):
		release()
		t.Fatal("the settlement waited for the close under way instead of refusing its repair")
	}
	release()
	if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs == 0 {
		t.Fatal("the refused repair stamped the room")
	}
	requireNoSuppressedSlots(t, service, artifactID, "after the refused repair")
	requireNextSettlementStamps(t, service, artifactID, "before\n\nadded\n")
	if gates := closeGates(service); gates != 0 {
		t.Fatalf("%d close gates kept once nothing closes or repairs the room", gates)
	}
}

// A repair holds the exact document Server.Apply found. Once its room has retired that document,
// holdOpen refuses and releases the gate it acquired for the check.
func TestHoldOpenRefusesRoomThatNoLongerHoldsItsDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	owedStamp(t, service, artifactID)
	doc := service.srv.GetDoc(artifactID)
	if doc == nil {
		t.Fatal("document room is not resident")
	}
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close document room: %v", err)
	}
	if release, open := service.srv.holdOpen(artifactID, doc); open {
		release()
		t.Fatal("a repair was held open on a document its room no longer holds")
	}
	if gates := closeGates(service); gates != 0 {
		t.Fatalf("%d close gates kept after the refusal", gates)
	}
}

// The room budget's sweep leaves a room an Apply is inside its callback on, and closes it once
// that Apply has returned. ygo's Apply holds the room's in-flight count "to protect rm from a
// concurrent eviction while in use" and its idle sweep honours that count, but its CloseRoom does
// not (sjawhar/ygo v1.51.3-sami.7, provider/websocket/inject.go:334-337 and :495-646): a close
// takes the room out of the server's map whatever is inside an Apply on it, and the apply's write
// then lands on a room the server no longer holds, which the next read does not load. A decision
// whose body a browser broke would then read as the body it replaced, and an ask edit of it would
// be made on that body rather than refused.
func TestTheBudgetSweepLeavesARoomAnApplyIsWriting(t *testing.T) {
	service, _ := newTestService(t)
	service.settle = time.Hour
	ctx := context.Background()
	artifactID := createIssueDocument(t, service.store, 2, "# First")
	seedServiceText(t, service, artifactID, ":::ask{#ask-lists urgency=\"med\" multiple=\"false\" state=\"open\"}\nShould we ship?\n\n- Ship: Release it\n- Hold: Wait for review\n:::\n")
	settleCurrentGeneration(t, service, artifactID)
	// Every charged room is past a budget of one byte, so the sweep closes whatever it may.
	service.SetRoomBudgetForTest(1)

	// A decision body the content rule refuses: a list where the question's paragraph must be.
	broken := []*pmdoc.Node{askBodyBlock(t, "L", 1), askBodyBlock(t, "P", 2)}
	err := service.srv.Apply(ctx, artifactID, func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
		fragment := doc.GetXmlFragment(fragmentName)
		tree, readErr := pmdoc.Read(fragment)
		if readErr != nil {
			t.Fatalf("read the live tree: %v", readErr)
		}
		firstAsk(tree).Children = broken
		// The sweep runs on a goroutine of its own (roomBudget.startSweepLocked); run here, it
		// meets this apply exactly as that goroutine can.
		service.sweepRooms()
		if service.srv.GetDoc(artifactID) == nil {
			t.Fatal("the budget's sweep closed the room while an apply was inside its callback on it")
		}
		transact(func(txn *crdt.Transaction) {
			if updateErr := pmdoc.Update(txn, fragment, tree); updateErr != nil {
				t.Errorf("write the broken decision body: %v", updateErr)
			}
		})
	})
	if err != nil {
		t.Fatalf("apply the browser's edit: %v", err)
	}
	// Nothing of the write is lost to the sweep that gave way: the room keeps it, the room's
	// persistence takes it, and the close the budget makes once the room is unused flushes it.
	settleCurrentGeneration(t, service, artifactID)
	if closed, err := service.evictUnusedRoom(artifactID, service.invalidateForEviction(artifactID)); !closed || err != nil {
		t.Fatalf("the budget's close of the room once the apply had returned = %t, %v, want it closed", closed, err)
	}

	// The room the next read loads holds what the apply wrote, so the decision still breaks its
	// content rule and the ask edit of it is refused rather than made on the body it replaced.
	if kinds := len(firstAsk(liveTree(t, service, artifactID)).Children); kinds != len(broken) {
		t.Fatalf("the reloaded decision holds %d blocks, want the %d the apply wrote", kinds, len(broken))
	}
	var unrepresentable *ErrAskBlockUnrepresentable
	if askErr := sendAskOptions(service, artifactID, AskBlockEdit{Urgency: new("high")}); !errors.As(askErr, &unrepresentable) {
		t.Fatalf("ask edit of the broken body = %v, want ErrAskBlockUnrepresentable", askErr)
	}
}

// The rebuild's close of the room whose history it replaces gives way to a Server.Apply inside
// its callback on the room, and closes it once that Apply has returned (evictBeforeRebuild). A
// forced close would take the room out of the server's map under that apply, whose commit then
// reaches the store only through the room's retiring persistence worker: landing before
// RebuildTx, it is deleted with the history the rebuild replaces; landing after it, it puts the
// replaced history's structs on top of the rebuilt seed. A refused close leaves the room's record
// as it found it, its armed settlement included, since the rebuild it refuses goes no further.
func TestTheRebuildsCloseLeavesARoomAnApplyIsWriting(t *testing.T) {
	service, _ := newTestService(t)
	ctx := context.Background()
	artifactID := createIssueDocument(t, service.store, 2, "# First")
	seedServiceText(t, service, artifactID, "before\n")
	settleCurrentGeneration(t, service, artifactID)

	generation := roomGeneration(t, service, artifactID)
	err := service.srv.Apply(ctx, artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {
		closed, closeErr := service.evictBeforeRebuild(artifactID)
		if closed || closeErr != nil {
			t.Errorf("the rebuild's close while an apply was inside its callback = %t, %v, want it refused", closed, closeErr)
		}
		if service.srv.GetDoc(artifactID) == nil {
			t.Error("the rebuild's close closed the room while an apply was inside its callback on it")
		}
	})
	if err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		t.Fatalf("apply on the room: %v", err)
	}
	if armed := roomGeneration(t, service, artifactID); armed != generation {
		t.Fatalf("the refused close moved the room's generation from %d to %d, want the record left as it was", generation, armed)
	}
	if closed, err := service.evictBeforeRebuild(artifactID); !closed || err != nil {
		t.Fatalf("the rebuild's close once the apply had returned = %t, %v, want it closed", closed, err)
	}
	if service.srv.GetDoc(artifactID) != nil {
		t.Fatal("the rebuild's close left the room live")
	}
}

// closeGates counts the close gates service's server holds.
func closeGates(service *Service) int {
	service.srv.gatesMu.Lock()
	defer service.srv.gatesMu.Unlock()
	return len(service.srv.gates)
}
