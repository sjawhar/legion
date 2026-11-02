package docs

import (
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

// gcFloodGroups and gcFloodClient are the flood the hook's insert count is weighed against here:
// 1,024 client groups, each one GC struct of maxEditorClock clocks, the most the walk takes from a
// browser (update_walk.go), which is about fifteen bytes a group. 1,024 x 2^53 is 2^63 exactly, so
// a signed count of those clocks wraps to its most negative value, and an update whose count is
// negative reads to every rule as one that inserts nothing. The clients are past the 32 bits
// crdt.NewClientID draws, so none of them is a client the room already holds.
const (
	gcFloodGroups = 1_024
	gcFloodClient = 1 << 40
)

// A browser's frame may carry structs that are born deleted, and their lengths cost it nothing: a
// GC run declares its clocks in a varint where every other kind's length is its content's own
// bytes. Counted as inserts, a flood of them carries the hook's insert count past what int64
// holds, and both rules that read it - the at-the-bound rule and the room budget's pressure
// (weighPeerUpdate, refusedUnderPressure) - read the wrapped count as an update that inserts
// nothing, so a paste each of them refuses on its own goes in beside the flood: a 2,000,000
// character paste into a room at pmdoc.MaxDocumentBytes, and a keystroke into a room whose budget
// is full. So the count leaves clocks born deleted out of it and saturates
// (peerWeighing.weighStruct): what the flood weighs is what its content alone weighs, and each
// paste is refused with the flood as it is without it.
func TestAFloodOfGCRunsCarriesNothingPastTheHooksInsertRules(t *testing.T) {
	ordinary := "# Title\n\nOne paragraph of ordinary prose.\n"
	atTheBound := proseAtLeast(pmdoc.MaxDocumentBytes)
	paste := func(t *testing.T, room *crdt.Doc) []byte {
		return peerTyping(t, room, strings.Repeat("p", 2_000_000))
	}
	keystroke := func(t *testing.T, room *crdt.Doc) []byte { return peerTyping(t, room, "y") }
	for _, row := range []struct {
		name string
		// markdown is the document the room loaded with, and leastBytes what its rendering must
		// measure for the row to drive the state it is written for.
		markdown   string
		leastBytes int
		// busy is the room budget's signal that it is full and nothing can be closed.
		busy bool
		// update is what the browser carries, and flood whether the GC groups are carried with it.
		update func(t *testing.T, room *crdt.Doc) []byte
		flood  bool
		// refused is the code of the close the hook answers and reason its reason.
		refused int
		reason  string
	}{
		{
			name:       "a 2,000,000 character paste at the byte bound is refused",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			update:     paste,
			refused:    contracts.DocumentTooLargeCloseCode,
			reason:     contracts.DocumentTooLargeCloseReason,
		},
		{
			name:       "the same paste carried with a flood of GC runs is refused the same way",
			markdown:   atTheBound,
			leastBytes: pmdoc.MaxDocumentBytes,
			update:     paste,
			flood:      true,
			refused:    contracts.DocumentTooLargeCloseCode,
			reason:     contracts.DocumentTooLargeCloseReason,
		},
		{
			name:     "a keystroke while the room budget is full is refused as transient",
			markdown: ordinary,
			busy:     true,
			update:   keystroke,
			refused:  contracts.DocumentBusyCloseCode,
			reason:   contracts.DocumentBusyCloseReason,
		},
		{
			name:     "the same keystroke carried with a flood of GC runs is refused the same way",
			markdown: ordinary,
			busy:     true,
			update:   keystroke,
			flood:    true,
			refused:  contracts.DocumentBusyCloseCode,
			reason:   contracts.DocumentBusyCloseReason,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			service, room, replica := peerUpdateRoom(t, row.markdown, row.leastBytes)
			service.budget.full.Store(row.busy)
			update := row.update(t, room)
			carried := weighedPeer(t, room, update).inserted
			if row.flood {
				update = withGCRunFlood(t, update, gcFloodGroups)
			}
			weighed := weighedPeer(t, room, update)
			before := crdt.EncodeStateAsUpdateV1(room, nil)
			called := false
			err := service.weighPeerUpdate(
				websocket.PeerUpdateInfo{Room: "room", Doc: room, Update: update},
				func() error {
					called = true
					return crdt.ApplyUpdateV1(room, update, nil)
				},
			)
			// A live room's own update observer holds its replica, which the service lists weakly
			// (keepReplica); nothing else here does, so the measurement would be collectable from
			// under the hook.
			runtime.KeepAlive(replica)
			var refusal *websocket.PeerClose
			if !errors.As(err, &refusal) || refusal.Code != row.refused || refusal.Reason != row.reason {
				t.Fatalf("the hook answered %v (applied %t), want the close %d %q", err, called, row.refused, row.reason)
			}
			if called {
				t.Fatal("the hook called the apply of an update it refused")
			}
			if string(crdt.EncodeStateAsUpdateV1(room, nil)) != string(before) {
				t.Fatal("the refused update changed the room's document")
			}
			if weighed.inserted != carried || weighed.inserted <= 0 {
				t.Fatalf("the %d-byte frame weighs %d inserted, want the %d its content alone inserts",
					len(update), weighed.inserted, carried)
			}
		})
	}
}

// withGCRunFlood is update with groups client groups of one GC struct of maxEditorClock clocks
// each carried in front of its own groups, which leaves the update's structs and its delete set
// exactly as they were: the frame the deep review's attack sends.
func withGCRunFlood(t *testing.T, update []byte, groups int) []byte {
	t.Helper()
	tally, err := walkUpdate(update, fromPeer, nil)
	if err != nil {
		t.Fatal(err)
	}
	flooded := newV1Frame(int(tally.clients) + groups)
	for group := range groups {
		flooded.group(1, gcFloodClient+uint64(group), 0).gc(maxEditorClock)
	}
	frame := flooded.raw(update[len(varuint(tally.clients)):]...).done()
	if _, err := walkUpdate(frame, fromPeer, nil); err != nil {
		t.Fatalf("the flooded frame does not walk: %v", err)
	}
	return frame
}
