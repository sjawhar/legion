package docs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// A transaction whose operation abandoned its fork after the transaction had already appended
// cannot write the document again once the room has committed since its read: the state its
// appends were made on is nowhere left to be read, so the fork cannot be built again
// (rebuildFork). What the caller gets then is a refusal to serve this write, not a fault of the
// server: the retry that follows reads the room afresh, and the route serves it as 503
// (DOC_SERVICE_UNAVAILABLE) rather than the 500 an error that maps to no status becomes.
//
// This pins that refusal through the path that really releases the fork - an edit that appends,
// a mark the growth weigh refuses after it has moved the fork, a browser edit the room commits,
// then a second edit - so a refusal that stopped being ErrServiceUnavailable fails here. The
// handler maps the error this write answers with (documentErrorCode); nothing here asserts on the
// sentinel itself.
func TestAnEditWhoseForkWentAndWhoseRoomHasCommittedIsRefusedAsUnavailable(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "First.\n\nSecond.\n\nThird.\n")
	// A room holds the document, so the transaction forks the room and its fork can be built
	// again only while the room stands where that fork read it (rebuildFork); a write with no
	// room forks the store, which its advisory lock keeps.
	if err := service.warmLiveDocument(context.Background(), artifactID); err != nil {
		t.Fatalf("load the document's room: %v", err)
	}
	agent := model.Actor{Kind: "session", ID: "session-0123456789abcdef"}
	// A mark whose author carries more text than a document's anchor marks may hold: the growth
	// weigh refuses it once the write has moved the fork, so the operation abandons the fork
	// (liveOperation.abandon) and leaves the transaction the update it appended before it.
	heavy := model.Actor{Kind: "session", ID: "s" + strings.Repeat("a", maxAnchorBytes)}

	refusal := joinedWrite(service, artifactID, func(ctx context.Context) error {
		first := []model.EditOp{{Op: "replace", Find: "First.", With: "First, agent."}}
		if _, err := service.ApplyOps(ctx, artifactID, first, agent, nil); err != nil {
			t.Fatalf("the agent's first edit: %v", err)
		}
		_, err := service.MarkQuote(ctx, artifactID, MarkSpec{Kind: MarkComment, ID: "c-heavy", By: heavy}, "Second", nil)
		if !errors.Is(err, ErrDocumentTooLarge) {
			t.Fatalf("the heavy mark answered %v, want the growth refusal that abandons the fork", err)
		}
		write := joinedLiveWrite(ctx, artifactID)
		if write == nil {
			t.Fatal("the transaction holds no write")
		}
		if write.cold {
			t.Fatal("the transaction forked the store, not the room its browser edits")
		}
		if write.fork != nil || len(write.updates) == 0 {
			t.Fatalf("after the refusal the write holds a fork = %t and %d appended updates, want no fork and the first edit's", write.fork != nil, len(write.updates))
		}
		// The room commits a browser's edit, which moves it past the point the transaction read
		// it at, so the fork the next operation needs cannot be built on that read again.
		editAsPeer(t, service, artifactID, replaceRun("Third.", "Third, bob."))
		second := []model.EditOp{{Op: "replace", Find: "Second.", With: "Second, agent."}}
		outcome, err := service.ApplyOps(ctx, artifactID, second, agent, nil)
		if err == nil {
			t.Fatalf("the second edit was taken although the transaction's fork was gone: %+v", outcome)
		}
		return err
	})
	if !errors.Is(refusal, ErrServiceUnavailable) {
		t.Fatalf("the refused edit answered %v, want the service-unavailable refusal a route serves as 503", refusal)
	}
}
