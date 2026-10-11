package docs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reearth/ygo/persistence"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A write to a document no room holds forks the stored document and loads no room while it runs:
// a room beside the fork would hold a second copy of a heavy document until the write commits
// (LEGION-504). Once committed and published, the room holds the write and the write keeps no fork.
func TestAColdWriteLoadsNoRoomUntilItCommits(t *testing.T) {
	for _, conditional := range []bool{false, true} {
		name := "unconditional"
		if conditional {
			name = "conditional"
		}
		t.Run(name, func(t *testing.T) {
			service, artifactID := newTestService(t)
			service.settle = time.Hour
			seedServiceText(t, service, artifactID, "Alpha paragraph.\n\nBravo paragraph.\n")
			closeRoom := func() {
				if service.srv.GetDoc(artifactID) == nil {
					return
				}
				if err := service.srv.CloseRoom(artifactID, true); err != nil {
					t.Fatalf("close the document's room: %v", err)
				}
				waitForNoLiveDocument(t, service, artifactID)
			}
			closeRoom()
			ops := []model.EditOp{{Op: "replace", Find: "Bravo", With: "Charlie"}}
			var precondition *model.EditPrecondition
			if conditional {
				token := documentToken(t, service, artifactID)
				closeRoom()
				precondition = &model.EditPrecondition{Document: token}
			}
			var write *liveWrite
			err := joinedWrite(service, func(ctx context.Context) error {
				if _, err := service.ApplyOps(ctx, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, precondition); err != nil {
					return err
				}
				if service.srv.GetDoc(artifactID) != nil {
					t.Error("the write loaded the document's room before it committed")
				}
				write = joinedLiveWrite(ctx, artifactID)
				if write == nil || write.fork == nil || !write.cold {
					t.Errorf("the write's fork is not the stored document's: %+v", write)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("apply the edit: %v", err)
			}
			if write.fork != nil || write.tree != nil || write.updates != nil {
				t.Error("the committed write kept its fork, its rendering or its updates")
			}
			if service.srv.GetDoc(artifactID) == nil {
				t.Fatal("the published write left no room holding it")
			}
			got, err := service.Text(context.Background(), artifactID)
			if err != nil {
				t.Fatalf("read the document: %v", err)
			}
			if want := "Alpha paragraph.\n\nCharlie paragraph.\n"; got != want {
				t.Fatalf("document = %q, want %q", got, want)
			}
		})
	}
}

// roomDuringColdReadStore runs opened once, inside the first store read of room after it is armed
// and before that read returns: a browser's room load takes no lock a write holds, so it can land
// there, while a cold write reads the store.
type roomDuringColdReadStore struct {
	VersionedStore
	room   string
	armed  atomic.Bool
	opened func()
}

func (s *roomDuringColdReadStore) Load(ctx context.Context, room string) (persistence.LoadResult, error) {
	result, err := s.VersionedStore.Load(ctx, room)
	if err == nil && room == s.room && s.armed.CompareAndSwap(true, false) {
		s.opened()
	}
	return result, err
}

// A browser that opens a document and edits it while a cold write is in flight keeps its edit, and
// so does the write: each lands exactly once, in the room and in the store. A room that loads while
// the write reads the store is forked from instead of the copy that read made, so the write holds
// no copy of the document beside the room's own; one that loads after the read meets the write at
// its publish (LEGION-504).
func TestABrowserOpeningADocumentMidColdWriteKeepsBothEdits(t *testing.T) {
	for _, during := range []bool{true, false} {
		name := "after the store read"
		if during {
			name = "during the store read"
		}
		t.Run(name, func(t *testing.T) {
			database := storetest.Open(t)
			artifactID := createDocument(t, database, "# First")
			store := &roomDuringColdReadStore{VersionedStore: NewPgVersioned(database), room: artifactID}
			service := New(Deps{
				Store:       database,
				Persistence: store,
				Events:      events.NewBroker(),
				Identity:    headerIdentity(database),
				ServerURL:   "https://dispatch.example",
				Settle:      time.Hour,
			})
			t.Cleanup(func() {
				if err := service.Shutdown(context.Background()); err != nil {
					t.Errorf("shutdown document service: %v", err)
				}
			})
			seedServiceText(t, service, artifactID, "Alpha paragraph.\n\nBravo paragraph.\n")
			closeDocumentRoom(t, service, artifactID)

			browserEdit := appendBlocks(t, "Browser paragraph.\n")
			browserOpens := func() {
				if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
					t.Errorf("a browser opens the document: %v", err)
					return
				}
				if err := browserStyleWrite(service, artifactID, browserEdit, make(chan struct{})); err != nil {
					t.Errorf("the browser edits the document: %v", err)
				}
			}
			if during {
				store.opened = browserOpens
				store.armed.Store(true)
			} else {
				opened := false
				service.afterForkRead = func(room string) {
					if room == artifactID && !opened && service.srv.GetDoc(artifactID) == nil {
						opened = true
						browserOpens()
					}
				}
			}

			ops := []model.EditOp{{Op: "replace", Find: "Bravo", With: "Charlie"}}
			err := joinedWrite(service, func(ctx context.Context) error {
				if _, err := service.ApplyOps(ctx, artifactID, ops, model.Actor{Kind: "user", ID: "alice"}, nil); err != nil {
					return err
				}
				room := service.srv.GetDoc(artifactID)
				if room == nil {
					t.Error("the browser's room is not resident")
					return nil
				}
				write := joinedLiveWrite(ctx, artifactID)
				if during && (write.cold || write.forkedFrom.Value() != room) {
					t.Error("the write kept a copy of the document read from the store beside the browser's room")
				}
				return nil
			})
			if err != nil {
				t.Fatalf("apply the edit: %v", err)
			}
			if during && store.armed.Load() {
				t.Fatal("the cold write never read the store")
			}
			want := "Alpha paragraph.\n\nCharlie paragraph.\n\nBrowser paragraph.\n"
			requireText(t, service, artifactID, want)
			// What the room holds is what the store holds once the room is gone.
			closeDocumentRoom(t, service, artifactID)
			requireText(t, service, artifactID, want)
		})
	}
}

// closeDocumentRoom closes artifactID's room, flushing it to the store, when one is resident.
func closeDocumentRoom(t *testing.T, service *Service, artifactID string) {
	t.Helper()
	if service.srv.GetDoc(artifactID) == nil {
		return
	}
	if err := service.srv.CloseRoom(artifactID, true); err != nil {
		t.Fatalf("close the document's room: %v", err)
	}
	waitForNoLiveDocument(t, service, artifactID)
}

// documentToken is the whole-document token a guarded edit passes.
func documentToken(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	outcome, err := joinedApplyOps(service, artifactID, nil, model.Actor{Kind: "user", ID: "alice"}, nil)
	if err != nil {
		t.Fatalf("read the document's token: %v", err)
	}
	return outcome.Token
}
