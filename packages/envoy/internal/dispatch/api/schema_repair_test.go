package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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

// appendRenderOnlySchemaViolation persists a tree the validator reads but the renderer refuses: a
// checked task whose empty first paragraph precedes a heading has no markdown the browser reads
// back as a task.
func appendRenderOnlySchemaViolation(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	doc := crdt.New()
	fragment := doc.GetXmlFragment("prosemirror")
	tree := &pmdoc.Node{Type: "doc", Children: []*pmdoc.Node{{
		Type: "bullet_list",
		Children: []*pmdoc.Node{{
			Type:  "list_item",
			Attrs: pmdoc.Attrs{"checked": true},
			Children: []*pmdoc.Node{
				{Type: "paragraph"},
				{Type: "heading", Attrs: pmdoc.Attrs{"level": float64(2)}, Children: []*pmdoc.Node{{Type: "text", Text: "Unreadable task"}}},
			},
		}},
	}}}
	if err := doc.TransactE(func(transaction *crdt.Transaction) error {
		return pmdoc.Update(transaction, fragment, tree)
	}); err != nil {
		t.Fatalf("write render-only violation: %v", err)
	}
	if _, err := docs.NewPgVersioned(database).AppendUpdate(context.Background(), artifactID, crdt.EncodeStateAsUpdateV1(doc, nil)); err != nil {
		t.Fatalf("append render-only violation: %v", err)
	}
}

// An artifact whose persisted tree is outside the schema is a repairable conflict: its text route
// names the replacement, and uploading markdown under the same name restores the document. That
// holds for a tree the reader refuses and for one only the renderer refuses, which the upload
// replaces rather than failing to render.
func TestOutsideSchemaArtifactIsRepairableThroughUpload(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(*testing.T, *store.Store, string)
	}{
		{"a tree the reader refuses", appendSchemaInvalidUpdate},
		{"a tree only the renderer refuses", appendRenderOnlySchemaViolation},
	} {
		t.Run(test.name, func(t *testing.T) {
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
			test.corrupt(t, database, artifact.ID)

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
		})
	}
}
