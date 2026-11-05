package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// paragraphBlockIDs are the ids of the document's paragraph blocks, sorted as the references
// read sorts the blocks it names.
func paragraphBlockIDs(t *testing.T, handler http.Handler, artifactID string) []string {
	t.Helper()
	ids := []string{}
	for _, block := range decodeBody[[]model.ArtifactBlock](t, dispatchRequest(
		t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice")) {
		if block.Type == "paragraph" {
			ids = append(ids, block.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// The references read names the blocks citing each document it answers. The answer keeps one
// member per document, so a document two blocks of one citer cite is answered once, naming both
// blocks; a document no block cites — here one whose citer the block index has not reached, the
// state every document was in before migration 0121 — is answered with none rather than an empty
// name.
func TestIssueReferencesNameTheBlocksCitingEachDocument(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	createReferenceAPIProject(t, handler, "CORE")
	issue := createReferenceAPIIssue(t, handler, "CORE")
	twiceCited := createProjectDocument(t, handler, "CORE", "Twice cited", "# Twice cited\n")
	onceCited := createProjectDocument(t, handler, "CORE", "Once cited", "# Once cited\n")
	citer := createProjectDocument(t, handler, "CORE", "Citing notes",
		"# Citing notes\n\nFirst, see dispatch://CORE/artifact/twice-cited\n\nAgain, see dispatch://CORE/artifact/twice-cited\n")
	unindexed := createProjectDocument(t, handler, "CORE", "Unindexed notes",
		"# Unindexed notes\n\nSee dispatch://CORE/artifact/once-cited\n")
	if _, err := database.Pool.Exec(context.Background(),
		`delete from block_refs where artifact_id = $1`, unindexed.ID); err != nil {
		t.Fatalf("leave the citing document's blocks unindexed: %v", err)
	}
	createReferenceAPIComment(t, handler, issue.Key,
		"See dispatch://CORE/artifact/citing-notes and dispatch://CORE/artifact/unindexed-notes")

	response := issueReferencesRequest(t, handler, issue.Key, "")
	if response.Code != http.StatusOK {
		t.Fatalf("read the issue's references: status=%d body=%s", response.Code, response.Body.String())
	}
	answer := decodeBody[model.IssueReferences](t, response)
	members := map[string]model.ReferenceMember{}
	for _, member := range answer.Members {
		members[member.Artifact.ID] = member
	}
	if answer.Truncated || len(members) != 4 {
		t.Fatalf("the read answered %#v; want the two citers and the two documents they cite", answer)
	}

	twice := members[twiceCited.ID]
	wantBlocks := paragraphBlockIDs(t, handler, citer.ID)
	if len(wantBlocks) != 2 {
		t.Fatalf("the citing document holds %d paragraphs, want the two that cite: %v", len(wantBlocks), wantBlocks)
	}
	if twice.Via != (model.ReferenceVia{Kind: "artifact", ID: citer.ID}) {
		t.Fatalf("the twice-cited document's via = %#v, want the citing document", twice.Via)
	}
	if !slices.Equal(twice.ViaBlocks, wantBlocks) {
		t.Errorf("the twice-cited document's via_blocks = %v, want both citing blocks %v", twice.ViaBlocks, wantBlocks)
	}

	once := members[onceCited.ID]
	if once.Via != (model.ReferenceVia{Kind: "artifact", ID: unindexed.ID}) {
		t.Fatalf("the once-cited document's via = %#v, want the unindexed document", once.Via)
	}
	if once.ViaBlocks != nil {
		t.Errorf("the once-cited document's via_blocks = %v, want none: no block of its citer holds the citation", once.ViaBlocks)
	}
	if got := strings.Count(response.Body.String(), `"via_blocks":null`); got != 1 {
		t.Errorf("the answer carries %d null via_blocks, want the one member no block cites:\n%s", got, response.Body.String())
	}
}

// The ETag covers the blocks citing a member: a member no block cites, one a block cites and one
// the citation moved to another block of the same document hash to three different tags. Without
// that, a client holding the tag from before the move would be told 304 for an answer naming the
// other block, since neither the issue's last_seq nor any member's version need have moved with
// a citation that stayed inside one document.
func TestTheReferencesETagCoversTheBlocksCitingAMember(t *testing.T) {
	member := model.ReferenceMember{
		Artifact: model.Artifact{
			ID:       "4a2b4e8c-6d0a-4f44-9a7e-2f6c6d9a1b23",
			Versions: []model.Version{{Number: 3}},
		},
		Depth: 2,
		Via:   model.ReferenceVia{Kind: "artifact", ID: "9f1c8b7a-2d34-4c55-8e66-7a8b9c0d1e2f"},
	}
	tag := func(blocks ...string) string {
		member.ViaBlocks = blocks
		return referencesETag(7, []model.ReferenceMember{member}, false, false)
	}
	cited, atOneBlock, atAnother := tag(), tag("b2"), tag("b3")
	if cited == atOneBlock || cited == atAnother || atOneBlock == atAnother {
		t.Fatalf("the ETags of no block (%s), b2 (%s) and b3 (%s) are not three distinct tags",
			cited, atOneBlock, atAnother)
	}
}
