package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// dropBlock returns a live edit that removes the block whose text starts with prefix, as a
// browser does when it selects a paragraph and hits delete. A deletion creates no new Yjs
// structs, so the update it produces leaves the room's state vector exactly where it was.
// That is what makes it the case a clock-only signal cannot see.
func dropBlock(t *testing.T, prefix string) func(*pmdoc.Node) *pmdoc.Node {
	t.Helper()
	return func(tree *pmdoc.Node) *pmdoc.Node {
		kept := tree.Children[:0]
		for _, child := range tree.Children {
			rendered, err := renderTree(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{child}})
			if err != nil {
				t.Fatalf("render candidate block: %v", err)
			}
			if strings.HasPrefix(strings.TrimSpace(rendered), prefix) {
				continue
			}
			kept = append(kept, child)
		}
		tree.Children = kept
		return tree
	}
}

// A browser DELETE the room gains under the transaction moves the fork exactly as an insertion
// does - the text is gone from the fork - so the snapshot must render the moved document. A
// deletion is the case an insertion-shaped signal misses: it creates no Yjs struct and advances
// no client's clock, and the version must not be written from text the browser has removed.
func TestAForkThatLosesRoomContentIsRenderedAgain(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before\n\ndoomed paragraph")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)

	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "after",
	}}, alice, nil); err != nil {
		t.Fatalf("joined edit: %v", err)
	}
	editLiveTree(t, service, artifactID, dropBlock(t, "doomed paragraph"))

	_, markdown, _, _, err := service.captureLiveTextAndAuthors(joinedCtx, artifactID, nil)
	if err != nil {
		t.Fatalf("capture live text: %v", err)
	}
	if strings.Contains(markdown, "doomed paragraph") {
		t.Fatalf("snapshot markdown = %q, want the paragraph the browser deleted to be gone", markdown)
	}
	if !strings.Contains(markdown, "after") {
		t.Fatalf("snapshot markdown = %q, want it to carry the transaction's own edit", markdown)
	}
}

// The version write skips its anchor refresh when the transaction's own operation already
// refreshed that identical tree. A browser deletion the room gained since makes that tree no
// longer the document, so the skipped pass would be the only one that could have seen the
// anchored quote disappear. The ask anchored to the deleted paragraph must be orphaned.
func TestAnAnchorWhoseQuoteTheRoomDeletedIsOrphanedByTheVersionWrite(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before\n\nthe anchored quote paragraph")

	askID := "00000000-0000-4000-8000-0000000000b1"
	alice := model.Actor{Kind: "user", ID: "alice"}
	seedAnchoredAsk(t, service, artifactID, askID, "the anchored quote paragraph", "the anchored quote paragraph")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)

	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "after",
	}}, alice, nil); err != nil {
		t.Fatalf("joined edit: %v", err)
	}
	editLiveTree(t, service, artifactID, dropBlock(t, "the anchored quote paragraph"))

	if _, err := service.SnapshotVersion(joinedCtx, artifactID, alice); err != nil {
		t.Fatalf("snapshot version: %v", err)
	}
	var encoded []byte
	if err := tx.QueryRow(ctx, `select anchor from asks where id = $1`, askID).Scan(&encoded); err != nil {
		t.Fatalf("read the ask's anchor: %v", err)
	}
	var anchor model.Anchor
	if err := json.Unmarshal(encoded, &anchor); err != nil {
		t.Fatalf("decode the ask's anchor: %v", err)
	}
	if !anchor.Orphaned {
		t.Fatalf("anchor = %#v, want it orphaned: the browser deleted the paragraph it quotes", anchor)
	}
}

// One room change costs one re-render. Once the fork has absorbed what the room gained, an
// unchanged room must not look like another change: the contended case is a browser and an
// agent writing together, where every spurious invalidation is a full document render back
// under the issue row.
func TestOneAbsorbedRoomChangeCostsOneRender(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "before")

	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin transactional edit: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	joinedCtx, ledger := service.Join(ctx, tx)
	t.Cleanup(ledger.Discard)

	alice := model.Actor{Kind: "user", ID: "alice"}
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "before", With: "after",
	}}, alice, nil); err != nil {
		t.Fatalf("first joined edit: %v", err)
	}
	editLiveTree(t, service, artifactID, appendBlocks(t, "gained in the room"))
	// The next operation absorbs the room's change and renders the document it leaves.
	if _, err := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
		Op: "replace", Find: "after", With: "AFTER",
	}}, alice, nil); err != nil {
		t.Fatalf("second joined edit: %v", err)
	}
	write := ledger.liveWriteFor(artifactID)
	if write == nil || write.tree == nil {
		t.Fatal("the second edit recorded no rendering")
	}

	tree, markdown, _, _, err := service.captureLiveTextAndAuthors(joinedCtx, artifactID, nil)
	if err != nil {
		t.Fatalf("capture live text: %v", err)
	}
	if tree != write.tree {
		t.Fatal("the snapshot rendered the document again though the room stood still")
	}
	if !strings.Contains(markdown, "gained in the room") || !strings.Contains(markdown, "AFTER") {
		t.Fatalf("snapshot markdown = %q, want the room's content and the transaction's", markdown)
	}
}

// A transaction may reuse the rendering it cached only while the fork it was taken from still
// describes the document, and the only thing that can say so is the fork itself, read on either
// side of the one update it absorbs. Two reads of the room are two snapshots - websocket
// Server.Apply holds no lock across its callback (ygo provider/websocket/inject.go:295) - so a
// browser update landing between them is in one and not the other, and a signal built from that
// pair concludes the room held while handing the fork the very update it missed. The version is
// then written from a document that no longer exists.
//
// The browser's delete here is queued on the document's mutex while the transaction's first
// encoding of the room runs, so it is granted the lock in that window: the interleave is driven
// rather than waited for.
func TestACaptureNeverReusesARenderingTheForkHasMovedPast(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	body := make([]string, 0, 1200)
	for index := range 1200 {
		body = append(body, fmt.Sprintf("Paragraph %d of the specification body with enough words to be realistic prose.", index+1))
	}
	seedServiceText(t, service, artifactID, strings.Join(body, "\n\n"))

	alice := model.Actor{Kind: "user", ID: "alice"}
	reused, stale := 0, 0
	const rounds = 640
	for round := range rounds {
		marker := fmt.Sprintf("DOOMED-%d under the browser's cursor", round)
		editLiveTree(t, service, artifactID, appendBlocks(t, marker))

		// Precompute the tree the browser's delete will write, so that when it fires it does
		// nothing but take the document's mutex and apply.
		var want *pmdoc.Node
		if err := service.srv.Apply(context.Background(), artifactID,
			func(doc *crdt.Doc, _ func(func(*crdt.Transaction))) {
				tree, err := pmdoc.Read(doc.GetXmlFragment(fragmentName))
				if err != nil {
					return
				}
				kept := tree.Children[:0]
				for _, child := range tree.Children {
					rendered, renderErr := renderTree(&pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{child}})
					if renderErr != nil || strings.Contains(rendered, marker) {
						continue
					}
					kept = append(kept, child)
				}
				tree.Children = kept
				want = tree
			}); err != nil && !strings.Contains(err.Error(), "no changes") {
			t.Fatalf("round %d: precompute the browser's delete: %v", round, err)
		}
		if want == nil {
			t.Fatalf("round %d: could not precompute the browser's delete", round)
		}

		ctx := context.Background()
		tx, err := service.store.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("round %d: begin: %v", round, err)
		}
		joinedCtx, ledger := service.Join(ctx, tx)

		var browser sync.WaitGroup
		browser.Add(1)
		go func() {
			defer browser.Done()
			// Long enough for the transaction's first forkLive to be inside its first encoding
			// of the room, short enough to be well before the version snapshot.
			// Walk the delete across the window in which the transaction's first forkLive
			// takes the owner row and then encodes the room, so it is queued on the
			// document's mutex while that encoding runs.
			time.Sleep(time.Duration(200+(round%160)*50) * time.Microsecond)
			_ = service.srv.Apply(context.Background(), artifactID,
				func(doc *crdt.Doc, transact func(func(*crdt.Transaction))) {
					fragment := doc.GetXmlFragment(fragmentName)
					transact(func(txn *crdt.Transaction) { _ = pmdoc.Update(txn, fragment, want) })
				})
		}()

		_, editErr := service.ApplyOps(joinedCtx, artifactID, []model.EditOp{{
			Op:   "replace",
			Find: fmt.Sprintf("Paragraph %d of the specification body with enough words to be realistic prose.", round+1),
			With: fmt.Sprintf("EDITED %d of the specification body with enough words to be realistic prose.", round+1),
		}}, alice, nil)
		browser.Wait()
		if editErr != nil {
			ledger.Discard()
			_ = tx.Rollback(ctx)
			continue
		}
		write := ledger.liveWriteFor(artifactID)
		if write != nil && write.tree != nil {
			tree, markdown, _, _, err := service.captureLiveTextAndAuthors(joinedCtx, artifactID, nil)
			if err != nil {
				t.Fatalf("round %d: capture: %v", round, err)
			}
			if tree == write.tree {
				reused++
				fresh, err := treeOf(write.fork)
				if err != nil {
					t.Fatalf("round %d: read the fork: %v", round, err)
				}
				freshMarkdown, err := renderTree(fresh)
				if err != nil {
					t.Fatalf("round %d: render the fork: %v", round, err)
				}
				if freshMarkdown != markdown {
					stale++
					t.Errorf("round %d: the capture reused a rendering of %d bytes while the fork"+
						" it was taken from now renders %d", round, len(markdown), len(freshMarkdown))
				}
			}
		}
		ledger.Discard()
		_ = tx.Rollback(ctx)
	}
	t.Logf("captures that reused the cached rendering: %d of %d; stale: %d", reused, rounds, stale)
	if stale > 0 {
		t.Fatalf("%d captures reused a rendering the fork had moved past", stale)
	}
}
