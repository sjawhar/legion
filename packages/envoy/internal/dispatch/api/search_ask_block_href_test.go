package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// A search hit for an ask a document's typed block holds links the block only while the block is
// a node of the reference graph, since an agent opens the hit with `dispatch read`, which reads
// the href's `#b-` fragment as the block's node. An ask whose block id is past
// text.MaxBlockIDBytes (stored, never a node) or whose block a later edit removed links the ask
// itself (`?ask=`), as a document ask with no block does. Each href names the reference the
// agent client's reader of a dashboard URL names - text.ParseRef, held to the same shared href
// table as the client's dispatchRefFromUrl - and the graph reads that reference. Every list a
// hit can come from carries it: the keyword list, the meaning list, and the keyword-only
// fallback when the query's embedding cannot be had.
func TestSearchLinksAnAskBlockOnlyWhileTheBlockIsANode(t *testing.T) {
	const server = "https://dispatch.example"
	embedder := &fakeEmbedder{vectors: map[string][]float32{"wibbleflorp": queryVector()}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "TEST", "Ask block links", "Context paragraph.\n")
	long := randomBlockID(t, 3200)
	askBlock := func(id, question string) string {
		return ":::ask{#" + id + " urgency=\"med\" multiple=\"false\"}\n" + question + "\n:::\n"
	}
	asks := []struct {
		word, block, question string
		node                  bool
		id                    string
	}{
		{word: "astrolabe", block: "kept", question: "Ship the astrolabe?", node: true},
		{word: "sextant", block: long, question: "Ship the sextant?"},
		{word: "quadrant", block: "gone", question: "Ship the quadrant?"},
	}
	for _, ask := range asks {
		written := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"actor": sessionActor(),
			"ops":   []map[string]string{{"op": "insert", "after": "end", "markdown": askBlock(ask.block, ask.question)}},
		})
		if written.Code != http.StatusOK {
			t.Fatalf("write the ask block for %q: status=%d body=%.400s", ask.question, written.Code, written.Body.String())
		}
		awaitIndexedAskBlock(t, handler, issue.PrimaryArtifactID, ask.block, ask.question)
	}
	for index := range asks {
		asks[index].id = blockAskID(t, handler, issue.Key, asks[index].block)
	}
	if deleted := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "delete", "block": "gone"}},
	}, "alice"); deleted.Code != http.StatusOK {
		t.Fatalf("delete the ask block gone: status=%d body=%s", deleted.Code, deleted.Body.String())
	}
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "after-delete")
	requireIndexedAt(t, database, issue.PrimaryArtifactID, latestVersion(t, database, issue.PrimaryArtifactID).Number)
	blocks, _ := readIndexedBlocks(t, database, issue.PrimaryArtifactID)
	if _, kept := blocks["kept"]; !kept {
		t.Fatalf("graph nodes = %v, want the block kept among them", blocks)
	}
	if _, gone := blocks["gone"]; gone {
		t.Fatalf("graph nodes = %v, want no node for the deleted block gone", blocks)
	}
	if _, stored := blocks[long]; stored {
		t.Fatalf("the block id of %d bytes is a graph node, want none past the bound", len(long))
	}

	// Each ask is a meaning match for wibbleflorp, at a cosine above the floor, on an axis of
	// its own, under the embedder's identity, which meaning search compares a query with alone;
	// no other row has an embedding.
	for index, ask := range asks {
		tag, err := database.Pool.Exec(context.Background(), `update embeddings set embedding = $2::vector, model = $3 where kind = 'ask' and id = $1`,
			ask.id, embed.Literal(angledVector(0.9-0.05*float64(index), 2+index)), embedder.Identity())
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("embed the ask %q: %v (%d rows)", ask.question, err, tag.RowsAffected())
		}
	}

	requireHref := func(t *testing.T, results []model.SearchResult, askID, wantHref, wantRef string) {
		t.Helper()
		for _, result := range results {
			if result.Kind != "ask" || result.ID != askID {
				continue
			}
			if result.Href != wantHref {
				t.Fatalf("ask %s href = %.120q, want %.120q", askID, result.Href, wantHref)
			}
			fromHref, hrefOK := text.ParseRef(server+result.Href, server)
			fromRef, refOK := text.ParseRef(wantRef, server)
			if !hrefOK || !refOK || fromHref != fromRef {
				t.Fatalf("href %.120q reads as %#v (%v), want %s's %#v (%v)", result.Href, fromHref, hrefOK, wantRef, fromRef, refOK)
			}
			if read := graphRequest(t, handler, url.Values{"to": {wantRef}}); read.Code != http.StatusOK {
				t.Fatalf("read %s: status=%d body=%.400s, want its node", wantRef, read.Code, read.Body.String())
			}
			return
		}
		t.Fatalf("results = %#v, want the ask %s among them", results, askID)
	}
	want := func(index int) (string, string) {
		ask := asks[index]
		if ask.node {
			return "/issues/" + issue.Key + "/spec#b-" + text.EncodeBlockID(ask.block), "dispatch://" + issue.Key + "/spec#b-" + text.EncodeBlockID(ask.block)
		}
		return "/issues/" + issue.Key + "/spec?ask=" + ask.id, "dispatch://" + issue.Key + "/ask/" + ask.id
	}

	t.Run("keyword", func(t *testing.T) {
		for index, ask := range asks {
			response := searchResponse(t, handler, "q="+ask.word)
			if response.Degraded != "" {
				t.Fatalf("search %q degraded: %q", ask.word, response.Degraded)
			}
			href, ref := want(index)
			requireHref(t, response.Results, ask.id, href, ref)
		}
	})
	t.Run("meaning", func(t *testing.T) {
		response := searchResponse(t, handler, "q=wibbleflorp")
		if response.Degraded != "" || len(response.Results) != len(asks) {
			t.Fatalf("meaning search = %#v, want the %d asks and no degradation", response, len(asks))
		}
		for index, ask := range asks {
			href, ref := want(index)
			requireHref(t, response.Results, ask.id, href, ref)
		}
	})
	t.Run("keyword only", func(t *testing.T) {
		embedder.err = errors.New("embedder unavailable")
		for index, ask := range asks {
			response := searchResponse(t, handler, "q="+ask.word)
			if response.Degraded == "" {
				t.Fatalf("search %q with the embedder failing is not degraded", ask.word)
			}
			href, ref := want(index)
			requireHref(t, response.Results, ask.id, href, ref)
		}
	})
}
