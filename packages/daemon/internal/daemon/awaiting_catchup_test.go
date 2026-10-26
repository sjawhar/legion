package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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

// askEvent is one event of CAPTURE-100's Dispatch event log, as Dispatch's events route serves
// it: type at seq by actor, carrying payload.
func askEvent(seq int64, eventType string, actor dispatch.Actor, payload map[string]any) json.RawMessage {
	return issueEvent("CAPTURE-100", seq, eventType, actor, payload)
}

// issueEvent is one event of key's Dispatch event log: type at seq by actor, carrying payload.
func issueEvent(key string, seq int64, eventType string, actor dispatch.Actor, payload map[string]any) json.RawMessage {
	raw, err := json.Marshal(map[string]any{
		"id": 1000 + seq, "issue_key": key, "seq": seq, "type": eventType, "notify": true,
		"actor": map[string]any{"kind": actor.Kind, "id": actor.ID}, "created_at": time.Date(2026, 10, 23, 17, 0, int(seq), 0, time.UTC),
		"payload": payload,
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// askOpened is the architect's question ask opened at seq.
func askOpened(seq int64, ask string) json.RawMessage {
	return askEvent(seq, "ask.opened", architectSession, map[string]any{"id": ask, "kind": "question",
		"author": map[string]any{"kind": architectSession.Kind, "id": architectSession.ID}})
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

// specApproved is a person's approval at seq of version of CAPTURE-100's spec document.
func specApproved(seq int64, version int) json.RawMessage {
	return askEvent(seq, "artifact.approved", alice, map[string]any{"artifact_id": "artifact-root", "version": version})
}

// specChanges is a person's changes request at seq on version of CAPTURE-100's spec document.
func specChanges(seq int64, version int, reason string) json.RawMessage {
	return askEvent(seq, "artifact.changes_requested", alice, map[string]any{"artifact_id": "artifact-root", "version": version, "reason": reason})
}

var (
	alice            = dispatch.Actor{Kind: "user", ID: "alice"}
	architectSession = dispatch.Actor{Kind: "session", ID: "session-architect"}
	settler          = dispatch.Actor{Kind: "system", ID: "document-settlement"}
)

// catchUpHandout is the sequence of the last handout of the root a catch-up test seeds: the
// record's last observation of it, which StartHandout takes in.
const catchUpHandout int64 = 6

// catchUpClock is the engine clock of the catch-up tests.
func catchUpClock() time.Time { return time.Date(2026, 10, 23, 18, 0, 0, 0, time.UTC) }

// catchUpRig is the daemon's workflow runtime over a fresh store of project CAPTURE, against
// client, with admission at cap: what the boot catch-up runs on.
func catchUpRig(t *testing.T, client dispatch.Client, cap int) (*pgxpool.Pool, *workflowRuntime) {
	t.Helper()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	engine := workflow.New(records, workflow.Config{Project: "CAPTURE", Repositories: testRepositories, Clock: catchUpClock}, quietLogger())
	admission := admit.New(records, engine, cap, "CAPTURE", quietLogger())
	return pool, &workflowRuntime{pool: pool, records: records, dispatch: client, dispatchProject: "CAPTURE", engine: engine, admission: admission,
		handlers: []intake.Handler{engine, admission}, log: quietLogger(), bootID: "boot-1", catchUpRetryDelay: time.Millisecond}
}

// bootCatchUp runs the boot's catch-up as the boot does: every root it reads is held in the
// reconcile's transaction (holdForCatchUp), then caught up (catchUpAwaiting).
func bootCatchUp(t *testing.T, w *workflowRuntime) error {
	t.Helper()
	if err := pgx.BeginFunc(context.Background(), w.pool, func(tx pgx.Tx) error { return w.holdForCatchUp(context.Background(), tx) }); err != nil {
		t.Fatalf("hold the roots to catch up: %v", err)
	}
	return w.catchUpAwaiting(context.Background())
}

// seedRecord runs put in a transaction of its own.
func seedCatchUp(t *testing.T, pool *pgxpool.Pool, put func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return put(ctx, tx, record.NewStore()) }); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// captureRoot is CAPTURE-100, a root of project CAPTURE in phase at, observed last at
// catchUpHandout.
func captureRoot(at phase.Phase, waiting *record.Awaiting) record.Issue {
	return record.Issue{Key: "CAPTURE-100", Tree: "CAPTURE-100", Project: "CAPTURE", Title: "root", Phase: at,
		Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: waiting, LastDispatchSeq: catchUpHandout}
}

// readIssue is key as the record holds it.
func readIssue(t *testing.T, pool *pgxpool.Pool, key string) *record.Issue {
	t.Helper()
	var got *record.Issue
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		var err error
		got, err = records.Issue(ctx, tx, key)
		return err
	})
	return got
}

// catchUpAwaitingRoot seeds CAPTURE-100 waiting on waiting, its handout taken, with gate when one is given,
// runs the boot catch-up against client, and returns the root as it stands after.
func catchUpAwaitingRoot(t *testing.T, waiting record.Awaiting, gate *record.DesignGate, client *awaitingDispatch) *record.Issue {
	t.Helper()
	pool, w := catchUpRig(t, client, 0)
	root := captureRoot(phase.AwaitingPerson, &waiting)
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		if err := records.PutIssue(ctx, tx, root); err != nil {
			return err
		}
		if err := records.StartHandout(ctx, tx, root.Key); err != nil {
			return err
		}
		if gate != nil {
			seeded := *gate
			seeded.Issue = root.Key
			return records.PutGate(ctx, tx, seeded)
		}
		return nil
	})
	client.issue = dispatch.Issue{Key: root.Key, LastSeq: 20}
	if err := bootCatchUp(t, w); err != nil {
		t.Fatalf("catch up awaiting root: %v", err)
	}
	return readIssue(t, pool, root.Key)
}

// A root that runs at admitted when the daemon goes down may have handed work to the person
// meanwhile: the boot catch-up reads its event log from its last handout, so the question its
// architect opened is mirrored and declared as the live event would have left it, and the cursor
// moves to the sequence the read went through.
func TestCatchUpReadsAnAdmittedRootsAsksFromItsLastHandout(t *testing.T) {
	client := &awaitingDispatch{issue: dispatch.Issue{Key: "CAPTURE-100", LastSeq: 9}, events: []json.RawMessage{askOpened(9, "ask-q")}}
	pool, w := catchUpRig(t, client, 0)
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		if err := records.PutIssue(ctx, tx, captureRoot(phase.Admitted, nil)); err != nil {
			return err
		}
		if err := records.StartHandout(ctx, tx, "CAPTURE-100"); err != nil {
			return err
		}
		return seedCaptureArchitect(ctx, tx, true)
	})
	if err := bootCatchUp(t, w); err != nil {
		t.Fatalf("catch up: %v", err)
	}
	if len(client.afters) != 1 || client.afters[0] != catchUpHandout {
		t.Fatalf("event log read after %v, want one read after the last handout %d", client.afters, catchUpHandout)
	}
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		got, err := records.Issue(ctx, tx, "CAPTURE-100")
		if err != nil {
			return err
		}
		ask, err := records.RootAsk(ctx, tx, "CAPTURE-100", "ask-q")
		if err != nil {
			return err
		}
		if got.DeclaredSeq != 9 || ask == nil || !ask.WithHuman() || !ask.ByTree {
			t.Fatalf("root after the catch-up = %#v with ask %#v, want the question mirrored with the person and declared at 9", got, ask)
		}
		cursor, set, err := records.CaughtUpSeq(ctx, tx, "CAPTURE-100")
		if err != nil || !set || cursor != 9 {
			t.Fatalf("catch-up cursor = %d (set %t, %v), want 9", cursor, set, err)
		}
		return nil
	})
}

// seedCaptureArchitect records CAPTURE-100's architect claim, running a turn, in session
// session-architect, its task taken when confirmed: a root whose architect owes nothing may give
// its slot back.
func seedCaptureArchitect(ctx context.Context, tx pgx.Tx, confirmed bool) error {
	if _, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ('legion-capture-capture-100-architect', 'CAPTURE', 'CAPTURE-100', 'CAPTURE-100', 'architect', 1, 'session-architect', '', 'working', 0, 0, 0, 0)`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase, confirmed_at)
		values ('legion-capture-capture-100-architect', 'outbox:7', 'Decompose it.', now(), 1, 'admitted', case when $1 then now() end)`, confirmed)
	return err
}

// catchUpRefuser answers every read of CAPTURE-1 with status, and CAPTURE-2's normally.
type catchUpRefuser struct {
	awaitingDispatch
	status int
	reads  int
}

func (d *catchUpRefuser) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	if key == "CAPTURE-1" {
		d.reads++
		return dispatch.Issue{}, &dispatch.Error{Status: d.status, Message: http.StatusText(d.status)}
	}
	return dispatch.Issue{Key: key, LastSeq: 9}, nil
}

// waitingRoots seeds an unanswered root of project CAPTURE waiting on its question ask-<key> for
// each of keys.
func waitingRoots(t *testing.T, pool *pgxpool.Pool, keys ...string) {
	t.Helper()
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		for i, key := range keys {
			if err := records.PutIssue(ctx, tx, record.Issue{Key: key, Tree: key, Project: "CAPTURE", Title: key, Phase: phase.AwaitingPerson,
				Generation: 1, Status: "in_progress", Rank: string(rune('A' + i)), HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main",
				GitHubRepo: "acme/widgets", Awaiting: &record.Awaiting{Ask: "ask-" + key}}); err != nil {
				return err
			}
		}
		return nil
	})
}

// answeredIn is a person's answer at seq of ask-<key> in key's event log.
func answeredIn(key string, seq int64) json.RawMessage {
	return issueEvent(key, seq, "ask.answered", alice, map[string]any{"id": "ask-" + key, "kind": "question"})
}

// A root Dispatch keeps refusing does not hold the boot: an error Dispatch may stop giving (a 5xx)
// is read again a bounded number of times, a credential refusal (401, 403) not at all, and the root
// is then skipped and released, so the next root is caught up and the catch-up finishes.
func TestCatchUpSkipsARootDispatchKeepsRefusing(t *testing.T) {
	for _, tc := range []struct {
		status int
		reads  int
	}{
		{status: http.StatusBadGateway, reads: catchUpAttempts},
		{status: http.StatusServiceUnavailable, reads: catchUpAttempts},
		{status: http.StatusUnauthorized, reads: 1},
		{status: http.StatusForbidden, reads: 1},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			client := &catchUpRefuser{status: tc.status, awaitingDispatch: awaitingDispatch{events: []json.RawMessage{answeredIn("CAPTURE-2", 9)}}}
			pool, w := catchUpRig(t, client, 0)
			waitingRoots(t, pool, "CAPTURE-1", "CAPTURE-2")
			if err := bootCatchUp(t, w); err != nil {
				t.Fatalf("catch up with a root Dispatch refuses = %v, want the root skipped", err)
			}
			if client.reads != tc.reads {
				t.Fatalf("reads of the refused root = %d, want %d", client.reads, tc.reads)
			}
			if got := readIssue(t, pool, "CAPTURE-2"); got == nil || !got.Answered() {
				t.Fatalf("the root after the refused one = %#v, want its answer caught up", got)
			}
			if held := w.admission.CatchUpHeld(); len(held) != 0 {
				t.Fatalf("roots still held = %v, want the skipped one released too", held)
			}
		})
	}
}

// What a person did while the daemon was down answers a waiting root at boot, as its live event
// would have: an approval or a changes request of the gate's version, a newer version whoever wrote
// it, a person's answer or resolution of the ask, a person's reply in the ask's thread whatever its
// state, and a person's reply handing the open approval request to the agent. Every one is read
// from the root's event log, with its own sequence.
func TestCatchUpAwaitingAppliesAPersonsAnswerMissedAtBoot(t *testing.T) {
	gate := &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3, RequestAsk: "ask-approval"}
	for _, tc := range []struct {
		name     string
		awaiting record.Awaiting
		gate     *record.DesignGate
		events   []json.RawMessage
	}{
		{name: "approved gate", awaiting: record.Awaiting{GateVersion: 3, Artifact: "artifact-root"}, events: []json.RawMessage{specApproved(9, 3)}},
		{name: "changes requested on the gate's version", awaiting: record.Awaiting{GateVersion: 3, Artifact: "artifact-root"},
			events: []json.RawMessage{specChanges(9, 3, "name the rollback path")}},
		{name: "a newer version, whoever wrote it", awaiting: record.Awaiting{GateVersion: 3, Artifact: "artifact-root"}, events: []json.RawMessage{specVersion(9, 4)}},
		{name: "a newer version a registration recorded on the gate", awaiting: record.Awaiting{GateVersion: 3, Artifact: "artifact-root"},
			gate: &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 4, RequestAsk: "ask-approval"}, events: []json.RawMessage{specVersion(9, 4)}},
		{name: "a second changes request at the version an earlier one answered", awaiting: record.Awaiting{GateVersion: 3, Artifact: "artifact-root"},
			gate: &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3, RequestAsk: "ask-approval", ToldChangesVersion: new(3),
				ToldChangesReason: "name the rollback path", ToldChangesMark: record.EventMark{Seq: 5}},
			events: []json.RawMessage{specChanges(9, 3, "and name who runs the restore")}},
		{name: "a person's reply on the approval request", awaiting: record.Awaiting{GateVersion: 3, Artifact: "artifact-root"},
			events: []json.RawMessage{askReplied(9, "ask-approval", "agent", alice)}},
		{name: "a person's answer", awaiting: record.Awaiting{Ask: "ask-root"}, events: []json.RawMessage{askClosed(9, "answered", "ask-root", "question", alice)}},
		{name: "a person's resolution", awaiting: record.Awaiting{Ask: "ask-root"}, events: []json.RawMessage{askClosed(9, "resolved", "ask-root", "question", alice)}},
		{name: "a person's reply handing the open ask to the agent", awaiting: record.Awaiting{Ask: "ask-root"},
			events: []json.RawMessage{askReplied(9, "ask-root", "agent", alice)}},
		{name: "a person's reply under an ask a session resolved", awaiting: record.Awaiting{Ask: "ask-root"},
			events: []json.RawMessage{askClosed(9, "resolved", "ask-root", "question", architectSession), askReplied(10, "ask-root", "", alice)}},
		{name: "a person's approval of the spec while the root waits on its question", awaiting: record.Awaiting{Ask: "ask-root"},
			gate: &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3}, events: []json.RawMessage{specApproved(9, 3)}},
		{name: "a person's changes request while the root waits on its question", awaiting: record.Awaiting{Ask: "ask-root"},
			gate: &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3}, events: []json.RawMessage{specChanges(9, 3, "name the rollback path")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waitGate := tc.gate
			if waitGate == nil && tc.awaiting.GateVersion != 0 {
				waitGate = gate
			}
			got := catchUpAwaitingRoot(t, tc.awaiting, waitGate, &awaitingDispatch{events: tc.events})
			if got == nil || !got.Answered() {
				t.Fatalf("caught-up root = %#v, want it answered", got)
			}
		})
	}
}

// What no person did leaves the root waiting at boot: an ask still waiting on the person, a
// session's resolution of another ask, a session's progress note on another ask, the changes
// request that answered an earlier wait, and an act Dispatch recorded before the wait began. The
// log is read once, from the root's last handout, which takes in the wait's start.
func TestCatchUpAwaitingLeavesARootNoPersonAnswered(t *testing.T) {
	gate := &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3, RequestAsk: "ask-approval"}
	for _, tc := range []struct {
		name     string
		awaiting record.Awaiting
		gate     *record.DesignGate
		events   []json.RawMessage
	}{
		{name: "an ask still waiting on the person", awaiting: record.Awaiting{Ask: "ask-root"},
			events: []json.RawMessage{askReplied(9, "ask-root", "human", architectSession)}},
		{name: "a session's resolution of another ask", awaiting: record.Awaiting{Ask: "ask-root"},
			events: []json.RawMessage{askClosed(9, "resolved", "ask-other", "question", architectSession)}},
		{name: "a session's progress note on another ask", awaiting: record.Awaiting{Ask: "ask-root"},
			events: []json.RawMessage{askReplied(9, "ask-other", "agent", architectSession)}},
		{name: "a person's answer Dispatch recorded before the wait began", awaiting: record.Awaiting{Ask: "ask-root", Since: record.EventMark{Seq: 10}},
			events: []json.RawMessage{askClosed(9, "answered", "ask-root", "question", alice)}},
		{name: "a version the architect wrote before it handed the request back", awaiting: record.Awaiting{GateVersion: 3, Artifact: "artifact-root", Since: record.EventMark{Seq: 10}},
			events: []json.RawMessage{specVersion(9, 4), askReplied(10, "ask-approval", "human", architectSession)}},
		{name: "the changes request that answered an earlier wait", awaiting: record.Awaiting{GateVersion: 3, Artifact: "artifact-root", Since: record.EventMark{Seq: 8}},
			gate: &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3, RequestAsk: "ask-approval", ToldChangesVersion: new(3),
				ToldChangesReason: "name the rollback path", ToldChangesMark: record.EventMark{Seq: 7}},
			events: []json.RawMessage{specChanges(7, 3, "name the rollback path")}},
		{name: "a changes request recorded before the wait began", awaiting: record.Awaiting{GateVersion: 3, Artifact: "artifact-root", Since: record.EventMark{Seq: 10}},
			events: []json.RawMessage{specChanges(8, 3, "name the rollback path")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waitGate := tc.gate
			if waitGate == nil && tc.awaiting.GateVersion != 0 {
				waitGate = gate
			}
			client := &awaitingDispatch{events: tc.events}
			got := catchUpAwaitingRoot(t, tc.awaiting, waitGate, client)
			if got == nil || !got.AwaitingPerson() || got.Answered() {
				t.Fatalf("root after the catch-up = %#v, want it still waiting on the person", got)
			}
			if len(client.afters) != 1 || client.afters[0] != got.HandoutSeq {
				t.Fatalf("event log read after %v, want one read after the root's last handout %d", client.afters, got.HandoutSeq)
			}
		})
	}
}

type unreachableEvents struct{ awaitingDispatch }

func (*unreachableEvents) EventsAfter(context.Context, string, int64) ([]json.RawMessage, error) {
	return nil, &dispatch.TransientError{Err: context.DeadlineExceeded}
}

// Dispatch being unreachable is the one error the boot's catch-up returns, so its readiness retry
// waits the outage out, and the root it could not read stays held meanwhile.
func TestCatchUpAwaitingFailsWhileDispatchIsDown(t *testing.T) {
	client := &unreachableEvents{awaitingDispatch{issue: dispatch.Issue{Key: "CAPTURE-2", LastSeq: 9}}}
	pool, w := catchUpRig(t, client, 0)
	waitingRoots(t, pool, "CAPTURE-2")
	if err := bootCatchUp(t, w); !dispatch.Unreachable(err) {
		t.Fatalf("catch up with Dispatch down = %v, want an unreachable error the boot retries", err)
	}
	if held := w.admission.CatchUpHeld(); !slices.Equal(held, []string{"CAPTURE-2"}) {
		t.Fatalf("roots still held = %v, want the root it could not read", held)
	}
}

// slowFirstRoot is a Dispatch whose read of CAPTURE-1's issue never answers within the catch-up's
// deadline for that root, the first time it is asked, and whose approval read fails for good.
type slowFirstRoot struct {
	awaitingDispatch
	stalled bool
	asked   []string
}

func (d *slowFirstRoot) GetIssue(ctx context.Context, key string) (dispatch.Issue, error) {
	d.asked = append(d.asked, key)
	if key == "CAPTURE-1" && !d.stalled {
		d.stalled = true
		<-ctx.Done()
		return dispatch.Issue{}, ctx.Err()
	}
	return dispatch.Issue{Key: key, LastSeq: 9}, nil
}

func (d *slowFirstRoot) Approval(context.Context, string) (dispatch.Approval, error) {
	return dispatch.Approval{}, errors.New("the document is gone")
}

// EventsAfter serves the answer only in CAPTURE-2's own log.
func (d *slowFirstRoot) EventsAfter(ctx context.Context, key string, after int64) ([]json.RawMessage, error) {
	if key != "CAPTURE-2" {
		return nil, nil
	}
	return d.awaitingDispatch.EventsAfter(ctx, key, after)
}

// The boot's catch-up gives each root a deadline of its own. A root whose deadline passes makes the
// attempt transient, so the boot's readiness retry runs it again, and the retry resumes at that root
// without reading again the roots it finished. A root whose gate request read fails for good still
// has its event log replayed, so a person's answer there is caught up.
func TestCatchUpAwaitingBoundsEachRootAndResumesAfterTheRootsItFinished(t *testing.T) {
	client := &slowFirstRoot{awaitingDispatch: awaitingDispatch{events: []json.RawMessage{answeredIn("CAPTURE-2", 9)}}}
	pool, w := catchUpRig(t, client, 0)
	// The bound is the stalled root's only wait; the others apply their facts well inside it, each
	// of which runs admission's whole pass, also under a loaded suite.
	w.catchUpRootTimeout = 3 * time.Second
	waitingRoots(t, pool, "CAPTURE-0", "CAPTURE-1", "CAPTURE-2")
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		return records.PutGate(ctx, tx, record.DesignGate{Issue: "CAPTURE-2", ArtifactID: "artifact-gone", LatestVersion: 2})
	})

	if err := bootCatchUp(t, w); !dispatch.Unreachable(err) {
		t.Fatalf("catch up with a root past its deadline = %v, want a transient error the boot retries", err)
	}
	if err := w.catchUpAwaiting(context.Background()); err != nil {
		t.Fatalf("the retry = %v, want it to finish", err)
	}
	if want := []string{"CAPTURE-0", "CAPTURE-1", "CAPTURE-1", "CAPTURE-2"}; !slices.Equal(client.asked, want) {
		t.Fatalf("roots read = %v, want %v: the retry resumes at the root whose deadline passed", client.asked, want)
	}
	if got := readIssue(t, pool, "CAPTURE-2"); got == nil || !got.Answered() {
		t.Fatalf("root whose request read failed = %#v, want the answer in its event log caught up", got)
	}
	if held := w.admission.CatchUpHeld(); len(held) != 0 {
		t.Fatalf("roots still held = %v, want every root released", held)
	}
}

// Successive waits at one version in one generation, each answered only by a changes request whose
// live event was lost, recover one after another at successive boots: each review is a new act of a
// person, whether it repeats the previous review's wording (A, A) or an earlier one's (A, B, A),
// and each boot reads only the events after the cursor the last one left. A boot with no new act
// answers nothing.
func TestCatchUpAwaitingRecoversSuccessiveReviewsAtOneVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reasons []string
	}{
		{name: "A, B, A", reasons: []string{"name the rollback path", "name the operator", "name the rollback path"}},
		{name: "A, A", reasons: []string{"name the rollback path", "name the rollback path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &awaitingDispatch{}
			pool, w := catchUpRig(t, client, 0)
			seq := int64(10)
			next := func() int64 { seq++; return seq }
			root := captureRoot(phase.AwaitingPerson, &record.Awaiting{GateVersion: 4, Artifact: "artifact-root", Since: record.EventMark{Seq: next()}})
			seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
				if err := records.PutIssue(ctx, tx, root); err != nil {
					return err
				}
				if err := records.SetCaughtUp(ctx, tx, root.Key, seq); err != nil {
					return err
				}
				return records.PutGate(ctx, tx, record.DesignGate{Issue: root.Key, ArtifactID: "artifact-root", LatestVersion: 4, RequestAsk: "ask-approval"})
			})
			// waitAgain stands for the root's resume and its architect's hand-back, an event after
			// the review that answered the previous wait: a fresh, unanswered wait on the same
			// version that begins there.
			waitAgain := func() {
				t.Helper()
				again := *readIssue(t, pool, root.Key)
				again.Awaiting = &record.Awaiting{GateVersion: 4, Artifact: "artifact-root", Since: record.EventMark{Seq: next()}}
				seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
					return records.PutIssue(ctx, tx, again)
				})
			}
			// Each catch-up is another boot's: a boot's own retries keep the roots it finished
			// (caughtUp), a new boot starts with none.
			catchUp := func() {
				t.Helper()
				w.caughtUp = nil
				client.issue = dispatch.Issue{Key: root.Key, LastSeq: seq}
				if err := bootCatchUp(t, w); err != nil {
					t.Fatalf("catch up: %v", err)
				}
			}
			for i, reason := range tc.reasons {
				client.events = append(client.events, specChanges(next(), 4, reason))
				catchUp()
				if got := readIssue(t, pool, root.Key); !got.Answered() {
					t.Fatalf("wait %d, answered only by %q, = %#v after the catch-up, want it answered", i+1, reason, got)
				}
				waitAgain()
			}
			catchUp()
			if got := readIssue(t, pool, root.Key); got.Answered() {
				t.Fatalf("a wait no new review answered = %#v after the catch-up, want it still unanswered", got)
			}
			if last := client.afters[len(client.afters)-1]; last != seq-1 {
				t.Fatalf("the last boot read after %d, want after the cursor the boot before it left (%d)", last, seq-1)
			}
		})
	}
}

// A root waiting on its design gate is answered by a person's act on any ask of the root, and the
// boot catch-up applies every one of them: the architect is the tree's only agent while it waits,
// so a person who resolves the approval request, or answers another ask of the root, while the
// daemon is down has answered the wait as surely as a review would have.
func TestCatchUpAnswersWithAPersonsActOnAnyAskOfTheRoot(t *testing.T) {
	gate := &record.DesignGate{ArtifactID: "artifact-root", LatestVersion: 3, RequestAsk: "ask-approval"}
	for _, tc := range []struct {
		name   string
		events []json.RawMessage
	}{
		{name: "a person resolved the approval request", events: []json.RawMessage{askClosed(9, "resolved", "ask-approval", "approval", alice)}},
		{name: "a person answered another ask of the root", events: []json.RawMessage{askClosed(9, "answered", "ask-question", "question", alice)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := catchUpAwaitingRoot(t, record.Awaiting{GateVersion: 3, Artifact: "artifact-root"}, gate, &awaitingDispatch{events: tc.events})
			if got == nil || !got.Answered() {
				t.Fatalf("root after the catch-up = %#v, want a person's act to have answered its gate wait", got)
			}
		})
	}
}

// A version newer than the one a root waits on, and the person's approval of it after, both missed
// while the daemon was down, are applied in the order Dispatch recorded them, and the root is
// resumed only once its whole window is applied: it resumes with its gate open, never with a task
// telling it to revise a spec the person has already approved.
func TestCatchUpAppliesTheApprovalOfANewerVersion(t *testing.T) {
	client := &awaitingDispatch{issue: dispatch.Issue{Key: "CAPTURE-100", LastSeq: 20}, events: []json.RawMessage{specVersion(19, 4), specApproved(20, 4)}}
	pool, w := catchUpRig(t, client, 1)
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		if err := records.PutIssue(ctx, tx, captureRoot(phase.AwaitingPerson, &record.Awaiting{GateVersion: 3, Artifact: "artifact-root", Since: record.EventMark{Seq: 10}})); err != nil {
			return err
		}
		return records.PutGate(ctx, tx, record.DesignGate{Issue: "CAPTURE-100", ArtifactID: "artifact-root", LatestVersion: 3, RequestAsk: "ask-approval"})
	})
	if err := bootCatchUp(t, w); err != nil {
		t.Fatalf("catch up the awaiting root: %v", err)
	}
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		got, err := records.Issue(ctx, tx, "CAPTURE-100")
		if err != nil {
			return err
		}
		gate, err := records.Gate(ctx, tx, "CAPTURE-100")
		if err != nil {
			return err
		}
		if got.Phase != phase.Planning || !classify.DesignGateOpen(*gate) {
			t.Errorf("Dispatch shows version 4 approved; recovered phase=%s gate=%#v, want planning with the gate open", got.Phase, gate)
		}
		return nil
	})
	tasks := captureArchitectTasks(t, pool)
	if len(tasks) != 1 || !strings.Contains(tasks[0], "approved at version 4") || strings.Contains(tasks[0], "Hand") || strings.Contains(tasks[0], "revise") {
		t.Fatalf("architect starts = %q, want one, saying the spec is approved at version 4 and asking for no revision", tasks)
	}
}

// captureArchitectTasks is the task of every architect start the outbox holds for CAPTURE-100.
func captureArchitectTasks(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var tasks []string
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, _ *record.Postgres) error {
		rows, err := tx.Query(ctx, `select coalesce(payload->>'task', '') from outbox where issue = 'CAPTURE-100' and kind = 'supervise'
			and payload->>'op' = 'start' and payload->>'role' = 'architect' order by id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var task string
			if err := rows.Scan(&task); err != nil {
				return err
			}
			tasks = append(tasks, task)
		}
		return rows.Err()
	})
	return tasks
}

// captureArchitectOps is every supervise operation the outbox holds for CAPTURE-100's architect.
func captureArchitectOps(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var ops []string
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, _ *record.Postgres) error {
		rows, err := tx.Query(ctx, `select payload->>'op' from outbox where issue = 'CAPTURE-100' and kind = 'supervise'
			and payload->>'role' = 'architect' order by id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var op string
			if err := rows.Scan(&op); err != nil {
				return err
			}
			ops = append(ops, op)
		}
		return rows.Err()
	})
	return ops
}

// captureNotices counts the outbox rows of CAPTURE-100 that tell anyone anything: architect
// notices and controller notices.
func captureNotices(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var kinds []string
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, _ *record.Postgres) error {
		rows, err := tx.Query(ctx, `select kind || ':' || coalesce(payload->>'kind', '') from outbox where issue = 'CAPTURE-100'
			and kind in ('notice', 'controller_notice') order by id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind string
			if err := rows.Scan(&kind); err != nil {
				return err
			}
			kinds = append(kinds, kind)
		}
		return rows.Err()
	})
	return kinds
}

// slottedCapture seeds CAPTURE-100 admitted in the one slot with its architect running a task it
// took, and CAPTURE-200 waiting in line behind it, with gate when one is given; cursor names the
// root's catch-up cursor, nil for a root recorded before the cursor existed.
func slottedCapture(t *testing.T, pool *pgxpool.Pool, gate *record.DesignGate, cursor *int64) {
	t.Helper()
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		if err := records.PutIssue(ctx, tx, captureRoot(phase.Admitted, nil)); err != nil {
			return err
		}
		if err := records.StartHandout(ctx, tx, "CAPTURE-100"); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: "CAPTURE-100", Index: 0, AdmittedAt: catchUpClock()}); err != nil {
			return err
		}
		if err := records.PutIssue(ctx, tx, record.Issue{Key: "CAPTURE-200", Tree: "CAPTURE-200", Project: "CAPTURE", Title: "next", Phase: phase.Admitted,
			Generation: 1, Status: "todo", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", LastDispatchSeq: 1}); err != nil {
			return err
		}
		if gate != nil {
			if err := records.PutGate(ctx, tx, *gate); err != nil {
				return err
			}
		}
		if cursor == nil {
			if _, err := tx.Exec(ctx, `update issues set caught_up_seq = null where key = 'CAPTURE-100'`); err != nil {
				return err
			}
		} else if err := records.SetCaughtUp(ctx, tx, "CAPTURE-100", *cursor); err != nil {
			return err
		}
		return seedCaptureArchitect(ctx, tx, true)
	})
}

// assertCaptureKeptItsSlot fails unless CAPTURE-100 is admitted with no wait and still holds the
// one slot, and its architect was neither suspended nor started.
func assertCaptureKeptItsSlot(t *testing.T, pool *pgxpool.Pool) *record.Issue {
	t.Helper()
	got := readIssue(t, pool, "CAPTURE-100")
	if got == nil || got.Phase != phase.Admitted || got.Awaiting != nil {
		t.Fatalf("root after the catch-up = %#v, want it admitted with no wait", got)
	}
	var slotted []string
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		slots, err := records.Slots(ctx, tx)
		for _, slot := range slots {
			slotted = append(slotted, slot.Issue)
		}
		return err
	})
	if !slices.Equal(slotted, []string{"CAPTURE-100"}) {
		t.Fatalf("slots = %v, want CAPTURE-100 alone to hold the one slot", slotted)
	}
	if ops := captureArchitectOps(t, pool); len(ops) != 0 {
		t.Fatalf("architect operations = %v, want neither a suspend nor a start", ops)
	}
	return got
}

// The first boot after the upgrade replays nothing of a root recorded before the cursor existed:
// a daemon of the earlier release applied that history live, and replayed through this release's
// handlers it would read a question answered long ago as a fresh hand-back, suspend the running
// architect, give its slot away, resume it with a stale task and tell a changes request again. The
// boot only sets the cursor to the issue's latest sequence; the next boot reads from there.
func TestTheFirstBootAfterTheUpgradeReplaysNothing(t *testing.T) {
	gate := &record.DesignGate{Issue: "CAPTURE-100", ArtifactID: "artifact-root", LatestVersion: 3}
	client := &awaitingDispatch{issue: dispatch.Issue{Key: "CAPTURE-100", LastSeq: 9}, approval: dispatch.Approval{State: "changes_requested",
		LatestVersion: 3, Version: new(3), Reason: new("name the rollback path"), Seq: 9},
		events: []json.RawMessage{askOpened(3, "ask-q1"), askClosed(5, "answered", "ask-q1", "question", alice), askOpened(8, "ask-q2"),
			specChanges(9, 3, "name the rollback path")}}
	pool, w := catchUpRig(t, client, 1)
	slottedCapture(t, pool, gate, nil)
	if err := bootCatchUp(t, w); err != nil {
		t.Fatalf("catch up: %v", err)
	}
	assertCaptureKeptItsSlot(t, pool)
	if len(client.afters) != 0 {
		t.Fatalf("event log read after %v, want no read on the first boot", client.afters)
	}
	if notices := captureNotices(t, pool); len(notices) != 0 {
		t.Fatalf("notices = %v, want none", notices)
	}
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		cursor, set, err := records.CaughtUpSeq(ctx, tx, "CAPTURE-100")
		if err != nil || !set || cursor != 9 {
			t.Fatalf("catch-up cursor = %d (set %t, %v), want the issue's latest sequence 9", cursor, set, err)
		}
		return nil
	})
}

// A question its architect opened and a person answered while the daemon was down is applied at
// the boot with the root held out of admission's decision until the whole window is in: one pass
// then finds nothing still with the person and takes the hand-back in, so the running architect,
// which had the answer through Dispatch already, is neither suspended nor resumed and keeps its
// slot. A window that leaves a second question with the person is decided once, on that question.
func TestAnOutageWindowIsDecidedOnceAfterItsCatchUp(t *testing.T) {
	cursor := catchUpHandout
	t.Run("answered within the window", func(t *testing.T) {
		client := &awaitingDispatch{issue: dispatch.Issue{Key: "CAPTURE-100", LastSeq: 9},
			events: []json.RawMessage{askOpened(7, "ask-q1"), askClosed(9, "answered", "ask-q1", "question", alice)}}
		pool, w := catchUpRig(t, client, 1)
		slottedCapture(t, pool, nil, &cursor)
		if err := bootCatchUp(t, w); err != nil {
			t.Fatalf("catch up: %v", err)
		}
		if got := assertCaptureKeptItsSlot(t, pool); got.DeclaredSeq != 0 {
			t.Fatalf("declared_seq = %d, want the answered hand-back taken in", got.DeclaredSeq)
		}
		if notices := captureNotices(t, pool); len(notices) != 0 {
			t.Fatalf("notices = %v, want none", notices)
		}
	})
	t.Run("a second question left with the person", func(t *testing.T) {
		client := &awaitingDispatch{issue: dispatch.Issue{Key: "CAPTURE-100", LastSeq: 11},
			events: []json.RawMessage{askOpened(7, "ask-q1"), askClosed(9, "answered", "ask-q1", "question", alice), askOpened(11, "ask-q2")}}
		pool, w := catchUpRig(t, client, 1)
		slottedCapture(t, pool, nil, &cursor)
		if err := bootCatchUp(t, w); err != nil {
			t.Fatalf("catch up: %v", err)
		}
		got := readIssue(t, pool, "CAPTURE-100")
		if got == nil || !got.AwaitingPerson() || got.Awaiting.Ask != "ask-q2" || got.Awaiting.Since.Seq != 11 || got.Answered() {
			t.Fatalf("root after the catch-up = %#v, want it waiting on ask-q2 from 11, unanswered", got)
		}
		if ops := captureArchitectOps(t, pool); !slices.Equal(ops, []string{"suspend"}) {
			t.Fatalf("architect operations = %v, want the one suspend of the one decision", ops)
		}
	})
}

// A root already waiting on its gate's approval at the deploy returns its slot at the first boot:
// its gate, registered by a daemon of the earlier release, names no approval request, so the boot
// binds it to the request Dispatch shows open and, with that request on the person's turn, declares
// the hand-back as a registration would. A request Dispatch shows on the agent's turn hands nothing
// back, and the root keeps its slot.
func TestARootWaitingOnItsGateAtDeployReturnsItsSlot(t *testing.T) {
	for _, tc := range []struct {
		name      string
		waitingOn string
		waits     bool
	}{
		{name: "the request with the person", waitingOn: "human", waits: true},
		{name: "the request with the agent", waitingOn: "agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &awaitingDispatch{issue: dispatch.Issue{Key: "CAPTURE-100", LastSeq: 9},
				approval: dispatch.Approval{State: "awaiting", LatestVersion: 3, AskID: "ask-approval", WaitingOn: tc.waitingOn}}
			pool, w := catchUpRig(t, client, 1)
			slottedCapture(t, pool, &record.DesignGate{Issue: "CAPTURE-100", ArtifactID: "artifact-root", LatestVersion: 3}, nil)
			if err := bootCatchUp(t, w); err != nil {
				t.Fatalf("catch up: %v", err)
			}
			var gate *record.DesignGate
			seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
				var err error
				gate, err = records.Gate(ctx, tx, "CAPTURE-100")
				return err
			})
			if gate == nil || gate.RequestAsk != "ask-approval" {
				t.Fatalf("gate after the catch-up = %#v, want it bound to ask-approval", gate)
			}
			if !tc.waits {
				assertCaptureKeptItsSlot(t, pool)
				return
			}
			got := readIssue(t, pool, "CAPTURE-100")
			if got == nil || !got.AwaitingPerson() || got.Awaiting.GateVersion != 3 || got.Awaiting.Artifact != "artifact-root" || got.Awaiting.Since.Seq != 9 {
				t.Fatalf("root after the catch-up = %#v, want it waiting on version 3 of artifact-root from 9", got)
			}
			if ops := captureArchitectOps(t, pool); !slices.Equal(ops, []string{"suspend"}) {
				t.Fatalf("architect operations = %v, want one suspend", ops)
			}
			if !slotHeld(t, pool, "CAPTURE-200") || slotHeld(t, pool, "CAPTURE-100") {
				t.Fatalf("slot held by CAPTURE-200 %t, by CAPTURE-100 %t, want the root waiting in line to take the one slot",
					slotHeld(t, pool, "CAPTURE-200"), slotHeld(t, pool, "CAPTURE-100"))
			}
		})
	}
}

// slotHeld says whether key holds a slot.
func slotHeld(t *testing.T, pool *pgxpool.Pool, key string) bool {
	t.Helper()
	held := false
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		slots, err := records.Slots(ctx, tx)
		for _, slot := range slots {
			held = held || slot.Issue == key
		}
		return err
	})
	return held
}

// A registration whose read of Dispatch is older than the review the daemon already applied at
// that version strands nothing: the gate keeps the review, no wait starts, and the gate seed and
// the boot catch-up that re-read the same review leave the root where it is, with its slot.
func TestADelayedRegistrationDoesNotStrandARootAcrossASeedAndABoot(t *testing.T) {
	client := &awaitingDispatch{
		issue: dispatch.Issue{Key: "CAPTURE-100", LastSeq: 11},
		approval: dispatch.Approval{State: "changes_requested", LatestVersion: 4, Version: new(4),
			Reason: new("name the rollback path"), Seq: 11},
		events: []json.RawMessage{specChanges(11, 4, "name the rollback path")},
	}
	pool, w := catchUpRig(t, client, 1)
	seedCatchUp(t, pool, func(ctx context.Context, tx pgx.Tx, records *record.Postgres) error {
		if err := records.PutIssue(ctx, tx, captureRoot(phase.Admitted, nil)); err != nil {
			return err
		}
		if err := records.PutSlot(ctx, tx, record.Slot{Issue: "CAPTURE-100", Index: 0, AdmittedAt: time.Now()}); err != nil {
			return err
		}
		return records.PutGate(ctx, tx, record.DesignGate{Issue: "CAPTURE-100", ArtifactID: "artifact-root", LatestVersion: 3})
	})
	ctx := context.Background()
	runner := &outbox{pool: pool, dispatch: client, handlers: w.handlers}
	// A version-3 seed queued earlier reads the version-4 review before the registration's own,
	// older read applies.
	if err := runner.seedGate(ctx, record.OutboxRow{Issue: "CAPTURE-100"}, record.GateSeed{ArtifactID: "artifact-root", Version: 3, Generation: 1}); err != nil {
		t.Fatalf("seed the gate at version 3: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "review-11", intake.DispatchArtifact{Key: "CAPTURE-100", ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 11}}, w.handlers...); err != nil {
		t.Fatalf("apply the person's review: %v", err)
	}
	if _, err := intake.ApplyFact(ctx, pool, "api", "register-v4", intake.GateRegistered{Issue: "CAPTURE-100", ArtifactID: "artifact-root",
		Version: 4, DispatchVersion: 4, Since: record.EventMark{Seq: 10}}, w.handlers...); err != nil {
		t.Fatalf("register the gate at version 4: %v", err)
	}
	// The version-4 seed reads that same review, and a restart catches up on the same Dispatch.
	if err := runner.seedGate(ctx, record.OutboxRow{Issue: "CAPTURE-100"}, record.GateSeed{ArtifactID: "artifact-root", Version: 4, Generation: 1}); err != nil {
		t.Fatalf("seed the gate at version 4: %v", err)
	}
	if err := bootCatchUp(t, w); err != nil {
		t.Fatalf("catch up after the restart: %v", err)
	}
	if got := readIssue(t, pool, "CAPTURE-100"); got.AwaitingPerson() && !got.Answered() {
		t.Errorf("the review at sequence 11 reached the live handler and the seed; the registration whose read began at 10 left the root %#v, want it admitted or answered", got)
	}
}
