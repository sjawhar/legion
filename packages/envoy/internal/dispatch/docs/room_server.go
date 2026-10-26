package docs

import (
	"context"
	"errors"
	"sync"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
)

// roomServer is the service's ygo server. Its CloseRoom shadows ygo's, so every close of a room
// the service makes waits for each repair committing into the room (holdOpen) to hand its update
// to the room's persistence, and holds off new repairs until the room is closed. Its Apply shadows
// ygo's, so the room each Apply touched is looked at once the Apply has returned (applied).
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
	// applied is handed the room of each Apply once it has returned: the service's close of a room
	// its own work opened over the room budget (Service.closeIfOpenedOverBudget). Nil looks at
	// nothing.
	applied func(room string)
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

// Apply runs fn in room through ygo's Apply, then hands room to applied: a room the Apply loaded
// is published once the Apply's load has returned, and some openers - a snapshot read, a live
// tree's read, a warm-up - hold nothing of the room's state while they use it, so the end of their
// Apply is the moment their use of it ends.
func (r *roomServer) Apply(ctx context.Context, room string, fn func(doc *crdt.Doc, transact func(func(*crdt.Transaction)))) error {
	err := r.Server.Apply(ctx, room, fn)
	if r.applied != nil {
		r.applied(room)
	}
	return err
}

// closeInstance closes room through ygo's CloseRoom only while room holds doc, answering whether it
// closed it. It holds the close gate as CloseRoom does, so no other close the service makes, and no
// repair, comes between its check of the instance and ygo's lookup of the room by name. Its caller
// keeps ygo's idle sweep, the one retirement that takes no gate, off the room for the same span (a
// Server.Apply on it, closeRoomInstance), so the room the check found is the room ygo closes.
func (r *roomServer) closeInstance(room string, doc *crdt.Doc) (bool, error) {
	gate := r.acquireGate(room)
	defer r.releaseGate(room, gate)
	gate.Lock()
	defer gate.Unlock()
	if r.GetDoc(room) != doc {
		return false, nil
	}
	if err := r.Server.CloseRoom(room, true); err != nil {
		if errors.Is(err, websocket.ErrRoomNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// closeOwned is CloseRoom for a room its caller owns: holding the gate exclusively, it asks owns
// whether the room under that name is still the caller's, and only then closes the room and hands
// the close's error to after, whose error it returns. CloseRoom closes by name, so two closes
// queued on one gate for one room, the second of which runs once the first has let go, would
// close whatever replaced the room in between, a browser's reload among them. owns is what tells
// the second close that what is there now is not its own. A failed room's eviction is its one
// caller (evictFailed).
//
// The check, the close and after all run holding the gate, so no other close the service makes
// falls between them, and a close that checks after this one sees what after recorded (the
// closed room's state handed over, forgetEvicted). owns and after take the room's state lock
// under the gate, the order CloseRoom's own flush takes them in.
func (r *roomServer) closeOwned(room string, owns func() bool, after func(error) error) error {
	gate := r.acquireGate(room)
	defer r.releaseGate(room, gate)
	gate.Lock()
	defer gate.Unlock()
	if !owns() {
		return nil
	}
	return after(r.Server.CloseRoom(room, true))
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
