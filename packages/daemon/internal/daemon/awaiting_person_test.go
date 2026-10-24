package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

type awaitingDispatch struct {
	outboxDispatch
	issue    dispatch.Issue
	approval dispatch.Approval
	ask      dispatch.Ask
}

func (d *awaitingDispatch) GetIssue(context.Context, string) (dispatch.Issue, error) {
	return d.issue, nil
}
func (d *awaitingDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return d.approval, nil
}
func (d *awaitingDispatch) Ask(context.Context, string) (dispatch.Ask, error) { return d.ask, nil }

func TestCatchUpAwaitingAppliesAnApprovalOrClosedAskMissedAtBoot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		awaiting record.Awaiting
		approval dispatch.Approval
		ask      dispatch.Ask
		gate     *record.DesignGate
	}{
		{
			name:     "approved gate",
			awaiting: record.Awaiting{GateVersion: 3},
			approval: dispatch.Approval{State: "approved", LatestVersion: 3, Version: new(3)},
			gate:     &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3},
		},
		{
			name:     "answered ask",
			awaiting: record.Awaiting{Ask: "ask-root"},
			ask:      dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "answered"},
		},
		{
			name:     "open ask a reply handed back to the agent",
			awaiting: record.Awaiting{Ask: "ask-root"},
			ask:      dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "open", WaitingOn: "agent"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			root := record.Issue{Key: "CAPTURE-ROOT", Tree: "CAPTURE-ROOT", Project: "CAPTURE", Title: "root", Phase: phase.AwaitingPerson,
				Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Awaiting: &tc.awaiting}
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				if err := records.PutIssue(context.Background(), tx, root); err != nil {
					return err
				}
				if tc.gate != nil {
					gate := *tc.gate
					gate.Issue = root.Key
					return records.PutGate(context.Background(), tx, gate)
				}
				return nil
			}); err != nil {
				t.Fatalf("seed awaiting root: %v", err)
			}
			engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: func() time.Time { return time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) }}, quietLogger())
			admission := admit.New(records, engine, 0, "CAPTURE", quietLogger())
			client := &awaitingDispatch{issue: dispatch.Issue{Key: root.Key, LastSeq: 7}, approval: tc.approval, ask: tc.ask}
			w := &workflowRuntime{pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE", handlers: []intake.Handler{engine, admission}}

			if err := w.catchUpAwaiting(context.Background()); err != nil {
				t.Fatalf("catch up awaiting root: %v", err)
			}
			var got *record.Issue
			if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
				var err error
				got, err = records.Issue(context.Background(), tx, root.Key)
				return err
			}); err != nil {
				t.Fatalf("read caught-up root: %v", err)
			}
			if got == nil || !got.Answered() || got.ClaimSeq != 7 {
				t.Fatalf("caught-up root = %#v, want answered root with claim sequence 7", got)
			}
		})
	}
}

// An open ask still waiting on the person is no answer: the boot catch-up leaves the root waiting.
func TestCatchUpAwaitingLeavesAnAskStillWaitingOnThePerson(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := record.Issue{Key: "CAPTURE-ROOT", Tree: "CAPTURE-ROOT", Project: "CAPTURE", Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{Ask: "ask-root"}}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error { return records.PutIssue(context.Background(), tx, root) }); err != nil {
		t.Fatalf("seed awaiting root: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: func() time.Time { return time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) }}, quietLogger())
	admission := admit.New(records, engine, 0, "CAPTURE", quietLogger())
	client := &awaitingDispatch{issue: dispatch.Issue{Key: root.Key, LastSeq: 7},
		ask: dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "open", WaitingOn: "human"}}
	w := &workflowRuntime{pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE", handlers: []intake.Handler{engine, admission}}

	if err := w.catchUpAwaiting(context.Background()); err != nil {
		t.Fatalf("catch up awaiting root: %v", err)
	}
	var got *record.Issue
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		got, err = records.Issue(context.Background(), tx, root.Key)
		return err
	}); err != nil {
		t.Fatalf("read the root: %v", err)
	}
	if got == nil || !got.AwaitingPerson() || got.Answered() {
		t.Fatalf("root after the catch-up = %#v, want it still waiting on the person", got)
	}
}

type unreachableAsks struct{ awaitingDispatch }

func (*unreachableAsks) Ask(context.Context, string) (dispatch.Ask, error) {
	return dispatch.Ask{}, &dispatch.TransientError{Err: context.DeadlineExceeded}
}

// A root the catch-up cannot read is skipped and logged: one bad record, here a gate wait with no
// gate recorded, stops neither the boot nor the next root's catch-up. Dispatch being unreachable
// is the one error the boot still returns, so its readiness retry waits the outage out.
func TestCatchUpAwaitingSkipsARootItCannotReadAndStillFailsWhenDispatchIsDown(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	broken := record.Issue{Key: "CAPTURE-1", Tree: "CAPTURE-1", Project: "CAPTURE", Title: "broken", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{GateVersion: 3}}
	asking := record.Issue{Key: "CAPTURE-2", Tree: "CAPTURE-2", Project: "CAPTURE", Title: "asking", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{Ask: "ask-2"}}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(context.Background(), tx, broken); err != nil {
			return err
		}
		return records.PutIssue(context.Background(), tx, asking)
	}); err != nil {
		t.Fatalf("seed awaiting roots: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: func() time.Time { return time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) }}, quietLogger())
	admission := admit.New(records, engine, 0, "CAPTURE", quietLogger())
	client := &awaitingDispatch{issue: dispatch.Issue{Key: asking.Key, LastSeq: 7},
		ask: dispatch.Ask{ID: "ask-2", IssueKey: "CAPTURE-2", Kind: "question", State: "answered"}}
	w := &workflowRuntime{pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE", handlers: []intake.Handler{engine, admission}, log: quietLogger()}

	if err := w.catchUpAwaiting(context.Background()); err != nil {
		t.Fatalf("catch up with one unreadable root = %v, want it skipped", err)
	}
	var got *record.Issue
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		var err error
		got, err = records.Issue(context.Background(), tx, asking.Key)
		return err
	}); err != nil {
		t.Fatalf("read the asking root: %v", err)
	}
	if got == nil || !got.Answered() {
		t.Fatalf("root after the broken one = %#v, want its answer caught up", got)
	}

	w.dispatch = &unreachableAsks{awaitingDispatch{issue: dispatch.Issue{Key: asking.Key, LastSeq: 7}}}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		asking.Awaiting = &record.Awaiting{Ask: "ask-2"}
		return records.PutIssue(context.Background(), tx, asking)
	}); err != nil {
		t.Fatalf("reset the asking root: %v", err)
	}
	if err := w.catchUpAwaiting(context.Background()); !dispatch.Unreachable(err) {
		t.Fatalf("catch up with Dispatch down = %v, want an unreachable error the boot retries", err)
	}
}
