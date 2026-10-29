package docs

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// readInvariant counts, per room, the settlements whose version reused the rendering of the tree
// they read and those that rendered their version again (requireReadUnchangedWhenReused), and
// requireCounts checks those counts. read holds, per room, the tree its latest settlement read, as
// Node.JSON encoded it then.
// Settlements run on timer goroutines as well as the test's, so mu guards every field.
type readInvariant struct {
	mu       sync.Mutex
	read     map[string][]byte
	reused   map[string]int
	rendered map[string]int
}

// requireReadUnchangedWhenReused fails the test whenever a settlement of service versions the
// rendering it made of the tree it read while that tree has changed since the read. The version
// takes that rendering only when nothing changed the tree in place, so a settlement that changes
// it without recording the change (changedTree) would version a document the tree no longer
// holds. A tree Node.JSON cannot encode fails the test as well. The returned readInvariant counts
// the settlements that reused the rendering and those that rendered again, for requireCounts.
func requireReadUnchangedWhenReused(t *testing.T, service *Service) *readInvariant {
	t.Helper()
	invariant := &readInvariant{
		read:     map[string][]byte{},
		reused:   map[string]int{},
		rendered: map[string]int{},
	}
	service.afterSettleTreeRead = func(room string, read *pmdoc.Node) {
		encoded, err := read.JSON()
		invariant.mu.Lock()
		defer invariant.mu.Unlock()
		if err != nil {
			t.Errorf("a settlement of room %s read a tree that does not encode: %v", room, err)
			delete(invariant.read, room)
			return
		}
		invariant.read[room] = encoded
	}
	service.beforeSettleVersionRender = func(room string, read *pmdoc.Node, reused bool) {
		invariant.mu.Lock()
		defer invariant.mu.Unlock()
		if !reused {
			invariant.rendered[room]++
			return
		}
		invariant.reused[room]++
		before, found := invariant.read[room]
		if !found {
			t.Errorf("a settlement of room %s reused the rendering of a tree it was never seen reading", room)
			return
		}
		after, err := read.JSON()
		if err != nil {
			t.Errorf("a settlement of room %s reused the rendering of the tree it read, which does not encode at its version: %v", room, err)
			return
		}
		if !bytes.Equal(before, after) {
			t.Errorf("a settlement of room %s reused the rendering of the tree it read, which changed after the read:\nread:    %s\nversion: %s", room, before, after)
		}
	}
	t.Cleanup(func() {
		service.afterSettleTreeRead = nil
		service.beforeSettleVersionRender = nil
	})
	return invariant
}

// requireCounts fails the test unless exactly reuses of room's settlements reused the rendering of
// their read and exactly renders of them rendered their version again.
func (i *readInvariant) requireCounts(t *testing.T, room string, reuses, renders int) {
	t.Helper()
	i.mu.Lock()
	gotReuses, gotRenders := i.reused[room], i.rendered[room]
	i.mu.Unlock()
	if gotReuses != reuses || gotRenders != renders {
		t.Fatalf("the settlement reused the rendering of its read %d times and rendered again %d times, want %d reuses and %d renders", gotReuses, gotRenders, reuses, renders)
	}
}

// A settlement that writes nothing into the room versions the rendering it made of the tree it
// read, and the tree is unchanged by the step it takes before that: its reconciliation of an ask
// block that matches its row, which repairs nothing. The document holds that block, and the
// settlement under test versions a plain text edit.
func TestSettlementReusesTheRenderOnlyOfAnUntouchedRead(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n\n- Yes: Monday.\n- No: Later.\n:::\n\nThe quick brown fox.\n")
	// The first settlement stamps the blocks' ids and indexes the ask block's row.
	settleCurrentGeneration(t, service, artifactID)
	if ask := blockAskOn(t, service, artifactID, "ask-block"); ask.State != "open" {
		t.Fatalf("the ask block's row is %q, want open", ask.State)
	}
	editAsPeer(t, service, artifactID, replaceRun("fox.", "dog."))
	tree := liveTree(t, service, artifactID)
	if repairs := pmdoc.BlockIDRepairCount(tree); repairs != 0 {
		t.Fatalf("the room has %d blocks to stamp, want none: a stamp would replace the read", repairs)
	}
	versionsBefore := latestVersionNumber(t, service, artifactID)

	invariant := requireReadUnchangedWhenReused(t, service)
	settleCurrentGeneration(t, service, artifactID)

	if versions := latestVersionNumber(t, service, artifactID); versions != versionsBefore+1 {
		t.Fatalf("latest version = %d, want %d: the settlement wrote no version of the edited document", versions, versionsBefore+1)
	}
	invariant.requireCounts(t, artifactID, 1, 0)
}
