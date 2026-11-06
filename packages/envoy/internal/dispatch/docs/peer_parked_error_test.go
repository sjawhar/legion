package docs

import (
	"context"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"
)

type parkedErrorStore struct {
	VersionedStore
	rows [][]byte
}

func (p *parkedErrorStore) AppendParkedUpdate(_ context.Context, _ string, update []byte) (persistence.Version, error) {
	p.rows = append(p.rows, append([]byte(nil), update...))
	return persistence.Version(len(p.rows)), nil
}

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
			service := &Service{srv: newRoomServer(websocket.NewServer()), persistence: store}
			state := newRoomState()
			state.unsettled = true
			service.rooms.Store("r", state)
			room := newDocumentCopy()
			room.Transact(typeParagraph(room, "seed"))
			replica := service.keepReplica(room, room.OpenUpdateLog(), nil)
			instance := &roomInstance{}
			replica.classify = func(logged crdt.LoggedUpdate, seq uint64, changed bool, asks func() map[string]struct{}) (bool, bool, bool) {
				return service.classifyDrained("r", instance, logged, seq, changed, asks)
			}
			room.OnAfterTransaction(func(txn *crdt.Transaction) {
				if replica.log.Pending() > 0 && len(txn.ChangedTypes()) == 0 {
					service.observeRoomUpdates("r", replica)
				}
			})
			err := service.onPeerUpdate(websocket.PeerUpdateInfo{Room: "r", Doc: room}, func() error {
				return crdt.ApplyUpdateV1(room, update, nil)
			})
			if cut && err == nil {
				t.Fatal("truncated delete set was accepted")
			}
			if !cut && err != nil {
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
