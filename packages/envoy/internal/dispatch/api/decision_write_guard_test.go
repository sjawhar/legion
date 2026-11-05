package api

import (
	"net/http"
	"strings"
	"testing"
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
			response = dispatchRequest(t, handler, http.MethodPost, path, body, "alice")
			if response.Code != http.StatusCreated {
				t.Fatalf("person's own creation: %d %s", response.Code, response.Body.String())
			}
		})
	}
}
