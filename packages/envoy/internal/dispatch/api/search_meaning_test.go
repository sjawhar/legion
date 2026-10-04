package api

import (
	"context"
	"errors"
	"math"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/embedqueue"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fakeEmbedder is a deterministic, text-keyed stand-in for Cohere (LEGION-549's tests use a fake
// embedder, never the real API): Embed looks every text up in vectors and answers its mapped
// vector, or background() for a text it does not recognize, so a test controls exactly which
// pairs of texts cosine-match without depending on any real model. err, when set, is returned
// instead, for the keyword-only fallback tests.
type fakeEmbedder struct {
	vectors map[string][]float32
	err     error
	calls   int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string, _ embed.InputType) ([][]float32, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if v, ok := f.vectors[text]; ok {
			out[i] = v
		} else {
			out[i] = backgroundVector()
		}
	}
	return out, nil
}

// backgroundVector is a fixed non-zero vector with no particular direction (pgvector's cosine
// distance is undefined for an all-zero vector), standing in for "unrelated content" in a test:
// every topicVector's cosine similarity to it is small but defined.
func backgroundVector() []float32 {
	v := make([]float32, embed.Dimension)
	for i := range v {
		v[i] = 0.001
	}
	return v
}

// topicVector is backgroundVector with one dimension boosted, so two texts mapped to the same
// topic cosine-match near 1.0, and texts of different topics cosine-match near 0.
func topicVector(dimension int) []float32 {
	v := backgroundVector()
	v[dimension] = 5.0
	return v
}

func processAllPending(t *testing.T, database *store.Store, embedder embed.Embedder) {
	t.Helper()
	deps := embedqueue.Deps{Store: database, Embedder: embedder}
	for {
		count, blocked, err := embedqueue.ProcessBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("process pending embeddings: %v", err)
		}
		if count == 0 || blocked {
			return
		}
	}
}

// TestSearchFindsAMeaningOnlyMatch is LEGION-549's acceptance criterion: a query sharing no word
// with the right issue, that means the same thing, finds it - here via a fake embedder's
// deterministic topic match, since keyword search (websearch_to_tsquery) cannot match a query
// term absent from the corpus at all.
func TestSearchFindsAMeaningOnlyMatch(t *testing.T) {
	const topic = 7
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		"Celestial navigation device maintenance": topicVector(topic),
		"wibbleflorp": topicVector(topic),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "MEAN", "Celestial navigation device maintenance", "Routine upkeep notes.")
	processAllPending(t, database, embedder)

	// Sanity: keyword search for this query matches nothing at all (the term appears nowhere),
	// so any hit below can only have come from the meaning leg.
	keywordOnly := searchResponse(t, newTestHandler(t), "q=wibbleflorp")
	if len(keywordOnly.Results) != 0 {
		t.Fatalf("keyword-only baseline found %d results for a term absent from the corpus, want 0", len(keywordOnly.Results))
	}

	response := searchResponse(t, handler, "q=wibbleflorp")
	if response.Degraded != "" {
		t.Fatalf("Degraded = %q, want empty: the embedder is configured and did not fail", response.Degraded)
	}
	// The acceptance bar (LEGION-549) is "finds it in the top five", not "finds only it": meaning
	// search ranks every embedded row of a kind that has one (no similarity floor, matching
	// keyword search's own "every matching row, ranked" shape), so the issue's own auto-created
	// spec document - the only other row this tiny corpus holds - legitimately also appears, far
	// behind the issue itself on the same topic.
	if len(response.Results) == 0 || response.Results[0].ID != issue.Key {
		t.Fatalf("Results[0] = %+v, want the issue %q ranked first", response.Results, issue.Key)
	}
}

// TestSearchAnswersKeywordOnlyWhenTheEmbedderFails is the acceptance criterion "with the embedder
// down, search answers with keyword results and says it did": a request whose query embedding
// fails still returns the ordinary keyword hit, with Degraded naming why.
func TestSearchAnswersKeywordOnlyWhenTheEmbedderFails(t *testing.T) {
	embedder := &fakeEmbedder{err: errors.New("cohere: simulated outage")}
	handler, _, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "DEGR", "Astrolabe calibration guide", "Keyword body text.")

	response := searchResponse(t, handler, "q=astrolabe")
	if response.Degraded != degradedEmbedderUnavailable {
		t.Fatalf("Degraded = %q, want %q", response.Degraded, degradedEmbedderUnavailable)
	}
	if len(response.Results) != 1 || response.Results[0].ID != issue.Key {
		t.Fatalf("Results = %+v, want the one keyword match for %q", response.Results, issue.Key)
	}
	if embedder.calls == 0 {
		t.Error("the embedder was never called; the fallback path should still try the query's embedding")
	}
}

// TestSearchAnswersKeywordOnlyWithNoEmbedderConfigured covers the other half of "a Dispatch
// without a Cohere key": Deps.Embedder is nil, exactly the production boot state when
// COHERE_API_KEY is unset (cmd/dispatch's bootConfig), and search still answers, keyword-only,
// saying so.
func TestSearchAnswersKeywordOnlyWithNoEmbedderConfigured(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "NOEM", "Astrolabe calibration guide", "Keyword body text.")

	response := searchResponse(t, handler, "q=astrolabe")
	if response.Degraded != degradedEmbedderUnavailable {
		t.Fatalf("Degraded = %q, want %q", response.Degraded, degradedEmbedderUnavailable)
	}
	if len(response.Results) != 1 || response.Results[0].ID != issue.Key {
		t.Fatalf("Results = %+v, want the one keyword match for %q", response.Results, issue.Key)
	}
}

// TestSearchFusesAnItemFoundByBothListsAboveOneFoundOnlyByMeaning exercises the two-list
// reciprocal rank fusion search.go's "fused" CTE now sums (grouped by (kind, id), the invariant
// Round 3's review of PR #1764 flagged a second per-kind list must preserve): an issue the query
// both keyword-matches and whose embedding is the query's own topic sits at position 1 of each
// list, scoring the sum of two 1/(60+1) terms; an issue the query shares no word with (so it never
// enters the keyword list at all) and whose embedding is an unrelated topic sits at position 2 of
// the meaning list alone, scoring one 1/(60+2) term - under half as much. Fusion summing, not
// taking the better list's score alone, is what makes that gap as large as it is.
func TestSearchFusesAnItemFoundByBothListsAboveOneFoundOnlyByMeaning(t *testing.T) {
	const queryTopic, otherTopic = 11, 99
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		"Harbor lantern inspection report":     topicVector(queryTopic), // keyword match and the query's own topic: both lists
		"Unrelated maintenance budget request": topicVector(otherTopic), // no shared word with the query, and a different topic: meaning list only, weakly
		"lantern inspection":                   topicVector(queryTopic),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	both := createInteractionIssue(t, handler, "FUSE", "Harbor lantern inspection report", "Body text one.")
	meaningOnly := createInteractionIssue(t, handler, "FUSM", "Unrelated maintenance budget request", "Body text two.")
	processAllPending(t, database, embedder)

	response := searchResponse(t, handler, "q="+url.QueryEscape("lantern inspection"))
	if response.Degraded != "" {
		t.Fatalf("Degraded = %q, want empty", response.Degraded)
	}
	var scoreBoth, scoreMeaningOnly float64
	var sawBoth, sawMeaningOnly bool
	for _, result := range response.Results {
		switch result.ID {
		case both.Key:
			scoreBoth, sawBoth = result.Rank, true
		case meaningOnly.Key:
			scoreMeaningOnly, sawMeaningOnly = result.Rank, true
		}
	}
	if !sawBoth || !sawMeaningOnly {
		t.Fatalf("expected both issues in the results; got %+v", response.Results)
	}
	const k = 60.0
	wantBoth := 1.0/(k+1) + 1.0/(k+1)
	wantMeaningOnly := 1.0 / (k + 2)
	if math.Abs(scoreBoth-wantBoth) > 1e-9 {
		t.Errorf("score for the both-lists issue = %v, want %v (position 1 of each list, summed)", scoreBoth, wantBoth)
	}
	if math.Abs(scoreMeaningOnly-wantMeaningOnly) > 1e-9 {
		t.Errorf("score for the meaning-only issue = %v, want %v (position 2 of the meaning list alone)", scoreMeaningOnly, wantMeaningOnly)
	}
	if scoreBoth <= scoreMeaningOnly {
		t.Errorf("fused score for the both-lists issue = %v, want it greater than the meaning-only issue's %v", scoreBoth, scoreMeaningOnly)
	}
}
