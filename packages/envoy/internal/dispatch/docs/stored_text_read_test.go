package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// storedTextReadRenders is how often a text read of a document a room holds renders it: not at
// all. The version written from that very state stored its markdown and its document token beside
// what they were rendered from, and the read answers them without reading the document
// (TextWithToken, storedText). The base this measurement records was one: the read took the
// room's tree (heldTree) and rendered it, though the version held that rendering already.
const storedTextReadRenders = 0

// storedTextReadRendersUnserved is how often a read renders the document where the stored
// rendering is not the document's own: once, as it always did.
const storedTextReadRendersUnserved = 1

// seedSettledDocument seeds markdown as the document's text and settles it, so the document's
// latest version was written from the state its room still holds, and the room holds it.
func seedSettledDocument(t *testing.T, service *Service, artifactID, markdown string) {
	t.Helper()
	seedServiceText(t, service, artifactID, markdown)
	// The settlement writes the version from the room's document, and nothing edits the room
	// afterwards, so the room holds the state the version was written from.
	settleCurrentGeneration(t, service, artifactID)
	requireLatestVersionMarkdown(t, service, artifactID, markdown)
	if tree := liveTree(t, service, artifactID); tree == nil {
		t.Fatal("the room holds no document to read")
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("no room holds the document, so the read would be a cold one")
	}
}

// A text read of a document whose room still holds the state its latest version was written from
// renders the document storedTextReadRenders times, and answers the version's stored markdown.
func TestStoredTextReadCountsTheRendersOfAReadOfAVersionedDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const written = "The quick brown fox.\n"
	seedSettledDocument(t, service, artifactID, written)
	stored := latestVersionMarkdown(t, service, artifactID)

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), artifactID)
	counted := renders()
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	t.Logf("a text read of a document its room holds costs %d renders of it", counted)
	if markdown != written {
		t.Fatalf("the read answered %q, want the version's markdown %q", markdown, written)
	}
	// The read answers the very bytes the version stored, not a rendering that agrees with them.
	if markdown != stored {
		t.Fatalf("the read answered %q, want the version's stored markdown %q, byte for byte", markdown, stored)
	}
	// The token is the live document's own, so a guarded edit holding it means what it meant
	// when the read rendered the document itself.
	live, err := nodeToken(liveTree(t, service, artifactID))
	if err != nil {
		t.Fatalf("take the live document's token: %v", err)
	}
	if token == "" || token != live {
		t.Fatalf("the read answered token %q, want the live document's %q", token, live)
	}
	if counted != storedTextReadRenders {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRenders)
	}
}

// A room that has applied a browser's update and not yet stored it holds text no version does, so
// the read renders the room rather than serving the version's markdown.
func TestStoredTextReadRendersARoomHoldingAnUnstoredUpdate(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedSettledDocument(t, service, artifactID, "The quick brown fox.\n")
	// The peer's update is applied to the room and counted among the updates it has not stored
	// (queueUpdateClass), and its observer is held before persistence is handed it.
	release := holdPeerEdit(t, service, artifactID, &service.afterCreditUpdate, replaceRun("fox.", "dog."))
	defer release()

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), artifactID)
	counted := renders()
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	if markdown != "The quick brown dog.\n" {
		t.Fatalf("the read answered %q, want the room's unstored text", markdown)
	}
	if token == "" {
		t.Fatal("the read answered no document token")
	}
	if counted != storedTextReadRendersUnserved {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRendersUnserved)
	}
}

// An anchor write appends an update and writes no version, so the document's stamp moves past the
// one its latest version holds and the read renders the document.
func TestStoredTextReadRendersAfterAnAnchorWriteMovedTheStamp(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const written = "The quick brown fox.\n"
	seedSettledDocument(t, service, artifactID, written)
	spec := MarkSpec{Kind: MarkComment, ID: "comment-1", By: model.Actor{Kind: "session", ID: "session-0123456789abcdef"}}
	if _, err := joinedMarkQuote(service, artifactID, spec, "brown", nil); err != nil {
		t.Fatalf("anchor a comment: %v", err)
	}

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), artifactID)
	counted := renders()
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	// A mark renders nothing, so the text is the version's all the same; the stamp is not.
	if markdown != written {
		t.Fatalf("the read answered %q, want %q", markdown, written)
	}
	if token == "" {
		t.Fatal("the read answered no document token")
	}
	if counted != storedTextReadRendersUnserved {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRendersUnserved)
	}
}

// A version another renderer wrote is not this build's to serve: its markdown and its token are
// what that renderer made of the tree, so the read renders the document itself.
func TestStoredTextReadRendersAVersionOfAnotherRendererVersion(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const written = "The quick brown fox.\n"
	seedSettledDocument(t, service, artifactID, written)
	if _, err := service.store.Pool.Exec(context.Background(), `
		update artifact_versions set renderer_version = $2 where artifact_id = $1
	`, artifactID, pmdoc.RendererVersion+1); err != nil {
		t.Fatalf("stamp the version with another renderer version: %v", err)
	}

	renders := countRenders(t)
	markdown, token, err := service.TextWithToken(context.Background(), artifactID)
	counted := renders()
	if err != nil {
		t.Fatalf("read the document's text: %v", err)
	}
	if markdown != written {
		t.Fatalf("the read answered %q, want %q", markdown, written)
	}
	if token == "" {
		t.Fatal("the read answered no document token")
	}
	if counted != storedTextReadRendersUnserved {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRendersUnserved)
	}
}
