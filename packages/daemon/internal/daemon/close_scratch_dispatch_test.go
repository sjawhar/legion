package daemon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The daemon's own close — the done a merge writes, and the done an architect's close_root writes
// — is queued in the very transaction that takes the record out of the workflow, so the ordinary
// rule cannot judge its retry: that rule drops a row whose issue has left the workflow, which is
// every close. One refusal is enough to meet it, and refusals are ordinary here: Dispatch moves an
// issue's sequence on every event, the agent's next comment and the done reason the row itself
// posts included. The row would then be dropped, the tree would linger and free its slot, and
// Dispatch would show the issue open for good — nothing writes that status again, since the linger
// writes none and a later event only keeps the status the record holds.
//
// Both closes are driven here against the real Dispatch server, each with one unrelated event
// between the row's read and its write, and each must end with Dispatch showing done.
func TestTheDaemonsOwnCloseLandsOnScratchDispatchAfterAnEventOvertakesItsRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		close func(t *testing.T, pool *pgxpool.Pool, records record.Store, engine *workflow.Engine, issue string)
	}{
		{name: "the merge's close", close: mergeClosesTheIssue},
		{name: "an architect's close_root", close: architectClosesTheRoot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseURL, token := startScratchDispatch(t)
			issue := createScratchDispatchIssue(t, baseURL)
			setDispatchStatus(t, baseURL, issue, "in_progress")
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Linger: time.Hour}, quietLogger())
			tc.close(t, pool, records, engine, issue)

			// A person leaves a comment while the row is between its read and its write: the
			// status is untouched, and Dispatch has moved the issue's sequence on. The merge's
			// close carries no reason, so its write is refused ISSUE_CHANGED and its retry decides;
			// close_root's posts its reason and reads the issue after it, so its write lands first.
			client := &interleaving{Client: dispatch.New(baseURL, token), act: func() {
				dispatchCall(t, baseURL, http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{"body": "any news?"}, nil)
			}}
			runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, dispatch: client,
				handlers: []intake.Handler{engine}, now: time.Now, log: quietLogger()}
			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("run the close: %v", err)
			}

			if !client.done {
				t.Fatal("the comment was never made, so nothing overtook the row's read")
			}
			if got := dispatchStatusOf(t, baseURL, token, issue); got != "done" {
				t.Fatalf("Dispatch shows %s at %q, want done: the close read again and wrote", issue, got)
			}
			if rows := statusRowsOf(t, pool, issue); rows != 0 {
				t.Fatalf("status rows after the close = %d, want the close's finished", rows)
			}
		})
	}
}

// The close is owed only for the run it ended. A person who reopens the issue takes it back into
// the workflow in a generation of its own, and the done this row carries is of the run before
// that one: writing it would close the issue the person just reopened. The row names the
// generation it closed, so its retry reads the record and finishes having written nothing.
func TestAReopenedIssueKeepsTheStatusThePersonGaveItAndTheCloseWritesNothing(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	issue := createScratchDispatchIssue(t, baseURL)
	setDispatchStatus(t, baseURL, issue, "in_progress")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Linger: time.Hour}, quietLogger())
	mergeClosesTheIssue(t, pool, records, engine, issue)

	// The reopen, as a re-admission leaves it: the issue is back in the workflow in a generation
	// of its own, which is what tells this row's close from the one the record now holds.
	reopened := recordOf(t, pool, records, issue)
	reopened.Generation, reopened.Phase, reopened.Status = reopened.Generation+1, phase.Implementing, "in_progress"
	reopened.LingerUntil = nil
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutIssue(context.Background(), tx, reopened)
	}); err != nil {
		t.Fatalf("reopen the issue: %v", err)
	}

	client := &interleaving{Client: dispatch.New(baseURL, token), act: func() {
		dispatchCall(t, baseURL, http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{"body": "reopening this"}, nil)
	}}
	runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, dispatch: client,
		handlers: []intake.Handler{engine}, now: time.Now, log: quietLogger()}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the close of the reopened issue: %v", err)
	}

	if got := dispatchStatusOf(t, baseURL, token, issue); got != "in_progress" {
		t.Fatalf("Dispatch shows %s at %q, want the in_progress the reopen left: the close of the run before it writes nothing", issue, got)
	}
	if rows := statusRowsOf(t, pool, issue); rows != 0 {
		t.Fatalf("status rows after the reopen = %d, want the close's finished having written nothing", rows)
	}
}

// mergeClosesTheIssue drives the close the daemon makes when it reads the merged pull request of
// an issue in merging, and holds it to the one close row that leaves.
func mergeClosesTheIssue(t *testing.T, pool *pgxpool.Pool, records record.Store, engine *workflow.Engine, issue string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, record.Issue{Key: issue, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Tree: issue, Project: "LEGION", Title: issue, Phase: phase.Merging, Generation: 1,
			Status: "in_progress", DispatchStatus: "in_progress", HandedOver: true, Rank: "U"}); err != nil {
			return err
		}
		return records.PutPullRequest(ctx, tx, record.PullRequest{State: record.PullRequestOpen, Issue: issue,
			Repo: "acme/widgets", Number: 42, Branch: claim.IssueBranch(issue), HeadSHA: "head"})
	}); err != nil {
		t.Fatalf("seed the merging root: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "github", "merged",
		intake.PullRequestMerged{Repo: "acme/widgets", Number: 42, MergeSHA: "merge"}, engine); err != nil {
		t.Fatalf("apply the merge: %v", err)
	}
	wantsOneCloseRow(t, pool, issue)
}

// architectClosesTheRoot drives the close an architect makes of a root that has started no phase,
// and holds it to the one close row that leaves.
func architectClosesTheRoot(t *testing.T, pool *pgxpool.Pool, records record.Store, engine *workflow.Engine, issue string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		return records.PutIssue(ctx, tx, record.Issue{Key: issue, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Tree: issue, Project: "LEGION", Title: issue, Phase: phase.Admitted, Generation: 1,
			Status: "in_progress", DispatchStatus: "in_progress", HandedOver: true, Rank: "U"})
	}); err != nil {
		t.Fatalf("seed the admitted root: %v", err)
	}
	result, err := intake.ApplyFact(ctx, pool, "api", "close",
		intake.CloseRoot{Issue: issue, Reason: "the human decided no change is needed"}, engine)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply the architect's close = %+v, %v", result.Refusal, err)
	}
	wantsOneCloseRow(t, pool, issue)
}

// wantsOneCloseRow holds the close to the row it queues: one status row, of the close kind, writing
// done over the status Dispatch shows, fenced on the generation it closed. A wrong kind is
// reported and the case runs on, since what the kind costs is the write itself and the run that
// follows is where that shows.
func wantsOneCloseRow(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	var kind, status, observed string
	var generation int64
	if err := pool.QueryRow(context.Background(), `select payload->>'kind', payload->>'status', payload->>'observedStatus',
		coalesce((payload->>'closeGeneration')::bigint, 0) from outbox where kind = 'dispatch_status' and issue = $1`,
		issue).Scan(&kind, &status, &observed, &generation); err != nil {
		t.Fatalf("read the close row of %s: %v", issue, err)
	}
	if kind != string(record.CloseStatusRow) || status != "done" || observed != "in_progress" || generation != 1 {
		t.Errorf("the close row is kind %q writing %q over %q at generation %d, want a close row writing done over in_progress at generation 1",
			kind, status, observed, generation)
	}
	if rows := statusRowsOf(t, pool, issue); rows != 1 {
		t.Fatalf("status rows the close queued = %d, want one", rows)
	}
}
