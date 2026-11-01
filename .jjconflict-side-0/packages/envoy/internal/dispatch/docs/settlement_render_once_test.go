package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A settlement renders the tree it read once, and its version reuses that rendering only while the
// tree is unchanged. A settlement that repairs an ask block's server attributes changes that tree
// in place, and a peer can have written the same attributes into the room first, so the repair
// writes nothing into the room. The version still holds the repaired block, with the answered
// state, and its indexed blocks are the room's: reusing the rendering made before the repair would
// version the block as still open and index the open block's text. An answer that selects
// nothing is versioned as the room holds it, an empty list.
func TestSettlementVersionsARepairTheRoomAlreadyHolds(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"open\"}\nShip it?\n\n- Yes: Monday.\n- No: Later.\n:::\n\nContext before.\n")
	service.settleRoom(artifactID, 0)
	answer := answerBlockAsk(t, service, artifactID)
	ask := blockAskOn(t, service, artifactID, "ask-block")
	// A content edit, so the settlement has something to version.
	editAsPeer(t, service, artifactID, replaceRun("Context before.", "Context after."))
	if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs != 0 {
		t.Fatalf("the room has %d blocks to stamp, want none: the settlement must reconcile the tree it read", repairs)
	}
	versionsBefore := latestVersionNumber(t, service, artifactID)

	var release func()
	service.afterSettleReconcile = func(room string) {
		if room != artifactID || release != nil {
			return
		}
		// A peer writes the answered ask's server attributes into the block first, so the
		// settlement's own repair finds nothing to write and holds no slot. The peer's observer is
		// held once it has classified and credited the edit and before it arms a settlement, so
		// the room's generation stays where the settlement read it. Held before it classifies
		// anything (beforeObserveUpdate), the edit would be classified by the observer of the
		// repair's own empty transaction, which drains the room's update log too and arms a
		// settlement, and the superseded settlement would write no version at all.
		release = holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, func(tree *pmdoc.Node) *pmdoc.Node {
			pmdoc.Walk(tree, func(node *pmdoc.Node) bool {
				if id, _ := node.Attrs[pmdoc.BlockIDAttr].(string); id == "ask-block" {
					setAskServerAttributes(node, ask, "", false)
					return false
				}
				return true
			})
			return tree
		})
	}
	invariant := requireReadUnchangedWhenReused(t, service)
	settleCurrentGeneration(t, service, artifactID)
	if release == nil {
		t.Fatal("settlement never reached the window between its reconciliation and its repair")
	}
	release()
	service.afterSettleReconcile = nil
	requireNoSuppressedSlots(t, service, artifactID, "after the repair")
	// The repair changed the tree the settlement read, so its version renders that tree again.
	invariant.requireCounts(t, artifactID, 0, 1)

	if versions := latestVersionNumber(t, service, artifactID); versions != versionsBefore+1 {
		t.Fatalf("latest version = %d, want %d: the settlement wrote no version of the edited document", versions, versionsBefore+1)
	}
	want := ":::ask{#ask-block urgency=\"med\" multiple=\"false\" state=\"answered\" answered_by=\"alice\" answered_at=\"" +
		answer.At.Format(time.RFC3339Nano) + "\" selected=\"[]\"}\nShip it?\n\n- Yes: Monday.\n- No: Later.\n:::\n\nContext after.\n"
	markdown, offsets := roomRendering(t, service, artifactID)
	if markdown != want {
		t.Fatalf("the room renders %q, want %q", markdown, want)
	}
	requireLatestVersionMarkdown(t, service, artifactID, want)
	requireIndexedBlocks(t, service, artifactID, markdown, offsets)
}

// A settlement that stamps block ids versions the tree its stamp left, not the one it read before
// it. Another writer can stamp the ids first, so the settlement's stamp mints nothing and writes
// nothing into the room, and the tree it reconciles and versions is still the stamp's, not the
// read. The version holds the ids the room's document holds, rendered and indexed: reusing the
// rendering of the read would index no row for the appended paragraph, and version and index the
// pasted callout under the id of the callout it copies.
func TestSettlementVersionsTheTreeOfAStampThatMintedNothing(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before\n\n:::callout{#note kind=\"note\" title=\"\"}\nNoted.\n:::\n")
	settleCurrentGeneration(t, service, artifactID)
	// A paragraph without an id, and a pasted copy of the callout carrying the callout's own id.
	// Typed blocks render their ids, so the stamp that gives the copy an id of its own changes the
	// room's rendering, which the hold below needs.
	editLiveTree(t, service, artifactID, appendUnidentifiedBlocks(t, "added"))
	editLiveTree(t, service, artifactID, appendBlockWithCraftedID(t, ":::callout{kind=\"note\" title=\"\"}\nCopied.\n:::\n", "note"))
	if repairs := pmdoc.BlockIDRepairCount(liveTree(t, service, artifactID)); repairs == 0 {
		t.Fatal("the room has no block to stamp, so nothing here is tested")
	}
	versionsBefore := latestVersionNumber(t, service, artifactID)

	var release func()
	var copyID atomic.Value
	service.afterSettleRead = func(room string) {
		if room != artifactID || release != nil {
			return
		}
		// Another writer identifies the blocks first, as an agent's edit stamps every block it
		// addresses. Its observer is held once it has classified and credited the stamp and
		// before it arms a settlement, so the room's generation stays where the settlement read
		// it: the observer of the settlement's own empty stamp then drains nothing but its own
		// update.
		release = holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, func(tree *pmdoc.Node) *pmdoc.Node {
			pmdoc.EnsureBlockIDs(tree)
			copyID.Store(tree.Children[len(tree.Children)-1].Attrs[pmdoc.BlockIDAttr])
			return tree
		})
	}
	invariant := requireReadUnchangedWhenReused(t, service)
	settleCurrentGeneration(t, service, artifactID)
	if release == nil {
		t.Fatal("settlement never reached the window between its read and its stamp")
	}
	release()
	service.afterSettleRead = nil
	requireNoSuppressedSlots(t, service, artifactID, "after the stamp")
	// The stamp's tree replaced the read, so the version renders the stamp's tree.
	invariant.requireCounts(t, artifactID, 0, 1)

	if versions := latestVersionNumber(t, service, artifactID); versions != versionsBefore+1 {
		t.Fatalf("latest version = %d, want %d: the settlement wrote no version of the appended blocks", versions, versionsBefore+1)
	}
	stamped, _ := copyID.Load().(string)
	if stamped == "" || stamped == "note" {
		t.Fatalf("the other writer left the pasted callout id %q, want one of its own", stamped)
	}
	tree := liveTree(t, service, artifactID)
	if repairs := pmdoc.BlockIDRepairCount(tree); repairs != 0 {
		t.Fatalf("the room has %d blocks left to stamp, want none", repairs)
	}
	if held, _ := tree.Children[len(tree.Children)-1].Attrs[pmdoc.BlockIDAttr].(string); held != stamped {
		t.Fatalf("the pasted callout carries id %q, want %q, the other writer's: the settlement's stamp minted one", held, stamped)
	}
	markdown, offsets := roomRendering(t, service, artifactID)
	if !strings.Contains(markdown, "#"+stamped) {
		t.Fatalf("the room renders %q, want the pasted callout's id %q in it", markdown, stamped)
	}
	requireIndexedBlocks(t, service, artifactID, markdown, offsets)
	requireLatestVersionMarkdown(t, service, artifactID, markdown)
}

// roomRendering is the room's live document rendered with its block offsets, as a settlement
// renders the tree it versions.
func roomRendering(t *testing.T, service *Service, artifactID string) (string, []pmdoc.BlockOffset) {
	t.Helper()
	markdown, offsets, err := renderTreeWithOffsets(liveTree(t, service, artifactID))
	if err != nil {
		t.Fatalf("render live tree: %v", err)
	}
	return markdown, offsets
}

// requireIndexedBlocks requires the document's blocks to be indexed at its latest version as the
// blocks of markdown's rendering: one document_blocks row for the first block of each id in
// offsets, with that block's type and the sha256 of its text, and no other row.
func requireIndexedBlocks(t *testing.T, service *Service, artifactID, markdown string, offsets []pmdoc.BlockOffset) {
	t.Helper()
	indexedAt := blockIndexAt(t, service.store, artifactID)
	if latest := latestVersionNumber(t, service, artifactID); indexedAt == nil || *indexedAt != latest {
		t.Fatalf("the document's blocks are indexed at version %v, want the latest, %d", indexedAt, latest)
	}
	var want []string
	seen := map[string]bool{}
	for _, offset := range offsets {
		if offset.ID == "" || seen[offset.ID] {
			continue
		}
		seen[offset.ID] = true
		sum := sha256.Sum256([]byte(markdown[offset.From:offset.To]))
		want = append(want, offset.ID+" "+offset.Type+" "+hex.EncodeToString(sum[:]))
	}
	rows, err := service.store.Pool.Query(context.Background(), `
		select block_id, type, text_sha256 from document_blocks where artifact_id = $1
	`, artifactID)
	if err != nil {
		t.Fatalf("read the document's indexed blocks: %v", err)
	}
	got, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (string, error) {
		var id, kind, sum string
		err := row.Scan(&id, &kind, &sum)
		return id + " " + kind + " " + sum, err
	})
	if err != nil {
		t.Fatalf("read the document's indexed blocks: %v", err)
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("the document's indexed blocks (id type text_sha256) = %q, want the room rendering's %q", got, want)
	}
}
