package docs

// This file holds one loaded room's charge against the budget (roomCharge): the one weight it
// carries, and the two writes that move it - the drain that adds each committed update's bytes
// to the room's tracked state, and the re-base that replaces the tracked state with a figure
// read from bytes already in hand.

// roomCharge is one loaded room's charge against the budget: one weight, its two copies of the
// room's tracked state at bytesOfHeapAStateByte each (roomWeight), and the room it belongs to.
//
// Its only writer is the room's update observer, whose drain of the room's update log adds each
// committed update's bytes to the room's tracked state and weighs the room again from the sum
// (renderedReplica.chargeAdded), whoever wrote the update: a browser's update the peer hook
// admitted, an API write's publish, an injected update, settlement's repair. The peer hook
// charges nothing of its own: an update it admits is drained by that observer inside the apply
// that committed it (crdt/doc.go:984-992), and one its apply refused committed nothing to drain.
//
// The charge lasts as long as the room's instance. Its load makes it, from the stored state the
// load decoded (chargeLoadedRoom); each drain raises it; a compaction that folds the room's
// whole stored history into one row re-bases it on what that fold wrote
// (Service.rebaseRoomState); and the room's retirement drops it whole (roomBudget.retire).
type roomCharge struct {
	budget *roomBudget
	room   string
	// weight is what the budget charges the room now, both copies together (roomWeight), and
	// retired says the room's instance has gone, after which nothing is charged again.
	weight  int64
	retired bool
	// closeOnceUnused is set on a room loaded while the budget was over its limit, which the
	// server's own work opened over it; it is closed once unused
	// (Service.closeIfOpenedOverBudget) unless a browser joins it first (join).
	closeOnceUnused bool
}

// weigh charges the room what its tracked state of stateBytes bytes comes to (roomWeight) and
// re-weighs its entry under the budget's lock. A nil charge, a replica no room's load charged (a
// test's), charges nothing.
func (c *roomCharge) weigh(stateBytes int64) {
	if c == nil {
		return
	}
	c.budget.mu.Lock()
	defer c.budget.unlock()
	c.budget.reweighLocked(c, roomWeight(stateBytes))
}
