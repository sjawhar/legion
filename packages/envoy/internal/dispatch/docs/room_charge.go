package docs

// This file holds one loaded room's charge against the budget (roomCharge): its three parts, the
// writes that move each of them - a count that lands, the content a transaction adds, the
// structure a browser's update or a server write leaves - and what a published rendering's count
// re-bases them from.

// earlyCountCharge is the uncounted part of a room's charge (roomCharge) past which the room's next
// rendering is counted at once rather than once its last count is roomCountInterval old: 16 MiB of
// charge, about 7,900 bytes of added content at the densest price at both copies. Typing never
// reaches it - a person typing ten characters a second adds 150 bytes in the interval, 317,400
// bytes of charge - while a paste of 8 KiB does, and its room is counted within one count
// of the measurer's queue rather than up to fifteen seconds later. It is still one count at a time
// for the pod (elementMeasurer).
const earlyCountCharge = 16 << 20

// roomCharge is one loaded room's charge (weight), the sum of three parts (roomWeight):
//
//   - the counted part: its latest measured document, priced from the count of its rendering, the
//     rendering's bytes and the struct total of its state (copyHeap), at both copies. Each count
//     replaces the last rather than adding to it, so a document that shrinks is charged less.
//   - the uncounted part: the content bytes the room's transactions have added since the rendering
//     that count measured (contentAdded), each at the densest price at both copies, since what
//     elements they make is unknown until a count. Every transaction is charged it once, whoever
//     wrote it - a browser's admitted update, the server's own write, an identity-closure repair -
//     by the update observer whose drain of the room's update log hands it over (chargeAdded). A
//     count re-bases it: it clears the bytes added up to the rendering it measured, never those
//     added since. A whole-state handoff of a document the room already holds adds nothing here.
//   - the structure part: what each update the room integrates adds that a count of the rendering
//     cannot see (peerWeighing.structureHeap) - each new struct at bytesOfHeapAStruct, the splits
//     its integration can cause and the splits each delete range can leave at its ends, none at an
//     end the room already held deleted (deletedRuns.splitEnds), at bytesOfHeapASplit, and the
//     content no rendering prices at its kind's heap a byte - at both copies. A browser's update is
//     charged it by the peer hook that admitted it (grew); every other transaction - an API write's
//     publish, an injected update, settlement's repair, any of the server's own writes - by the
//     update observer whose drain of the room's update log hands it over (wrote), at the same
//     prices, as each of their scans counts toward the room's scan total (roomScans.observed). The
//     server's writes are never refused for it: they only raise the charge, so the budget refuses
//     browsers or closes unused rooms sooner. A count never clears it: one update of GC structs
//     keeps tens of megabytes with the rendering unchanged. Only the room's retirement drops it,
//     with the rest of the charge.
//
// Two parts can charge one struct: the structure part charges what each update adds, and a count
// prices the struct total of the whole state once its structs outweigh its elements (copyHeap),
// which every struct the structure part charged is part of. The structure part over-charges what
// merges back too: ygo squashes a run of typed characters into one item and re-merges a split
// nothing came between (sjawhar/ygo v1.55.1-sami.1, crdt/transaction.go:39-44), while each struct
// is charged as one of its own with both its splits; and it charges every struct at the heaviest
// kind's price, rendered or not, so a rendered struct is charged here beside its count's price of
// the elements it makes. What deletes leave behind - tombstones a rendering does not show - is
// priced between counts only as far as the structure part charged their ranges' splits, and by
// the next count with every other struct of the state.
//
// What a room is charged has a lifecycle. Its admission reserves the count of its stored version
// and the structs of its stored history at both copies (roomReservation); its load, once the
// rendering it loaded with is counted (keepReplica), replaces that reservation with this charge
// (load); the room's update observer charges each transaction's added content (adds) and the
// structure of each transaction the hook did not apply (wrote), the hook each admitted update's
// structure (grew); and each count the measurer makes after that replaces the counted part
// (measured), on its cadence or sooner once the uncounted part passes earlyCountCharge. A rendering
// published without a count of its own (renderedReplica.publishSize) re-bases nothing.
type roomCharge struct {
	budget *roomBudget
	room   string
	// heap is what one copy of the room's latest measured document is priced at (copyHeap).
	heap int64
	// content is the bytes of content the room's transactions have added since it loaded, and
	// measuredAt what it was at the rendering heap prices.
	content, measuredAt int64
	// structure is the structure part, at both copies already.
	structure int64
	weight    int64
	retired   bool
	// closeOnceUnused is set on a room loaded with no socket's reservation while the budget is
	// over its limit, which the server's own work opened over it; it is closed once unused
	// (Service.closeIfOpenedOverBudget) unless a browser joins it first (join).
	closeOnceUnused bool
	// deleted is the delete ranges the room is known to hold deleted, which the structure part
	// and the struct total charge nothing for again (deletedRuns). It has its own lock, never the
	// budget's.
	deleted deletedRuns
}

// charged moves the room's charge by content in its uncounted content part and by structure in
// its structure part, and re-weighs its entry under the budget's lock: the one body the four
// charges below share. A nil charge, a replica no room's load charged (a test's), charges
// nothing, and so does a move of nothing in either part.
func (c *roomCharge) charged(content, structure int64) {
	if c == nil || (content == 0 && structure == 0) {
		return
	}
	c.budget.mu.Lock()
	defer c.budget.unlock()
	c.content += content
	c.structure += structure
	c.budget.reweighLocked(c)
}

// adds charges the room content bytes of content a transaction added (contentAdded) to its
// uncounted part, at the densest price at both copies until a count of a rendering holding them
// re-bases it (measured). Nil, a replica no room's load charged (a test's), charges nothing.
func (c *roomCharge) adds(content int64) {
	c.charged(content, 0)
}

// applied charges the room what update added, one transaction's update the peer hook's own apply
// committed, which the room's update observer hands over (renderedReplica.chargeAdded): its
// content a rendering can show to the uncounted part, as contentAdded counts it, its structure
// having been charged by the hook that admitted it (grew). From the same walk it lists what that
// commit left the room holding deleted as the room's (deletedRuns.list): the update's delete set,
// and every GC struct it carries.
//
// ygo writes the transaction's own deletions into the update of every commit
// (cloneDeleteSet(txn.deleteSet) in collectAndEncodeLocked, sjawhar/ygo v1.55.1-sami.1,
// crdt/doc.go:861, written by encodeV1Locked, crdt/update.go:403; reearth/ygo#350), so what the
// apply of the browser's update actually deleted is in it, which is the room's own answer for
// what the browser's bytes only claimed: a browser's GC run over clocks the room holds live
// deletes none of them, and a delete range past the clocks the room holds parks and deletes
// nothing (deletedRuns). The one thing ygo holds deleted that no commit's delete set names is a
// GC struct, which it appends to the store already deleted without integrating it
// (crdt/update.go:1161-1174 and :938-949), so the walk lists those runs too - safely, because
// this is ygo's own encode of a commit the room made, from the state before the transaction
// (encodeV1Locked over txn.beforeState, crdt/update.go:367-405), so every struct in it is one
// the room's store holds, a GC struct over clocks the room already held having been skipped by
// the apply before anything was appended (crdt/update.go:914-917). An item of the update the
// room holds live is still not listed: a struct that renders nothing is not always one ygo holds
// deleted, and one it does hold deleted the commit's delete set names already (deletedRuns).
//
// A range the list holds already costs a read of it and no more, and a keystroke's update carries
// no delete range and no GC struct at all, so the listing costs the walk the charge already makes
// and nothing besides.
//
// A delete range weighed between the apply and this drain finds those clocks unlisted and is
// charged for them, which over-charges and never under-charges. An update the walk cannot read is
// charged its bytes as content, as contentAdded charges it, and lists what the walk read before
// it refused. Nil, a replica no room's load charged (a test's), charges nothing and walks nothing.
func (c *roomCharge) applied(update []byte) {
	if c == nil {
		return
	}
	walked := appliedWalk{deleted: c.deletes()}
	if _, err := walkUpdate(update, fromRoom, &walked); err != nil {
		c.adds(int64(len(update)))
		return
	}
	c.adds(walked.content.bytes)
}

// appliedWalk is applied's visitor: the content a rendering can show (contentAdded's
// renderedContent) of one walk of a transaction's update, and the room's list of delete ranges,
// which that walk lists the update's delete set and its GC runs into.
type appliedWalk struct {
	content renderedContent
	deleted *deletedRuns
}

// walkedStruct counts the struct's content and lists it when it is a GC run, the one struct of
// the room's own commit ygo holds deleted that the commit's delete set does not name (applied).
func (w *appliedWalk) walkedStruct(walked updateStruct) {
	w.content.walkedStruct(walked)
	if walked.kind == structGC {
		w.deleted.list(walked.client, walked.clock, walked.length)
	}
}

func (w *appliedWalk) walkedDelete(client, clock, length uint64) {
	w.deleted.list(client, clock, length)
}

// grew charges the room structure more in its structure part, which no count clears: what an
// update the peer hook admitted adds that the rendering does not price (peerWeighing.
// structureHeap). Nil charges nothing.
func (c *roomCharge) grew(structure int64) {
	c.charged(0, structure)
}

// wrote charges the room what update added, one transaction's update the peer hook did not apply -
// an API write's publish, an injected update, settlement's repair, any of the server's own writes:
// its content a rendering can show to the uncounted part, as contentAdded counts it, and its
// structure to the structure part, priced as the hook prices a browser's update
// (peerWeighing.structureHeap). Every struct the update carries is new to the room, since ygo
// encodes a transaction's update from the state before it (sjawhar/ygo v1.55.1-sami.1,
// collectAndEncodeLocked, crdt/doc.go:844-873), so each is charged with the splits its
// integration can cause, with the splits each delete range can leave at its ends
// (deletedRuns.splitEnds) and
// the content no rendering prices. It refuses nothing: the write has committed, and its charge only
// raises the budget's figures, so the budget refuses browsers and closes unused rooms sooner.
//
// The same walk lists what the commit left the room holding deleted as the room's
// (deletedRuns.list): its delete set, what that commit deleted, every range of it weighed before
// it is listed - ygo sorts and merges each client's ranges as it decodes them
// (crdt/update.go:1527), so no two ranges of one update touch, and listing one leaves what the
// rest are charged as it was - and every GC struct it carries, the one struct ygo holds deleted
// that no commit's delete set names, listed here as in applied and safe for the same reason: the
// update is ygo's own encode of a commit the room made, so every struct in it is one the room's
// store holds. An item the room holds live is not listed, here as in applied. One walk reads the
// content, the structure, the delete set and the GC runs. An update the walk cannot read is
// charged its bytes as content, as contentAdded charges it, and the structure the walk read
// before it refused. Nil, a replica no room's load charged (a test's), charges nothing and walks
// nothing.
func (c *roomCharge) wrote(update []byte) {
	if c == nil {
		return
	}
	weighed := writtenWeighing{structure: peerWeighing{known: c.deletes()}, deleted: c.deletes()}
	_, err := walkUpdate(update, fromRoom, &weighed)
	content := weighed.content.bytes
	if err != nil {
		content = int64(len(update))
	}
	c.charged(content, weighed.structure.structureHeap())
}

// writtenWeighing is wrote's visitor: the content a rendering can show (contentAdded's
// renderedContent) and the structure (peerWeighing) of one walk of a server write's update, and
// the room's list of delete ranges, which that walk lists the update's delete set and its GC
// runs into. Its peerWeighing holds no state vector, so every struct counts as past the room's
// state, as every struct of a transaction's update is.
type writtenWeighing struct {
	content   renderedContent
	structure peerWeighing
	deleted   *deletedRuns
}

// walkedStruct weighs walked as the peer hook's weighing does, but for the apply-heap estimate
// (structApplyHeap), which wrote never reads: it reads the structure (structureHeap) alone, so a
// server write pays not even the estimate's arithmetic over what the walk counted in each struct's
// content (contentShape). It lists walked when it is a GC run, as applied's walk does, and only
// after weighing it, so the struct is charged against the list as it stood before this update.
func (w *writtenWeighing) walkedStruct(walked updateStruct) {
	w.content.walkedStruct(walked)
	w.structure.weighStruct(walked, applyCost{})
	if walked.kind == structGC {
		w.deleted.list(walked.client, walked.clock, walked.length)
	}
}

// walkedDelete weighs the range against the room's list and only then lists it, so the splits it
// can leave are charged against the list as it stood before this update's own delete set.
func (w *writtenWeighing) walkedDelete(client, clock, length uint64) {
	w.structure.walkedDelete(client, clock, length)
	w.deleted.list(client, clock, length)
}

// gaveBack takes structure back out of the room's structure part: what growWithin charged before
// an apply under pressure for an update whose apply then failed, which the room's bounded document
// took back out whole (newRoomScans), so the room holds none of what it was charged for. Nil
// gives back nothing.
func (c *roomCharge) gaveBack(structure int64) {
	c.charged(0, -structure)
}

// contentTaken is the content the room's transactions have added since it loaded, which a
// rendering published now holds, so its count, once it lands, re-bases the uncounted part from
// there (measured); and whether the uncounted part has passed earlyCountCharge, so the rendering is
// to be counted at once. Zero and false for a replica no room's load charged (a test's).
func (c *roomCharge) contentTaken() (content int64, early bool) {
	if c == nil {
		return 0, false
	}
	c.budget.mu.Lock()
	defer c.budget.mu.Unlock()
	return c.content, (c.content-c.measuredAt)*bytesOfHeapAByteAtTheDensest*roomCopies >= earlyCountCharge
}

// measured re-bases the room's counted part on heap (copyHeap), the price of a count of the
// rendering published when the room had added content bytes of content: the content added before
// that rendering is in the count, and only what was added since stays uncounted beside it. The
// structure part stays as it is. Only a count lands here (renderedReplica.storeCounted); a
// rendering whose bytes alone are published re-bases nothing. A replica no room's load charged has
// nothing to re-base.
func (c *roomCharge) measured(heap, content int64) {
	if c == nil {
		return
	}
	c.budget.mu.Lock()
	defer c.budget.unlock()
	c.heap = heap
	c.measuredAt = content
	c.budget.reweighLocked(c)
}

// deletes is the delete ranges the room is known to hold deleted (deletedRuns); nil for a nil
// charge, a room no load charged.
func (c *roomCharge) deletes() *deletedRuns {
	if c == nil {
		return nil
	}
	return &c.deleted
}

// contentAdded is the bytes of content update, one transaction's update as the room's update
// observer is handed it, added that a rendering can show: the content of each struct it carries,
// every one of them new to the room, since ygo encodes a transaction's update from the state
// before it (sjawhar/ygo v1.55.1-sami.1, collectAndEncodeLocked, crdt/doc.go:844-873), whoever
// wrote it - a browser's update the peer hook admitted, or the server's own write. Its delete
// set, what that transaction deleted (reearth/ygo#350), adds nothing and is not counted:
// charging the update's bytes, as the budget did, charged a heavily edited room its whole delete
// set again on every keystroke. So a whole-state handoff of a document the room already holds,
// which integrates nothing, adds nothing; and a struct parked until what it waits for arrives
// counts in the transaction that integrates it.
//
// What no rendering shows is left out: a struct directly under the margin, under a rebuilt
// history's retired clients (parentRetired) or under any other named root, a GC run or a skip, and
// JSON, binary, deleted and GC content, which the Proof schema renders nowhere. Every
// transaction's are charged in the structure part instead (peerWeighing.structureHeap): a
// browser's by the peer hook, the server's own by the update observer (roomCharge.wrote). Any
// content under an item or beside a neighbour may be an XML element's attribute, which renders,
// and is counted. An update the walk cannot read is counted whole, which is never less than its
// content. The room's own charges run this rule inside the one walk each of them makes (applied's
// appliedWalk, wrote's writtenWeighing, both over renderedContent); this is the rule itself, the
// one the tests of the content charge read.
func contentAdded(update []byte) int64 {
	var added renderedContent
	if _, err := walkUpdate(update, fromRoom, &added); err != nil {
		return int64(len(update))
	}
	return added.bytes
}

// renderedContent is contentAdded's visitor.
type renderedContent struct{ bytes int64 }

func (c *renderedContent) walkedStruct(walked updateStruct) {
	switch walked.parent {
	case parentMargin, parentRetired, parentOtherRoot, parentUnnamed:
		return
	}
	switch walked.kind {
	case structString, structEmbed, structFormat, structType, structAny:
		c.bytes += int64(len(walked.content))
	}
}

func (*renderedContent) walkedDelete(_, _, _ uint64) {}
