package api

import (
	"context"
	"errors"
	"math"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/embed/embedtest"
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
	// errFor, when set, returns err only for a call of the matching InputType and succeeds for
	// every other type - models a backfill's own bulk document traffic throttling while
	// unrelated, low-volume query traffic still gets through
	// (TestSearchStaysNonDegradedWhileEmbedqueueIsThrottled). vectors/err/errFor are set up
	// before a test's concurrent goroutines start and never mutated after, so they need no lock;
	// callsMu guards calls, the one field tests mutate concurrently.
	errFor map[embed.InputType]error

	callsMu sync.Mutex
	calls   int
}

func (f *fakeEmbedder) Embed(_ context.Context, texts []string, inputType embed.InputType) ([][]float32, error) {
	f.callsMu.Lock()
	f.calls++
	f.callsMu.Unlock()
	err := f.err
	if e, ok := f.errFor[inputType]; ok {
		err = e
	}
	if err != nil {
		return nil, err
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

// TestSearchStaysNonDegradedWhileEmbedqueueIsThrottled proves a live
// query embedding priority over background work: cmd/dispatch wires embedqueue's own Embedder
// behind embed.RateLimitedEmbedder and never wraps the one api.Deps holds, so a live search
// request's own query embedding is never paced or blocked by that limiter, however deep into
// backoff it currently is. This drives that exact scenario with one shared fake embedder whose
// errFor discriminates by InputType, matching how the two callers actually differ in production
// (embedqueue always calls with InputDocument, search's embedQuery always calls with
// InputQuery): a background goroutine keeps embedqueue's own ProcessBatch throttled and backing
// off for the whole test, simulating a backfill saturating the account's Bedrock quota for bulk
// document traffic, while concurrent live search requests keep succeeding non-degraded, because
// their InputQuery calls go straight to the unwrapped fake embedder and are never subject to
// that backoff at all.
func TestSearchStaysNonDegradedWhileEmbedqueueIsThrottled(t *testing.T) {
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		"Celestial navigation device maintenance": angledVector(0.9, 2),
		"wibbleflorp": queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	createInteractionIssue(t, handler, "PRIO", "Celestial navigation device maintenance", "Routine upkeep notes.")
	// Embeds while nothing is throttled yet: this is the content a later query below must still
	// find - already embedded, before the saturating bulk traffic simulated next ever starts.
	processAllPending(t, database, embedder)

	// Simulate an unrelated bulk backfill now saturating the account: every further
	// InputDocument call throttles, forever; InputQuery (a live query's own embedding) keeps
	// succeeding. A second issue, created only now, gives embedqueue perpetual pending work to
	// retry and back off on for the rest of the test - exactly a saturating backfill in progress.
	throttleErr := embedtest.FakeThrottleError{Code: "ThrottlingException"}
	embedder.errFor = map[embed.InputType]error{embed.InputDocument: throttleErr}
	createInteractionIssue(t, handler, "SAT", "Other bulk content that never finishes embedding", "Body.")

	bgCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	backgroundDeps := embedqueue.Deps{Store: database, Embedder: embed.NewRateLimitedEmbedder(embedder)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for bgCtx.Err() == nil {
			if _, _, _, _, err := embedqueue.ProcessBatch(bgCtx, backgroundDeps); err != nil {
				return
			}
		}
	}()
	defer func() { <-done }()

	// Give the background goroutine a moment to actually hit a throttle and start backing off
	// (its very first call is unpaced, at RateLimitedEmbedder's own floor interval).
	time.Sleep(50 * time.Millisecond)

	for i := range 5 {
		started := time.Now()
		response := searchResponse(t, handler, "q=wibbleflorp")
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Errorf("search request %d took %v while embedqueue was throttled/backing off, want well under 1s - it must never wait behind background work", i, elapsed)
		}
		if response.Degraded != "" {
			t.Errorf("search request %d: Degraded = %q, want empty - a live query must stay non-degraded while only background document traffic is throttled", i, response.Degraded)
		}
		if len(response.Results) != 1 {
			t.Errorf("search request %d: Results = %+v, want the one meaning match (proves the query embedding actually ran, not just that nothing errored)", i, response.Results)
		}
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
// embedder and all.
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
