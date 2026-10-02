package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

func appendSchemaInvalidUpdate(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	doc.Transact(func(transaction *crdt.Transaction) {
		fragment.InsertElement(transaction, 0, crdt.NewYXmlElement("callout"))
	})
	if _, err := docs.NewPgVersioned(database).AppendUpdate(context.Background(), artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		t.Fatalf("append crafted document update: %v", err)
	}
}

// An artifact whose persisted tree is outside the schema is a repairable conflict: its text route
// names the replacement, and uploading markdown under the same name restores the document.
func TestOutsideSchemaArtifactIsRepairableThroughUpload(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	issue := createArtifactIssue(t, handler)
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{
		"name": "repair.md", "content": "before\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create repair document: status=%d body=%s", created.Code, created.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	if err := deps.Docs.(*docs.Service).Evict(context.Background(), artifact.ID); err != nil {
		t.Fatalf("evict document before crafted update: %v", err)
	}
	appendSchemaInvalidUpdate(t, database, artifact.ID)

	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text", nil, "alice")
	if text.Code != http.StatusConflict || !strings.Contains(text.Body.String(), `"code":"DOC_SCHEMA"`) || !strings.Contains(text.Body.String(), "replace the document from markdown") {
		t.Fatalf("read outside-schema document: status=%d body=%s, want 409 DOC_SCHEMA naming the repair", text.Code, text.Body.String())
	}
	repaired := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts", map[string]string{"name": "repair.md"}, "repair.md", "text/markdown", []byte("repaired\n"), "alice")
	if repaired.Code != http.StatusCreated {
		t.Fatalf("repair document upload: status=%d body=%s", repaired.Code, repaired.Body.String())
	}
	text = dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifact.ID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), `"markdown":"repaired\n"`) {
		t.Fatalf("read repaired document: status=%d body=%s", text.Code, text.Body.String())
	}
}
