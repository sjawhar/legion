package docs

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// stopSettlementShort makes each settlement of room that writes a version stop short before its
// commit, while stop answers true: its transaction rolls back having written nothing into the
// room, so the document still owes the settlement (settleRoomWithin).
func stopSettlementShort(service *Service, room string, stop func() bool) {
	service.afterSettleVersionWrite = func(settled string) error {
		if settled == room && stop() {
			return errors.New("injected settlement stop")
		}
		return nil
	}
}

// A settlement that stops short is run again by SettleIssueDocuments: the block written within the
// settle delay is an ask, credited to its writer, when it returns, for the close that called it to
// withdraw. Settled once and given up on, the block would be no ask when the close committed.
func TestSettlingAnIssuesDocumentsRetriesASettlementThatStoppedShort(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")
	actor := model.Actor{Kind: "session", ID: "retried-asking-session"}
	writeThroughLedger(t, service, artifactID, actor, freshBlock, withoutVersion)

	// The first attempt stops short before its commit; the next commits.
	var attempts atomic.Int32
	service.afterSettleWarm = func(room string) {
		if room == artifactID {
			attempts.Add(1)
		}
	}
	stopSettlementShort(service, artifactID, func() bool { return attempts.Load() == 1 })
	err := service.SettleIssueDocuments(store.WithTransactionTracking(context.Background()), "DOC-1")
	service.afterSettleVersionWrite, service.afterSettleWarm = nil, nil
	if err != nil {
		t.Fatalf("settle the issue's documents: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("the document's settlement ran %d times, want 2: the one that stopped short and the one after it", got)
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

	var attempts atomic.Int32
	service.afterSettleWarm = func(room string) {
		if room == artifactID {
			attempts.Add(1)
		}
	}
	stopSettlementShort(service, artifactID, func() bool { return true })
	started := time.Now()
	err := service.SettleIssueDocuments(store.WithTransactionTracking(context.Background()), "DOC-1")
	elapsed := time.Since(started)
	service.afterSettleVersionWrite, service.afterSettleWarm = nil, nil
	if err == nil {
		t.Fatal("settling a document whose settlement never lands succeeded")
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
