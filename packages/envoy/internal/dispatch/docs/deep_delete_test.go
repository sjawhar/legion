package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/stacktest"
)

// An authenticated peer grows a nested element chain through the document websocket, in updates
// small enough that none is remarkable on its own, and then sends one ordinary delete of the
// chain's root. Deleting an element deletes everything inside it, and ygo walked those children
// with one stack frame per level (crdt.(*Item).delete), so the stack that delete needed grew with
// the nesting the peer chose; past the goroutine's stack limit that is a fatal error no recover
// sees, in the process serving every other room. The pinned fork walks them iteratively.
//
// The cap below is far under Go's one-gigabyte default, so the chain stays cheap to build: a frame
// of the recursive delete is 208 bytes, so at this depth it needs about 40 MiB, two and a half
// times the cap.
func TestDeletingADeeplyNestedLiveTreeNeedsNoStackPerLevel(t *testing.T) {
	stacktest.Under(t, 16<<20, func(t *testing.T) {
		const (
			levels      = 200_000
			levelsPerUp = 5_000
		)
		service, artifactID := newTestService(t)
		service.settle = time.Hour
		seedServiceText(t, service, artifactID, "before")
		httpServer := httptest.NewServer(http.HandlerFunc(service.ServeHTTP))
		t.Cleanup(httpServer.Close)

		peer := connectPeer(t, httpServer.URL, artifactID)
		fragment := peer.Doc.GetXmlFragment(fragmentName)
		root := crdt.NewYXmlElement("blockquote")
		opening, err := peer.Send(func(txn *crdt.Transaction) { fragment.InsertElement(txn, 0, root) })
		if err != nil {
			t.Fatalf("send the chain's root: %v", err)
		}
		deepest := root
		for grown := 1; grown < levels; {
			batch := min(levelsPerUp, levels-grown)
			if _, err := peer.Send(func(txn *crdt.Transaction) {
				for range batch {
					child := crdt.NewYXmlElement("blockquote")
					deepest.InsertElement(txn, 0, child)
					deepest = child
				}
			}); err != nil {
				t.Fatalf("grow the chain: %v", err)
			}
			grown += batch
		}
		waitFor(t, 30*time.Second, "the room to hold the nested chain", func() bool {
			_, err := service.Text(context.Background(), artifactID)
			return errors.Is(err, ErrDocSchema)
		})

		// The delete is written on a document holding the chain's root alone, so the peer sends
		// the same delete an ordinary client would and this test process does not walk the chain
		// itself.
		deleter := crdt.New()
		if err := crdt.ApplyUpdateV1(deleter, opening, nil); err != nil {
			t.Fatalf("open the deleting document: %v", err)
		}
		deleterFragment := deleter.GetXmlFragment(fragmentName)
		deletion := docstest.Transact(deleter, func(txn *crdt.Transaction) {
			deleterFragment.Delete(txn, 0, 1)
		})
		if deletion == nil {
			t.Fatal("deleting the chain's root produced no update")
		}
		if err := peer.Write(ygsync.EncodeUpdate(deletion)); err != nil {
			t.Fatalf("send the chain's delete: %v", err)
		}

		waitFor(t, 30*time.Second, "the room to read back as the document it was", func() bool {
			text, err := service.Text(context.Background(), artifactID)
			return err == nil && strings.TrimSpace(text) == "before"
		})
		state := service.room(artifactID)
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.failed != nil {
			t.Fatalf("the delete failed the live room: %v", state.failed)
		}
	})
}
