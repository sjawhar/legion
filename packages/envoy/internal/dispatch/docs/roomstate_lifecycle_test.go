package docs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every condition unusedLocked names keeps an otherwise detached document state until that holder
// ends. Removing any clause would let a later operation on that document create a second state.
func TestEveryRoomStateHolderKeepsADetachedState(t *testing.T) {
	for _, test := range []struct {
		name    string
		hold    func(*Service, *roomState)
		release func(*roomState)
	}{
		{
			name: "live writer",
			hold: func(_ *Service, state *roomState) { state.liveWriter = &liveWrite{} },
		},
		{
			name: "running settlement",
			hold: func(_ *Service, state *roomState) { state.settling = 1 },
		},
		{
			name: "armed settlement timer",
			hold: func(service *Service, state *roomState) {
				service.settleWG.Add(1)
				timerID := service.nextSettleTimer.Add(1)
				service.registerSettleTimer(timerID)
				timer := time.NewTimer(time.Hour)
				service.attachSettleTimer(timerID, timer)
				state.settle = timer
			},
		},
		{
			name: "unsettled authors",
			hold: func(_ *Service, state *roomState) { state.unsettled = true },
		},
		{
			name: "failure recovery",
			hold: func(_ *Service, state *roomState) { state.failed = errors.New("test failure") },
		},
		{
			name: "connected browser",
			hold: func(_ *Service, state *roomState) { state.connected[1] = model.Actor{Kind: "user", ID: "alice"} },
		},
		{
			name: "persistence update",
			hold: func(_ *Service, state *roomState) { state.pendingUpdates = 1 },
		},
		{
			name:    "durable append",
			hold:    func(_ *Service, state *roomState) { state.durableAppends.Store(1) },
			release: func(state *roomState) { state.durableAppends.Store(0) },
		},
		{
			name: "version awaiting commit",
			hold: func(_ *Service, state *roomState) { state.pendingVersions[1] = versionPending{} },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, _ := newRoomReleaseService(t)
			id := "00000000-0000-4000-8000-000000000515"
			state := service.lockState(id)
			test.hold(service, state)
			service.unlockState(id, state)
			if _, held := service.rooms.Load(id); !held {
				t.Fatalf("a state holding a %s was released after its room had gone", test.name)
			}
			if test.release != nil {
				state := service.lockExistingState(id)
				test.release(state)
				service.unlockState(id, state)
			}
		})
	}
}

// A transaction's writer slot pins its state, so another transaction sees the slot and waits rather
// than taking a fresh state and a second writer slot for the same document.
func TestLiveWriterKeepsOneWriterSlotAfterItsRoomHasGone(t *testing.T) {
	service, _ := newRoomReleaseService(t)
	const id = "00000000-0000-4000-8000-000000000516"
	first, err := service.openLiveWrite(context.Background(), &Ledger{}, id)
	if err != nil {
		t.Fatalf("take the first writer slot: %v", err)
	}
	defer service.finishLiveWrite(first)
	wait, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := service.openLiveWrite(wait, &Ledger{}, id); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("take a second writer slot while the first is open: %v, want context deadline exceeded", err)
	}
}

// OnUnloadDocument is the direct release path when ygo retires a room. The persistence worker's
// exit compaction also looks the state up, so calling the registered hook directly keeps this test
// about the hook rather than that incidental backstop.
func TestOnUnloadDocumentReleasesAnOtherwiseUnusedState(t *testing.T) {
	service, _ := newRoomReleaseService(t)
	const id = "00000000-0000-4000-8000-000000000517"
	_ = service.room(id) // the test accessor deliberately leaves an otherwise unused state behind.
	hook := service.srv.OnUnloadDocument
	if hook == nil {
		t.Fatal("the ygo server has no OnUnloadDocument hook")
	}
	hook(context.Background(), id)
	if _, held := service.rooms.Load(id); held {
		t.Fatal("OnUnloadDocument left an otherwise unused document state behind")
	}
}

// A room ygo has published but not finished loading is still live: releaseIfUnusedLocked must see
// it in Rooms even before GetDoc can return it. Otherwise the load would attach to a forgotten state.
func TestAStillLoadingRoomKeepsItsState(t *testing.T) {
	service, artifactID := newTestService(t)
	load := service.srv.OnLoadDocument
	entered := make(chan struct{})
	release := make(chan struct{})
	service.srv.OnLoadDocument = func(ctx context.Context, room string, doc *crdt.Doc) error {
		if err := load(ctx, room, doc); err != nil {
			return err
		}
		if room == artifactID {
			close(entered)
			<-release
		}
		return nil
	}
	done := make(chan error, 1)
	go func() {
		done <- service.srv.Apply(context.Background(), artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {})
	}()
	<-entered
	state := service.lockExistingState(artifactID)
	if state == nil {
		t.Fatal("the state of a room still loading was released")
	}
	service.unlockState(artifactID, state)
	close(release)
	if err := <-done; err != nil && !errors.Is(err, websocket.ErrNoChanges) {
		t.Fatalf("finish loading the room: %v", err)
	}
	if err := service.Evict(context.Background(), artifactID); err != nil {
		t.Fatalf("evict the loaded room: %v", err)
	}
}
