package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/embed"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// searchCoverageResponse reads GET /api/v1/search/coverage as alice and holds its body to
// model.SearchCoverage's own encoding, byte for byte: every field present, null where the model
// says null.
func searchCoverageResponse(t *testing.T, handler http.Handler) model.SearchCoverage {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/search/coverage", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("search coverage: status=%d body=%s", response.Code, response.Body.String())
	}
	report := decodeBody[model.SearchCoverage](t, response)
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode search coverage: %v", err)
	}
	if body := bytes.TrimSuffix(response.Body.Bytes(), []byte("\n")); !bytes.Equal(body, encoded) {
		t.Fatalf("search coverage body is not model.SearchCoverage's encoding:\nbody  %s\nmodel %s", body, encoded)
	}
	return report
}

// coverageKinds names each kind's counts in a report, for a check that runs over every kind.
func coverageKinds(report model.SearchCoverage) map[string]model.SearchCoverageCounts {
	return map[string]model.SearchCoverageCounts{
		"issue":          report.Kinds.Issue,
		"document_block": report.Kinds.DocumentBlock,
		"comment_block":  report.Kinds.CommentBlock,
		"message_block":  report.Kinds.MessageBlock,
		"ask_part":       report.Kinds.AskPart,
	}
}

// requireEveryKindCovered checks that every kind holds an item of what seedSearchCorpus wrote -
// the issue, and the windows of its spec's blocks, of the comment's, of the message's and of the
// ask's parts - and that the total is the kinds' sum. A kind's items are counted rather than
// named: the spec's long paragraph is cut into pieces, so the number of windows is the
// windowing's to decide, not this test's. It returns that total.
func requireEveryKindCovered(t *testing.T, report model.SearchCoverage) int64 {
	t.Helper()
	var total int64
	for kind, counts := range coverageKinds(report) {
		if counts.Items < 1 {
			t.Fatalf("%s items = %d, want at least one: %+v", kind, counts.Items, report)
		}
		total += counts.Items
	}
	if report.Total.Items != total {
		t.Fatalf("total items = %d, want the kinds' %d: %+v", report.Total.Items, total, report)
	}
	return total
}

func requireCoverageIdentity(t *testing.T, report model.SearchCoverage, identity, place, name string) {
	t.Helper()
	if report.Identity == nil || *report.Identity != identity || report.Place == nil || *report.Place != place || report.Model == nil || *report.Model != name {
		t.Fatalf("identity, place, model = %v, %v, %v, want %q, %q, %q", deref(report.Identity), deref(report.Place), deref(report.Model), identity, place, name)
	}
}

// An item with no embeddings row at all - an issue restored with its trigger off - is counted
// lacking and aged from when it was written, and so is every window waiting for its first
// vector.
func TestSearchCoverageCountsItemsWithNoRowAsLacking(t *testing.T) {
	embedder := &fakeEmbedder{}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	seedSearchCorpus(t, handler)
	if _, err := database.Pool.Exec(context.Background(), "delete from embeddings where kind = 'issue'"); err != nil {
		t.Fatalf("delete the issue's row: %v", err)
	}

	report := searchCoverageResponse(t, handler)
	requireCoverageIdentity(t, report, embed.BedrockModel, "bedrock", embed.BedrockModel)
	items := requireEveryKindCovered(t, report)
	for kind, counts := range coverageKinds(report) {
		if counts.Current != 0 || counts.Failed != 0 || counts.Lacking != counts.Items {
			t.Errorf("%s = %+v, want current 0, failed 0, lacking = items", kind, counts)
		}
		if counts.OldestLackingSeconds == nil || *counts.OldestLackingSeconds < 0 {
			t.Errorf("%s oldest_lacking_seconds = %v, want an age from when the item was written", kind, counts.OldestLackingSeconds)
		}
	}
	if report.Total.Current != 0 || report.Total.Lacking != items || report.Total.OldestLackingSeconds == nil {
		t.Errorf("total = %+v, want current 0, lacking %d and an oldest age", report.Total, items)
	}
}

// Once the queue drains, every item is current and nothing lacks, so no kind has an oldest
// lacking age. A dead row is then counted failed, not lacking; a row embedded at an older text, or
// by another identity, is lacking.
func TestSearchCoverageCountsCurrentFailedAndLackingRows(t *testing.T) {
	embedder := &fakeEmbedder{}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	corpus := seedSearchCorpus(t, handler)
	processAllPending(t, database, embedder)

	drained := searchCoverageResponse(t, handler)
	requireCoverageIdentity(t, drained, embed.BedrockModel, "bedrock", embed.BedrockModel)
	items := requireEveryKindCovered(t, drained)
	for kind, counts := range coverageKinds(drained) {
		if counts.Current != counts.Items || counts.Lacking != 0 || counts.Failed != 0 || counts.OldestLackingSeconds != nil {
			t.Errorf("%s = %+v, want every item current, nothing lacking or failed and no oldest age", kind, counts)
		}
	}
	if drained.Total.Current != items || drained.Total.Lacking != 0 || drained.Total.OldestLackingSeconds != nil {
		t.Errorf("total = %+v, want current %d, lacking 0 and no oldest age", drained.Total, items)
	}

	ctx := context.Background()
	// Each window of the item, which a short comment, message or ask has exactly one of.
	for _, statement := range []struct {
		why, sql, owner string
	}{
		{"dead-letter the comment's window", "update embeddings set dead = true, embedding = null, model = null, embedded_hash = null where kind = 'comment_block' and split_part(id, '#', 1) = $1", "comment/" + corpus.commentID},
		{"leave the message's window at an older text", "update embeddings set embedded_hash = 'an older text' where kind = 'message_block' and split_part(id, '#', 1) = $1", "message/" + corpus.messageID},
		{"give the ask's parts another identity's vector", "update embeddings set model = 'azure:other.example/embed-v-4-0' where kind = 'ask_part' and split_part(id, '#', 1) = $1", "ask/" + corpus.askID},
	} {
		if tag, err := database.Pool.Exec(ctx, statement.sql, statement.owner); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("%s: %v (rows %d)", statement.why, err, tag.RowsAffected())
		}
	}

	report := searchCoverageResponse(t, handler)
	if counts := report.Kinds.CommentBlock; counts.Failed != 1 || counts.Current != 0 || counts.Lacking != 0 || counts.OldestLackingSeconds != nil {
		t.Errorf("comment_block = %+v, want the dead row failed, not lacking", counts)
	}
	for kind, counts := range map[string]model.SearchCoverageCounts{"message_block": report.Kinds.MessageBlock, "ask_part": report.Kinds.AskPart} {
		if counts.Lacking != 1 || counts.Current != 0 || counts.Failed != 0 || counts.OldestLackingSeconds == nil {
			t.Errorf("%s = %+v, want lacking 1 with an oldest age", kind, counts)
		}
	}
	if report.Total.Current != items-3 || report.Total.Failed != 1 || report.Total.Lacking != 2 || report.Total.OldestLackingSeconds == nil {
		t.Errorf("total = %+v, want current %d, failed 1, lacking 2 and an oldest age", report.Total, items-3)
	}
}

// With no embedder configured the report names no identity, place or model, and nothing is
// current: the rows the issue's trigger and the corpus's indexers wrote are never embedded.
func TestSearchCoverageWithNoEmbedderNamesNoIdentity(t *testing.T) {
	handler := newTestHandler(t)
	seedSearchCorpus(t, handler)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/search/coverage", nil, "alice")
	if !bytes.HasPrefix(response.Body.Bytes(), []byte(`{"identity":null,"place":null,"model":null,`)) {
		t.Fatalf("coverage with no embedder: status=%d body=%s, want identity, place and model null", response.Code, response.Body.String())
	}
	report := searchCoverageResponse(t, handler)
	items := requireEveryKindCovered(t, report)
	if report.Total.Current != 0 || report.Total.Lacking != items || report.Total.Failed != 0 || report.Total.OldestLackingSeconds == nil {
		t.Errorf("total = %+v, want current 0, lacking %d, failed 0 and an oldest age", report.Total, items)
	}
}

// The coverage report admits whoever search admits: an agent's bearer token and a browser
// session cookie, and refuses a request with no credential exactly as GET /api/v1/search does.
func TestSearchCoverageAuthentication(t *testing.T) {
	handler := newTestHandler(t)

	if agent := agentRequest(t, handler, http.MethodGet, "/api/v1/search/coverage", nil, "agent-token"); agent.Code != http.StatusOK {
		t.Fatalf("agent coverage: status=%d body=%s", agent.Code, agent.Body.String())
	}

	_, _, _, cookieRequest := newCookieServer(t)
	if cookie := cookieRequest(http.MethodGet, "/api/v1/search/coverage", nil); cookie.Code != http.StatusOK {
		t.Fatalf("cookie coverage: status=%d body=%s", cookie.Code, cookie.Body.String())
	}

	anonymous := dispatchRequest(t, handler, http.MethodGet, "/api/v1/search/coverage", nil, "")
	search := dispatchRequest(t, handler, http.MethodGet, "/api/v1/search?q=astrolabe", nil, "")
	if anonymous.Code != http.StatusUnauthorized || anonymous.Code != search.Code || anonymous.Body.String() != search.Body.String() {
		t.Fatalf("anonymous coverage: status=%d body=%s, want what anonymous search answers: status=%d body=%s",
			anonymous.Code, anonymous.Body.String(), search.Code, search.Body.String())
	}

	wrongBearerRequest := httptest.NewRequest(http.MethodGet, "/api/v1/search/coverage", nil)
	wrongBearerRequest.Header.Set("Authorization", "Bearer wrong-token")
	wrongBearer := httptest.NewRecorder()
	handler.ServeHTTP(wrongBearer, wrongBearerRequest)
	if wrongBearer.Code != http.StatusUnauthorized {
		t.Fatalf("wrong bearer coverage: status=%d body=%s", wrongBearer.Code, wrongBearer.Body.String())
	}
}
