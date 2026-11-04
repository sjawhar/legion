package api

import (
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/itemblocks"
)

// What bounds an anchored comment is its document's margin budgets, not any bound on the length
// of its body. The block bounds count UTF-16 units and hold each block on its own, so a body of
// several blocks is far longer than the single 32,000-unit cap that stood before; the margin
// budgets count bytes and did not move: 256 KiB of text per margin record and 1 MiB per document
// margin, refused 413 CAP_EXCEEDED. What makes that refusal reachable is that the margin carries
// the comment's body whole rather than a summary of it, so one anchored comment can spend a
// quarter of a record's budget in ASCII and up to three times that outside it. This test holds
// the anchored path at a body of six 20,000-unit paragraphs: the comment is accepted, and the
// margin record read back carries it whole (INFRA-370).
func TestAnchoredCommentOfManyBlocksReachesTheMarginWhole(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchored body bounds", "The quick brown fox")
	body := paragraphBody(6, 20_000)

	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":   body,
		"anchor": map[string]any{"artifact": "spec", "quote": "quick brown fox"},
	}, "alice")
	if comment.Body != body {
		t.Fatalf("the comment written back is %d units, want the %d sent", itemblocks.Units16(comment.Body), itemblocks.Units16(body))
	}

	projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, comment.ID)
	text, ok := projection["text"].(string)
	if !ok {
		t.Fatalf("the margin record of the anchored comment = %#v, want its text", projection)
	}
	if text != body {
		t.Fatalf("the margin record holds %d UTF-16 units, want the body of %d sent whole; a margin holding a summary instead would spend less of its 256 KiB record budget than the bounds allow",
			itemblocks.Units16(text), itemblocks.Units16(body))
	}
}
