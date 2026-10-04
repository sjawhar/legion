// Package docstest speaks the Hocuspocus websocket sync that docs.Service.ServeHTTP serves, and
// writes the deep trees a crafted client sends, for Dispatch tests in more than one package. It is
// an ordinary package rather than a _test.go file because api, docs and pmdoc cannot import one
// another's test helpers, and it imports none of them.
package docstest

import (
	"sync"

	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// WriteFrame writes syncMessage to connection as Hocuspocus carries it - the document's name, then
// the sync kind - holding writes while it writes: gorilla/websocket panics on two writes at once,
// and a peer's Drain answers the room's sync step 1 while its test sends.
func WriteFrame(writes *sync.Mutex, connection *gws.Conn, artifactID string, syncMessage []byte) error {
	frame := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(0)
	})
	writes.Lock()
	defer writes.Unlock()
	return connection.WriteMessage(gws.BinaryMessage, append(frame, syncMessage...))
}

// Drain applies every sync frame the room sends and answers its sync step 1 until the connection
// closes. send serializes writes when the caller has another writer, and onSyncStep2 receives the
// content of each sync step 2 after it is applied.
func Drain(connection *gws.Conn, document *crdt.Doc, send func([]byte) error, onSyncStep2 func([]byte)) {
	for {
		_, message, err := connection.ReadMessage()
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
		reply, err := ygsync.ApplySyncMessage(document, payload, nil)
		if err != nil {
			return
		}
		if kind == ygsync.MsgSyncStep1 && reply != nil {
			if err := send(reply); err != nil {
				return
			}
		}
		if kind == ygsync.MsgSyncStep2 && onSyncStep2 != nil {
			onSyncStep2(content)
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
