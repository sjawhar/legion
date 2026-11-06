package docs

// This file holds the one price the room budget charges a loaded room at - the heap one copy of a
// document is measured to hold for each byte of its encoded state - and the arithmetic that turns
// a room's tracked state bytes into the weight the budget carries (roomWeight).

// bytesOfHeapAStateByte is the heap one copy of a document holds for each byte of its encoded
// state. Measured 2026-11-05 over eleven shapes
// (dispatch://INFRA-179/artifact/infra179-simplify-measured-md): the most 13.66 bytes a state
// byte (a paragraph typed into 100,000 times before its last character and then deleted), the
// least 0.83 (a run of 300,000 keystrokes, edited), a spread of 16.5 between them. It is set past
// the most.
//
// One price replaces the six the budget charged before - an element, a struct held, a rendered
// byte, a byte at the densest, a struct added, a split - because the encoded state is the one
// measure of a document that every shape's heap follows within that spread, and because it is
// the one measure the room can keep without reading the document: a whole-state encode costs
// 18.7 ms and 5,495,143 bytes allocated on a mebibyte of prose, 64 times what a keystroke's apply
// costs, so nothing on the keystroke path may take one.
const bytesOfHeapAStateByte = 14

// roomCopies is the copies of its document a room is charged at, from its load (roomBudget.load)
// to its retirement (retire): its live document and the rendered replica its update observer
// keeps (renderedReplica), which the room holds from its load on, the copy its load decoded of
// the stored state (keepReplica). Every byte of state the room takes, the replica takes too: the
// replica holds the live document's whole state and then takes every update the room commits
// (renderedReplica.observe).
const roomCopies = 2

// roomWeight is what the budget charges a room whose tracked state is stateBytes long: its state
// at bytesOfHeapAStateByte in each of its two copies, 28 bytes a state byte. A mebibyte of prose,
// whose state is 3,035,240 bytes, is charged 84,986,720.
//
// The state bytes are tracked, never re-read (renderedReplica.chargeAdded): the bytes of the
// stored state the room's load decoded, plus the bytes of each update the room's own update
// observer drains, re-based at each load and at each compaction that folds the room's whole
// stored history into one row (Service.rebaseRoomState). Between re-bases the tracked figure runs
// above the state itself, since an update's bytes carry what its transaction deleted beside what
// it added and ygo merges a run of typed characters into one item as it commits, which is the
// safe side for a budget: a room is charged what it may hold, and the next load charges it what
// it does hold.
func roomWeight(stateBytes int64) int64 {
	return stateBytes * bytesOfHeapAStateByte * roomCopies
}
