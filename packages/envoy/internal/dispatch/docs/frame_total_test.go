package docs

import (
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/reearth/ygo/crdt"
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
	if running, _ := total.runningFrame(); running != heaviestHandoffBound {
		t.Fatalf("after the paste the frame total is %d, want F, %d", running, uint64(heaviestHandoffBound))
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
	if unrendered, _ := total.runningUnrendered(); unrendered >= maxUnrenderedBytes/2 {
		t.Fatalf("the room's unrendered total is %d, want it far under the %d-byte bound, so that only the frame total refused", unrendered, maxUnrenderedBytes)
	}
	runtime.KeepAlive(replica)
}

// An update that inserts nothing is never refused by the frame total, so a room at or past F can
// still be shortened and handed back whole: with the frame total past F, a cut and a whole-state
// message of what the room holds are admitted, without a count, and a keystroke is refused.
func TestADeleteOnlyUpdateIsAdmittedAtTheFrameTotal(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n\nAnother paragraph.\n", 0)
	held := countRoom(room).frame
	total, counts := framePadded(t, replica, room, heaviestHandoffBound+1-held)
	if running, stale := total.runningFrame(); running <= heaviestHandoffBound || stale {
		t.Fatalf("the frame total is %d (stale %t), want it counted past F, %d", running, stale, uint64(heaviestHandoffBound))
	}
	for _, admitted := range []struct {
		name   string
		update []byte
	}{
		{"a cut", peerDeleting(t, room, 3)},
		{"the room's whole state handed back", crdt.EncodeStateAsUpdateV1(room, nil)},
	} {
		if weighed := weighedPeer(t, room, admitted.update); weighed.inserted != 0 || weighed.frame != 0 {
			t.Fatalf("%s inserts %d and adds %d frame bytes, want neither", admitted.name, weighed.inserted, weighed.frame)
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
// whole-state frame, so the running total is never below the frame it bounds, short of the splits
// and deletes the struct total bounds: for typing, a paste, a margin record, text nested under the
// margin, and the JSON ygo writes again longer than it read it - a number written in full, and
// legacy lib0 Any falses written as JSON text.
func TestTheFrameTotalIsNeverBelowTheRoomsWholeStateFrame(t *testing.T) {
	for _, row := range []struct {
		name   string
		update func(t *testing.T, room *crdt.Doc) []byte
	}{
		{"a keystroke", func(t *testing.T, room *crdt.Doc) []byte { return peerTyping(t, room, "y") }},
		{"a paste", func(t *testing.T, room *crdt.Doc) []byte { return peerTyping(t, room, strings.Repeat("pasted ", 4096)) }},
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
			running, stale := replica.structs.runningFrame()
			t.Logf("a %d-byte update took the frame from %d bytes to %d; the frame total is %d", len(update), before, len(after), running)
			if stale || running < uint64(len(after)) {
				t.Fatalf("the frame total is %d (stale %t), want at least the room's %d-byte frame", running, stale, len(after))
			}
			runtime.KeepAlive(replica)
		})
	}
}
