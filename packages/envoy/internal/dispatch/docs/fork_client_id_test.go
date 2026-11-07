package docs

import (
	"context"
	"testing"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/persistence"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/pmdoc"
)

type forkHistoryStore struct {
	VersionedStore
	update []byte
}

func (p *forkHistoryStore) Load(context.Context, string) (persistence.LoadResult, error) {
	return persistence.LoadResult{Update: p.update, Version: 1}, nil
}

func TestAFirstForkTracksTheClientIDItsHistoryLeavesIt(t *testing.T) {
	for _, cold := range []bool{false, true} {
		name := "warm"
		if cold {
			name = "cold"
		}
		t.Run(name, func(t *testing.T) {
			room := newDocumentCopy(crdt.WithClientID(7))
			room.Transact(typeParagraph(room, "old"))
			service := &Service{srv: newRoomServer(websocket.NewServer())}
			write := &liveWrite{artifactID: "r", clientID: 7, state: newRoomState()}
			var fork *crdt.Doc
			var err error
			if cold {
				service.persistence = &forkHistoryStore{update: crdt.EncodeStateAsUpdateV1(room, nil)}
				fork, err = service.coldFork(context.Background(), write, 1)
			} else {
				fork, err = service.warmFork(context.Background(), write, room)
			}
			if err != nil {
				t.Fatal(err)
			}
			if fork.ClientID() == 7 {
				t.Fatal("the fixture did not rotate the colliding client ID")
			}
			if write.clientID != fork.ClientID() {
				t.Fatalf("write client ID = %d, fork client ID = %d", write.clientID, fork.ClientID())
			}
			fork.Transact(typeParagraph(fork, "new"))
			tracked, err := pmdoc.AuthoredTextRuns(fork.GetXmlFragment(fragmentName), write.clientID, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(tracked) == 0 {
				t.Fatal("the write's client ID tracks none of its inserted text")
			}
		})
	}
}
