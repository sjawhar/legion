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

// appendTextEmbed persists the update a crafted document websocket client can send: an embed in
// the first paragraph's text, which the tree reader refuses and pmdoc.Update cannot diff against.
func appendTextEmbed(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	persistence := docs.NewPgVersioned(database)
	loaded, err := persistence.Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load document before crafted embed: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode document before crafted embed: %v", err)
	}
	before := doc.StateVector()
	fragment := doc.GetXmlFragment("prosemirror")
	var text *crdt.YXmlText
	for _, block := range fragment.Children() {
		if paragraph, ok := block.(*crdt.YXmlElement); ok && paragraph.NodeName == "paragraph" {
			for _, child := range paragraph.Children() {
				if found, ok := child.(*crdt.YXmlText); ok {
					text = found
				}
			}
		}
	}
	if text == nil {
		t.Fatal("document holds no paragraph text to embed into")
	}
	doc.Transact(func(transaction *crdt.Transaction) {
		text.InsertEmbed(transaction, 0, map[string]any{"image": "crafted"}, nil)
	})
	if _, err := persistence.AppendUpdate(context.Background(), artifactID, crdt.EncodeStateAsUpdateV1(doc, before)); err != nil {
		t.Fatalf("append crafted embed: %v", err)
	}
}

// outsideSchemaCorruptions are the stored trees outside the Proof schema a document can come to
// hold: one the reader refuses, one only the renderer refuses, and one whose paragraph text
// carries an embed.
var outsideSchemaCorruptions = []struct {
	name    string
	corrupt func(*testing.T, *store.Store, string)
}{
	{"a tree the reader refuses", appendSchemaInvalidUpdate},
	{"a tree only the renderer refuses", appendRenderOnlySchemaViolation},
	{"a paragraph whose text holds an embed", appendTextEmbed},
}

// createCorruptedDocument creates a document holding "before" and gives it corrupt's stored tree,
// written while no room holds the document.
func createCorruptedDocument(t *testing.T, handler http.Handler, database *store.Store, deps Deps, issueKey, name string, corrupt func(*testing.T, *store.Store, string)) model.Artifact {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]string{
		"name": name, "content": "before\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create document %q: status=%d body=%s", name, created.Code, created.Body.String())
	}
	artifact := decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
	if err := deps.Docs.(*docs.Service).Evict(context.Background(), artifact.ID); err != nil {
		t.Fatalf("evict document before crafted update: %v", err)
	}
	corrupt(t, database, artifact.ID)
	return artifact
}

// A stored tree outside the schema is the document's own state, so every route that starts from
// it answers alike - the reads, an edit (what dispatch_doc_edit calls) and a named version - with
// 409 DOC_SCHEMA and one message naming the repair, whatever each route wraps the failure in.
func TestEveryRouteAnswersAStoredTreeOutsideTheSchemaAlike(t *testing.T) {
	for _, test := range outsideSchemaCorruptions {
		t.Run(test.name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{})
			issue := createArtifactIssue(t, handler)
			artifact := createCorruptedDocument(t, handler, database, deps, issue.Key, "routes.md", test.corrupt)
			base := "/api/v1/artifacts/" + artifact.ID
			messages := map[string]string{}
			for _, route := range []struct {
				name, method, path string
				body               any
			}{
				{"text", http.MethodGet, base + "/text", nil},
				{"blocks", http.MethodGet, base + "/blocks", nil},
				{"edits", http.MethodPost, base + "/edits", map[string]any{
					"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "More.\n"}},
				}},
				{"versions", http.MethodPost, base + "/versions", map[string]string{"summary": "checkpoint"}},
			} {
				response := dispatchRequest(t, handler, route.method, route.path, route.body, "alice")
				answer := decodeBody[struct {
					Code  string `json:"code"`
					Error string `json:"error"`
				}](t, response)
				if response.Code != http.StatusConflict || answer.Code != "DOC_SCHEMA" || !strings.Contains(answer.Error, "replace the document from markdown to repair it") {
					t.Fatalf("%s %s: status=%d body=%s, want 409 DOC_SCHEMA naming the repair", route.method, route.name, response.Code, response.Body.String())
				}
				messages[route.name] = answer.Error
			}
			for name, message := range messages {
				if message != messages["text"] {
					t.Fatalf("%s answered %q, /text %q: want one message on every route", name, message, messages["text"])
				}
			}
		})
	}
}

// An artifact whose persisted tree is outside the schema is a repairable conflict: its text route
// names the replacement, and uploading markdown under the same name restores the document. That
// holds for a tree the reader refuses, for one only the renderer refuses, and for one whose text
// holds an embed, which the upload replaces whole rather than diffing against.
func TestOutsideSchemaArtifactIsRepairableThroughUpload(t *testing.T) {
	for _, test := range outsideSchemaCorruptions {
		t.Run(test.name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{})
			issue := createArtifactIssue(t, handler)
			artifact := createCorruptedDocument(t, handler, database, deps, issue.Key, "repair.md", test.corrupt)

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
