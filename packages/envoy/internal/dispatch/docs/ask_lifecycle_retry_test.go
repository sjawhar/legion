package docs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// holdUnstoredUpdate makes room hold one update its persistence has not taken, as a browser's
// edit does between reaching the room and being stored, and returns its release: the update
// landing. A settlement that meets it stops short (settleRoomWithin).
func holdUnstoredUpdate(service *Service, room string) (release func()) {
	state := service.lockState(room)
	state.pendingUpdates++
	service.unlockState(room, state)
	return func() {
		state := service.lockState(room)
		state.pendingUpdates--
		service.unlockState(room, state)
	}
}

// A settlement that meets updates its room's persistence has not stored yet stops short, and
// SettleIssueDocuments runs it again once they have landed: the block written within the settle
// delay is an ask, credited to its writer, when it returns, for the close that called it to
// withdraw. Settled once and given up on, the block would be no ask when the close committed.
func TestSettlingAnIssuesDocumentsRetriesASettlementThatMetUnstoredUpdates(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	actor := model.Actor{Kind: "session", ID: "retried-asking-session"}
	writeThroughLedger(t, service, artifactID, actor, freshBlock, withoutVersion)

	// The first attempt meets the update once it holds the document's lock; the update has landed
	// by the time the next attempt has warmed the room.
	var attempts atomic.Int32
	var release func()
	service.afterSettleLock = func(room string) {
		if room == artifactID && release == nil {
			release = holdUnstoredUpdate(service, room)
		}
	}
	service.afterSettleWarm = func(room string) {
		if room == artifactID && attempts.Add(1) > 1 && release != nil {
			release()
			release = func() {}
		}
	}
	err := service.SettleIssueDocuments(store.WithTransactionTracking(context.Background()), "DOC-1")
	service.afterSettleLock, service.afterSettleWarm = nil, nil
	if err != nil {
		t.Fatalf("settle the issue's documents: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("the document's settlement ran %d times, want 2: the one that met the update and the one after it landed", got)
	}
	ask := blockAskOn(t, service, artifactID, "fresh")
	if ask.State != "open" || ask.Author != actor {
		t.Fatalf("the settled block's ask is %s by %+v, want open by %+v", ask.State, ask.Author, actor)
	}
	requireSettledAtLatestVersion(t, service, artifactID)
}

// A settlement that stops short every time is given up after issueSettleAttempts attempts, well
// within issueSettleBudget, so the close waiting on it goes ahead; the document keeps its debt
// for the room's next load or the resumption, and its block is no ask yet.
func TestSettlingAnIssuesDocumentsGivesUpOnASettlementThatNeverLands(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	writeThroughLedger(t, service, artifactID, model.Actor{Kind: "session", ID: "stalled-session"}, freshBlock, withoutVersion)

	// The update the first attempt meets never lands while SettleIssueDocuments runs, so every
	// later attempt's wait for it ends short too.
	var attempts atomic.Int32
	var release func()
	service.afterSettleLock = func(room string) {
		if room == artifactID && release == nil {
			release = holdUnstoredUpdate(service, room)
		}
	}
	service.afterSettleWarm = func(room string) {
		if room == artifactID {
			attempts.Add(1)
		}
	}
	started := time.Now()
	err := service.SettleIssueDocuments(store.WithTransactionTracking(context.Background()), "DOC-1")
	elapsed := time.Since(started)
	service.afterSettleLock, service.afterSettleWarm = nil, nil
	if release != nil {
		release()
	}
	if err == nil {
		t.Fatal("settling a document whose update never lands succeeded")
	}
	if got := attempts.Load(); got != issueSettleAttempts {
		t.Fatalf("the document's settlement ran %d times, want %d", got, issueSettleAttempts)
	}
	if limit := issueSettleBudget + 500*time.Millisecond; elapsed >= limit {
		t.Fatalf("settling the issue's documents took %v, want less than %v", elapsed, limit)
	}
	owed, pendingErr := settlementPending(context.Background(), service.store.Pool, artifactID)
	if pendingErr != nil || !owed {
		t.Fatalf("the document owes a settlement = %t (%v), want true", owed, pendingErr)
	}
	if count := asksOn(t, service, artifactID); count != 0 {
		t.Fatalf("a settlement that never landed indexed %d asks", count)
	}
}
