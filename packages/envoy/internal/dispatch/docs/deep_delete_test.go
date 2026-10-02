package docs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	ygsync "github.com/reearth/ygo/sync"

	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
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

		peer := newDeepPeer(t, httpServer.URL, artifactID)
		fragment := peer.doc.GetXmlFragment(fragmentName)
		root := crdt.NewYXmlElement("blockquote")
		opening := peer.send(t, func(txn *crdt.Transaction) { fragment.InsertElement(txn, 0, root) })
		deepest := root
		for grown := 1; grown < levels; {
			batch := min(levelsPerUp, levels-grown)
			peer.send(t, func(txn *crdt.Transaction) {
				for range batch {
					child := crdt.NewYXmlElement("blockquote")
					deepest.InsertElement(txn, 0, child)
					deepest = child
				}
			})
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
		peer.write(t, ygsync.EncodeUpdate(deletion))

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

// deepPeer is a writable document connection that sends the updates its own transactions make and
// drains everything the room sends, so the room never blocks writing to it. Its reader answers the
// room's sync step 1 while the test sends updates, and gorilla/websocket panics on two writes at
// once, so every write goes through writes.
type deepPeer struct {
	doc        *crdt.Doc
	connection *gws.Conn
	artifactID string
	writes     sync.Mutex
}

func newDeepPeer(t *testing.T, serverURL, artifactID string) *deepPeer {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http") + "/ws/doc/" + artifactID +
		"?schema_version=" + strconv.Itoa(pmdoc.SchemaVersion())
	connection, response, err := gws.DefaultDialer.Dial(wsURL, http.Header{"X-Dispatch-User": []string{"alice"}})
	if err != nil {
		t.Fatalf("connect document peer: response=%#v err=%v", response, err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	peer := &deepPeer{doc: crdt.New(), connection: connection, artifactID: artifactID}
	go peer.drain()
	return peer
}

// drain reads the room's messages, applying each sync message and answering its sync step 1, as a
// browser's provider does, until the connection closes.
func (p *deepPeer) drain() {
	docstest.Drain(p.connection, p.doc, p.sendFrame, nil)
}

// send runs change in one transaction and sends the update it produced, as a keystroke does.
func (p *deepPeer) send(t *testing.T, change func(*crdt.Transaction)) []byte {
	t.Helper()
	update := docstest.Transact(p.doc, change)
	if update == nil {
		t.Fatal("a peer transaction produced no update")
	}
	p.write(t, ygsync.EncodeUpdate(update))
	return update
}

func (p *deepPeer) write(t *testing.T, syncMessage []byte) {
	t.Helper()
	if err := p.sendFrame(syncMessage); err != nil {
		t.Fatalf("send peer update: %v", err)
	}
}

// sendFrame writes one framed sync message, one writer at a time.
func (p *deepPeer) sendFrame(syncMessage []byte) error {
	p.writes.Lock()
	defer p.writes.Unlock()
	return p.connection.WriteMessage(gws.BinaryMessage, docstest.Frame(p.artifactID, syncMessage))
}
