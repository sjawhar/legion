package docs

import (
	"context"
	"errors"
	"sync"
	"time"

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
//
// ygo closes no room a caller is inside - a Server.Apply, a relay Inject, or a browser's join
// before its peer registers - and answers websocket.ErrRoomInUse instead (sjawhar/ygo
// v1.55.1-sami.2, reearth/ygo#342). So a close from outside the room waits for those callers to
// leave (closeWhenFree), and the one close the service makes from inside its own Apply on the
// room (closeInstance) is ygo's CloseRoomWithin.
type roomServer struct {
	*websocket.Server
	// beforeClose runs ahead of every close of a room the service makes, holding the room's close
	// gate, once the close knows the room is the one it closes (Service.removeLocalAwareness). It
	// must not block. Nil runs nothing.
	beforeClose func(room string)
	// afterClose runs after every close of a room the service makes that closed it, still holding
	// the room's close gate (the test hooks' close count). It must not block. Nil runs nothing.
	afterClose func(room string)
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
// off every repair while it closes, and once no caller is inside it (closeWhenFree). A caller
// inside its own Apply on room must not call it: it would wait for itself.
func (r *roomServer) CloseRoom(room string, force bool) error {
	return r.closeRoom(context.Background(), room, force)
}

// closeRoom checks ctx before attempting a close and while retrying a room still in use. Once
// ygo's CloseRoom starts, ctx cannot interrupt it: loading, peer teardown and persistence drain
// can keep it blocked, and the room can close after ctx ends.
func (r *roomServer) closeRoom(ctx context.Context, room string, force bool) error {
	return r.closeWhenFree(ctx, room, func() error {
		return r.closeHeld(room, func() error { return r.Server.CloseRoom(room, force) })
	})
}

// roomInUseFirstWait and roomInUseLongestWait bound how long a close waits before it tries again
// a room a caller is inside (closeWhenFree): the wait doubles from the first to the longest. A
// caller inside a room is a write's publish, a read that loads the room, a settlement's repair or
// a browser's join, each of which leaves within milliseconds.
const (
	roomInUseFirstWait   = time.Millisecond
	roomInUseLongestWait = 50 * time.Millisecond
)

// closeWhenFree runs try, a close of room, holding room's gate exclusively, until try answers
// anything but websocket.ErrRoomInUse or ctx ends between attempts. It checks ctx after taking
// the gate, but cannot cancel the gate lock or try itself. It lets go of the gate between tries,
// so the callers it waits for are not held behind it: a close of the service's own made from
// inside an Apply on the room (closeInstance) waits for that gate, and a repair refuses to start
// while a close holds it.
func (r *roomServer) closeWhenFree(ctx context.Context, room string, try func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	gate := r.acquireGate(room)
	defer r.releaseGate(room, gate)
	wait := roomInUseFirstWait
	for {
		gate.Lock()
		if err := ctx.Err(); err != nil {
			gate.Unlock()
			return err
		}
		err := try()
		gate.Unlock()
		if !errors.Is(err, websocket.ErrRoomInUse) {
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		wait = min(2*wait, roomInUseLongestWait)
	}
}

// closeHeld runs attempt, one try at closing room by a caller holding room's gate, after
// beforeClose and, once it has closed the room, before afterClose. An attempt ygo refuses
// because a caller is inside the room (websocket.ErrRoomInUse) closed nothing, so it runs no
// afterClose and is tried again (closeWhenFree), beforeClose with it: the one hook there is
// stands being run again, since it takes the room's awareness states out and finds none the
// second time (Service.removeLocalAwareness).
func (r *roomServer) closeHeld(room string, attempt func() error) error {
	if r.beforeClose != nil {
		r.beforeClose(room)
	}
	err := attempt()
	if err == nil && r.afterClose != nil {
		r.afterClose(room)
	}
	return err
}

// closeInstance closes room through ygo's CloseRoomWithin only while room holds doc, answering
// whether it closed it. Its caller runs it inside its own Server.Apply on room (closeRoomInstance),
// which ygo's CloseRoom would refuse as a caller inside the room, so it is that Apply's close; it
// is refused, with websocket.ErrRoomInUse, while any other caller is inside the room. It holds the
// close gate as CloseRoom does, so no other close the service makes, and no repair, comes between
// its check of the instance and ygo's lookup of the room by name. That Apply keeps ygo's idle sweep,
// the one retirement that takes no gate, off the room for the same span, so the room the check
// found is the room ygo closes.
func (r *roomServer) closeInstance(room string, doc *crdt.Doc) (bool, error) {
	gate := r.acquireGate(room)
	defer r.releaseGate(room, gate)
	gate.Lock()
	defer gate.Unlock()
	if r.GetDoc(room) != doc {
		return false, nil
	}
	if err := r.closeHeld(room, func() error { return r.Server.CloseRoomWithin(room, doc, true) }); err != nil {
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
// under the gate, the order CloseRoom's own flush takes them in. A close ygo refuses because a
// caller is inside the room is tried again once that caller has left (closeWhenFree), owns asked
// again each time, with no bound: a failed room admits no new caller (awaitRoomRecovery), so the
// callers inside it leave, and every reader of the room waits for this eviction.
func (r *roomServer) closeOwned(room string, owns func() bool, after func(error) error) error {
	return r.closeWhenFree(context.Background(), room, func() error {
		if !owns() {
			return nil
		}
		err := r.closeHeld(room, func() error { return r.Server.CloseRoom(room, true) })
		if errors.Is(err, websocket.ErrRoomInUse) {
			return err // tried again once the caller inside has left
		}
		return after(err) // never handed ErrRoomInUse, so it ends the tries
	})
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
