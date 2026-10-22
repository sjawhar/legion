package api

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// The body contracts.DispatchBodyMax allows is spent twice. Once against the comment cap, which
// counts UTF-16 units; and once, when the comment is anchored to a document, against that
// document's margin budgets, which count bytes and which this change does not move: 256 KiB of
// text per margin record and 1 MiB per document margin, refused 413 CAP_EXCEEDED. What makes that
// refusal reachable is that the margin carries the comment's body whole rather than a summary of
// it, so a body at the cap spends 32,000 bytes of those budgets in ASCII and up to three times
// that outside it. This test holds the anchored path at the cap: the comment is accepted, and the
// margin record read back carries the body whole (INFRA-45).
func TestAnchoredCommentAtTheBodyCapReachesTheMarginWhole(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchored body cap", "The quick brown fox")
	atCap := strings.Repeat("a", contracts.DispatchBodyMax)

	comment := createThreadComment(t, handler, issue.Key, map[string]any{
		"body":   atCap,
		"anchor": map[string]any{"artifact": "spec", "quote": "quick brown fox"},
	}, "alice")
	if comment.Body != atCap {
		t.Fatalf("the comment written back is %d units, want the %d sent", len16(comment.Body), contracts.DispatchBodyMax)
	}

	projection := loadMarkProjection(t, database, issue.PrimaryArtifactID, comment.ID)
	text, ok := projection["text"].(string)
	if !ok {
		t.Fatalf("the margin record of the anchored comment = %#v, want its text", projection)
	}
	if text != atCap {
		t.Fatalf("the margin record holds %d UTF-16 units, want the body of %d sent whole; a margin holding a summary instead would spend less of its 256 KiB record budget than the cap allows",
			len16(text), contracts.DispatchBodyMax)
	}
}
