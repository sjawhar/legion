package api

import (
	"net/http"
	"testing"
)

type blockWithReferences struct {
	ID         string `json:"id"`
	References struct {
		Asks     int `json:"asks"`
		Comments int `json:"comments"`
	} `json:"references"`
}

type anchoredRecord struct {
	Anchor struct {
		BlockID string `json:"block_id"`
	} `json:"anchor"`
}

func TestQuoteAnchorsRecordBlockAndBlocksCountReferences(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Block anchor", "Introduction.\n\nTarget passage.\n")

	blocks := decodeBody[[]blockWithReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v, want introduction and target", blocks)
	}
	target := blocks[1]

	comment := decodeBody[anchoredRecord](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor": map[string]string{"artifact": "spec", "quote": "Target passage"},
		"body":   "Please clarify this.",
	}, "alice"))
	if comment.Anchor.BlockID != target.ID {
		t.Fatalf("comment anchor block_id = %q, want %q", comment.Anchor.BlockID, target.ID)
	}

	ask := decodeBody[anchoredRecord](t, dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"anchor":   map[string]string{"artifact": "spec", "quote": "Target passage"},
		"question": "Is this complete?",
	}, "alice"))
	if ask.Anchor.BlockID != target.ID {
		t.Fatalf("ask anchor block_id = %q, want %q", ask.Anchor.BlockID, target.ID)
	}

	blocks = decodeBody[[]blockWithReferences](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice"))
	if got := blocks[1].References; got.Comments != 1 || got.Asks != 1 {
		t.Fatalf("target block references = %#v, want one comment and one ask", got)
	}
}

func TestQuoteAnchorAcrossTopLevelBlocksKeepsBlockIDNull(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Cross-block anchor", "One.\n\nTwo.\n")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"anchor": map[string]string{"artifact": "spec", "quote": "One. Two."},
		"body":   "This crosses paragraphs.",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create cross-block comment: status=%d body=%s", created.Code, created.Body.String())
	}
	comment := decodeBody[struct {
		Anchor struct {
			BlockID *string `json:"block_id"`
		} `json:"anchor"`
	}](t, created)
	if comment.Anchor.BlockID != nil {
		t.Fatalf("cross-block anchor block_id = %q, want null", *comment.Anchor.BlockID)
	}
}
