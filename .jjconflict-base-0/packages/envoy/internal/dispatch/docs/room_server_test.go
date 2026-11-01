package docs

import (
	"context"
	"fmt"
	"testing"
	"time"

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

// closeGates counts the close gates service's server holds.
func closeGates(service *Service) int {
	service.srv.gatesMu.Lock()
	defer service.srv.gatesMu.Unlock()
	return len(service.srv.gates)
}
