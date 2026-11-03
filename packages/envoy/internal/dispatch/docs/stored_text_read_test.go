package docs

import (
	"context"
	"testing"
	"time"
)

// storedTextReadRendersBase is how often a text read of a document a room holds renders it today,
// the base this measurement records: TextWithToken reads the room's tree (heldTree) and renders
// it, though the version written from that very state already stored its markdown. The change's
// commit lowers it to zero and makes the markdown the read answers the version's stored markdown,
// byte for byte.
const storedTextReadRendersBase = 1

// A text read of a document whose room still holds the state its latest version was written from
// renders the document storedTextReadRendersBase times.
func TestStoredTextReadCountsTheRendersOfAReadOfAVersionedDocument(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	const written = "The quick brown fox.\n"
	seedServiceText(t, service, artifactID, written)
	// The settlement writes the version from the room's document, and nothing edits the room
	// afterwards, so the room holds the state the version was written from.
	settleCurrentGeneration(t, service, artifactID)
	requireLatestVersionMarkdown(t, service, artifactID, written)
	if tree := liveTree(t, service, artifactID); tree == nil {
		t.Fatal("the room holds no document to read")
	}
	if service.srv.GetDoc(artifactID) == nil {
		t.Fatal("no room holds the document, so the read would be a cold one")
	}

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
	if token == "" {
		t.Fatal("the read answered no document token")
	}
	if counted != storedTextReadRendersBase {
		t.Fatalf("the read rendered the document %d times, want %d", counted, storedTextReadRendersBase)
	}
}
