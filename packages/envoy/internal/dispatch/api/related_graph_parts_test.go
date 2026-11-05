package api

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func TestRelatedGraphIncludesReferencesInAskAnswers(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "PARTS")
	foundation := createSearchIssue(t, handler, "PARTS", "Reference foundation", "")
	candidate := createSearchIssue(t, handler, "PARTS", "Earlier implementation discussion", "")
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+candidate+"/asks", map[string]any{"question": "Which approach should we choose?", "options": []map[string]string{{"label": "First"}, {"label": "Second"}}}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("ask: %d %s", response.Code, response.Body.String())
	}
	ask := decodeBody[model.Ask](t, response)
	answerPartAsk(t, handler, ask, "alice", []string{"First"}, "Use dispatch://"+foundation+" as the basis.")
	params := url.Values{"title": {"A newly reported defect"}, "text": {"Relates to dispatch://" + foundation}, "keywords_only": {"true"}}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/PARTS/related?"+params.Encode(), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("related: %d %s", response.Code, response.Body.String())
	}
	result := decodeBody[model.Suggestions](t, response)
	if len(result.GraphRelated) != 1 || result.GraphRelated[0].Key != candidate {
		t.Fatalf("answer mention missing: %s", response.Body.String())
	}
	if len(result.GraphRelated[0].Shared) != 1 {
		t.Fatalf("same target counted repeatedly: %+v", result.GraphRelated)
	}
}

func TestRelatedGraphDoesNotSpendTheMeaningStagesAllowance(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "BUDGET")
	source := createSearchIssue(t, handler, "BUDGET", "Source zymurgy ledger", "")
	createSearchIssue(t, handler, "BUDGET", "Earlier zymurgy ledger", "")
	refreshTermStats(t, database, "BUDGET")
	deps.Embedder = &fakeEmbedder{delay: 2300 * time.Millisecond}
	srv := directServer(deps)
	lock, err := database.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Rollback(context.Background()) }()
	// This table belongs to the graph only. The graph times out after its second while
	// the embedding and text search retain their complete three-second allowance.
	if _, err := lock.Exec(t.Context(), `lock table issue_component_members in access exclusive mode`); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	answer := srv.relatedCheck(store.WithTransactionTracking(t.Context()), relatedInput{
		project: "BUDGET", text: "zymurgy ledger", source: suggestionSource{kind: "issue", issueKey: source},
	})
	if answer.Search != "meaning_and_keywords" || answer.GraphMissing == "" {
		t.Fatalf("meaning lost its allowance or graph was not joined: %+v", answer)
	}
	if took := time.Since(started); took >= 3*time.Second {
		t.Fatalf("parallel check took %s, want less than three seconds", took)
	}
}

func TestRelatedGraphDoesNotRunForAsksOrDecisionBlocks(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "SOURCES")
	source := createSearchIssue(t, handler, "SOURCES", "Source conversation", "")
	if _, err := database.Pool.Exec(t.Context(), `drop view graph_issue_edges`); err != nil {
		t.Fatal(err)
	}
	srv := directServer(deps)
	for _, kind := range []string{"ask", "decision_block"} {
		answer := srv.relatedCheck(t.Context(), relatedInput{
			project: "SOURCES", text: "Source conversation", source: suggestionSource{kind: kind, issueKey: source},
		})
		if answer.GraphMissing != "" || len(answer.GraphRelated) != 0 {
			t.Fatalf("%s ran an issue graph query: %+v", kind, answer)
		}
	}
}
