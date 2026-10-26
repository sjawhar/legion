package api

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// blockIndexHandler is a test server whose documents never settle during the test, so every
// version a test reads is one its own requests wrote.
func blockIndexHandler(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	return handler, database
}

func createSpecIssue(t *testing.T, handler http.Handler, project, spec string) model.Issue {
	t.Helper()
	response := createIssueRequest(t, handler, map[string]any{"project": project, "title": "Block index", "spec": spec})
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue with spec: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[model.Issue](t, response)
}

// blockIDsByText is, for each block GET …/blocks lists, its id keyed by its text: the document's
// markdown over the block's range.
func blockIDsByText(t *testing.T, handler http.Handler, artifactID string) map[string]string {
	t.Helper()
	markdown := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")).Markdown
	ids := map[string]string{}
	for _, block := range decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice")) {
		ids[markdown[block.From:block.To]] = block.ID
	}
	return ids
}

// editSpec applies ops to an issue's spec as a named version and returns the version it wrote.
func editSpec(t *testing.T, handler http.Handler, key string, ops []map[string]any) model.Version {
	t.Helper()
	return editIssueDocument(t, handler, key, "spec", ops)
}

// editIssueDocument applies ops to the issue document slug as a named version and returns the
// version it wrote.
func editIssueDocument(t *testing.T, handler http.Handler, key, slug string, ops []map[string]any) model.Version {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/artifacts/"+slug+"/edits", map[string]any{
		"ops": ops, "summary": "edit",
	}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("edit %s of %s: status=%d body=%s", slug, key, response.Code, response.Body.String())
	}
	version := decodeBody[struct {
		Version *model.Version `json:"version"`
	}](t, response).Version
	if version == nil {
		t.Fatalf("the edit wrote no version: %s", response.Body.String())
	}
	return *version
}

type indexedBlock struct {
	Type           string
	Excerpt        string
	ChangedVersion int
}

// readIndexedBlocks is the document's block nodes by block id, and each one's changed_at.
func readIndexedBlocks(t *testing.T, database *store.Store, artifactID string) (map[string]indexedBlock, map[string]time.Time) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select d.block_id, d.node_id, a.ref_key, d.type, d.excerpt, d.changed_version, d.changed_at
		from document_blocks d join artifacts a on a.id = d.artifact_id where d.artifact_id = $1
	`, artifactID)
	if err != nil {
		t.Fatalf("read document_blocks: %v", err)
	}
	defer rows.Close()
	blocks, changedAt := map[string]indexedBlock{}, map[string]time.Time{}
	for rows.Next() {
		var id, node, refKey string
		var block indexedBlock
		var at time.Time
		if err := rows.Scan(&id, &node, &refKey, &block.Type, &block.Excerpt, &block.ChangedVersion, &at); err != nil {
			t.Fatalf("scan document_blocks: %v", err)
		}
		if node != refKey+"#"+id {
			t.Fatalf("block %q node id = %q, want %q", id, node, refKey+"#"+id)
		}
		blocks[id], changedAt[id] = block, at
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate document_blocks: %v", err)
	}
	return blocks, changedAt
}

// readBlockTexts is the text each block's row at version records: nil for a block it deleted.
func readBlockTexts(t *testing.T, database *store.Store, artifactID string, version int) map[string]*string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, text from document_block_texts where artifact_id = $1 and version = $2
	`, artifactID, version)
	if err != nil {
		t.Fatalf("read document_block_texts: %v", err)
	}
	defer rows.Close()
	texts := map[string]*string{}
	for rows.Next() {
		var id string
		var body *string
		if err := rows.Scan(&id, &body); err != nil {
			t.Fatalf("scan document_block_texts: %v", err)
		}
		texts[id] = body
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate document_block_texts: %v", err)
	}
	return texts
}

type blockRefRow struct {
	BlockID, FromNode, ToKind, ToID, Kind string
	CreatedVersion                        int
	CreatedAt                             time.Time
	SourceSeq                             *int64
}

func readBlockRefs(t *testing.T, database *store.Store, artifactID string) []blockRefRow {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		select block_id, from_node, to_kind, to_id, kind, created_version, created_at, source_seq
		from block_refs where artifact_id = $1 order by block_id, to_kind, to_id, kind
	`, artifactID)
	if err != nil {
		t.Fatalf("read block_refs: %v", err)
	}
	defer rows.Close()
	var result []blockRefRow
	for rows.Next() {
		var row blockRefRow
		if err := rows.Scan(&row.BlockID, &row.FromNode, &row.ToKind, &row.ToID, &row.Kind, &row.CreatedVersion, &row.CreatedAt, &row.SourceSeq); err != nil {
			t.Fatalf("scan block_refs: %v", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate block_refs: %v", err)
	}
	return result
}

// requireIndexedAt fails unless the document's blocks are indexed at version: its
// document_block_versions row names it, and document_indexed_versions records it.
func requireIndexedAt(t *testing.T, database *store.Store, artifactID string, version int) {
	t.Helper()
	var indexed *int
	var recorded bool
	if err := database.Pool.QueryRow(context.Background(), `
		select (select version from document_block_versions where artifact_id = $1),
		       exists (select 1 from document_indexed_versions where artifact_id = $1 and version = $2)
	`, artifactID, version).Scan(&indexed, &recorded); err != nil {
		t.Fatalf("read the block index version: %v", err)
	}
	if indexed == nil || *indexed != version || !recorded {
		t.Fatalf("document %s blocks indexed at %v (version %d recorded indexed: %v), want version %d", artifactID, indexed, version, recorded, version)
	}
}

func latestVersion(t *testing.T, database *store.Store, artifactID string) model.Version {
	t.Helper()
	var version model.Version
	if err := database.Pool.QueryRow(context.Background(), `
		select number, created_at from artifact_versions where artifact_id = $1 order by number desc limit 1
	`, artifactID).Scan(&version.Number, &version.CreatedAt); err != nil {
		t.Fatalf("read the latest version of %s: %v", artifactID, err)
	}
	return version
}

// A version records each block GET …/blocks lists as a node with its text, and a mention written
// inside a block as an edge out of the innermost block holding it, dated by the version that wrote
// it and the event that recorded that version.
func TestAVersionIndexesItsBlocksAndTheMentionsInThem(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	target := createReferenceAPIIssue(t, handler, "CORE")
	callout := ":::callout{#ship_it kind=\"note\" title=\"\"}\nShip it.\n:::"
	issue := createSpecIssue(t, handler, "CORE", callout+"\n\nOther paragraph.\n")
	spec := issue.PrimaryArtifactID

	ids := blockIDsByText(t, handler, spec)
	inner, other := ids["Ship it."], ids["Other paragraph."]
	if len(ids) != 3 || ids[callout] != "ship_it" || inner == "" || other == "" {
		t.Fatalf("GET /blocks = %#v, want the callout ship_it, its paragraph and the other paragraph", ids)
	}
	blocks, _ := readIndexedBlocks(t, database, spec)
	want := map[string]indexedBlock{
		"ship_it": {Type: "callout", Excerpt: callout, ChangedVersion: 1},
		inner:     {Type: "paragraph", Excerpt: "Ship it.", ChangedVersion: 1},
		other:     {Type: "paragraph", Excerpt: "Other paragraph.", ChangedVersion: 1},
	}
	if !reflect.DeepEqual(blocks, want) {
		t.Fatalf("document_blocks after version 1 = %#v, want %#v", blocks, want)
	}
	requireIndexedAt(t, database, spec, 1)
	if texts := readBlockTexts(t, database, spec, 1); len(texts) != 3 || *texts["ship_it"] != callout || *texts[inner] != "Ship it." || *texts[other] != "Other paragraph." {
		t.Fatalf("block texts of version 1 = %#v, want each block's text", texts)
	}
	if pairs := readBlockRefs(t, database, spec); len(pairs) != 0 {
		t.Fatalf("block_refs of a spec citing nothing = %#v", pairs)
	}

	version := editSpec(t, handler, issue.Key, []map[string]any{
		{"op": "replace", "find": "Other paragraph.", "with": "Other paragraph. See dispatch://" + target.Key + "."},
		{"op": "replace", "find": "Ship it.", "with": "Ship it after dispatch://" + target.Key + "."},
	})
	var eventID int64
	if err := database.Pool.QueryRow(context.Background(), `
		select id from events
		where type = 'artifact.version' and payload->>'artifact_id' = $1 and (payload->'version'->>'number')::int = $2
	`, spec, version.Number).Scan(&eventID); err != nil {
		t.Fatalf("read the event of version %d: %v", version.Number, err)
	}
	pairs := readBlockRefs(t, database, spec)
	// The callout holds the paragraph that cites the issue, but the paragraph is the innermost
	// block holding the mention: the callout itself writes no edge.
	wantPairs := []blockRefRow{
		{BlockID: inner, FromNode: issue.Key + "/spec#" + inner, ToKind: "issue", ToID: target.Key, Kind: "mentions"},
		{BlockID: other, FromNode: issue.Key + "/spec#" + other, ToKind: "issue", ToID: target.Key, Kind: "mentions"},
	}
	if inner > other {
		wantPairs[0], wantPairs[1] = wantPairs[1], wantPairs[0]
	}
	for index := range wantPairs {
		wantPairs[index].CreatedVersion, wantPairs[index].CreatedAt, wantPairs[index].SourceSeq = version.Number, version.CreatedAt, &eventID
	}
	if len(pairs) != len(wantPairs) {
		t.Fatalf("block_refs after the citing edit = %#v, want %#v", pairs, wantPairs)
	}
	for index, pair := range pairs {
		wanted := wantPairs[index]
		if pair.SourceSeq == nil || *pair.SourceSeq != eventID || !pair.CreatedAt.Equal(wanted.CreatedAt) {
			t.Fatalf("block_refs[%d] = %#v (source_seq %v), want created at %s by event %d", index, pair, pair.SourceSeq, wanted.CreatedAt, eventID)
		}
		pair.SourceSeq, pair.CreatedAt = wanted.SourceSeq, wanted.CreatedAt
		if pair != wanted {
			t.Fatalf("block_refs[%d] = %#v, want %#v", index, pair, wanted)
		}
	}
	requireIndexedAt(t, database, spec, version.Number)
}

// A5: a block's changed version moves only when its own text (its markdown, children included)
// changes. Editing the paragraph inside the callout moves the paragraph and the callout; editing
// another paragraph moves only that one. Deleting the callout records both texts as gone, and
// restoring the callout's id records it anew.
func TestABlocksChangedVersionMovesOnlyWithItsText(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	callout := func(body string) string { return ":::callout{#ship_it kind=\"note\" title=\"\"}\n" + body + "\n:::" }
	issue := createSpecIssue(t, handler, "CORE", callout("Ship it.")+"\n\nQ holds steady.\n\nR holds steady.\n")
	spec := issue.PrimaryArtifactID
	ids := blockIDsByText(t, handler, spec)
	p, q, r := ids["Ship it."], ids["Q holds steady."], ids["R holds steady."]
	if len(ids) != 4 || ids[callout("Ship it.")] != "ship_it" || p == "" || q == "" || r == "" {
		t.Fatalf("GET /blocks = %#v, want the callout, its paragraph P, and Q and R", ids)
	}
	versionAt := map[int]time.Time{1: latestVersion(t, database, spec).CreatedAt}
	requireBlocks := func(stage string, want map[string]int) {
		t.Helper()
		blocks, changedAt := readIndexedBlocks(t, database, spec)
		got := make(map[string]int, len(blocks))
		for id, block := range blocks {
			got[id] = block.ChangedVersion
			if !changedAt[id].Equal(versionAt[block.ChangedVersion]) {
				t.Fatalf("%s: block %q changed_at = %s, want version %d's %s", stage, id, changedAt[id], block.ChangedVersion, versionAt[block.ChangedVersion])
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: changed versions = %#v, want %#v", stage, got, want)
		}
	}
	requireTexts := func(stage string, version int, want map[string]*string) {
		t.Helper()
		if got := readBlockTexts(t, database, spec, version); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: block texts of version %d = %#v, want %#v", stage, version, got, want)
		}
	}
	requireBlocks("version 1", map[string]int{"ship_it": 1, p: 1, q: 1, r: 1})

	edited := editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "Ship it.", "with": "Ship it now."}})
	versionAt[edited.Number] = edited.CreatedAt
	requireBlocks("P edited", map[string]int{"ship_it": edited.Number, p: edited.Number, q: 1, r: 1})
	requireTexts("P edited", edited.Number, map[string]*string{"ship_it": new(callout("Ship it now.")), p: new("Ship it now.")})

	movedQ := editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "Q holds steady.", "with": "Q moves."}})
	versionAt[movedQ.Number] = movedQ.CreatedAt
	requireBlocks("Q edited", map[string]int{"ship_it": edited.Number, p: edited.Number, q: movedQ.Number, r: 1})
	requireTexts("Q edited", movedQ.Number, map[string]*string{q: new("Q moves.")})

	deleted := editSpec(t, handler, issue.Key, []map[string]any{{"op": "delete", "block": "ship_it"}})
	versionAt[deleted.Number] = deleted.CreatedAt
	requireBlocks("callout deleted", map[string]int{q: movedQ.Number, r: 1})
	requireTexts("callout deleted", deleted.Number, map[string]*string{"ship_it": nil, p: nil})

	restored := editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": callout("Ship it now.")}})
	versionAt[restored.Number] = restored.CreatedAt
	restoredP := blockIDsByText(t, handler, spec)["Ship it now."]
	requireBlocks("callout restored", map[string]int{"ship_it": restored.Number, restoredP: restored.Number, q: movedQ.Number, r: 1})
	requireTexts("callout restored", restored.Number, map[string]*string{"ship_it": new(callout("Ship it now.")), restoredP: new("Ship it now.")})
	requireIndexedAt(t, database, spec, restored.Number)
}

// A node one document cites from two blocks is cited twice, so a version that moves a citation
// from one citing block to two, or back, moves the node's count even though the document cites it
// throughout: each such version's event names it, and the count reads one per citing block plus
// the target's own spec, attached_to it: 2, 3, 2.
func TestAVersionWriteNamesATargetWhoseBlockCountChanged(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	target := createReferenceAPIIssue(t, handler, "CORE")
	cite := "dispatch://" + target.Key
	source := createSpecIssue(t, handler, "OPS", "First cites "+cite+".\n\nSecond paragraph.\n")
	events := "/api/v1/issues/" + source.Key + "/events"
	named := []model.ChangedReference{{Kind: "issue", ID: target.Key}}
	requireCitations := func(stage string, blocks, count int) {
		t.Helper()
		var got int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from graph_edges where from_kind = 'block' and to_kind = 'issue' and to_id = $1
		`, target.Key).Scan(&got); err != nil {
			t.Fatalf("count block citations: %v", err)
		}
		if got != blocks {
			t.Fatalf("%s: blocks citing %s = %d, want %d", stage, target.Key, got, blocks)
		}
		requireIssueBacklinkCount(t, handler, target.Key, count)
	}
	requireCitations("one citing block", 1, 2)

	editSpec(t, handler, source.Key, []map[string]any{{"op": "replace", "find": "Second paragraph.", "with": "Second cites " + cite + "."}})
	requireArtifactEventNames(t, handler, events, source.PrimaryArtifactID, named)
	requireCitations("two citing blocks", 2, 3)

	editSpec(t, handler, source.Key, []map[string]any{{"op": "replace", "find": "Second cites " + cite + ".", "with": "Second paragraph."}})
	requireArtifactEventNames(t, handler, events, source.PrimaryArtifactID, named)
	requireCitations("one citing block again", 1, 2)
}

// Migration 0118's trigger marks a document owed on every version insert; each path a block-aware
// build writes a version through indexes that version's blocks in the same transaction, so none
// leaves its document owed: issue creation, the edit route, the upload route (a new document and
// a replacement) and an accepted suggestion.
func TestEveryVersionWriteLeavesItsDocumentIndexed(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	requireWritten := func(path, artifactID, text string) {
		t.Helper()
		version := latestVersion(t, database, artifactID)
		requireIndexedAt(t, database, artifactID, version.Number)
		blocks, _ := readIndexedBlocks(t, database, artifactID)
		for _, block := range blocks {
			if block.Excerpt == text {
				return
			}
		}
		t.Fatalf("%s: version %d of %s indexed blocks %#v, none of them %q", path, version.Number, artifactID, blocks, text)
	}

	issue := createSpecIssue(t, handler, "CORE", "Created paragraph.\n")
	spec := issue.PrimaryArtifactID
	requireWritten("issue creation", spec, "Created paragraph.")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "Created paragraph.", "with": "Edited paragraph."}})
	requireWritten("edit route", spec, "Edited paragraph.")

	uploaded := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "notes.md", "text/markdown", []byte("Uploaded paragraph.\n"), "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload a document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact.ID
	requireWritten("upload of a new document", notes, "Uploaded paragraph.")
	if replaced := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "notes.md"}, "notes.md", "text/markdown", []byte("Replaced paragraph.\n"), "alice"); replaced.Code != http.StatusCreated {
		t.Fatalf("replace the document: status=%d body=%s", replaced.Code, replaced.Body.String())
	}
	requireWritten("upload replacing a document", notes, "Replaced paragraph.")

	suggestion := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor":     map[string]any{"artifact": "spec", "quote": "Edited paragraph."},
		"body":       "Reword it.",
		"suggestion": map[string]any{"replace_with": "Suggested paragraph."},
	}, "alice")
	if suggestion.Code != http.StatusCreated {
		t.Fatalf("suggest a rewording: status=%d body=%s", suggestion.Code, suggestion.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+decodeBody[writtenRecord](t, suggestion).ID+"/accept", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("accept the suggestion: status=%d body=%s", response.Code, response.Body.String())
	}
	requireWritten("accepted suggestion", spec, "Suggested paragraph.")
}

// shipIt is a callout written with its id, ship_it, whose `_` a block reference encodes.
func shipIt(body string) string {
	return ":::callout{#ship_it kind=\"note\" title=\"\"}\n" + body + "\n:::"
}

// createShipItSpec creates an issue in project whose spec is the callout ship_it holding "Ship it."
// and a paragraph after it, and returns the issue with the reference to the callout.
func createShipItSpec(t *testing.T, handler http.Handler, project string) (model.Issue, string) {
	t.Helper()
	issue := createSpecIssue(t, handler, project, shipIt("Ship it.")+"\n\nOther paragraph.\n")
	return issue, "dispatch://" + issue.Key + "/spec#b-ship%5Fit"
}

// requireReferenceNotFound fails unless the references read of query answers 404 NOT_FOUND
// saying message.
func requireReferenceNotFound(t *testing.T, handler http.Handler, query url.Values, message string) {
	t.Helper()
	response := graphRequest(t, handler, query)
	if response.Code != http.StatusNotFound {
		t.Fatalf("GET /api/v1/references?%s: status=%d body=%s, want 404", query.Encode(), response.Code, response.Body.String())
	}
	if body := decodeBody[apiErrorBody](t, response); body.Code != "NOT_FOUND" || body.Error != message {
		t.Fatalf("GET /api/v1/references?%s = %#v, want NOT_FOUND %q", query.Encode(), body, message)
	}
}

// A1: a comment that cites a block is listed on the block alone, and on its document as a link
// at that block; read from the comment, its one link is the block. The document's blocks are
// part_of it, an edge its read lists only when asked for.
func TestReferencesToABlockListOnlyLinksNamingIt(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue, blockRef := createShipItSpec(t, handler, "CORE")
	specRef := "dispatch://" + issue.Key + "/spec"
	nodeID := issue.Key + "/spec#ship_it"
	citesBlock := createReferenceAPIComment(t, handler, issue.Key, "Decided in "+blockRef+".")
	citesSpec := createReferenceAPIComment(t, handler, issue.Key, "See "+specRef+".")

	toBlock := graphEdges(t, handler, url.Values{"to": {blockRef}})
	issueKey := issue.Key
	changedAt := latestVersion(t, database, issue.PrimaryArtifactID).CreatedAt
	wantNode := model.GraphNode{
		Kind: "block", ID: nodeID, BlockID: "ship_it", IssueKey: &issueKey, Project: "CORE", Ref: blockRef,
		Document: specRef, ChangedVersion: 1, ChangedAt: &changedAt, Text: shipIt("Ship it."),
	}
	if toBlock.Node.ChangedAt == nil || !toBlock.Node.ChangedAt.Equal(changedAt) {
		t.Fatalf("block node changed_at = %v, want version 1's %s", toBlock.Node.ChangedAt, changedAt)
	}
	toBlock.Node.ChangedAt = &changedAt
	if !reflect.DeepEqual(toBlock.Node, wantNode) {
		t.Fatalf("block node = %#v, want %#v", toBlock.Node, wantNode)
	}
	if len(toBlock.Edges) != 1 || toBlock.Edges[0].Kind != "mentions" || toBlock.Edges[0].Node.Kind != "comment" || toBlock.Edges[0].Node.ID != citesBlock.ID {
		t.Fatalf("links to the block = %#v, want comment %s's mention alone", toBlock.Edges, citesBlock.ID)
	}

	toSpec := graphEdges(t, handler, url.Values{"to": {specRef}})
	atBlock := map[string]string{}
	for _, edge := range toSpec.Edges {
		if edge.Kind != "mentions" || edge.Node.Kind != "comment" {
			t.Fatalf("links to the spec = %#v, want the two comments' mentions alone, no part_of", toSpec.Edges)
		}
		atBlock[edge.Node.ID] = ""
		if edge.Block != nil {
			atBlock[edge.Node.ID] = edge.Block.BlockID
		}
	}
	if want := map[string]string{citesBlock.ID: "ship_it", citesSpec.ID: ""}; len(toSpec.Edges) != 2 || !reflect.DeepEqual(atBlock, want) {
		t.Fatalf("links to the spec by the block each names = %#v, want %#v", atBlock, want)
	}

	partOf := graphEdges(t, handler, url.Values{"to": {specRef}, "kind": {"part_of"}})
	if len(partOf.Edges) != len(blockIDsByText(t, handler, issue.PrimaryArtifactID)) {
		t.Fatalf("part_of edges of the spec = %#v, want one per spec block", partOf.Edges)
	}

	fromComment := graphEdges(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/comment/" + citesBlock.ID}})
	if len(fromComment.Edges) != 1 || fromComment.Edges[0].Kind != "mentions" || fromComment.Edges[0].Node.Kind != "block" || fromComment.Edges[0].Node.ID != nodeID {
		t.Fatalf("links of the comment citing the block = %#v, want one mention of the block and none of the spec", fromComment.Edges)
	}
	if excerpt := fromComment.Edges[0].Excerpt; excerpt == nil || *excerpt != (model.GraphExcerpt{BlockID: "ship_it", Text: shipIt("Ship it.")}) {
		t.Fatalf("excerpt of the cited block = %#v, want the callout's text", excerpt)
	}
}

// A2: deleting a block takes it out of the graph, so its reference answers 404 naming it; a
// version that holds its id again brings back every link written to it.
func TestReferencesToADeletedBlockAnswer404AndReturnWithTheBlock(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue, blockRef := createShipItSpec(t, handler, "CORE")
	comment := createReferenceAPIComment(t, handler, issue.Key, "Decided in "+blockRef+".")

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "delete", "block": "ship_it"}})
	requireReferenceNotFound(t, handler, url.Values{"to": {blockRef}}, `no block "ship_it" in dispatch://`+issue.Key+`/spec`)
	requireReferenceNotFound(t, handler, url.Values{"from": {blockRef}}, `no block "ship_it" in dispatch://`+issue.Key+`/spec`)

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "markdown": shipIt("Ship it."), "before": "Other paragraph."}})
	edges := graphEdges(t, handler, url.Values{"to": {blockRef}}).Edges
	if len(edges) != 1 || edges[0].Node.Kind != "comment" || edges[0].Node.ID != comment.ID {
		t.Fatalf("links to the restored block = %#v, want comment %s's mention again", edges, comment.ID)
	}
}

// R7: a document's own links are listed once each, from the block holding it.
func TestReferencesFromADocumentNameTheBlockHoldingEachLink(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	target := createReferenceAPIIssue(t, handler, "CORE")
	paragraph := "The plan cites dispatch://" + target.Key + " here."
	issue := createSpecIssue(t, handler, "CORE", "Intro paragraph.\n\n"+paragraph+"\n")
	paragraphID := blockIDsByText(t, handler, issue.PrimaryArtifactID)[paragraph]

	edges := graphEdges(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/spec"}, "kind": {"mentions"}}).Edges
	if len(edges) != 1 || edges[0].Node.Kind != "issue" || edges[0].Node.ID != target.Key {
		t.Fatalf("links of the spec = %#v, want its one mention of %s", edges, target.Key)
	}
	if block := edges[0].Block; paragraphID == "" || block == nil || block.Kind != "block" || block.BlockID != paragraphID || block.ID != issue.Key+"/spec#"+paragraphID {
		t.Fatalf("block holding the link = %#v, want the paragraph %q", block, paragraphID)
	}
}

// R7: a mention a document's block holds is listed from that block, with the block's text as its
// excerpt: the excerpt the document's own row showed before.
func TestAMentionABlockHoldsIsListedFromTheBlock(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	target := createReferenceAPIIssue(t, handler, "CORE")
	paragraph := "The plan cites dispatch://" + target.Key + " here."
	issue := createSpecIssue(t, handler, "CORE", "Intro paragraph.\n\n"+paragraph+"\n")
	paragraphID := blockIDsByText(t, handler, issue.PrimaryArtifactID)[paragraph]

	edges := graphEdges(t, handler, url.Values{"to": {"dispatch://" + target.Key}, "kind": {"mentions"}}).Edges
	if len(edges) != 1 || edges[0].Node.Kind != "block" || edges[0].Node.Document != "dispatch://"+issue.Key+"/spec" || edges[0].Node.BlockID != paragraphID {
		t.Fatalf("mentions of %s = %#v, want one from the spec's paragraph %q", target.Key, edges, paragraphID)
	}
	if excerpt := edges[0].Excerpt; excerpt == nil || *excerpt != (model.GraphExcerpt{BlockID: paragraphID, Text: paragraph}) {
		t.Fatalf("excerpt = %#v, want the paragraph %q", excerpt, paragraph)
	}
}

// A node one document cites from two blocks is cited twice: its count and its list each hold
// both blocks' mentions, beside the target's own spec, attached_to it.
func TestBacklinkCountsCountEachCitingBlock(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	target := createReferenceAPIIssue(t, handler, "CORE")
	cite := "dispatch://" + target.Key
	issue := createSpecIssue(t, handler, "CORE", "First cites "+cite+".\n\nSecond cites "+cite+".\n")

	requireIssueBacklinkCount(t, handler, target.Key, 3)
	edges := graphEdges(t, handler, url.Values{"to": {cite}}).Edges
	citing := map[string]bool{}
	for _, edge := range edges {
		if edge.Kind == "mentions" && edge.Node.Kind == "block" && edge.Node.Document == "dispatch://"+issue.Key+"/spec" {
			citing[edge.Node.ID] = true
		}
	}
	if len(edges) != 3 || len(citing) != 2 {
		t.Fatalf("links to %s = %#v, want two citing blocks and the attached spec", target.Key, edges)
	}
}

// Contract item 11: the older references route loses no source. A document that cites only a
// block of the spec, and a comment that cites the block and the spec, each reference the spec
// once; the citing document's outgoing link is the block, carrying the spec as its document.
func TestArtifactReferencesKeepADocumentThatCitesOnlyABlock(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue, blockRef := createShipItSpec(t, handler, "CORE")
	notes := createProjectDocument(t, handler, "CORE", "Notes", "Decision: "+blockRef+"\n")
	comment := createReferenceAPIComment(t, handler, issue.Key, "See "+blockRef+" in dispatch://"+issue.Key+"/spec.")

	spec := decodeBody[model.ArtifactReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/references", nil, "alice"))
	sources := make([][2]string, len(spec.ReferencedBy))
	for index, reference := range spec.ReferencedBy {
		sources[index] = [2]string{reference.Kind, reference.ID}
	}
	if want := [][2]string{{"artifact", notes.ID}, {"comment", comment.ID}}; !reflect.DeepEqual(sources, want) {
		t.Fatalf("spec referenced_by = %v, want %v", sources, want)
	}

	outgoing := decodeBody[model.ArtifactReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+notes.ID+"/references", nil, "alice")).Outgoing
	if len(outgoing) != 1 || outgoing[0].Kind != "block" || outgoing[0].ToID != issue.Key+"/spec#ship_it" || outgoing[0].Artifact == nil || outgoing[0].Artifact.ID != issue.PrimaryArtifactID {
		t.Fatalf("notes outgoing = %#v, want the block ship_it carrying the spec", outgoing)
	}
}

// R6, A10: every block GET …/blocks lists is a node part_of its document, and no other is.
func TestEveryBlockIsPartOfItsDocument(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "# Plan\n\n"+shipIt("Ship it.")+"\n\n- First item\n- Second item\n\nClosing paragraph.\n")
	specRef := "dispatch://" + issue.Key + "/spec"

	want := map[string]bool{}
	for _, id := range blockIDsByText(t, handler, issue.PrimaryArtifactID) {
		want[issue.Key+"/spec#"+id] = true
	}
	got := map[string]bool{}
	for _, edge := range graphEdges(t, handler, url.Values{"to": {specRef}, "kind": {"part_of"}}).Edges {
		if edge.Kind != "part_of" || edge.Node.Kind != "block" || edge.Node.Document != specRef {
			t.Fatalf("part_of edge = %#v, want a block of %s", edge, specRef)
		}
		got[edge.Node.ID] = true
	}
	if len(want) < 6 || !reflect.DeepEqual(got, want) {
		t.Fatalf("blocks part_of the spec = %v, want each block GET /blocks lists: %v", got, want)
	}
}

// R7, contract item 2: list items and table cells are not nodes, so a link in a list item starts
// at the item's paragraph and one in a table cell starts at the table.
func TestALinkInAListItemStartsAtTheItemsParagraph(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	inItem := createReferenceAPIIssue(t, handler, "CORE")
	inCell := createReferenceAPIIssue(t, handler, "OPS")
	item := "Track dispatch://" + inItem.Key + " first"
	issue := createSpecIssue(t, handler, "CORE", "- "+item+"\n- Then the rest\n\n| Name | Link |\n| --- | --- |\n| row | dispatch://"+inCell.Key+" |\n")
	blocks := decodeBody[[]model.ArtifactBlock](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	tableID := ""
	for _, block := range blocks {
		if block.Type == "table" {
			tableID = block.ID
		}
	}
	itemID := blockIDsByText(t, handler, issue.PrimaryArtifactID)[item]
	if itemID == "" || tableID == "" {
		t.Fatalf("GET /blocks = %#v, want the item's paragraph and the table", blocks)
	}

	for target, holder := range map[string]string{inItem.Key: itemID, inCell.Key: tableID} {
		edges := graphEdges(t, handler, url.Values{"to": {"dispatch://" + target}, "kind": {"mentions"}}).Edges
		if len(edges) != 1 || edges[0].Node.Kind != "block" || edges[0].Node.BlockID != holder {
			t.Fatalf("mentions of %s = %#v, want one from block %q", target, edges, holder)
		}
	}
}

// A2, design 4: a link to a block counts only while a current version holds the block. Deleting it
// takes the link out of the document's list, the block's own read, the citing document's links
// and outgoing list, the citing issue's closure and the block links between the two documents;
// restoring the block with its id brings every one back.
func TestADeletedBlockLeavesEveryListAndReturnsWithItsID(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	createReferenceAPIProject(t, handler, "OPS")
	core, blockRef := createShipItSpec(t, handler, "CORE")
	specRef := "dispatch://" + core.Key + "/spec"
	nodeID := core.Key + "/spec#ship_it"
	ops := createReferenceAPIIssue(t, handler, "OPS")
	uploaded := multipartRequest(t, handler, "/api/v1/issues/"+ops.Key+"/artifacts", map[string]string{"name": "notes.md"}, "notes.md", "text/markdown", []byte("Decision: "+blockRef+"\n"), "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the citing document: status=%d body=%s", uploaded.Code, uploaded.Body.String())
	}
	notes := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	notesRef := "dispatch://" + ops.Key + "/artifact/" + notes.Slug

	requireListed := func(stage string, listed bool) {
		t.Helper()
		want := 0
		if listed {
			want = 1
		}
		atBlock := 0
		for _, edge := range graphEdges(t, handler, url.Values{"to": {specRef}}).Edges {
			if edge.Block != nil && edge.Block.ID == nodeID && edge.Node.Kind == "block" && edge.Node.Document == notesRef {
				atBlock++
			}
		}
		if atBlock != want {
			t.Fatalf("%s: links to %s at ship_it = %d, want %d", stage, specRef, atBlock, want)
		}
		if listed {
			if edges := graphEdges(t, handler, url.Values{"to": {blockRef}}).Edges; len(edges) != 1 || edges[0].Node.Document != notesRef {
				t.Fatalf("%s: links to the block = %#v, want the notes' block", stage, edges)
			}
		} else {
			requireReferenceNotFound(t, handler, url.Values{"to": {blockRef}}, `no block "ship_it" in `+specRef)
		}
		toBlock := 0
		for _, edge := range graphEdges(t, handler, url.Values{"from": {notesRef}}).Edges {
			if edge.Node.ID == nodeID {
				toBlock++
			}
		}
		if toBlock != want {
			t.Fatalf("%s: links of the notes to the block = %d, want %d", stage, toBlock, want)
		}
		outgoing := decodeBody[model.ArtifactReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+notes.ID+"/references", nil, "alice")).Outgoing
		if len(outgoing) != want || listed && (outgoing[0].ToID != nodeID || outgoing[0].Artifact == nil || outgoing[0].Artifact.ID != core.PrimaryArtifactID) {
			t.Fatalf("%s: notes outgoing = %#v, want %d entries for the block", stage, outgoing, want)
		}
		response := issueReferencesRequest(t, handler, ops.Key, "")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: closure of %s: status=%d body=%s", stage, ops.Key, response.Code, response.Body.String())
		}
		reached := 0
		for _, member := range decodeBody[model.IssueReferences](t, response).Members {
			if member.Artifact.ID == core.PrimaryArtifactID {
				reached++
			}
		}
		if reached != want {
			t.Fatalf("%s: closure of %s reaches %s %d times, want %d", stage, ops.Key, specRef, reached, want)
		}
		links := readBlockLinks(t, handler, notesRef, specRef).Links
		if len(links) != want || listed && (links[0].From.Document != notesRef || links[0].To.ID != nodeID) {
			t.Fatalf("%s: block links from the notes to %s = %#v, want %d to the block", stage, specRef, links, want)
		}
	}
	requireListed("cited", true)

	editSpec(t, handler, core.Key, []map[string]any{{"op": "delete", "block": "ship_it"}})
	requireListed("block deleted", false)

	editSpec(t, handler, core.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": shipIt("Ship it.")}})
	requireListed("block restored", true)
}

// pinnedParagraph is a 600-rune paragraph whose runes 500-520 are middle, so a version that
// rewrites only those runes changes the paragraph past the 480 runes an excerpt shows.
func pinnedParagraph(middle string) string {
	return strings.Repeat("abcé ", 100) + middle + strings.Repeat(" tail", 16)
}

// A15: a reference pinned to a version shows the block's whole text at that version and says it
// changed since; the unpinned reference shows the whole current text. An edge's end at the block
// carries only the excerpt.
func TestAPinnedReferenceShowsItsVersionsText(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	original, changed := pinnedParagraph("ORIGINALMIDDLETEXT20"), pinnedParagraph("REPLACEDMIDDLETEXT20")
	if runes := len([]rune(original)); runes != 600 {
		t.Fatalf("paragraph is %d runes, want 600", runes)
	}
	issue := createSpecIssue(t, handler, "CORE", "Intro paragraph.\n\n"+original+"\n")
	paragraphID := blockIDsByText(t, handler, issue.PrimaryArtifactID)[original]
	if paragraphID == "" {
		t.Fatalf("GET /blocks lists no block of text %q", original)
	}
	paragraphRef := "dispatch://" + issue.Key + "/spec#b-" + text.EncodeBlockID(paragraphID)
	comment := createReferenceAPIComment(t, handler, issue.Key, "Read "+paragraphRef+" as "+paragraphRef+"@v1.")
	version := editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "ORIGINALMIDDLETEXT20", "with": "REPLACEDMIDDLETEXT20"}})

	unpinned := graphEdges(t, handler, url.Values{"to": {paragraphRef}})
	if unpinned.Node.Text != changed || unpinned.Node.Pin != nil || unpinned.Node.ChangedVersion != version.Number {
		t.Fatalf("unpinned node = %#v, want version %d's whole text and no pin", unpinned.Node, version.Number)
	}
	pinned := graphEdges(t, handler, url.Values{"to": {paragraphRef + "@v1"}})
	if want := (model.BlockPin{Version: 1, Ref: paragraphRef + "@v1", Text: original, State: "changed"}); pinned.Node.Pin == nil || *pinned.Node.Pin != want {
		t.Fatalf("pin = %#v, want %#v", pinned.Node.Pin, want)
	}
	if pinned.Node.ChangedVersion != version.Number || pinned.Node.Text != changed {
		t.Fatalf("pinned node = %#v, want changed in version %d with its current text", pinned.Node, version.Number)
	}
	for _, read := range []model.GraphReferences{unpinned, pinned} {
		if len(read.Edges) != 1 || read.Edges[0].Node.ID != comment.ID {
			t.Fatalf("links to %s = %#v, want the comment's one mention", read.Node.Ref, read.Edges)
		}
	}

	cited := graphEdges(t, handler, url.Values{"from": {"dispatch://" + issue.Key + "/comment/" + comment.ID}}).Edges
	if len(cited) != 1 || cited[0].Node.Text != "" || cited[0].Excerpt == nil || *cited[0].Excerpt != (model.GraphExcerpt{BlockID: paragraphID, Text: text.HeadRunes(changed, refs.BlockExcerptRunes)}) {
		t.Fatalf("the comment's link = %#v, want the block with its 480-rune excerpt", cited)
	}
}

// "Changed" compares the current text with the pinned version's, not the history between: a text
// that changed and went back reads unchanged.
func TestAPinnedReferenceWhoseTextReturnedIsUnchanged(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Intro paragraph.\n\nP says one thing.\n")
	paragraphRef := "dispatch://" + issue.Key + "/spec#b-" + text.EncodeBlockID(blockIDsByText(t, handler, issue.PrimaryArtifactID)["P says one thing."])
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "P says one thing.", "with": "P says another thing."}})
	back := editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "P says another thing.", "with": "P says one thing."}})

	for version, want := range map[int]model.BlockPin{
		1: {Version: 1, Text: "P says one thing.", State: "unchanged"},
		2: {Version: 2, Text: "P says another thing.", State: "changed"},
	} {
		want.Ref = paragraphRef + "@v" + strconv.Itoa(version)
		node := graphEdges(t, handler, url.Values{"to": {want.Ref}}).Node
		if node.Pin == nil || *node.Pin != want || node.ChangedVersion != back.Number || node.Text != "P says one thing." {
			t.Fatalf("node pinned to version %d = %#v (pin %#v), want pin %#v and changed in version %d", version, node, node.Pin, want, back.Number)
		}
	}
}

// A2, pinned: a reference pinned to a version that held a block deleted since still shows that
// version's text and says which version removed it, with no link, since the graph lists none to a
// deleted block; the unpinned reference answers 404. Restored with its id, the block reads as its
// text compares again.
func TestAPinnedReferenceToADeletedBlockSaysItWasRemoved(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue, blockRef := createShipItSpec(t, handler, "CORE")
	specRef := "dispatch://" + issue.Key + "/spec"
	createReferenceAPIComment(t, handler, issue.Key, "Decided in "+blockRef+"@v2.")
	editSpec(t, handler, issue.Key, []map[string]any{{"op": "replace", "find": "Ship it.", "with": "Ship it now."}})
	deleted := editSpec(t, handler, issue.Key, []map[string]any{{"op": "delete", "block": "ship_it"}})

	response := graphRequest(t, handler, url.Values{"to": {blockRef + "@v2"}})
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"edges":[]`) {
		t.Fatalf("pinned read of the deleted block: status=%d body=%s, want 200 with no edges", response.Code, response.Body.String())
	}
	issueKey := issue.Key
	want := model.GraphNode{
		Kind: "block", ID: issue.Key + "/spec#ship_it", BlockID: "ship_it", IssueKey: &issueKey, Project: "CORE", Ref: blockRef, Document: specRef,
		Pin: &model.BlockPin{Version: 2, Ref: blockRef + "@v2", Text: shipIt("Ship it now."), State: "removed", RemovedVersion: deleted.Number},
	}
	if got := decodeBody[model.GraphReferences](t, response).Node; !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned node of the deleted block = %#v (pin %#v), want %#v (pin %#v)", got, got.Pin, want, want.Pin)
	}
	requireReferenceNotFound(t, handler, url.Values{"to": {blockRef}}, `no block "ship_it" in `+specRef)

	editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": shipIt("Ship it now.")}})
	for version, state := range map[int]string{1: "changed", 2: "unchanged"} {
		read := graphEdges(t, handler, url.Values{"to": {blockRef + "@v" + strconv.Itoa(version)}})
		if read.Node.Pin == nil || read.Node.Pin.State != state || len(read.Edges) != 1 {
			t.Fatalf("restored block pinned to version %d = %#v (pin %#v), want %s with the comment's link", version, read.Node, read.Node.Pin, state)
		}
	}
}

// A pin the index cannot read answers 404 saying why: a version the document lacks, a version an
// older build wrote and no block-aware build indexed, and a version that did not hold the block.
func TestAPinnedReferenceAnswers404ForAVersionItCannotRead(t *testing.T) {
	handler, database := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createSpecIssue(t, handler, "CORE", "Intro paragraph.\n")
	specRef := "dispatch://" + issue.Key + "/spec"
	blockRef := specRef + "#b-ship%5Fit"
	added := editSpec(t, handler, issue.Key, []map[string]any{{"op": "insert", "after": "end", "markdown": shipIt("Ship it.")}})
	if pin := graphEdges(t, handler, url.Values{"to": {blockRef + "@v" + strconv.Itoa(added.Number)}}).Node.Pin; pin == nil || pin.State != "unchanged" {
		t.Fatalf("pin to the version that added the block = %#v, want unchanged", pin)
	}
	unindexed := added.Number + 1
	if _, err := database.Pool.Exec(context.Background(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		select artifact_id, $2, markdown, authors from artifact_versions where artifact_id = $1 and number = $3
	`, issue.PrimaryArtifactID, unindexed, added.Number); err != nil {
		t.Fatalf("write a version as an older build would: %v", err)
	}

	requireReferenceNotFound(t, handler, url.Values{"to": {blockRef + "@v9"}}, "no version 9 of "+specRef)
	requireReferenceNotFound(t, handler, url.Values{"to": {blockRef + "@v" + strconv.Itoa(unindexed)}},
		"version "+strconv.Itoa(unindexed)+" of "+specRef+" has no record of its blocks; the earliest is version 1")
	requireReferenceNotFound(t, handler, url.Values{"to": {blockRef + "@v1"}}, `no block "ship_it" in `+specRef+` at version 1`)
}

// A pin whose number no document version can reach - past the largest version number Postgres
// stores, or past what fits in 64 bits but still a well-formed `@v<N>` - names a version the
// document lacks: 404 saying so, as for any other missing version, never a server error.
func TestAPinPastEveryVersionNumberAnswers404(t *testing.T) {
	handler, _ := blockIndexHandler(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue, blockRef := createShipItSpec(t, handler, "CORE")
	specRef := "dispatch://" + issue.Key + "/spec"
	for _, version := range []string{"2147483648", "9223372036854775807"} {
		for _, direction := range []string{"to", "from"} {
			requireReferenceNotFound(t, handler, url.Values{direction: {blockRef + "@v" + version}}, "no version "+version+" of "+specRef)
		}
	}
}

// A block reference whose target id, `<KEY>/<slug>#<block id>`, is exactly text.MaxBlockTargetBytes
// long is a reference the graph holds: a comment citing it and a document citing it are written,
// and the block lists both. The name is incompressible, so no index holds it by compressing it.
// One byte more names nothing, so the API answers 400 INVALID_REFERENCE for it.
func TestABlockReferenceAtTheTargetBoundIsWrittenAndRead(t *testing.T) {
	_, handler, _ := browserDocumentService(t)
	issue := createInteractionIssue(t, handler, "TEST", "Cites a block at the target bound", "Context paragraph.\n")
	// The slug of `<hex>.md` is `<hex>-md`, so the target id `<key>/<hex>-md#x` is the bound.
	name := randomBlockID(t, text.MaxBlockTargetBytes-len(issue.Key+"/-md#x")) + ".md"
	uploaded := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": name, "content": boundCallout("x"),
	}, "alice")
	if uploaded.Code != http.StatusCreated {
		t.Fatalf("upload the cited document: status=%d body=%.400s", uploaded.Code, uploaded.Body.String())
	}
	cited := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, uploaded).Artifact
	if target := issue.Key + "/" + cited.Slug + "#x"; len(target) != text.MaxBlockTargetBytes {
		t.Fatalf("the target id is %d bytes, want the bound %d", len(target), text.MaxBlockTargetBytes)
	}
	block := "dispatch://" + issue.Key + "/artifact/" + cited.Slug + "#b-x"

	comment := createReferenceAPIComment(t, handler, issue.Key, "See "+block+".")
	citerRef := "dispatch://" + issue.Key + "/artifact/citer-md"
	if citer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
		"name": "citer.md", "content": "This document cites " + block + ".\n",
	}, "alice"); citer.Code != http.StatusCreated {
		t.Fatalf("upload a document citing the block: status=%d body=%.400s", citer.Code, citer.Body.String())
	}

	edges := graphEdges(t, handler, url.Values{"to": {block}}).Edges
	var fromComment, fromCiter bool
	for _, edge := range edges {
		fromComment = fromComment || edge.Node.Kind == "comment" && edge.Node.ID == comment.ID
		fromCiter = fromCiter || edge.Node.Kind == "block" && edge.Node.Document == citerRef
	}
	if len(edges) != 2 || !fromComment || !fromCiter {
		t.Fatalf("links to the block = %#v, want the comment's and the citing document's block's", edges)
	}
	for _, direction := range []string{"to", "from"} {
		if read := graphRequest(t, handler, url.Values{direction: {block + "y"}}); read.Code != http.StatusBadRequest || responseCode(t, read) != "INVALID_REFERENCE" {
			t.Fatalf("?%s= a block one byte past the bound: status=%d body=%.400s, want 400 INVALID_REFERENCE", direction, read.Code, read.Body.String())
		}
	}
}
