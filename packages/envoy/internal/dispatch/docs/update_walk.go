package docs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unicode/utf8"
)

// walkUpdate reads a Yjs V1 update in one pass, without decoding it: for each struct it yields
// the struct's client, clock, length and kind, the encoded bytes of its content and where its
// parent is named, and for each range of the delete set its client, clock and length, and it
// allocates nothing per struct. It exists because ygo exports no count of the structs an update
// carries. crdt.ContentIDsFromUpdateV1 returns clock runs that IDSet.Add coalesces as it goes
// (sjawhar/ygo v1.51.3-sami.2, crdt/idset.go:83, merging at :21-28), so two structs and one
// struct of the same clocks are the same answer; and the decoder under it, decodeStructsV1, is
// unexported and allocates an Item, its content and its decoded values for every struct
// (crdt/merge.go:24-81), up to 1,048,576 of them for one frame. This repository cannot change
// ygo (its GitHub Apps have no write to sjawhar/ygo), so the walk lives here.
//
// It is ported from two places in ygo. The header, the struct loop and the delete set follow
// ygo's own allocation-free header scanner, pendingScanner (crdt/pending_budget.go:239-566),
// which walks the caller's buffer without materialising a value; each struct's fields and each
// content reference follow decodeItem (crdt/update.go:794-966), decodeContent (:968-1110) and
// decodeTypeContent (:1112-1177), which are what ygo's apply actually runs, so that the two read
// every byte the same way. That includes ContentJSON's rule (decodeContent's wireJSON case,
// :977-1008): a JSON item whose first value starts with a byte from 116 to 127, which is both a
// JSON text of that many bytes and a lib0 Any tag, is read as JSON text for every value when
// that succeeds and as lib0 Any for every value otherwise, so the walk ends that item where ygo
// does and cannot drift out of step with it. An Any value is skipped as
// internal/anycodec/decoder.go:255-393 skips it, which this package cannot import.
//
// It refuses, with an *updateRefusal wrapping one of two sentinels:
//
//   - errUpdateUndecodable, what ygo's V1 apply refuses (applyV1Txn, crdt/update.go:483-531): a
//     header of more clients than half the frame's bytes or than maxUpdateItems (:503); more
//     than maxUpdateItems structs in total, counted after each client's header as decodeAndPark
//     (:550-558) and decodeStructsV1 (crdt/merge.go:30-46) count them; a delete set of more than
//     maxUpdateItems clients, or of more than maxUpdateItems ranges for one client (:1185,
//     :1197); a varint past 53 bits or a length past the end of the frame, wherever it is; a
//     string, a root name or a map key that is not UTF-8; JSON that encoding/json would not
//     unmarshal into a float64-numbered value; an Any value anycodec would not read; and a
//     content reference ygo does not know.
//   - errUpdateNotFromEditor, what the Proof editor never sends, when the update comes from a
//     browser (fromPeer): a subdocument (ContentDoc, whose decode builds a whole crdt.Doc for
//     each struct, crdt/content.go:382-397); a move (ContentMove, wire tag 11, which is ygo's own
//     extension: Yjs has no content reference 11, its readItemContent indexing an eleven-entry
//     table and calling what is not there, yjs 13.6.32 src/structs/Item.js:705-724, so relaying
//     one would break every Yjs browser in the room); a struct whose parent is a named root other
//     than the document's fragment (fragmentName) and its margin (marksMapName), the only two
//     roots the editor writes (milkdown's CollabService.bindDoc takes
//     doc.getXmlFragment("prosemirror"), which y-prosemirror's sync plugin writes under;
//     packages/dispatch/web/src/features/doc/editor.ts takes doc.getMap("marks"); nothing else in
//     packages/dispatch/web or packages/proof-editor names a root or makes a subdocument); a
//     struct or a delete range ending past 2^53, which no JavaScript clock reaches, so that a
//     caller adding the walk's clocks cannot wrap where ygo's decodeAndPark adds lengths
//     unchecked (crdt/update.go:582-587); and bytes after the delete set, which no Yjs encoder
//     writes and ygo's apply ignores.
//
// Refusing is safe in both directions, and the tests hold it as a set relation over generated
// edits, whole states and the fuzz corpus. Every update the walk accepts, ygo applies to a fresh
// room (newDocumentCopy, whose pending queue of maxUpdateItems no update ygo decodes can
// overflow) without error, because the walk refuses every byte sequence ygo's decoder refuses; so
// the refusal closes the case where a frame of valid structs ending in one bad byte is
// half-applied (ApplyUpdateV1 integrates each struct as it decodes it, inside a transaction it
// never rolls back, crdt/update.go:50-56 and :561-669). And every update ygo applies without error
// that carries nothing the editor never sends, the walk accepts, so a browser running the editor
// is never refused by it.
//
// A walk of the room's own state (fromRoom: crdt.EncodeStateAsUpdateV1 of the live document,
// which carries parked structs, the skips before them and parked delete ranges, crdt/update.go:
// 69-91) refuses only what ygo's decoder refuses: what a stored document already holds, whatever
// wrote it, is counted rather than turned away.
//
// The visitor sees each struct and delete range as it is read, so a walk that ends in a refusal
// has already shown it the structs before the refusal; a caller discards what it gathered from a
// refused walk. visit may be nil when only the tally is wanted.
func walkUpdate(update []byte, source updateSource, visit updateVisitor) (updateTally, error) {
	w := updateWalker{buf: update, source: source}
	var tally updateTally
	tally.clients = w.uint()
	if w.refusal == nil && (tally.clients > uint64(len(update)/2) || tally.clients > maxUpdateItems) {
		w.undecodable("more clients than the frame can hold")
	}
	for range tally.clients {
		if w.refusal != nil {
			break
		}
		structs := w.uint()
		tally.structs += structs
		if w.refusal == nil && tally.structs > maxUpdateItems {
			w.undecodable("more structs than ygo decodes in one update")
		}
		id := w.uint()
		clock := w.uint()
		for range structs {
			if w.refusal != nil {
				break
			}
			walked := updateStruct{client: id, clock: clock}
			w.walkStruct(&walked)
			if w.refusal != nil {
				break
			}
			end := clock + walked.length
			if source == fromPeer && (end < clock || end > maxEditorClock) {
				w.notFromEditor("a clock past what a JavaScript client can write")
				break
			}
			if walked.kind == structSkip {
				tally.skips++
			}
			if visit != nil {
				visit.walkedStruct(walked)
			}
			clock = end
		}
	}
	if w.refusal == nil {
		tally.deleteClients = w.uint()
	}
	if w.refusal == nil && tally.deleteClients > maxUpdateItems {
		w.undecodable("a delete set of more clients than ygo decodes")
	}
	for range tally.deleteClients {
		if w.refusal != nil {
			break
		}
		id := w.uint()
		ranges := w.uint()
		if w.refusal == nil && ranges > maxUpdateItems {
			w.undecodable("more delete ranges for one client than ygo decodes")
		}
		for range ranges {
			if w.refusal != nil {
				break
			}
			clock := w.uint()
			length := w.uint()
			if w.refusal != nil {
				break
			}
			if end := clock + length; source == fromPeer && (end < clock || end > maxEditorClock) {
				w.notFromEditor("a deleted clock past what a JavaScript client can write")
				break
			}
			tally.deleteRanges++
			if visit != nil {
				visit.walkedDelete(id, clock, length)
			}
		}
	}
	if w.refusal == nil && source == fromPeer && w.pos != len(update) {
		w.notFromEditor("bytes after the delete set")
	}
	if w.refusal != nil {
		return tally, w.refusal
	}
	return tally, nil
}

// updateSource is who wrote the update a walk reads, which decides whether what the Proof editor
// never sends is refused.
type updateSource uint8

const (
	// fromPeer is an update a browser sent, refused when it carries what the editor never sends.
	fromPeer updateSource = iota
	// fromRoom is a room's own encoded state, of which only what ygo cannot decode is refused.
	fromRoom
)

// updateVisitor is handed each struct and each delete range of a walk in the order the update
// carries them.
type updateVisitor interface {
	walkedStruct(updateStruct)
	walkedDelete(client, clock, length uint64)
}

// updateTally is what a walk counts, all of it from the update's own headers.
type updateTally struct {
	// clients is the number of client groups of structs the update's header declares.
	clients uint64
	// structs is every struct the update carries, skips and zero-length structs included: the
	// number ygo checks against maxUpdateItems when it decodes the update.
	structs uint64
	// skips is how many of the structs are skips, clocks the update withholds.
	skips uint64
	// deleteClients is the number of clients the delete set declares.
	deleteClients uint64
	// deleteRanges is the number of ranges the delete set carries, as encoded: ygo sorts and merges
	// each client's ranges when it decodes them (crdt/update.go:1214-1216), so a decoded delete set
	// can hold fewer.
	deleteRanges uint64
}

// updateStruct is one struct of an update as walkUpdate reads it. Its byte slices alias the
// update, so a visitor that keeps one past the walk copies it.
type updateStruct struct {
	client, clock uint64
	// length is the clocks the struct spans: its content's length in Yjs's units (a UTF-16 code
	// unit of a string, a value of an Any or JSON item, the length of a GC, skip or deleted run,
	// and one for any other content).
	length uint64
	kind   structKind
	// parent is where the struct's parent is named; root is the root's name when it is a root.
	parent structParent
	root   []byte
	// keyed is whether the struct names a map key (parentSub): a map entry, or an attribute of an
	// XML element. A later entry for the same key carries an origin instead and names neither its
	// parent nor its key (Yjs typeMapSet; ygo's YMap.Set, crdt/ymap.go:220-225; crdt/update.go:
	// 909-922), so it reads as unkeyed here.
	keyed bool
	// content is the encoded content after the struct's header, length prefix included, which is
	// what decodeContent reads: empty for a GC run or a skip, which carry only their length.
	content []byte
}

// structKind is a struct's kind: the wire's content reference for an item, and the GC and skip
// structs, whose references are 0 and 10 (crdt/update.go:14-27).
type structKind uint8

const (
	structGC       structKind = 0
	structDeleted  structKind = 1
	structJSON     structKind = 2
	structBinary   structKind = 3
	structString   structKind = 4
	structEmbed    structKind = 5
	structFormat   structKind = 6
	structType     structKind = 7
	structAny      structKind = 8
	structDocument structKind = 9
	structSkip     structKind = 10
	structMove     structKind = 11
)

// structParent is where a struct names its parent.
type structParent uint8

const (
	// parentUnnamed is a GC run or a skip, which have no parent.
	parentUnnamed structParent = iota
	// parentOfNeighbour is an item with an origin or a right origin, whose parent is its
	// neighbour's, so the wire does not name it (crdt/update.go:835-858, :939-960).
	parentOfNeighbour
	// parentItem is an item inside a nested type, named by the ID of the item holding that type.
	// Which root it belongs to is the parent chain's, which the walk does not resolve.
	parentItem
	// parentFragment is an item directly under the document's fragment, fragmentName.
	parentFragment
	// parentMargin is an item directly under the document's margin, marksMapName.
	parentMargin
	// parentOtherRoot is an item under any other named root, which only a walk of fromRoom yields.
	parentOtherRoot
)

// maxEditorClock is 2^53, past which no JavaScript number counts every integer: no struct or
// delete range the editor writes ends beyond it.
const maxEditorClock = 1 << 53

// Item info bits (crdt/update.go:29-34).
const (
	infoHasOrigin      byte = 0x80
	infoHasRightOrigin byte = 0x40
	infoHasParentSub   byte = 0x20
)

// lib0 Any bounds anycodec enforces (internal/anycodec/decoder.go:230-241).
const (
	maxAnyDepth    = 100
	maxAnyElements = 100_000
)

var (
	// errUpdateUndecodable is a refusal of an update ygo's decoder refuses too.
	errUpdateUndecodable = errors.New("ygo's decoder refuses the update")
	// errUpdateNotFromEditor is a refusal of an update ygo would decode that carries what the Proof
	// editor never sends.
	errUpdateNotFromEditor = errors.New("the update carries what the Proof editor never sends")
)

// updateRefusal is walkUpdate's refusal: errors.Is matches it against errUpdateUndecodable or
// errUpdateNotFromEditor, which is which rule refused it.
type updateRefusal struct {
	rule error
	// at is the offset into the update at which the walk refused it.
	at     int
	reason string
}

func (r *updateRefusal) Error() string {
	return fmt.Sprintf("%v: %s at byte %d", r.rule, r.reason, r.at)
}

func (r *updateRefusal) Unwrap() error { return r.rule }

// updateWalker is walkUpdate's cursor. It keeps the first refusal and reads nothing after it.
type updateWalker struct {
	buf     []byte
	pos     int
	source  updateSource
	refusal *updateRefusal
}

func (w *updateWalker) undecodable(reason string) {
	if w.refusal == nil {
		w.refusal = &updateRefusal{rule: errUpdateUndecodable, at: w.pos, reason: reason}
	}
}

func (w *updateWalker) notFromEditor(reason string) {
	if w.refusal == nil {
		w.refusal = &updateRefusal{rule: errUpdateNotFromEditor, at: w.pos, reason: reason}
	}
}

// walkStruct reads one struct into walked, as decodeItem reads it (crdt/update.go:794-966).
func (w *updateWalker) walkStruct(walked *updateStruct) {
	info := w.byte()
	if w.refusal != nil {
		return
	}
	switch tag := info & 0x1f; tag {
	case byte(structGC), byte(structSkip):
		walked.kind = structKind(tag)
		walked.length = w.uint()
		return
	}
	hasOrigin := info&infoHasOrigin != 0
	hasRightOrigin := info&infoHasRightOrigin != 0
	if hasOrigin {
		w.uint()
		w.uint()
	}
	if hasRightOrigin {
		w.uint()
		w.uint()
	}
	walked.parent = parentOfNeighbour
	if !hasOrigin && !hasRightOrigin {
		if w.byte() == 1 {
			walked.root = w.varBytes()
			w.namedRoot(walked)
		} else {
			w.uint()
			w.uint()
			walked.parent = parentItem
		}
		if info&infoHasParentSub != 0 {
			w.str()
			walked.keyed = true
		}
	}
	if w.refusal != nil {
		return
	}
	start := w.pos
	walked.kind = structKind(info & 0x1f)
	walked.length = w.content(walked.kind)
	walked.content = w.buf[start:w.pos]
}

// namedRoot sorts walked's root into the fragment, the margin or another root, and refuses
// another root in an update from a browser.
func (w *updateWalker) namedRoot(walked *updateStruct) {
	if w.refusal != nil {
		return
	}
	switch string(walked.root) {
	case fragmentName:
		walked.parent = parentFragment
	case marksMapName:
		walked.parent = parentMargin
	default:
		if w.source == fromPeer {
			w.notFromEditor("a root the editor never writes")
			return
		}
		if !utf8.Valid(walked.root) {
			w.undecodable("a root name that is not UTF-8")
			return
		}
		walked.parent = parentOtherRoot
	}
}

// content reads the content of an item of kind and returns its length, as decodeContent reads it
// (crdt/update.go:968-1110).
func (w *updateWalker) content(kind structKind) uint64 {
	switch kind {
	case structDeleted:
		return w.uint()
	case structJSON:
		n := w.uint()
		if w.refusal != nil {
			return 0
		}
		if n > uint64(len(w.buf)-w.pos) {
			w.undecodable("more JSON values than bytes")
			return 0
		}
		if n == 0 || !isAnyTag(w.buf[w.pos]) {
			if !w.skipJSONValues(n) {
				w.undecodable("JSON that does not unmarshal")
			}
			return n
		}
		// ygo up to 1.51.0 wrote each value as lib0 Any, which always starts with a tag from 116
		// to 127, as does a JSON text of 116 to 127 bytes. One encoder writes a whole item, so
		// JSON text is tried for every value, and lib0 Any for every value only when that fails.
		start := w.pos
		if w.skipJSONValues(n) {
			return n
		}
		w.pos = start
		for range n {
			if !w.skipAny(0) {
				w.undecodable("JSON that is neither JSON text nor lib0 Any")
				return 0
			}
		}
		return n
	case structBinary:
		w.varBytes()
		return 1
	case structString:
		return w.str()
	case structEmbed:
		if !w.skipJSON() {
			w.undecodable("an embed that is not JSON")
		}
		return 1
	case structFormat:
		w.str()
		if w.refusal == nil && !w.skipJSON() {
			w.undecodable("a format value that is not JSON")
		}
		return 1
	case structType:
		switch w.byte() {
		case 3, 5: // an XML element's node name, an XML hook's name
			w.str()
		}
		return 1
	case structAny:
		n := w.uint()
		if w.refusal != nil {
			return 0
		}
		if n > uint64(len(w.buf)-w.pos) {
			w.undecodable("more Any values than bytes")
			return 0
		}
		for range n {
			if !w.skipAny(0) {
				w.undecodable("an Any value anycodec does not read")
				return 0
			}
		}
		return n
	case structDocument:
		if w.source == fromPeer {
			w.notFromEditor("a subdocument")
			return 0
		}
		w.varBytes()
		if w.refusal == nil && !w.skipAny(0) {
			w.undecodable("a subdocument's options anycodec does not read")
		}
		return 1
	case structMove:
		if w.source == fromPeer {
			w.notFromEditor("a move, which ygo alone writes")
			return 0
		}
		w.uint()
		w.uint()
		w.uint()
		return 1
	default:
		w.undecodable("an unknown content reference")
		return 0
	}
}

// byte reads one byte.
func (w *updateWalker) byte() byte {
	if w.refusal != nil {
		return 0
	}
	if w.pos >= len(w.buf) {
		w.undecodable("a truncated frame")
		return 0
	}
	b := w.buf[w.pos]
	w.pos++
	return b
}

// uint reads a lib0 varuint, refusing one past 53 bits as anycodec's ReadVarUint does.
func (w *updateWalker) uint() uint64 {
	if w.refusal != nil {
		return 0
	}
	v, ok := readVarUint(w.buf, &w.pos)
	if !ok {
		w.undecodable("a truncated or overlong varuint")
		return 0
	}
	return v
}

// varBytes reads a length-prefixed byte string.
func (w *updateWalker) varBytes() []byte {
	if w.refusal != nil {
		return nil
	}
	b, ok := readVarBytes(w.buf, &w.pos)
	if !ok {
		w.undecodable("a length past the end of the frame")
		return nil
	}
	return b
}

// str reads a length-prefixed UTF-8 string and returns its length in UTF-16 code units, which is
// a Yjs string's length in clocks (crdt/content.go:26-36).
func (w *updateWalker) str() uint64 {
	b := w.varBytes()
	if w.refusal != nil {
		return 0
	}
	if !utf8.Valid(b) {
		w.undecodable("a string that is not UTF-8")
		return 0
	}
	// In valid UTF-8 every rune starts with a byte that is not a continuation byte, and the runes
	// past U+FFFF, which take two UTF-16 code units, are exactly those of four bytes.
	var units uint64
	for _, c := range b {
		if c&0xc0 != 0x80 {
			units++
		}
		if c >= 0xf0 {
			units++
		}
	}
	return units
}

// skipJSONValues reads n JSON texts, as readJSONVals does (crdt/update.go:1311-1325), and reports
// whether every one would unmarshal. It records no refusal, since the legacy rule retries.
func (w *updateWalker) skipJSONValues(n uint64) bool {
	for range n {
		if !w.skipJSON() {
			return false
		}
	}
	return true
}

// skipJSON reads one JSON text and reports whether fmtValFromJSON would take it
// (crdt/update.go:1296-1306), recording no refusal.
func (w *updateWalker) skipJSON() bool {
	if w.refusal != nil {
		return false
	}
	raw, ok := readVarBytes(w.buf, &w.pos)
	return ok && jsonUnmarshals(raw)
}

// jsonUnmarshals reports whether fmtValFromJSON takes raw: "undefined", or UTF-8 that
// encoding/json unmarshals into an interface value, whose numbers must each fit a float64. It is
// ygo's own validatePendingJSON (crdt/pending_budget.go:535-566), which decides the legacy Any
// fallback for an ambiguous 116-127-byte text the way the decoder does.
func jsonUnmarshals(raw []byte) bool {
	if !utf8.Valid(raw) {
		return false
	}
	if bytes.Equal(raw, []byte("undefined")) {
		return true
	}
	if !json.Valid(raw) {
		return false
	}
	for j := 0; j < len(raw); j++ {
		switch c := raw[j]; {
		case c == '"':
			for j++; raw[j] != '"'; j++ {
				if raw[j] == '\\' {
					j++
				}
			}
		case c == '-' || c >= '0' && c <= '9':
			start := j
			for j < len(raw) && (raw[j] >= '0' && raw[j] <= '9' || raw[j] == '-' || raw[j] == '+' ||
				raw[j] == '.' || raw[j] == 'e' || raw[j] == 'E') {
				j++
			}
			if _, err := strconv.ParseFloat(string(raw[start:j]), 64); err != nil {
				return false
			}
			j--
		}
	}
	return true
}

// isAnyTag reports whether b is a lib0 Any tag (crdt/update.go:1308).
func isAnyTag(b byte) bool { return b >= 116 && b <= 127 }

// skipAny reads one lib0 Any value at depth as anycodec's readAny skips it
// (internal/anycodec/decoder.go:261-393) and reports whether it reads, recording no refusal.
func (w *updateWalker) skipAny(depth int) bool {
	if depth > maxAnyDepth || w.pos >= len(w.buf) {
		return false
	}
	tag := w.buf[w.pos]
	w.pos++
	switch tag {
	case 127, 126, 120, 121: // undefined, null, true, false
		return true
	case 125: // integer
		return readVarInt(w.buf, &w.pos)
	case 124: // float32
		return w.skipFixed(4)
	case 123, 122: // float64, bigint64
		return w.skipFixed(8)
	case 119: // string
		b, ok := readVarBytes(w.buf, &w.pos)
		return ok && utf8.Valid(b)
	case 116: // binary
		_, ok := readVarBytes(w.buf, &w.pos)
		return ok
	case 117, 118: // array, object
		n, ok := readVarUint(w.buf, &w.pos)
		if !ok || n > maxAnyElements || n > uint64(len(w.buf)-w.pos) {
			return false
		}
		for range n {
			if tag == 118 {
				key, ok := readVarBytes(w.buf, &w.pos)
				if !ok || !utf8.Valid(key) {
					return false
				}
			}
			if !w.skipAny(depth + 1) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func (w *updateWalker) skipFixed(n int) bool {
	if len(w.buf)-w.pos < n {
		return false
	}
	w.pos += n
	return true
}

// readVarUint reads a lib0 varuint at *pos, as anycodec's ReadVarUint does: one whose eighth byte
// continues is past 53 bits and refused (internal/anycodec/decoder.go:89-107).
func readVarUint(buf []byte, pos *int) (uint64, bool) {
	var v uint64
	for shift := uint(0); ; shift += 7 {
		if *pos >= len(buf) {
			return 0, false
		}
		b := buf[*pos]
		*pos++
		v |= uint64(b&0x7f) << shift
		if b&0x80 == 0 {
			return v, true
		}
		if shift+7 > 49 {
			return 0, false
		}
	}
}

// readVarInt skips a lib0 sign-magnitude varint at *pos, as anycodec's ReadVarInt reads it
// (internal/anycodec/decoder.go:110-136).
func readVarInt(buf []byte, pos *int) bool {
	if *pos >= len(buf) {
		return false
	}
	b := buf[*pos]
	*pos++
	for shift := uint(6); b&0x80 != 0; shift += 7 {
		if shift > 48 || *pos >= len(buf) {
			return false
		}
		b = buf[*pos]
		*pos++
	}
	return true
}

// readVarBytes reads a length-prefixed byte string at *pos, refusing a length past the end of buf
// as anycodec's ReadVarBytes does (internal/anycodec/decoder.go:162-175).
func readVarBytes(buf []byte, pos *int) ([]byte, bool) {
	n, ok := readVarUint(buf, pos)
	if !ok || n > uint64(len(buf)-*pos) {
		return nil, false
	}
	b := buf[*pos : *pos+int(n)]
	*pos += int(n)
	return b, true
}
