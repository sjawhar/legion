package docs

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// The plan's departure 12 proposed an absolute struct cap S on what a browser's insert may take a
// room to, in place of the decode cap (maxUpdateItems), so that four times a room's whole-state
// frame stays at or under the 64 MiB ygo allows a message by default. The architect's rule: S is the
// largest struct total for which four times the frame at the densest shape a browser builds is at
// or under 64 MiB, and it must also be at least twice the struct total of the heaviest document the
// server's own writes store and of a 5 KiB document after a long editing session; if no S meets
// both, the cap is not built. This file holds what that rule measures. Measured on 2026-10-26 at
// sjawhar/ygo v1.51.3-sami.5 (TestNoAbsoluteStructCapBothKeepsTheFrameAndClearsWhatRoomsHold):
//
//   - The densest shape, alternating empty bullet items each with its minted block id, pasted
//     through the hook until the struct total refused one more pair: a struct total of 1,041,050
//     and a whole-state frame of 33,609,343 bytes, 32.28 bytes a struct, four times it 134,437,372,
//     rendering 416,458 bytes; one decoded copy of that state held 350,252,752 and 383,864,352
//     bytes of heap in two runs, and a room holds two copies (roomCopies). S at that density is
//     519,676.
//   - heaviestDocument, at every bound the server's writes obey: 100,357 structs. A document of
//     16,384 headings an agent rewrote whole through ReplaceText until a write was refused: 27
//     writes, the stored state at exactly maxUpdateItems, 1,048,576 structs rendering 1,048,575
//     bytes in a 12,598,044-byte frame, the 28th refused by the decode cap (983,040 structs had
//     ygo's RunGC merged its deleted runs, structsAndRendering).
//   - The long-edited fixture (longEditedRoom, 1,000,000 keystrokes): 494,308 structs once
//     loaded, rendering 5,314 bytes in a 7,814,707-byte frame (84,847 had RunGC merged its deleted
//     runs).
//
// So no S meets both: the frame allows at most 519,676, and twice the storable document is
// 2,097,152 and twice the long-edited one 988,616. The cap was not built.

// longEditFixture is the long-edited document's encoded state as longEditedRoom leaves it after
// longEditKeystrokes keystrokes, gzipped. It is stored rather than made by each test that reads
// it because the simulation takes about twelve minutes on one processor: every keystroke's place
// is found by a walk of its text's items, and the items grow with the edit.
const longEditFixture = "testdata/long_edited_document.update.gz"

// longEditKeystrokes is how many keystrokes the long-edited fixture took.
const longEditKeystrokes = 1_000_000

// longEdit is what longEditedRoom did to its document: keystrokes in all, characters typed (each
// a keystroke), typos (each a wrong character typed and taken back, two keystrokes), characters
// taken back one keystroke at a time, selections deleted (each a keystroke) and the characters
// they held, and the room's residencies.
type longEdit struct {
	keystrokes, typed, typos, backspaced, selections, selected, sessions int
}

// longEditedRoom is a room's live document holding about 5 KiB of prose after one person has edited
// it for keystrokes keystrokes, deterministically: the same keystrokes always make the same bytes.
//
// The document is ten paragraphs of 512 bytes. The person places the cursor anywhere in any
// paragraph and then, while the document stays within 256 bytes of 5 KiB, types a run of about 14
// characters (55 per cent of the time; one character in 33 is a typo taken back at once), takes
// back about 5 characters one keystroke at a time (30 per cent), or deletes a selection of about 24
// characters in one keystroke (15 per cent); outside the 256 bytes they type, or delete, until
// back within them. Every keystroke is a transaction of its own on the room, with the browser's
// client id: what a room holds after applying each keystroke's update as it arrives, which
// TestALongEditsKeystrokesLeaveTheRoomWhatTheirUpdatesLeaveThroughTheHook holds. Each 4,000
// keystrokes (about twenty minutes of editing) the room is retired and loaded again from its state
// in one decode, as a room retired once unused is, and the person's next page load edits it with a
// new client id. relay, when not nil, is called with the state each residency loads from, and the
// function it returns is handed each keystroke's update in that residency.
func longEditedRoom(tb testing.TB, keystrokes int, relay func(loaded []byte) func(update []byte)) (*crdt.Doc, longEdit) {
	tb.Helper()
	const (
		paragraphs        = 10
		target            = 5 << 10
		sessionKeystrokes = 4_000
	)
	minted := 0
	pmdoc.SetBlockIDGenerator(func() string {
		minted++
		return fmt.Sprintf("long-edit-%d", minted)
	})
	defer pmdoc.SetBlockIDGenerator(nil)
	var seed strings.Builder
	for range paragraphs {
		seed.WriteString(strings.TrimSpace(strings.Repeat("word ", target/paragraphs/5)))
		seed.WriteString("\n\n")
	}
	tree, err := pmdoc.Parse(seed.String())
	if err != nil {
		tb.Fatal(err)
	}
	seeded := newDocumentCopy(crdt.WithClientID(1))
	fragment := seeded.GetXmlFragment(fragmentName)
	seeded.GetMap(marksMapName)
	if err := seeded.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
		tb.Fatal(err)
	}
	state := crdt.EncodeStateAsUpdateV1(seeded, nil)

	rng := rand.New(rand.NewPCG(179, 12))
	geometric := func(mean float64) int { return 1 + int(-math.Log(1-rng.Float64())*(mean-1)) }
	const letters = "etaoinshrdlucmfwypvbgkjqxz      "
	var done longEdit
	var room *crdt.Doc
	var texts []*crdt.YXmlText
	keystroke := func(change func(txn *crdt.Transaction)) {
		room.Transact(change)
		done.keystrokes++
	}
	for done.keystrokes < keystrokes {
		if room == nil || done.keystrokes/sessionKeystrokes >= done.sessions {
			if room != nil {
				state = crdt.EncodeStateAsUpdateV1(room, nil)
			}
			// A client id of five bytes, as most random ids are (the browser's
			// TestTheHeaviestWholeStateFrameABrowserCanGrowFitsTheMessageFloor gives why).
			room = newDocumentCopy(crdt.WithClientID(crdt.ClientID(1<<31 + rng.Uint32()>>1)))
			if err := crdt.ApplyUpdateV1(room, state, nil); err != nil {
				tb.Fatal(err)
			}
			if relay != nil {
				keystrokeUpdate := relay(state)
				room.OnUpdate(func(update []byte, _ any) { keystrokeUpdate(update) })
			}
			texts = allTexts(room.GetXmlFragment(fragmentName))
			if len(texts) != paragraphs {
				tb.Fatalf("the document holds %d texts, want %d", len(texts), paragraphs)
			}
			done.sessions++
		}
		text := texts[rng.IntN(len(texts))]
		pos := rng.IntN(text.Len() + 1)
		total := 0
		for _, text := range texts {
			total += text.Len()
		}
		roll := rng.Float64()
		switch {
		case total < target-256 || (total <= target+256 && roll < 0.55):
			for range geometric(14) {
				if rng.Float64() < 0.03 {
					keystroke(func(txn *crdt.Transaction) { text.Insert(txn, pos, "q", nil) })
					keystroke(func(txn *crdt.Transaction) { text.Delete(txn, pos, 1) })
					done.typos++
				}
				char := string(letters[rng.IntN(len(letters))])
				keystroke(func(txn *crdt.Transaction) { text.Insert(txn, pos, char, nil) })
				pos++
				done.typed++
			}
		case total > target+256 || roll < 0.85:
			for range min(geometric(5), pos) {
				keystroke(func(txn *crdt.Transaction) { text.Delete(txn, pos-1, 1) })
				pos--
				done.backspaced++
			}
		default:
			if n := min(geometric(24), text.Len()-pos); n > 0 {
				keystroke(func(txn *crdt.Transaction) { text.Delete(txn, pos, n) })
				done.selections++
				done.selected += n
			}
		}
	}
	return room, done
}

// allTexts is every text in node, in document order, however deep.
func allTexts(node any) []*crdt.YXmlText {
	var texts []*crdt.YXmlText
	var walk func(node any)
	walk = func(node any) {
		switch node := node.(type) {
		case *crdt.YXmlText:
			texts = append(texts, node)
		case *crdt.YXmlFragment:
			for _, child := range node.Children() {
				walk(child)
			}
		case *crdt.YXmlElement:
			for _, child := range node.Children() {
				walk(child)
			}
		}
	}
	walk(node)
	return texts
}

// longEditedState is the long-edited fixture's encoded state.
func longEditedState(tb testing.TB) []byte {
	tb.Helper()
	file, err := os.Open(longEditFixture)
	if err != nil {
		tb.Fatal(err)
	}
	defer file.Close()
	unzipped, err := gzip.NewReader(file)
	if err != nil {
		tb.Fatal(err)
	}
	state, err := io.ReadAll(unzipped)
	if err != nil {
		tb.Fatal(err)
	}
	return state
}

// The long-edited fixture is a document of about 5 KiB of prose that loads and renders, whose
// state carries the history of its million keystrokes: more than ten structs for each byte its
// text renders (93 measured), since ygo keeps each character taken back one keystroke at a time a
// struct of its own (gcTxnDeleteSet replaces a deleted item's content and merges nothing,
// sjawhar/ygo v1.51.3-sami.5, crdt/snapshot.go:284-313).
func TestTheLongEditedFixtureIsAFiveKiBDocumentCarryingItsHistory(t *testing.T) {
	room := newDocumentCopy()
	state := longEditedState(t)
	if err := crdt.ApplyUpdateV1(room, state, nil); err != nil {
		t.Fatal(err)
	}
	rendering, err := renderDocument(room)
	if err != nil {
		t.Fatal(err)
	}
	structs := countRoomStructs(room)
	t.Logf("the long-edited fixture renders %d bytes; its %d-byte state loads as %d structs", len(rendering), len(state), structs)
	if len(rendering) < 4<<10 || len(rendering) > 6<<10 || structs < 10*uint64(len(rendering)) {
		t.Fatalf("the fixture renders %d bytes in %d structs, want about 5 KiB carrying more than ten structs a byte", len(rendering), structs)
	}
}

// The fixture is what longEditedRoom makes, byte for byte. Set DISPATCH_LONG_EDIT_FIXTURE=check
// to check it, or =write to write it again after a change to the simulation; either runs the
// simulation, about twelve minutes on one processor.
func TestTheLongEditedFixtureIsWhatTheSimulationMakes(t *testing.T) {
	mode := os.Getenv("DISPATCH_LONG_EDIT_FIXTURE")
	if mode == "" {
		t.Skip("set DISPATCH_LONG_EDIT_FIXTURE=check or =write to run the twelve-minute simulation")
	}
	room, done := longEditedRoom(t, longEditKeystrokes, nil)
	state := crdt.EncodeStateAsUpdateV1(room, nil)
	t.Logf("%+v: a %d-byte state", done, len(state))
	if mode == "write" {
		var zipped bytes.Buffer
		writer, err := gzip.NewWriterLevel(&zipped, gzip.BestCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(state); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(longEditFixture), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(longEditFixture, zipped.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(state, longEditedState(t)) {
		t.Fatalf("the simulation makes a %d-byte state that differs from the fixture's: write it again with DISPATCH_LONG_EDIT_FIXTURE=write", len(state))
	}
}

// longEditedRoom applies each keystroke to the room as a transaction of its own. A room takes a
// browser's keystrokes as updates through the hook, each applied in a transaction of its own
// (weighPeerUpdate): the two leave the same struct total, so the simulation's figure is a room's.
// One residency is driven both ways here, its keystrokes relayed through the hook to a second room
// loaded from the same state.
func TestALongEditsKeystrokesLeaveTheRoomWhatTheirUpdatesLeaveThroughTheHook(t *testing.T) {
	const keystrokes = 3_000
	var through *crdt.Doc
	var replica *renderedReplica
	relayed := 0
	room, done := longEditedRoom(t, keystrokes, func(loaded []byte) func(update []byte) {
		through = newDocumentCopy()
		if err := crdt.ApplyUpdateV1(through, loaded, nil); err != nil {
			t.Fatal(err)
		}
		rendering, err := renderDocument(through)
		if err != nil {
			t.Fatal(err)
		}
		service := &Service{}
		replica = service.keepReplica(through, nil, &rendering)
		return func(update []byte) {
			if err, called := weighInto(service, through, update); err != nil || !called {
				t.Fatalf("the hook answered %v (applied %t) to keystroke %d, want it applied", err, called, relayed+1)
			}
			relayed++
		}
	})
	runtime.KeepAlive(replica)
	if done.sessions != 1 || relayed != done.keystrokes {
		t.Fatalf("the drive took %d residencies and relayed %d of %d keystrokes, want one residency relaying each", done.sessions, relayed, done.keystrokes)
	}
	direct, hooked := countRoomStructs(room), countRoomStructs(through)
	t.Logf("%+v: %d structs applied as transactions, %d through the hook", done, direct, hooked)
	if direct != hooked {
		t.Fatalf("the keystrokes leave %d structs applied as transactions and %d through the hook, want the same", direct, hooked)
	}
}

// densestStructCapRoom is a room grown through the hook by updates of the densest markdown a
// browser writes - alternating empty bullet items, `- \n\n* \n\n`, each list, item and paragraph
// with the block id a browser's write mints, as
// TestTheHeaviestWholeStateFrameABrowserCanGrowFitsTheMessageFloor writes them - from a small
// document until the hook's struct total refuses a single pair more. Updates start at 4,096 pairs,
// and each refused one is tried again at half its pairs. It returns the room's state at the start
// and at the end.
func densestStructCapRoom(t *testing.T) (start, end []byte) {
	t.Helper()
	const pair = "- \n\n* \n\n"
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	start = crdt.EncodeStateAsUpdateV1(room, nil)
	var browser *crdt.Doc
	var tree *pmdoc.Node
	var fragment *crdt.YXmlFragment
	// copyRoom makes the browser's document a fresh copy of the room, as an editor whose update was
	// refused rebuilds from a fresh read.
	copyRoom := func() {
		browser = newDocumentCopy(crdt.WithClientID(math.MaxUint32))
		if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(room, nil), nil); err != nil {
			t.Fatal(err)
		}
		var err error
		if tree, err = treeOf(browser); err != nil {
			t.Fatal(err)
		}
		fragment = browser.GetXmlFragment(fragmentName)
	}
	copyRoom()
	grow := func(pairs int) error {
		// An upload's parse refuses past pmdoc.MaxDocumentElements, so the pairs are parsed a
		// thousand at a time; the browser's update carries them all.
		for left := pairs; left > 0; left -= 1024 {
			added, err := pmdoc.Parse(strings.Repeat(pair, min(left, 1024)))
			if err != nil {
				t.Fatal(err)
			}
			tree.Children = append(tree.Children, added.Children...)
		}
		if err := browser.TransactE(func(txn *crdt.Transaction) error { return pmdoc.Update(txn, fragment, tree) }); err != nil {
			t.Fatal(err)
		}
		if err, _ := weighInto(service, room, crdt.EncodeStateAsUpdateV1(browser, room.StateVector())); err != nil {
			copyRoom()
			return err
		}
		rendering, err := renderDocument(room)
		if err != nil {
			t.Fatal(err)
		}
		replica.publishCounted(&rendering, countElements(rendering))
		return nil
	}
	for pairs := 4096; pairs > 0; {
		err := grow(pairs)
		if err == nil {
			continue
		}
		var refusal *websocket.PeerClose
		if !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode {
			t.Fatalf("the hook answered %v to %d pairs, want them admitted or refused by the struct total", err, pairs)
		}
		pairs /= 2
	}
	if bytes, _, _ := replica.size(); bytes >= pmdoc.MaxDocumentBytes {
		t.Fatalf("the room renders %d bytes, past the byte bound: what refused it was not the struct total alone", bytes)
	}
	runtime.KeepAlive(replica)
	return start, crdt.EncodeStateAsUpdateV1(room, nil)
}

// heapOfOneCopy is the heap one decoded copy of state holds once collected.
func heapOfOneCopy(t *testing.T, state []byte) int64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	held := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(held, state, nil); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(held)
	return int64(after.HeapAlloc) - int64(before.HeapAlloc)
}

// agentRewrittenState is the stored state of a document an agent has rewritten whole through the
// API (ReplaceText), each write 16,384 headings of fresh text at both halves of the byte and element
// bounds, until a write is refused; and the refusal.
func agentRewrittenState(t *testing.T) ([]byte, error) {
	t.Helper()
	service, artifactID := newTestService(t)
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	const headingBytes = 64
	headings := pmdoc.MaxDocumentBytes / headingBytes
	for write := 0; ; write++ {
		var markdown strings.Builder
		for heading := range headings {
			fmt.Fprintf(&markdown, "# %0*d\n\n", headingBytes-4, write*headings+heading)
		}
		_, err := joinedReplaceText(service, artifactID, markdown.String(), agent)
		if err != nil {
			if write == 0 {
				t.Fatalf("the first write: %v", err)
			}
			loaded, loadErr := service.persistence.Load(context.Background(), artifactID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			t.Logf("the agent's write %d was refused: %v", write+1, err)
			return loaded.Update, err
		}
	}
}

// structsAndRendering is state's struct total as a room loaded from it counts it, what it renders,
// and the struct total ygo's RunGC would leave it, which merges each run of adjacent deleted items
// one client wrote in order into one (sjawhar/ygo v1.51.3-sami.5, crdt/snapshot.go:315-392), as
// Yjs merges them at each transaction's end; no room runs it.
func structsAndRendering(t *testing.T, state []byte) (structs uint64, rendered int, merged uint64) {
	t.Helper()
	doc := newDocumentCopy()
	if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
		t.Fatal(err)
	}
	rendering, err := renderDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	structs = countRoomStructs(doc)
	crdt.RunGC(doc)
	return structs, len(rendering), countRoomStructs(doc)
}

// The architect's rule for the absolute struct cap S (departure 12), measured: S is the largest
// struct total whose whole-state frame at the densest shape a browser builds is at most a quarter
// of 64 MiB, and it must be at least twice the struct total of the heaviest document the server's
// own writes store and of the long-edited 5 KiB document. It holds that no S meets both, which is
// why the cap is not built; should ygo or the server come to hold far fewer structs for the same
// history, it fails and says the cap can be measured again. Set DISPATCH_MEASURE_STRUCT_CAP=1 to
// run it: about eight minutes on one processor, with the store.
func TestNoAbsoluteStructCapBothKeepsTheFrameAndClearsWhatRoomsHold(t *testing.T) {
	if os.Getenv("DISPATCH_MEASURE_STRUCT_CAP") == "" {
		t.Skip("set DISPATCH_MEASURE_STRUCT_CAP=1 to run the eight-minute measurement")
	}
	const frameCeiling = 64 << 20 / 4

	start, end := densestStructCapRoom(t)
	startStructs, _, _ := structsAndRendering(t, start)
	capStructs, capRendered, _ := structsAndRendering(t, end)
	perStruct := float64(len(end)-len(start)) / float64(capStructs-startStructs)
	structCap := capStructs - uint64(float64(len(end)-frameCeiling)/perStruct)
	t.Logf("the densest shape refused at %d structs rendering %d bytes: a %d-byte frame, four times it %d; %.2f frame bytes a struct, so S is %d; one decoded copy holds %d heap bytes",
		capStructs, capRendered, len(end), 4*len(end), perStruct, structCap, heapOfOneCopy(t, end))
	if capStructs < maxUpdateItems-maxUpdateItems/64 {
		t.Fatalf("the struct total refused the densest shape at %d structs, want it within a sixty-fourth of the %d cap", capStructs, maxUpdateItems)
	}

	heaviestStructs := countRoomStructs(heaviestDocument(t))
	rewritten, refusal := agentRewrittenState(t)
	if !errors.Is(refusal, ErrDocumentTooLarge) {
		t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
	}
	rewrittenStructs, rewrittenRendered, rewrittenMerged := structsAndRendering(t, rewritten)
	t.Logf("the server's writes store %d structs at heaviestDocument, and %d rendering %d bytes in a %d-byte frame once an agent's rewrites are refused (%d after RunGC)",
		heaviestStructs, rewrittenStructs, rewrittenRendered, len(rewritten), rewrittenMerged)

	edited := longEditedState(t)
	editedStructs, editedRendered, editedMerged := structsAndRendering(t, edited)
	t.Logf("the long-edited document holds %d structs rendering %d bytes in a %d-byte frame (%d after RunGC)",
		editedStructs, editedRendered, len(edited), editedMerged)

	heaviestKept := max(heaviestStructs, rewrittenStructs, editedStructs)
	if structCap >= 2*heaviestKept {
		t.Fatalf("S is %d, at least twice the heaviest document a room keeps (%d structs): the absolute struct cap fits, and departure 12 can be built",
			structCap, heaviestKept)
	}
}
