package docs

import (
	"context"
	"errors"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/contracts"
)

type parkedErrorStore struct {
	VersionedStore
	rows [][]byte
}

func (p *parkedErrorStore) AppendParkedUpdate(_ context.Context, _ string, update []byte) (persistence.Version, error) {
	p.rows = append(p.rows, append([]byte(nil), update...))
	return persistence.Version(len(p.rows)), nil
}

// A peer update that parks is durable before ygo answers the browser, and an update the room
// refuses leaves nothing behind to store.
//
// The valid row is main's case: a successor whose predecessor the room lacks parks whole, the
// hook stores what it parked from the peer's own update hook (storeParked) before the apply
// returns, and a restart that replays the stored row and then the predecessor reads the text
// both edits make. The truncated row is this change's: the hook reads nothing of the frame
// itself any more, so the apply runs and ygo's own decoder refuses a cut delete set, rolling the
// whole apply back on the bounded document every room keeps (keepReplica, newRoomScans,
// reearth/ygo#340). The room is left holding none of what that frame had integrated or parked,
// so it parks nothing and stores nothing, and the hook answers 4413 DOC_UPDATE_REFUSED.
func TestAPeerApplyErrorStillStoresWhatTheRoomParked(t *testing.T) {
	for _, cut := range []bool{false, true} {
		name := "valid"
		if cut {
			name = "truncated_delete_set"
		}
		t.Run(name, func(t *testing.T) {
			writer := newDocumentCopy(crdt.WithClientID(7))
			writer.Transact(typeParagraph(writer, "base"))
			text := writer.GetXmlFragment(fragmentName).Children()[0].(*crdt.YXmlElement).Children()[0].(*crdt.YXmlText)
			predecessor := crdt.EncodeStateAsUpdateV1(writer, nil)
			before := writer.StateVector()
			writer.Transact(func(txn *crdt.Transaction) { text.Insert(txn, 4, " later", nil) })
			update := crdt.EncodeStateAsUpdateV1(writer, before)
			if cut {
				update = append(append([]byte(nil), update[:len(update)-1]...), 1, 42)
			}

			store := &parkedErrorStore{}
			// Calibrated bounds, since the hook weighs each update against them before the room
			// takes it (weighPeerUpdate): a service with none refuses every apply.
			service := calibratedService()
			service.srv, service.persistence = newRoomServer(websocket.NewServer()), store
			state := newRoomState()
			state.unsettled = true
			service.rooms.Store("r", state)
			room := newDocumentCopy()
			room.Transact(typeParagraph(room, "seed"))
			replica := service.keepReplica(room, room.OpenUpdateLog(), nil, nil)
			instance := &roomInstance{}
			replica.classify = func(logged crdt.LoggedUpdate, seq uint64, changed bool, asks func() map[string]struct{}) (bool, bool) {
				return service.classifyDrained("r", instance, logged, seq, changed, asks)
			}
			room.OnAfterTransaction(func(txn *crdt.Transaction) {
				if replica.log.Pending() > 0 && len(txn.ChangedTypes()) == 0 {
					service.observeRoomUpdates("r", replica)
				}
			})
			applied := false
			err := service.onPeerUpdate(websocket.PeerUpdateInfo{Room: "r", Doc: room, Update: update}, func() error {
				applied = true
				return crdt.ApplyUpdateV1(room, update, nil)
			})
			if cut {
				var refusal *websocket.PeerClose
				if !errors.As(err, &refusal) || refusal.Code != contracts.DocumentTooLargeCloseCode ||
					refusal.Reason != contracts.DocumentUpdateRefusedCloseReason || !applied {
					t.Fatalf("the truncated delete set was answered %v (the apply ran %t), want it run and refused %d %q",
						err, applied, contracts.DocumentTooLargeCloseCode, contracts.DocumentUpdateRefusedCloseReason)
				}
				if room.PendingStats().Items != 0 || len(state.parked) != 0 || len(store.rows) != 0 {
					t.Fatalf("the refused frame left %d parked items, %d parked rows owed and %d stored, want none of each",
						room.PendingStats().Items, len(state.parked), len(store.rows))
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if room.PendingStats().Items == 0 {
				t.Fatal("the fixture did not park a successor")
			}
			if len(state.parked) != 0 || len(store.rows) != 1 {
				t.Fatalf("parked queue = %d, stored rows = %d, want 0 and 1", len(state.parked), len(store.rows))
			}
			restarted := newDocumentCopy()
			for _, stored := range store.rows {
				if err := crdt.ApplyUpdateV1(restarted, stored, nil); err != nil {
					t.Fatal(err)
				}
			}
			if err := crdt.ApplyUpdateV1(restarted, predecessor, nil); err != nil {
				t.Fatal(err)
			}
			got, err := renderDocument(restarted)
			if err != nil {
				t.Fatal(err)
			}
			if got != "base later\n" {
				t.Fatalf("restarted document = %q, want base later", got)
			}
		})
	}
}
