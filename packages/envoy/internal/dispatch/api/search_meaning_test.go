package api

import (
	"context"
	"errors"
	"math"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/embedqueue"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// fakeEmbedder is a deterministic, text-keyed stand-in for Cohere (LEGION-549's tests use a fake
// embedder, never the real API): Embed looks every text up in vectors and answers its mapped
// vector, or defaultVector() (cosine 0 with every query this file embeds, well below
// searchMeaningFloor) for a text it does not recognize, so a test controls exactly which pairs of
// texts cosine-match without depending on any real model. err, when set, is returned instead, for
// the keyword-only fallback tests.
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
			out[i] = defaultVector()
		}
	}
	return out, nil
}

// angledVector returns a unit vector in the plane spanned by dimension 0 and axis, whose cosine
// similarity with queryVector() (dimension 0's own unit vector) is exactly similarity - computed
// from the angle, not approximated from noisy components, so a test's expected fused score (which
// depends on exact rank position, and now also on searchMeaningFloor) is exact rather than a
// vector-math guess. axis must differ across every vector a test embeds in the same query's
// results, or two "unrelated" vectors would accidentally correlate with each other (never with
// the query itself, which only ever compares against dimension 0).
func angledVector(similarity float64, axis int) []float32 {
	v := make([]float32, embed.Dimension)
	v[0] = float32(similarity)
	v[axis] = float32(math.Sqrt(1 - similarity*similarity))
	return v
}

// queryVector is every test query's own embedding: the pure dimension-0 unit vector, so
// angledVector(similarity, axis)'s cosine similarity with it is similarity by construction.
func queryVector() []float32 { return angledVector(1, 1) }

// defaultVector stands in for unrelated content this file never explicitly maps: cosine 0 with
// queryVector(), comfortably below searchMeaningFloor.
func defaultVector() []float32 { return angledVector(0, 1) }

func processAllPending(t *testing.T, database *store.Store, embedder embed.Embedder) {
	t.Helper()
	deps := embedqueue.Deps{Store: database, Embedder: embedder}
	for {
		succeeded, failed, blocked, _, err := embedqueue.ProcessBatch(context.Background(), deps)
		if err != nil {
			t.Fatalf("process pending embeddings: %v", err)
		}
		if succeeded+failed == 0 || blocked {
			return
		}
	}
}

// TestSearchFindsAMeaningOnlyMatch is LEGION-549's acceptance criterion: a query sharing no word
// with the right issue, that means the same thing, finds it - here via a fake embedder's
// deterministic cosine match, since keyword search (websearch_to_tsquery) cannot match a query
// term absent from the corpus at all.
func TestSearchFindsAMeaningOnlyMatch(t *testing.T) {
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		"Celestial navigation device maintenance": angledVector(0.9, 2),
		"wibbleflorp": queryVector(),
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
	// Exactly one result: the issue, at 0.9 cosine similarity. Its own auto-created spec
	// document's body ("Routine upkeep notes.") is never mapped, so it defaults to cosine 0 with
	// the query - below searchMeaningFloor, and does not appear at all.
	if len(response.Results) != 1 || response.Results[0].ID != issue.Key {
		t.Fatalf("Results = %+v, want exactly one result: the issue %q", response.Results, issue.Key)
	}
}

// TestSearchMeaningFloorExcludesWeakMatches is the regression the floor exists for: a result
// Cohere ranks closest to the query, in a kind otherwise empty of better candidates, is not the
// same thing as a relevant result. Below searchMeaningFloor the content never reaches legs at
// all; at or above it, it does.
func TestSearchMeaningFloorExcludesWeakMatches(t *testing.T) {
	const belowFloor = 0.1 // < searchMeaningFloor (0.25)
	const atFloor = 0.25
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		"Unrelated quarterly budget notes": angledVector(belowFloor, 2),
		"Right at the line":                angledVector(atFloor, 3),
		"wibbleflorp":                      queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	weak := createInteractionIssue(t, handler, "WEAK", "Unrelated quarterly budget notes", "Body text.")
	line := createInteractionIssue(t, handler, "LINE", "Right at the line", "Body text.")
	processAllPending(t, database, embedder)

	response := searchResponse(t, handler, "q=wibbleflorp")
	ids := make(map[string]bool, len(response.Results))
	for _, result := range response.Results {
		ids[result.ID] = true
	}
	if ids[weak.Key] {
		t.Errorf("a %.2f-similarity match appeared in results; want it excluded below searchMeaningFloor (0.25)", belowFloor)
	}
	if !ids[line.Key] {
		t.Errorf("a %.2f-similarity match (at the floor) did not appear; want >= searchMeaningFloor included", atFloor)
	}
}

// TestSearchAnswersKeywordOnlyWhenTheEmbedderFails is the acceptance criterion "with the embedder
// down, search answers with keyword results and says it did": a request whose query embedding
// fails still returns the ordinary keyword hit, with Degraded naming why.
func TestSearchAnswersKeywordOnlyWhenTheEmbedderFails(t *testing.T) {
	embedder := &fakeEmbedder{err: errors.New("bedrock: simulated outage")}
	handler, _, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "DEGR", "Astrolabe calibration guide", "Keyword body text.")

	response := searchResponse(t, handler, "q=astrolabe")
	if response.Degraded != contracts.SearchDegradedEmbedderUnavailable {
		t.Fatalf("Degraded = %q, want %q", response.Degraded, contracts.SearchDegradedEmbedderUnavailable)
	}
	if len(response.Results) != 1 || response.Results[0].ID != issue.Key {
		t.Fatalf("Results = %+v, want the one keyword match for %q", response.Results, issue.Key)
	}
	if embedder.calls == 0 {
		t.Error("the embedder was never called; the fallback path should still try the query's embedding")
	}
}

// TestSearchAnswersKeywordOnlyWithNoEmbedderConfigured covers the other half of "a Dispatch
// without Bedrock credentials": Deps.Embedder is nil, exactly the production boot state when
// embed.New's AWS config load fails (cmd/dispatch's bootConfig), and search still answers,
// keyword-only, saying so.
func TestSearchAnswersKeywordOnlyWithNoEmbedderConfigured(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "NOEM", "Astrolabe calibration guide", "Keyword body text.")

	response := searchResponse(t, handler, "q=astrolabe")
	if response.Degraded != contracts.SearchDegradedEmbedderUnavailable {
		t.Fatalf("Degraded = %q, want %q", response.Degraded, contracts.SearchDegradedEmbedderUnavailable)
	}
	if len(response.Results) != 1 || response.Results[0].ID != issue.Key {
		t.Fatalf("Results = %+v, want the one keyword match for %q", response.Results, issue.Key)
	}
}

// TestSearchSkipsTheEmbedderForAStopWordOnlyQuery proves runSearch's numnode check runs before
// the one Bedrock call it ever makes: a query whose websearch_to_tsquery is empty (pure
// stopwords) answers with no results without ever paying for a query embedding, configured
// embedder and all - the performance finding round 3's simplify pass raised against the
// now-fixed ordering (runSearch used to call embedQuery unconditionally, discovering the empty
// query only after a wasted Bedrock round trip).
func TestSearchSkipsTheEmbedderForAStopWordOnlyQuery(t *testing.T) {
	embedder := &fakeEmbedder{}
	handler, _, _ := newTestServer(t, testServerOptions{embedder: embedder})
	createInteractionIssue(t, handler, "SKIP", "Astrolabe calibration guide", "Keyword body text.")

	response := searchResponse(t, handler, "q=the")
	if len(response.Results) != 0 {
		t.Fatalf("Results = %+v, want none for a stopword-only query", response.Results)
	}
	if response.Degraded != "" {
		t.Fatalf("Degraded = %q, want empty - a stopword-only query never reaches the embedder at all", response.Degraded)
	}
	if embedder.calls != 0 {
		t.Errorf("embedder.calls = %d, want 0 - the numnode check must short-circuit before embedQuery", embedder.calls)
	}
}

// TestSearchFusesAnItemFoundByBothListsAboveOneFoundOnlyByMeaning exercises the two-list
// reciprocal rank fusion search.go's "fused" CTE now sums (grouped by (kind, id), the invariant
// Round 3's review of PR #1764 flagged a second per-kind list must preserve): an issue the query
// both keyword-matches and whose embedding cosine-matches at 0.9 sits at position 1 of each list,
// scoring the sum of two 1/(60+1) terms; an issue the query shares no word with (so it never
// enters the keyword list at all) and whose embedding cosine-matches at a weaker 0.4 (above
// searchMeaningFloor, so it still reaches legs) sits at position 2 of the meaning list alone,
// scoring one 1/(60+2) term - under half as much. Fusion summing, not taking the better list's
// score alone, is what makes that gap as large as it is.
func TestSearchFusesAnItemFoundByBothListsAboveOneFoundOnlyByMeaning(t *testing.T) {
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		"Harbor lantern inspection report":     angledVector(0.9, 2), // keyword match and a strong cosine match: both lists
		"Unrelated maintenance budget request": angledVector(0.4, 3), // no shared word with the query, a weaker cosine match: meaning list only
		"lantern inspection":                   queryVector(),
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
