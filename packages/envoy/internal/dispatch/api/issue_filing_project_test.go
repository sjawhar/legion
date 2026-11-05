package api

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func TestIssueFiledFromPublicProjectConversation(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, "PUB")
	artifact := createProjectDocument(t, handler, "PUB", "Planning notes", "A public project document")
	for _, kind := range []string{"comment", "ask"} {
		body := map[string]any{"body": "File the reported defect"}
		if kind == "ask" {
			body = map[string]any{"question": "Should the parser defect be filed?", "options": []map[string]string{{"label": "Yes"}, {"label": "No"}}}
		}
		response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/"+kind+"s", body, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("%s: %d %s", kind, response.Code, response.Body.String())
		}
		item := decodeBody[struct {
			ID string `json:"id"`
		}](t, response)
		ref := "dispatch://PUB/artifact/" + artifact.Slug + "/" + kind + "/" + item.ID
		key := createSuggestionIssue(t, handler, map[string]any{"project": "PUB", "title": "Public " + kind + " filing", "filed_from": ref})
		response = dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/links", nil, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("links: %d %s", response.Code, response.Body.String())
		}
		links := decodeBody[model.GraphReferences](t, response)
		if len(links.Edges) != 1 || links.Edges[0].Kind != "filed_from" || links.Edges[0].Node.Ref != ref {
			t.Fatalf("project source: %+v", links)
		}
		for _, wrong := range []string{
			"dispatch://PUB/artifact/missing-document/" + kind + "/" + item.ID,
			testServerURL + "/projects/PUB/documents/missing-document?" + kind + "=" + url.QueryEscape(item.ID),
		} {
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
				"project": "PUB", "title": "Forged " + kind + " source", "filed_from": wrong, "force": true,
			}, "alice")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("forged document %s: %d %s", wrong, response.Code, response.Body.String())
			}
		}
		page := testServerURL + "/projects/PUB/documents/" + artifact.Slug + "?" + kind + "=" + url.QueryEscape(item.ID)
		response = dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
			"project": "PUB", "title": "Dashboard " + kind + " source", "filed_from": page, "force": true,
		}, "alice")
		if response.Code != http.StatusCreated {
			t.Fatalf("dashboard source: %d %s", response.Code, response.Body.String())
		}
	}
}
