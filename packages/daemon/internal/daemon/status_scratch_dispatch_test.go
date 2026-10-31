package daemon

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// interleaving is the daemon's Dispatch client with one act of a person between the status row's
// read of the issue and its write: the race the row's sequence fence exists for. It acts once, so
// the read the row makes after Dispatch refuses the write sees the person's issue, not a third
// state.
type interleaving struct {
	dispatch.Client
	act  func()
	done bool
}

func (d *interleaving) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	issue, err := d.Client.GetIssue(ctx, key)
	if err != nil || d.done {
		return issue, err
	}
	d.done = true
	d.act()
	return issue, err
}

// A daemon deployed over a tree already running finds issues whose Dispatch status is not their
// phase's — a child Legion has been planning for hours that Dispatch still shows `todo` — and its
// boot queues the write that corrects each (dispatch://INFRA-714/spec R4). The write carries the
// sequence of the read the row makes as it runs, and Dispatch moves that sequence on every event,
// its own messages included, so a refusal says only that the read was overtaken: the row reads
// again and writes, and Dispatch ends at the phase's status.
func TestTheBootsStatusWriteLandsOnScratchDispatchAfterAnEventOvertakesItsRead(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	issue := createScratchDispatchIssue(t, baseURL)
	setDispatchStatus(t, baseURL, issue, "todo")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedPlanningRoot(t, pool, records, issue)
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Linger: time.Hour}, quietLogger())
	bootQueuesTheWrite(t, pool, issue, engine)

	// A person leaves a comment while the row is between its read and its write; the status is
	// untouched, but Dispatch has moved the issue's sequence on.
	client := &interleaving{Client: dispatch.New(baseURL, token), act: func() {
		dispatchCall(t, baseURL, http.MethodPost, "/api/v1/issues/"+issue+"/messages", map[string]any{"body": "any news?"}, nil)
	}}
	runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, dispatch: client, handlers: []intake.Handler{engine},
		now: time.Now, log: quietLogger()}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the boot's status write: %v", err)
	}

	if !client.done {
		t.Fatal("the comment was never made, so nothing overtook the row's read")
	}
	if got := dispatchStatusOf(t, baseURL, token, issue); got != "in_progress" {
		t.Fatalf("Dispatch shows %s at %q, want in_progress: the row read again and wrote", issue, got)
	}
	if rows := statusRowsOf(t, pool, issue); rows != 0 {
		t.Fatalf("status rows after the write = %d, want the boot's finished", rows)
	}
}

// The same write must never land over a change Dispatch recorded after the row's read. A person
// who closes the issue in that moment has ended the work, and the status the row carries is of a
// workflow the issue has left: Dispatch keeps the close, and the daemon applies it to its own
// record, which takes the issue out of the workflow rather than waiting for an event its consumer
// may deliver days later (dispatch://INFRA-714/spec R4).
func TestABootStatusWriteDoesNotLandOverACloseScratchDispatchTookAfterItsRead(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	issue := createScratchDispatchIssue(t, baseURL)
	setDispatchStatus(t, baseURL, issue, "todo")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedPlanningRoot(t, pool, records, issue)
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Linger: time.Hour}, quietLogger())
	bootQueuesTheWrite(t, pool, issue, engine)

	client := &interleaving{Client: dispatch.New(baseURL, token), act: func() {
		setDispatchStatus(t, baseURL, issue, "done")
	}}
	runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, dispatch: client, handlers: []intake.Handler{engine},
		now: time.Now, log: quietLogger()}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the boot's status write: %v", err)
	}

	if got := dispatchStatusOf(t, baseURL, token, issue); got != "done" {
		t.Fatalf("Dispatch shows %s at %q, want the person's done: the daemon wrote nothing over it", issue, got)
	}
	recorded := recordOf(t, pool, records, issue)
	if workflow.InWorkflow(recorded) {
		t.Fatalf("the record of %s = %+v, want it out of the workflow: the row's second read brought the close", issue, recorded)
	}
	if rows := statusRowsOf(t, pool, issue); rows != 0 {
		t.Fatalf("status rows after the close = %d, want the boot's finished having written nothing", rows)
	}
}

// A person's move within the workflow is as much a change made after the row's read as a close
// is: the row's first attempt writes only while Dispatch shows the status the row observed, and
// the read it makes after the refusal is held to the same comparison. A person who moves the
// issue to another lifecycle status in that moment keeps it, and the row finishes having written
// nothing (dispatch://INFRA-714/spec A6).
func TestABootStatusWriteDoesNotLandOverAPersonsMoveWithinTheWorkflow(t *testing.T) {
	baseURL, token := startScratchDispatch(t)
	issue := createScratchDispatchIssue(t, baseURL)
	setDispatchStatus(t, baseURL, issue, "todo")
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	seedPlanningRoot(t, pool, records, issue)
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Linger: time.Hour}, quietLogger())
	bootQueuesTheWrite(t, pool, issue, engine)

	client := &interleaving{Client: dispatch.New(baseURL, token), act: func() {
		setDispatchStatus(t, baseURL, issue, "needs_review")
	}}
	runner := &outbox{dispatchProject: "LEGION", pool: pool, records: records, dispatch: client, handlers: []intake.Handler{engine},
		now: time.Now, log: quietLogger()}
	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("run the boot's status write: %v", err)
	}

	if got := dispatchStatusOf(t, baseURL, token, issue); got != "needs_review" {
		t.Fatalf("Dispatch shows %s at %q, want the person's needs_review: the daemon wrote nothing over it", issue, got)
	}
	if rows := statusRowsOf(t, pool, issue); rows != 0 {
		t.Fatalf("status rows after the person's move = %d, want the boot's finished having written nothing", rows)
	}
}

// seedPlanningRoot records key as a tree of its own that the older daemon left planning while
// Dispatch still shows it todo: the state the boot corrects.
func seedPlanningRoot(t *testing.T, pool *pgxpool.Pool, records record.Store, key string) {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		return records.PutIssue(context.Background(), tx, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main",
			GitHubRepo: "acme/widgets", Tree: key, Project: "LEGION", Title: key, Phase: phase.Planning, Generation: 1,
			Status: "todo", DispatchStatus: "todo", HandedOver: true, Rank: "U"})
	}); err != nil {
		t.Fatalf("seed the planning root: %v", err)
	}
}

// bootQueuesTheWrite applies the daemon's boot and holds it to the one status write it queues for
// issue: in_progress, conditional on the todo the record holds.
func bootQueuesTheWrite(t *testing.T, pool *pgxpool.Pool, issue string, engine *workflow.Engine) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "daemon", "boot", intake.DaemonBooted{}, engine); err != nil {
		t.Fatalf("apply the boot: %v", err)
	}
	var kind, status, observed string
	if err := pool.QueryRow(context.Background(),
		"select kind, payload->>'status', payload->>'observedStatus' from outbox where issue = $1", issue).Scan(&kind, &status, &observed); err != nil {
		t.Fatalf("read the row the boot queued: %v", err)
	}
	if kind != string(record.OutboxKindDispatchStatus) || status != "in_progress" || observed != "todo" {
		t.Fatalf("the boot queued a %s row writing %q over %q, want a status write of in_progress over todo", kind, status, observed)
	}
}

// dispatchStatusOf is the status Dispatch itself shows for key.
func dispatchStatusOf(t *testing.T, baseURL, token, key string) string {
	t.Helper()
	issue, err := dispatch.New(baseURL, token).GetIssue(context.Background(), key)
	if err != nil {
		t.Fatalf("read %s from Dispatch: %v", key, err)
	}
	return issue.Status
}

// recordOf is the daemon's own record of key.
func recordOf(t *testing.T, pool *pgxpool.Pool, records record.Store, key string) record.Issue {
	t.Helper()
	var issue *record.Issue
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		issue, err = records.Issue(context.Background(), tx, key)
		return err
	}); err != nil {
		t.Fatalf("read the record of %s: %v", key, err)
	}
	if issue == nil {
		t.Fatalf("%s is not recorded", key)
	}
	return *issue
}

// statusRowsOf is how many status writes of issue the outbox still holds.
func statusRowsOf(t *testing.T, pool *pgxpool.Pool, issue string) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(),
		"select count(*) from outbox where kind = 'dispatch_status' and issue = $1", issue).Scan(&rows); err != nil {
		t.Fatalf("count the status rows of %s: %v", issue, err)
	}
	return rows
}
