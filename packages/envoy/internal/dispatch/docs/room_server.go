package docs

import (
	"sync"

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
	// gates holds, per room, the gate between a repair's commit into the room (holdOpen) and a
	// close of it (CloseRoom).
	gates sync.Map
}

// CloseRoom closes room through ygo's CloseRoom once no repair is committing into it, holding
// off every repair while it closes.
func (r *roomServer) CloseRoom(room string, force bool) error {
	gate := r.gate(room)
	gate.Lock()
	defer gate.Unlock()
	return r.Server.CloseRoom(room, force)
}

// holdOpen holds off every close of room while a repair commits into it, until releaseOpen. It
// refuses, holding nothing, while a close is under way or waiting: a repair never waits for one.
func (r *roomServer) holdOpen(room string) (*sync.RWMutex, bool) {
	gate := r.gate(room)
	if !gate.TryRLock() {
		return nil, false
	}
	return gate, true
}

func (r *roomServer) releaseOpen(gate *sync.RWMutex) {
	gate.RUnlock()
}

func (r *roomServer) gate(room string) *sync.RWMutex {
	if gate, ok := r.gates.Load(room); ok {
		return gate.(*sync.RWMutex)
	}
	gate, _ := r.gates.LoadOrStore(room, &sync.RWMutex{})
	return gate.(*sync.RWMutex)
}
