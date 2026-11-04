package docs

import (
	"errors"
)

// This file holds the estimate of what ygo's apply of a browser's update allocates
// (peerWeighing.applyHeap), the factors it is priced from and the two bounds it is weighed
// against: unweighedApplyHeapBytes, under which an update never waits on the apply-heap bound,
// and MemoryBounds.ApplyTooHeavy, past which it is refused (errApplyTooHeavy).

// errApplyTooHeavy is why the hook refuses an update whose apply alone is estimated to allocate
// more than the service's MemoryBounds.ApplyTooHeavy: no turn could hold it, even alone. It is
// never applied, and the hook answers it with the document's own close,
// DocumentTooLargeCloseCode and DocumentTooLargeCloseReason, this error behind it (refusedWith),
// as it answers a frame the walk refuses.
var errApplyTooHeavy = errors.New("the update is heavier to apply than the pod can hold")

// unweighedApplyHeapBytes is the update's own share of its estimate (peerWeighing.ownApplyHeap)
// at or under which an admitted update applies without waiting on the apply-heap bound
// (MemoryBounds.ApplyHeap): 2 MiB, over
// three times the own share of the editor's 64 KiB paste (the creep cap's size, far past a
// keystroke) into the heaviest document and over four hundred times a keystroke's, so typing never
// waits on a reconnect storm's handoffs (TestEveryApplysTransientHeapIsWithinItsEstimate). The
// room's share is left out of the comparison because it is the room's, not the update's: every
// transaction on a room pays for the room's clients and deleted items, the server's own writes
// included, and ygo runs one transaction at a time in a room (its document lock), so that share is
// paid at most once a room at any moment. An unweighed apply runs beside the bound, one per
// browser's read loop at most, in the slack neither bound grants (MemoryBounds.HeapSlack).
const unweighedApplyHeapBytes = 2 << 20

// applyHeapName is what the apply-heap bound's grants are logged as (logHeapGrant, weighedHeaps).
// The bound itself is the service's (Service.applyHeaps), sized from MemoryBounds.ApplyHeap, and
// a waiter on it has no timeout - it is a browser's read loop waiting its turn, and the browser's
// next frame waits with it - and holds nothing while it waits, neither part of the bound nor its
// room's admission lock.
const applyHeapName = "apply"

// applyHeap is the estimate of what ygo's apply of the walked update allocates, frame its bytes,
// into a room of roomStructs structs (roomTotals.roomStructs): an upper bound, every factor below
// set from TestEveryApplysTransientHeapIsWithinItsEstimate's measurement of ygo's apply at scale
// (runtime.MemStats.TotalAlloc across one apply, which bounds what the apply holds at its peak
// from above), into a room's bounded document (newRoomScans) with an update observer, as every
// room is. It is the update's own share (ownApplyHeap) and the room's (roomApplyHeap):
//
//   - the room's state vector, which ygo's apply copies into maps before it reads a struct
//     (sjawhar/ygo v1.55.1-sami.1, crdt/update.go:736) and its transaction twice more
//     (crdt/doc.go:937, :1014), each a slot for every client the room holds;
//   - the update the transaction made, which ygo encodes for the room's update observers at
//     every commit that changed the document (collectAndEncodeLocked, crdt/doc.go:844-873,
//     called at :902), with that transaction's own deletions (reearth/ygo#350), and, for a whole
//     state, by walking the room's store and building the delete set of all of it, a range for
//     every deleted item before it compacts them (encodeV1Locked and buildDeleteSet,
//     crdt/update.go:365-405 and :659-678). The term for the room's deleted items is kept as
//     headroom: at sjawhar/ygo v1.54.1-sami.3, where every transaction's update carried the
//     room's whole delete set, 875,008 bytes were measured for a one-character keystroke into a
//     string of 10,000 deleted items among 10,000 live ones, against 2,328 into one of 20,000
//     live items and none deleted;
//   - the marks its scans make, one for each distinct item they pass (crdt/item.go:138-180), at
//     bytesOfAScanMark each: the items they can pass are the room's and those the apply makes,
//     no more than the room's structs and the structs the apply can hold, and never more than
//     the per-frame ceiling, the most any allowance gives an apply (roomScans). An update that
//     integrates no struct scans nothing, its drain included, since no clock of its moves.
//
// The room's parked queue costs an apply nothing in proportion to it unless the apply reaches it:
// a bounded document's apply keeps the parked structs and parked delete ranges by reference rather
// than copying them (beginRemoteApply, crdt/apply_rollback.go:150-176), and a client's parked
// delete ranges are retried only once an apply moves that client's clock past the first of them,
// and only for a client the apply gave a struct (retryPendingDs, crdt/delete_set.go:197-212, and
// StructStore.takeReadyDeletes, crdt/store.go:163-180). Measured on 2026-10-27, a keystroke
// allocates 4,240 bytes into a room holding the most parked structs its struct total admits,
// 349,517, and 4,240 into one holding the most parked delete ranges, 524,280. A retry an apply
// does start re-parks what it cannot apply yet, a copy of that client's parked ranges: 44,739,304
// bytes for the first character of the client whose 524,280 ranges the room holds parked, 85.3 a
// range.
// Each parked range is splitsAStructCanCause structs of the room's struct total, which the room's
// share prices at applyHeapPerRoomStruct each, 512 bytes a range, so no term of its own prices it.
//
// What it leaves out is what depends on the shape of the room's lists, which no walk of the update
// can see: an item or a delete range splitting a room's Any or JSON item copies the values right of
// the split (ContentAny.Splice, crdt/content.go:170-174).
func (w *peerWeighing) applyHeap(frame int, roomStructs uint64) uint64 {
	return w.ownApplyHeap(frame) + w.roomApplyHeap(roomStructs)
}

// roomApplyHeap is the room's share of applyHeap: the three terms above that the room the update
// reaches decides - its clients, the structs reachable in it, and the marks the update's scans
// can make. The hook weighs the update's own share on its own (unweighedApplyHeapBytes) and adds
// this to it, rather than estimating the apply twice (weighPeerUpdate).
func (w *peerWeighing) roomApplyHeap(roomStructs uint64) uint64 {
	reachable := roomStructs + w.applyStructs()
	var marks uint64
	if w.structs > 0 {
		marks = bytesOfAScanMark * min(reachable, perFrameScanCeiling)
	}
	return applyHeapPerRoomClient*uint64(len(w.held)) + applyHeapPerRoomStruct*reachable + marks
}

// applyStructs is the most structs the walked update's apply can hold in the room's store before
// its transaction ends: each new struct with the splits its integration can cause, and the two
// splits of every delete range, one the room holds deleted already included, since ygo makes those
// splits as it applies the range and merges them back only as the transaction ends
// (deletedRuns.splitEnds).
func (w *peerWeighing) applyStructs() uint64 {
	return w.structs*(1+splitsAStructCanCause) + w.deletes*splitsAStructCanCause
}

// ownApplyHeap is the update's own share of applyHeap, what it costs whatever room it reaches:
// the apply itself, each byte of the frame, which bounds every string ygo copies out of it (root
// names, map keys, text, binary, type names, format keys, Any strings and JSON text), each delete
// range, each struct ygo decodes, and each struct it integrates with each byte of the frame
// again, which bounds what the observers' encode writes of them. It is the share that decides
// whether the update waits on the bound (unweighedApplyHeapBytes).
func (w *peerWeighing) ownApplyHeap(frame int) uint64 {
	heap := applyHeapPerApply + applyHeapPerFrameByte*uint64(frame) + applyHeapPerDeleteRange*w.deletes +
		w.decoded + w.integrated
	if w.structs > 0 {
		heap += applyHeapPerIntegratedFrameByte * uint64(frame)
	}
	return heap
}

// The estimate's factors, in bytes, each set with headroom from what
// TestEveryApplysTransientHeapIsWithinItsEstimate measures (2026-10-26: bytes one apply allocates
// into a document with an update observer, per struct of a frame of 20,000 unless said, decode
// and integration together unless said); that test holds the estimate over every measurement.
// What each covers is what ygo's decode allocates (sjawhar/ygo v1.55.1-sami.1, decodeItem,
// crdt/update.go:1154-1317; decodeContent, :1319-1462; decodeTypeContent, :1464-1520;
// decodeDeleteSet, :1522-1579), and what integrating and encoding again for the room's observers
// allocates (encodeItem, :441-559).
const (
	// applyHeapPerApply is the transaction, the decode's own state and the observers' encode of a
	// small update: 2,336 bytes measured for the whole apply of a keystroke into the heaviest
	// document, 3,224 for a deletion of one character from it.
	applyHeapPerApply = 4 << 10
	// applyHeapPerRoomClient is each client in the room's state vector, which ygo's apply copies
	// into maps sized for it. A map takes slots for its size past 8/7 rounded up to a power of
	// two, so a room of 57,345 clients, the least that rounds to 131,072 slots, pays the most a
	// client: 19,356,120 bytes measured for a keystroke into such a room, each client holding one
	// GC struct, 338 a client with the observers' encode of that struct as a deleted item
	// (applyHeapPerRoomStruct), against 299 a client in a room of 20,000.
	applyHeapPerRoomClient = 160
	// applyHeapPerRoomStruct is each deleted item of the room, bounded by each struct the room
	// holds, at every apply: 875,008 bytes measured at sjawhar/ygo v1.54.1-sami.3, whose every
	// transaction's update carried the room's whole delete set, for a keystroke into a string of
	// 10,000 deleted items among 10,000 live ones, 88 a deleted item. The pin carries only the
	// transaction's own deletions in that update (reearth/ygo#350), so the term is headroom.
	applyHeapPerRoomStruct = 256
	// applyHeapPerFrameByte is each byte of the frame: a string ygo copies out of it is one
	// allocation of its bytes rounded up to a size class, at most twice them for a short one (a
	// string of 9 bytes takes 16), and JSON text is copied twice more (applyHeapPerJSONByte).
	applyHeapPerFrameByte = 2
	// applyHeapPerIntegratedFrameByte is each byte of the frame of an update that integrates a
	// struct, for the observers' encode of what it integrated: a buffer grown by appending, whose
	// allocations sum to about five times what it holds once it grows by a quarter at a time, and
	// the copy handed to the observers. 2,427,728 bytes measured for 220,018 bytes of Any strings
	// of 9 bytes in one struct, 11.0 a byte, and 1,592,072 for one string of 512 KiB, 3.
	applyHeapPerIntegratedFrameByte = 7
	// applyHeapPerRun is a GC struct or a skip: an Item and its content. 120 bytes measured for
	// each of maxUpdateItems GC structs into a room holding them, which decodes them and
	// integrates nothing, and for each skip.
	applyHeapPerRun = 192
	// applyHeapPerItem is an item of any content: the Item, its origins and its content's own
	// struct. 182 bytes measured for a margin entry of one null value into a room holding it.
	applyHeapPerItem = 192
	// applyHeapPerKey is an item that names a map key: the key.
	applyHeapPerKey = 64
	// applyHeapPerType is an item of a nested type: the type, its maps and, for an XML element,
	// its name.
	applyHeapPerType = 192
	// applyHeapPerNewStruct is a struct the update integrates: its place in the store and its
	// parent, the splits of the items its origins fall inside (sjawhar/ygo v1.55.1-sami.1,
	// crdt/update.go:976-981, crdt/item.go:83-114), and the observers' encode of it. 218 bytes
	// measured for a GC struct, 327 for a one-character string naming both origins, 563 for one
	// splitting the room's string at both, 526 for a margin entry of one null value.
	applyHeapPerNewStruct = 384
	// applyHeapPerNewType is an integrated item of a nested type beyond applyHeapPerNewStruct: the
	// observers' encode of its type and name. 749 bytes measured for an XML element, 592 for an
	// XML text.
	applyHeapPerNewType = 160
	// applyHeapPerDeleteRange is a range of the delete set: its decoded range, the splits of the
	// items it starts and ends inside, and its place in the transaction's delete set. 689 bytes
	// measured for each of 10,000 ranges splitting the room's string, the room's deleted items
	// included, and 129 for a range of a client the room has not seen.
	applyHeapPerDeleteRange = 768
	// applyHeapPerAnyValue is each lib0 Any value, at every depth (contentShape.anyHeap): its slot in
	// the item's values and their normalised copy, a boxed scalar or string header, and the copy a
	// partly held item's split makes of its values. 3,835,480 bytes measured for 100,000 null
	// values in one struct, 38 a value.
	applyHeapPerAnyValue = 64
	// applyHeapPerNewAnyValue is each Any value of an integrated item, as the observers' encode
	// writes it.
	applyHeapPerNewAnyValue = 8
	// applyHeapPerAnyArray is each Any array beyond its values: its boxed slice header.
	applyHeapPerAnyArray = 32
	// applyHeapPerAnyMap is each Any map: its header and its first group of slots, which a map of
	// one entry allocates whole. 392 bytes measured for a map of one entry with its null value,
	// the densest Any there is.
	applyHeapPerAnyMap = 384
	// applyHeapPerAnyMapEntry is each entry of an Any map past the map's first group of eight: its
	// slots, at a load as low as a map just past a growth leaves them. 124 bytes an entry
	// measured for maps of 10,000 entries, with each entry's key and value.
	applyHeapPerAnyMapEntry = 64
	// applyHeapPerJSONText is each JSON text ygo decodes with encoding/json (contentShape.jsonHeap):
	// its slot in the item's values and what encoding/json allocates for any text. 80 bytes
	// measured for a null text.
	applyHeapPerJSONText = 64
	// applyHeapPerNewJSONText is each JSON value of an integrated item, which the observers'
	// encode writes as JSON text again with an encoder of its own (fmtValToJSON, sjawhar/ygo
	// v1.55.1-sami.1, crdt/update.go:1669-1680). 298 bytes measured for a number, 705 for a map
	// read from legacy lib0 Any.
	applyHeapPerNewJSONText = 384
	// applyHeapPerJSONContainer is each JSON object or array: an object's map with its first
	// group. 763 bytes measured for an object of one entry, 835 for an object of one empty object.
	applyHeapPerJSONContainer = 384
	// applyHeapPerJSONMember is each separator of a JSON text, a ',' or a ':', which bounds its
	// members: an array's element in a slice grown by appending, an object's entry in a map grown
	// by inserting. 112 bytes measured for each element of arrays of 10,000 numbers.
	applyHeapPerJSONMember = 128
	// applyHeapPerNewJSONMember is each separator of an integrated JSON text, a member the
	// observers' encode writes again, keys sorted and each written through encoding/json. 290
	// bytes measured for each entry of objects of 10,000 entries.
	applyHeapPerNewJSONMember = 128
	// applyHeapPerJSONByte is each byte of JSON text beyond applyHeapPerFrameByte: ygo copies the
	// text into a string and again into the bytes encoding/json reads (fmtValFromJSON, sjawhar/ygo
	// v1.55.1-sami.1, crdt/update.go:1722-1731), and the strings it decodes copy it once more.
	applyHeapPerJSONByte = 2
	// applyHeapPerNewJSONByte is each byte of an integrated JSON text beyond
	// applyHeapPerIntegratedFrameByte, which the observers' encode escapes and copies again. 11.4
	// bytes a byte measured for strings of 1,000 escapes.
	applyHeapPerNewJSONByte = 6
)

// applyCost is a struct's share of the apply heap's estimate beyond its bytes, which
// applyHeapPerFrameByte prices with the frame's: decode is what decoding it costs, which every
// struct of an update does, and integrate is what integrating it and encoding it for the room's
// observers costs, which only a struct past the room's state does.
type applyCost struct {
	decode, integrate uint64
}

func (c *applyCost) add(other applyCost) {
	c.decode += other.decode
	c.integrate += other.integrate
}

// structApplyHeap is walked's applyCost: arithmetic over what the walk counted inside its content
// as it read it (contentShape), so that the content is read once, by the walk that also refuses
// what ygo's decode would refuse (walkUpdate).
func structApplyHeap(walked updateStruct) applyCost {
	switch walked.kind {
	case structGC, structSkip:
		return applyCost{decode: applyHeapPerRun, integrate: applyHeapPerNewStruct}
	}
	cost := applyCost{decode: applyHeapPerItem, integrate: applyHeapPerNewStruct}
	if walked.keyed {
		cost.decode += applyHeapPerKey
	}
	if walked.kind == structType {
		cost.add(applyCost{decode: applyHeapPerType, integrate: applyHeapPerNewType})
	}
	shape := walked.shape
	if !shape.jsonReadAsAny {
		cost.add(shape.anyHeap())
		cost.add(shape.jsonHeap())
		return cost
	}
	// A ContentJSON item whose values read only as lib0 Any is charged both readings, the values
	// slice the failed reading of them as JSON text made included (contentShape.jsonReadAsAny);
	// integrated, every value is written as JSON text again, taken at four bytes of text for each
	// byte of lib0 Any, and the values the first reading read are not written twice.
	cost.decode += shape.anyHeap().decode + shape.jsonHeap().decode + walked.length*applyHeapPerJSONText
	cost.integrate += walked.length*applyHeapPerNewJSONText + 4*applyHeapPerNewJSONByte*uint64(len(walked.content))
	return cost
}

// anyHeap is the applyCost of the lib0 Any values the walk counted in a struct's content, each
// value at every depth with its arrays, its maps and those maps' entries, as anycodec's readAny
// reads them (sjawhar/ygo v1.55.1-sami.1, internal/anycodec/decoder.go:261-393).
func (s contentShape) anyHeap() applyCost {
	return applyCost{
		decode: s.anyValues*applyHeapPerAnyValue + s.anyArrays*applyHeapPerAnyArray +
			s.anyMaps*applyHeapPerAnyMap + s.anyEntries*applyHeapPerAnyMapEntry,
		integrate: s.anyValues * applyHeapPerNewAnyValue,
	}
}

// jsonHeap is the applyCost of the JSON texts the walk counted in a struct's content, which ygo
// decodes with encoding/json: each text, its containers and its separators, counted outside its
// strings, and its bytes beyond the frame's price.
func (s contentShape) jsonHeap() applyCost {
	return applyCost{
		decode: s.jsonTexts*applyHeapPerJSONText + s.jsonContainers*applyHeapPerJSONContainer +
			s.jsonSeparators*applyHeapPerJSONMember + s.jsonBytes*applyHeapPerJSONByte,
		integrate: s.jsonTexts*applyHeapPerNewJSONText + s.jsonSeparators*applyHeapPerNewJSONMember +
			s.jsonBytes*applyHeapPerNewJSONByte,
	}
}
