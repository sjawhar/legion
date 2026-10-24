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
	asks     map[string]dispatch.Ask
}

func (d *awaitingDispatch) GetIssue(context.Context, string) (dispatch.Issue, error) {
	return d.issue, nil
}
func (d *awaitingDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return d.approval, nil
}
func (d *awaitingDispatch) Ask(_ context.Context, id string) (dispatch.Ask, error) {
	if ask, ok := d.asks[id]; ok {
		return ask, nil
	}
	return d.ask, nil
}

var (
	alice            = dispatch.Actor{Kind: "user", ID: "alice"}
	architectSession = dispatch.Actor{Kind: "session", ID: "session-architect"}
	settler          = dispatch.Actor{Kind: "system", ID: "document-settlement"}
)

// catchUpRoot seeds CAPTURE-ROOT waiting on waiting, with gate when one is given, runs the boot
// catch-up against client, and returns the root as it stands after.
func catchUpRoot(t *testing.T, waiting record.Awaiting, gate *record.DesignGate, client *awaitingDispatch) *record.Issue {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	root := record.Issue{Key: "CAPTURE-ROOT", Tree: "CAPTURE-ROOT", Project: "CAPTURE", Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Awaiting: &waiting}
	if err := pgx.BeginFunc(context.Background(), pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(context.Background(), tx, root); err != nil {
			return err
		}
		if gate != nil {
			seeded := *gate
			seeded.Issue = root.Key
			return records.PutGate(context.Background(), tx, seeded)
		}
		return nil
	}); err != nil {
		t.Fatalf("seed awaiting root: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: func() time.Time { return time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) }}, quietLogger())
	admission := admit.New(records, engine, 0, "CAPTURE", quietLogger())
	client.issue = dispatch.Issue{Key: root.Key, LastSeq: 7}
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
	return got
}

// What a person did while the daemon was down answers a waiting root at boot, as its live event
// would have: an approval or a changes request of the gate's version, a newer version whoever wrote
// it, a person's answer or resolution of the ask, a person's reply in the ask's thread whatever its
// state, and a person's reply handing the open approval request to the agent.
func TestCatchUpAwaitingAppliesAPersonsAnswerMissedAtBoot(t *testing.T) {
	gate := &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3}
	for _, tc := range []struct {
		name     string
		awaiting record.Awaiting
		client   awaitingDispatch
	}{
		{name: "approved gate", awaiting: record.Awaiting{GateVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "approved", LatestVersion: 3, Version: new(3)}}},
		{name: "changes requested on the gate's version", awaiting: record.Awaiting{GateVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3)}}},
		{name: "a newer version, whoever wrote it", awaiting: record.Awaiting{GateVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "awaiting", LatestVersion: 4, AskID: "ask-approval", WaitingOn: "agent"}}},
		{name: "a person's reply on the approval request", awaiting: record.Awaiting{GateVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "awaiting", LatestVersion: 3, AskID: "ask-approval", WaitingOn: "agent"},
				asks: map[string]dispatch.Ask{"ask-approval": {ID: "ask-approval", IssueKey: "CAPTURE-ROOT", Kind: "approval", State: "open", WaitingOn: "agent", LastReplyID: "reply-1", LastReplyBy: alice}}}},
		{name: "a person's answer", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{ask: dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "answered", ClosedBy: alice}}},
		{name: "a person's resolution", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{ask: dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "resolved", ClosedBy: alice}}},
		{name: "a person's reply handing the open ask to the agent", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{ask: dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "open", WaitingOn: "agent", LastReplyID: "reply-1", LastReplyBy: alice}}},
		{name: "a person's reply under an ask a session resolved", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{ask: dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "resolved", ClosedBy: architectSession, LastReplyID: "reply-2", LastReplyBy: alice}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var waitGate *record.DesignGate
			if tc.awaiting.GateVersion != 0 {
				waitGate = gate
			}
			got := catchUpRoot(t, tc.awaiting, waitGate, &tc.client)
			if got == nil || !got.Answered() || got.ClaimSeq != 7 {
				t.Fatalf("caught-up root = %#v, want answered root with claim sequence 7", got)
			}
		})
	}
}

// What no person did leaves the root waiting at boot: an ask still waiting on the person, a
// session's or settlement's resolution with no person's reply after it, a session's progress note
// handing the turn to the agent, and an approval request a session's reply moved to the agent.
func TestCatchUpAwaitingLeavesARootNoPersonAnswered(t *testing.T) {
	gate := &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3}
	for _, tc := range []struct {
		name     string
		awaiting record.Awaiting
		client   awaitingDispatch
	}{
		{name: "an ask still waiting on the person", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{ask: dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "open", WaitingOn: "human", LastReplyID: "reply-1", LastReplyBy: architectSession}}},
		{name: "a session's resolution", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{ask: dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "resolved", ClosedBy: architectSession}}},
		{name: "settlement's retraction", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{ask: dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "resolved", ClosedBy: settler}}},
		{name: "a session's progress note", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{ask: dispatch.Ask{ID: "ask-root", IssueKey: "CAPTURE-ROOT", Kind: "question", State: "open", WaitingOn: "agent", LastReplyID: "reply-1", LastReplyBy: architectSession}}},
		{name: "an approval request a session's reply moved to the agent", awaiting: record.Awaiting{GateVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "awaiting", LatestVersion: 3, AskID: "ask-approval", WaitingOn: "agent"},
				asks: map[string]dispatch.Ask{"ask-approval": {ID: "ask-approval", IssueKey: "CAPTURE-ROOT", Kind: "approval", State: "open", WaitingOn: "agent", LastReplyID: "reply-1", LastReplyBy: architectSession}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var waitGate *record.DesignGate
			if tc.awaiting.GateVersion != 0 {
				waitGate = gate
			}
			got := catchUpRoot(t, tc.awaiting, waitGate, &tc.client)
			if got == nil || !got.AwaitingPerson() || got.Answered() {
				t.Fatalf("root after the catch-up = %#v, want it still waiting on the person", got)
			}
		})
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
		ask: dispatch.Ask{ID: "ask-2", IssueKey: "CAPTURE-2", Kind: "question", State: "answered", ClosedBy: alice}}
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
