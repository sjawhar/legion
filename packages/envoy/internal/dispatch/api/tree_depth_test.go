package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// documentDepthBound is how many levels below the document a node may stand (pmdoc's
// maxTreeDepth), the document being level 0.
const documentDepthBound = 1_000

// A live tree as deep as the schema allows is a document every read serves, and one level deeper
// is outside the schema for every read alike. A crafted browser client writes either through the
// room's CRDT, below pmdoc.Update's own validation. At the bound, the last peer's settlement stamps
// the chain's blocks and versions it, GET /text answers 200 with its markdown and token, and
// GET /blocks 200 with a token for every block, each of which hashes the subtree below it. One level
// deeper, settlement writes no version and both reads answer 500 DOC_SCHEMA.
func TestEveryReadServesATreeAtTheDepthBound(t *testing.T) {
	for _, test := range []struct {
		name     string
		depth    int
		status   int
		versions int
	}{
		{name: "at the bound", depth: documentDepthBound, status: http.StatusOK, versions: 2},
		{name: "past the bound", depth: documentDepthBound + 1, status: http.StatusInternalServerError, versions: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			documentService, handler, database := browserDocumentService(t)
			issue := createInteractionIssue(t, handler, "TEST", "Deep document", "before\n")
			artifactID := issue.PrimaryArtifactID
			sockets := &servedSockets{finished: make(map[string]chan struct{})}
			server := httptest.NewServer(sockets.serve(documentService.ServeHTTP))
			t.Cleanup(server.Close)
			peer := &syncedPeer{
				wsURL: "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/doc/" + artifactID, sockets: sockets,
				headers: http.Header{"X-Dispatch-User": []string{"alice"}}, artifactID: artifactID, doc: crdt.New(),
			}
			peer.connect(t)

			// Blockquotes stand at levels 1 through depth-2, then a paragraph, then its text at depth.
			peer.transact(t, func(txn *crdt.Transaction, fragment *crdt.YXmlFragment) error {
				fragment.Delete(txn, 0, len(fragment.Children()))
				parent := crdt.NewYXmlElement("blockquote")
				fragment.InsertElement(txn, 0, parent)
				for range test.depth - 3 {
					child := crdt.NewYXmlElement("blockquote")
					parent.InsertElement(txn, 0, child)
					parent = child
				}
				paragraph := crdt.NewYXmlElement("paragraph")
				parent.InsertElement(txn, 0, paragraph)
				text := crdt.NewYXmlText()
				paragraph.InsertText(txn, 0, text)
				text.Insert(txn, 0, "deepest", nil)
				return nil
			})
			peer.barrier(t)
			peer.mu.Lock()
			socketID := peer.socketID
			peer.mu.Unlock()
			peer.close()
			select {
			case <-sockets.done(socketID):
			case <-time.After(30 * time.Second):
				t.Fatal("the document server did not let go of the browser's connection")
			}

			var versions int
			if err := database.Pool.QueryRow(context.Background(), `select count(*) from artifact_versions where artifact_id = $1`, artifactID).Scan(&versions); err != nil {
				t.Fatalf("count document versions: %v", err)
			}
			if versions != test.versions {
				t.Fatalf("settlement left %d versions of a tree %d levels deep, want %d", versions, test.depth, test.versions)
			}

			text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")
			blocks := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/blocks", nil, "alice")
			for _, read := range []struct {
				route    string
				response *httptest.ResponseRecorder
			}{{"GET /text", text}, {"GET /blocks", blocks}} {
				if read.response.Code != test.status {
					t.Fatalf("%s of a tree %d levels deep: status=%d body=%.300s, want %d", read.route, test.depth, read.response.Code, read.response.Body.String(), test.status)
				}
				if test.status != http.StatusOK && !strings.Contains(read.response.Body.String(), `"code":"DOC_SCHEMA"`) {
					t.Fatalf("%s of a tree %d levels deep: body=%.300s, want DOC_SCHEMA", read.route, test.depth, read.response.Body.String())
				}
			}
			if test.status != http.StatusOK {
				return
			}
			document := decodeBody[struct {
				Markdown string `json:"markdown"`
				Version  int    `json:"version"`
				Token    string `json:"token"`
			}](t, text)
			if !strings.Contains(document.Markdown, "deepest") || document.Version != test.versions || !strings.HasPrefix(document.Token, "sha256:") {
				t.Fatalf("GET /text of a tree %d levels deep: version %d, token %q, markdown ends %q", test.depth, document.Version, document.Token, document.Markdown[max(0, len(document.Markdown)-80):])
			}
			stamped := decodeBody[[]model.ArtifactBlock](t, blocks)
			if len(stamped) != test.depth-1 {
				t.Fatalf("GET /blocks of a tree %d levels deep lists %d blocks, want %d", test.depth, len(stamped), test.depth-1)
			}
			for _, block := range stamped {
				if !strings.HasPrefix(block.Token, "sha256:") {
					t.Fatalf("block %q (%s) has token %q", block.ID, block.Type, block.Token)
				}
			}
		})
	}
}
