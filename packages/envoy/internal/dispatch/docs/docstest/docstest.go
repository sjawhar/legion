// Package docstest speaks the Hocuspocus websocket sync that docs.Service.ServeHTTP serves, and
// writes the deep trees a crafted client sends, for Dispatch tests in more than one package. It is
// an ordinary package rather than a _test.go file because api, docs and pmdoc cannot import one
// another's test helpers, and it imports none of them.
package docstest

import (
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// Peer is a writable connection to a document's room that behaves as a browser's provider does: it
// applies every sync message the room sends to Doc, answers the room's sync step 1, and sends the
// updates its own transactions make.
type Peer struct {
	// Doc is the peer's copy of the document. The peer's reader applies the room's updates to it.
	Doc *crdt.Doc
	// Answers carries the content of each sync step 2 the room sends, once Doc holds it. One that
	// arrives while sixteen are waiting to be read is dropped.
	Answers <-chan []byte
	// Ended is closed once the connection has ended and the reader has stopped.
	Ended <-chan struct{}

	artifactID string
	connection *gws.Conn
	// writes holds one write at a time: gorilla/websocket panics on two writes at once, and the
	// reader answers the room's sync step 1 while a test sends.
	writes  sync.Mutex
	endedAt time.Time
}

// Dial connects a peer to artifactID's room at url, the document websocket's full address with its
// schema_version, with header naming the caller, and starts its reader. doc is the peer's copy of
// the document: a new one, or the copy a reconnecting browser kept. The connection closes when t
// ends.
func Dial(t testing.TB, url string, header http.Header, artifactID string, doc *crdt.Doc) *Peer {
	t.Helper()
	connection, response, err := gws.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("connect a peer to document %s: response=%#v err=%v", artifactID, response, err)
	}
	answers, ended := make(chan []byte, 16), make(chan struct{})
	peer := &Peer{Doc: doc, Answers: answers, Ended: ended, artifactID: artifactID, connection: connection}
	t.Cleanup(peer.Close)
	go func() {
		peer.read(answers)
		peer.endedAt = time.Now()
		close(ended)
	}()
	return peer
}

// Write sends syncMessage to the room as Hocuspocus carries it: the document's name, then the sync
// kind.
func (p *Peer) Write(syncMessage []byte) error {
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(p.artifactID)
		encoder.WriteVarUint(0)
	})
	p.writes.Lock()
	defer p.writes.Unlock()
	return p.connection.WriteMessage(gws.BinaryMessage, append(frame, syncMessage...))
}

// Send runs change in one transaction on Doc and sends the room the update it made, as a keystroke
// does, returning that update.
func (p *Peer) Send(change func(*crdt.Transaction)) ([]byte, error) {
	update := Transact(p.Doc, change)
	if update == nil {
		return nil, errors.New("the peer's transaction made no update")
	}
	return update, p.Write(ygsync.EncodeUpdate(update))
}

// AskForDocument asks the room for its whole document, a sync step 1 naming no state. The room
// answers in order on this connection, and the answer arrives on Answers once Doc holds it.
func (p *Peer) AskForDocument() error {
	return p.Write(ygsync.EncodeSyncStep1(crdt.New()))
}

// Close closes the connection and waits for the reader to stop.
func (p *Peer) Close() {
	_ = p.connection.Close()
	<-p.Ended
}

// EndedAt is when the connection ended. It is read once Ended is closed.
func (p *Peer) EndedAt() time.Time {
	<-p.Ended
	return p.endedAt
}

// read applies every sync frame the room sends and answers its sync step 1 until the connection
// closes, handing answers the content of each sync step 2 once it is applied.
func (p *Peer) read(answers chan<- []byte) {
	for {
		_, message, err := p.connection.ReadMessage()
		if err != nil {
			return
		}
		decoder := encoding.NewDecoder(message)
		if _, err := decoder.ReadVarString(); err != nil {
			return
		}
		if kind, err := decoder.ReadVarUint(); err != nil || kind != 0 {
			continue
		}
		payload := decoder.RemainingBytes()
		kind, content, err := ygsync.ReadSyncMessage(payload)
		if err != nil {
			return
		}
		reply, err := ygsync.ApplySyncMessage(p.Doc, payload, nil)
		if err != nil {
			return
		}
		if kind == ygsync.MsgSyncStep1 && reply != nil {
			if err := p.Write(reply); err != nil {
				return
			}
		}
		if kind == ygsync.MsgSyncStep2 {
			select {
			case answers <- content:
			default:
			}
		}
	}
}

// Transact runs change in one transaction on document and returns the update it produced, as a
// browser's provider sends a keystroke's, or nil when it produced none.
func Transact(document *crdt.Doc, change func(*crdt.Transaction)) []byte {
	origin := &transactOrigin{}
	var update []byte
	unsubscribe := document.OnUpdate(func(encoded []byte, updateOrigin any) {
		if updateOrigin == origin {
			update = append([]byte(nil), encoded...)
		}
	})
	document.Transact(change, origin)
	unsubscribe()
	return update
}

// transactOrigin tags Transact's own transaction. Transact tells its update from others by
// comparing origins with ==, so it is not zero sized: two pointers to zero-sized values may be equal.
type transactOrigin struct{ _ byte }

// WriteDeepChain writes, as the fragment's first child, blockquotes nested one inside another
// around a paragraph whose text, text, stands textLevel levels below the document: the document is
// level 0, the blockquotes levels 1 through textLevel-2, and the paragraph level textLevel-1.
// textLevel is the unit pmdoc's tree-depth bound counts. It writes the elements itself, as a
// crafted client can, rather than through pmdoc.Update, which validates the tree first.
func WriteDeepChain(txn *crdt.Transaction, fragment *crdt.YXmlFragment, textLevel int, text string) {
	parent := crdt.NewYXmlElement("blockquote")
	fragment.InsertElement(txn, 0, parent)
	for range textLevel - 3 {
		child := crdt.NewYXmlElement("blockquote")
		parent.InsertElement(txn, 0, child)
		parent = child
	}
	paragraph := crdt.NewYXmlElement("paragraph")
	parent.InsertElement(txn, 0, paragraph)
	run := crdt.NewYXmlText()
	paragraph.InsertText(txn, 0, run)
	run.Insert(txn, 0, text, nil)
}
