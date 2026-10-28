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

// loadStoredDocument is the document artifactID's stored history decodes to.
func loadStoredDocument(t *testing.T, database *store.Store, artifactID string) *crdt.Doc {
	t.Helper()
	loaded, err := docs.NewPgVersioned(database).Load(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("load stored document: %v", err)
	}
	doc := crdt.New()
	if err := crdt.ApplyUpdateV1(doc, loaded.Update, nil); err != nil {
		t.Fatalf("decode stored document: %v", err)
	}
	return doc
}

// appendStoredChange persists the update change makes to artifactID's stored document: the update a
// crafted document websocket client can send, written below pmdoc.Update's validation.
func appendStoredChange(t *testing.T, database *store.Store, artifactID string, change func(*crdt.Transaction, *crdt.YXmlFragment)) {
	t.Helper()
	doc := loadStoredDocument(t, database, artifactID)
	before := doc.StateVector()
	fragment := doc.GetXmlFragment("prosemirror")
	doc.Transact(func(transaction *crdt.Transaction) { change(transaction, fragment) })
	if _, err := docs.NewPgVersioned(database).AppendUpdate(context.Background(), artifactID, crdt.EncodeStateAsUpdateV1(doc, before)); err != nil {
		t.Fatalf("append crafted update: %v", err)
	}
}

// appendTextChange persists change to the first text of artifactID's stored document, the text of
// its paragraph "before".
func appendTextChange(t *testing.T, database *store.Store, artifactID string, change func(*crdt.Transaction, *crdt.YXmlText)) {
	t.Helper()
	appendStoredChange(t, database, artifactID, func(transaction *crdt.Transaction, fragment *crdt.YXmlFragment) {
		text := docstest.FirstText(fragment)
		if text == nil {
			t.Fatal("document holds no paragraph text to change")
		}
		change(transaction, text)
	})
}

// appendTextEmbed persists the update a crafted document websocket client can send: an embed in
// the first paragraph's text, which the tree reader refuses and pmdoc.Update cannot diff against.
func appendTextEmbed(t *testing.T, database *store.Store, artifactID string) {
	appendTextChange(t, database, artifactID, func(transaction *crdt.Transaction, text *crdt.YXmlText) {
		text.InsertEmbed(transaction, 0, map[string]any{"image": "crafted"}, nil)
	})
}

// appendStringEmbed persists an embed holding a string at the start of the first paragraph's
// text: an embed the reader takes as that string's text, so the tree stays inside the schema.
func appendStringEmbed(t *testing.T, database *store.Store, artifactID string) {
	appendTextChange(t, database, artifactID, func(transaction *crdt.Transaction, text *crdt.YXmlText) {
		text.InsertEmbed(transaction, 0, "embedded ", nil)
	})
}

// appendTextMark persists attrs, one mark, over the whole of the first paragraph's text, so the
// text's one run carries that mark alone.
func appendTextMark(attrs crdt.Attributes) func(*testing.T, *store.Store, string) {
	return func(t *testing.T, database *store.Store, artifactID string) {
		appendTextChange(t, database, artifactID, func(transaction *crdt.Transaction, text *crdt.YXmlText) {
			text.Format(transaction, 0, text.Len(), attrs)
		})
	}
}

// appendMarkedCodeBlock persists, as the document's first block, a code block whose text carries
// a strong mark: the one place a Y tree can put a mark on a node the schema keeps marks off, since
// the reader gives marks to text alone and a code block's text holds only the record marks.
func appendMarkedCodeBlock(t *testing.T, database *store.Store, artifactID string) {
	appendStoredChange(t, database, artifactID, func(transaction *crdt.Transaction, fragment *crdt.YXmlFragment) {
		code := crdt.NewYXmlElement("code_block")
		fragment.InsertElement(transaction, 0, code)
		run := crdt.NewYXmlText()
		code.InsertText(transaction, 0, run)
		run.Insert(transaction, 0, "code", crdt.Attributes{"strong": map[string]any{}})
	})
}

// appendDeepChain persists, as the document's first block, the chain docstest.WriteDeepChain
// writes with its text textLevel levels below the document.
func appendDeepChain(textLevel int) func(*testing.T, *store.Store, string) {
	return func(t *testing.T, database *store.Store, artifactID string) {
		appendStoredChange(t, database, artifactID, func(transaction *crdt.Transaction, fragment *crdt.YXmlFragment) {
			docstest.WriteDeepChain(transaction, fragment, textLevel, "deep")
		})
	}
}

// storedBlockID is the block id of the first top-level block of artifactID's stored document that
// carries one: the paragraph "before" the document was created with.
func storedBlockID(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	for _, child := range loadStoredDocument(t, database, artifactID).GetXmlFragment("prosemirror").Children() {
		if element, ok := child.(*crdt.YXmlElement); ok {
			if id, ok := element.GetAttribute(pmdoc.BlockIDAttr); ok && id != "" {
				return id
			}
		}
	}
	t.Fatal("stored document holds no block with an id")
	return ""
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
// hold, each with the error every route answers it with: one the reader refuses, one only the
// renderer refuses, one whose paragraph text carries an embed, one whose text carries a mark nested
// past what ygo's text writers take, and the malformed trees pmdoc's own tests refuse - a mark
// where the schema keeps marks off, an unknown mark, a mark whose value is not an object, a tree one
// level past the depth bound, and a tree at the bound whose text carries a mark past its nesting
// bound. treeReads marks the tree the reader takes and only the renderer refuses: the block path
// reads the stored tree where its rendering is refused (Service.readDocument), so it answers that
// tree's path.
var outsideSchemaCorruptions = []struct {
	name      string
	corrupt   func(*testing.T, *store.Store, string)
	refusal   string
	treeReads bool
}{
	{name: "a tree the reader refuses", corrupt: appendSchemaInvalidUpdate,
		refusal: outsideSchemaRefusal + `typed block "callout" content "block+" requires one or more blocks`},
	{name: "a tree only the renderer refuses", corrupt: func(t *testing.T, database *store.Store, artifactID string) {
		if err := docs.AppendRenderOnlySchemaViolationForTest(context.Background(), database, artifactID); err != nil {
			t.Fatal(err)
		}
	}, refusal: outsideSchemaRefusal + "a task item whose first paragraph is empty cannot hold another block after it; the browser editor reads no such item as a task", treeReads: true},
	{name: "a paragraph whose text holds an embed", corrupt: appendTextEmbed,
		refusal: outsideSchemaRefusal + "unsupported yjs text embed map[string]interface {}"},
	{name: "a mark nested past the bound", corrupt: appendNestedLink,
		refusal: outsideSchemaRefusal + `mark "link" attribute "nested" nests more than 99 arrays and objects`},
	{name: "a mark on a code block's text", corrupt: appendMarkedCodeBlock,
		refusal: outsideSchemaRefusal + `code block cannot contain mark "strong"`},
	{name: "an unknown mark", corrupt: appendTextMark(crdt.Attributes{"unknown": map[string]any{}}),
		refusal: outsideSchemaRefusal + `mark "unknown"`},
	{name: "a mark whose value is not an object", corrupt: appendTextMark(crdt.Attributes{"strong": true}),
		refusal: outsideSchemaRefusal + `mark "strong" attributes: want object, got bool`},
	{name: "a tree one level past the depth bound", corrupt: appendDeepChain(pmdoc.MaxTreeDepth + 1),
		refusal: outsideSchemaRefusal + "a node 1001 levels deep; a document nests at most 1000 levels"},
	{name: "a mark nested past the bound on text at the depth bound", corrupt: func(t *testing.T, database *store.Store, artifactID string) {
		appendDeepChain(pmdoc.MaxTreeDepth)(t, database, artifactID)
		appendNestedLink(t, database, artifactID)
	}, refusal: outsideSchemaRefusal + `mark "link" attribute "nested" nests more than 99 arrays and objects`},
}

// outsideSchemaRefusal opens every refusal of a stored tree outside the schema: the repair, then
// the schema refusal that names what is wrong.
const outsideSchemaRefusal = "document is outside the Proof schema; replace the document from markdown to repair it: outside Proof schema: "

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
// it answers alike - the reads, the path of one of its blocks, an edit (what `dispatch doc-edit`
// calls) and a named version - with 409 DOC_SCHEMA and the one message that names what is wrong
// and the repair, whatever each route wraps the failure in. The block path of a tree only the
// renderer refuses is read off the tree, which renders nothing, so it answers the block's path. A
// text embed holding a string is text, so a document holding one reads.
func TestEveryRouteAnswersAStoredTreeOutsideTheSchemaAlike(t *testing.T) {
	for _, test := range outsideSchemaCorruptions {
		t.Run(test.name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{})
			issue := createArtifactIssue(t, handler)
			artifact := createCorruptedDocument(t, handler, database, deps, issue.Key, "routes.md", test.corrupt)
			base := "/api/v1/artifacts/" + artifact.ID
			blockID := storedBlockID(t, database, artifact.ID)
			for _, route := range []struct {
				name, method, path string
				body               any
			}{
				{"text", http.MethodGet, base + "/text", nil},
				{"blocks", http.MethodGet, base + "/blocks", nil},
				{"block path", http.MethodGet, base + "/blocks/" + blockID, nil},
				{"edits", http.MethodPost, base + "/edits", map[string]any{
					"ops": []map[string]string{{"op": "insert", "after": "end", "markdown": "More.\n"}},
				}},
				{"versions", http.MethodPost, base + "/versions", map[string]string{"summary": "checkpoint"}},
			} {
				response := dispatchRequest(t, handler, route.method, route.path, route.body, "alice")
				if route.name == "block path" && test.treeReads {
					path := decodeBody[model.BlockPath](t, response)
					if response.Code != http.StatusOK || path.ID != blockID || path.Type != "paragraph" || len(path.Path) != 1 || path.Path[0].ID != blockID || path.Path[0].Type != "paragraph" {
						t.Fatalf("%s %s: status=%d body=%s, want 200 and the top-level paragraph %s", route.method, route.name, response.Code, response.Body.String(), blockID)
					}
					continue
				}
				answer := decodeBody[struct {
					Code  string `json:"code"`
					Error string `json:"error"`
				}](t, response)
				if response.Code != http.StatusConflict || answer.Code != "DOC_SCHEMA" || answer.Error != test.refusal {
					t.Fatalf("%s %s: status=%d body=%s, want 409 DOC_SCHEMA %q", route.method, route.name, response.Code, response.Body.String(), test.refusal)
				}
			}
		})
	}

	t.Run("a paragraph whose text holds a string embed", func(t *testing.T) {
		handler, database, deps := newTestServer(t, testServerOptions{})
		issue := createArtifactIssue(t, handler)
		artifact := createCorruptedDocument(t, handler, database, deps, issue.Key, "routes.md", appendStringEmbed)
		base := "/api/v1/artifacts/" + artifact.ID
		text := dispatchRequest(t, handler, http.MethodGet, base+"/text", nil, "alice")
		blocks := dispatchRequest(t, handler, http.MethodGet, base+"/blocks", nil, "alice")
		t.Logf("GET /text: %d %s", text.Code, text.Body.String())
		t.Logf("GET /blocks: %d %s", blocks.Code, blocks.Body.String())
		if text.Code != http.StatusOK || blocks.Code != http.StatusOK {
			t.Fatalf("read a text holding a string embed: /text %d %s, /blocks %d %s, want 200 each", text.Code, text.Body.String(), blocks.Code, blocks.Body.String())
		}
		document := decodeBody[struct {
			Markdown string `json:"markdown"`
			Version  int    `json:"version"`
		}](t, text)
		if document.Markdown != "embedded before\n" || document.Version != 1 {
			t.Fatalf("GET /text of a text holding a string embed: %s, want the embed's string before the text, version 1", text.Body.String())
		}
		read := decodeBody[[]model.ArtifactBlock](t, blocks)
		if len(read) != 1 || read[0].ID != storedBlockID(t, database, artifact.ID) || read[0].Type != "paragraph" || read[0].From != 0 || read[0].To != len("embedded before") {
			t.Fatalf("GET /blocks of a text holding a string embed: %s, want its one paragraph over the embed and the text", blocks.Body.String())
		}
	})
}

// An artifact whose persisted tree is outside the schema is a repairable conflict: its text route
// names the replacement, and uploading markdown under the same name restores the document. That
// holds for every stored tree outsideSchemaCorruptions lists, one whose text holds an embed among
// them, which the upload replaces whole rather than diffing against.
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
