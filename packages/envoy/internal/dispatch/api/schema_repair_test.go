package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
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

// appendNestedLink persists a link over the first paragraph's text whose value nests 100 arrays
// deep, one more than a mark's attribute may: the update a crafted document websocket client can
// send, and what a server whose bound did not count the map ygo stores a mark as could write.
func appendNestedLink(t *testing.T, database *store.Store, artifactID string) {
	t.Helper()
	persistence := docs.NewPgVersioned(database)
	loaded, err := persistence.Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load document before crafted link: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode document before crafted link: %v", err)
	}
	if _, err := persistence.AppendUpdate(context.Background(), artifactID, docstest.NestedLinkUpdate(t, doc, docstest.NestedValue(100))); err != nil {
		t.Fatalf("append crafted link: %v", err)
	}
}

// outsideSchemaCorruptions are the stored trees outside the Proof schema a document can come to
// hold: one the reader refuses, one only the renderer refuses, one whose paragraph text carries an
// embed, and one whose text carries a mark nested past what ygo's text writers take.
var outsideSchemaCorruptions = []struct {
	name    string
	corrupt func(*testing.T, *store.Store, string)
}{
	{"a tree the reader refuses", appendSchemaInvalidUpdate},
	{"a tree only the renderer refuses", func(t *testing.T, database *store.Store, artifactID string) {
		if err := docs.AppendRenderOnlySchemaViolationForTest(context.Background(), database, artifactID); err != nil {
			t.Fatal(err)
		}
	}},
	{"a paragraph whose text holds an embed", appendTextEmbed},
	{"a mark nested past the bound", appendNestedLink},
}

// createCorruptedDocument creates a document holding "before" and gives it corrupt's stored tree,
// written while no room holds the document.
func createCorruptedDocument(t *testing.T, handler http.Handler, database *store.Store, deps Deps, issueKey, name string, corrupt func(*testing.T, *store.Store, string)) model.Artifact {
	t.Helper()
	artifact := createBeforeDocument(t, handler, issueKey, name)
	corruptDocument(t, database, deps, artifact.ID, corrupt)
	return artifact
}

// createBeforeDocument creates a document holding "before", for a test that writes to it (an
// anchored comment, say) before corruptDocument gives it a stored tree outside the schema.
func createBeforeDocument(t *testing.T, handler http.Handler, issueKey, name string) model.Artifact {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/artifacts", map[string]string{
		"name": name, "content": "before\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create document %q: status=%d body=%s", name, created.Code, created.Body.String())
	}
	return decodeBody[struct {
		Artifact model.Artifact `json:"artifact"`
	}](t, created).Artifact
}

// corruptDocument gives the document corrupt's stored tree, written while no room holds the
// document, as a crafted client or an older server could have left it.
func corruptDocument(t *testing.T, database *store.Store, deps Deps, artifactID string, corrupt func(*testing.T, *store.Store, string)) {
	t.Helper()
	if err := deps.Docs.(*docs.Service).Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict document before crafted update: %v", err)
	}
	corrupt(t, database, artifactID)
}

// createAnchoredComment comments on the document's "before" and returns the comment.
func createAnchoredComment(t *testing.T, handler http.Handler, issueKey, artifactID, body string) model.Comment {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/comments", map[string]any{
		"body": body, "anchor": map[string]any{"artifact": artifactID, "quote": "before"},
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create anchored comment %q: status=%d body=%s", body, created.Code, created.Body.String())
	}
	return decodeBody[model.Comment](t, created)
}

// A stored tree outside the schema is the document's own state, so every route that starts from
// it answers alike - the reads, an edit (what `dispatch doc-edit` calls), a named version, and the
// comment routes that rewrite an anchored thread's margin record (a reply, a resolve, a reopen and
// an edit of the comment) - with 409 DOC_SCHEMA and one message naming the repair, whatever each
// route wraps the failure in. The margin record lives beside the document text, so a route that
// writes only the margin could be tempted to skip reading the tree; for a tree only the renderer
// refuses, the render is the one thing that finds the corruption, so the comment routes must
// still refuse it as /text does. The threads are anchored before the corruption, as a document
// that came to hold such a tree after its review began would have them.
func TestEveryRouteAnswersAStoredTreeOutsideTheSchemaAlike(t *testing.T) {
	for _, test := range outsideSchemaCorruptions {
		t.Run(test.name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{})
			issue := createArtifactIssue(t, handler)
			artifact := createBeforeDocument(t, handler, issue.Key, "routes.md")
			open := createAnchoredComment(t, handler, issue.Key, artifact.ID, "open thread")
			resolved := createAnchoredComment(t, handler, issue.Key, artifact.ID, "resolved thread")
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+resolved.ID+"/resolve", nil, "alice"); response.Code != http.StatusOK {
				t.Fatalf("resolve before corruption: status=%d body=%s", response.Code, response.Body.String())
			}
			corruptDocument(t, database, deps, artifact.ID, test.corrupt)
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
				{"comment reply", http.MethodPost, "/api/v1/issues/" + issue.Key + "/comments", map[string]any{
					"body": "a reply", "reply_to": open.ID,
				}},
				{"comment resolve", http.MethodPost, "/api/v1/comments/" + open.ID + "/resolve", nil},
				{"comment reopen", http.MethodPost, "/api/v1/comments/" + resolved.ID + "/reopen", nil},
				{"comment edit", http.MethodPatch, "/api/v1/comments/" + open.ID, map[string]string{"body": "edited thread"}},
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
