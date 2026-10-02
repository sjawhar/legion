package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A rebuild is only for a history ygo cannot load. A healthy, idle document can carry updates
// newer than its latest version, so the human-only route refuses it without deleting any history.
func TestRebuildArtifactRouteIsHumanOnlyAndRefusesADocumentThatLoads(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createArtifactIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "healthy.md", "content": "before\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create healthy document: status=%d body=%s", created.Code, created.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	documentService := deps.Docs.(*docs.Service)
	if _, err := documentService.ReplaceText(context.Background(), artifact.ID, "unsettled\n", model.Actor{Kind: "user", ID: "alice"}); err != nil {
		t.Fatalf("write unsettled document update: %v", err)
	}
	if err := documentService.Evict(context.Background(), artifact.ID); err != nil {
		t.Fatalf("evict healthy document: %v", err)
	}
	var before int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifact.ID).Scan(&before); err != nil {
		t.Fatalf("count document updates before rebuild: %v", err)
	}

	path := "/api/v1/artifacts/" + artifact.ID + "/rebuild"
	bearer := sessionRequest(t, handler, http.MethodPost, path, map[string]any{"actor": sessionActor()})
	if bearer.Code != http.StatusForbidden || !strings.Contains(bearer.Body.String(), `"code":"HUMAN_ONLY"`) {
		t.Fatalf("bearer rebuild: status=%d body=%s, want 403 HUMAN_ONLY", bearer.Code, bearer.Body.String())
	}
	human := dispatchRequest(t, handler, http.MethodPost, path, map[string]any{}, "alice")
	if human.Code != http.StatusConflict || !strings.Contains(human.Body.String(), `"code":"DOCUMENT_LOADS"`) {
		t.Fatalf("healthy document rebuild: status=%d body=%s, want 409 DOCUMENT_LOADS", human.Code, human.Body.String())
	}
	var after int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from doc_updates where artifact_id = $1`, artifact.ID).Scan(&after); err != nil {
		t.Fatalf("count document updates after refused rebuild: %v", err)
	}
	if after != before {
		t.Fatalf("refused rebuild changed doc_updates from %d to %d", before, after)
	}
	if text, err := documentService.Text(context.Background(), artifact.ID); err != nil || text != "unsettled\n" {
		t.Fatalf("healthy document after refused rebuild: %q (%v), want its unsettled update", text, err)
	}
}
