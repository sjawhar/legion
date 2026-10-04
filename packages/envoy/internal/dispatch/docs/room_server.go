package docs

import (
	"sync"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
)

// roomServer is the service's ygo server. Its CloseRoom shadows ygo's, so every close of a room
// the service makes waits for each repair committing into the room (holdOpen) to hand its update
// to the room's persistence, and holds off new repairs until the room is closed.
//
// A settlement writes its repairs while it holds the document's advisory lock, and ygo hands a
// commit's update to the room's persistence inside the commit. Were the room's persistence worker
// retired under the commit, the update would go to ygo's stranded persistence on the repair's own
// goroutine, which waits for the worker to exit and then queues behind every other stranded write
// into the same document, and neither wait could end (LEGION-498): a failed room's worker compacts
// under the document's lock before it exits, while the settlement holding the lock waits for that
// exit; a second writer's stranded write, waiting for the repair's suppression slot or for the
// lock to append, holds the repair's write behind it. With the close waiting for the commit, the
// worker takes the update while the room is still the server's, and a failed room's eviction
// compacts under the lock once the settlement that holds it returns. A repair never waits for a
// close, so no cycle runs through the gate.
type roomServer struct {
	*websocket.Server
	// gatesMu guards gates and every gate's holders.
	gatesMu sync.Mutex
	// gates holds a room's closeGate only while a close or a repair holds it or waits for it: the
	// last of them to let go removes it (releaseGate), so no gate outlives the work on its room.
	gates map[string]*closeGate
}

// closeGate is a room's gate between the repairs committing into the room, which hold it shared,
// and a close of the room, which holds it exclusively. holders counts the closes and repairs that
// hold it or wait for it; each finds the gate already in gates when there is one, so any two of
// them that overlap share it.
type closeGate struct {
	sync.RWMutex
	holders int
}

func newRoomServer(server *websocket.Server) *roomServer {
	return &roomServer{Server: server, gates: make(map[string]*closeGate)}
}

// CloseRoom closes room through ygo's CloseRoom once no repair is committing into it, holding
// off every repair while it closes.
func (r *roomServer) CloseRoom(room string, force bool) error {
	gate := r.acquireGate(room)
	defer r.releaseGate(room, gate)
	gate.Lock()
	defer gate.Unlock()
	return r.Server.CloseRoom(room, force)
}

// holdOpen holds doc's room open while a repair commits into it, until release. It refuses,
// holding nothing, while a close is under way or waiting, or when room has retired or replaced doc:
// a repair never waits for a close.
func (r *roomServer) holdOpen(room string, doc *crdt.Doc) (release func(), open bool) {
	gate := r.acquireGate(room)
	if !gate.TryRLock() {
		r.releaseGate(room, gate)
		return nil, false
	}
	release = func() {
		gate.RUnlock()
		r.releaseGate(room, gate)
	}
	if r.GetDoc(room) != doc {
		release()
		return nil, false
	}
	return release, true
}

func (r *roomServer) acquireGate(room string) *closeGate {
	r.gatesMu.Lock()
	defer r.gatesMu.Unlock()
	gate := r.gates[room]
	if gate == nil {
		gate = &closeGate{}
		r.gates[room] = gate
	}
	gate.holders++
	return gate
}

func (r *roomServer) releaseGate(room string, gate *closeGate) {
	r.gatesMu.Lock()
	defer r.gatesMu.Unlock()
	gate.holders--
	if gate.holders == 0 {
		delete(r.gates, room)
	}
}
