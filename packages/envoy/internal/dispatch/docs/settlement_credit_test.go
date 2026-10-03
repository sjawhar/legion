package docs

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// An ask block a session writes just before its issue closes - inside the settle delay, so no
// settlement indexed it yet - is still that session's ask once the issue reopens and the document
// settles: the asking session can reword it, and readers can tell who asked (AGENTC-150).
func TestAnAskWrittenJustBeforeItsIssueClosedKeepsItsAuthorAfterAReopen(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // the issue closes before the edit's own settlement runs
	actor := model.Actor{Kind: "session", ID: "asking-session"}
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")

	writeAskBeforeClose(t, service, artifactID, actor)
	closeTestIssue(t, service)
	reopenTestIssue(t, service)
	service.settle = 20 * time.Millisecond
	service.ScheduleSettlement(artifactID)
	waitForAskAuthor(t, service, artifactID, actor)
}

// The author persisted for a document that closes before its settlement also survives a process
// restart between the close and the reopen, where the old service's room state is unavailable.
func TestAnAskWrittenJustBeforeItsIssueClosedKeepsItsAuthorAfterRestartAndReopen(t *testing.T) {
	service, artifactID := newTestService(t)
	service.settle = time.Hour // the issue closes before the edit's own settlement runs
	ctx := context.Background()
	actor := model.Actor{Kind: "session", ID: "restarted-asking-session"}
	seedServiceText(t, service, artifactID, "# Decision\n\nContext.\n")

	writeAskBeforeClose(t, service, artifactID, actor)
	closeTestIssue(t, service)
	if err := service.Shutdown(ctx); err != nil {
		t.Fatalf("stop the service between close and reopen: %v", err)
	}
	restarted := New(Deps{Store: service.store, Events: events.NewBroker(), Settle: 20 * time.Millisecond})
	t.Cleanup(func() {
		if err := restarted.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown restarted document service: %v", err)
		}
	})
	reopenTestIssue(t, restarted)
	if err := restarted.resumeOwedSettlements(ctx, 0); err != nil {
		t.Fatalf("resume the reopened document's settlement: %v", err)
	}
	waitForAskAuthor(t, restarted, artifactID, actor)
}

func writeAskBeforeClose(t *testing.T, service *Service, artifactID string, actor model.Actor) {
	t.Helper()
	ctx := context.Background()
	tx, err := service.store.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin edit: %v", err)
	}
	defer tx.Rollback(ctx)
	joined, ledger := service.Join(ctx, tx)
	defer ledger.Discard()
	ops := []model.EditOp{{Op: "insert", After: "end", Markdown: ":::ask{#closing-ask urgency=\"med\" multiple=\"false\"}\nWho is asking?\n:::\n"}}
	if _, err := service.ApplyOps(joined, artifactID, ops, actor, nil); err != nil {
		t.Fatalf("write the ask block: %v", err)
	}
	if err := ledger.Commit(ctx); err != nil {
		t.Fatalf("commit the ask block: %v", err)
	}
	service.ScheduleSettlement(artifactID)
}

func closeTestIssue(t *testing.T, service *Service) {
	t.Helper()
	ctx := context.Background()
	if _, err := service.store.Pool.Exec(ctx, `update issues set closed_at = now() where key = 'DOC-1'`); err != nil {
		t.Fatalf("close the issue: %v", err)
	}
	service.SetIssueClosed(ctx, "DOC-1", true)
	waitFor(t, 30*time.Second, "the closed issue's room to close", func() bool {
		return len(service.srv.Rooms()) == 0
	})
}

func reopenTestIssue(t *testing.T, service *Service) {
	t.Helper()
	ctx := context.Background()
	if _, err := service.store.Pool.Exec(ctx, `update issues set closed_at = null where key = 'DOC-1'`); err != nil {
		t.Fatalf("reopen the issue: %v", err)
	}
	service.SetIssueClosed(ctx, "DOC-1", false)
}

func waitForAskAuthor(t *testing.T, service *Service, artifactID string, want model.Actor) {
	t.Helper()
	ctx := context.Background()
	var author model.Actor
	waitFor(t, 30*time.Second, "the reopened document's settlement to index the ask", func() bool {
		return service.store.Pool.QueryRow(ctx, `select author from asks where block_artifact_id = $1 and block_id = 'closing-ask'`, artifactID).Scan(&author) == nil
	})
	if author != want {
		t.Fatalf("the ask block %s wrote just before its issue closed is indexed as %+v's after the reopen, want %+v's", want.ID, author, want)
	}
}
