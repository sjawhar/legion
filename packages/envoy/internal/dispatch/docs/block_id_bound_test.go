package docs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// boundSpec is a spec of two callouts and a paragraph. A browser edit gives the first callout and
// its paragraph, and the second callout's paragraph, ids past text.MaxBlockIDBytes
// (giveLongBlockIDs): the mention of CORE-2 then lies in no block the index can key, and the
// mention of CORE-1 in a long-id paragraph inside the callout `outer`.
const boundSpec = ":::callout{#first kind=\"note\" title=\"\"}\nShip it after dispatch://CORE-2.\n:::\n\n" +
	":::callout{#outer kind=\"note\" title=\"\"}\nThen dispatch://CORE-1.\n:::\n\nOther paragraph.\n"

// randomHexID is an id of length incompressible random hex characters, so no Postgres index can
// hold it by compressing it.
func randomHexID(t *testing.T, length int) string {
	t.Helper()
	raw := make([]byte, (length+1)/2)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw)[:length]
}

// giveLongBlockIDs writes, as a browser's edit can, an id of 3200 random hex characters on
// boundSpec's first callout, on its paragraph, and on the paragraph of the callout `outer`, and
// returns the three ids.
func giveLongBlockIDs(t *testing.T, service *Service, artifactID string) []string {
	t.Helper()
	long := []string{randomHexID(t, 3200), randomHexID(t, 3200), randomHexID(t, 3200)}
	editLiveTree(t, service, artifactID, func(tree *pmdoc.Node) *pmdoc.Node {
		first, outer := tree.Children[0], tree.Children[1]
		if first.Type != "callout" || outer.Type != "callout" || first.Children[0].Type != "paragraph" || outer.Children[0].Type != "paragraph" {
			t.Fatalf("the live tree is %#v, want boundSpec's two callouts", tree.Children)
		}
		first.Attrs[pmdoc.BlockIDAttr] = long[0]
		first.Children[0].Attrs[pmdoc.BlockIDAttr] = long[1]
		outer.Children[0].Attrs[pmdoc.BlockIDAttr] = long[2]
		return tree
	})
	waitForLandedAppends(t, service, artifactID)
	return long
}

// requireIndexedWithoutLongIDs checks the document's index at version: every block of its live
// text whose id is within the bound is a node and none past it is, and the one mention a node holds
// is CORE-1's, from `outer`, the innermost node around the long-id paragraph it is written in;
// CORE-2's lies in no node.
func requireIndexedWithoutLongIDs(t *testing.T, database *store.Store, service *Service, artifactID string, version int, long []string) {
	t.Helper()
	ctx := context.Background()
	if indexed := blockIndexAt(t, database, artifactID); indexed == nil || *indexed != version {
		t.Fatalf("blocks indexed at %v, want version %d", indexed, version)
	}
	_, blocks, err := service.TextWithBlocks(ctx, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	held := 0
	for _, block := range blocks {
		if slices.Contains(long, block.ID) {
			held++
			continue
		}
		want = append(want, block.ID)
	}
	if held != len(long) || !slices.Contains(want, "outer") {
		t.Fatalf("the live blocks %#v, want the three long ids and the callout outer", blocks)
	}
	rows, err := database.Pool.Query(ctx, `select block_id from document_blocks where artifact_id = $1`, artifactID)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(want)
	slices.Sort(nodes)
	if !slices.Equal(nodes, want) {
		t.Fatalf("document_blocks holds %d ids %q, want the %d within the bound %q", len(nodes), nodes, len(want), want)
	}
	var texts int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from document_block_texts where artifact_id = $1 and length(block_id) > $2
	`, artifactID, text.MaxBlockIDBytes).Scan(&texts); err != nil || texts != 0 {
		t.Fatalf("document_block_texts holds %d texts under a long id (%v), want none", texts, err)
	}
	if pairs := blockPairsOf(t, database, artifactID); len(pairs) != 1 || pairs[0].BlockID != "outer" || pairs[0].ToKind != "issue" || pairs[0].ToID != "CORE-1" {
		t.Fatalf("block_refs = %#v, want the one mention of CORE-1 from outer", pairs)
	}
}

// A version whose document holds a block id of 3200 random hex characters - past what a Postgres
// B-tree key holds, which a browser's edit can write - is written: the version is stored with the
// id in its markdown, its other blocks are indexed, the long-id blocks are no nodes, and a mention
// written in one starts at the innermost node holding it.
func TestAVersionHoldingABlockIDPastTheBoundIsWrittenAndIndexed(t *testing.T) {
	database, service := blockIndexService(t)
	spec := createOwedDocument(t, database, service, "", "spec", boundSpec, "")
	long := giveLongBlockIDs(t, service, spec)

	result, err := namedVersion(t, service, spec, "a version holding long block ids", model.Actor{Kind: "user", ID: "alice"})
	if err != nil {
		t.Fatalf("name a version of the document holding long block ids: %v", err)
	}
	if !result.Wrote || result.Version.Number != 2 {
		t.Fatalf("named version = %+v, want version 2 written", result)
	}
	if markdown := storedVersionMarkdown(t, database, spec, 2); !strings.Contains(markdown, "{#"+long[0]+" ") {
		t.Fatalf("version 2 = %q, want it to hold the callout's long id", markdown)
	}
	requireIndexedWithoutLongIDs(t, database, service, spec, 2, long)
}

// A document already holding block ids past the bound - its latest version written by a build
// without the bound - is indexed by the reconciliation like any other: its other blocks become
// nodes, the long-id blocks none, and it is owed nothing more, so the next tick has nothing to do.
func TestADocumentAlreadyHoldingABlockIDPastTheBoundIsIndexedAndLeavesOwed(t *testing.T) {
	database, service := blockIndexService(t)
	spec := createOwedDocument(t, database, service, "", "spec", boundSpec, "")
	settle(t, database, spec)
	long := giveLongBlockIDs(t, service, spec)
	version, _ := writeAsAnOlderBuild(t, database, service, spec, "Other paragraph.", "Other paragraph, edited.")
	if indexed := blockIndexAt(t, database, spec); indexed != nil {
		t.Fatalf("after the older build's version the blocks stand at %d, want the document owed", *indexed)
	}

	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 1 || tick.OwedDocuments != 0 {
		t.Fatalf("tick = %#v, want the document holding long block ids indexed", tick)
	}
	requireIndexedWithoutLongIDs(t, database, service, spec, version.Number, long)
	if tick := reconcileBlockIndex(t, service, ""); tick.Documents != 0 || tick.OwedDocuments != 0 {
		t.Fatalf("next tick = %#v, want nothing owed", tick)
	}
}

// An ask block a browser's edit gives an id of 3200 random hex characters - past what a Postgres
// B-tree key holds - settles as any other: its ask opens under that id, an edit of its question
// reaches the row, and a second document of the issue holding a copy of the block shows that ask
// and opens none. No settlement fails on an index key past its limit (SQLSTATE 54000).
func TestAnAskBlockWhoseIDIsPastTheBoundSettles(t *testing.T) {
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	service, original := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, original, "Context\n\n"+shipBlock)
	long := randomHexID(t, 3200)
	editLiveTree(t, service, original, func(tree *pmdoc.Node) *pmdoc.Node {
		if ask := tree.Children[1]; ask.Type != "ask" {
			t.Fatalf("the live tree is %#v, want the ship ask second", tree.Children)
		}
		tree.Children[1].Attrs[pmdoc.BlockIDAttr] = long
		return tree
	})
	settleCurrentGeneration(t, service, original)
	if line := logs.Lines("54000"); line != "" {
		t.Fatalf("the settlement of the long-id ask block failed: %.400s", line)
	}
	if ask := blockAskOn(t, service, original, long); ask.State != "open" || ask.Question != "Ship on Monday?" {
		t.Fatalf("the long-id block's ask is %s %q, want it open asking the block's question", ask.State, ask.Question)
	}

	editLiveTree(t, service, original, replaceRun("Ship on Monday?", "Ship on Tuesday?"))
	settleCurrentGeneration(t, service, original)
	ask := blockAskOn(t, service, original, long)
	if ask.Question != "Ship on Tuesday?" {
		t.Fatalf("the long-id block's ask asks %q after the edit, want the block's new question", ask.Question)
	}

	markdown, err := service.Text(context.Background(), original)
	if err != nil {
		t.Fatalf("read the original: %v", err)
	}
	copied := createIssueDocument(t, service.store, 1, "# Copy")
	seedServiceText(t, service, copied, "# Copy\n")
	browserReplaceText(t, service, copied, markdown, model.Actor{Kind: "user", ID: "alice"})
	settleCurrentGeneration(t, service, copied)
	if count := asksOn(t, service, copied); count != 0 {
		t.Fatalf("the copy opened %d asks, want none", count)
	}
	requireCopiedStates(t, service, copied, copiedFrom(t, service, ask.ID, original))
	if line := logs.Lines("54000"); line != "" {
		t.Fatalf("a settlement failed on an index key past its limit: %.400s", line)
	}
}
