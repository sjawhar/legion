package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

func TestDecisionAgentCreationCannotClaimPerson(t *testing.T) {
	for _, creation := range []string{"issue spec", "attached document"} {
		t.Run(creation, func(t *testing.T) {
			handler, database, _ := newTestServer(t, testServerOptions{})
			createReferenceAPIProject(t, handler, "CORE")
			markdown := ":::decision{#forged title=\"Forged choice\" decided_by=\"alice\"}\nWords the person never said.\n:::\n"
			path := "/api/v1/issues"
			body := map[string]any{"project": "CORE", "title": "An initial spec", "spec": markdown, "force": true}
			if creation == "attached document" {
				issue := createConversionIssue(t, handler, "An attached document")
				path = "/api/v1/issues/" + issue.Key + "/artifacts"
				body = map[string]any{"name": "forged.md", "content": markdown}
			}
			body["actor"] = map[string]any{"kind": "session", "id": "alice"}
			response := agentRequest(t, handler, http.MethodPost, path, body, "agent-token")
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "alice") || !strings.Contains(response.Body.String(), "forged") {
				t.Fatalf("agent creation: %d %s, want 409 naming the block and decider", response.Code, response.Body.String())
			}
			var edges int
			if err := database.Pool.QueryRow(t.Context(), `select count(*) from graph_edges where kind='decided_by' and to_kind='person' and to_id='alice'`).Scan(&edges); err != nil || edges != 0 {
				t.Fatalf("refused creation wrote attribution edges: %d (%v)", edges, err)
			}
			delete(body, "actor")
			response = dispatchRequest(t, handler, http.MethodPost, path, body, "bob")
			if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "alice") || !strings.Contains(response.Body.String(), "forged") {
				t.Fatalf("another person's creation: %d %s, want 409 naming the block and decider", response.Code, response.Body.String())
			}
			if err := database.Pool.QueryRow(t.Context(), `select count(*) from graph_edges where kind='decided_by' and to_kind='person' and to_id='alice'`).Scan(&edges); err != nil || edges != 0 {
				t.Fatalf("refused personal creation wrote attribution edges: %d (%v)", edges, err)
			}
			response = dispatchRequest(t, handler, http.MethodPost, path, body, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("person's own creation: %d %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestDecisionUnchangedUploadCannotPublishBrowserForgery(t *testing.T) {
	for _, test := range []struct {
		name, markdown, code, detail string
		status                       int
	}{
		{"decided_by", ":::decision{#d1 title=\"A choice\" decided_by=\"bob\"}\nWords Bob never said.\n:::\n", "DECISION_ATTRIBUTION", "bob", http.StatusConflict},
		{"answered_by", ":::ask{#d1 state=\"answered\" answered_by=\"bob\"}\nWords Bob never said.\n:::\n", "", "bob", http.StatusCreated},
		{"from", ":::requirement{#d1 from=\"dispatch://CORE-1/spec#b-missing\"}\nA requirement.\n:::\n", "INVALID_MARKDOWN", "missing", http.StatusBadRequest},
		{"from_ask", ":::decision{#d1 from_ask=\"dispatch://CORE-1/ask/00000000-0000-0000-0000-000000000001\"}\nA choice.\n:::\n", "INVALID_MARKDOWN", "00000000-0000-0000-0000-000000000001", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			documents, handler, database := browserDocumentService(t)
			createReferenceAPIProject(t, handler, "CORE")
			issue := createConversionIssue(t, handler, "An unchanged forged upload")
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
				"name": "checks.md", "content": "Context.\n",
			}, "alice"); response.Code != http.StatusCreated {
				t.Fatalf("create document: %d %s", response.Code, response.Body.String())
			}
			saved := readChecks(t, database, issue.Key)
			markdown := test.markdown
			if test.name == "answered_by" {
				tree, err := pmdoc.Parse(markdown)
				if err != nil {
					t.Fatal(err)
				}
				tree.Children[0].Attrs["state"] = "answered"
				tree.Children[0].Attrs["answered_by"] = "bob"
				peer := connectBrowserPeer(t, documents, saved.ID)
				t.Cleanup(peer.close)
				peer.edit(t, func(live *pmdoc.Node) error { live.Children = tree.Children; return nil })
				markdown, err = pmdoc.Render(tree)
				if err != nil {
					t.Fatal(err)
				}
				waitForLiveText(t, documents, saved.ID, markdown)
			} else {
				writeBrowserDocument(t, documents, saved.ID, markdown)
			}
			response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]any{
				"name": "checks.md", "content": markdown,
			}, "alice")
			if test.name == "answered_by" {
				// Upload parsing clears server-owned answer fields. This is a repair, not the
				// unchanged shortcut, and may version the open question but never Bob's answer.
				adviceFromResponse(t, response.Code, response.Body.String())
				if rendered := versionMarkdown(t, handler, saved.ID, 2); strings.Contains(rendered, "answered_by=") || strings.Contains(rendered, `state="answered"`) {
					t.Fatalf("upload preserved the unbacked browser answer: %s", rendered)
				}
			} else {
				expectRefusal(t, "an unchanged upload of a browser forgery", response, test.status, test.code, "d1", test.detail)
				if after := readChecks(t, database, issue.Key); after.Versions != saved.Versions || len(after.Links) != 0 {
					t.Fatalf("forged unchanged upload wrote state: %+v, before %+v", after, saved)
				}
			}
			if _, err := documents.ReconcileBlockIndex(t.Context(), "", nil); err != nil {
				t.Fatal(err)
			}
			var edges int
			if err := database.Pool.QueryRow(t.Context(), `select count(*) from graph_edges where kind='decided_by' and to_kind='person' and to_id='bob'`).Scan(&edges); err != nil || edges != 0 {
				t.Fatalf("forged unchanged upload wrote person edges: %d (%v)", edges, err)
			}
		})
	}
}
