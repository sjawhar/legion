package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// awaitingDispatch is the Dispatch a boot catch-up reads: the issue, the design document's
// approval, and the root's event log, which it serves past the sequence asked for, as Dispatch's
// events route does, recording each sequence it was asked to read after.
type awaitingDispatch struct {
	outboxDispatch
	issue    dispatch.Issue
	approval dispatch.Approval
	events   []json.RawMessage
	afters   []int64
}

func (d *awaitingDispatch) GetIssue(context.Context, string) (dispatch.Issue, error) {
	return d.issue, nil
}
func (d *awaitingDispatch) Approval(context.Context, string) (dispatch.Approval, error) {
	return d.approval, nil
}
func (d *awaitingDispatch) EventsAfter(_ context.Context, _ string, after int64) ([]json.RawMessage, error) {
	d.afters = append(d.afters, after)
	var events []json.RawMessage
	for _, raw := range d.events {
		var event struct {
			Seq int64 `json:"seq"`
		}
		if err := json.Unmarshal(raw, &event); err != nil {
			return nil, err
		}
		if event.Seq > after {
			events = append(events, raw)
		}
	}
	return events, nil
}

// askEvent is one event of CAPTURE-ROOT's Dispatch event log, as Dispatch's events route serves
// it: type at seq by actor, carrying payload.
func askEvent(seq int64, eventType string, actor dispatch.Actor, payload map[string]any) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"id": 1000 + seq, "issue_key": "CAPTURE-100", "seq": seq, "type": eventType, "notify": true,
		"actor": map[string]any{"kind": actor.Kind, "id": actor.ID}, "created_at": time.Date(2026, 10, 23, 17, 0, int(seq), 0, time.UTC),
		"payload": payload,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// askClosed is an answer or resolution (state) of ask at seq, by actor.
func askClosed(seq int64, state, ask, kind string, actor dispatch.Actor) json.RawMessage {
	return askEvent(seq, "ask."+state, actor, map[string]any{"id": ask, "kind": kind})
}

// askReplied is a reply at seq in ask's thread by actor, leaving the turn waitingOn.
func askReplied(seq int64, ask, waitingOn string, actor dispatch.Actor) json.RawMessage {
	return askEvent(seq, "comment.created", actor, map[string]any{"ask_id": ask, "ask_waiting_on": waitingOn})
}

// specVersion is the event at seq of CAPTURE-100's spec document reaching version.
func specVersion(seq int64, version int) json.RawMessage {
	return askEvent(seq, "artifact.version", architectSession, map[string]any{"artifact_id": "artifact-root", "version": map[string]any{"number": version}})
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
	root := record.Issue{Key: "CAPTURE-100", Tree: "CAPTURE-100", Project: "CAPTURE", Title: "root", Phase: phase.AwaitingPerson,
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
// it (newer than the version waited on, even when a registration already recorded it on the gate
// and its own event never arrived), a person's answer or resolution of the ask, a person's reply in
// the ask's thread whatever its state, and a person's reply handing the open approval request to
// the agent.
func TestCatchUpAwaitingAppliesAPersonsAnswerMissedAtBoot(t *testing.T) {
	gate := &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3}
	for _, tc := range []struct {
		name     string
		awaiting record.Awaiting
		gate     *record.DesignGate
		client   awaitingDispatch
	}{
		{name: "approved gate", awaiting: record.Awaiting{GateVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "approved", LatestVersion: 3, Version: new(3)}}},
		{name: "changes requested on the gate's version", awaiting: record.Awaiting{GateVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3)}}},
		{name: "a newer version, whoever wrote it", awaiting: record.Awaiting{GateVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "awaiting", LatestVersion: 4, AskID: "ask-approval", WaitingOn: "agent"},
				events: []json.RawMessage{specVersion(9, 4)}}},
		{name: "a newer version a registration recorded on the gate", awaiting: record.Awaiting{GateVersion: 3},
			gate: &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 4},
			client: awaitingDispatch{approval: dispatch.Approval{State: "awaiting", LatestVersion: 4, AskID: "ask-approval", WaitingOn: "agent"},
				events: []json.RawMessage{specVersion(9, 4)}}},
		{name: "a second changes request at the version an earlier one answered", awaiting: record.Awaiting{GateVersion: 3},
			gate:   &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3, ToldChangesVersion: new(3), ToldChangesReason: "name the rollback path"},
			client: awaitingDispatch{approval: dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3), Reason: new("and name who runs the restore")}}},
		{name: "a person's reply on the approval request", awaiting: record.Awaiting{GateVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "awaiting", LatestVersion: 3, AskID: "ask-approval", WaitingOn: "agent"},
				events: []json.RawMessage{askReplied(9, "ask-approval", "agent", alice)}}},
		{name: "a person's answer", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{events: []json.RawMessage{askClosed(9, "answered", "ask-root", "question", alice)}}},
		{name: "a person's resolution", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{events: []json.RawMessage{askClosed(9, "resolved", "ask-root", "question", alice)}}},
		{name: "a person's reply handing the open ask to the agent", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{events: []json.RawMessage{askReplied(9, "ask-root", "agent", alice)}}},
		{name: "a person's reply under an ask a session resolved", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{events: []json.RawMessage{askClosed(9, "resolved", "ask-root", "question", architectSession), askReplied(10, "ask-root", "", alice)}}},
		{name: "a person's approval of the spec while the root waits on its question", awaiting: record.Awaiting{Ask: "ask-root"},
			gate:   &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "approved", LatestVersion: 3, Version: new(3), Seq: 9}}},
		{name: "a person's changes request while the root waits on its question", awaiting: record.Awaiting{Ask: "ask-root"},
			gate: &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3),
				Reason: new("name the rollback path"), Seq: 9}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waitGate := tc.gate
			if waitGate == nil && tc.awaiting.GateVersion != 0 {
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
// handing the turn to the agent, an approval request a session's reply moved to the agent, the
// changes request that answered an earlier wait, and a changes request Dispatch recorded before the
// wait began.
func TestCatchUpAwaitingLeavesARootNoPersonAnswered(t *testing.T) {
	gate := &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3}
	told := 3
	reviewed := time.Date(2026, 10, 23, 17, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		awaiting record.Awaiting
		gate     *record.DesignGate
		client   awaitingDispatch
	}{
		{name: "an ask still waiting on the person", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{events: []json.RawMessage{askReplied(9, "ask-root", "human", architectSession)}}},
		{name: "a session's resolution", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{events: []json.RawMessage{askClosed(9, "resolved", "ask-root", "question", architectSession)}}},
		{name: "settlement's retraction", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{events: []json.RawMessage{askClosed(9, "resolved", "ask-root", "question", settler)}}},
		{name: "a session's progress note", awaiting: record.Awaiting{Ask: "ask-root"},
			client: awaitingDispatch{events: []json.RawMessage{askReplied(9, "ask-root", "agent", architectSession)}}},
		{name: "an approval request a session's reply moved to the agent", awaiting: record.Awaiting{GateVersion: 3},
			client: awaitingDispatch{approval: dispatch.Approval{State: "awaiting", LatestVersion: 3, AskID: "ask-approval", WaitingOn: "agent"},
				events: []json.RawMessage{askReplied(9, "ask-approval", "agent", architectSession)}}},
		{name: "a person's answer Dispatch recorded before the wait began", awaiting: record.Awaiting{Ask: "ask-root", Since: record.EventMark{Seq: 10}},
			client: awaitingDispatch{events: []json.RawMessage{askClosed(9, "answered", "ask-root", "question", alice)}}},
		{name: "a person's reply Dispatch recorded before a registration's read began", awaiting: record.Awaiting{GateVersion: 3, Since: record.EventMark{Seq: 10}},
			client: awaitingDispatch{approval: dispatch.Approval{State: "awaiting", LatestVersion: 3, AskID: "ask-approval", WaitingOn: "human"},
				events: []json.RawMessage{askReplied(9, "ask-approval", "agent", alice), askReplied(10, "ask-approval", "human", architectSession)}}},
		{name: "a version the architect wrote before it handed the request back", awaiting: record.Awaiting{GateVersion: 3, Since: record.EventMark{Seq: 10}},
			client: awaitingDispatch{approval: dispatch.Approval{State: "awaiting", LatestVersion: 4, AskID: "ask-approval", WaitingOn: "human"},
				events: []json.RawMessage{specVersion(9, 4), askReplied(10, "ask-approval", "human", architectSession)}}},
		{name: "the changes request that answered an earlier wait", awaiting: record.Awaiting{GateVersion: 3},
			gate: &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3, ToldChangesVersion: &told, ToldChangesReason: "name the rollback path",
				ToldChangesMark: record.EventMark{Seq: 5, At: reviewed}},
			client: awaitingDispatch{approval: dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3), Reason: new("name the rollback path"), Seq: 5, At: reviewed}}},
		{name: "a changes request recorded before the wait began", awaiting: record.Awaiting{GateVersion: 3, Since: record.EventMark{Seq: 10, At: reviewed.Add(time.Minute)}},
			client: awaitingDispatch{approval: dispatch.Approval{State: "changes_requested", LatestVersion: 3, Version: new(3), Reason: new("name the rollback path"), Seq: 8, At: reviewed}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waitGate := tc.gate
			if waitGate == nil && tc.awaiting.GateVersion != 0 {
				waitGate = gate
			}
			got := catchUpRoot(t, tc.awaiting, waitGate, &tc.client)
			if got == nil || !got.AwaitingPerson() || got.Answered() {
				t.Fatalf("root after the catch-up = %#v, want it still waiting on the person", got)
			}
			if len(tc.client.afters) != 1 || tc.client.afters[0] != tc.awaiting.Since.Seq {
				t.Fatalf("event log read after %v, want one read after the wait's start %d", tc.client.afters, tc.awaiting.Since.Seq)
			}
		})
	}
}

type unreachableEvents struct{ awaitingDispatch }

func (*unreachableEvents) EventsAfter(context.Context, string, int64) ([]json.RawMessage, error) {
	return nil, &dispatch.TransientError{Err: context.DeadlineExceeded}
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
	answered, err := json.Marshal(map[string]any{"id": 1009, "issue_key": "CAPTURE-2", "seq": 9, "type": "ask.answered", "notify": true,
		"actor": map[string]any{"kind": alice.Kind, "id": alice.ID}, "created_at": time.Date(2026, 10, 23, 17, 0, 9, 0, time.UTC),
		"payload": map[string]any{"id": "ask-2", "kind": "question"}})
	if err != nil {
		t.Fatalf("encode the answer: %v", err)
	}
	client := &awaitingDispatch{issue: dispatch.Issue{Key: asking.Key, LastSeq: 7}, events: []json.RawMessage{answered}}
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

	w.dispatch = &unreachableEvents{awaitingDispatch{issue: dispatch.Issue{Key: asking.Key, LastSeq: 7}}}
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

// Successive waits at one version in one generation, each answered only by a changes request whose
// live event was lost and which Dispatch recorded after that wait began, recover one after another
// at boot: each review is a new act of a person, whether it repeats the previous review's wording
// (A, A) or an earlier one's (A, B, A). Re-reading the review that answered the last wait, with no
// new act since, answers nothing.
func TestCatchUpAwaitingRecoversSuccessiveReviewsAtOneVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reasons []string
	}{
		{name: "A, B, A", reasons: []string{"name the rollback path", "name the operator", "name the rollback path"}},
		{name: "A, A", reasons: []string{"name the rollback path", "name the rollback path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			ctx := context.Background()
			// Dispatch's event log of the root: the hand-back that starts each wait, then the
			// person's review that answers it, each event one sequence and a minute after the last.
			start := time.Date(2026, 10, 23, 17, 0, 0, 0, time.UTC)
			seq := int64(10)
			next := func() record.EventMark {
				seq++
				return record.EventMark{Seq: seq, At: start.Add(time.Duration(seq) * time.Minute)}
			}
			root := record.Issue{Key: "CAPTURE-100", Tree: "CAPTURE-100", Project: "CAPTURE", Title: "root", Phase: phase.AwaitingPerson,
				Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Awaiting: &record.Awaiting{GateVersion: 4, Since: next()}}
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				if err := records.PutIssue(ctx, tx, root); err != nil {
					return err
				}
				return records.PutGate(ctx, tx, record.DesignGate{Issue: root.Key, ArtifactID: "artifact-root", LatestVersion: 4})
			}); err != nil {
				t.Fatalf("seed awaiting root: %v", err)
			}
			engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: func() time.Time { return time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) }}, quietLogger())
			client := &awaitingDispatch{issue: dispatch.Issue{Key: root.Key, LastSeq: 7}}
			w := &workflowRuntime{pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE", handlers: []intake.Handler{engine}}
			read := func() record.Issue {
				t.Helper()
				var got *record.Issue
				if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
					var err error
					got, err = records.Issue(ctx, tx, root.Key)
					return err
				}); err != nil || got == nil {
					t.Fatalf("read root: %v", err)
				}
				return *got
			}
			// waitAgain stands for the root's resume and its architect's hand-back, an event after
			// the review that answered the previous wait: a fresh, unanswered wait on the same
			// version that begins there.
			waitAgain := func() {
				t.Helper()
				again := read()
				again.Awaiting = &record.Awaiting{GateVersion: 4, Since: next()}
				if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return records.PutIssue(ctx, tx, again) }); err != nil {
					t.Fatalf("start the next wait: %v", err)
				}
			}
			// review is the person's changes request, after the wait began, whose live event is lost:
			// only Dispatch's read of the document's latest review shows it.
			review := func(reason string) {
				mark := next()
				client.approval = dispatch.Approval{State: "changes_requested", LatestVersion: 4, Version: new(4), Reason: &reason, Seq: mark.Seq, At: mark.At}
			}
			catchUp := func() {
				t.Helper()
				if err := w.catchUpAwaiting(ctx); err != nil {
					t.Fatalf("catch up: %v", err)
				}
			}

			for i, reason := range tc.reasons {
				review(reason)
				catchUp()
				if got := read(); !got.Answered() {
					t.Fatalf("wait %d, answered only by %q, = %#v after the catch-up, want it answered", i+1, reason, got)
				}
				waitAgain()
			}
			catchUp()
			if got := read(); got.Answered() {
				t.Fatalf("a wait no new review answered = %#v after the catch-up, want it still unanswered", got)
			}
		})
	}
}

// A root waiting on its design gate is answered by a person's act on any ask of the root, and the
// boot catch-up applies every one of them: the architect is the tree's only agent while it waits,
// so a person who resolves the approval request, or answers another ask of the root, while the
// daemon is down has answered the wait as surely as a review would have.
func TestCatchUpAnswersWithAPersonsActOnAnyAskOfTheRoot(t *testing.T) {
	gate := &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3}
	for _, tc := range []struct {
		name   string
		client awaitingDispatch
	}{
		{name: "a person resolved the approval request", client: awaitingDispatch{
			approval: dispatch.Approval{State: "draft", LatestVersion: 3},
			events:   []json.RawMessage{askClosed(9, "resolved", "ask-approval", "approval", alice)},
		}},
		{name: "a person answered another ask of the root", client: awaitingDispatch{
			approval: dispatch.Approval{State: "awaiting", LatestVersion: 3, AskID: "ask-approval", WaitingOn: "human"},
			events:   []json.RawMessage{askClosed(9, "answered", "ask-question", "question", alice)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := catchUpRoot(t, record.Awaiting{GateVersion: 3}, gate, &tc.client)
			if got == nil || !got.Answered() {
				t.Fatalf("root after the catch-up = %#v, want a person's act to have answered its gate wait", got)
			}
		})
	}
}

// A version newer than the one a root waits on does not end the catch-up's read: the same
// approval may show the person's review of that newer version, whose event the daemon missed too.
// The version is applied, then the approval, so the root resumes with its gate open rather than
// with a task telling it to revise a spec the person has already approved.
func TestCatchUpAppliesTheApprovalOfANewerVersion(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := record.Issue{Key: "CAPTURE-100", Tree: "CAPTURE-100", Project: "CAPTURE", Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{GateVersion: 3}}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		return records.PutGate(ctx, tx, record.DesignGate{Issue: root.Key, ArtifactID: "artifact-root", LatestVersion: 3})
	}); err != nil {
		t.Fatalf("seed the awaiting root: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: time.Now}, quietLogger())
	admission := admit.New(records, engine, 1, "CAPTURE", quietLogger())
	client := &awaitingDispatch{
		issue:    dispatch.Issue{Key: root.Key, LastSeq: 20},
		approval: dispatch.Approval{State: "approved", LatestVersion: 4, Version: new(4), Seq: 20},
	}
	w := &workflowRuntime{pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE",
		handlers: []intake.Handler{engine, admission}, log: quietLogger()}
	if err := w.catchUpAwaiting(ctx); err != nil {
		t.Fatalf("catch up the awaiting root: %v", err)
	}

	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		got, err := records.Issue(ctx, tx, root.Key)
		if err != nil {
			return err
		}
		gate, err := records.Gate(ctx, tx, root.Key)
		if err != nil {
			return err
		}
		if got.Phase != phase.Planning || !classify.DesignGateOpen(*gate) {
			t.Errorf("Dispatch shows version 4 approved; recovered phase=%s gate=%#v, want planning with the gate open", got.Phase, gate)
		}
		// The resume task is written once, at the promotion: it must say the spec was approved,
		// never that the architect should revise it.
		rows, err := tx.Query(ctx, `select payload->>'task' from outbox where issue = $1 and kind = 'supervise' and payload->>'op' = 'start' and payload->>'role' = 'architect'`, root.Key)
		if err != nil {
			return err
		}
		defer rows.Close()
		tasks := 0
		for rows.Next() {
			var task *string
			if err := rows.Scan(&task); err != nil {
				return err
			}
			if task == nil || !strings.Contains(*task, "was approved") {
				t.Errorf("resume task = %v, want the one saying the spec was approved", task)
			}
			tasks++
		}
		if tasks != 1 {
			t.Errorf("architect starts queued = %d, want one", tasks)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read the recovered root: %v", err)
	}
}

// A registration whose read of Dispatch is older than the review the daemon already applied at
// that version strands nothing: the gate keeps the review, no wait starts, and the gate seed and
// the boot catch-up that re-read the same review leave the root where it is, with its slot.
func TestADelayedRegistrationDoesNotStrandARootAcrossASeedAndABoot(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	ctx := context.Background()
	root := record.Issue{Key: "CAPTURE-100", Tree: "CAPTURE-100", Project: "CAPTURE", Title: "root", Phase: phase.Admitted,
		Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: root.Key, Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		return records.PutGate(ctx, tx, record.DesignGate{Issue: root.Key, ArtifactID: "artifact-root", LatestVersion: 3})
	}); err != nil {
		t.Fatalf("seed the admitted root: %v", err)
	}
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: time.Now}, quietLogger())
	admission := admit.New(records, engine, 1, "CAPTURE", quietLogger())
	client := &awaitingDispatch{
		issue: dispatch.Issue{Key: root.Key, LastSeq: 11},
		approval: dispatch.Approval{State: "changes_requested", LatestVersion: 4, Version: new(4),
			Reason: new("name the rollback path"), Seq: 11},
	}
	handlers := []intake.Handler{engine, admission}
	runner := &outbox{pool: pool, dispatch: client, handlers: handlers}

	// A version-3 seed queued earlier reads the version-4 review before the registration's own,
	// older read applies.
	if err := runner.seedGate(ctx, record.OutboxRow{Issue: root.Key}, record.GateSeed{ArtifactID: "artifact-root", Version: 3, Generation: 1}); err != nil {
		t.Fatalf("seed the gate at version 3: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "review-11", intake.DispatchArtifact{Key: root.Key, ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 11}}, handlers...); err != nil {
		t.Fatalf("apply the person's review: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "api", "register-v4", intake.GateRegistered{Issue: root.Key, ArtifactID: "artifact-root",
		Version: 4, DispatchVersion: 4, Since: record.EventMark{Seq: 10}}, handlers...); err != nil {
		t.Fatalf("register the gate at version 4: %v", err)
	}
	// The version-4 seed reads that same review, and a restart catches up on the same Dispatch.
	if err := runner.seedGate(ctx, record.OutboxRow{Issue: root.Key}, record.GateSeed{ArtifactID: "artifact-root", Version: 4, Generation: 1}); err != nil {
		t.Fatalf("seed the gate at version 4: %v", err)
	}
	w := &workflowRuntime{pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE", handlers: handlers, log: quietLogger()}
	if err := w.catchUpAwaiting(ctx); err != nil {
		t.Fatalf("catch up after the restart: %v", err)
	}

	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		got, err := records.Issue(ctx, tx, root.Key)
		if err != nil {
			return err
		}
		if got.AwaitingPerson() && !got.Answered() {
			t.Errorf("the review at sequence 11 reached the live handler and the seed; the registration whose read began at 10 left the root %#v, want it admitted or answered", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the root after the boot: %v", err)
	}
}
