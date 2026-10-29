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
		"issue":    report.Kinds.Issue,
		"document": report.Kinds.Document,
		"comment":  report.Kinds.Comment,
		"ask":      report.Kinds.Ask,
		"message":  report.Kinds.Message,
	}
}

// requireOneOfEachKind checks the counts seedSearchCorpus leaves: one item of every kind (the
// issue, its spec, one comment, one answered ask, one message), five in all.
func requireOneOfEachKind(t *testing.T, report model.SearchCoverage) {
	t.Helper()
	for kind, counts := range coverageKinds(report) {
		if counts.Items != 1 {
			t.Fatalf("%s items = %d, want 1: %+v", kind, counts.Items, report)
		}
	}
	if report.Total.Items != 5 {
		t.Fatalf("total items = %d, want 5: %+v", report.Total.Items, report)
	}
}

func requireCoverageIdentity(t *testing.T, report model.SearchCoverage, identity, place, name string) {
	t.Helper()
	if report.Identity == nil || *report.Identity != identity || report.Place == nil || *report.Place != place || report.Model == nil || *report.Model != name {
		t.Fatalf("identity, place, model = %v, %v, %v, want %q, %q, %q", deref(report.Identity), deref(report.Place), deref(report.Model), identity, place, name)
	}
}

// With no embeddings row at all (a corpus loaded with its triggers off), nothing is current and
// every item lacks a vector, aged from when it was written.
func TestSearchCoverageCountsItemsWithNoRowAsLacking(t *testing.T) {
	embedder := &fakeEmbedder{}
	handler, database, _ := newTestServer(t, testServerOptions{embedder: embedder})
	seedSearchCorpus(t, handler)
	if _, err := database.Pool.Exec(context.Background(), "delete from embeddings"); err != nil {
		t.Fatalf("delete embeddings rows: %v", err)
	}

	report := searchCoverageResponse(t, handler)
	requireCoverageIdentity(t, report, embed.BedrockModel, "bedrock", embed.BedrockModel)
	requireOneOfEachKind(t, report)
	for kind, counts := range coverageKinds(report) {
		if counts.Current != 0 || counts.Failed != 0 || counts.Lacking != counts.Items {
			t.Errorf("%s = %+v, want current 0, failed 0, lacking = items", kind, counts)
		}
		if counts.OldestLackingSeconds == nil || *counts.OldestLackingSeconds < 0 {
			t.Errorf("%s oldest_lacking_seconds = %v, want an age from when the item was written", kind, counts.OldestLackingSeconds)
		}
	}
	if report.Total.Current != 0 || report.Total.Lacking != 5 || report.Total.OldestLackingSeconds == nil {
		t.Errorf("total = %+v, want current 0, lacking 5 and an oldest age", report.Total)
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
	requireOneOfEachKind(t, drained)
	for kind, counts := range coverageKinds(drained) {
		if counts.Current != 1 || counts.Lacking != 0 || counts.Failed != 0 || counts.OldestLackingSeconds != nil {
			t.Errorf("%s = %+v, want current 1, lacking 0, failed 0 and no oldest age", kind, counts)
		}
	}
	if drained.Total.Current != 5 || drained.Total.Lacking != 0 || drained.Total.OldestLackingSeconds != nil {
		t.Errorf("total = %+v, want current 5, lacking 0 and no oldest age", drained.Total)
	}

	ctx := context.Background()
	for _, statement := range []struct {
		why, sql, id string
	}{
		{"dead-letter the comment", "update embeddings set dead = true, embedding = null, model = null, embedded_hash = null where kind = 'comment' and id = $1", corpus.commentID},
		{"leave the message's vector at an older text", "update embeddings set embedded_hash = 'an older text' where kind = 'message' and id = $1", corpus.messageID},
		{"give the ask another identity's vector", "update embeddings set model = 'azure:other.example/embed-v-4-0' where kind = 'ask' and id = $1", corpus.askID},
	} {
		if tag, err := database.Pool.Exec(ctx, statement.sql, statement.id); err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("%s: %v (rows %d)", statement.why, err, tag.RowsAffected())
		}
	}

	report := searchCoverageResponse(t, handler)
	if counts := report.Kinds.Comment; counts.Failed != 1 || counts.Current != 0 || counts.Lacking != 0 || counts.OldestLackingSeconds != nil {
		t.Errorf("comment = %+v, want the dead row failed, not lacking", counts)
	}
	for kind, counts := range map[string]model.SearchCoverageCounts{"message": report.Kinds.Message, "ask": report.Kinds.Ask} {
		if counts.Lacking != 1 || counts.Current != 0 || counts.Failed != 0 || counts.OldestLackingSeconds == nil {
			t.Errorf("%s = %+v, want lacking 1 with an oldest age", kind, counts)
		}
	}
	if report.Total.Current != 2 || report.Total.Failed != 1 || report.Total.Lacking != 2 || report.Total.OldestLackingSeconds == nil {
		t.Errorf("total = %+v, want current 2, failed 1, lacking 2 and an oldest age", report.Total)
	}
}

// With no embedder configured the report names no identity, place or model, and nothing is
// current: the rows the write triggers queued are never embedded.
func TestSearchCoverageWithNoEmbedderNamesNoIdentity(t *testing.T) {
	handler := newTestHandler(t)
	seedSearchCorpus(t, handler)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/search/coverage", nil, "alice")
	if !bytes.HasPrefix(response.Body.Bytes(), []byte(`{"identity":null,"place":null,"model":null,`)) {
		t.Fatalf("coverage with no embedder: status=%d body=%s, want identity, place and model null", response.Code, response.Body.String())
	}
	report := searchCoverageResponse(t, handler)
	requireOneOfEachKind(t, report)
	if report.Total.Current != 0 || report.Total.Lacking != 5 || report.Total.Failed != 0 || report.Total.OldestLackingSeconds == nil {
		t.Errorf("total = %+v, want current 0, lacking 5, failed 0 and an oldest age", report.Total)
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
