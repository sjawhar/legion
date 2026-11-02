package api

import (
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every ask route that serves an anchor answers over an ask whose stored artifact_id names no
// document, and leaves the canonically anchored ask beside it with the document it had: the
// issue's ask list and the single-ask read (askRowFrom, whose anchor join casts
// anchor->>'artifact_id') and the issue's event log, whose ask payloads are hydrated by the same
// join (attachAskDocumentArtifacts). An artifact_id that is not a uuid at all answered 500 for
// the whole route before store.CastableUUID's guard; one stored as a JSON number also has to
// decode, which model.DecodeAnchor reads as an anchor naming no document rather than failing on
// a number where it holds a string. Both asks below carry block_artifact_id too, the shape
// asks_anchor_artifact (0032) never cast, which is how an earlier release could leave such a
// value stored.
func TestAskRoutesAnswerOverAnAnchorArtifactIDThatNamesNoDocument(t *testing.T) {
	for _, stored := range []struct{ name, rawJSON, served string }{
		{name: "not a uuid", rawJSON: `"not-a-uuid"`, served: "not-a-uuid"},
		{name: "a json number", rawJSON: `5`},
	} {
		t.Run(stored.name, func(t *testing.T) {
			handler, database := newTestHandlerWithStore(t)
			issue := createInteractionIssue(t, handler, "TEST", "Anchored asks", "The quick brown fox jumps over the lazy dog")
			blockDocument, _ := secondDocument(t, handler, issue.Key)
			canonical := openAnchoredAsk(t, handler, issue.Key, "spec", "brown", "Why brown?")
			nameless := openAnchoredAsk(t, handler, issue.Key, "spec", "lazy", "Why lazy?")
			storeAnchorArtifactID(t, database, nameless, blockDocument, stored.rawJSON)
			want := anchorDocument(t, database, issue.PrimaryArtifactID)

			asks := issueAsks(t, handler, issue.Key)
			if len(asks) != 2 {
				t.Fatalf("the issue's ask list = %#v, want both asks", asks)
			}
			if got := asks[canonical].AnchorArtifact; got == nil || *got != want {
				t.Errorf("canonically anchored ask's anchor document = %#v, want %#v", got, want)
			}
			if got := asks[nameless].AnchorArtifact; got != nil {
				t.Errorf("issue ask list: the anchor's document = %#v, want none: its artifact_id names no document", *got)
			}
			if block := asks[nameless].BlockArtifact; block == nil || block.ID != blockDocument {
				t.Errorf("issue ask list: the ask's block document = %#v, want %q", block, blockDocument)
			}

			ask := singleAsk(t, handler, nameless)
			if ask.AnchorArtifact != nil {
				t.Errorf("single ask read: the anchor's document = %#v, want none", *ask.AnchorArtifact)
			}
			if ask.Anchor == nil || ask.Anchor.ArtifactID != stored.served {
				t.Errorf("single ask read: the anchor = %#v, want one whose artifact_id is %q", ask.Anchor, stored.served)
			}

			opened := openedAskAnchors(t, handler, issue.Key)
			if len(opened) != 2 {
				t.Fatalf("the issue's ask.opened events = %#v, want one for both asks", opened)
			}
			if got := opened[canonical]; got == nil || *got != want {
				t.Errorf("canonically anchored ask event's anchor document = %#v, want %#v", got, want)
			}
			if got := opened[nameless]; got != nil {
				t.Errorf("issue events: the anchor's ask.opened document = %#v, want none", *got)
			}
		})
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
	storeAnchorArtifactID(t, database, uncastable, blockDocument, `"not-a-uuid"`)

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
	storeAnchorArtifactID(t, database, uncastable, blockDocument, `"not-a-uuid"`)
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

// The references graph and search put one such ask on the same document: an anchor artifact_id
// that casts to no uuid names no document, so loadAskNodes falls through to the ask's block
// document for the node's parent, which is the document the search hit's href already names.
func TestRefsAndSearchPutAnAskWhoseAnchorDoesNotCastOnItsBlockDocument(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "SRCH", "Anchored ask graph", "The quick brown fox jumps over the lazy dog")
	blockDocument, blockSlug := secondDocument(t, handler, issue.Key)
	ask := openAnchoredAsk(t, handler, issue.Key, "spec", "brown", "Keep the quadrant appendix?")
	storeAnchorArtifactID(t, database, ask, blockDocument, `"not-a-uuid"`)

	node := graphEdges(t, handler, url.Values{"to": {"dispatch://" + issue.Key + "/ask/" + ask}}).Node
	wantParent := model.NodeParent{Kind: "artifact", ID: blockDocument, Ref: "dispatch://" + issue.Key + "/artifact/" + blockSlug}
	if node.Parent == nil || *node.Parent != wantParent {
		t.Fatalf("ask node's parent = %#v, want its ask block's document %#v", node.Parent, wantParent)
	}
	if href, want := askHits(searchResponse(t, handler, "q=quadrant"))[ask].Href, "/issues/"+issue.Key+"/artifacts/"+blockSlug+"?ask="+ask; href != want {
		t.Fatalf("ask hit's href = %q, want the same document as its node's parent %q", href, want)
	}
}
