package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
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

// documentToken is the whole-document token a guarded edit passes.
func documentToken(t *testing.T, service *Service, artifactID string) string {
	t.Helper()
	outcome, err := joinedApplyOps(service, artifactID, nil, model.Actor{Kind: "user", ID: "alice"}, nil)
	if err != nil {
		t.Fatalf("read the document's token: %v", err)
	}
	return outcome.Token
}
