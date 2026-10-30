package docs

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/docs/docstest"
)

// marginTextKey is the margin key under which nestedMarginText nests a text.
const marginTextKey = "nested"

// nestedMarginText is a browser's copy of room with a text nested under the margin
// (marksMapName), the update that nests it, and the text: a value the walk reads as text wherever
// its characters go, since their parent is an item the walk cannot place, so neither the byte
// rule's rendering nor the unrendered total sees them.
func nestedMarginText(t *testing.T, room *crdt.Doc) (*crdt.Doc, []byte, *crdt.YText) {
	t.Helper()
	browser, _ := peerDocument(t, room)
	held := browser.StateVector()
	margin := browser.GetMap(marksMapName)
	browser.Transact(func(txn *crdt.Transaction) { margin.Set(txn, marginTextKey, crdt.NewTextPrelim()) })
	nested, ok := margin.Get(marginTextKey)
	text, isText := nested.(*crdt.YText)
	if !ok || !isText {
		t.Fatalf("the margin holds %T under %q, want a text", nested, marginTextKey)
	}
	return browser, crdt.EncodeStateAsUpdateV1(browser, held), text
}

// typedInto is the update browser sends for typing typed at the end of text.
func typedInto(browser *crdt.Doc, text *crdt.YText, typed string) []byte {
	held := browser.StateVector()
	browser.Transact(func(txn *crdt.Transaction) { text.Insert(txn, text.Len(), typed, nil) })
	return crdt.EncodeStateAsUpdateV1(browser, held)
}

// weighedPeer is what the hook's walk of update finds against room.
func weighedPeer(t *testing.T, room *crdt.Doc, update []byte) peerWeighing {
	t.Helper()
	weighed := peerWeighing{held: room.StateVector()}
	if _, err := walkUpdate(update, fromPeer, &weighed); err != nil {
		t.Fatal(err)
	}
	return weighed
}

// framePadded makes replica's totals count the room's whole-state frame as pad bytes more than it
// is, so a test can stand the frame total at F without building a room of forty-five mebibytes, and
// recounts them; it counts the counts as countedStructTotal does.
func framePadded(t *testing.T, replica *renderedReplica, room *crdt.Doc, pad uint64) (*structTotal, *atomic.Int64) {
	t.Helper()
	total, counts := countedStructTotal(t, replica)
	total.count = func(doc *crdt.Doc) roomCount {
		counts.Add(1)
		counted := countRoom(doc)
		counted.frame += pad
		return counted
	}
	total.recount(room)
	counts.Store(0)
	return total, counts
}

// The frame total holds the room's whole-state frame under F (heaviestHandoffBound) where the
// other rules cannot see what grows it: text nested under the margin, which renders nothing and
// adds no unrendered bytes. With the room's frame total standing one paste short of F, that paste
// is admitted, the total then standing at F; the next paste, which would take it past F, is
// refused with the document's own close once an exact count confirms the total, and leaves the
// room as it was, and so is a keystroke in the document's own text.
func TestAnUpdateThatWouldTakeTheFrameTotalPastTheHeaviestHandoffIsRefused(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	browser, nesting, text := nestedMarginText(t, room)
	if err, called := weighInto(service, room, nesting); err != nil || !called {
		t.Fatalf("nesting a text under the margin was answered %v (applied %t), want it admitted", err, called)
	}
	paste := strings.Repeat("p", 64<<10)
	first := typedInto(browser, text, paste)
	weighed := weighedPeer(t, room, first)
	if weighed.unrendered != 0 || weighed.frame < uint64(len(paste)) {
		t.Fatalf("a %d-byte paste into the margin's text adds %d unrendered bytes and %d frame bytes, want none and at least the paste",
			len(paste), weighed.unrendered, weighed.frame)
	}
	held := countRoom(room).frame
	total, counts := framePadded(t, replica, room, heaviestHandoffBound-held-weighed.frame)
	if err, called := weighInto(service, room, first); err != nil || !called {
		t.Fatalf("a paste taking the frame total to F was answered %v (applied %t), want it admitted", err, called)
	}
	if running, _ := total.runningTotals(); running.frame != heaviestHandoffBound {
		t.Fatalf("after the paste the frame total is %d, want F, %d", running.frame, uint64(heaviestHandoffBound))
	}
	for _, refused := range []struct {
		name   string
		update []byte
	}{
		{"a second paste into the margin's text", typedInto(browser, text, paste)},
		{"a keystroke in the document's text", peerTyping(t, room, "y")},
	} {
		before := crdt.EncodeStateAsUpdateV1(room, nil)
		err, called := weighInto(service, room, refused.update)
		requireRefusedTooLarge(t, err, called, room, before)
	}
	if counts.Load() != 1 {
		t.Fatalf("the hook counted the room %d times before its refusals, want one confirmation", counts.Load())
	}
	if running, _ := total.runningTotals(); running.unrendered >= maxUnrenderedBytes/2 {
		t.Fatalf("the room's unrendered total is %d, want it far under the %d-byte bound, so that only the frame total refused", running.unrendered, maxUnrenderedBytes)
	}
	runtime.KeepAlive(replica)
}

// An update that inserts nothing is never refused by the frame total, though it adds to it what it
// can add to the frame, so a room at or past F can still be shortened and handed back whole: with
// the frame total past F, a cut and a whole-state message of what the room holds are admitted,
// without a count, and a keystroke is refused.
func TestADeleteOnlyUpdateIsAdmittedAtTheFrameTotal(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n\nAnother paragraph.\n", 0)
	held := countRoom(room).frame
	total, counts := framePadded(t, replica, room, heaviestHandoffBound+1-held)
	if running, stale := total.runningTotals(); running.frame <= heaviestHandoffBound || stale {
		t.Fatalf("the frame total is %d (stale %t), want it counted past F, %d", running.frame, stale, uint64(heaviestHandoffBound))
	}
	for _, admitted := range []struct {
		name   string
		update []byte
	}{
		{"a cut", peerDeleting(t, room, 3)},
		{"the room's whole state handed back", crdt.EncodeStateAsUpdateV1(room, nil)},
	} {
		if weighed := weighedPeer(t, room, admitted.update); weighed.inserted != 0 || weighed.structs != 0 {
			t.Fatalf("%s inserts %d and adds %d structs, want neither", admitted.name, weighed.inserted, weighed.structs)
		}
		if err, called := weighInto(service, room, admitted.update); err != nil || !called {
			t.Fatalf("%s was answered %v (applied %t), want it admitted past F", admitted.name, err, called)
		}
	}
	if counts.Load() != 0 {
		t.Fatalf("the hook counted the room %d times for updates that insert nothing, want none", counts.Load())
	}
	before := crdt.EncodeStateAsUpdateV1(room, nil)
	err, called := weighInto(service, room, peerTyping(t, room, "y"))
	requireRefusedTooLarge(t, err, called, room, before)
	runtime.KeepAlive(replica)
}

// legacyFalses is client 7's frame of one ContentJSON item under the margin's key "k" holding n
// values that read only as lib0 Any, each false: ygo up to 1.51.0 wrote JSON values so, and ygo
// writes each again as the JSON text false, six bytes for each one of the frame
// (jsonReencodeGrowth).
func legacyFalses(n int) []byte {
	f := newV1Frame(1).group(1, 7, 0).rooted(byte(structJSON), marksMapName, "k").uint(uint64(n))
	for range n {
		f = f.raw(121)
	}
	return f.noDeletes()
}

// What an admitted update adds to the frame total is never less than what it adds to the room's
// whole-state frame, so the running total is never below the frame it bounds: for typing, a paste,
// a cut that splits an item, a margin record, text nested under the margin, the JSON ygo writes
// again longer than it read it - a number written in full, and legacy lib0 Any falses written as
// JSON text - and lib0 Any integers past 32 bits, which ygo writes again as float64.
func TestTheFrameTotalIsNeverBelowTheRoomsWholeStateFrame(t *testing.T) {
	for _, row := range []struct {
		name   string
		update func(t *testing.T, room *crdt.Doc) []byte
	}{
		{"a keystroke", func(t *testing.T, room *crdt.Doc) []byte { return peerTyping(t, room, "y") }},
		{"a paste", func(t *testing.T, room *crdt.Doc) []byte { return peerTyping(t, room, strings.Repeat("pasted ", 4096)) }},
		{"a cut that splits an item", func(t *testing.T, room *crdt.Doc) []byte { return peerDeleting(t, room, 3) }},
		{"a margin record", func(*testing.T, *crdt.Doc) []byte { return marginRecord(7, 0, "c-1", "a comment's text") }},
		{"text nested under the margin", func(t *testing.T, room *crdt.Doc) []byte {
			browser, nesting, text := nestedMarginText(t, room)
			if err := crdt.ApplyUpdateV1(room, nesting, nil); err != nil {
				t.Fatal(err)
			}
			return typedInto(browser, text, strings.Repeat("n", 10_000))
		}},
		{"JSON numbers ygo writes in full", func(*testing.T, *crdt.Doc) []byte { return marginJSON(1000, "1e20") }},
		{"legacy lib0 Any falses ygo writes as JSON text", func(*testing.T, *crdt.Doc) []byte { return legacyFalses(1000) }},
		{"lib0 Any integers ygo writes as float64", func(*testing.T, *crdt.Doc) []byte {
			f := newV1Frame(1).group(1, 7, 0).rooted(byte(structAny), marksMapName, "k").uint(1000)
			for range 1000 {
				f.raw(125).WriteVarInt(1 << 34)
			}
			return f.noDeletes()
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
			update := row.update(t, room)
			replica.structs.recount(room)
			before := len(crdt.EncodeStateAsUpdateV1(room, nil))
			if err, called := weighInto(service, room, update); err != nil || !called {
				t.Fatalf("the update was answered %v (applied %t), want it admitted", err, called)
			}
			after := crdt.EncodeStateAsUpdateV1(room, nil)
			running, stale := replica.structs.runningTotals()
			t.Logf("a %d-byte update took the frame from %d bytes to %d; the frame total is %d", len(update), before, len(after), running.frame)
			if stale || running.frame < uint64(len(after)) {
				t.Fatalf("the frame total is %d (stale %t), want at least the room's %d-byte frame", running.frame, stale, len(after))
			}
			runtime.KeepAlive(replica)
		})
	}
}

// fullFrameBoundEnv names the environment variable that runs
// TestTheStoredFrameNeverPassesTheHeaviestHandoffWhateverTheMixOfUpdates at F itself, its room's
// frame grown all the way to F, rather than on a count padded to stand a few mebibytes short of it.
const fullFrameBoundEnv = "DISPATCH_TEST_FULL_FRAME_BOUND"

// The frame total holds the stored whole-state frame under F (heaviestHandoffBound) whatever mix
// of updates browsers send: at every step the total is never below the room's frame
// (crdt.EncodeStateAsUpdateV1), so a total the hook keeps within F keeps the frame within it.
// Browsers holding the room drive the mix this rule could undercount through the hook on one real
// room: typing, formatting, cuts that split items and delete-only updates that remove a whole block
// and everything under it, margin entries written again and again under a few keys from browsers
// that may not hold each other's, whole-state handoffs from browsers holding the room and a few
// edits they did not send, JSON ygo writes again longer than it read it - legacy lib0 Any falses,
// numbers written in full - and lib0 Any integers it writes again as float64, and pastes into a
// text nested under the margin, which grow the frame and nothing else the hook weighs, until the
// frame total refuses an insert. A browser whose edit another rule refuses builds afresh from the
// room, as the editor does on 4413.
//
// Run in the unit suite, the room's count is padded (framePadded) to stand 4 MiB short of F, so the
// total refuses after a few hundred updates, and the invariant asserted at every step is the
// padded total's: the total, less the pad the count stands on, is never below the frame. The
// browsers' clients are random, so each run differs a little: measured on 2026-10-28, it refused
// after about 640 updates in 6 s. With fullFrameBoundEnv set it runs with no pad, the frame
// grown to F itself, in 256 KiB pastes: one run took 1,938 updates in 84 s, refused at a keystroke
// with the frame at 44,744,820 bytes and the total at 44,921,527, under F's 44,936,089. Before the
// frame total counted splits, delete ranges and lib0 Any's longer writing (frameBytes,
// deleteFrameBytes), the first whole-state handoff here, carrying a few keystrokes typed into the
// room's texts, left the frame 18 bytes above the total; the cut of
// TestTheFrameTotalIsNeverBelowTheRoomsWholeStateFrame left it 13 above, and its 1,000 Any
// integers 2,000 above.
func TestTheStoredFrameNeverPassesTheHeaviestHandoffWhateverTheMixOfUpdates(t *testing.T) {
	headroom, pasteBytes, maxSteps := uint64(4<<20), 64<<10, 5_000
	full := os.Getenv(fullFrameBoundEnv) != ""
	if full {
		pasteBytes, maxSteps = 256<<10, 50_000
	}
	started := time.Now()
	rng := rand.New(rand.NewPCG(179, heaviestHandoffBound))
	var markdown strings.Builder
	markdown.WriteString("# Title\n\n")
	for i := range 6 {
		fmt.Fprintf(&markdown, "Paragraph %d of ordinary prose that browsers type into, cut and format.\n\n- an item\n- another item\n\n", i)
	}
	service, room, replica := peerUpdateRoom(t, markdown.String(), 0)
	nested, nesting, nestedText := nestedMarginText(t, room)
	if err, called := weighInto(service, room, nesting); err != nil || !called {
		t.Fatalf("nesting a text under the margin was answered %v (applied %t), want it admitted", err, called)
	}
	var pad uint64
	if !full {
		pad = heaviestHandoffBound - countRoom(room).frame - headroom
	}
	total, counts := framePadded(t, replica, room, pad)

	fresh := func() *crdt.Doc {
		browser := newDocumentCopy()
		if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(room, nil), nil); err != nil {
			t.Fatal(err)
		}
		return browser
	}
	browsers := []*crdt.Doc{fresh(), fresh(), fresh(), fresh()}
	word := func(n int) string {
		letters := make([]byte, n)
		for i := range letters {
			letters[i] = "abcdefghij klmnopqrstuvwxyz"[rng.IntN(27)]
		}
		return string(letters)
	}
	// textOf is one of browser's texts, taken before its transaction: ygo's fragment walk takes the
	// document's lock, which a transaction holds.
	textOf := func(browser *crdt.Doc) *crdt.YXmlText {
		texts := allTexts(browser.GetXmlFragment(fragmentName))
		if len(texts) == 0 {
			return nil
		}
		return texts[rng.IntN(len(texts))]
	}
	typeInto := func(browser *crdt.Doc) []byte {
		text := textOf(browser)
		if text == nil {
			return nil
		}
		at, typed := rng.IntN(text.Len()+1), word(1+rng.IntN(12))
		return docstest.Transact(browser, func(txn *crdt.Transaction) { text.Insert(txn, at, typed, nil) })
	}
	// span is a run of text's characters, at least two long, or false for a text too short.
	span := func(text *crdt.YXmlText) (at, n int, ok bool) {
		if text == nil || text.Len() < 2 {
			return 0, 0, false
		}
		at = rng.IntN(text.Len() - 1)
		return at, 1 + rng.IntN(min(text.Len()-at, 40)), true
	}
	var crafted uint64
	// reencoded is a client's frame of one item under a margin key of its own whose content ygo
	// writes again longer than it read it: legacy lib0 Any falses, read as JSON and written as JSON
	// text, six bytes a byte; JSON numbers written in full; and lib0 Any integers past 32 bits,
	// each read from six bytes and written as a nine-byte float64.
	reencoded := func() []byte {
		crafted++
		client, key, n := 1<<40+crafted, fmt.Sprintf("j-%d", crafted), 50+rng.IntN(500)
		switch crafted % 3 {
		case 0:
			f := newV1Frame(1).group(1, client, 0).rooted(byte(structJSON), marksMapName, key).uint(uint64(n))
			for range n {
				f.raw(121)
			}
			return f.noDeletes()
		case 1:
			f := newV1Frame(1).group(1, client, 0).rooted(byte(structJSON), marksMapName, key).uint(uint64(n))
			for range n {
				f.str("1e20")
			}
			return f.noDeletes()
		default:
			f := newV1Frame(1).group(1, client, 0).rooted(byte(structAny), marksMapName, key).uint(uint64(n))
			for range n {
				f.raw(125).WriteVarInt(1 << 34)
			}
			return f.noDeletes()
		}
	}
	const (
		paste    = "a paste nested under the margin"
		reencode = "JSON and Any ygo writes again longer"
	)
	ops := []struct {
		kind   string
		weight int
		// update is what browser sends, nil for an op that sends nothing.
		update func(browser *crdt.Doc) []byte
	}{
		{"typing", 30, typeInto},
		{"a cut", 10, func(browser *crdt.Doc) []byte {
			text := textOf(browser)
			at, n, ok := span(text)
			if !ok {
				return nil
			}
			return docstest.Transact(browser, func(txn *crdt.Transaction) { text.Delete(txn, at, n) })
		}},
		{"a block deleted with everything under it", 3, func(browser *crdt.Doc) []byte {
			fragment := browser.GetXmlFragment(fragmentName)
			if fragment.Len() <= 4 {
				return nil
			}
			at := rng.IntN(fragment.Len())
			return docstest.Transact(browser, func(txn *crdt.Transaction) { fragment.Delete(txn, at, 1) })
		}},
		{"a format", 5, func(browser *crdt.Doc) []byte {
			text := textOf(browser)
			at, n, ok := span(text)
			if !ok {
				return nil
			}
			return docstest.Transact(browser, func(txn *crdt.Transaction) { text.Format(txn, at, n, crdt.Attributes{"bold": true}) })
		}},
		{"a margin entry", 12, func(browser *crdt.Doc) []byte {
			margin := browser.GetMap(marksMapName)
			key := fmt.Sprintf("c-%d", rng.IntN(8))
			record := map[string]any{"text": word(10 + rng.IntN(200)), "at": int64(rng.IntN(1 << 20))}
			return docstest.Transact(browser, func(txn *crdt.Transaction) { margin.Set(txn, key, record) })
		}},
		{"a whole-state handoff", 5, func(browser *crdt.Doc) []byte {
			for range 1 + rng.IntN(3) {
				typeInto(browser)
			}
			return crdt.EncodeStateAsUpdateV1(browser, nil)
		}},
		{"a sync", 10, func(browser *crdt.Doc) []byte {
			if err := crdt.ApplyUpdateV1(browser, crdt.EncodeStateAsUpdateV1(room, browser.StateVector()), nil); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{reencode, 8, nil},
		{paste, 8, nil},
	}
	weights := 0
	for _, op := range ops {
		weights += op.weight
	}
	pick := func() (string, func(*crdt.Doc) []byte) {
		at := rng.IntN(weights)
		for _, op := range ops {
			if at < op.weight {
				return op.kind, op.update
			}
			at -= op.weight
		}
		panic("the weights do not sum")
	}

	admitted, refused := make(map[string]int), make(map[string]int)
	refusedBy, steps := "", 0
	for ; refusedBy == "" && steps < maxSteps; steps++ {
		kind, send := pick()
		from := rng.IntN(len(browsers))
		var update []byte
		switch kind {
		case paste:
			update = typedInto(nested, nestedText, strings.Repeat("p", pasteBytes))
		case reencode:
			update = reencoded()
		default:
			update = send(browsers[from])
		}
		if update == nil {
			continue
		}
		err, called := weighInto(service, room, update)
		frame := uint64(len(crdt.EncodeStateAsUpdateV1(room, nil)))
		running, stale := total.runningTotals()
		if stale || frame+pad > running.frame {
			t.Fatalf("after %s (step %d, %d bytes, answered %v) the room's frame is %d bytes and the frame total %d less the pad (stale %t), want the total never below the frame",
				kind, steps, len(update), err, frame, running.frame-pad, stale)
		}
		if err == nil && called {
			admitted[kind]++
			continue
		}
		// With the frame grown to F (fullFrameBoundEnv), a whole-state handoff is estimated past
		// the apply heap's bound and applies alone, holding all of it; the hook never answers one
		// as too heavy (errApplyTooHeavy), and such an answer falls to the failure below.
		var refusal *websocket.PeerClose
		if !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode {
			t.Fatalf("%s (step %d, %d bytes) was answered %v (applied %t), want it admitted or refused as too large", kind, steps, len(update), err, called)
		}
		weighed := weighedPeer(t, room, update)
		if added := weighed.addition(); refusal.Reason == contracts.DocumentTooLargeCloseReason &&
			added.inserts && running.frame+added.frame > heaviestHandoffBound {
			refusedBy = kind
			continue
		}
		if kind == paste || kind == reencode {
			t.Fatalf("%s (step %d) was refused %v with the frame total at %d of F, want only the frame total to refuse it", kind, steps, refusal, running.frame)
		}
		// Another rule refused it: the browser drops what it sent and builds afresh, as the
		// editor does on 4413.
		refused[kind+" "+refusal.Reason]++
		browsers[from] = fresh()
	}
	frame := uint64(len(crdt.EncodeStateAsUpdateV1(room, nil)))
	running, _ := total.runningTotals()
	t.Logf("after %d steps in %s the frame total refused %s: the room's frame is %d bytes and the total %d, with a pad of %d against F's %d; %d counts; admitted %v; refused before %v",
		steps, time.Since(started).Round(time.Millisecond), refusedBy, frame+pad, running.frame, pad, uint64(heaviestHandoffBound), counts.Load(), admitted, refused)
	if refusedBy == "" {
		t.Fatalf("the frame total refused nothing in %d steps; the room's frame is %d bytes of F's %d", steps, frame+pad, uint64(heaviestHandoffBound))
	}
	if frame+pad > heaviestHandoffBound {
		t.Fatalf("the room's frame is %d bytes, past F, %d", frame+pad, uint64(heaviestHandoffBound))
	}
	runtime.KeepAlive(replica)
}
