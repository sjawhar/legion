package daemon

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A push whose own file list cannot show it handoff-only - GitHub cut the list short, as it does
// past 3000 files, or the listener carried no paths at all - leaves the reviewer's approval of the
// head below set aside. With no periodic pass left to compare the two heads, the push itself
// queues the compare as a durable outbox row (workflow's carryApproval), and the row carries the
// approval when GitHub shows only the issue's own handoffs changed between them: the round ends
// and the issue reaches accepting. The row is fenced by the head it names, so one left behind by a
// head that moved on compares nothing.
func TestAPushThatLeavesAnApprovalSetAsideQueuesTheCarryAsAnOutboxRow(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	seedApprovalBelowHead(t, pool, "code")
	engine := workflow.New(record.NewStore(), workflow.Config{Project: "CAPTURE", Repositories: testRepositories}, quietLogger())
	truncated, whole := ".legion/CAPTURE-1/review.json", "true"
	push := intake.Push{Repo: "acme/widgets", Branch: "legion/CAPTURE-1", Before: "code", After: "handoff",
		ChangedPaths: &truncated, Truncated: &whole}
	if _, err := intake.ApplyFact(ctx, pool, "github", "push-CAPTURE-1", push, engine); err != nil {
		t.Fatalf("apply the push: %v", err)
	}
	if got, _ := capturePhase(t, pool); got != phase.Reviewing {
		t.Fatalf("after a push that says nothing about its paths the issue is in %s, want reviewing: the approval is set aside", got)
	}
	row := onlyApprovalCarry(t, pool)
	payload, err := record.DecodeOutboxPayload(row)
	if err != nil {
		t.Fatalf("decode the approval carry row: %v", err)
	}
	carry, ok := payload.(record.ApprovalCarry)
	want := record.ApprovalCarry{Repo: "acme/widgets", Number: 86, Approved: "code", Head: "handoff"}
	if !ok || carry != want {
		t.Fatalf("the queued row carries %#v, want %#v", payload, want)
	}

	var compares atomic.Int64
	api := compareStandIn(t, http.StatusOK, `{"status":"ahead","files":[{"filename":".legion/CAPTURE-1/review.json"},{"filename":".legion/CAPTURE-1/test.json"}]}`, &compares).URL
	runner := &outbox{pool: pool, records: record.NewStore(), tokens: outboxTokens{}, githubAPI: api,
		project: "CAPTURE", dispatchProject: "CAPTURE", handlers: []intake.Handler{engine}, log: quietLogger()}
	if err := runner.approvalCarry(ctx, row, carry); err != nil {
		t.Fatalf("run the approval carry: %v", err)
	}
	if got := compares.Load(); got != 1 {
		t.Fatalf("the row compared the approved head with the head %d times, want once", got)
	}
	got, pr := capturePhase(t, pool)
	if got != phase.Accepting {
		t.Fatalf("after the carry the issue is in %s, want accepting", got)
	}
	carried := false
	for _, recorded := range pr.Pushes {
		carried = carried || (recorded.Before == "code" && recorded.SHA == "handoff" && recorded.HandoffOnly)
	}
	if !carried {
		t.Fatalf("the pushes recorded are %+v, want the handoff-only push from code to handoff", pr.Pushes)
	}

	// The same row run again, with the record moved past the head it names, compares nothing: the
	// push that left that head has a row of its own.
	setCapturePullRequest(t, pool, func(pr *record.PullRequest) { pr.HeadSHA = "later" })
	if err := runner.approvalCarry(ctx, row, carry); err != nil {
		t.Fatalf("run the approval carry again: %v", err)
	}
	if got := compares.Load(); got != 1 {
		t.Fatalf("after the head moved on the compares number %d, want the first and no more", got)
	}
}

// onlyApprovalCarry is the one approval_carry row of CAPTURE-1 in the outbox, failing the test
// when the outbox holds any other number of them.
func onlyApprovalCarry(t *testing.T, pool *pgxpool.Pool) record.OutboxRow {
	t.Helper()
	var rows []record.OutboxRow
	err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		rows, err = record.NewStore().DueRows(context.Background(), tx, "CAPTURE", "CAPTURE-1",
			[]record.OutboxKind{record.OutboxKindApprovalCarry}, time.Now().Add(time.Minute))
		return err
	})
	if err != nil {
		t.Fatalf("list the approval carry rows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the outbox holds %d approval carry rows, want one", len(rows))
	}
	return rows[0]
}
