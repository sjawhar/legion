package api

import (
	"context"
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
		Token    string `json:"token"`
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
		if block.ID == "" || block.Token == "" || seen[block.ID] || block.From < 0 || block.To < block.From || block.To > len(text.Markdown) {
			t.Fatalf("invalid block %#v for markdown %q", block, text.Markdown)
		}
		seen[block.ID] = true
	}
	if text.Token == "" {
		t.Fatal("document text omitted its optimistic-concurrency token")
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

// Every blocks route says where meaning search stands on each block it lists: pending while a
// window covering the block waits for its vector, embedded once every one of them holds a vector
// of the text the window has now, and none for a block no window covers - what every block of an
// item reads while its windows are not written, as an item a build before them wrote has none
// until the block-index tick reaches it.
func TestTheBlocksRoutesSayWhereMeaningSearchStands(t *testing.T) {
	embedder := &fakeEmbedder{vectors: map[string][]float32{}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "EMBD", "Block embedding", "# Heading\n\nSecond paragraph\n")
	comment := createThreadComment(t, handler, issue.Key, map[string]any{"body": "Alpha.\n\nBeta."}, "alice")
	message := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "One.\n\nTwo."}, "alice")
	documentBlocks := func(step string) []model.ArtifactBlock {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/blocks", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: get the document's blocks: status=%d body=%s", step, response.Code, response.Body.String())
		}
		return decodeBody[[]model.ArtifactBlock](t, response)
	}
	itemBlocks := func(step, path string) []model.ItemBlock {
		t.Helper()
		response := dispatchRequest(t, handler, http.MethodGet, path, nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: GET %s: status=%d body=%s", step, path, response.Code, response.Body.String())
		}
		return decodeBody[model.ItemBlocks](t, response).Blocks
	}
	requireEvery := func(step, what string, states []string, want string) {
		t.Helper()
		if len(states) < 2 {
			t.Fatalf("%s: the %s lists %d blocks, want at least two", step, what, len(states))
		}
		for index, state := range states {
			if state != want {
				t.Fatalf("%s: the %s's block %d reads embedding %q, want %q", step, what, index, state, want)
			}
		}
	}
	requireAll := func(step, want string) {
		t.Helper()
		document := make([]string, 0, 2)
		for _, block := range documentBlocks(step) {
			document = append(document, block.Embedding)
		}
		requireEvery(step, "document", document, want)
		for what, path := range map[string]string{
			"comment": "/api/v1/comments/" + comment.ID + "/blocks",
			"message": "/api/v1/messages/" + message.ID + "/blocks",
		} {
			states := make([]string, 0, 2)
			for _, block := range itemBlocks(step, path) {
				states = append(states, block.Embedding)
			}
			requireEvery(step, what, states, want)
		}
	}

	requireAll("before the queue runs", model.BlockEmbeddingPending)
	processAllPending(t, database, embedder)
	requireAll("once the queue drained", model.BlockEmbedded)

	// An edit leaves the comment's own window waiting again, where the message's, untouched,
	// stays current.
	requireEdit(t, "reword", editCommentAs(t, handler, comment.ID, "Alpha reworded.\n\nBeta.", nil, nil))
	for index, block := range itemBlocks("after an edit", "/api/v1/comments/"+comment.ID+"/blocks") {
		if block.Embedding != model.BlockEmbeddingPending {
			t.Fatalf("after an edit: the comment's block %d reads embedding %q, want %q", index, block.Embedding, model.BlockEmbeddingPending)
		}
	}

	// An item whose windows are not written - what a build before them left behind - reads none
	// for every block, and no read writes one.
	if _, err := database.Pool.Exec(context.Background(),
		`delete from embeddings where kind = 'message_block' and split_part(id, '#', 1) = 'message/' || $1`, message.ID); err != nil {
		t.Fatalf("drop the message's windows: %v", err)
	}
	for index, block := range itemBlocks("with no window", "/api/v1/messages/"+message.ID+"/blocks") {
		if block.Embedding != model.BlockNotEmbedded {
			t.Fatalf("with no window: the message's block %d reads embedding %q, want %q", index, block.Embedding, model.BlockNotEmbedded)
		}
	}
}
