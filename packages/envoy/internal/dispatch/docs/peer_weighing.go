package docs

import (
	"github.com/reearth/ygo/crdt"
)

// This file holds what one walk of a browser's update weighs it at (peerWeighing, the visitor
// weighPeerUpdate hands walkUpdate) and the arithmetic each of its rules is priced by: what the
// update inserts, what it adds to the room's struct, unrendered and frame totals, and what it
// adds to the room's structure charge.

// jsonReencodeGrowth is the most a byte of JSON content can grow to once ygo writes it into the
// room's state again. ygo keeps a JSON item's, a format's and an embed's values decoded and encodes
// each again as JSON text with encoding/json (fmtValToJSON, sjawhar/ygo v1.55.1-sami.1,
// crdt/update.go:468-480 and :1669-1680). A JSON item whose values read only as lib0 Any, as ygo
// up to 1.51.0 wrote them (decodeContent's wireJSON case, :1328-1360), grows the most: each false,
// one byte, is written as the five of false with its length prefix, six bytes a byte. A JSON text
// grows less: encoding/json writes a number below 10^21 in full, so the four bytes of 1e20, five
// with their length prefix, become the twenty-one of 100000000000000000000, twenty-two with
// theirs, 4.4 bytes a byte; a line separator's three bytes become its six-byte escape.
const jsonReencodeGrowth = 6

// peerWeighing is what one walk of a browser's update finds against held, the room's state vector
// when the walk began (walkUpdate's visitor). It replaces a decode of the update
// (crdt.ContentIDsFromUpdateV1, which allocates an item for every struct, up to maxUpdateItems
// of them for one frame) with the same arithmetic over the walk's structs: a struct, or the part of
// one, past its client's clock in held is what the room does not already hold. Skips are clocks an
// update withholds and are never integrated, so they count for nothing. From the same walk it
// estimates what ygo's apply of the update allocates (applyHeap).
type peerWeighing struct {
	held crdt.StateVector
	// inserted is how much content the update carries that the room does not already hold and
	// that the room can render, counted in the units a Yjs item's content is measured in: a
	// character of text, an element, an attribute an element gains. It is zero for an update that
	// only deletes, zero for one that only carries clocks born deleted, and zero for a
	// whole-state message of a document the room already holds, which every browser sends on its
	// first sync and which measured literally would be the whole document. No frame can wrap it
	// (weighStruct).
	inserted int64
	// structs is the structs reaching past the room's state, each of which integration adds to the
	// room's store; deletes is the update's delete ranges, and splits the items at their two ends
	// that their applies can leave split for good (deletedRuns.splitEnds): none for a range the
	// room is known to hold deleted already (known), and for one that overlaps what it is known to
	// hold, none at an end inside that. deleteStructs is what the ranges can add to the room's
	// struct total: their splits, with the last end of a range that reaches past the room's clock
	// for its client (held) counted splitsAStructCanCause rather than one, since the range's tail
	// can park there as a parked range of its own, which the room's count holds
	// splitsAStructCanCause structs for (structTotalOf). known is nil for a room no load charged,
	// in which every end but a client's first clock can split.
	structs, deletes, splits, deleteStructs uint64
	known                                   *deletedRuns
	// decoded is every walked struct's share of the apply's estimate, since ygo's apply decodes
	// each before it asks whether the room holds it; integrated is the share of the structs past
	// the room's state alone, which only they cost (structApplyHeap).
	decoded, integrated uint64
	// content is the heap one copy of the room keeps for the content of the structs past its
	// state that no rendering prices (unrenderedContentHeap).
	content uint64
	// unrendered is the unrendered bytes of the structs past the room's state (unrenderedBytes),
	// what the update adds to the room's unrendered total, and frame the bytes its new structs and
	// the delete ranges the room is not known to hold deleted can add to the room's whole-state
	// frame (frameBytes, deleteFrameBytes), what it adds to the room's frame total.
	unrendered, frame uint64
	// retired is the clients the room's history lists as retired by a rebuild, each with the clock
	// it had reached (roomTotals.retired), nil for a room never rebuilt or no load listed, and
	// entries where that list's own entries sit (roomTotals.entries). former is whether the update
	// carries a client group of a retired client starting below that clock (walkedGroup), which
	// only a browser holding the replaced history sends; forged whether it changes the list - an
	// entry past the room's state, a struct naming an entry as its origin, which is how an
	// overwrite of a map key is written, or a delete range over an entry - which no browser does.
	retired        map[crdt.ClientID]uint64
	entries        retiredEntries
	former, forged bool
}

// walkedGroup marks the update former when the group is a retired client's and starts below the
// clock that client had reached: the group's first struct is then the replaced history's. Every
// struct of a group starts at or past the group's clock, so this sees every struct of a retired
// client below its clock.
//
// It reads each group's header - its client and the clock its first struct starts at - which the
// walk hands over before it reads the group's structs or weighs the group's count against
// maxUpdateItems (groupVisitor), so it answers whatever the walk read before a refusal. That is
// what a browser holding a history at the decode cap needs: its whole-state message, the history
// with an edit made since, is past the cap, and the walk refuses it at the header of the group
// that passes it; when the browser's own client sorts before the history's (ygo and Yjs both
// write groups by descending client, sjawhar/ygo v1.55.1-sami.1, crdt/update.go:386-387; yjs
// 13.6.32 src/utils/encoding.js:99), and a history one client wrote is one group, no struct of the history
// is read before that refusal, and only the header shows it. Every rule the walk refuses a
// history's handoff by - its count, its content, its delete set - comes at that header or after
// it: the groups before it are the browser's own clients', carrying only what its editor wrote
// since, which the walk refuses only for more structs than the cap or content no editor sends.
//
// It never closes an honest browser. A retired client's clocks below the one it had reached are
// the replaced history's structs: a client numbers its structs from zero up, the rebuild reads
// that clock from the history it replaces (rebuiltSeed), and the rebuilt room holds none of them -
// its seed is a fresh client's, and the hook refuses every update carrying them - so a browser
// that took the document from the rebuilt room holds none either, and writes a group of a retired
// client, if any, only from that client's structs at or past that clock. The editor's 4413 path
// builds its document afresh on a new connection, a new Y.Doc with a client id of its own
// (packages/dispatch/web/src/features/doc/connection.ts:153, :222-231), so what it writes next is
// no retired client's. And no encoder begins a client group with anything but a struct it holds of
// that client: ygo starts each group inside its first item (encodeV1Locked, crdt/update.go:365-405;
// encodeStructStoreV1, crdt/merge.go:157-195, which writes a skip only between two items), and Yjs
// at the first struct of its store, which holds no skips (writeStructs, src/utils/encoding.js:57-71),
// at the first struct of a merge, whose readers drop skips (mergeUpdatesV2, src/utils/updates.js:338,
// writing a skip only between two structs of one client, :405-417), and never with a skip in a
// diff (diffUpdateV2, :477-481). So a header of a retired client below that clock is a struct of
// the replaced history, short of a fresh browser drawing a retired client's random 32-bit id.
func (w *peerWeighing) walkedGroup(client, clock, structs uint64) {
	if limit, listed := w.retired[crdt.ClientID(client)]; listed && structs > 0 && clock < limit {
		w.former = true
	}
}

func (w *peerWeighing) walkedStruct(walked updateStruct) {
	w.weighStruct(walked, structApplyHeap(walked))
}

// weighStruct weighs walked, whose share of the apply's estimate is cost. It is walkedStruct's
// body with that share passed in, so that a weighing no reader of the estimate holds pays not
// even its arithmetic (writtenWeighing.walkedStruct).
func (w *peerWeighing) weighStruct(walked updateStruct, cost applyCost) {
	w.decoded += cost.decode
	if walked.kind == structSkip {
		return
	}
	known := w.held[crdt.ClientID(walked.client)]
	end := walked.clock + walked.length
	// A struct of no clocks at the room's clock or past it is a struct all the same: it spans
	// nothing a state vector can see, and integration still stores it.
	if end <= known && (walked.length != 0 || walked.clock < known) {
		return
	}
	// Clocks that render nothing - a GC run's and a deleted item's (bornDeleted) - are not
	// counted here: nothing the room renders ever comes of them, and a rendering that can grow is
	// what both readers of this count weigh (the at-the-bound rule in weighPeerUpdate, and
	// refusedUnderPressure). Their length is also a varint where every other kind's is its
	// content's own bytes, so one struct of about fifteen bytes declares 2^53 clocks: counted,
	// 1,024 such groups took this sum past 2^63 and wrapped it negative, which read as an update
	// inserting nothing, and both rules then admitted a paste each of them refuses on its own.
	// What those structs do add - a struct, the splits its integration can cause, its frame bytes
	// - the struct total, the frame total and the structure part charge in full (added,
	// frameBytes, structureHeap). Every clock still counted costs the frame at least one byte, so
	// the sum is under the frame's length and far under the saturation point; that point is here
	// so that a content kind measuring its length in anything but its bytes cannot wrap it
	// either.
	if !bornDeleted(walked) {
		w.inserted = min(maxEditorClock, w.inserted+int64(min(end-max(walked.clock, known), maxEditorClock)))
	}
	w.structs++
	w.integrated += cost.integrate
	w.content += unrenderedContentHeap(walked)
	w.unrendered += unrenderedBytes(walked)
	w.frame += frameBytes(walked)
	w.forged = w.forged || walked.parent == parentRetired ||
		(walked.hasOrigin && w.entries.overlap(walked.origin.client, walked.origin.clock, 1)) ||
		(walked.hasRightOrigin && w.entries.overlap(walked.rightOrigin.client, walked.rightOrigin.clock, 1))
}

// bornDeleted is whether walked renders nothing of its own: a GC run, or a deleted item. It is
// not whether the room holds those clocks deleted - ygo skips a GC run's clocks the room holds
// already, and holds a deleted item it parked live until its predecessor arrives (deletedRuns) -
// and nothing reads it for that.
func bornDeleted(walked updateStruct) bool {
	return walked.kind == structGC || walked.kind == structDeleted
}

func (w *peerWeighing) walkedDelete(client, clock, length uint64) {
	w.deletes++
	var splits uint64
	start, end := w.known.splitEnds(client, clock, length)
	if start {
		splits++
	}
	if end {
		splits++
	}
	w.splits += splits
	w.deleteStructs += splits
	if end && clock+length > w.held[crdt.ClientID(client)] {
		w.deleteStructs += splitsAStructCanCause - 1
	}
	if !w.known.covers(client, clock, length) {
		w.frame += deleteFrameBytes(client, clock, length, splits)
	}
	w.forged = w.forged || w.entries.overlap(client, clock, length)
}

// splitsAStructCanCause is the most structs integrating one struct can add to a store beyond
// itself, and the most applying one delete range can: each splits at most the item it starts in
// and the item it ends in (an item's integrate splits the items holding its origin and its right
// origin, sjawhar/ygo v1.55.1-sami.1, crdt/update.go:976-981 and crdt/item.go:99-119, through
// crdt/store.go:213-263; a delete range splits the items at its two ends,
// crdt/delete_set.go:276-278).
const splitsAStructCanCause = 2

// added is what the walked update can add to the room's struct total: each new struct with the
// splits its integration can cause, and what its delete ranges can add (deleteStructs). A
// whole-state handoff of the document the room holds adds nothing: it carries no struct past the
// room's state, and every one of its delete ranges the room holds deleted already, whose splits
// ygo merges back at commit (deletedRuns.splitEnds). So a browser reconnecting to a room at the
// decode cap, which hands its whole state over on connecting as every browser does, is admitted.
func (w *peerWeighing) added() uint64 {
	return w.structs*(1+splitsAStructCanCause) + w.deleteStructs
}

// addition is what the walked update can add to the room's totals (roomTotals): its structs
// (added), its unrendered bytes and its frame bytes, and whether it inserts a struct.
func (w *peerWeighing) addition() roomCount {
	return roomCount{structs: w.added(), unrendered: w.unrendered, frame: w.frame, inserts: w.structs > 0}
}

// structureHeap is what the walked update adds to its room that a count of the rendering cannot
// see, at both copies (roomCopies), which the room budget charges to the room's structure part
// (roomCharge): each new struct at bytesOfHeapAStruct, the splits its integration can cause and
// the splits its delete ranges can leave (splits) at bytesOfHeapASplit, which a split keeps where
// the kind the split divides does not, and the content of its new structs that no rendering prices
// (content). A whole-state handoff of a document the room already holds adds no struct and carries
// no delete range the room is not known to hold deleted, and is charged nothing.
func (w *peerWeighing) structureHeap() int64 {
	splits := w.structs*splitsAStructCanCause + w.splits
	return int64((w.structs*bytesOfHeapAStruct + splits*bytesOfHeapASplit + w.content) * roomCopies)
}

// unrenderedContentHeap is the heap one copy of a room keeps for walked's content that no count of
// the rendering prices: a map key's bytes, as text, and the content itself at its kind's heap a
// byte - text for a string or an XML type's name (bytesOfHeapAByteOfText), values for Any, JSON or
// a format (bytesOfHeapAByteOfValues). Binary content and embeds the walk refuses from a browser
// (errUpdateNotFromEditor) before any weighing, so the room budget never charges them; they are
// priced here as a binary's bytes and an embed's value would weigh, as text and as values, only so
// that an estimate of an update a walk of a room's own state reads (fromRoom) holds them too
// (TestEveryApplysTransientHeapIsWithinItsEstimate). The rendering prices what it shows: a string,
// an embed, a format or an XML type directly under the fragment. Everything else is charged here,
// whether or not it is shown, since the walk cannot tell for a struct whose parent is an item or
// its neighbour's (parentItem, parentOfNeighbour) where it sits: text typed into a paragraph, and
// a later entry for a margin key, both read that way. Charged as unshown, typing costs its text's
// price a byte beside the per-struct price, a few bytes a keystroke, against content in the margin
// that no count would ever see were it taken as shown. A deleted item's or a GC run's content is a
// length, which no heap follows.
func unrenderedContentHeap(walked updateStruct) uint64 {
	heap := uint64(len(walked.key)) * bytesOfHeapAByteOfText
	content := uint64(len(walked.content))
	switch walked.kind {
	case structString, structType, structEmbed, structFormat:
		if walked.parent == parentFragment {
			return heap
		}
	}
	switch walked.kind {
	case structString, structBinary, structType:
		return heap + content*bytesOfHeapAByteOfText
	case structAny, structJSON, structEmbed, structFormat:
		return heap + content*bytesOfHeapAByteOfValues
	}
	return heap
}

// unrenderedBytes is walked's unrendered bytes: the encoded bytes of its map key and of its
// content that are not text, which are what the hook's byte and clock rules cannot see. A value
// of an Any or JSON item, a binary, an embed or a format value, and an XML element's name, are one
// clock each whatever their size, and a map key is no clock at all; none of them is weighed by the
// rendering where it renders nothing - the margin's records, an element's attributes the schema
// does not render. A text's characters are not counted: each is a clock, which the bound's rules
// weigh where the room renders them (inserted, maxPeerInsertAtTheBound), and each renders where
// the editor writes it. A deleted item's, a GC run's and a skip's content is a length, which
// takes no bytes past its header; a deleted map entry's key stays in the state, and is counted.
//
// It is measured wherever the struct sits, because the walk cannot place a value whose parent is
// an item or its neighbour's (parentItem, parentOfNeighbour): a later record for a margin key and a
// later value for an element's attribute read alike. So it counts what the server's own margin
// rule (maxMarginBytes) and anchor rule (maxAnchorBytes) weigh - each record's whole encoding,
// keys and the server's own fields with its text, and each anchor mark's format value, written
// again at each text it covers - and with them the names and attributes of the fragment's
// elements and the values of its other marks, which the server weighs through its rendering and
// element bounds. maxUnrenderedBytes bounds the sum of all of them.
func unrenderedBytes(walked updateStruct) uint64 {
	switch walked.kind {
	case structString, structDeleted, structGC, structSkip:
		return uint64(len(walked.key))
	}
	return uint64(len(walked.key) + len(walked.content))
}

// frameBytes is what walked, a struct past the room's state, can add to the room's frame total:
// to the whole-state frame (crdt.EncodeStateAsUpdateV1, sjawhar/ygo v1.55.1-sami.1,
// crdt/update.go:94-98 and :235-257, and encodeItem, :441-559), now and through whatever happens
// to it later -
//
//   - its own bytes in the update (updateStruct.size), with what ygo's writing of its content
//     again can grow them by. ygo keeps a JSON item's, a format's and an embed's values decoded
//     and writes them as JSON text (jsonReencodeGrowth), and an Any item's values decoded and
//     written by lib0's rules, which read an integer of more than 32 bits from as few as six bytes,
//     a tag and a five-byte varint, and write it as a nine-byte float64 (writeAnyInt,
//     encoding/encoder.go:283-299; internal/anycodec/decoder.go:276-284): half as much again.
//     ygo writes every other content as it read it, and names a struct's origins and parent as
//     the update did, or writes an item it holds as a GC orphan - one whose parent could not be
//     resolved (markGCOrphan, crdt/update.go:413-438) - in fewer bytes still (encodeItem,
//     :458-466, and :533-551);
//   - its place among the room's structs (placeBytes);
//   - its deletion, by its own delete range or by whatever deletes it later - a block deleted
//     around it, an entry written over it, a parent deleted before it arrives (deleteRangeBytes);
//   - the splits its integration can cause: one in the item holding its origin and one in the item
//     holding its right origin, for each it names (splitsAStructCanCause gives where), so a GC run
//     or an item naming only its parent splits nothing (splitFrameBytes).
//
// A delete range's share is deleteFrameBytes.
func frameBytes(walked updateStruct) uint64 {
	written := uint64(walked.size)
	switch walked.kind {
	case structJSON, structFormat, structEmbed:
		written += (jsonReencodeGrowth - 1) * uint64(len(walked.content))
	case structAny:
		written += uint64(len(walked.content)) / 2
	}
	written += placeBytes(walked) + deleteRangeBytes(walked.client, walked.clock, walked.length)
	if walked.hasOrigin {
		written += splitFrameBytes
	}
	if walked.hasRightOrigin {
		written += splitFrameBytes
	}
	return written
}

// deleteFrameBytes is what a delete range the room is not known to hold deleted (deletedRuns) can
// add to the room's frame total: its range in the room's delete set, which a range of clocks the
// room does not hold yet keeps there parked (deleteRangeBytes), and splitFrameBytes for each of
// the splits its apply can leave for good in the items at its two ends, splits of them
// (deletedRuns.splitEnds). The items it deletes are already priced for their deletion: by the
// count (deletionReserve) or by the update that brought them (frameBytes). A range the room is
// known to hold deleted adds nothing to the frame: ygo's encode writes the room's delete set from
// its deleted items, merged where their clocks touch (buildDeleteSet, sjawhar/ygo v1.55.1-sami.1,
// crdt/update.go:659-678), and the room's parked ranges merged the same way (DeleteSet.Merge,
// crdt/delete_set.go:125-142), so the clocks it names are written as they were, and it leaves no
// split.
func deleteFrameBytes(client, clock, length, splits uint64) uint64 {
	return deleteRangeBytes(client, clock, length) + splits*splitFrameBytes
}

// maxVarintBytes is the most bytes a lib0 varint takes for any client or clock a room holds: the
// walk refuses a varint past 53 bits from a browser (update_walk.go), a server write's client is 32
// bits (crdt.NewClientID, sjawhar/ygo v1.55.1-sami.1, crdt/doc.go:481-488) and its clocks count
// its own structs, and 53 bits take eight bytes of seven.
const maxVarintBytes = 8

// varintBytes is the bytes lib0 writes v in as a varint: seven bits a byte.
func varintBytes(v uint64) uint64 {
	n := uint64(1)
	for ; v >= 0x80; v >>= 7 {
		n++
	}
	return n
}

// placeBytes is the most walked's place among the room's structs adds beside its own bytes: a
// group of its own, should the room hold nothing of its client - the group's struct count, client
// and first clock, and a byte more of the count of groups - or, should it park beyond a gap in its
// client's clocks, the skip ygo writes before it, a byte and the gap's length, no more than its
// clock (encodeStructStoreV1, sjawhar/ygo v1.55.1-sami.1, crdt/merge.go:157-195), with a byte more
// of its group's struct count.
func placeBytes(walked updateStruct) uint64 {
	return 2 + varintBytes(walked.client) + varintBytes(walked.clock)
}

// deleteRangeBytes is the most deleting the clocks [clock, clock+length) of client adds to the
// room's delete set as ygo writes it (encodeDeleteSet, sjawhar/ygo v1.55.1-sami.1,
// crdt/update.go:680-698): the range's clock and length, and should the delete set hold no range
// of client, its client and a byte of its count of ranges; and a byte more of a count, of ranges
// or of clients. A range that touches one the delete set holds is merged with it (buildDeleteSet,
// :659-678) and adds less.
func deleteRangeBytes(client, clock, length uint64) uint64 {
	return 2 + varintBytes(client) + varintBytes(clock) + varintBytes(length)
}

// maxDeleteRangeBytes is deleteRangeBytes for a client, a clock and a length of maxVarintBytes
// each.
const maxDeleteRangeBytes = 2 + 3*maxVarintBytes

// splitFrameBytes is the most one split of an item the room holds can add to the room's frame
// total (splitItem, sjawhar/ygo v1.55.1-sami.1, crdt/item.go:588). The right part becomes an item
// of its own, written with an info byte, its origin - the left part's last clock - and the item's
// right origin, each a client and a clock, and its content's length (a string's bytes, a run's
// values or clocks), whose bytes are the item's own, moved and not added; an item with an origin
// names no parent; and its group's struct count may take a byte more: 2 + 5 x maxVarintBytes. The
// right part can be deleted later, a range of its own (maxDeleteRangeBytes), and the deletion of
// the two parts can add more than the deletion of the item was priced at (deletionReserve) by the
// right part's length, which its content no longer gives back (maxVarintBytes).
const splitFrameBytes = 2 + 5*maxVarintBytes + maxDeleteRangeBytes + maxVarintBytes

// deletionReserve is the most deleting walked, a struct of the room's encoded state, can add to
// the room's frame: the range it adds to the delete set (deleteRangeBytes), less what ygo gives
// back of its content, which it writes as a deleted run's length once the item is deleted
// (gcTxnDeleteSet, sjawhar/ygo v1.55.1-sami.1, crdt/snapshot.go:370-407, in every room, whose
// documents keep ygo's GC on). A GC run, a deleted run and a skip are deleted already.
func deletionReserve(walked updateStruct) uint64 {
	switch walked.kind {
	case structGC, structDeleted, structSkip:
		return 0
	}
	added := deleteRangeBytes(walked.client, walked.clock, walked.length)
	givenBack := uint64(len(walked.content)) - min(uint64(len(walked.content)), varintBytes(walked.length))
	return added - min(added, givenBack)
}
