package api

import (
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestArtifactBlocksReferenceCanonicalMarkdownByteRanges(t *testing.T) {
	handler := newTestHandler(t)
	issue := createArtifactIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "blocks.md", "content": "# Heading\n\nSecond paragraph\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create document: status=%d body=%s", created.Code, created.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	text := decodeBody[struct {
		Markdown string `json:"markdown"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text", nil, "alice"))
	blocksResponse := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/blocks", nil, "alice")
	if blocksResponse.Code != http.StatusOK {
		t.Fatalf("get blocks: status=%d body=%s", blocksResponse.Code, blocksResponse.Body.String())
	}
	blocks := decodeBody[[]model.ArtifactBlock](t, blocksResponse)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %#v, want heading and paragraph", blocks)
	}
	seen := map[string]bool{}
	for _, block := range blocks {
		if block.ID == "" || seen[block.ID] || block.From < 0 || block.To < block.From || block.To > len(text.Markdown) {
			t.Fatalf("invalid block %#v for markdown %q", block, text.Markdown)
		}
		seen[block.ID] = true
	}
	if got := text.Markdown[blocks[0].From:blocks[0].To]; got != "# Heading" || blocks[0].Type != "heading" {
		t.Fatalf("heading block = %q (%s), want # Heading", got, blocks[0].Type)
	}
	if got := text.Markdown[blocks[1].From:blocks[1].To]; got != "Second paragraph" || blocks[1].Type != "paragraph" {
		t.Fatalf("paragraph block = %q (%s), want Second paragraph", got, blocks[1].Type)
	}
	bySlug := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/artifacts/blocks-md/blocks", nil, "alice")
	if bySlug.Code != http.StatusOK {
		t.Fatalf("get blocks by issue slug: status=%d body=%s", bySlug.Code, bySlug.Body.String())
	}
}
