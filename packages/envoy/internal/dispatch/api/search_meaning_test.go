package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/embed/embedtest"
	"github.com/sjawhar/envoy/internal/dispatch/embedqueue"
	"github.com/sjawhar/envoy/internal/dispatch/model"
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
	// every other type - models the queue's own background document traffic (the coverage pass
	// and embedqueue's scan draining a backlog) throttling while unrelated, low-volume query
	// traffic still gets through
	// (TestSearchStaysNonDegradedWhileEmbedqueueIsThrottled). vectors/err/errFor are set up
	// before a test's concurrent goroutines start and never mutated after, so they need no lock;
	// callsMu guards calls, the one field tests mutate concurrently.
	errFor map[embed.InputType]error
	// delay holds every call that long before it answers; a call whose context ends first answers
	// the context's error then. It models a model service slower than its caller allows (the
	// related-item check's meaning allowance).
	delay time.Duration

	callsMu sync.Mutex
	calls   int
}

func (f *fakeEmbedder) Embed(ctx context.Context, texts []string, inputType embed.InputType) ([][]float32, error) {
	f.callsMu.Lock()
	f.calls++
	f.callsMu.Unlock()
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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

// Identity and Limits are Bedrock's: the identity embedqueue records today for the vectors these
// tests embed.
func (f *fakeEmbedder) Identity() string     { return embed.BedrockModel }
func (f *fakeEmbedder) Limits() embed.Limits { return new(embed.BedrockClient).Limits() }

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

// issueEmbeddingText is the text an issue created with a one-paragraph spec embeds (0122's
// issue_embedding_text): its title, a blank line and its spec as the primary document stores it,
// serialized markdown ending in a newline. A test keys an issue's vector by this, not by the title
// alone.
func issueEmbeddingText(title, spec string) string { return title + "\n\n" + spec + "\n" }

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
	const title, spec = "Celestial navigation device maintenance", "Routine upkeep notes."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		"wibbleflorp":                   queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "MEAN", title, spec)
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
// off for the whole test, simulating a backlog saturating the model service's quota for bulk
// document traffic, while concurrent live search requests keep succeeding non-degraded, because
// their InputQuery calls go straight to the unwrapped fake embedder and are never subject to
// that backoff at all.
func TestSearchStaysNonDegradedWhileEmbedqueueIsThrottled(t *testing.T) {
	const title, spec = "Celestial navigation device maintenance", "Routine upkeep notes."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(title, spec): angledVector(0.9, 2),
		"wibbleflorp":                   queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	createInteractionIssue(t, handler, "PRIO", title, spec)
	// Embeds while nothing is throttled yet: this is the content a later query below must still
	// find - already embedded, before the saturating bulk traffic simulated next ever starts.
	processAllPending(t, database, embedder)

	// Simulate the queue's background drain (the coverage pass and embedqueue's scan working
	// through a backlog) now saturating the account: every further InputDocument call
	// throttles, forever; InputQuery (a live query's own embedding) keeps succeeding. A second
	// issue, created only now, gives embedqueue perpetual pending work to retry and back off on
	// for the rest of the test - exactly a saturating background drain in progress.
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
	const weakTitle, lineTitle, spec = "Unrelated quarterly budget notes", "Right at the line", "Body text."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(weakTitle, spec): angledVector(belowFloor, 2),
		issueEmbeddingText(lineTitle, spec): angledVector(atFloor, 3),
		"wibbleflorp":                       queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	weak := createInteractionIssue(t, handler, "WEAK", weakTitle, spec)
	line := createInteractionIssue(t, handler, "LINE", lineTitle, spec)
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
// fails still returns the ordinary keyword hit, with Degraded naming why. The issue create's
// related-item check tries the embedder too, so only calls made after it answered are the
// search's.
func TestSearchAnswersKeywordOnlyWhenTheEmbedderFails(t *testing.T) {
	embedder := &fakeEmbedder{err: errors.New("bedrock: simulated outage")}
	handler, _, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "DEGR", "Astrolabe calibration guide", "Keyword body text.")
	embedder.callsMu.Lock()
	before := embedder.calls
	embedder.callsMu.Unlock()

	response := searchResponse(t, handler, "q=astrolabe")
	if response.Degraded != contracts.SearchDegradedEmbedderUnavailable {
		t.Fatalf("Degraded = %q, want %q", response.Degraded, contracts.SearchDegradedEmbedderUnavailable)
	}
	if len(response.Results) != 1 || response.Results[0].ID != issue.Key {
		t.Fatalf("Results = %+v, want the one keyword match for %q", response.Results, issue.Key)
	}
	embedder.callsMu.Lock()
	defer embedder.callsMu.Unlock()
	if embedder.calls == before {
		t.Error("the search never called the embedder; the fallback path should still try the query's embedding")
	}
}

// TestSearchAnswersKeywordOnlyWithNoEmbedderConfigured covers the other half of "a Dispatch
// without model-service credentials": Deps.Embedder is nil, exactly the production boot state
// when the default Bedrock client's AWS config load fails (cmd/dispatch's newEmbedder), and
// search still answers, keyword-only, saying so.
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
// the one model-service call it ever makes: a query whose websearch_to_tsquery is empty (pure
// stopwords) answers with no results without ever paying for a query embedding, configured
// embedder and all. The issue create's related-item check embeds its own text, so only calls
// made after it answered are the search's.
func TestSearchSkipsTheEmbedderForAStopWordOnlyQuery(t *testing.T) {
	embedder := &fakeEmbedder{}
	handler, _, _ := newTestServer(t, testServerOptions{embedder: embedder})
	createInteractionIssue(t, handler, "SKIP", "Astrolabe calibration guide", "Keyword body text.")
	embedder.callsMu.Lock()
	before := embedder.calls
	embedder.callsMu.Unlock()

	response := searchResponse(t, handler, "q=the")
	if len(response.Results) != 0 {
		t.Fatalf("Results = %+v, want none for a stopword-only query", response.Results)
	}
	if response.Degraded != "" {
		t.Fatalf("Degraded = %q, want empty - a stopword-only query never reaches the embedder at all", response.Degraded)
	}
	embedder.callsMu.Lock()
	defer embedder.callsMu.Unlock()
	if searchCalls := embedder.calls - before; searchCalls != 0 {
		t.Errorf("embedder calls by the search = %d, want 0 - the numnode check must short-circuit before embedQuery", searchCalls)
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
	const bothTitle, bothSpec = "Harbor lantern inspection report", "Body text one."
	const meaningOnlyTitle, meaningOnlySpec = "Unrelated maintenance budget request", "Body text two."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(bothTitle, bothSpec):               angledVector(0.9, 2), // keyword match and a strong cosine match: both lists
		issueEmbeddingText(meaningOnlyTitle, meaningOnlySpec): angledVector(0.4, 3), // no shared word with the query, a weaker cosine match: meaning list only
		"lantern inspection":                                  queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	both := createInteractionIssue(t, handler, "FUSE", bothTitle, bothSpec)
	meaningOnly := createInteractionIssue(t, handler, "FUSM", meaningOnlyTitle, meaningOnlySpec)
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

// TestSearchNeverRanksAVectorOfAnotherIdentity is INFRA-393's "search never compares a query with
// a vector of another identity": an issue whose vector another model or place made scores closer
// to the query than the issue embedded by the query's own embedder, and still never appears as a
// meaning hit, while the right-identity issue does. Recording the same vector under the query
// embedder's identity then ranks it first, so its absence was the identity alone.
func TestSearchNeverRanksAVectorOfAnotherIdentity(t *testing.T) {
	const rightTitle, foreignTitle, spec = "Celestial navigation device maintenance", "Sextant mirror alignment checklist", "Routine upkeep notes."
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		issueEmbeddingText(rightTitle, spec):   angledVector(0.5, 2),
		issueEmbeddingText(foreignTitle, spec): angledVector(0.95, 3),
		"wibbleflorp":                          queryVector(),
	}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	right := createInteractionIssue(t, handler, "RGHT", rightTitle, spec)
	foreign := createInteractionIssue(t, handler, "FRGN", foreignTitle, spec)
	processAllPending(t, database, embedder)

	const otherIdentity = "azure:other.example/embed-v-4-0"
	setModel := func(identity string) {
		t.Helper()
		tag, err := database.Pool.Exec(context.Background(), "update embeddings set model = $1 where kind = 'issue' and id = $2", identity, foreign.Key)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("record issue %s's vector under %q: %v (rows %d)", foreign.Key, identity, err, tag.RowsAffected())
		}
	}
	setModel(otherIdentity)

	response := searchResponse(t, handler, "q=wibbleflorp")
	if response.Degraded != "" {
		t.Fatalf("Degraded = %q, want empty", response.Degraded)
	}
	if len(response.Results) != 1 || response.Results[0].ID != right.Key {
		t.Fatalf("Results = %+v, want exactly the right-identity issue %q, never %q's %q vector", response.Results, right.Key, foreign.Key, otherIdentity)
	}

	setModel(embedder.Identity())
	response = searchResponse(t, handler, "q=wibbleflorp")
	if len(response.Results) != 2 || response.Results[0].ID != foreign.Key || response.Results[1].ID != right.Key {
		t.Fatalf("Results = %+v, want %q (0.95) then %q (0.5) once both vectors carry the query's identity", response.Results, foreign.Key, right.Key)
	}
}

// recordingEmbedder is fakeEmbedder, also keeping every text it was asked to embed.
type recordingEmbedder struct {
	*fakeEmbedder
	mu    sync.Mutex
	texts []string
}

func (r *recordingEmbedder) Embed(ctx context.Context, texts []string, inputType embed.InputType) ([][]float32, error) {
	r.mu.Lock()
	r.texts = append(r.texts, texts...)
	r.mu.Unlock()
	return r.fakeEmbedder.Embed(ctx, texts, inputType)
}

// maxBody is a body of exactly contracts.DispatchBodyMax characters: word repeated, then filler
// up to the cap, ending in a letter so no trimming can shorten it.
func maxBody(word string) string {
	body := strings.Repeat(word+" ", contracts.DispatchBodyMax/(len(word)+1))
	return body + strings.Repeat("x", contracts.DispatchBodyMax-len(body))
}

// TestTheLongestCommentAndMessageReachTheEmbedderWhole: a comment and a message at
// DISPATCH_BODY_MAX, written through the API, reach the embedder whole once the queue drains -
// the body cap and the embedder's input bound (embed's maxInputChars) agree, so no tail of a
// body the server accepted goes unsearched.
func TestTheLongestCommentAndMessageReachTheEmbedderWhole(t *testing.T) {
	embedder := &recordingEmbedder{fakeEmbedder: &fakeEmbedder{}}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	issue := createInteractionIssue(t, handler, "LONG", "Longest bodies", "Body text.")
	commentBody, messageBody := maxBody("astrolabe"), maxBody("sextant")
	if comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": commentBody}, "alice"); comment.Code != http.StatusCreated {
		t.Fatalf("create comment: status=%d body=%s", comment.Code, comment.Body.String())
	}
	if message := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/messages", map[string]any{"body": messageBody}, "alice"); message.Code != http.StatusCreated {
		t.Fatalf("create message: status=%d body=%s", message.Code, message.Body.String())
	}
	processAllPending(t, database, embedder)

	embedded := make(map[string]bool, len(embedder.texts))
	for _, text := range embedder.texts {
		embedded[text] = true
	}
	for name, body := range map[string]string{"comment": commentBody, "message": messageBody} {
		if len(body) != contracts.DispatchBodyMax {
			t.Fatalf("%s body is %d characters, want %d", name, len(body), contracts.DispatchBodyMax)
		}
		if !embedded[body] {
			t.Errorf("the %d-character %s never reached the embedder whole", len(body), name)
		}
	}
	var notCurrent int
	if err := database.Pool.QueryRow(context.Background(),
		"select count(*) from embeddings where kind in ('comment', 'message') and (embedding is null or embedded_hash is distinct from content_hash)").Scan(&notCurrent); err != nil {
		t.Fatalf("count unembedded rows: %v", err)
	}
	if notCurrent != 0 {
		t.Errorf("%d comment or message rows are not embedded at their text after the queue drained", notCurrent)
	}
}

// TestSearchAndWritesRideOutAnAzureOutage drives the real embed.AzureClient against a stand-in
// Azure endpoint. While it answers 500, search answers its keyword hits and says it is
// degraded, a comment is still written, and the queue keeps the comment's row pending rather
// than dead-lettering it (the canary call fails too, so the failure is never the row's own).
// Once the endpoint answers 200 and the queue runs again, the comment is embedded under the
// Azure identity, the coverage report says so, and search ranks it by meaning, not degraded.
func TestSearchAndWritesRideOutAnAzureOutage(t *testing.T) {
	const commentBody = "Recalibrate the theodolite before the survey."
	var healthy atomic.Bool
	azure := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"InternalServerError"}}`))
			return
		}
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		data := make([]item, len(request.Input))
		for i, text := range request.Input {
			vector := defaultVector()
			switch text {
			case commentBody:
				vector = angledVector(0.9, 2)
			case "wibbleflorp":
				vector = queryVector()
			}
			data[i] = item{Index: i, Embedding: vector}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(azure.Close)
	client, err := embed.NewAzure(embed.AzureConfig{
		Endpoint: azure.URL + "/models", Deployment: "embed-v-4-0", TokensPerMinute: 300_000,
		APIKey: "test-key", HTTPClient: azure.Client(),
	})
	if err != nil {
		t.Fatalf("build the Azure client: %v", err)
	}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: client})
	issue := createInteractionIssue(t, handler, "AZUR", "Astrolabe calibration guide", "Keyword body text.")

	degraded := searchResponse(t, handler, "q=astrolabe")
	if degraded.Degraded != contracts.SearchDegradedEmbedderUnavailable {
		t.Fatalf("Degraded = %q while Azure answers 500, want %q", degraded.Degraded, contracts.SearchDegradedEmbedderUnavailable)
	}
	if len(degraded.Results) != 1 || degraded.Results[0].ID != issue.Key {
		t.Fatalf("Results = %+v while Azure answers 500, want the one keyword match %q", degraded.Results, issue.Key)
	}
	comment := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{"body": commentBody}, "alice")
	if comment.Code != http.StatusCreated {
		t.Fatalf("create comment while Azure answers 500: status=%d body=%s", comment.Code, comment.Body.String())
	}
	commentID := decodeBody[model.Comment](t, comment).ID

	ctx := context.Background()
	processAllPending(t, database, client)
	var embeddedDuringOutage, dead bool
	if err := database.Pool.QueryRow(ctx, "select embedding is not null, dead from embeddings where kind = 'comment' and id = $1", commentID).Scan(&embeddedDuringOutage, &dead); err != nil {
		t.Fatalf("read the comment's embeddings row: %v", err)
	}
	if embeddedDuringOutage || dead {
		t.Fatalf("comment row embedded=%v dead=%v while Azure answers 500, want pending: not embedded, not dead", embeddedDuringOutage, dead)
	}

	// The outage put every row into retry backoff; bring their next attempt to now rather than
	// wait it out, as the queue's own scan would once the backoff passed.
	if _, err := database.Pool.Exec(ctx, "update embeddings set next_attempt_at = now() where embedded_hash is distinct from content_hash"); err != nil {
		t.Fatalf("end the retry backoff: %v", err)
	}
	healthy.Store(true)
	processAllPending(t, database, client)

	var identity string
	var current bool
	if err := database.Pool.QueryRow(ctx, "select coalesce(model, ''), embedding is not null and embedded_hash = content_hash from embeddings where kind = 'comment' and id = $1", commentID).Scan(&identity, &current); err != nil {
		t.Fatalf("read the comment's embeddings row: %v", err)
	}
	if !current || identity != client.Identity() {
		t.Fatalf("comment row current=%v model=%q once Azure answers 200, want current under %q", current, identity, client.Identity())
	}
	report := searchCoverageResponse(t, handler)
	requireCoverageIdentity(t, report, client.Identity(), "azure", "embed-v-4-0")
	if report.Kinds.Comment.Current != 1 || report.Kinds.Comment.Lacking != 0 {
		t.Errorf("comment coverage = %+v, want the comment current", report.Kinds.Comment)
	}

	recovered := searchResponse(t, handler, "q=wibbleflorp")
	if recovered.Degraded != "" {
		t.Fatalf("Degraded = %q once Azure answers 200, want empty", recovered.Degraded)
	}
	if len(recovered.Results) != 1 || recovered.Results[0].ID != commentID {
		t.Fatalf("Results = %+v once Azure answers 200, want the comment %q as a meaning hit", recovered.Results, commentID)
	}
}
