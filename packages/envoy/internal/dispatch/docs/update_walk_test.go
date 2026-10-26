package docs

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	_ "unsafe" // ygoDecodeStructsV1's go:linkname

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"
)

// ygoDecodeStructsV1 is ygo's own V1 struct decoder (sjawhar/ygo v1.51.3-sami.5,
// crdt/merge.go:24-81), unexported, reached here as the independent count the walk is checked
// against: it decodes every struct of an update into an Item, in the update's order for each
// client, through the same decodeItem the room's apply runs, and drops only skips. ygo exports
// nothing that counts structs (crdt.ContentIDsFromUpdateV1 coalesces them into runs), which is why
// the walk exists at all.
//
//go:linkname ygoDecodeStructsV1 github.com/reearth/ygo/crdt.decodeStructsV1
func ygoDecodeStructsV1(scratch *crdt.Doc, update []byte) (map[crdt.ClientID][]*crdt.Item, crdt.DeleteSet, error)

// The walk is held to ygo struct by struct, over updates ygo wrote of every content kind and
// frames built byte for byte for the kinds ygo's API cannot write: each client's structs carry
// the clocks, lengths, kinds and map keys ygo's own decoder reads, the skips fill exactly the gaps
// ygo's decoder leaves between them, the count is ygo's structs and skips, and the runs and
// deletes, coalesced, are crdt.ContentIDsFromUpdateV1's.
func TestTheUpdateWalkCountsWhatYgoEncodes(t *testing.T) {
	seen := map[structKind]bool{}
	keyed := false
	for _, made := range ygoMadeUpdates(t) {
		t.Run(made.name, func(t *testing.T) {
			walked, tally, err := walkRecorded(made.update, fromRoom)
			if err != nil {
				t.Fatalf("the walk refused an update ygo wrote: %v", err)
			}
			items, _, err := ygoDecodeStructsV1(crdt.New(), made.update)
			if err != nil {
				t.Fatalf("ygo's decoder refused its own update: %v", err)
			}
			walkedItems := map[crdt.ClientID][]updateStruct{}
			skips := map[crdt.ClientID][]crdt.IDRange{}
			for _, s := range walked.structs {
				seen[s.kind] = true
				keyed = keyed || s.keyed
				client := crdt.ClientID(s.client)
				if s.kind == structSkip {
					skips[client] = append(skips[client], crdt.IDRange{Clock: s.clock, Len: s.length})
					continue
				}
				walkedItems[client] = append(walkedItems[client], s)
			}
			decoded := uint64(0)
			for client, list := range items {
				decoded += uint64(len(list))
				got := walkedItems[client]
				if len(got) != len(list) {
					t.Fatalf("client %d: the walk read %d structs that are not skips, ygo decoded %d", client, len(got), len(list))
				}
				var gaps []crdt.IDRange
				for i, item := range list {
					want := updateStruct{
						client: uint64(client),
						clock:  item.ID.Clock,
						length: uint64(item.Content.Len()),
						kind:   kindOfItem(item),
						keyed:  item.ParentSub != nil,
					}
					s := got[i]
					if s.clock != want.clock || s.length != want.length || s.kind != want.kind || s.keyed != want.keyed {
						t.Fatalf("client %d struct %d: walked clock %d length %d kind %d keyed %t, ygo decoded clock %d length %d kind %d keyed %t",
							client, i, s.clock, s.length, s.kind, s.keyed, want.clock, want.length, want.kind, want.keyed)
					}
					if i > 0 {
						if end := list[i-1].ID.Clock + uint64(list[i-1].Content.Len()); item.ID.Clock > end {
							gaps = append(gaps, crdt.IDRange{Clock: end, Len: item.ID.Clock - end})
						}
					}
				}
				if !slices.Equal(coalesced(skips[client]), gaps) {
					t.Fatalf("client %d: the walk's skips cover %v, ygo's decoder leaves gaps %v", client, coalesced(skips[client]), gaps)
				}
			}
			if len(walkedItems) > len(items) {
				t.Fatalf("the walk read structs of %d clients, ygo decoded %d", len(walkedItems), len(items))
			}
			if tally.structs != decoded+tally.skips {
				t.Fatalf("the walk counted %d structs, ygo decoded %d and %d skips", tally.structs, decoded, tally.skips)
			}
			if made.structs != 0 && tally.structs != made.structs {
				t.Fatalf("the walk counted %d structs in a frame built with %d", tally.structs, made.structs)
			}
			ids, err := crdt.ContentIDsFromUpdateV1(made.update)
			if err != nil {
				t.Fatal(err)
			}
			inserts, deletes := crdt.NewIDSet(), crdt.NewIDSet()
			for _, s := range walked.structs {
				if s.kind != structSkip {
					inserts.Add(crdt.ClientID(s.client), s.clock, s.length)
				}
			}
			for _, d := range walked.deletes {
				deletes.Add(crdt.ClientID(d.client), d.clock, d.length)
			}
			if !sameIDSet(inserts, ids.Inserts) {
				t.Fatalf("the walk's runs are not ygo's inserts")
			}
			if !sameIDSet(deletes, ids.Deletes) {
				t.Fatalf("the walk's delete ranges are not ygo's deletes")
			}
			if uint64(len(walked.deletes)) != tally.deleteRanges {
				t.Fatalf("the walk showed %d delete ranges and counted %d", len(walked.deletes), tally.deleteRanges)
			}
		})
	}
	for _, kind := range []structKind{structGC, structDeleted, structJSON, structBinary, structString, structEmbed,
		structFormat, structType, structAny, structDocument, structSkip, structMove} {
		if !seen[kind] {
			t.Errorf("no update of the corpus carried a struct of kind %d", kind)
		}
	}
	if !keyed {
		t.Error("no update of the corpus carried a map entry")
	}
}

// The walk names each struct's parent and hands over its content's encoded bytes, which is what
// the hook charges by: a struct directly under the fragment or the margin says so, a nested one
// names an item, one with an origin names its neighbour, and the content slice is the bytes ygo's
// content decoder reads.
func TestTheUpdateWalkNamesParentsAndContent(t *testing.T) {
	update := newV1Frame(1).group(5, 9, 0).
		rooted(4, fragmentName, "").str("héllo🙂").
		rooted(8, marksMapName, "c-1").uint(1).any(map[string]any{"kind": "comment"}).
		raw(0x84).uint(9).uint(0).str("x").
		raw(4, 0).uint(9).uint(0).str("in").
		gc(3).
		noDeletes()
	walked, tally, err := walkRecorded(update, fromPeer)
	if err != nil {
		t.Fatal(err)
	}
	anyContent := encoding.NewEncoder()
	anyContent.WriteVarUint(1)
	anyContent.WriteAny(map[string]any{"kind": "comment"})
	want := []updateStruct{
		// Seven UTF-16 code units over ten bytes: the emoji is two units and four bytes.
		{client: 9, clock: 0, length: 7, kind: structString, parent: parentFragment, root: []byte(fragmentName), content: append([]byte{10}, "héllo🙂"...)},
		{client: 9, clock: 7, length: 1, kind: structAny, parent: parentMargin, root: []byte(marksMapName), keyed: true, key: append([]byte{3}, "c-1"...), content: anyContent.Bytes()},
		{client: 9, clock: 8, length: 1, kind: structString, parent: parentOfNeighbour, content: []byte{1, 'x'}},
		{client: 9, clock: 9, length: 2, kind: structString, parent: parentItem, content: []byte{2, 'i', 'n'}},
		{client: 9, clock: 11, length: 3, kind: structGC},
	}
	if tally.structs != uint64(len(want)) || len(walked.structs) != len(want) {
		t.Fatalf("the walk counted %d structs and showed %d, want %d", tally.structs, len(walked.structs), len(want))
	}
	for i, s := range walked.structs {
		w := want[i]
		if s.client != w.client || s.clock != w.clock || s.length != w.length || s.kind != w.kind || s.parent != w.parent ||
			s.keyed != w.keyed || string(s.key) != string(w.key) || string(s.root) != string(w.root) || string(s.content) != string(w.content) {
			t.Errorf("struct %d: walked %+v, want %+v", i, s, w)
		}
	}
}

// What the walk accepts and what a room takes are the same set but for what the editor never
// sends, over every update a run of random editing on the fragment and the margin by three
// browsers sends - each transaction, each sync between them, each whole state, merges of runs of
// updates and states holding parked structs - and over refused kinds built for the purpose. Every
// update the walk accepts, a fresh room applies without error, so a refusal is never the only
// thing between a room and a half-applied frame; every update a room takes that carries nothing
// the editor never sends, the walk accepts, so the editor is never refused by it.
func TestTheUpdateWalkAcceptsEveryUpdateARoomTakes(t *testing.T) {
	corpus := editorCorpus(t)
	if len(corpus) < 500 {
		t.Fatalf("the editing run made %d updates, want a corpus of at least 500", len(corpus))
	}
	for i, update := range corpus {
		if err := updateWalkSetRelation(update); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
		if _, err := walkUpdate(update, fromPeer, nil); err != nil {
			t.Fatalf("update %d, which only the editor's roots and kinds make: %v", i, err)
		}
	}
	for _, made := range ygoMadeUpdates(t) {
		if err := updateWalkSetRelation(made.update); err != nil {
			t.Fatalf("%s: %v", made.name, err)
		}
	}
	for i, seed := range updateWalkSeeds(t) {
		if err := updateWalkSetRelation(seed); err != nil {
			t.Fatalf("fuzz seed %d: %v", i, err)
		}
	}
}

// A frame of valid structs ending in one bad byte is refused whole. ygo's apply refuses it too,
// but only after it has integrated every struct before the bad byte (crdt/update.go:68-74), which
// on a document that sets no integrate-scan bound, as newDocumentCopy's does not, stay in it, and
// which a room's bounded document takes back out only once it has integrated them all: the work
// the hook's walk keeps off the room.
func TestTheUpdateWalkRefusesValidStructsBeforeOneBadByte(t *testing.T) {
	update := newV1Frame(1).group(4, 7, 0).gc(2).gc(2).gc(2).raw(0xff).done()
	_, err := walkUpdate(update, fromPeer, nil)
	if !errors.Is(err, errUpdateUndecodable) {
		t.Fatalf("the walk answered %v, want a refusal ygo's decoder shares", err)
	}
	room := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(room, update, nil); err == nil {
		t.Fatal("ygo applied the frame without error")
	}
	if got := room.StateVector()[7]; got != 6 {
		t.Fatalf("ygo's failed apply left client 7 at clock %d, want the 6 its three valid structs integrated", got)
	}
}

// Each refusal ygo's decoder makes, the walk makes, and ygo's apply to a fresh room fails on the
// same bytes; a walk of the room's own state refuses each as undecodable.
func TestTheUpdateWalkRefusesWhatYgoRefuses(t *testing.T) {
	// A value nested in maxAnyDepth+1 arrays sits past the depth anycodec reads; an array of
	// maxAnyElements+1 values is wider than it reads.
	deepAny := varuint(1)
	for range maxAnyDepth + 1 {
		deepAny = append(deepAny, 117, 1)
	}
	deepAny = append(deepAny, 126)
	wideAny := append(append(varuint(1), 117), varuint(maxAnyElements+1)...)
	wideAny = append(wideAny, slices.Repeat([]byte{126}, maxAnyElements+1)...)
	for _, row := range []struct {
		name   string
		update []byte
	}{
		{"an empty frame", nil},
		{"more clients than half the frame", []byte{3, 0}},
		{"more structs in one client than ygo decodes", newV1Frame(1).uint(maxUpdateItems + 1).done()},
		{"more structs across clients than ygo decodes", newV1Frame(2).group(1, 1, 0).gc(1).uint(maxUpdateItems).done()},
		{"a delete set of more clients than ygo decodes", append([]byte{0}, varuint(maxUpdateItems+1)...)},
		{"more delete ranges for one client than ygo decodes", append([]byte{0, 1, 1}, varuint(maxUpdateItems+1)...)},
		{"a varuint past 53 bits", []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01}},
		{"a truncated delete set", newV1Frame(1).group(1, 1, 0).gc(1).uint(1).uint(1).uint(2).uint(0).done()},
		{"a string past the frame", newV1Frame(1).group(1, 1, 0).rooted(4, fragmentName, "").uint(100).raw('a').done()},
		{"a string that is not UTF-8", newV1Frame(1).group(1, 1, 0).rooted(4, fragmentName, "").uint(1).raw(0xff).noDeletes()},
		{"a root name that is not UTF-8", newV1Frame(1).group(1, 1, 0).raw(4, 1, 1, 0xff).str("x").noDeletes()},
		{"a map key that is not UTF-8", newV1Frame(1).group(1, 1, 0).raw(8|0x20, 1).str(marksMapName).raw(1, 0xff).uint(1).raw(126).noDeletes()},
		{"an unknown content reference", newV1Frame(1).group(1, 1, 0).rooted(12, fragmentName, "").noDeletes()},
		{"JSON that is not JSON", newV1Frame(1).group(1, 1, 0).rooted(2, marksMapName, "k").uint(1).str("{nope").noDeletes()},
		{"a JSON number past a float64", newV1Frame(1).group(1, 1, 0).rooted(2, marksMapName, "k").uint(1).str("1e400").noDeletes()},
		{"more JSON values than bytes", newV1Frame(1).group(1, 1, 0).rooted(2, marksMapName, "k").uint(50).done()},
		{"JSON that is neither JSON text nor lib0 Any", newV1Frame(1).group(1, 1, 0).rooted(2, marksMapName, "k").uint(1).raw(119, 5, 'a').noDeletes()},
		{"an embed that is not JSON", newV1Frame(1).group(1, 1, 0).rooted(5, fragmentName, "").str("{").noDeletes()},
		{"a format value that is not JSON", newV1Frame(1).group(1, 1, 0).rooted(6, fragmentName, "").str("bold").str("tru").noDeletes()},
		{"an XML element name that is not UTF-8", newV1Frame(1).group(1, 1, 0).rooted(7, fragmentName, "").raw(3, 1, 0xff).noDeletes()},
		{"an unknown Any tag", newV1Frame(1).group(1, 1, 0).rooted(8, marksMapName, "k").uint(1).raw(115).noDeletes()},
		{"an Any deeper than anycodec reads", newV1Frame(1).group(1, 1, 0).rooted(8, marksMapName, "k").raw(deepAny...).noDeletes()},
		{"an Any wider than anycodec reads", newV1Frame(1).group(1, 1, 0).rooted(8, marksMapName, "k").raw(wideAny...).noDeletes()},
		{"more Any values than bytes", newV1Frame(1).group(1, 1, 0).rooted(8, marksMapName, "k").uint(9).raw(126).done()},
		{"valid GC structs then one bad byte", newV1Frame(1).group(4, 7, 0).gc(2).gc(2).gc(2).raw(0xff).done()},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := walkUpdate(row.update, fromRoom, nil); !errors.Is(err, errUpdateUndecodable) {
				t.Fatalf("a walk of a room's state answered %v, want a refusal ygo's decoder shares", err)
			}
			if _, err := walkUpdate(row.update, fromPeer, nil); err == nil {
				t.Fatal("a walk of a browser's update accepted it")
			}
			if err := crdt.ApplyUpdateV1(newDocumentCopy(), row.update, nil); err == nil {
				t.Fatal("ygo applied it without error, so the walk refuses what ygo takes")
			}
		})
	}
}

// What the editor never sends is refused from a browser and only from a browser: ygo applies each
// to a fresh room without error, and a walk of a room's own state counts it.
func TestTheUpdateWalkRefusesWhatTheEditorNeverSends(t *testing.T) {
	array := newDocumentCopy(crdt.WithClientID(3))
	list := array.GetArray("a")
	array.Transact(func(txn *crdt.Transaction) { list.Push(txn, []any{"a", "b", "c"}) })
	var move []byte
	stop := array.OnUpdate(func(update []byte, _ any) { move = slices.Clone(update) })
	array.Transact(func(txn *crdt.Transaction) { list.Move(txn, 0, 2) })
	stop()
	keystroke := newV1Frame(1).group(1, 4, 0).rooted(4, fragmentName, "").str("y").noDeletes()
	for _, row := range []struct {
		name   string
		update []byte
	}{
		{"another root", newV1Frame(1).group(1, 5, 0).rooted(4, "t", "").str("hi").noDeletes()},
		{"a subdocument", newV1Frame(1).group(1, 5, 0).rooted(9, marksMapName, "sub").str("guid").any(map[string]any{"autoLoad": false}).noDeletes()},
		{"a move", move},
		{"a struct ending past 2^53", newV1Frame(1).group(1, 5, maxEditorClock).gc(1).noDeletes()},
		{"a delete range ending past 2^53", append(append([]byte{0, 1, 5, 1}, varuint(maxEditorClock)...), 1)},
		{"a byte after the delete set", append(slices.Clone(keystroke), 0)},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := walkUpdate(row.update, fromPeer, nil); !errors.Is(err, errUpdateNotFromEditor) {
				t.Fatalf("a walk of a browser's update answered %v, want a refusal of what the editor never sends", err)
			}
			if _, err := walkUpdate(row.update, fromRoom, nil); err != nil {
				t.Fatalf("a walk of a room's state refused it: %v", err)
			}
			if err := crdt.ApplyUpdateV1(newDocumentCopy(), row.update, nil); err != nil {
				t.Fatalf("ygo refused it (%v), so it is no refusal of the editor's", err)
			}
		})
	}
	if _, err := walkUpdate(keystroke, fromPeer, nil); err != nil {
		t.Fatalf("the walk refused the keystroke the trailing-byte row extends: %v", err)
	}
}

// ygo's bounds hold at their edges: 1,048,576 structs in one update and 1,048,576 delete ranges
// for one client are walked, and one more of either is refused.
func TestTheUpdateWalkHoldsYgosBoundsAtTheirEdges(t *testing.T) {
	for _, row := range []struct {
		name    string
		update  func(n int) []byte
		counted func(updateTally) uint64
	}{
		{
			name: "structs",
			update: func(n int) []byte {
				f := newV1Frame(2).group(1, 1, 0).gc(1).group(n-1, 2, 0)
				for range n - 1 {
					f.gc(1)
				}
				return f.noDeletes()
			},
			counted: func(tally updateTally) uint64 { return tally.structs },
		},
		{
			name: "delete ranges for one client",
			update: func(n int) []byte {
				f := newV1Frame(0).uint(1).uint(1).uint(uint64(n))
				for clock := range n {
					f.uint(uint64(2 * clock)).uint(1)
				}
				return f.done()
			},
			counted: func(tally updateTally) uint64 { return tally.deleteRanges },
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			tally, err := walkUpdate(row.update(maxUpdateItems), fromPeer, nil)
			if err != nil {
				t.Fatalf("the walk refused %d: %v", maxUpdateItems, err)
			}
			if got := row.counted(tally); got != maxUpdateItems {
				t.Fatalf("the walk counted %d, want %d", got, maxUpdateItems)
			}
			if _, err := walkUpdate(row.update(maxUpdateItems+1), fromPeer, nil); !errors.Is(err, errUpdateUndecodable) {
				t.Fatalf("the walk answered %v to one more than ygo decodes", err)
			}
		})
	}
}

// ContentJSON's two encodings are read where ygo's decoder reads them. A JSON text of 116 to 127
// bytes starts with a byte that is also a lib0 Any tag; ygo tries JSON text for every value and
// lib0 Any only when that fails (crdt/update.go:1000-1031), and a walk that chose otherwise would
// end the item somewhere else and misread every struct after it. Each case is followed by a struct
// the walk must still read as ygo does.
func TestTheUpdateWalkReadsJSONWhereYgoDoes(t *testing.T) {
	type jsonCase struct {
		name    string
		content []byte
		values  uint64
	}
	var cases []jsonCase
	for size := 116; size <= 127; size++ {
		text := `"` + strings.Repeat("j", size-2) + `"`
		cases = append(cases, jsonCase{fmt.Sprintf("a %d-byte JSON text", size), append(varuint(1), varstring(text)...), 1})
	}
	legacy := encoding.NewEncoder()
	legacy.WriteVarUint(2)
	legacy.WriteAny(map[string]any{"a": 1.5, "b": []any{true, "x"}})
	legacy.WriteAny("plain")
	cases = append(cases, jsonCase{"two legacy lib0 Any values", legacy.Bytes(), 2})
	// Both readings take these bytes: as JSON text, a 119-byte string; as lib0 Any, a string of 34
	// bytes (the text's opening quote). JSON text wins, and ends 120 bytes on.
	both := `"` + strings.Repeat("b", 117) + `"`
	cases = append(cases, jsonCase{"bytes both readings take", append(varuint(1), varstring(both)...), 1})
	// Only lib0 Any takes these: 119 is a JSON length past the frame, and a string tag before "hi".
	cases = append(cases, jsonCase{"bytes only lib0 Any takes", []byte{1, 119, 2, 'h', 'i'}, 1})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			update := newV1Frame(1).group(2, 6, 0).rooted(2, marksMapName, "k").raw(c.content...).
				rooted(4, fragmentName, "").str("after").noDeletes()
			walked, _, err := walkRecorded(update, fromPeer)
			if err != nil {
				t.Fatalf("the walk refused it: %v", err)
			}
			if len(walked.structs) != 2 || walked.structs[0].length != c.values || string(walked.structs[0].content) != string(c.content) {
				t.Fatalf("the walk read %+v, want a JSON item of %d values over %d bytes", walked.structs, c.values, len(c.content))
			}
			if s := walked.structs[1]; s.kind != structString || s.clock != c.values || s.length != 5 {
				t.Fatalf("the struct after it was read as %+v", s)
			}
			if err := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil); err != nil {
				t.Fatalf("ygo refused it: %v", err)
			}
			items, _, err := ygoDecodeStructsV1(crdt.New(), update)
			if err != nil || len(items[6]) != 2 || uint64(items[6][1].ID.Clock) != c.values {
				t.Fatalf("ygo's decoder read %v (%v)", items[6], err)
			}
		})
	}
}

// The walk allocates nothing for each struct: a frame of 500,000 GC structs, the shape of the
// cheapest flood a client that is not the editor can send, and the whole state of a 1 MiB document
// are each walked in the same few allocations, whatever their struct counts.
func TestTheUpdateWalkAllocatesNothingPerStruct(t *testing.T) {
	for _, row := range []struct {
		name   string
		update []byte
	}{
		{"a frame of 500,000 GC structs", gcFrame(500_000)},
		{"the whole state of a 1 MiB document", crdt.EncodeStateAsUpdateV1(residentRoomDocument(t, 1<<20), nil)},
	} {
		t.Run(row.name, func(t *testing.T) {
			var counted walkCounter
			tally, err := walkUpdate(row.update, fromPeer, &counted)
			if err != nil {
				t.Fatal(err)
			}
			if tally.structs < 50_000 {
				t.Fatalf("the update carries %d structs, too few to show a per-struct allocation", tally.structs)
			}
			allocs := testing.AllocsPerRun(5, func() {
				if _, err := walkUpdate(row.update, fromPeer, &counted); err != nil {
					t.Fatal(err)
				}
			})
			if allocs > 2 {
				t.Fatalf("a walk of %d structs made %.0f allocations, want at most 2", tally.structs, allocs)
			}
		})
	}
}

// BenchmarkUpdateWalk measures the walk on the update a keystroke sends against a 524 KiB room,
// which every keystroke will pay once the hook walks every update, on the whole state of a 1 MiB
// document, which a browser hands over on its first sync and which a room's exact struct count
// walks, and on a frame of 500,000 GC structs.
func BenchmarkUpdateWalk(b *testing.B) {
	keystroke := benchmarkKeystrokeUpdate(b, residentRoomDocument(b, 524<<10))
	whole := crdt.EncodeStateAsUpdateV1(residentRoomDocument(b, 1<<20), nil)
	for _, row := range []struct {
		name   string
		update []byte
	}{
		{"keystroke-524KiB", keystroke},
		{"whole-state-1MiB", whole},
		{"gc-frame-500000", gcFrame(500_000)},
	} {
		b.Run(row.name, func(b *testing.B) {
			var counted walkCounter
			b.ReportAllocs()
			b.SetBytes(int64(len(row.update)))
			for b.Loop() {
				if _, err := walkUpdate(row.update, fromPeer, &counted); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// FuzzUpdateWalk holds the set relation on whatever bytes the fuzzer makes: the walk never panics,
// what it accepts a fresh room applies, and what a fresh room applies it accepts unless it refuses
// it as something the editor never sends. Its seeds are the edges a reviewer named: JSON texts of
// 116 to 127 bytes, nested Any, a subdocument, GC and skip runs, a run of GC structs before an
// unknown content reference, and truncations of each; the seed corpus runs as an ordinary test.
func FuzzUpdateWalk(f *testing.F) {
	for _, seed := range updateWalkSeeds(f) {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, update []byte) {
		if err := updateWalkSetRelation(update); err != nil {
			t.Fatal(err)
		}
	})
}

// updateWalkSetRelation is nil when the walk of a browser's update and a fresh room's apply agree
// on update: the walk accepts nothing the room refuses, and refuses nothing the room takes except
// as something the editor never sends.
func updateWalkSetRelation(update []byte) error {
	_, walkErr := walkUpdate(update, fromPeer, nil)
	applyErr := crdt.ApplyUpdateV1(newDocumentCopy(), update, nil)
	switch {
	case walkErr == nil && applyErr != nil:
		return fmt.Errorf("the walk accepted an update a fresh room refuses: %v", applyErr)
	case walkErr != nil && applyErr == nil && !errors.Is(walkErr, errUpdateNotFromEditor):
		return fmt.Errorf("the walk refused an update a fresh room takes, as other than what the editor never sends: %v", walkErr)
	}
	if _, err := walkUpdate(update, fromRoom, nil); applyErr == nil && err != nil {
		return fmt.Errorf("a walk of a room's state refused an update a fresh room takes: %v", err)
	}
	return nil
}

type madeUpdate struct {
	name   string
	update []byte
	// structs is the struct count a frame built by hand was built with, zero for one ygo wrote.
	structs uint64
}

// ygoMadeUpdates is ygo's own encoding of every content kind: text with formats and embeds,
// deleted and garbage-collected runs, XML elements and texts, maps with nested types, a
// subdocument and binary values, an array with a move, JSON items in both encodings, states that
// hold parked structs behind skips, merges with gaps, and each transaction's update with its
// delete set. Where ygo's API cannot write a kind (JSON, binary, GC runs), a frame built by hand
// is applied to a document first and ygo's encoding of that document is the update.
func ygoMadeUpdates(tb testing.TB) []madeUpdate {
	tb.Helper()
	var made []madeUpdate
	record := func(name string, doc *crdt.Doc) func() {
		return doc.OnUpdate(func(update []byte, _ any) {
			made = append(made, madeUpdate{name: fmt.Sprintf("%s transaction %d", name, len(made)), update: slices.Clone(update)})
		})
	}
	state := func(name string, doc *crdt.Doc) {
		made = append(made, madeUpdate{name: name + " whole state", update: crdt.EncodeStateAsUpdateV1(doc, nil)})
	}

	editor := newDocumentCopy(crdt.WithClientID(11))
	stop := record("editor", editor)
	fragment := editor.GetXmlFragment(fragmentName)
	marks := editor.GetMap(marksMapName)
	var text *crdt.YXmlText
	editor.Transact(func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		fragment.InsertElement(txn, 0, paragraph)
		paragraph.SetAttribute(txn, "id", "b-1")
		text = crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, text)
		text.Insert(txn, 0, "héllo wörld 🙂 and more", nil)
	})
	editor.Transact(func(txn *crdt.Transaction) { text.Format(txn, 0, 5, crdt.Attributes{"bold": true}) })
	editor.Transact(func(txn *crdt.Transaction) { text.InsertEmbed(txn, 3, map[string]any{"image": "x.png"}, nil) })
	editor.Transact(func(txn *crdt.Transaction) {
		marks.Set(txn, "c-1", map[string]any{"kind": "comment", "quote": "héllo", "at": 1.5})
		marks.Set(txn, "c-2", "plain")
	})
	editor.Transact(func(txn *crdt.Transaction) {
		marks.Set(txn, "c-1", map[string]any{"kind": "comment", "resolved": true})
	})
	editor.Transact(func(txn *crdt.Transaction) { text.Delete(txn, 6, 6) })
	editor.Transact(func(txn *crdt.Transaction) { marks.Delete(txn, "c-2") })
	editor.Transact(func(txn *crdt.Transaction) { fragment.InsertElement(txn, 1, crdt.NewYXmlElement("horizontal_rule")) })
	editor.Transact(func(txn *crdt.Transaction) { fragment.Delete(txn, 1, 1) })
	stop()
	state("editor", editor)

	other := newDocumentCopy(crdt.WithClientID(12), crdt.WithGC(false))
	stop = record("other roots", other)
	words := other.GetText("t")
	list := other.GetArray("a")
	nested := other.GetMap("m")
	other.Transact(func(txn *crdt.Transaction) {
		words.Insert(txn, 0, "text with an embed", nil)
		words.InsertEmbed(txn, 4, "embedded", nil)
		words.Format(txn, 0, 4, crdt.Attributes{"italic": true})
	})
	other.Transact(func(txn *crdt.Transaction) { list.Push(txn, []any{"a", 2.0, true, nil, []byte{1, 2}}) })
	other.Transact(func(txn *crdt.Transaction) { list.Move(txn, 0, 3) })
	other.Transact(func(txn *crdt.Transaction) {
		nested.Set(txn, "map", crdt.NewMapPrelim())
		nested.Set(txn, "text", crdt.NewTextPrelim())
		nested.Set(txn, "array", crdt.NewArrayPrelim())
		nested.Set(txn, "sub", crdt.New(crdt.WithGUID("sub-guid")))
	})
	other.Transact(func(txn *crdt.Transaction) { words.Delete(txn, 2, 6) })
	stop()
	state("other roots", other)

	// JSON in both encodings, binary, deleted content and GC runs, which only arrive by decoding.
	handBuilt := []madeUpdate{
		{name: "JSON as text", structs: 1, update: newV1Frame(1).group(1, 21, 0).rooted(2, marksMapName, "j").
			uint(2).str(`{"a":[1,2.5,"x"]}`).str("undefined").noDeletes()},
		{name: "JSON as legacy lib0 Any", structs: 1, update: newV1Frame(1).group(1, 22, 0).rooted(2, marksMapName, "j").
			uint(1).any(map[string]any{"legacy": []any{1.5, "y"}}).noDeletes()},
		{name: "binary and deleted content", structs: 3, update: newV1Frame(1).group(3, 23, 0).rooted(3, marksMapName, "bin").
			str("\x00\x01\xff").rooted(1, fragmentName, "").uint(4).rooted(4, fragmentName, "").str("tail").noDeletes()},
		{name: "GC and skip runs", structs: 7, update: newV1Frame(2).group(4, 24, 0).gc(3).skip(2).gc(4).gc(1).
			group(3, 25, 5).gc(2).skip(7).gc(1).noDeletes()},
	}
	made = append(made, handBuilt...)
	for _, built := range handBuilt[:3] {
		doc := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(doc, built.update, nil); err != nil {
			tb.Fatalf("%s: %v", built.name, err)
		}
		state(built.name+" through ygo", doc)
	}
	gcDoc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(gcDoc, newV1Frame(1).group(2, 26, 0).gc(5).rooted(4, fragmentName, "").str("ok").noDeletes(), nil); err != nil {
		tb.Fatal(err)
	}
	state("a GC run through ygo", gcDoc)

	// Parked structs behind a skip: a document given a transaction's update without the one before
	// it parks it, and its state carries the skip and the parked structs (crdt/update.go:87-109).
	var steps [][]byte
	writer := newDocumentCopy(crdt.WithClientID(27))
	stopSteps := writer.OnUpdate(func(update []byte, _ any) { steps = append(steps, slices.Clone(update)) })
	writing := writer.GetXmlFragment(fragmentName)
	for i := range 4 {
		writer.Transact(func(txn *crdt.Transaction) {
			paragraph := crdt.NewYXmlElement("paragraph")
			writing.InsertElement(txn, i, paragraph)
			typed := crdt.NewYXmlText()
			paragraph.InsertText(txn, 0, typed)
			typed.Insert(txn, 0, fmt.Sprintf("paragraph %d", i), nil)
		})
	}
	stopSteps()
	parked := newDocumentCopy()
	for _, step := range [][]byte{steps[0], steps[2], steps[3]} {
		if err := crdt.ApplyUpdateV1(parked, step, nil); err != nil {
			tb.Fatal(err)
		}
	}
	state("a document holding parked structs", parked)
	merged, err := crdt.MergeUpdatesV1(steps[0], steps[2])
	if err != nil {
		tb.Fatal(err)
	}
	made = append(made, madeUpdate{name: "a merge across a gap", update: merged})
	return made
}

// editorCorpus is every update a run of random editing by three browsers sends, on the fragment
// and the margin only, as the editor edits: paragraphs inserted and removed, text typed, deleted
// and marked, block ids set, margin records written, overwritten and removed; with each sync
// between two of them, each browser's whole state, merges of runs of one browser's updates, and the
// state of a room given every other update, which parks the rest behind skips.
func editorCorpus(tb testing.TB) [][]byte {
	tb.Helper()
	rng := rand.New(rand.NewPCG(179, 2))
	var corpus [][]byte
	peers := make([]*crdt.Doc, 3)
	own := make([][][]byte, len(peers))
	for i := range peers {
		peers[i] = newDocumentCopy(crdt.WithClientID(crdt.ClientID(100 + i)))
		peers[i].OnUpdate(func(update []byte, origin any) {
			corpus = append(corpus, slices.Clone(update))
			if origin == nil {
				own[i] = append(own[i], slices.Clone(update))
			}
		})
	}
	words := []string{"a", "word", "héllo", "日本語", "🙂ok", "x y z", "**", "-"}
	for step := range 400 {
		peer := peers[rng.IntN(len(peers))]
		fragment := peer.GetXmlFragment(fragmentName)
		marks := peer.GetMap(marksMapName)
		var paragraphs []*crdt.YXmlElement
		var texts []*crdt.YXmlText
		for _, block := range fragment.Children() {
			if paragraph, ok := block.(*crdt.YXmlElement); ok {
				for _, child := range paragraph.Children() {
					if text, ok := child.(*crdt.YXmlText); ok {
						paragraphs = append(paragraphs, paragraph)
						texts = append(texts, text)
						break
					}
				}
			}
		}
		word := words[rng.IntN(len(words))]
		peer.Transact(func(txn *crdt.Transaction) {
			switch op := rng.IntN(8); {
			case op == 0 || len(texts) == 0:
				paragraph := crdt.NewYXmlElement("paragraph")
				fragment.InsertElement(txn, rng.IntN(fragment.Len()+1), paragraph)
				text := crdt.NewYXmlText()
				paragraph.InsertText(txn, 0, text)
				text.Insert(txn, 0, word, nil)
			case op == 1:
				text := texts[rng.IntN(len(texts))]
				text.Insert(txn, rng.IntN(text.Len()+1), word, nil)
			case op == 2:
				text := texts[rng.IntN(len(texts))]
				if text.Len() > 0 {
					at := rng.IntN(text.Len())
					text.Delete(txn, at, 1+rng.IntN(text.Len()-at))
				}
			case op == 3:
				text := texts[rng.IntN(len(texts))]
				if text.Len() > 0 {
					at := rng.IntN(text.Len())
					text.Format(txn, at, 1+rng.IntN(text.Len()-at), crdt.Attributes{"strong": true})
				}
			case op == 4 && fragment.Len() > 1:
				fragment.Delete(txn, rng.IntN(fragment.Len()), 1)
			case op == 5:
				paragraphs[rng.IntN(len(paragraphs))].SetAttribute(txn, "id", fmt.Sprintf("b-%d", step))
			case op == 6:
				marks.Set(txn, fmt.Sprintf("c-%d", rng.IntN(6)), map[string]any{"kind": "comment", "quote": word, "by": "ai:x", "at": float64(step)})
			default:
				marks.Delete(txn, fmt.Sprintf("c-%d", rng.IntN(6)))
			}
		})
		if rng.IntN(4) == 0 {
			from, to := peers[rng.IntN(len(peers))], peers[rng.IntN(len(peers))]
			sync := crdt.EncodeStateAsUpdateV1(from, to.StateVector())
			corpus = append(corpus, sync)
			if err := crdt.ApplyUpdateV1(to, sync, "sync"); err != nil {
				tb.Fatal(err)
			}
		}
	}
	for i, peer := range peers {
		corpus = append(corpus, crdt.EncodeStateAsUpdateV1(peer, nil))
		for start := 0; start+5 <= len(own[i]); start += 5 {
			merged, err := crdt.MergeUpdatesV1(own[i][start : start+5]...)
			if err != nil {
				tb.Fatal(err)
			}
			corpus = append(corpus, merged)
		}
		halves := newDocumentCopy()
		for j := 0; j < len(own[i]); j += 2 {
			if err := crdt.ApplyUpdateV1(halves, own[i][j], nil); err != nil {
				tb.Fatal(err)
			}
		}
		corpus = append(corpus, crdt.EncodeStateAsUpdateV1(halves, nil))
	}
	return corpus
}

// updateWalkSeeds is FuzzUpdateWalk's seed corpus.
func updateWalkSeeds(tb testing.TB) [][]byte {
	tb.Helper()
	var seeds [][]byte
	for size := 116; size <= 127; size++ {
		text := `"` + strings.Repeat("s", size-2) + `"`
		seeds = append(seeds, newV1Frame(1).group(1, 1, 0).rooted(2, marksMapName, "k").uint(2).str(text).str(text).noDeletes())
	}
	nested := encoding.NewEncoder()
	nested.WriteAny(map[string]any{"a": []any{map[string]any{"b": []any{1.5, "c", []byte{9}, nil}}, true}, "d": -7.0})
	seeds = append(seeds,
		newV1Frame(1).group(1, 2, 0).rooted(8, marksMapName, "k").uint(1).raw(nested.Bytes()...).noDeletes(),
		newV1Frame(1).group(1, 3, 0).rooted(2, marksMapName, "k").uint(1).raw(nested.Bytes()...).noDeletes(),
		newV1Frame(1).group(1, 4, 0).rooted(9, marksMapName, "sub").str("guid").any(map[string]any{"autoLoad": true}).noDeletes(),
		newV1Frame(2).group(3, 5, 0).gc(4).skip(3).gc(2).group(2, 6, 2).skip(2).gc(1).noDeletes(),
		newV1Frame(1).group(4, 7, 0).gc(3).gc(3).gc(3).rooted(12, fragmentName, "").noDeletes(),
		newV1Frame(1).group(2, 8, 0).rooted(4, fragmentName, "").str("ab").raw(0x84).uint(8).uint(1).str("c").
			uint(1).uint(8).uint(1).uint(0).uint(2).done(),
	)
	editor := newDocumentCopy(crdt.WithClientID(9))
	fragment := editor.GetXmlFragment(fragmentName)
	margin := editor.GetMap(marksMapName)
	editor.Transact(func(txn *crdt.Transaction) {
		paragraph := crdt.NewYXmlElement("paragraph")
		fragment.InsertElement(txn, 0, paragraph)
		text := crdt.NewYXmlText()
		paragraph.InsertText(txn, 0, text)
		text.Insert(txn, 0, "seed", crdt.Attributes{"em": true})
		margin.Set(txn, "c-1", map[string]any{"kind": "comment"})
	})
	seeds = append(seeds, crdt.EncodeStateAsUpdateV1(editor, nil))
	for _, seed := range seeds[:len(seeds):len(seeds)] {
		seeds = append(seeds, seed[:len(seed)/2], seed[:len(seed)-1])
	}
	return seeds
}

// gcFrame is one client's frame of n GC structs of one clock each.
func gcFrame(n int) []byte {
	f := newV1Frame(1).group(n, 1, 0)
	for range n {
		f.gc(1)
	}
	return f.noDeletes()
}

// kindOfItem is the struct kind of an item ygo's decoder returned: a GC run decodes to deleted
// content on an item already marked deleted, where deleted content is not (crdt/update.go:820-831).
func kindOfItem(item *crdt.Item) structKind {
	switch item.Content.(type) {
	case *crdt.ContentDeleted:
		if item.Deleted {
			return structGC
		}
		return structDeleted
	case *crdt.ContentJSON:
		return structJSON
	case *crdt.ContentBinary:
		return structBinary
	case *crdt.ContentString:
		return structString
	case *crdt.ContentEmbed:
		return structEmbed
	case *crdt.ContentFormat:
		return structFormat
	case *crdt.ContentType:
		return structType
	case *crdt.ContentAny:
		return structAny
	case *crdt.ContentDoc:
		return structDocument
	case *crdt.ContentMove:
		return structMove
	}
	panic(fmt.Sprintf("ygo decoded content %T", item.Content))
}

// walkRecord keeps every struct and delete range a walk shows it.
type walkRecord struct {
	structs []updateStruct
	deletes []walkedRange
}

type walkedRange struct{ client, clock, length uint64 }

func (r *walkRecord) walkedStruct(s updateStruct) { r.structs = append(r.structs, s) }

func (r *walkRecord) walkedDelete(client, clock, length uint64) {
	r.deletes = append(r.deletes, walkedRange{client, clock, length})
}

func walkRecorded(update []byte, source updateSource) (walkRecord, updateTally, error) {
	var record walkRecord
	tally, err := walkUpdate(update, source, &record)
	return record, tally, err
}

// walkCounter sums what a walk shows it without keeping it, as a caller of the walk does.
type walkCounter struct {
	clocks  [structMove + 1]uint64
	content [structMove + 1]uint64
	deleted uint64
}

func (c *walkCounter) walkedStruct(s updateStruct) {
	c.clocks[s.kind] += s.length
	c.content[s.kind] += uint64(len(s.content))
}

func (c *walkCounter) walkedDelete(_, _, length uint64) { c.deleted += length }

// coalesced is ranges sorted, with adjacent ranges joined, as ygo's IDSet normalises them.
func coalesced(ranges []crdt.IDRange) []crdt.IDRange {
	set := crdt.NewIDSet()
	for _, r := range ranges {
		set.Add(0, r.Clock, r.Len)
	}
	return set.Ranges(0)
}

func sameIDSet(a, b *crdt.IDSet) bool {
	if !slices.Equal(a.Clients(), b.Clients()) {
		return false
	}
	for _, client := range a.Clients() {
		if !slices.Equal(a.Ranges(client), b.Ranges(client)) {
			return false
		}
	}
	return true
}

// v1Frame lays out a V1 update by hand, byte for byte as a Yjs encoder does, for the frames ygo's
// API cannot write: JSON and binary content, legacy lib0 Any JSON, GC and skip runs, and malformed
// bytes.
type v1Frame struct{ *encoding.Encoder }

// newV1Frame starts a frame of clients groups of structs.
func newV1Frame(clients int) v1Frame {
	f := v1Frame{encoding.NewEncoder()}
	f.WriteVarUint(uint64(clients))
	return f
}

// group starts one client's group of structs structs from clock.
func (f v1Frame) group(structs int, client, clock uint64) v1Frame {
	return f.uint(uint64(structs)).uint(client).uint(clock)
}

func (f v1Frame) gc(length uint64) v1Frame   { return f.raw(0).uint(length) }
func (f v1Frame) skip(length uint64) v1Frame { return f.raw(10).uint(length) }

// rooted starts an item of content reference tag directly under root, a map entry under key when
// key is not empty.
func (f v1Frame) rooted(tag byte, root, key string) v1Frame {
	if key == "" {
		return f.raw(tag, 1).str(root)
	}
	return f.raw(tag|infoHasParentSub, 1).str(root).str(key)
}

func (f v1Frame) uint(v uint64) v1Frame {
	f.WriteVarUint(v)
	return f
}

func (f v1Frame) str(s string) v1Frame {
	f.WriteVarBytes([]byte(s))
	return f
}

func (f v1Frame) any(v any) v1Frame {
	f.WriteAny(v)
	return f
}

func (f v1Frame) raw(b ...byte) v1Frame {
	f.WriteRaw(b)
	return f
}

// noDeletes ends the frame with an empty delete set.
func (f v1Frame) noDeletes() []byte { return f.uint(0).done() }

func (f v1Frame) done() []byte { return slices.Clone(f.Bytes()) }

func varuint(v uint64) []byte {
	e := encoding.NewEncoder()
	e.WriteVarUint(v)
	return e.Bytes()
}

func varstring(s string) []byte {
	e := encoding.NewEncoder()
	e.WriteVarBytes([]byte(s))
	return e.Bytes()
}
