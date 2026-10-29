package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// openAnchoredAsk opens an ask anchored on the issue document named by slug, at quote.
func openAnchoredAsk(t *testing.T, handler http.Handler, issueKey, slug, quote, question string) string {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/asks", map[string]any{
		"question": question,
		"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
		"anchor":   map[string]any{"artifact": slug, "quote": quote},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the ask anchored on %q: status=%d body=%s", quote, created.Code, created.Body.String())
	}
	return decodeBody[model.Ask](t, created).ID
}

// storeUncastableAnchor rewrites the ask's anchor into the shape only a release before
// asks_anchor_artifact_id (migration 0141) could store: an artifact_id that is not a uuid at all,
// beside a block_artifact_id. asks_anchor_artifact (0032) casts the anchor only where
// block_artifact_id is null, so no index and no census has ever cast this row's anchor, and an
// unguarded cast in a read answers SQLSTATE 22P02 for the whole statement.
func storeUncastableAnchor(t *testing.T, database *store.Store, askID, blockDocument string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update asks
		set anchor = jsonb_set(anchor, '{artifact_id}', '"not-a-uuid"'), block_artifact_id = $2
		where id = $1
	`, askID, blockDocument); err != nil {
		t.Fatalf("store the anchor an earlier release could write: %v", err)
	}
}

// anchorDocument is the document an ask read resolves aa to, read straight from artifacts, so a
// test compares the resolved fields against the row they are resolved from.
func anchorDocument(t *testing.T, database *store.Store, artifactID string) model.AskAnchorArtifact {
	t.Helper()
	var document model.AskAnchorArtifact
	if err := database.Pool.QueryRow(context.Background(), `
		select project_key, slug, name, is_primary from artifacts where id = $1
	`, artifactID).Scan(&document.Project, &document.Slug, &document.Name, &document.Primary); err != nil {
		t.Fatalf("read document %q: %v", artifactID, err)
	}
	return document
}

// secondDocument uploads a second document to the issue, the one a seeded ask names as its ask
// block so that asks_anchor_artifact (0032) never cast its anchor, and answers its id and slug.
func secondDocument(t *testing.T, handler http.Handler, issueKey string) (string, string) {
	t.Helper()
	upload := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]any{
		"content": "Block document.\n", "name": "blocks.md",
	}, "alice")
	if upload.Code != http.StatusCreated {
		t.Fatalf("create the block document: status=%d body=%s", upload.Code, upload.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, upload).Artifact
	return artifact.ID, artifact.Slug
}

// The issue's ask list reads every ask of the issue through askRowFrom, whose anchor join casts
// anchor->>'artifact_id'. Over a stored anchor that is not a uuid an unguarded cast answers 500
// for the whole route; store.CastableUUID's guard resolves that one ask to no anchor document and
// leaves the canonically anchored ask beside it with the document it had.
func TestIssueAskListAnswersOverAnAnchorArtifactIDThatDoesNotCast(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchored asks", "The quick brown fox jumps over the lazy dog")
	blockDocument, _ := secondDocument(t, handler, issue.Key)
	canonical := openAnchoredAsk(t, handler, issue.Key, "spec", "brown", "Why brown?")
	uncastable := openAnchoredAsk(t, handler, issue.Key, "spec", "lazy", "Why lazy?")
	storeUncastableAnchor(t, database, uncastable, blockDocument)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/asks", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("list the issue's asks: status=%d body=%s", response.Code, response.Body.String())
	}
	asks := map[string]model.Ask{}
	for _, ask := range decodeBody[[]model.Ask](t, response) {
		asks[ask.ID] = ask
	}
	if len(asks) != 2 {
		t.Fatalf("the issue's ask list = %s, want both asks", response.Body.String())
	}
	want := anchorDocument(t, database, issue.PrimaryArtifactID)
	if got := asks[canonical].AnchorArtifact; got == nil || *got != want {
		t.Fatalf("canonically anchored ask's anchor document = %#v, want %#v", got, want)
	}
	if got := asks[uncastable].AnchorArtifact; got != nil {
		t.Fatalf("uncastable anchor ask's anchor document = %#v, want none: its anchor names no document", *got)
	}
	if block := asks[uncastable].BlockArtifact; block == nil || block.ID != blockDocument {
		t.Fatalf("uncastable anchor ask's block document = %#v, want %q", block, blockDocument)
	}
}

// Every ask event payload the issue's event log serves is hydrated with its ask's anchor document
// (attachAskDocumentArtifacts), by a read whose own anchor join casts anchor->>'artifact_id': one
// ask whose stored anchor is not a uuid answered 500 for the whole log before the guard.
func TestIssueEventsAnswerOverAnAnchorArtifactIDThatDoesNotCast(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Anchored ask events", "The quick brown fox jumps over the lazy dog")
	blockDocument, _ := secondDocument(t, handler, issue.Key)
	canonical := openAnchoredAsk(t, handler, issue.Key, "spec", "brown", "Why brown?")
	uncastable := openAnchoredAsk(t, handler, issue.Key, "spec", "lazy", "Why lazy?")
	storeUncastableAnchor(t, database, uncastable, blockDocument)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the issue's events: status=%d body=%s", response.Code, response.Body.String())
	}
	type askEvent struct {
		Type    string `json:"type"`
		Payload struct {
			ID             string                   `json:"id"`
			AnchorArtifact *model.AskAnchorArtifact `json:"anchor_artifact"`
		} `json:"payload"`
	}
	opened := map[string]*model.AskAnchorArtifact{}
	for _, event := range decodeBody[[]askEvent](t, response) {
		if event.Type == "ask.opened" {
			opened[event.Payload.ID] = event.Payload.AnchorArtifact
		}
	}
	if len(opened) != 2 {
		t.Fatalf("the issue's events = %s, want an ask.opened event for both asks", response.Body.String())
	}
	want := anchorDocument(t, database, issue.PrimaryArtifactID)
	if got := opened[canonical]; got == nil || *got != want {
		t.Fatalf("canonically anchored ask event's anchor document = %#v, want %#v", got, want)
	}
	if got := opened[uncastable]; got != nil {
		t.Fatalf("uncastable anchor ask event's anchor document = %#v, want none: its anchor names no document", *got)
	}
}

// askHits are a search response's ask hits, by ask id.
func askHits(response model.SearchResponse) map[string]model.SearchResult {
	hits := map[string]model.SearchResult{}
	for _, result := range response.Results {
		if result.Kind == "ask" {
			hits[result.ID] = result
		}
	}
	return hits
}

// Search's keyword ask arm resolves each hit's document through coalesce(artifact_id,
// block_artifact_id, <anchor>). Every stored ask whose anchor does not cast names an ask block
// (asks_anchor_artifact, 0032, casts the anchor wherever block_artifact_id is null), so coalesce
// stops before the anchor expression on every row a schema allows; the arm's guard keeps it on
// store.CastableUUID's one definition. The search answers over that row shape, links the
// canonically anchored hit to its anchor's document and the other to its ask block's document.
func TestSearchKeywordAnswersOverAnAnchorArtifactIDThatDoesNotCast(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Anchored ask search", "The quick brown fox jumps over the lazy dog")
	blockDocument, blockSlug := secondDocument(t, handler, issue.Key)
	canonical := openAnchoredAsk(t, handler, issue.Key, "spec", "brown", "Keep the quadrant appendix?")
	uncastable := openAnchoredAsk(t, handler, issue.Key, "spec", "lazy", "Keep the quadrant diagram?")
	storeUncastableAnchor(t, database, uncastable, blockDocument)

	anchorHref := "/issues/" + issue.Key + "/spec?ask=" + canonical
	hits := askHits(searchResponse(t, handler, "q=quadrant"))
	if len(hits) != 2 {
		t.Fatalf("keyword search for quadrant found ask hits %v, want both asks", hits)
	}
	if href := hits[canonical].Href; href != anchorHref {
		t.Fatalf("canonically anchored ask hit's href = %q, want %q", href, anchorHref)
	}
	if href, want := hits[uncastable].Href, "/issues/"+issue.Key+"/artifacts/"+blockSlug+"?ask="+uncastable; href != want {
		t.Fatalf("uncastable anchor ask hit's href = %q, want its ask block's document %q", href, want)
	}
}

// Search's meaning ask arm (meaning_ask) carries its own copy of that coalesce, over the asks its
// embeddings bring back rather than the ones a term matched: the query below shares no word with
// either ask and reaches both only through the fake embedder.
func TestSearchMeaningAnswersOverAnAnchorArtifactIDThatDoesNotCast(t *testing.T) {
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		"Keep the quadrant appendix? Yes, keep it.": angledVector(0.9, 2),
		"Keep the quadrant diagram? Yes, keep it.":  angledVector(0.8, 3),
		"wibbleflorp": queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "SRCH", "Anchored ask meaning", "The quick brown fox jumps over the lazy dog")
	blockDocument, blockSlug := secondDocument(t, handler, issue.Key)
	canonical := openAnchoredAsk(t, handler, issue.Key, "spec", "brown", "Keep the quadrant appendix?")
	uncastable := openAnchoredAsk(t, handler, issue.Key, "spec", "lazy", "Keep the quadrant diagram?")
	answerSearchAsk(t, handler, canonical, map[string]any{"selected": []string{"Yes"}, "text": "Yes, keep it."})
	answerSearchAsk(t, handler, uncastable, map[string]any{"selected": []string{"Yes"}, "text": "Yes, keep it."})
	storeUncastableAnchor(t, database, uncastable, blockDocument)
	processAllPending(t, database, embedder)

	anchorHref := "/issues/" + issue.Key + "/spec?ask=" + canonical
	response := searchResponse(t, handler, "q=wibbleflorp")
	if response.Degraded != "" {
		t.Fatalf("Degraded = %q, want empty: the embedder is configured and did not fail", response.Degraded)
	}
	hits := askHits(response)
	if len(hits) != 2 {
		t.Fatalf("meaning search found ask hits %v, want both asks", hits)
	}
	if href := hits[canonical].Href; href != anchorHref {
		t.Fatalf("canonically anchored ask hit's href = %q, want %q", href, anchorHref)
	}
	if href, want := hits[uncastable].Href, "/issues/"+issue.Key+"/artifacts/"+blockSlug+"?ask="+uncastable; href != want {
		t.Fatalf("uncastable anchor ask hit's href = %q, want its ask block's document %q", href, want)
	}
}
