package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/docs/crafted"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

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

// appendStringEmbed persists an embed holding a string at the start of the document's first text,
// the text of its paragraph "before": an embed the reader takes as that string's text, so the tree
// stays inside the schema.
func appendStringEmbed(ctx context.Context, database *store.Store, artifactID string) error {
	return docs.AppendStoredChangeForTest(ctx, database, artifactID, func(transaction *crdt.Transaction, fragment *crdt.YXmlFragment) error {
		text := crafted.FirstText(fragment)
		if text == nil {
			return errors.New("document holds no paragraph text to embed into")
		}
		text.InsertEmbed(transaction, 0, "embedded ", nil)
		return nil
	})
}

// appendDeepChain persists, as the document's first block, the chain docstest.WriteDeepChain
// writes with its text textLevel levels below the document.
func appendDeepChain(textLevel int) func(context.Context, *store.Store, string) error {
	return func(ctx context.Context, database *store.Store, artifactID string) error {
		return docs.AppendStoredChangeForTest(ctx, database, artifactID, func(transaction *crdt.Transaction, fragment *crdt.YXmlFragment) error {
			docstest.WriteDeepChain(transaction, fragment, textLevel, "deep")
			return nil
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

// outsideSchemaCorruptions are the stored trees docs.OutsideSchemaCorruptionsForTest lists and the
// two past the depth bound pmdoc's own tests refuse: a tree one level past the bound, and a tree at
// the bound whose text carries a mark past its nesting bound. Their chain is docstest's, which the
// server's test hooks cannot link.
var outsideSchemaCorruptions = slices.Concat(docs.OutsideSchemaCorruptionsForTest, []docs.OutsideSchemaCorruptionForTest{
	{Name: "a tree one level past the depth bound", Corrupt: appendDeepChain(pmdoc.MaxTreeDepth + 1),
		Refusal: "a node 1001 levels deep; a document nests at most 1000 levels"},
	{Name: "a mark nested past the bound on text at the depth bound", Corrupt: func(ctx context.Context, database *store.Store, artifactID string) error {
		if err := appendDeepChain(pmdoc.MaxTreeDepth)(ctx, database, artifactID); err != nil {
			return err
		}
		return docs.AppendNestedLinkForTest(ctx, database, artifactID)
	}, Refusal: `mark "link" attribute "nested" nests more than 99 arrays and objects`},
})

// outsideSchemaRefusal opens every refusal of a stored tree outside the schema: the repair, then
// the schema refusal that names what is wrong.
const outsideSchemaRefusal = "document is outside the Proof schema; replace the document from markdown to repair it: outside Proof schema: "

// createCorruptedDocument creates a document holding "before" and gives it corrupt's stored tree,
// written while no room holds the document.
func createCorruptedDocument(t *testing.T, handler http.Handler, database *store.Store, deps Deps, issueKey, name string, corrupt func(context.Context, *store.Store, string) error) model.Artifact {
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
func corruptDocument(t *testing.T, database *store.Store, deps Deps, artifactID string, corrupt func(context.Context, *store.Store, string) error) {
	t.Helper()
	if err := deps.Docs.(*docs.Service).Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict document before crafted update: %v", err)
	}
	if err := corrupt(context.Background(), database, artifactID); err != nil {
		t.Fatal(err)
	}
}

// createAnchoredComment comments on the document's "before" and returns the comment.
func createAnchoredComment(t *testing.T, handler http.Handler, issueKey, artifactID, body string) model.Comment {
	t.Helper()
	return createThreadComment(t, handler, issueKey, map[string]any{
		"body": body, "anchor": map[string]any{"artifact": artifactID, "quote": "before"},
	}, "alice")
}

// A stored tree outside the schema is the document's own state, so every route that starts from
// it answers alike - the reads, the path of one of its blocks, an edit (what `dispatch doc-edit`
// calls), a named version, and the comment routes that rewrite an anchored thread's margin record
// (a reply, a resolve, a reopen and an edit of the comment) - with 409 DOC_SCHEMA and the one
// message that names what is wrong and the repair, whatever each route wraps the failure in. The
// margin record lives beside the document text, so a route that writes only the margin could be
// tempted to skip reading the tree; for a tree only the renderer refuses, the render is the one
// thing that finds the corruption, so the comment routes must still refuse it as /text does. The
// block path of that tree is read off the tree, which renders nothing, so it answers the block's
// path. The threads are anchored before the corruption, as a document that came to hold such a
// tree after its review began would have them. A text embed holding a string is text, so a
// document holding one reads.
func TestEveryRouteAnswersAStoredTreeOutsideTheSchemaAlike(t *testing.T) {
	for _, test := range outsideSchemaCorruptions {
		t.Run(test.Name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{})
			issue := createArtifactIssue(t, handler)
			artifact := createBeforeDocument(t, handler, issue.Key, "routes.md")
			open := createAnchoredComment(t, handler, issue.Key, artifact.ID, "open thread")
			resolved := createAnchoredComment(t, handler, issue.Key, artifact.ID, "resolved thread")
			if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/comments/"+resolved.ID+"/resolve", nil, "alice"); response.Code != http.StatusOK {
				t.Fatalf("resolve before corruption: status=%d body=%s", response.Code, response.Body.String())
			}
			corruptDocument(t, database, deps, artifact.ID, test.Corrupt)
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
				{"comment reply", http.MethodPost, "/api/v1/issues/" + issue.Key + "/comments", map[string]any{
					"body": "a reply", "reply_to": open.ID,
				}},
				{"comment resolve", http.MethodPost, "/api/v1/comments/" + open.ID + "/resolve", nil},
				{"comment reopen", http.MethodPost, "/api/v1/comments/" + resolved.ID + "/reopen", nil},
				{"comment edit", http.MethodPatch, "/api/v1/comments/" + open.ID, map[string]string{"body": "edited thread"}},
			} {
				response := dispatchRequest(t, handler, route.method, route.path, route.body, "alice")
				if route.name == "block path" && test.RenderOnly {
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
				if refusal := outsideSchemaRefusal + test.Refusal; response.Code != http.StatusConflict || answer.Code != "DOC_SCHEMA" || answer.Error != refusal {
					t.Fatalf("%s %s: status=%d body=%s, want 409 DOC_SCHEMA %q", route.method, route.name, response.Code, response.Body.String(), refusal)
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
		t.Run(test.Name, func(t *testing.T) {
			handler, database, deps := newTestServer(t, testServerOptions{})
			issue := createArtifactIssue(t, handler)
			artifact := createCorruptedDocument(t, handler, database, deps, issue.Key, "repair.md", test.Corrupt)

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
