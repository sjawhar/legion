package docs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// The rows the store keeps for a room fold to the room's own document when a peer's update parks in
// the room: a keystroke sent before the keystroke it was typed after waits in the room's pending
// queue, which the update the room hands its store leaves out, and the room integrates it once the
// earlier keystroke arrives. A load of the stored rows - the fold a room load and a compaction make -
// reads the document the room holds, both while the keystroke waits and once it has landed.
func TestTheStoredRowsOfARoomFoldToItsDocumentWhenAPeerUpdateParks(t *testing.T) {
	service, artifactID := newTestService(t)
	seedServiceText(t, service, artifactID, "Seed.\n")
	server := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
	t.Cleanup(server.Close)
	alpha := connectPeer(t, server.URL, artifactID, "alice")
	beta := connectPeer(t, server.URL, artifactID, "alice")
	waitFor(t, 10*time.Second, "both peers to hold the room's document", func() bool {
		return strings.TrimSpace(peerMarkdown(alpha)) == "Seed." && strings.TrimSpace(peerMarkdown(beta)) == "Seed."
	})

	// Two keystrokes of one writer, the second typed after the first, so the second parks in a
	// document that lacks the first.
	writer := crdt.New()
	if err := crdt.ApplyUpdateV1(writer, crdt.EncodeStateAsUpdateV1(alpha.Doc, nil), nil); err != nil {
		t.Fatalf("copy the peer's document: %v", err)
	}
	first := docstest.Transact(writer, typeParagraph(writer, "first"))
	second := docstest.Transact(writer, typeParagraph(writer, "second"))

	if err := alpha.Write(ygsync.EncodeUpdate(second)); err != nil {
		t.Fatalf("send the second keystroke: %v", err)
	}
	waitFor(t, 10*time.Second, "the room to park the second keystroke", func() bool {
		live := service.srv.GetDoc(artifactID)
		return live != nil && live.PendingStats().Items > 0
	})
	requireStoredRowsFoldToTheRoom(t, service, artifactID, "while the second keystroke waits")
	if got := strings.TrimSpace(peerMarkdown(beta)); got != "Seed." {
		t.Fatalf("the other peer reads %q while the second keystroke waits, want only the seed", got)
	}

	if err := alpha.Write(ygsync.EncodeUpdate(first)); err != nil {
		t.Fatalf("send the first keystroke: %v", err)
	}
	waitFor(t, 10*time.Second, "the other peer to receive both keystrokes", func() bool {
		return strings.HasPrefix(peerMarkdown(beta), "second\n\nfirst\n\nSeed.")
	})
	requireStoredRowsFoldToTheRoom(t, service, artifactID, "once the first keystroke landed")
	if got := liveMarkdown(t, service, artifactID); !strings.HasPrefix(got, "second\n\nfirst\n\nSeed.") {
		t.Fatalf("the room reads %q once both keystrokes landed, want second, first, then the seed", got)
	}
}

// requireStoredRowsFoldToTheRoom loads the room's stored rows as a cold read folds them and requires
// the document they make to be the room's: the same rendering and the same state vector.
func requireStoredRowsFoldToTheRoom(t *testing.T, service *Service, artifactID, when string) {
	t.Helper()
	waitForLandedAppends(t, service, artifactID)
	loaded, err := service.persistence.(*PgVersioned).LoadDocument(context.Background(), artifactID)
	if err != nil {
		t.Fatalf("%s: load the stored rows: %v", when, err)
	}
	live := service.srv.GetDoc(artifactID)
	if live == nil {
		t.Fatalf("%s: the document's room is not open", when)
	}
	if room, stored := liveMarkdown(t, service, artifactID), docMarkdown(loaded.Doc); room != stored {
		t.Fatalf("%s: the room reads %q but its stored rows fold to %q", when, room, stored)
	}
	if room, stored := fmt.Sprint(live.StateVector()), fmt.Sprint(loaded.Doc.StateVector()); room != stored {
		t.Fatalf("%s: the room's state vector is %s but its stored rows fold to %s", when, room, stored)
	}
}
