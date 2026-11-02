package daemon

// Review round 6's defect, pinned where it can be seen: on an ISSUE_CHANGED refusal the retry's
// read must not reach the engine, because the read carries no actor and a session's own close
// would then be taken for a person ending the tree — lingering it and freeing its slot.
//
// TestTheDaemonsReassertIsWrittenAfterDispatchRefusesItsFirstWrite asserts that by counting the
// issue's outbox rows, but it never records the issue, and an unrecorded issue has no tree to
// linger: that assertion holds whether or not the read is applied, so it does not pin the fix.
// This test records the issue in the workflow first, which is the state the defect needs, and
// then asks the same question of the record itself.

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

func TestAReassertLeavesItsRecordedIssueInTheWorkflow(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories}, quietLogger())
	admission := admit.New(records, engine, 1, "LEGION", quietLogger())
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		key   string
		wrote string
	}{
		{name: "a session closed its own issue", key: "LEGION-771", wrote: "done"},
		{name: "a session parked its own issue to backlog", key: "LEGION-772", wrote: "backlog"},
		{name: "a session parked its own issue to icebox", key: "LEGION-773", wrote: "icebox"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The issue the daemon is running: in the workflow, at the status the record kept
			// (workflow's keepStatus), which is the status the re-assert restores.
			putOutboxIssue(t, pool, records, record.Issue{
				Key: tc.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: tc.key, Title: tc.key, Phase: phase.Implementing,
				Generation: 1, Status: "in_progress",
			})

			shown := dispatch.Issue{Key: tc.key, Status: tc.wrote, Title: tc.key, Rank: "A",
				Labels: []string{dispatch.LegionLabel}, HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 4}
			reread := shown
			reread.LastSeq = shown.LastSeq + 1
			client := &refusedOnceDispatch{outboxDispatch: outboxDispatch{issue: shown}, reread: reread}
			runner := &outbox{pool: pool, records: records, dispatch: client,
				handlers: []intake.Handler{engine, admission}, log: quietLogger()}
			row := mustOutboxRow(t, tc.key, record.StatusWrite{
				Status: "in_progress", ObservedStatus: tc.wrote, Reassert: true}, time.Now())

			if err := runner.execute(ctx, row); err != nil {
				t.Fatalf("execute the re-assert: %v", err)
			}

			// The record is the thing the defect moved: applying the read would have the engine
			// read the session's close as a person's, take the issue out of the workflow and
			// linger its tree. Nothing of the read reaches the engine, so the issue stands where
			// the workflow left it.
			stored := storedIssueOf(t, ctx, pool, records, tc.key)
			if !workflow.InWorkflow(*stored) {
				t.Fatalf("the recorded issue is out of the workflow at phase %q status %q, want it still in: "+
					"the retry's read reached the engine and the session's own %s was taken for a person's",
					stored.Phase, stored.Status, tc.wrote)
			}
			if stored.Phase != phase.Implementing {
				t.Errorf("recorded phase = %q, want %q unchanged", stored.Phase, phase.Implementing)
			}
			if rows := outboxRowsOf(t, pool, tc.key); rows != 0 {
				t.Errorf("%d rows left for %s, want none: a linger would have queued its own", rows, tc.key)
			}
		})
	}
}

// A6 end to end, in one run rather than two tests: the write the close refused leaves the record
// where the workflow left it (above), and the close's own event — carrying its actor, which the
// row's read does not — then takes the issue out of the workflow. Nothing the refused write left
// behind stands in that event's way, which is the half neither test covers alone.
func TestTheCloseEventTakesTheIssueOutAfterItsWriteWasRefused(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories,
		Linger: time.Hour, Clock: func() time.Time { return time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) }}, quietLogger())
	admission := admit.New(records, engine, 1, "LEGION", quietLogger())
	ctx := context.Background()
	const key = "LEGION-781"

	putOutboxIssue(t, pool, records, record.Issue{
		Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: key, Title: key, Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "A", LastDispatchSeq: 1,
	})

	// The person's close lands between the row's read and its write.
	shown := dispatch.Issue{Key: key, Status: "done", Title: key, Rank: "A",
		Labels: []string{dispatch.LegionLabel}, HandedOver: true, ResolvedRepo: "acme/widgets", LastSeq: 4}
	reread := shown
	reread.LastSeq = shown.LastSeq + 1
	client := &refusedOnceDispatch{outboxDispatch: outboxDispatch{issue: shown}, reread: reread}
	runner := &outbox{pool: pool, records: records, dispatch: client,
		handlers: []intake.Handler{engine, admission}, log: quietLogger()}
	row := mustOutboxRow(t, key, record.StatusWrite{Status: "testing", ObservedStatus: "in_progress"}, time.Now())

	if err := runner.execute(ctx, row); err != nil {
		t.Fatalf("execute the status write: %v", err)
	}
	if len(client.statuses) != 0 {
		t.Fatalf("status writes = %v, want none: the issue left the workflow under the write", client.statuses)
	}
	// Before the event, the record still stands in the workflow. This is the test's own control:
	// the assertion after the event would pass against a record the refused write had already
	// moved, and this one says it had not.
	if before := storedIssueOf(t, ctx, pool, records, key); !workflow.InWorkflow(*before) {
		t.Fatalf("the record is already out of the workflow at phase %q before the close event, "+
			"so what follows would prove nothing about the event", before.Phase)
	}

	// Now the close's own event, with its actor, as the consumer delivers it.
	result, err := intake.ApplyFact(ctx, pool, "dispatch", "close:"+key,
		intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: 9, Type: "issue.closed",
			Status: "done", Title: key, Rank: "A"}, engine, admission)
	if err != nil || result.Refusal != nil {
		t.Fatalf("apply the close event = %#v, %v", result, err)
	}

	stored := storedIssueOf(t, ctx, pool, records, key)
	if workflow.InWorkflow(*stored) {
		t.Fatalf("the recorded issue is still in the workflow at phase %q, want the close's own event to have taken it out",
			stored.Phase)
	}
}

// storedIssueOf is the issue as the record holds it, failing the test when the record has none:
// every caller here is asking where the issue stands, and a nil would read as "not in the
// workflow", which is one of the answers under test.
func storedIssueOf(t *testing.T, ctx context.Context, pool *pgxpool.Pool, records record.Store, key string) *record.Issue {
	t.Helper()
	var stored *record.Issue
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		stored, err = records.Issue(ctx, tx, key)
		return err
	}); err != nil {
		t.Fatalf("read the recorded issue %s: %v", key, err)
	}
	if stored == nil {
		t.Fatalf("%s is not recorded at all, want it recorded", key)
	}
	return stored
}
