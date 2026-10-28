package docs

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// readInvariant counts, per room, the settlements whose version reused the rendering of the tree
// they read and those that rendered their version again (requireReadUnchangedWhenReused).
// Settlements run on timer goroutines as well as the test's, so mu guards every field.
type readInvariant struct {
	mu       sync.Mutex
	read     map[string]readSnapshot
	reused   map[string]int
	rendered map[string]int
}

// readSnapshot is the tree a room's latest settlement read, as Node.JSON encoded it then.
type readSnapshot struct {
	encoded []byte
	err     error
}

// requireReadUnchangedWhenReused fails the test whenever a settlement of service versions the
// rendering it made of the tree it read while that tree has changed since the read. The version
// takes that rendering only when nothing changed the tree in place, so a settlement that changes
// it without recording the change (changedTree) would version a document the tree no longer
// holds. A tree Node.JSON cannot encode counts as changed. The returned readInvariant counts the
// settlements that reused the rendering and those that rendered again.
func requireReadUnchangedWhenReused(t *testing.T, service *Service) *readInvariant {
	t.Helper()
	invariant := &readInvariant{
		read:     map[string]readSnapshot{},
		reused:   map[string]int{},
		rendered: map[string]int{},
	}
	service.afterSettleTreeRead = func(room string, read *pmdoc.Node) {
		encoded, err := read.JSON()
		invariant.mu.Lock()
		defer invariant.mu.Unlock()
		invariant.read[room] = readSnapshot{encoded: encoded, err: err}
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
		if before.err != nil || err != nil {
			t.Errorf("a settlement of room %s reused the rendering of the tree it read, which does not encode (at its read: %v, at its version: %v)", room, before.err, err)
			return
		}
		if !bytes.Equal(before.encoded, after) {
			t.Errorf("a settlement of room %s reused the rendering of the tree it read, which changed after the read:\nread:    %s\nversion: %s", room, before.encoded, after)
		}
	}
	t.Cleanup(func() {
		service.afterSettleTreeRead = nil
		service.beforeSettleVersionRender = nil
	})
	return invariant
}

// reuses is the number of room's settlements whose version reused the rendering of their read.
func (i *readInvariant) reuses(room string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.reused[room]
}

// renders is the number of room's settlements that rendered their version again.
func (i *readInvariant) renders(room string) int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.rendered[room]
}

// A settlement that writes nothing into the room versions the rendering it made of the tree it
// read, and the tree is unchanged by every step it takes before that: its reconciliation of an ask
// block that matches its row, its listing of a recorded comment mark and of an unrecorded one the
// sweep tracks. The document holds each, and the settlement under test versions a plain text edit.
func TestSettlementReusesTheRenderOnlyOfAnUntouchedRead(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n\n- Yes: Monday.\n- No: Later.\n:::\n\nThe quick brown fox.\n")
	// The first settlement stamps the blocks' ids and indexes the ask block's row.
	settleCurrentGeneration(t, service, artifactID)
	if ask := blockAskOn(t, service, artifactID, "ask-block"); ask.State != "open" {
		t.Fatalf("the ask block's row is %q, want open", ask.State)
	}
	commentID := insertAnchoredComment(t, service, artifactID, "quick")
	browserMark(t, service, artifactID, "proofComment", "dangling", "brown")
	editAsPeer(t, service, artifactID, replaceRun("fox.", "dog."))
	tree := liveTree(t, service, artifactID)
	if repairs := pmdoc.BlockIDRepairCount(tree); repairs != 0 {
		t.Fatalf("the room has %d blocks to stamp, want none: a stamp would replace the read", repairs)
	}
	for _, id := range []string{commentID, "dangling"} {
		if _, _, found := pmdoc.FindMark(tree, "proofComment", id); !found {
			t.Fatalf("the room holds no comment mark %q", id)
		}
	}
	versionsBefore := latestVersionNumber(t, service, artifactID)

	invariant := requireReadUnchangedWhenReused(t, service)
	settleCurrentGeneration(t, service, artifactID)

	if versions := latestVersionNumber(t, service, artifactID); versions != versionsBefore+1 {
		t.Fatalf("latest version = %d, want %d: the settlement wrote no version of the edited document", versions, versionsBefore+1)
	}
	if reuses, renders := invariant.reuses(artifactID), invariant.renders(artifactID); reuses != 1 || renders != 0 {
		t.Fatalf("the settlement reused the rendering of its read %d times and rendered again %d times, want one reuse and no render", reuses, renders)
	}
}
