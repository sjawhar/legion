package docs

import (
	"slices"
)

// The updates a room instance holds and the store does not (roomInstance.unstored): an update is
// listed from the moment the room's update observer classified it until its append has returned,
// and a room query of another process reads them, merged, as what this room holds and the store
// may not (unstoredShare, awaitStored). Storing them is not this list's business: every durable
// writer of the room stores the room's not-yet-stored updates through the point it read the room
// at, from the room's own queue, before an append of its own (flushThrough).

// listUnstored lists class, an update only this instance holds, as unstored (roomInstance.unstored).
func (instance *roomInstance) listUnstored(class *queuedClass) {
	instance.unstoredMu.Lock()
	defer instance.unstoredMu.Unlock()
	instance.unstored = append(instance.unstored, class)
}

// unlistUnstored takes class off the instance's unstored updates once its append has returned,
// whichever writer stored it. A nil instance is a class nothing listed.
func (instance *roomInstance) unlistUnstored(class *queuedClass) {
	if instance == nil {
		return
	}
	instance.unstoredMu.Lock()
	defer instance.unstoredMu.Unlock()
	if index := slices.Index(instance.unstored, class); index >= 0 {
		instance.unstored = slices.Delete(instance.unstored, index, index+1)
	}
}

// unstoredUpdates is the instance's unstored updates as of now, in the order the room committed
// them.
func (instance *roomInstance) unstoredUpdates() []*queuedClass {
	instance.unstoredMu.Lock()
	defer instance.unstoredMu.Unlock()
	return slices.Clone(instance.unstored)
}
