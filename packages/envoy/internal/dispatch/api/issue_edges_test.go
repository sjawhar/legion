package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestIssueLinksGraphDerivesDuplicateAndPreservesLegacyLinks(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "EDGE")
	parent := createSearchIssue(t, handler, "EDGE", "Parent topic", "")
	target := createSearchIssue(t, handler, "EDGE", "Canonical topic", "")
	source := createSuggestionIssue(t, handler, map[string]any{"project": "EDGE", "title": "Repeated topic", "parent": parent, "blocked_by": []string{target}})
	if _, err := database.Pool.Exec(t.Context(), `
		with attachment as (insert into issue_components (issue_key,mode) values ($1,'explicit') returning issue_key)
		insert into issue_component_members (issue_key,project_key,component_id) select issue_key,'EDGE','parser' from attachment;
	`, source); err != nil {
		t.Fatal(err)
	}
	createIssueMessage(t, handler, source, map[string]any{"body": "Duplicate of dispatch://" + target + "."}, "alice")
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+source, map[string]any{"status": "done"}, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("close: %d %s", response.Code, response.Body.String())
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+source+"/links", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("links: %d %s", response.Code, response.Body.String())
	}
	links := decodeBody[model.GraphReferences](t, response)
	kinds := []string{}
	for _, edge := range links.Edges {
		kinds = append(kinds, edge.Kind)
		if edge.Kind == "duplicate_of" && edge.Node.ID != target {
			t.Fatalf("wrong duplicate: %+v", edge)
		}
	}
	for _, kind := range []string{"child_of", "blocked_by", "duplicate_of"} {
		if !slices.Contains(kinds, kind) {
			t.Fatalf("missing %s in %+v", kind, links)
		}
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+source+"/links/shadow", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("shadow: %d %s", response.Code, response.Body.String())
	}
	var shadow struct {
		Mismatches int `json:"mismatches"`
		Legacy     int `json:"legacy"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &shadow); err != nil {
		t.Fatal(err)
	}
	if shadow.Mismatches != 0 || shadow.Legacy != 3 {
		t.Fatalf("shadow=%s", response.Body.String())
	}
	t.Logf("Links shadow: %s", response.Body.String())
	// Negative control: remove a legacy edge from the view only; source records stay intact.
	if _, err := database.Pool.Exec(context.Background(), `create or replace view graph_edges as select from_kind,from_id,kind,to_kind,to_id,created_at,source_seq from refs`); err != nil {
		t.Fatal(err)
	}
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+source+"/links/shadow", nil, "alice")
	if err := json.Unmarshal(response.Body.Bytes(), &shadow); err != nil {
		t.Fatal(err)
	}
	if shadow.Mismatches != 3 {
		t.Fatalf("negative control=%s", response.Body.String())
	}
	t.Logf("Links shadow negative control: %s", response.Body.String())
}

func TestIssueFiledFromIsExplicitAndRejectsPrivateOrWrongScopeSources(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "FROM")
	createSuggestionProject(t, handler, "OTHER")
	source := createSearchIssue(t, handler, "FROM", "Source discussion", "")
	message := createIssueMessage(t, handler, source, map[string]any{"body": "Please file the parser defect."}, "alice")
	ref := "dispatch://" + source + "/message/" + message.ID
	created := createSuggestionIssue(t, handler, map[string]any{"project": "FROM", "title": "Parser defect", "filed_from": ref})
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+created+"/links", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("links: %d %s", response.Code, response.Body.String())
	}
	links := decodeBody[model.GraphReferences](t, response)
	if len(links.Edges) != 1 || links.Edges[0].Kind != "filed_from" || links.Edges[0].Node.Ref != ref {
		t.Fatalf("links=%+v", links)
	}
	wrongProject := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "OTHER", "title": "Cross project source", "filed_from": ref}, "alice")
	if wrongProject.Code != http.StatusBadRequest {
		t.Fatalf("wrong scope: %d %s", wrongProject.Code, wrongProject.Body.String())
	}
	for _, bad := range []string{"dispatch://agent/private/message/" + message.ID, "dispatch://" + source + "/message/00000000-0000-4000-8000-000000000000", "dispatch://" + source} {
		response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{"project": "FROM", "title": "Another distinct defect", "filed_from": bad}, "alice")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("filed_from %s: %d %s", bad, response.Code, response.Body.String())
		}
	}
}

func TestIssueFiledFromAskAndComment(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "PART")
	source := createSearchIssue(t, handler, "PART", "Source conversation", "")
	comment := createThreadComment(t, handler, source, map[string]any{"body": "A reported defect"}, "alice")
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+source+"/asks", map[string]any{"question": "Should we repair the parser?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}}}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("ask: %d %s", response.Code, response.Body.String())
	}
	ask := decodeBody[model.Ask](t, response)
	for _, item := range []struct{ kind, id, title string }{{"ask", ask.ID, "Answer driven parser repair"}, {"comment", comment.ID, "Comment driven release repair"}} {
		ref := "dispatch://" + source + "/" + item.kind + "/" + item.id
		key := createSuggestionIssue(t, handler, map[string]any{"project": "PART", "title": item.title, "filed_from": ref})
		response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/links", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("links: %d %s", response.Code, response.Body.String())
		}
		links := decodeBody[model.GraphReferences](t, response)
		if len(links.Edges) != 1 || links.Edges[0].Kind != "filed_from" || links.Edges[0].Node.Ref != ref {
			t.Fatalf("filing %s: %+v", ref, links)
		}
	}
}

func TestRelatedGraphFindsSharedSpecReferenceWhenTextMisses(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "INFRA")
	createSuggestionProject(t, handler, "ELSE")
	foundation := createSearchIssue(t, handler, "INFRA", "Foundational substrate", "")
	// Titles and excerpts from the opened INFRA-1044 and INFRA-1098 specs. Both
	// cite INFRA-156, but the later "folded into" note is deliberately absent.
	target := createSearchIssue(t, handler, "INFRA", "One view in Dispatch that is a query over the graph: every node related to me, in place of the Inbox and Replies split", "A person's threads are spread over the Inbox, the Replies list and each issue page. In their place, one view that is a query over the graph: every node related to the person, whatever kind it is. Builds on dispatch://"+foundation)
	const title = "Dispatch models everything as nodes with typed relations: the Decisions tab and the unified inbox are its first queries"
	text := "Single source of truth: a decision is a node in the spec itself, of the decision type in Dispatch's block graph (dispatch://" + foundation + "). Nothing copies it anywhere."
	source := createSearchIssue(t, handler, "INFRA", title, text)
	// On the real project these are common infrastructure words, not rare-word
	// evidence of a duplicate. The fixture supplies that corpus context explicitly.
	for n := range 30 {
		createSearchIssue(t, handler, "INFRA", fmt.Sprintf("Dispatch graph queries nodes related relations inbox decision spec block notes %d", n), "INFRA infrastructure maintenance")
	}
	elsewhere := createSearchIssue(t, handler, "ELSE", "Other project candidate", "Inspired by dispatch://"+foundation)
	refreshTermStats(t, database, "INFRA")
	params := url.Values{"title": {title}, "text": {text}, "exclude_issue": {source}, "keywords_only": {"true"}}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/INFRA/related?"+params.Encode(), nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("related: %d %s", response.Code, response.Body.String())
	}
	var result struct {
		Related []model.WriteSuggestion `json:"related"`
		Graph   []struct {
			Key    string   `json:"key"`
			Shared []string `json:"shared"`
		} `json:"graph_related"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, item := range result.Related {
		if item.Owner.Key == target {
			t.Fatalf("text unexpectedly found target: %s", response.Body.String())
		}
	}
	if len(result.Graph) == 0 || result.Graph[0].Key != target {
		t.Fatalf("graph missed %s: %s", target, response.Body.String())
	}
	for _, item := range result.Graph {
		if item.Key == elsewhere || item.Key == source {
			t.Fatalf("graph scope or self leak: %s", response.Body.String())
		}
	}
	t.Logf("Text-only miss, graph hit: %s", response.Body.String())
	// Removing the shared citation removes the signal, not just its explanation.
	params.Set("text", "No cited foundation")
	params.Del("exclude_issue")
	result.Graph = nil
	response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/projects/INFRA/related?"+params.Encode(), nil, "alice")
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Graph) != 0 {
		t.Fatalf("negative control=%s", response.Body.String())
	}
	t.Logf("Graph negative control: %s", response.Body.String())
}

func TestIssueDuplicateDoesNotReturnAfterReopenAndOrdinaryClose(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "REOPEN")
	target := createSearchIssue(t, handler, "REOPEN", "Original defect", "")
	source := createSearchIssue(t, handler, "REOPEN", "Repeated defect", "")
	createIssueMessage(t, handler, source, map[string]any{"body": "Duplicate of dispatch://" + target}, "alice")
	for _, status := range []string{"done", "in_progress", "done"} {
		response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+source, map[string]any{"status": status}, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", status, response.Code, response.Body.String())
		}
	}
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+source+"/links", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("links: %d %s", response.Code, response.Body.String())
	}
	for _, edge := range decodeBody[model.GraphReferences](t, response).Edges {
		if edge.Kind == "duplicate_of" {
			t.Fatalf("old duplicate returned: %+v", edge)
		}
	}
}
