// Package synctest shares the Hocuspocus websocket sync plumbing that Dispatch tests need across
// packages. It is an ordinary package rather than a _test.go file because api and docs cannot
// import one another's test helpers.
package synctest

import (
	gws "github.com/gorilla/websocket"
	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
	ygsync "github.com/reearth/ygo/sync"
)

// Frame wraps a sync message as Hocuspocus carries it: the document's name, then the sync kind.
func Frame(artifactID string, syncMessage []byte) []byte {
	header := encoding.EncodeBytes(func(encoder *encoding.Encoder) {
		encoder.WriteVarString(artifactID)
		encoder.WriteVarUint(0)
	})
	return append(header, syncMessage...)
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
