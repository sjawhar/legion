package docs

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// readInvariant counts, per room, the settlement passes that reached their version, each of which
// takes the rendering the pass made of the tree it read before its transaction
// (requireReadUnchangedAtVersion), and requireVersions checks that count. read holds, per room, the
// tree its latest settlement pass read, as Node.JSON encoded it then.
// Settlements run on timer goroutines as well as the test's, so mu guards every field.
type readInvariant struct {
	mu       sync.Mutex
	read     map[string][]byte
	versions map[string]int
}

// requireReadUnchangedAtVersion fails the test whenever a settlement pass of service reaches its
// version while the tree it read has changed since the read. The version takes the rendering the
// pass made of that tree before its transaction, so a pass that changes the tree in place and
// still versions it would version a document the rendering does not hold. A tree Node.JSON
// cannot encode fails the test as well. The returned readInvariant counts the passes that reached
// their version, for requireVersions.
func requireReadUnchangedAtVersion(t *testing.T, service *Service) *readInvariant {
	t.Helper()
	invariant := &readInvariant{
		read:     map[string][]byte{},
		versions: map[string]int{},
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
	service.beforeSettleVersion = func(room string, read *pmdoc.Node) {
		invariant.mu.Lock()
		defer invariant.mu.Unlock()
		invariant.versions[room]++
		before, found := invariant.read[room]
		if !found {
			t.Errorf("a settlement of room %s reached its version of a tree it was never seen reading", room)
			return
		}
		after, err := read.JSON()
		if err != nil {
			t.Errorf("a settlement of room %s reached its version of the tree it read, which does not encode at its version: %v", room, err)
			return
		}
		if !bytes.Equal(before, after) {
			t.Errorf("a settlement of room %s versions the rendering of the tree it read, which changed after the read:\nread:    %s\nversion: %s", room, before, after)
		}
	}
	t.Cleanup(func() {
		service.afterSettleTreeRead = nil
		service.beforeSettleVersion = nil
	})
	return invariant
}

// requireVersions fails the test unless exactly versions of room's settlement passes reached their
// version.
func (i *readInvariant) requireVersions(t *testing.T, room string, versions int) {
	t.Helper()
	i.mu.Lock()
	got := i.versions[room]
	i.mu.Unlock()
	if got != versions {
		t.Fatalf("%d settlement passes reached their version, want %d", got, versions)
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

	invariant := requireReadUnchangedAtVersion(t, service)
	settleCurrentGeneration(t, service, artifactID)

	if versions := latestVersionNumber(t, service, artifactID); versions != versionsBefore+1 {
		t.Fatalf("latest version = %d, want %d: the settlement wrote no version of the edited document", versions, versionsBefore+1)
	}
	invariant.requireVersions(t, artifactID, 1)
}
