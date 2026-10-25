package docs

import (
	"weak"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// A browser's update reaches its room through no check of the server's own: applyLive weighs every
// write a caller makes by the document it leaves (growth.go), and a browser's update runs through
// none of it, so a browser can carry a document past the bound every server write obeys
// (growth.go:33-35). What stops that here is weighPeerUpdate, ygo's Server.OnPeerUpdate, which the
// room runs in place of its own apply for each update and whole-state message a read-write browser
// sends.
//
// It does not run applyLive's checks. Those weigh what a write leaves, which costs a fork of the
// document, a tree build and a full markdown render - about 420 ms and 123 MiB of heap for the fork
// alone at 1 MiB, with the render about 160 ms (AGENTS.md:531-568, :681-693) - against a keystroke
// on such a document that already takes between half a second and a second. The rule it keeps
// instead weighs the room's last published rendering (renderedReplica.publishSize) against the
// arriving update's own bytes, which is an atomic load and an addition, and only once that
// measurement is at or over the bound does it decode the update, work proportional to the update
// rather than to the document.
//
// The hook weighs bytes and not elements. Counting a rendering's elements is a whole goldmark
// parse, 58 to 89 ms at 524 KiB (BenchmarkElementCountAgainstRender), which no keystroke can pay on
// a one-processor pod: weighed in the hook, it took the keystroke benchmark's p50 up by 23 per cent
// against a stop condition of ten. No cheap exact count exists either: a render that counts what
// it writes agreed with the server's count over 1,127 documents, but took 1,404 lines emulating
// goldmark and still disagreed for a live tree whose rendering reads back as something else, which
// is what a browser holds. So the element bound is kept where it already is: every server write
// keeps its own refusal of a document past pmdoc.MaxDocumentElements (growth.go:139-150), so
// nothing Dispatch stores passes it. What a browser can leave live past it - about 64 KiB of
// headings - the server then refuses to write or rebuild until someone shortens it.
//
// The bound is not a ceiling the document cannot pass: the rendering the hook compares against is
// as recent as the room's last update observer, so a document passes it by the updates in flight
// when it crosses, and this is all that is claimed here.
//
// The hook has a third state, which is the room budget's rather than the document's: while the
// open rooms are past their budget and nothing can be closed (roomBudget.full), only an update
// that inserts nothing is admitted, in every room, and anything else is refused with the transient
// DocumentBusyCloseCode. Bytes cannot be that test. The hook no longer counts elements, and the
// elements are what the heap follows (bytesOfHeapAnElement): a mebibyte of markdown can make
// 786,432 of them (pmdoc/elements.go:16-29), so an update can leave a document as long as it was,
// or shorter, and still multiply what its room holds. What an update inserts is what can add an
// element, so an update that inserts nothing is the one update that cannot grow the room.
//
// That state therefore decodes every update a browser sends, which the other two do not pay for a
// keystroke under the bound. That is right: it runs only under pressure, while the budget is over
// and nothing can be closed, and the alternative to paying a decode there is the pod running out
// of memory. That refusal is not the document's own bound and is not folded into it: 4413 is
// permanent and the editor drops the refused edit, where 4503 is transient and the editor keeps
// it, to send once the pressure passes.

// maxPeerInsertAtTheBound is the most new content one update may carry into a document already at
// or over the bound: enough for the editor's own derived writes - heading ids, list labels, anchor
// marks - and for ordinary typing and deleting to carry on, and not enough for a paste. A document
// can therefore creep above the bound, and maxPeerByteCreep caps how far.
const maxPeerInsertAtTheBound = 8 << 10

// maxPeerByteCreep is how far past the bound a room's measurement may creep before only an update
// that inserts nothing at all is admitted, so a document that has crossed can still be trimmed or
// split and nothing can grow it further.
const maxPeerByteCreep = 64 << 10

// maxMessageBytes is the most one websocket message a browser sends may carry, ygo's
// Server.MaxMessageBytes, whose own default is 64 MiB (sjawhar/ygo v1.51.3-sami.2,
// provider/websocket/server.go:1017-1025). It is a floor against a client that is not the editor at
// all, never the bound on a document: the bound is weighPeerUpdate's, which turns away an oversized
// update before anything decodes it, so a frame at this floor costs the pod the frame itself and
// nothing more.
//
// The number is four times the largest frame the editor sends, which is a browser handing over the
// whole live state of the heaviest document the server's own writes can leave: its markdown at
// pmdoc.MaxDocumentBytes making pmdoc.MaxDocumentElements, its margin at maxMarginBytes and its
// anchor marks at maxAnchorBytes. That frame measured 5,360,879 bytes, well under the 16 MiB this
// design would have had to come back to the architect over - the markdown alone is 3,160,622 of it,
// since a document at the element bound carries 16,384 blocks and the block id of each - so the
// floor is four times the measured frame rounded up to the next mebibyte, 6 MiB, which is 24 MiB.
// TestTheHeaviestWholeStateFrameFitsTheMessageFloor builds that document and fails once its frame
// passes a quarter of this floor, since a frame over the floor is closed by the websocket library
// with a code the editor reconnects on, a loop no person's edit may ever reach.
const maxMessageBytes = 24 << 20

// weighPeerUpdate is ygo's Server.OnPeerUpdate: the room hands it every update and whole-state
// message a read-write browser sends, with the apply that would put it in the room, and a read-only
// browser never reaches it (sjawhar/ygo v1.51.3-sami.2, provider/websocket/peer_update.go:68-110).
// An update it does not apply is neither stored nor relayed to the room's other browsers, and the
// *websocket.PeerClose it refuses with closes that one browser with that code and reason.
//
// While the room's last measured rendering plus the update's own bytes stay under
// pmdoc.MaxDocumentBytes, the update is admitted on that comparison alone, which is what typing and
// deleting pay and which turns away a single huge frame before anything decodes it. At or over it,
// the update is decoded and admitted only when what it carries that the room does not already hold
// is under maxPeerInsertAtTheBound, or, past the creep cap, only when it carries nothing new at
// all; either refusal is the document's own, DocumentTooLargeCloseCode.
//
// While the room budget is full (roomBudget.full), the update is decoded in every room, and only
// one that carries nothing the room does not already hold is admitted: one that inserts anything
// is refused with DocumentBusyCloseCode, whatever it leaves the document's bytes at, since an
// insert of fewer bytes than it deletes can still add elements. An update does not say what it
// deletes without the document to measure it against, so an edit that replaces text with less of
// it is refused too, as the creep cap refuses it. The signal is one atomic load, so the peer's read
// loop takes neither the budget's lock nor a room's state lock for it.
//
// A room with no measurement at all is a document that left the Proof schema while it was live,
// which the socket path refuses to open in the first place (refuseOutsideSchema, websocket.go:175):
// the document's bound admits its updates, and the room's next render settles what it holds. An
// error the apply returns - an update whose bytes do not decode, or one that overflows the room's
// parked-item queue (MaxPendingItems) - is returned as it is, so ygo answers that browser
// SyncStatus(0) and leaves it connected, exactly as a room without this hook does.
func (s *Service) weighPeerUpdate(info websocket.PeerUpdateInfo, apply func() error) error {
	busy := s.budget.full.Load()
	bytes, measured := s.roomBytes(info.Doc)
	atTheBound := measured && bytes+int64(len(info.Update)) >= pmdoc.MaxDocumentBytes
	if !atTheBound && !busy {
		return apply()
	}
	inserted, err := insertedBeyond(info.Update, info.Doc.StateVector())
	if err != nil {
		// An update whose bytes do not decode is not a document grown too large, and the apply is
		// what answers it: it fails the same way, and the browser stays connected.
		return apply()
	}
	crept := bytes >= pmdoc.MaxDocumentBytes+maxPeerByteCreep
	if atTheBound && (inserted >= maxPeerInsertAtTheBound || (crept && inserted > 0)) {
		return &websocket.PeerClose{
			Code:   contracts.DocumentTooLargeCloseCode,
			Reason: contracts.DocumentTooLargeCloseReason,
		}
	}
	if busy && inserted > 0 {
		return &websocket.PeerClose{
			Code:   contracts.DocumentBusyCloseCode,
			Reason: contracts.DocumentBusyCloseReason,
		}
	}
	return apply()
}

// roomBytes is the bytes of the rendering the room holding live last published
// (renderedReplica.publishSize), and whether it has one. It is an atomic load off the listing the
// room's reads already go through (holdLive), so the peer's read loop takes neither the replica's
// lock, which every browser in the room waits behind (AGENTS.md:527-530), nor the room's state lock.
func (s *Service) roomBytes(live *crdt.Doc) (int64, bool) {
	listed, ok := s.replicas.Load(weak.Make(live))
	if !ok {
		return 0, false
	}
	replica := listed.(weak.Pointer[renderedReplica]).Value()
	if replica == nil {
		return 0, false
	}
	bytes := replica.bytes.Load()
	return bytes, bytes != sizeUnmeasured
}

// insertedBeyond is how much content update carries that a room at held does not already hold,
// counted in the units a Yjs item's content is measured in: a character of text, an element, an
// attribute an element gains. It is zero for an update that only deletes, which inserts no content,
// and zero for a whole-state message of a document the room already holds, which every browser
// sends on its first sync and which measured literally would be the whole document.
//
// crdt.ContentIDsFromUpdateV1 is one decode of the update that yields the clock runs it inserts
// (sjawhar/ygo v1.51.3-sami.2, crdt/contentmap.go:34-44), and what the room already holds is each
// client's clock in its state vector, so netting the two is arithmetic over those runs.
// crdt.DiffUpdateV1 (crdt/merge.go:289) re-encodes the update against that state vector instead,
// and is the more expensive of the two on the update a keystroke sends against a document at the
// bound: about 15.9 µs against 14.4 µs, 2,160 bytes in 34 allocations against 1,568 in 31
// (BenchmarkPeerUpdateInsertMeasures). It is also the wrong measure to take a length of, since its
// output carries the update's delete set, so a pure delete would not weigh zero.
func insertedBeyond(update []byte, held crdt.StateVector) (int64, error) {
	ids, err := crdt.ContentIDsFromUpdateV1(update)
	if err != nil {
		return 0, err
	}
	inserted := int64(0)
	for _, client := range ids.Inserts.Clients() {
		known := held[client]
		for _, run := range ids.Inserts.Ranges(client) {
			end := run.Clock + run.Len
			if end <= known {
				continue
			}
			inserted += int64(end - max(run.Clock, known))
		}
	}
	return inserted, nil
}
