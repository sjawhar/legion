package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// A write pays about 46 µs a block to index a document's blocks, so above a block count - docs'
// deferBlockIndexBlocks, 2,000 blocks, the 92 ms that count costs - it stores the version and its
// references alone and leaves the blocks to the block-index reconciliation. A bullet list of n
// items is n+1 blocks, the list and each item's paragraph, so these two lists straddle that
// count: 1,998 items is 1,999 blocks, one under it, and 2,000 items is 2,001 blocks, one over.
const (
	itemsUnderTheBlockBound = 1_998
	blocksUnderTheBound     = itemsUnderTheBlockBound + 1
	itemsOverTheBlockBound  = 2_000
	blocksOverTheBound      = itemsOverTheBlockBound + 1
)

// deferralList is a bullet list of items items whose last one mentions target.
func deferralList(items int, target string) string {
	var body strings.Builder
	for item := 1; item < items; item++ {
		body.WriteString("- x\n")
	}
	body.WriteString("- Last " + target + "\n")
	return body.String()
}

// blockIndexVersion is the version a document's blocks are indexed at, nil while the document is
// owed an index: the mark migration 0122's trigger leaves on every version a write does not index
// itself (document_block_versions).
func blockIndexVersion(t *testing.T, database *store.Store, artifactID string) *int {
	t.Helper()
	var version *int
	if err := database.Pool.QueryRow(context.Background(),
		`select version from document_block_versions where artifact_id = $1`, artifactID).Scan(&version); err != nil {
		t.Fatalf("read the block index version of %s: %v", artifactID, err)
	}
	return version
}

// mentionOf is the one edge the graph holds to target, which only the document under test
// mentions.
func mentionOf(t *testing.T, handler http.Handler, target string) model.GraphEdge {
	t.Helper()
	edges := graphEdges(t, handler, url.Values{"to": {target}, "kind": {"mentions"}}).Edges
	if len(edges) != 1 {
		t.Fatalf("mentions of %s = %#v, want exactly one", target, edges)
	}
	return edges[0]
}

// A document of more blocks than a write indexes in its own request is stored with its blocks
// owed, and one pass of the block-index reconciliation puts them there: the document's blocks
// become nodes, and the backlink to what it mentions names the block holding the mention rather
// than the document. The write itself records no block and no block edge.
func TestADocumentPastTheBlockBoundLeavesItsBlocksToTheReconciliation(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	ctx := context.Background()
	createReferenceAPIProject(t, handler, "CORE")
	mentioned := createReferenceAPIIssue(t, handler, "CORE")
	holder := createSpecIssue(t, handler, "CORE", "The heavy document's issue.\n")
	target := "dispatch://" + mentioned.Key
	item := "Last " + target

	notes := uploadDocument(t, handler, "/api/v1/issues/"+holder.Key, "heavy.md", deferralList(itemsOverTheBlockBound, target)).Artifact
	if indexed := blockIndexVersion(t, database, notes.ID); indexed != nil {
		t.Fatalf("the write indexed the blocks at version %d, want them left owed to the block-index reconciliation", *indexed)
	}
	if indexedBlocks, _ := readIndexedBlocks(t, database, notes.ID); len(indexedBlocks) != 0 {
		t.Fatalf("the write recorded %d block nodes, want none", len(indexedBlocks))
	}
	if edge := mentionOf(t, handler, target); edge.Node.Kind != "artifact" || edge.Node.ID != notes.ID {
		t.Fatalf("before the block-index reconciliation the mention of %s comes from %#v, want the document itself", target, edge.Node)
	}

	tick, err := deps.Docs.(*docs.Service).ReconcileBlockIndex(ctx, "", nil)
	if err != nil {
		t.Fatalf("one pass of the block-index reconciliation: %v", err)
	}
	if tick.Documents < 1 {
		t.Fatalf("the block-index reconciliation indexed %d documents (%+v), want the deferred one", tick.Documents, tick)
	}
	requireIndexedAt(t, database, notes.ID, 1)
	indexedBlocks, _ := readIndexedBlocks(t, database, notes.ID)
	if len(indexedBlocks) != blocksOverTheBound {
		t.Fatalf("the block-index reconciliation recorded %d block nodes of the %d-item list, want %d",
			len(indexedBlocks), itemsOverTheBlockBound, blocksOverTheBound)
	}
	holding := blockIDsByText(t, handler, notes.ID)[item]
	if holding == "" {
		t.Fatalf("no block of %s holds %q", notes.ID, item)
	}
	if edge := mentionOf(t, handler, target); edge.Node.Kind != "block" || edge.Node.BlockID != holding {
		t.Fatalf("after the block-index reconciliation the mention of %s comes from %#v, want block %q", target, edge.Node, holding)
	}
}

// The control that the bound is a bound and not a cutover: a document of one block fewer is
// indexed by its own write, with no block-index reconciliation pass at all. Its blocks are nodes
// and the backlink names the block holding the mention as soon as the write answers.
func TestADocumentUnderTheBlockBoundIsIndexedByItsOwnWrite(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	createReferenceAPIProject(t, handler, "CORE")
	mentioned := createReferenceAPIIssue(t, handler, "CORE")
	holder := createSpecIssue(t, handler, "CORE", "The heavy document's issue.\n")
	target := "dispatch://" + mentioned.Key
	item := "Last " + target

	notes := uploadDocument(t, handler, "/api/v1/issues/"+holder.Key, "heavy.md", deferralList(itemsUnderTheBlockBound, target)).Artifact
	requireIndexedAt(t, database, notes.ID, 1)
	indexedBlocks, _ := readIndexedBlocks(t, database, notes.ID)
	if len(indexedBlocks) != blocksUnderTheBound {
		t.Fatalf("the write recorded %d block nodes of the %d-item list, want %d",
			len(indexedBlocks), itemsUnderTheBlockBound, blocksUnderTheBound)
	}
	holding := blockIDsByText(t, handler, notes.ID)[item]
	if holding == "" {
		t.Fatalf("no block of %s holds %q", notes.ID, item)
	}
	if edge := mentionOf(t, handler, target); edge.Node.Kind != "block" || edge.Node.BlockID != holding {
		t.Fatalf("the write's mention of %s comes from %#v, want block %q", target, edge.Node, holding)
	}
}
