package docs

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/encoding"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// marginRecord is client's frame of one margin record under key at clock: a map whose one field
// holds text, as the server writes a comment's record (marginWatch weighs its text).
func marginRecord(client, clock uint64, key, text string) []byte {
	return newV1Frame(1).group(1, client, clock).rooted(byte(structAny), marksMapName, key).
		uint(1).any(map[string]any{"text": text}).noDeletes()
}

// marginRecordUnrendered is the unrendered bytes marginRecord's struct carries, worked out from
// the wire rather than by the walk: the key as encoded, and the content - the count of values and
// the one lib0 Any value.
func marginRecordUnrendered(key, text string) uint64 {
	value := encoding.NewEncoder()
	value.WriteAny(map[string]any{"text": text})
	return uint64(len(varstring(key)) + len(varuint(1)) + len(value.Bytes()))
}

// The finding the unrendered bound closes, as a browser makes it: eighty updates of a million
// margin bytes each, every one a single clock and a single struct, to a room under the byte bound
// and to one at it. Each is admitted until the room's unrendered bytes would pass
// maxUnrenderedBytes, the first past it is refused with the document's own close and leaves the room
// as it was, and every one after it is refused too. The room's whole-state frame ends under F
// (heaviestHandoffBound) and so under the message floor, where without the bound the eighty took a
// 264-byte frame to 80,001,457.
func TestEightyBrowserUpdatesOfMarginBytesStopAtTheUnrenderedBound(t *testing.T) {
	for _, row := range []struct {
		name     string
		markdown string
		least    int
	}{
		{"under the byte bound", "# Title\n\nOne paragraph of ordinary prose.\n", 0},
		{"at the byte bound", proseAtLeast(pmdoc.MaxDocumentBytes), pmdoc.MaxDocumentBytes},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, row.markdown, row.least)
			const updates, size = 80, 1_000_000
			text := strings.Repeat("m", size)
			held := countRoom(room).unrendered
			startFrame := len(crdt.EncodeStateAsUpdateV1(room, nil))
			each := marginRecordUnrendered("c00", text)
			fit := int((maxUnrenderedBytes - held) / each)
			admitted := 0
			for i := range updates {
				// A client of its own for each update, so a refused one leaves no clock gap the
				// next would park behind.
				key := fmt.Sprintf("c%02d", i)
				update := marginRecord(uint64(1000+i), 0, key, text)
				weighed := peerWeighing{held: room.StateVector()}
				if _, err := walkUpdate(update, fromPeer, &weighed); err != nil {
					t.Fatal(err)
				}
				if weighed.unrendered != each || weighed.inserted != 1 {
					t.Fatalf("update %d carries %d unrendered bytes inserting %d clocks, want the %d its wire holds in one clock", i, weighed.unrendered, weighed.inserted, each)
				}
				before := crdt.EncodeStateAsUpdateV1(room, nil)
				err, called := weighInto(service, room, update)
				if i < fit {
					if err != nil || !called {
						t.Fatalf("update %d of %d fitting under the bound was answered %v (applied %t), want it admitted", i, fit, err, called)
					}
					admitted++
					continue
				}
				requireRefusedTooLarge(t, err, called, room, before)
			}
			runtime.KeepAlive(replica)
			unrendered := countRoom(room).unrendered
			frame := len(crdt.EncodeStateAsUpdateV1(room, nil))
			t.Logf("%d of %d updates of %d margin bytes admitted from %d unrendered bytes to %d, against the %d-byte bound; the whole-state frame went from %d bytes to %d, against F %d and the %d-byte floor",
				admitted, updates, each, held, unrendered, maxUnrenderedBytes, startFrame, frame, heaviestHandoffBound, maxMessageBytes)
			if admitted == 0 || admitted == updates || unrendered > maxUnrenderedBytes || unrendered+each <= maxUnrenderedBytes {
				t.Fatalf("%d updates admitted leaving %d unrendered bytes, want them stopped at the %d-byte bound, one more %d-byte update past it", admitted, unrendered, maxUnrenderedBytes, each)
			}
			if frame > heaviestHandoffBound || frame >= maxMessageBytes {
				t.Fatalf("the room's whole-state frame is %d bytes, want it within F (%d) and under the %d-byte floor", frame, heaviestHandoffBound, maxMessageBytes)
			}
		})
	}
}

// An update that adds no unrendered bytes is never refused by the unrendered bound, so a room past
// it - here one the server's own writes filled, which the hook does not weigh - is typed in,
// trimmed and handed back whole as before; an update that adds any, a margin record or a new
// paragraph with its block id, is refused with the document's own close once an exact count
// confirms the room is past the bound.
func TestAnUpdateThatAddsNoUnrenderedBytesIsNeverRefusedByTheUnrenderedBound(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n\nAnother paragraph.\n", 0)
	text := strings.Repeat("s", 1_000_000)
	for i := 0; countRoom(room).unrendered <= maxUnrenderedBytes; i++ {
		if err := crdt.ApplyUpdateV1(room, marginRecord(500, uint64(i), fmt.Sprintf("s%02d", i), text), "server"); err != nil {
			t.Fatal(err)
		}
	}
	total, counts := countedStructTotal(t, replica)
	total.recountLive()
	held, stale := total.runningUnrendered()
	if stale || held <= maxUnrenderedBytes {
		t.Fatalf("the room's unrendered total is %d (stale %t), want it counted past the %d-byte bound", held, stale, maxUnrenderedBytes)
	}
	counts.Store(0)
	for _, admitted := range []struct {
		name   string
		update []byte
	}{
		{"a keystroke", peerTyping(t, room, "y")},
		{"a cut", peerDeleting(t, room, 3)},
		{"the room's whole state handed back", crdt.EncodeStateAsUpdateV1(room, nil)},
	} {
		if err, called := weighInto(service, room, admitted.update); err != nil || !called {
			t.Fatalf("%s was answered %v (applied %t), want it admitted past the unrendered bound", admitted.name, err, called)
		}
	}
	if counts.Load() != 0 {
		t.Fatalf("the hook counted the room %d times for updates that add no unrendered bytes, want none", counts.Load())
	}
	for _, refused := range []struct {
		name   string
		update []byte
	}{
		{"a margin record of a few bytes", marginRecord(8, 0, "c-1", "hello")},
		{"new paragraphs with their block ids", peerReshaping(t, room)},
	} {
		before := crdt.EncodeStateAsUpdateV1(room, nil)
		err, called := weighInto(service, room, refused.update)
		requireRefusedTooLarge(t, err, called, room, before)
	}
	if counts.Load() != 1 {
		t.Fatalf("the hook counted the room %d times before its refusals, want one confirmation", counts.Load())
	}
	runtime.KeepAlive(replica)
}

// A room's load counts its unrendered bytes from the room's encoded state, each struct's key and
// non-text content as the wire carries them; each update the hook admits adds its own, and the
// measurer's count re-bases the total on what the room holds, a server write's records included.
func TestTheUnrenderedTotalALoadCountsIsTheWalkOfTheRoomsEncodedState(t *testing.T) {
	service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
	service.measurer.interval = noInterval
	held, stale := replica.structs.runningUnrendered()
	if stale || held != countRoom(room).unrendered || held == 0 {
		t.Fatalf("the load counted %d unrendered bytes (stale %t), want the walk's %d of a document whose elements carry names and ids", held, stale, countRoom(room).unrendered)
	}

	record := marginRecord(7, 0, "c-1", "a comment's text")
	if err, called := weighInto(service, room, record); err != nil || !called {
		t.Fatalf("the hook answered %v (applied %t), want the record admitted", err, called)
	}
	want := held + marginRecordUnrendered("c-1", "a comment's text")
	if running, stale := replica.structs.runningUnrendered(); running != want || stale || countRoom(room).unrendered != want {
		t.Fatalf("after the record the total is %d (stale %t) and the room counts %d, want the load's %d and the record's wire, %d",
			running, stale, countRoom(room).unrendered, held, want)
	}

	if err := crdt.ApplyUpdateV1(room, marginRecord(9, 0, "c-2", "the server's record"), "server"); err != nil {
		t.Fatal(err)
	}
	if _, stale := replica.structs.runningUnrendered(); !stale {
		t.Fatal("the total reads fresh after a server write, want it stale")
	}
	rendering := headings(1)
	replica.publishSize(&rendering)
	service.measurer.waitIdle()
	want += marginRecordUnrendered("c-2", "the server's record")
	if running, stale := replica.structs.runningUnrendered(); running != want || stale {
		t.Fatalf("after the measurer's count the total is %d (stale %t), want the room's %d", running, stale, want)
	}
	runtime.KeepAlive(replica)
}

// A stale unrendered total is confirmed by an exact count before the hook admits an update that
// adds unrendered bytes on it once it is within unrenderedStaleMargin of the bound; further from
// it, or fresh, it answers alone.
func TestAStaleUnrenderedTotalNearTheBoundIsConfirmedBeforeItAdmits(t *testing.T) {
	for _, row := range []struct {
		name string
		// short is how far short of the bound the running total stands.
		short     uint64
		stale     bool
		confirmed bool
	}{
		{name: "a stale total near the bound is confirmed", short: unrenderedStaleMargin / 2, stale: true, confirmed: true},
		{name: "a stale total far from the bound is not", short: unrenderedStaleMargin + unrenderedStaleMargin/2, stale: true},
		{name: "a fresh total near the bound is not", short: unrenderedStaleMargin / 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, "# Title\n\nOne paragraph of ordinary prose.\n", 0)
			total, counts := countedStructTotal(t, replica)
			held, _ := total.runningUnrendered()
			total.admittedUnrendered.Add(maxUnrenderedBytes - row.short - held)
			if row.stale {
				text := firstText(room.GetXmlFragment(fragmentName))
				room.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 0, "s", nil) }, "server")
			}
			err, called := weighInto(service, room, marginRecord(7, 0, "c-1", "hello"))
			runtime.KeepAlive(replica)
			if err != nil || !called {
				t.Fatalf("the hook answered %v (applied %t), want the record admitted", err, called)
			}
			if confirmed := counts.Load() == 1; confirmed != row.confirmed || counts.Load() > 1 {
				t.Fatalf("the hook counted the room %d times, want a confirmation: %t", counts.Load(), row.confirmed)
			}
		})
	}
}

// A room with no measurement - a document outside the schema, or one no load listed - weighs an
// update's bytes on their own, as if it rendered nothing: a paste of a mebibyte is refused with
// the document's own close, where before it had no byte rule at all, and a paste well under the
// bound applies.
func TestAnUnmeasuredRoomWeighsAPeerUpdateOnItsOwnBytes(t *testing.T) {
	for _, row := range []struct {
		name   string
		listed bool
	}{
		{"a room whose load measured nothing", true},
		{"a room no load listed", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			tree, err := pmdoc.Parse("# Title\n\nOne paragraph of ordinary prose.\n")
			if err != nil {
				t.Fatal(err)
			}
			seed, err := encodeDocumentTree(tree)
			if err != nil {
				t.Fatal(err)
			}
			room := boundRoomDocument(newDocumentCopy())
			if err := crdt.ApplyUpdateV1(room, seed, nil); err != nil {
				t.Fatal(err)
			}
			service := &Service{}
			if row.listed {
				replica := service.keepReplica(room, nil, nil)
				t.Cleanup(func() { runtime.KeepAlive(replica) })
				if _, _, measured := replica.size(); measured {
					t.Fatal("the room's load published a measurement, want none")
				}
			}
			paste := peerTyping(t, room, strings.Repeat("pasted ", pmdoc.MaxDocumentBytes/7+1))
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			err, called := weighInto(service, room, paste)
			requireRefusedTooLarge(t, err, called, room, before)
			if err, called := weighInto(service, room, peerTyping(t, room, strings.Repeat("typed ", 64<<10/6))); err != nil || !called {
				t.Fatalf("a 64 KiB paste was answered %v (applied %t), want it admitted", err, called)
			}
		})
	}
}
