package admit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/blockers"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The tests below pin a root waiting on its design gate: a closed, armed gate whose approval
// request the root's architect handed to a person returns the root's slot (workflow's
// ReconcileAwaiting), and every way that wait is answered or moves.

var (
	alice     = dispatch.Actor{Kind: "user", ID: "alice"}
	architect = dispatch.Actor{Kind: "session", ID: "session-architect"}
	outsider  = dispatch.Actor{Kind: "session", ID: "session-elsewhere"}
	settler   = dispatch.Actor{Kind: "system", ID: "document-settlement"}
)

// handBack is the architect's hand-back of the approval request whose event Dispatch appended at
// seq, its transaction begun at handedBackAt: the wait it starts begins there.
var (
	handedBackAt = fixedNow.Add(-time.Hour)
	handBack     = record.EventMark{Seq: 10, At: handedBackAt}
)

// architectToken is the claim token of the root architect of the tree rooted at root.
func architectToken(t *testing.T, root string) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	token, err := claim.NewToken(project, root, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("architect token of %s: %v", root, err)
	}
	return token
}

// seedArchitectClaim records session as the architect of the tree rooted at root, running a turn
// it has taken its task for: the session SessionClaimsTree finds within that tree, and an
// architect that owes nothing (workflow's owed), so the root may wait on a person.
func seedArchitectSession(t *testing.T, pool *pgxpool.Pool, root, session string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'LEGION', $2, $2, 'architect', 1, $3, '', 'working', 0, 0, 0, 0)
			on conflict (token) do update set session = excluded.session, state = excluded.state`,
			string(architectToken(t, root)), root, session); err != nil {
			t.Fatalf("seed architect claim: %v", err)
		}
	})
}

// setArchitectState moves root's architect claim to state.
func setArchitectState(t *testing.T, pool *pgxpool.Pool, root string, state supervise.ClaimState) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `update claims set state = $2 where token = $1`, string(architectToken(t, root)), string(state)); err != nil {
			t.Fatalf("set the architect's state: %v", err)
		}
	})
}

// giveArchitectTask records task as delivered to root's architect, confirmed (taken) or not.
func giveArchitectTask(t *testing.T, pool *pgxpool.Pool, root, task string, confirmed bool) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, delivered_at, confirmed_at)
			values ($1, 'delivery-1', $2, now(), now(), case when $3 then now() end)
			on conflict (claim_token) do update set task = excluded.task, confirmed_at = excluded.confirmed_at`,
			string(architectToken(t, root)), task, confirmed); err != nil {
			t.Fatalf("give the architect its task: %v", err)
		}
	})
}

// runQueuedArchitectStarts stands in for the outbox running root's queued architect starts: each
// is taken off the queue, and its task (the last start's) is delivered to the claim unconfirmed.
// It reports whether a start was queued.
func runQueuedArchitectStarts(t *testing.T, pool *pgxpool.Pool, root string) bool {
	t.Helper()
	task, started := "", false
	inTx(t, pool, func(tx pgx.Tx) {
		rows, err := tx.Query(context.Background(), `delete from outbox where kind = 'supervise' and issue = $1
			and payload->>'role' = 'architect' and payload->>'op' = 'start' returning coalesce(payload->>'task', '')`, root)
		if err != nil {
			t.Fatalf("run the architect's starts: %v", err)
		}
		for rows.Next() {
			started = true
			if err := rows.Scan(&task); err != nil {
				t.Fatalf("run the architect's starts: %v", err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("run the architect's starts: %v", err)
		}
	})
	if started {
		giveArchitectTask(t, pool, root, task, false)
	}
	return started
}

// takeArchitectTask stands in for root's architect taking the work it was last given: its queued
// starts run, and the task they deliver is confirmed in the turn it works.
func takeArchitectTask(t *testing.T, pool *pgxpool.Pool, root string) {
	t.Helper()
	runQueuedArchitectStarts(t, pool, root)
	setArchitectState(t, pool, root, supervise.StateWorking)
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `update pending_task_deliveries set confirmed_at = now() where claim_token = $1`,
			string(architectToken(t, root))); err != nil {
			t.Fatalf("confirm the architect's task: %v", err)
		}
	})
}

// architectOps is every supervise operation the outbox holds for root's architect, in order.
func architectOps(t *testing.T, pool *pgxpool.Pool, root string) []string {
	t.Helper()
	var ops []string
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == root && request.Role == claim.RoleArchitect {
			ops = append(ops, string(request.Op))
		}
	}
	return ops
}

// lastArchitectTask is the task of the last architect start the outbox holds for root, "" for none.
func lastArchitectTask(t *testing.T, pool *pgxpool.Pool, root string) string {
	t.Helper()
	task := ""
	for _, effect := range effects(t, pool) {
		if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == root && start.Op == "start" && start.Role == claim.RoleArchitect {
			task = start.Task
		}
	}
	return task
}

// mirrorAsk writes ask's mirror row on root as given.
func mirrorAsk(t *testing.T, pool *pgxpool.Pool, ask record.RootAsk) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutRootAsk(context.Background(), tx, ask); err != nil {
			t.Fatalf("mirror ask %s: %v", ask.Ask, err)
		}
	})
}

// countSuspends counts the architect suspends in ops.
func countSuspends(ops []string) int {
	return strings.Count(strings.Join(ops, " "), "suspend")
}

// runningRoot records LEGION-100 holding the one slot with its architect running a turn it took,
// and LEGION-NEXT waiting for a slot.
func runningRoot(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectSession(t, pool, "LEGION-100", architect.ID)
}

func seqMark(seq int64) record.EventMark { return record.EventMark{Seq: seq} }

// openedBy is the architect's question ask on LEGION-100 at seq.
func openedBy(ask string, seq int64) intake.DispatchAskOpened {
	return intake.DispatchAskOpened{Key: "LEGION-100", Ask: ask, Kind: "question", AuthorSession: architect.ID, Mark: seqMark(seq)}
}

// answeredBy is alice's answer of the question ask on LEGION-100 at seq.
func answeredBy(ask string, seq int64) intake.DispatchAskClosed {
	return intake.DispatchAskClosed{Key: "LEGION-100", Ask: ask, Kind: "question", State: "answered", Actor: alice, Mark: seqMark(seq)}
}

// requestHandedBack is the architect's hand-back of the approval request ask-approval at seq.
func requestHandedBack(seq int64) intake.DispatchAskReply {
	return intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", Kind: "approval", Artifact: "artifact-root", WaitingOn: "human", Actor: architect, Mark: seqMark(seq)}
}

// nextFinished frees LEGION-NEXT's slot, so an answered LEGION-100 takes it and resumes.
func nextFinished(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	apply(t, pool, admission, "next-finished", intake.DispatchIssue{Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 1,
		Type: "issue.closed", Status: "done", Title: "LEGION-NEXT", Rank: "B", HandedOver: true}, admission.engine)
}

// seedChild records LEGION-CHILD under LEGION-100 in phase at.
func seedChild(t *testing.T, pool *pgxpool.Pool, at phase.Phase) {
	t.Helper()
	parent := "LEGION-100"
	putIssue(t, pool, record.Issue{Key: "LEGION-CHILD", Tree: "LEGION-100", Parent: &parent, Project: testProject, Title: "child",
		Phase: at, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "C", LastDispatchSeq: 1, HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
}

// parkChild is a person moving LEGION-CHILD to the backlog, out of the workflow.
func parkChild(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	apply(t, pool, admission, "child-parked", intake.DispatchIssue{Key: "LEGION-CHILD", ResolvedRepo: "acme/widgets", Seq: 30,
		Type: "issue.updated", Status: "backlog", Title: "child", Rank: "C", Parent: "LEGION-100", HandedOver: true}, admission.engine)
}

func assertAdmittedInItsSlot(t *testing.T, pool *pgxpool.Pool, why string) record.Issue {
	t.Helper()
	got := issue(t, pool, "LEGION-100")
	if got.Phase != phase.Admitted || got.Awaiting != nil {
		t.Fatalf("root after %s = %#v, want it admitted with no wait", why, got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
	return got
}

// personHoldsTheClaim records a person holding LEGION-100's Dispatch claim in the mirror of
// Dispatch's issues, so the root could not resume in its own session.
func personHoldsTheClaim(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedDispatchClaim(t, pool, "LEGION-100", "user:bob")
}

// applyThroughMirror applies fact through the mirror of Dispatch's issues, then the engine and
// admission, as the daemon's handlers run: a claim reaches the record through that mirror alone.
func applyThroughMirror(t *testing.T, pool *pgxpool.Pool, admission *Admission, id string, fact intake.Fact) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", id, fact, blockers.NewMirror(record.NewStore(), testProject), admission.engine, admission); err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
}

// awaitingRoot records LEGION-100 waiting on waiting, with no slot, beside LEGION-RUN holding the
// one slot, so an answer puts LEGION-100 at the front of the waiting line without resuming it.
func awaitingRoot(t *testing.T, pool *pgxpool.Pool, waiting record.Awaiting) {
	t.Helper()
	// A gate wait names the document it waits on, as every wait the workflow starts does.
	if waiting.GateVersion != 0 && waiting.Artifact == "" {
		waiting.Artifact = "artifact-root"
	}
	seedSlotted(t, pool, "LEGION-RUN", "A")
	putIssue(t, pool, record.Issue{
		Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &waiting,
	})
	if waiting.GateVersion != 0 {
		seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: waiting.GateVersion})
	}
}

// seedRequestedGate records gate, and the approval request ask-approval of its document as the
// mirror knows it from the request's own events: an approval ask of that document, its state left
// to the facts each test applies.
func seedRequestedGate(t *testing.T, pool *pgxpool.Pool, gate record.DesignGate) {
	t.Helper()
	seedGate(t, pool, gate)
	mirrorAsk(t, pool, record.RootAsk{Issue: gate.Issue, Ask: "ask-approval", Kind: record.AskApproval, Artifact: gate.ArtifactID})
}

// handBackOnAClosedGate leaves LEGION-100 waiting on its gate at version 4, its slot taken by
// LEGION-NEXT, after its own architect handed the approval request back to the person at handBack.
func handBackOnAClosedGate(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	handBackOnAClosedGateAt(t, pool, admission, handBack)
}

// handBackOnAClosedGateAt is handBackOnAClosedGate with the hand-back's event at mark, where the
// wait begins.
func handBackOnAClosedGateAt(t *testing.T, pool *pgxpool.Pool, admission *Admission, mark record.EventMark) {
	t.Helper()
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectSession(t, pool, "LEGION-100", architect.ID)
	seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
	applyFact(t, pool, admission, "hand-back", intake.DispatchAskReply{
		Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: mark})
	if waiting := issue(t, pool, "LEGION-100"); waiting.Phase != phase.AwaitingPerson || waiting.Answered() || waiting.Awaiting.Since.Seq != mark.Seq {
		t.Fatalf("root after its architect handed the request back = %#v, want it waiting unanswered from the hand-back", waiting)
	}
}

// resumedTask frees LEGION-RUN's slot, so the answered LEGION-100 is promoted, and returns the
// task its architect resumes with.
func resumedTask(t *testing.T, pool *pgxpool.Pool, admission *Admission) string {
	t.Helper()
	apply(t, pool, admission, "run-finished", intake.DispatchIssue{
		Key: "LEGION-RUN", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done", Title: "LEGION-RUN", Rank: "A", HandedOver: true,
	}, admission.engine)
	task := ""
	for _, effect := range effects(t, pool) {
		if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == "LEGION-100" && start.Op == "start" && start.Role == "architect" {
			task = start.Task
		}
	}
	return task
}

// assertNoAwaitingEffects fails when key's architect was suspended or the controller was told the
// root returned its slot to wait on a person.
func assertNoAwaitingEffects(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	for _, effect := range effects(t, pool) {
		if effect.issue != key {
			continue
		}
		switch payload := effect.payload.(type) {
		case record.SuperviseRequest:
			if payload.Op == "suspend" {
				t.Fatalf("outbox effects = %#v, want no suspend of %s", effects(t, pool), key)
			}
		case record.ControllerNotice:
			if payload.Kind == record.AwaitingPersonNotice {
				t.Fatalf("outbox effects = %#v, want no awaiting-person notice for %s", effects(t, pool), key)
			}
		}
	}
}

// A registration of a closed gate whose approval request waits on the person is the architect
// handing the spec to the person: the root, its architect running the turn it took, gives its slot
// to the next root, its architect suspended after that turn and the controller told.
func TestAClosedRegisteredGateReleasesItsRootSlotForAnotherWaitingRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectSession(t, pool, "LEGION-100", "session-architect")

	if _, err := intake.ApplyFact(context.Background(), pool, "api", "registered-closed-gate",
		intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 3, RequestAsk: "ask-approval", Since: record.EventMark{Seq: 5}},
		admission.engine, admission); err != nil {
		t.Fatalf("register closed gate: %v", err)
	}

	root := issue(t, pool, "LEGION-100")
	if root.Phase != phase.AwaitingPerson || root.Awaiting == nil || root.Awaiting.GateVersion != 3 || root.Awaiting.AnsweredAt != nil {
		t.Fatalf("root after closed gate = %#v, want unanswered awaiting_person on gate version 3", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
	var suspended, noticed bool
	for _, effect := range effects(t, pool) {
		switch payload := effect.payload.(type) {
		case record.SuperviseRequest:
			suspended = suspended || effect.issue == "LEGION-100" && payload.Op == "suspend" && payload.Role == "architect" && strings.Contains(payload.Reason, "spec version 3")
		case record.ControllerNotice:
			noticed = noticed || effect.issue == "LEGION-100" && payload.Kind == record.AwaitingPersonNotice && strings.Contains(payload.Reason, "slot is released")
		}
	}
	if !suspended || !noticed {
		t.Fatalf("outbox effects = %#v, want architect suspend and awaiting-person controller notice", effects(t, pool))
	}
}

func TestAnApprovedGateAnswersAnAwaitingRootAndReturnsItBeforeANeverRunRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-RUN", "A")
	root := record.Issue{
		Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "Z", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{GateVersion: 3, Artifact: "artifact-root"},
	}
	putIssue(t, pool, root)
	seedWaiting(t, pool, "LEGION-NEXT", "A")
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, record.DesignGate{Issue: root.Key, ArtifactID: "artifact-root", LatestVersion: 3}); err != nil {
			t.Fatalf("seed closed gate: %v", err)
		}
	})

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "approved-gate",
		intake.DispatchArtifact{Key: root.Key, ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 3}, admission.engine, admission); err != nil {
		t.Fatalf("approve gate: %v", err)
	}
	answered := issue(t, pool, root.Key)
	if !answered.Answered() {
		t.Fatalf("root after approval = %#v, want answered awaiting_person root", answered)
	}
	assertWaiting(t, pool, []string{root.Key, "LEGION-NEXT"})

	apply(t, pool, admission, "run-finished", intake.DispatchIssue{
		Key: "LEGION-RUN", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done",
		Title: "LEGION-RUN", Rank: "A", HandedOver: true,
	}, admission.engine)
	resumed := issue(t, pool, root.Key)
	if resumed.Phase != phase.Planning || resumed.Awaiting != nil {
		t.Fatalf("root after its slot freed = %#v, want planning with no awaiting state", resumed)
	}
	assertSlots(t, pool, []record.Slot{{Issue: root.Key, Index: 0, AdmittedAt: fixedNow}})
	var statusWrites, architectStarts int
	for _, effect := range effects(t, pool) {
		if effect.issue != root.Key {
			continue
		}
		switch payload := effect.payload.(type) {
		case record.StatusWrite:
			statusWrites++
		case record.SuperviseRequest:
			if payload.Op == "start" && payload.Role == "architect" && strings.Contains(payload.Task, "spec version 3") {
				architectStarts++
			}
		}
	}
	if statusWrites != 0 || architectStarts != 1 {
		t.Fatalf("root resume emitted %d status writes and %d answered architect starts, want 0 and 1; effects=%#v", statusWrites, architectStarts, effects(t, pool))
	}
}

func TestANewerSpecVersionWakesARootAwaitingAGate(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-RUN", "A")
	root := record.Issue{
		Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{GateVersion: 3, Artifact: "artifact-root"},
	}
	putIssue(t, pool, root)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, record.DesignGate{Issue: root.Key, ArtifactID: "artifact-root", LatestVersion: 3}); err != nil {
			t.Fatalf("seed gate: %v", err)
		}
	})

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "spec-version-moved",
		intake.DispatchArtifact{Key: root.Key, ArtifactID: "artifact-root", Kind: intake.DispatchArtifactVersion, Version: 4}, admission.engine, admission); err != nil {
		t.Fatalf("apply new spec version: %v", err)
	}
	got := issue(t, pool, root.Key)
	if !got.Answered() || got.Awaiting.GateVersion != 4 {
		t.Fatalf("root after newer version = %#v, want answered waiting on gate version 4", got)
	}
}

func TestAClosedGateDoesNotReleaseASlotWhileATreeMemberIsMidPhase(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedArchitectSession(t, pool, "LEGION-100", "session-architect")
	parent := "LEGION-100"
	putIssue(t, pool, record.Issue{
		Key: "LEGION-CHILD", Tree: "LEGION-100", Parent: &parent, Project: testProject, Title: "child", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
	})

	if _, err := intake.ApplyFact(context.Background(), pool, "api", "closed-gate-with-child",
		intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 3, RequestAsk: "ask-approval", Since: record.EventMark{Seq: 5}},
		admission.engine, admission); err != nil {
		t.Fatalf("register closed gate: %v", err)
	}
	root := issue(t, pool, "LEGION-100")
	if root.Phase != phase.Admitted || root.Awaiting != nil {
		t.Fatalf("root with active child = %#v, want its slot retained at admitted", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: root.Key, Index: 0, AdmittedAt: fixedNow}})
}

// A changes request resumes the root; its architect takes the resume task, revises the spec, asks
// again, and registers the new version. That registration, closed like the first, starts a new
// wait and returns the slot again.
func TestAResumedRootThatRegistersANewClosedVersionReleasesItsSlotAgain(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedArchitectSession(t, pool, "LEGION-100", "session-architect")
	register := func(id string, version int, read int64) {
		t.Helper()
		if _, err := intake.ApplyFact(context.Background(), pool, "api", id,
			intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: version, RequestAsk: "ask-approval",
				Since: record.EventMark{Seq: read}}, admission.engine, admission); err != nil {
			t.Fatalf("register gate version %d: %v", version, err)
		}
	}

	register("register-v3", 3, 5)
	if got := issue(t, pool, "LEGION-100"); got.Phase != phase.AwaitingPerson || got.Awaiting.GateVersion != 3 {
		t.Fatalf("root after closed v3 = %#v, want awaiting_person on version 3", got)
	}
	assertSlots(t, pool, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "changes-v3",
		intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 3, Reason: "name the rollback path",
			Mark: record.EventMark{Seq: 6}},
		admission.engine, admission); err != nil {
		t.Fatalf("request changes on v3: %v", err)
	}
	resumed := issue(t, pool, "LEGION-100")
	if resumed.Phase != phase.Admitted || resumed.Awaiting != nil {
		t.Fatalf("root after changes requested = %#v, want it resumed at admitted", resumed)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
	var told string
	for _, effect := range effects(t, pool) {
		if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == "LEGION-100" && start.Op == "start" && start.Role == "architect" {
			told = start.Task
		}
	}
	if !strings.Contains(told, "requested changes") || !strings.Contains(told, "register_gate") {
		t.Fatalf("resumed architect's task = %q, want it told of the changes and to register the revised version", told)
	}

	takeArchitectTask(t, pool, "LEGION-100")
	register("register-v4", 4, 8)
	again := issue(t, pool, "LEGION-100")
	if again.Phase != phase.AwaitingPerson || again.Awaiting == nil || again.Awaiting.GateVersion != 4 || again.Awaiting.AnsweredAt != nil {
		t.Fatalf("root after closed v4 = %#v, want a new unanswered wait on version 4", again)
	}
	assertSlots(t, pool, nil)
	var suspends int
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == "LEGION-100" && request.Op == "suspend" && request.Role == "architect" {
			suspends++
		}
	}
	if suspends != 2 {
		t.Fatalf("architect suspends = %d, want one per wait", suspends)
	}
}

// An approved gate that answers a root's wait advances the root and its admitted children together
// once the root has a slot again, and not before: a child never starts planning ahead of its root.
func TestAResumeWithTheGateOpenAdvancesTheAdmittedChildren(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-RUN", "A")
	putIssue(t, pool, record.Issue{
		Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{GateVersion: 3, Artifact: "artifact-root"},
	})
	parent := "LEGION-100"
	putIssue(t, pool, record.Issue{
		Key: "LEGION-CHILD", Tree: "LEGION-100", Parent: &parent, Project: testProject, Title: "child", Phase: phase.Admitted,
		Generation: 1, Status: "todo", Rank: "C", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", LastDispatchSeq: 1,
	})
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 3}); err != nil {
			t.Fatalf("seed closed gate: %v", err)
		}
	})

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "approved-gate",
		intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 3}, admission.engine, admission); err != nil {
		t.Fatalf("approve gate: %v", err)
	}
	if got := issue(t, pool, "LEGION-CHILD"); got.Phase != phase.Admitted {
		t.Fatalf("child before its root has a slot = %#v, want it still admitted", got)
	}

	apply(t, pool, admission, "run-finished", intake.DispatchIssue{
		Key: "LEGION-RUN", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done",
		Title: "LEGION-RUN", Rank: "A", HandedOver: true,
	}, admission.engine)
	if got := issue(t, pool, "LEGION-100"); got.Phase != phase.Planning {
		t.Fatalf("root after its resume = %#v, want planning", got)
	}
	if got := issue(t, pool, "LEGION-CHILD"); got.Phase != phase.Planning {
		t.Fatalf("child after its root's resume = %#v, want planning", got)
	}
}

// Round 21 same-model P2-1, outside-whole P3-C: the architect asks Q1 and Q2 in one turn, the
// person answers Q1, and the root resumes with Q2 still with the person. Neither the architect's
// own claim event nor a parked child sends the resumed root back to awaiting_person: the resume is
// a handout, and only a hand-back after it starts the next wait. Nothing suspends the architect
// after its resume's start.
func TestAResumedRootIsNotSentBackByAClaimEventOrAMemberLeave(t *testing.T) {
	for _, tc := range []struct {
		name  string
		after func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{name: "the architect's own claim event", after: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			applyFact(t, pool, admission, "claim-again", intake.DispatchClaim{Key: "LEGION-100", Seq: 40, ClaimedBy: "session:" + architect.ID})
		}},
		{name: "a parked child", after: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
			parkChild(t, pool, admission)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			runningRoot(t, pool)
			seedChild(t, pool, phase.Admitted)
			applyFact(t, pool, admission, "q1", openedBy("ask-q1", 10))
			applyFact(t, pool, admission, "q2", openedBy("ask-q2", 11))
			if got := issue(t, pool, "LEGION-100"); !got.AwaitingPerson() || got.Awaiting.Ask != "ask-q1" {
				t.Fatalf("root after two questions = %#v, want it waiting on ask-q1", got)
			}
			applyFact(t, pool, admission, "answer-q1", answeredBy("ask-q1", 20))
			nextFinished(t, pool, admission)
			assertAdmittedInItsSlot(t, pool, "the resume")
			if task := lastArchitectTask(t, pool, "LEGION-100"); !strings.Contains(task, "ask ask-q2") {
				t.Fatalf("resume task = %q, want it to name ask-q2 still with the person", task)
			}

			tc.after(t, pool, admission)
			assertAdmittedInItsSlot(t, pool, tc.name)
			takeArchitectTask(t, pool, "LEGION-100")
			applyFact(t, pool, admission, "tick", intake.ControllerTick{})
			assertAdmittedInItsSlot(t, pool, "the resume task taken")
			if ops := architectOps(t, pool, "LEGION-100"); strings.Join(ops, " ") != "suspend" {
				t.Fatalf("architect ops = %v, want the first wait's suspend alone after the start ran", ops)
			}
		})
	}
}

// Round 21 second-review P2-3: a changes request answers a gate wait and is recorded as told for
// the resume task. While the resume's start is queued, its claim is launching, or its task is not
// yet taken, neither a member's leave nor a fresh hand-back of the tree suspends the architect,
// so the told request reaches it in that task. Once the task is taken, the next fact starts the
// wait the hand-back declared.
func TestNoSuspendReachesAResumeBeforeItsTaskIsTaken(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	seedChild(t, pool, phase.Admitted)
	seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
	applyFact(t, pool, admission, "hand-back", requestHandedBack(10))
	applyFact(t, pool, admission, "changes", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: seqMark(11)})
	nextFinished(t, pool, admission)
	assertAdmittedInItsSlot(t, pool, "the resume")

	assertHeld := func(stage string) {
		t.Helper()
		assertAdmittedInItsSlot(t, pool, stage)
		if gate := storedGate(t, pool, "LEGION-100"); gate.ToldChangesVersion == nil || gate.ToldChangesReason != "name the rollback path" {
			t.Fatalf("gate %s = %#v, want the changes request still recorded as told", stage, gate)
		}
		if suspends := countSuspends(architectOps(t, pool, "LEGION-100")); suspends != 1 {
			t.Fatalf("architect suspends %s = %d, want the first wait's alone", stage, suspends)
		}
	}
	parkChild(t, pool, admission)
	applyFact(t, pool, admission, "fresh-hand-back", requestHandedBack(13))
	assertHeld("with the resume's start queued")

	runQueuedArchitectStarts(t, pool, "LEGION-100")
	setArchitectState(t, pool, "LEGION-100", supervise.StateLaunching)
	applyFact(t, pool, admission, "tick-launching", intake.ControllerTick{})
	assertHeld("with the claim launching")

	setArchitectState(t, pool, "LEGION-100", supervise.StateReady)
	applyFact(t, pool, admission, "tick-unconfirmed", intake.ControllerTick{})
	assertHeld("with the task delivered, not yet taken")

	takeArchitectTask(t, pool, "LEGION-100")
	applyFact(t, pool, admission, "tick-taken", intake.ControllerTick{})
	got := issue(t, pool, "LEGION-100")
	if !got.AwaitingPerson() || got.Answered() || got.Awaiting.GateVersion != 4 || got.Awaiting.Since.Seq != 13 {
		t.Fatalf("root once its task was taken = %#v, want it waiting on version 4 from the hand-back at 13", got)
	}
}

// Round 21 second-review P2-2, same-model P3-1: a hand-back a guard kept from starting a wait starts
// it on whatever path clears the guard, since every fact reaches admission's pass.
func TestAHandedBackWaitStartsOnEveryGuardClearingPath(t *testing.T) {
	for _, tc := range []struct {
		name  string
		guard func(t *testing.T, pool *pgxpool.Pool)
		clear func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
		again func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{name: "a member leaves the workflow",
			guard: func(t *testing.T, pool *pgxpool.Pool) { seedChild(t, pool, phase.Implementing) },
			clear: parkChild},
		{name: "a member set back to todo re-enters at admitted",
			guard: func(t *testing.T, pool *pgxpool.Pool) { seedChild(t, pool, phase.Implementing) },
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				apply(t, pool, admission, "child-todo", intake.DispatchIssue{Key: "LEGION-CHILD", ResolvedRepo: "acme/widgets", Seq: 30,
					Type: "issue.updated", Status: "todo", Title: "child", Rank: "C", Parent: "LEGION-100", HandedOver: true}, admission.engine)
			}},
		{name: "the legion label is put back",
			guard: func(t *testing.T, pool *pgxpool.Pool) {
				root := issue(t, pool, "LEGION-100")
				root.HandedOver, root.DispatchStatus = false, "in_progress"
				putIssue(t, pool, root)
			},
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				apply(t, pool, admission, "label-back", intake.DispatchIssue{Key: "LEGION-100", ResolvedRepo: "acme/widgets", Seq: 30,
					Type: "issue.updated", Status: "in_progress", Title: "LEGION-100", Rank: "A", HandedOver: true}, admission.engine)
			}},
		{name: "a claim release first seen on an issue event, then its issue.released",
			guard: personHoldsTheClaim,
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				applyThroughMirror(t, pool, admission, "released-on-an-edit", intake.DispatchIssue{Key: "LEGION-100", ResolvedRepo: "acme/widgets", Seq: 30,
					Type: "issue.updated", Status: "in_progress", Title: "LEGION-100, renamed", Rank: "A", HandedOver: true, ClaimKnown: true})
			},
			again: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				applyThroughMirror(t, pool, admission, "released", intake.DispatchClaim{Key: "LEGION-100", Seq: 30})
			}},
		{name: "issue.released", guard: personHoldsTheClaim,
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				applyThroughMirror(t, pool, admission, "released", intake.DispatchClaim{Key: "LEGION-100", Seq: 30})
			}},
		{name: "a tick, once the architect took its task",
			guard: func(t *testing.T, pool *pgxpool.Pool) {
				giveArchitectTask(t, pool, "LEGION-100", "plan the tree", false)
			},
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				takeArchitectTask(t, pool, "LEGION-100")
				applyFact(t, pool, admission, "tick", intake.ControllerTick{})
			}},
		{name: "the boot's reconcile, once the architect took its task",
			guard: func(t *testing.T, pool *pgxpool.Pool) {
				giveArchitectTask(t, pool, "LEGION-100", "plan the tree", false)
			},
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				takeArchitectTask(t, pool, "LEGION-100")
				reconcile(t, pool, admission, nil)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			runningRoot(t, pool)
			seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
			tc.guard(t, pool)
			applyFact(t, pool, admission, "hand-back", requestHandedBack(10))
			assertAdmittedInItsSlot(t, pool, "a hand-back the guard held")

			tc.clear(t, pool, admission)
			if tc.again != nil {
				tc.again(t, pool, admission)
			}
			got := issue(t, pool, "LEGION-100")
			if !got.AwaitingPerson() || got.Answered() || got.Awaiting.GateVersion != 4 || got.Awaiting.Since.Seq != 10 {
				t.Fatalf("root once the guard cleared = %#v, want it waiting on spec version 4 from the hand-back", got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
			if suspends := countSuspends(architectOps(t, pool, "LEGION-100")); suspends != 1 {
				t.Fatalf("architect suspends = %d, want one", suspends)
			}
		})
	}
}

// Round 21 second-review P2-1, rewritten for round 22's newest hand-back: a member of the tree is
// mid-phase while the architect hands the approval request back and opens a question, and a person
// answers another question; then the member leaves. The wait begins at the newest hand-back, so
// only a person's act Dispatch recorded after it is told to the architect where it stands. An act
// between the two hand-backs is one the architect read before it handed the turn back the second
// time: it answers nothing, and the root waits on its gate from the newest hand-back.
func TestAGateWaitHeldByAGuardIsToldDespiteAnActOnAnotherAsk(t *testing.T) {
	t.Run("the act after the newest hand-back", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		runningRoot(t, pool)
		seedChild(t, pool, phase.Implementing)
		seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
		applyFact(t, pool, admission, "hand-back", requestHandedBack(10))
		applyFact(t, pool, admission, "question", openedBy("ask-q", 11))
		applyFact(t, pool, admission, "answer-q0", answeredBy("ask-q0", 12))
		assertAdmittedInItsSlot(t, pool, "the person's act on another ask")
		parkChild(t, pool, admission)

		got := assertAdmittedInItsSlot(t, pool, "the member's leave")
		if got.DeclaredSeq != 0 {
			t.Fatalf("root told in place = %#v, want its hand-backs taken in by the handout", got)
		}
		task := lastArchitectTask(t, pool, "LEGION-100")
		for _, want := range []string{"ask ask-q0", "the approval request for spec version 4", "ask ask-q", "Still with the person"} {
			if !strings.Contains(task, want) {
				t.Fatalf("task told in place = %q, want it to name %q", task, want)
			}
		}
		if suspends := countSuspends(architectOps(t, pool, "LEGION-100")); suspends != 0 {
			t.Fatalf("architect suspends = %d, want none", suspends)
		}
	})
	t.Run("the act between the two hand-backs", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		runningRoot(t, pool)
		seedChild(t, pool, phase.Implementing)
		seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
		applyFact(t, pool, admission, "hand-back", requestHandedBack(10))
		applyFact(t, pool, admission, "answer-q0", answeredBy("ask-q0", 11))
		applyFact(t, pool, admission, "question", openedBy("ask-q", 12))
		parkChild(t, pool, admission)

		got := issue(t, pool, "LEGION-100")
		if !got.AwaitingPerson() || got.Answered() || got.Awaiting.GateVersion != 4 || got.Awaiting.Since.Seq != 12 {
			t.Fatalf("root once the member left = %#v, want it waiting on spec version 4 from the newest hand-back 12", got)
		}
		if task := lastArchitectTask(t, pool, "LEGION-100"); task != "" {
			t.Fatalf("architect told %q, want no task for an act it read before its newest hand-back", task)
		}
	})
}

// Round 21 second-review P2-1: the hand-back, the question and the person's act on another ask,
// delivered in any order while a member runs, give the architect the same task once the member
// leaves.
func TestTheSameEventsInAnyOrderGiveTheArchitectTheSameTask(t *testing.T) {
	events := []intake.Fact{requestHandedBack(10), openedBy("ask-q", 11), answeredBy("ask-q0", 12)}
	orders := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	var first string
	for _, order := range orders {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			runningRoot(t, pool)
			seedChild(t, pool, phase.Implementing)
			seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
			for _, i := range order {
				applyFact(t, pool, admission, fmt.Sprintf("event-%d", i), events[i])
			}
			parkChild(t, pool, admission)
			assertAdmittedInItsSlot(t, pool, "the member's leave")
			task := lastArchitectTask(t, pool, "LEGION-100")
			if task == "" {
				t.Fatal("no task told in place")
			}
			if first == "" {
				first = task
			}
			if task != first {
				t.Fatalf("task for order %v = %q, want the same as the first order's %q", order, task, first)
			}
		})
	}
}

// Round 21 outside-whole P3-D, second-review P3-3, same-model P3-2: a registration whose read began
// at the sequence of a question's own event keeps both: the root waits on the gate, and the
// question is still named once the person approves.
func TestAQuestionAndARegistrationAtOneSequenceAreBothKept(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{Issue: "LEGION-100",
		ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4, RequestAsk: "ask-approval", Since: seqMark(50)}, admission.engine, admission); err != nil {
		t.Fatalf("register gate v4: %v", err)
	}
	applyFact(t, pool, admission, "question", openedBy("ask-q", 50))
	if got := issue(t, pool, "LEGION-100"); !got.AwaitingPerson() || got.Awaiting.GateVersion != 4 || got.Awaiting.Since.Seq != 50 {
		t.Fatalf("root after a registration and a question at one sequence = %#v, want it waiting on version 4 from 50", got)
	}
	applyFact(t, pool, admission, "approved", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactApproved, Version: 4, Mark: seqMark(51)})
	nextFinished(t, pool, admission)
	if task := lastArchitectTask(t, pool, "LEGION-100"); !strings.Contains(task, "Still with the person: ask ask-q") {
		t.Fatalf("resume task = %q, want the question at the registration's sequence named", task)
	}
}

// Round 21 outside-whole P3-B: a question delivered after a later one, while the root waits, is
// still named at the resume.
func TestAnOutOfOrderQuestionIsListedAtTheResume(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	applyFact(t, pool, admission, "q1", openedBy("ask-q1", 10))
	applyFact(t, pool, admission, "q3", openedBy("ask-q3", 12))
	applyFact(t, pool, admission, "q2", openedBy("ask-q2", 11))
	applyFact(t, pool, admission, "answer-q1", answeredBy("ask-q1", 20))
	nextFinished(t, pool, admission)
	task := lastArchitectTask(t, pool, "LEGION-100")
	for _, want := range []string{"ask ask-q2", "ask ask-q3"} {
		if !strings.Contains(task, want) {
			t.Fatalf("resume task = %q, want it to name %q", task, want)
		}
	}
}

// Round 21 outside-whole P3-A: a root waits on its question while its gate's request is with the
// person. The person's answer of the request is a review, which its artifact.approved event
// applies: the root resumes with the gate open, never ahead of it with the old gate state, in
// either order of the two events.
func TestAnApprovalAnswerWaitsForItsReview(t *testing.T) {
	answer := intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-approval", Kind: "approval", State: "answered", Actor: alice, Mark: seqMark(21)}
	review := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 4, Mark: seqMark(21)}
	for _, tc := range []struct {
		name   string
		order  []intake.Fact
		before bool
	}{
		{name: "the answer, then the review", order: []intake.Fact{answer, review}},
		{name: "the review, then the answer", order: []intake.Fact{review, answer}, before: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{Ask: "ask-q", Since: seqMark(10)})
			seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
			applyFact(t, pool, admission, "first", tc.order[0])
			if got := issue(t, pool, "LEGION-100"); got.Answered() != tc.before {
				t.Fatalf("root after the first of the two = %#v, want answered %t", got, tc.before)
			}
			applyFact(t, pool, admission, "second", tc.order[1])
			got := issue(t, pool, "LEGION-100")
			if !got.Answered() || got.Awaiting.AnsweredBy != record.AnsweredByApproval {
				t.Fatalf("root after both = %#v, want it answered by the approval", got)
			}
			task := resumedTask(t, pool, admission)
			if !strings.Contains(task, "was approved") || strings.Contains(task, "not approved") {
				t.Fatalf("resume task = %q, want it told of the approval with the gate open", task)
			}
		})
	}
}

// Round 20 same-model P2-1: a person's answer of the root's approval request answers a wait on a
// question when no request of the gate's document it is; when it is, its review event answers it.
// A gate already open answers the wait at once, whatever opened it.
func TestAPersonsAnswerOfTheApprovalRequest(t *testing.T) {
	answer := intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-approval", Kind: "approval", State: "answered", Actor: alice, Mark: seqMark(11)}
	review := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 4, Mark: seqMark(11)}
	for _, tc := range []struct {
		name     string
		gate     *record.DesignGate
		byAnswer bool
	}{
		{name: "no_gate", byAnswer: true},
		// A gate standing open answers the wait at the admission pass the answer's fact runs, whatever
		// opened it (the question-wait slice's answerOpenGates); the answer itself is a review.
		{name: "gate_already_open", gate: &record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4,
			ApprovedVersion: new(4), ApprovedMark: seqMark(5)}, byAnswer: true},
		{name: "gate_closed", gate: &record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{Ask: "ask-root", Since: handBack})
			if tc.gate != nil {
				seedRequestedGate(t, pool, *tc.gate)
			}
			applyFact(t, pool, admission, "approval-answered", answer)
			if got := issue(t, pool, "LEGION-100"); got.Answered() != tc.byAnswer {
				t.Fatalf("root after the person answered the approval request = %#v, want answered %t", got, tc.byAnswer)
			}
			if tc.gate == nil {
				return
			}
			applyFact(t, pool, admission, "approved", review)
			if got := issue(t, pool, "LEGION-100"); !got.Answered() {
				t.Fatalf("root after the review = %#v, want it answered", got)
			}
		})
	}
}

// Round 21 second-review P3-5: a session's progress note that moves the question a root waits on to
// the agent's turn leaves nothing with the person there, so it answers the wait, and the resume
// task says so.
func TestANoteHandingTheAwaitedAskToTheAgentAnswersTheWait(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	applyFact(t, pool, admission, "question", openedBy("ask-q", 10))
	if got := issue(t, pool, "LEGION-100"); !got.AwaitingPerson() || got.Answered() {
		t.Fatalf("root after its question = %#v, want it waiting", got)
	}
	applyFact(t, pool, admission, "note", intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-q", WaitingOn: "agent", Actor: outsider, Mark: seqMark(11)})
	got := issue(t, pool, "LEGION-100")
	if !got.Answered() || got.Awaiting.AnsweredBy != record.AnsweredByTurn || got.Awaiting.AnsweredDetail != "ask-q" {
		t.Fatalf("root after a note handed its question to the agent = %#v, want it answered by the turn on ask-q", got)
	}
	nextFinished(t, pool, admission)
	if task := lastArchitectTask(t, pool, "LEGION-100"); !strings.Contains(task, "to your turn") || !strings.Contains(task, "ask-q") {
		t.Fatalf("resume task = %q, want it to say ask-q was handed to the architect's turn", task)
	}
}

// Round 21 second-review P3-4 and the resume-text items: with the gate open at the resume the
// root moves to planning, where a hand-back starts no wait, so the task names what is still with
// the person without telling the architect to hand it back (round 20 same-model P3-2: the question
// is still named). Once a changes request closed the approval request, the task no longer offers
// an answer in the request's thread.
func TestTheResumeTaskWithTheGateOpenGivesNoHandBackAdvice(t *testing.T) {
	t.Run("the gate open", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		runningRoot(t, pool)
		seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
		applyFact(t, pool, admission, "q1", openedBy("ask-q1", 10))
		applyFact(t, pool, admission, "q2", openedBy("ask-q2", 11))
		applyFact(t, pool, admission, "approved", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
			Kind: intake.DispatchArtifactApproved, Version: 4, Mark: seqMark(12)})
		nextFinished(t, pool, admission)
		task := lastArchitectTask(t, pool, "LEGION-100")
		if !strings.Contains(task, "ask ask-q1") || !strings.Contains(task, "ask ask-q2") || strings.Contains(task, "Handing") || strings.Contains(task, "hand") {
			t.Fatalf("resume task with the gate open = %q, want both questions named and no hand-back advice", task)
		}
		if got := issue(t, pool, "LEGION-100"); got.Phase != phase.Planning {
			t.Fatalf("root after the resume = %#v, want planning", got)
		}
	})
	t.Run("the request closed by a changes request", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		runningRoot(t, pool)
		seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
		applyFact(t, pool, admission, "hand-back", requestHandedBack(10))
		applyFact(t, pool, admission, "request-answered", intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-approval", Kind: "approval",
			State: "answered", Actor: alice, Mark: seqMark(11)})
		applyFact(t, pool, admission, "changes", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
			Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: seqMark(11)})
		nextFinished(t, pool, admission)
		task := lastArchitectTask(t, pool, "LEGION-100")
		if !strings.Contains(task, "register_gate") || strings.Contains(task, "request's thread") {
			t.Fatalf("resume task after a changes request = %q, want the re-request advice without the request's thread", task)
		}
	})
}

// Round 21 second-review P3-1: a changes request read with no sequence (a Dispatch that predates
// it), on a root waiting from a sequenced start, can be placed neither before nor after the wait:
// the gate seed's read and a registration's read neither tell it nor record it as told, and its
// sequenced event then answers the wait, told once, in the resume task.
func TestAnUnsequencedChangesReadNeitherTellsNorRecords(t *testing.T) {
	unsequenced := record.EventMark{At: handedBackAt.Add(time.Minute)}
	for _, tc := range []struct {
		name string
		read intake.Fact
	}{
		{name: "the gate seed's read", read: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
			Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: unsequenced}},
		{name: "a registration's read", read: intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4,
			ChangesRequestedVersion: 4, ChangesReason: "name the rollback path", ChangesRequestedMark: unsequenced, Since: seqMark(12)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			handBackOnAClosedGate(t, pool, admission)
			source := "dispatch"
			if _, ok := tc.read.(intake.GateRegistered); ok {
				source = "api"
			}
			if _, err := intake.ApplyFact(context.Background(), pool, source, "read", tc.read, admission.engine, admission); err != nil {
				t.Fatalf("apply %s: %v", tc.name, err)
			}
			if told := gateNotices(t, pool, "LEGION-100", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("notices after %s = %#v, want none", tc.name, told)
			}
			if gate := storedGate(t, pool, "LEGION-100"); gate.ToldChangesVersion != nil {
				t.Fatalf("gate after %s = %#v, want no changes request recorded as told", tc.name, gate)
			}
			if got := issue(t, pool, "LEGION-100"); got.Answered() {
				t.Fatalf("root after %s = %#v, want it still waiting", tc.name, got)
			}
			applyFact(t, pool, admission, "sequenced", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
				Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 13, At: unsequenced.At}})
			if got := issue(t, pool, "LEGION-100"); !got.Answered() || got.Awaiting.AnsweredBy != record.AnsweredByChanges {
				t.Fatalf("root after the sequenced event = %#v, want it answered by the changes request", got)
			}
			if told := gateNotices(t, pool, "LEGION-100", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("notices after the sequenced event = %#v, want none: the resume task names it", told)
			}
		})
	}
}

// Round 18 outside-whole P1-1: the architect resolves the question its root waits on while another
// of its questions is still with the person. Nobody will answer the closed one, so the wait moves to
// the other, unanswered; the person's answer of that one answers it.
func TestASessionsCloseOfTheAwaitedAskMovesTheWait(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	applyFact(t, pool, admission, "q", openedBy("ask-q", 10))
	applyFact(t, pool, admission, "q2", openedBy("ask-q2", 11))
	applyFact(t, pool, admission, "resolved", intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-q", Kind: "question", State: "resolved",
		Actor: architect, Mark: seqMark(12)})
	got := issue(t, pool, "LEGION-100")
	if !got.AwaitingPerson() || got.Answered() || got.Awaiting.Ask != "ask-q2" || got.Awaiting.Since.Seq != 10 {
		t.Fatalf("root after its architect closed the awaited question = %#v, want it waiting on ask-q2 from 10", got)
	}
	applyFact(t, pool, admission, "answer-q2", answeredBy("ask-q2", 13))
	if got := issue(t, pool, "LEGION-100"); !got.Answered() || got.Awaiting.AnsweredBy != record.AnsweredByAsk {
		t.Fatalf("root after the person answered ask-q2 = %#v, want it answered on that ask", got)
	}
}

// Round 18 outside-whole P1-1: a person's reply on the approval request, handing it to the agent,
// answers a root waiting on its question: the person acted on an ask of the root.
func TestAPersonsReplyOnTheRequestAnswersAQuestionWait(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	awaitingRoot(t, pool, record.Awaiting{Ask: "ask-q", Since: seqMark(10)})
	seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
	applyFact(t, pool, admission, "reply", intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "agent", Actor: alice, Mark: seqMark(11)})
	if got := issue(t, pool, "LEGION-100"); !got.Answered() || got.Awaiting.AnsweredBy != record.AnsweredByReply {
		t.Fatalf("root after the person's reply on the request = %#v, want it answered by the reply", got)
	}
}

// Round 18 outside-whole P1-1: the architect's edit replaces its question's decision block with
// another, so Dispatch opens the new ask and settlement retracts the old one. The root keeps
// waiting, on the new ask.
func TestARetractionThatOpensAnotherAskKeepsTheRootWaiting(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	applyFact(t, pool, admission, "q", openedBy("ask-q", 10))
	applyFact(t, pool, admission, "q2", openedBy("ask-q2", 11))
	applyFact(t, pool, admission, "retracted", intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-q", Kind: "question", State: "resolved",
		Actor: settler, Mark: seqMark(12)})
	got := issue(t, pool, "LEGION-100")
	if !got.AwaitingPerson() || got.Answered() || got.Awaiting.Ask != "ask-q2" {
		t.Fatalf("root after the retraction = %#v, want it still waiting, on ask-q2", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
}

// A root that handed its approval request back waits on its gate; the person then reviews the
// spec before the architect's register_gate reads Dispatch, so the review arrives with the
// registration. The live artifact event and the gate seed that follow find the gate as the
// registration left it, so only the registration can answer the wait: it does, and the root takes
// the next free slot. Without that answer the root stays suspended with nobody waiting on it.
func TestAReviewARegistrationCarriesAnswersTheRootItWaitsFor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fact     intake.GateRegistered
		live     intake.DispatchArtifact
		resumeAt phase.Phase
	}{
		{
			name:     "a person approved before the registration read Dispatch",
			fact:     intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, ApprovedVersion: 4},
			live:     intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 4},
			resumeAt: phase.Planning,
		},
		{
			name: "a person requested changes before the registration read Dispatch",
			fact: intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4,
				ChangesRequestedVersion: 4, ChangesReason: "name the rollback path", ChangesRequestedMark: record.EventMark{Seq: 11}},
			live: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
				Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 11}},
			resumeAt: phase.Admitted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			handBackOnAClosedGate(t, pool, admission)

			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", tc.fact, admission.engine, admission); err != nil {
				t.Fatalf("register gate v4: %v", err)
			}
			answered := issue(t, pool, "LEGION-100")
			if !answered.Answered() {
				t.Fatalf("root after the registration carried the person's review = %#v, want it answered", answered)
			}
			applyFact(t, pool, admission, "live-review", tc.live)
			// The gate seed's read of the same review follows, under an id of its own.
			applyFact(t, pool, admission, "seeded-review", tc.live)

			apply(t, pool, admission, "next-finished", intake.DispatchIssue{
				Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done",
				Title: "LEGION-NEXT", Rank: "B", HandedOver: true,
			}, admission.engine)
			resumed := issue(t, pool, "LEGION-100")
			if resumed.Phase != tc.resumeAt || resumed.Awaiting != nil {
				t.Fatalf("root once a slot freed = %#v, want %s with no wait", resumed, tc.resumeAt)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
			if told := gateNotices(t, pool, "LEGION-100", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("design-changes-requested notices = %#v, want none: the resume task names the answer", told)
			}
		})
	}
}

// A registration's read writes the approval request's mirror row as the read found it, at the
// issue's sequence the read began at, so a hand-back at or below that sequence, delivered after it,
// is older than the row and changes nothing (the round-21 gate fence, now the row's own sequence).
// The architect hands the request back at seq 9; at seq 10 a session's note moves the turn to the
// agent; register_gate reads seq 10, sees the request waiting on the agent, and keeps the slot. The
// seq-9 hand-back, delivered now, declares nothing.
func TestARegistrationReadOutranksAnOlderHandBackOfItsRequest(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectSession(t, pool, "LEGION-100", architect.ID)
	read := record.EventMark{Seq: 10, At: handedBackAt.Add(time.Minute)}
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
		Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4, RequestAsk: "ask-approval", RequestWaitsOnAgent: true, Since: read,
	}, admission.engine, admission); err != nil {
		t.Fatalf("register gate v4: %v", err)
	}
	if kept := issue(t, pool, "LEGION-100"); kept.Phase != phase.Admitted || kept.Awaiting != nil || kept.DeclaredSeq != 0 {
		t.Fatalf("root after a registration that kept the slot = %#v, want admitted with nothing declared", kept)
	}
	applyFact(t, pool, admission, "late-hand-back", intake.DispatchAskReply{
		Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: record.EventMark{Seq: 9, At: handedBackAt}})
	if late := issue(t, pool, "LEGION-100"); late.Phase != phase.Admitted || late.Awaiting != nil || late.DeclaredSeq != 0 {
		t.Fatalf("root after a hand-back the registration's read covered = %#v, want it still admitted with nothing declared", late)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
}

// Only a person's own act answers a wait. A registration whose approval request Dispatch shows
// waiting on the agent carries no author, and a session's progress note hands an open ask to the
// agent as a person's reply does, so that registration answers nothing: the root waits on, as it
// does for the session's note itself.
func TestARegistrationWhoseRequestWaitsOnTheArchitectAnswersNothing(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	handBackOnAClosedGate(t, pool, admission)
	applyFact(t, pool, admission, "session-note", intake.DispatchAskReply{
		Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "agent", Actor: outsider})

	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
		Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, RequestWaitsOnAgent: true,
	}, admission.engine, admission); err != nil {
		t.Fatalf("register gate v4: %v", err)
	}
	got := issue(t, pool, "LEGION-100")
	if got.Phase != phase.AwaitingPerson || got.Answered() {
		t.Fatalf("root after a registration no person's act reached = %#v, want it still waiting unanswered", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
}

// A registration whose read of Dispatch shows a document version newer than the one the root waits
// on answers the wait whoever wrote that version, as the live artifact.version would: Dispatch then
// waits on the agent, and the version's own event may never reach the daemon, so nothing else
// would resume the root. The version the caller names proves nothing: a registration naming v5
// while Dispatch still shows v4 waiting on the person answers nothing.
func TestARegistrationOfANewerVersionAnswersTheWaitWhenItsEventIsLost(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fact     intake.GateRegistered
		answered bool
	}{
		{name: "Dispatch shows version 5", answered: true,
			fact: intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 5, RequestWaitsOnAgent: true, DispatchVersion: 5}},
		{name: "only the caller names version 5",
			fact: intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 5, DispatchVersion: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			handBackOnAClosedGate(t, pool, admission)
			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v5", tc.fact, admission.engine, admission); err != nil {
				t.Fatalf("register gate v5: %v", err)
			}
			got := issue(t, pool, "LEGION-100")
			if got.Answered() != tc.answered || tc.answered && got.Awaiting.GateVersion != 5 {
				t.Fatalf("root after the registration = %#v, want answered %t (at version 5 when answered)", got, tc.answered)
			}
		})
	}
}

// The architect writes version 5 (seq 9), then hands the approval request back (seq 10); the
// version's event reaches the daemon late, so the wait starts on version 4. Neither that version
// event nor the architect's register_gate of version 5, whose read shows the request waiting on
// the person, answers the wait: Dispatch asks the person to review version 5, and the wait moves to
// it. A version written after the hand-back answers it, and so does a registration whose read
// shows a newer version moved the request to the agent.
func TestTheArchitectsOwnEarlierVersionNeverAnswersItsWait(t *testing.T) {
	late := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactVersion, Version: 5,
		Mark: record.EventMark{Seq: 9, At: handedBackAt.Add(-time.Minute)}}
	registered := intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 5, DispatchVersion: 5, Since: record.EventMark{Seq: 11}}
	for _, tc := range []struct {
		name  string
		first intake.Fact
	}{
		{name: "the late version event", first: late},
		{name: "the architect's registration of that version", first: registered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			handBackOnAClosedGateAt(t, pool, admission, handBack)
			if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "first", tc.first, admission.engine, admission); err != nil {
				t.Fatalf("apply %s: %v", tc.name, err)
			}
			got := issue(t, pool, "LEGION-100")
			if !got.AwaitingPerson() || got.Answered() || got.Awaiting.GateVersion != 5 {
				t.Fatalf("root after %s = %#v, want it waiting unanswered on version 5", tc.name, got)
			}
			applyFact(t, pool, admission, "version-after", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
				Kind: intake.DispatchArtifactVersion, Version: 6, Mark: record.EventMark{Seq: 12, At: handedBackAt.Add(time.Minute)}})
			if got := issue(t, pool, "LEGION-100"); !got.Answered() || got.Awaiting.GateVersion != 6 {
				t.Fatalf("root after a version written after the hand-back = %#v, want it answered on version 6", got)
			}
		})
	}
}

// A review of a version older than the one the root waits on, applied late, answers nothing: the
// person has not reviewed the version the root waits on.
func TestAnOlderReviewAppliedLateLeavesTheNewerWaitUnanswered(t *testing.T) {
	for _, kind := range []intake.DispatchArtifactKind{intake.DispatchArtifactChangesRequested, intake.DispatchArtifactApproved} {
		t.Run(string(kind), func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			handBackOnAClosedGate(t, pool, admission)
			applyFact(t, pool, admission, "late-v3-review", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
				Kind: kind, Version: 3, Reason: "name the rollback path"})
			if got := issue(t, pool, "LEGION-100"); got.Phase != phase.AwaitingPerson || got.Answered() {
				t.Fatalf("root after a late v3 review over a wait on v4 = %#v, want it still waiting unanswered", got)
			}
		})
	}
}

// The gate seed's own read of a changes request answers a wait a registration started before the
// request reached the root, without telling the architect a second time; on a gate a racing
// registration opened, it closes the gate again and tells the architect.
func TestASeededChangesRequestAnswersAWaitAndClosesAGateARaceOpened(t *testing.T) {
	seeded := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
		Version: 4, Reason: "name the rollback path"}

	t.Run("a root waiting on the gate", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		awaitingRoot(t, pool, record.Awaiting{GateVersion: 4})
		applyFact(t, pool, admission, "seed-changes-v4", seeded)
		if got := issue(t, pool, "LEGION-100"); !got.Answered() {
			t.Fatalf("root after the seed read a changes request = %#v, want answered", got)
		}
		if told := gateNotices(t, pool, "LEGION-100", "design-changes-requested"); len(told) != 0 {
			t.Fatalf("design-changes-requested notices = %#v, want none: the resume task tells it", told)
		}
	})

	t.Run("a gate a racing registration opened", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		approved := 4
		seedSlotted(t, pool, "LEGION-100", "A")
		seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: &approved})
		applyFact(t, pool, admission, "seed-changes-v4", seeded)
		var gate *record.DesignGate
		inTx(t, pool, func(tx pgx.Tx) {
			var err error
			if gate, err = record.NewStore().Gate(context.Background(), tx, "LEGION-100"); err != nil {
				t.Fatalf("read gate: %v", err)
			}
		})
		if gate == nil || gate.ApprovedVersion != nil || gate.LatestVersion != 4 {
			t.Fatalf("gate after the seed read a changes request = %#v, want closed at version 4", gate)
		}
		if told := gateNotices(t, pool, "LEGION-100", "design-changes-requested"); len(told) != 1 || told[0].Reason != "name the rollback path" {
			t.Fatalf("design-changes-requested notices = %#v, want one with the reason", told)
		}
	})
}

// A changes request Dispatch recorded before the root's wait began answers nothing: the person gave
// it before the request was handed back to them. One recorded after it answers the wait, also when
// its transaction began before the hand-back's and committed after it, so that its time is the
// earlier one: the issue's event sequence, not Dispatch's time, orders the two. A review carrying
// no sequence was recorded before Dispatch kept one (its migration 0118), so before every wait,
// whatever its time says.
func TestAChangesRequestGivenBeforeTheWaitBeganAnswersNothingOnTheLivePath(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mark     record.EventMark
		answered bool
	}{
		{name: "a review whose event precedes the hand-back's", mark: record.EventMark{Seq: 9, At: handedBackAt.Add(-time.Minute)}},
		{name: "a review after the hand-back", mark: record.EventMark{Seq: 11, At: handedBackAt.Add(time.Minute)}, answered: true},
		{name: "a review whose transaction began before the hand-back's and committed after it",
			mark: record.EventMark{Seq: 11, At: handedBackAt.Add(-time.Millisecond)}, answered: true},
		{name: "a review with no sequence, given before the hand-back", mark: record.EventMark{At: handedBackAt.Add(-time.Minute)}},
		{name: "a review with no sequence, whatever its time", mark: record.EventMark{At: handedBackAt.Add(time.Minute)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			handBackOnAClosedGateAt(t, pool, admission, handBack)
			applyFact(t, pool, admission, "changes-v4", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
				Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: tc.mark})
			if got := issue(t, pool, "LEGION-100"); got.Phase != phase.AwaitingPerson || got.Answered() != tc.answered {
				t.Fatalf("root after %s = %#v, want awaiting_person answered %t", tc.name, got, tc.answered)
			}
		})
	}
}

// A registration's read of a changes request Dispatch recorded before the root's wait began
// answers nothing either, while one recorded after it answers the wait.
func TestAChangesRequestARegistrationCarriesAnswersOnlyAWaitItFollows(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mark     record.EventMark
		answered bool
	}{
		{name: "a review whose event precedes the hand-back's", mark: record.EventMark{Seq: 9, At: handedBackAt.Add(-time.Minute)}},
		{name: "a review after the hand-back", mark: record.EventMark{Seq: 11, At: handedBackAt.Add(-time.Millisecond)}, answered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			handBackOnAClosedGateAt(t, pool, admission, handBack)
			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
				Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4,
				ChangesRequestedVersion: 4, ChangesReason: "name the rollback path", ChangesRequestedMark: tc.mark,
			}, admission.engine, admission); err != nil {
				t.Fatalf("register gate v4: %v", err)
			}
			if got := issue(t, pool, "LEGION-100"); got.Phase != phase.AwaitingPerson || got.Answered() != tc.answered {
				t.Fatalf("root after %s = %#v, want awaiting_person answered %t", tc.name, got, tc.answered)
			}
		})
	}
}

// register_gate reads the issue's event sequence, then the approval, then applies the fact: a
// person who requests changes between that read and the apply has written a review the read did
// not show, whose transaction may even have begun before the read. The wait the registration starts
// begins at the sequence its read began at, so that review's own event, or the gate seed's read of
// it, answers the wait; begun at the daemon's clock when the fact applies, it would have answered
// nothing, and the root would wait on with nobody to wake it.
func TestAChangesRequestBetweenARegistrationsReadAndItsApplyAnswersTheWait(t *testing.T) {
	registered := intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4,
		RequestAsk: "ask-approval", Since: record.EventMark{Seq: 10}}
	// Dispatch's time of the review is before the daemon's clock reads the registration.
	review := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
		Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 11, At: fixedNow.Add(-time.Second)}}
	for _, source := range []string{"the live event", "the gate seed"} {
		t.Run(source, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-100", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedArchitectSession(t, pool, "LEGION-100", architect.ID)
			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", registered, admission.engine, admission); err != nil {
				t.Fatalf("register gate v4: %v", err)
			}
			waiting := issue(t, pool, "LEGION-100")
			if waiting.Phase != phase.AwaitingPerson || waiting.Awaiting.Since.Seq != registered.Since.Seq || waiting.Answered() {
				t.Fatalf("root after the registration = %#v, want it waiting unanswered from the read's sequence", waiting)
			}
			applyFact(t, pool, admission, source, review)
			if got := issue(t, pool, "LEGION-100"); !got.Answered() {
				t.Fatalf("root after %s of a review the registration's read missed = %#v, want it answered", source, got)
			}
		})
	}
}

// An approval answers a wait whenever it opens the gate, given before the wait began or not: a root
// can wait on a gate the record still shows closed after an approval whose own event was lost, and
// were that approval skipped when the gate seed or the boot catch-up applies it, the root would stay
// suspended with its gate open and nothing left to answer it. Applied again, it answers nothing
// more, and the root resumes once. An approval of an older version opens nothing and answers
// nothing. (A gate already open answers a waiting root by itself: the question-wait slice's
// answerOpenGates.)
func TestAnApprovalThatOpensTheGateAnswersAWaitWhateverItsTime(t *testing.T) {
	approval := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved,
		Version: 4, Mark: record.EventMark{Seq: 8, At: handedBackAt.Add(-time.Minute)}}

	t.Run("an approval before the wait, applied by the seed and the catch-up", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		handBackOnAClosedGateAt(t, pool, admission, handBack)
		applyFact(t, pool, admission, "seed-approved-v4", approval)
		answered := issue(t, pool, "LEGION-100")
		if !answered.Answered() {
			t.Fatalf("root after the seed applied an approval given before its wait = %#v, want it answered", answered)
		}
		applyFact(t, pool, admission, "catch-up-approved-v4", approval)
		if again := issue(t, pool, "LEGION-100"); !again.Answered() || !again.Awaiting.AnsweredAt.Equal(*answered.Awaiting.AnsweredAt) {
			t.Fatalf("root after the approval applied again = %#v, want its first answer kept", again)
		}
		apply(t, pool, admission, "next-finished", intake.DispatchIssue{
			Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done",
			Title: "LEGION-NEXT", Rank: "B", HandedOver: true,
		}, admission.engine)
		if resumed := issue(t, pool, "LEGION-100"); resumed.Phase != phase.Planning || resumed.Awaiting != nil {
			t.Fatalf("root once a slot freed = %#v, want planning with no wait", resumed)
		}
		starts := 0
		for _, effect := range effects(t, pool) {
			if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == "LEGION-100" && start.Op == "start" && start.Role == "architect" {
				starts++
			}
		}
		if starts != 1 {
			t.Fatalf("architect starts = %d, want the root resumed once", starts)
		}
	})

	for _, tc := range []struct {
		name string
		gate record.DesignGate
		fact intake.DispatchArtifact
	}{
		{name: "an approval of an older version", gate: record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4},
			fact: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 3, Mark: record.EventMark{Seq: 11}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{GateVersion: 4, Since: handBack})
			seedRequestedGate(t, pool, tc.gate)
			applyFact(t, pool, admission, "approval", tc.fact)
			if got := issue(t, pool, "LEGION-100"); got.Phase != phase.AwaitingPerson || got.Answered() {
				t.Fatalf("root after %s = %#v, want it still waiting unanswered", tc.name, got)
			}
		})
	}
}

// A person's reply on any ask of a root waiting on its gate hands the turn to its architect, the
// tree's only agent while it waits, and so does a person's answer to a question on it: the root is
// answered. A session's reply, whatever turn it leaves, a person's hand-back, and a person's review
// (answering the approval request, whose artifact event answers the gate after it) leave it waiting.
func TestAPersonsReplyOnTheRootWakesARootWaitingOnItsGate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fact  intake.Fact
		wakes bool
	}{
		{name: "a person's reply on the approval request", fact: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "agent", Actor: alice}, wakes: true},
		{name: "a person's answer to a question", fact: intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-question", Kind: "question", State: "answered", Actor: alice}, wakes: true},
		{name: "a session's progress note", fact: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "agent", Actor: outsider}},
		{name: "the architect's own reply", fact: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "human", Actor: architect}},
		{name: "a person's hand-back", fact: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "human", Actor: alice}},
		{name: "a person's review", fact: intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-approval", Kind: "approval", Artifact: "artifact-root", State: "answered", Actor: alice, Mark: seqMark(9)}},
		{name: "a session's resolution of a question", fact: intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-question", Kind: "question", State: "resolved", Actor: architect}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{GateVersion: 3})
			applyFact(t, pool, admission, "act", tc.fact)
			if got := issue(t, pool, "LEGION-100"); got.Answered() != tc.wakes {
				t.Fatalf("root after %s = %#v, want answered %t", tc.name, got, tc.wakes)
			}
		})
	}
}

// The architect hands an ask on its admitted root back to the person: on a registered closed
// gate the root waits on that gate's latest version, otherwise on the ask itself, and returns its
// slot either way. The question here is one the architect's tree opened before its last handout
// and the person moved to the agent's turn, so only the reply hands it back. A hand-back by a
// session outside the tree, or by a person, changes nothing.
func TestAnArchitectsHandBackReturnsTheSlotToWaitOnThePerson(t *testing.T) {
	since := record.EventMark{Seq: 10}
	for _, tc := range []struct {
		name  string
		gate  *record.DesignGate
		reply intake.DispatchAskReply
		want  *record.Awaiting
	}{
		{name: "the approval request handed back on a closed gate", gate: &record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4},
			reply: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: since},
			want:  &record.Awaiting{GateVersion: 4, Artifact: "artifact-root", Since: since}},
		{name: "a human-turn reply on a question with no gate",
			reply: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-question", WaitingOn: "human", Actor: architect, Mark: since},
			want:  &record.Awaiting{Ask: "ask-question", Since: since}},
		{name: "a reply from outside the tree",
			reply: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-question", WaitingOn: "human", Actor: outsider, Mark: since}},
		{name: "a person's own hand-back",
			reply: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-question", WaitingOn: "human", Actor: alice, Mark: since}},
		{name: "the architect's progress note",
			reply: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-question", WaitingOn: "agent", Actor: architect, Mark: since}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-100", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedArchitectSession(t, pool, "LEGION-100", architect.ID)
			mirrorAsk(t, pool, record.RootAsk{Issue: "LEGION-100", Ask: "ask-question", Kind: "question", Open: true, WaitingOn: "agent", WaitingOnSeq: 4, Seq: 4, ByTree: true, PersonSeq: 4})
			if tc.gate != nil {
				seedRequestedGate(t, pool, *tc.gate)
			}
			applyFact(t, pool, admission, "hand-back", tc.reply)

			root := issue(t, pool, "LEGION-100")
			if tc.want == nil {
				if root.Phase != phase.Admitted || root.Awaiting != nil {
					t.Fatalf("root after %s = %#v, want it still admitted", tc.name, root)
				}
				assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
				assertNoAwaitingEffects(t, pool, "LEGION-100")
				return
			}
			if root.Phase != phase.AwaitingPerson || root.Awaiting == nil || *root.Awaiting != *tc.want {
				t.Fatalf("root after %s = %#v, want awaiting_person on %#v", tc.name, root, *tc.want)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
		})
	}
}

// The resume task tells the architect of a person's reply on its waiting gate, and that a reply of
// its own in the request's thread starts its next wait.
func TestTheResumeTaskOfAGateWaitNamesAReply(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	awaitingRoot(t, pool, record.Awaiting{GateVersion: 3})
	applyFact(t, pool, admission, "reply", intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "agent", Actor: alice})
	apply(t, pool, admission, "run-finished", intake.DispatchIssue{
		Key: "LEGION-RUN", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done", Title: "LEGION-RUN", Rank: "A", HandedOver: true,
	}, admission.engine)
	var task string
	for _, effect := range effects(t, pool) {
		if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == "LEGION-100" && start.Op == "start" && start.Role == "architect" {
			task = start.Task
		}
	}
	if !strings.Contains(task, "replied") || !strings.Contains(task, "thread") {
		t.Fatalf("resumed architect's task = %q, want it to name a person's reply and the thread", task)
	}
}

// A root waiting on its architect's question is answered by the person's review of its spec too:
// an approval that opens its gate, or a changes request of its latest version, given after the
// wait began, live or read by a registration. The root takes the next slot, and the architect's
// task names the review: the approval, or the changes request with its version and reason. A newer
// version answers no question, nor does a changes request given before the question was opened.
func TestAPersonsReviewOfTheSpecAnswersARootWaitingOnItsQuestion(t *testing.T) {
	after := record.EventMark{Seq: 11, At: handedBackAt.Add(time.Minute)}
	for _, tc := range []struct {
		name     string
		fact     intake.Fact
		source   string
		answered bool
		task     []string
	}{
		{name: "an approval", answered: true, task: []string{"spec version 4", "was approved"},
			fact: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 4, Mark: after}},
		{name: "a changes request", answered: true, task: []string{"requested changes", "version 4", "name the rollback path"},
			fact: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 4,
				Reason: "name the rollback path", Mark: after}},
		{name: "an approval a registration read", source: "api", answered: true, task: []string{"was approved"},
			fact: intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4, ApprovedVersion: 4,
				ApprovedMark: after, Since: record.EventMark{Seq: 12}}},
		{name: "a changes request a registration read", source: "api", answered: true, task: []string{"requested changes", "name the rollback path"},
			fact: intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4, ChangesRequestedVersion: 4,
				ChangesReason: "name the rollback path", ChangesRequestedMark: after, Since: record.EventMark{Seq: 12}}},
		{name: "a newer version",
			fact: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactVersion, Version: 5, Mark: after}},
		{name: "a changes request given before the question",
			fact: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 4,
				Reason: "name the rollback path", Mark: record.EventMark{Seq: 9, At: handedBackAt.Add(-time.Minute)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{Ask: "ask-question", Since: handBack})
			seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
			source := tc.source
			if source == "" {
				source = "dispatch"
			}
			if _, err := intake.ApplyFact(context.Background(), pool, source, "review", tc.fact, admission.engine, admission); err != nil {
				t.Fatalf("apply %s: %v", tc.name, err)
			}
			got := issue(t, pool, "LEGION-100")
			if got.Answered() != tc.answered {
				t.Fatalf("root waiting on its question after %s = %#v, want answered %t", tc.name, got, tc.answered)
			}
			if !tc.answered {
				return
			}
			task := resumedTask(t, pool, admission)
			for _, want := range tc.task {
				if !strings.Contains(task, want) {
					t.Fatalf("resumed architect's task = %q, want it to name %q", task, want)
				}
			}
		})
	}
}

// The task resuming a root answered on its design gate names what answered it: a changes request
// with its version and reason, a newer version, or a reply in a thread.
func TestTheResumeTaskNamesWhatAnsweredTheGateWait(t *testing.T) {
	after := record.EventMark{Seq: 11, At: handedBackAt.Add(time.Minute)}
	for _, tc := range []struct {
		name string
		fact intake.Fact
		task []string
	}{
		{name: "a changes request", task: []string{"requested changes", "version 4", `"name the rollback path"`},
			fact: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 4,
				Reason: "name the rollback path", Mark: after}},
		{name: "a newer version", task: []string{"newer version", "version 5"},
			fact: intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactVersion, Version: 5, Mark: after}},
		{name: "a reply", task: []string{"replied", "thread", "version 4"},
			fact: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "agent", Actor: alice, Mark: after}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{GateVersion: 4, Since: handBack})
			applyFact(t, pool, admission, "answer", tc.fact)
			task := resumedTask(t, pool, admission)
			for _, want := range tc.task {
				if !strings.Contains(task, want) {
					t.Fatalf("resumed architect's task = %q, want it to name %q", task, want)
				}
			}
		})
	}
}

// A person's act answers only a wait it follows. Dispatch's intake retries a delivery, so an
// answer or a reply given before the architect handed the work back can reach the daemon after
// the wait began; it is the act the architect already read, and the root waits on the person's
// next one, which answers.
func TestAPersonsActRecordedBeforeTheWaitBeganAnswersNoWait(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale intake.Fact
		fresh intake.Fact
	}{
		{
			name:  "a reply",
			stale: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "agent", Actor: alice, Mark: record.EventMark{Seq: 19}},
			fresh: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "agent", Actor: alice, Mark: record.EventMark{Seq: 21}},
		},
		{
			name:  "an answered ask",
			stale: intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-question", Kind: "question", State: "answered", Actor: alice, Mark: record.EventMark{Seq: 19}},
			fresh: intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-question", Kind: "question", State: "answered", Actor: alice, Mark: record.EventMark{Seq: 21}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{GateVersion: 4, Since: record.EventMark{Seq: 20}})

			applyFact(t, pool, admission, "act-before-the-wait", tc.stale)
			if got := issue(t, pool, "LEGION-100"); got.Answered() {
				t.Fatalf("root after an act Dispatch recorded before the wait began = %#v, want it still waiting", got)
			}
			applyFact(t, pool, admission, "act-after-the-wait", tc.fresh)
			if got := issue(t, pool, "LEGION-100"); !got.Answered() {
				t.Fatalf("root after the person's next act = %#v, want it answered", got)
			}
		})
	}
}

// Dispatch's intake retries a delivery and the daemon naks a failed fact while its consumer moves
// on, so a person's act can reach the daemon before the event that starts the wait it answers. The
// root records the act, and the late event starts no wait: the root keeps its slot and its running
// architect, which already has the act. That holds for a person's reply before the question it
// replies to, and for a reply, an answer of another ask, or a changes request before the hand-back
// of the approval request; a changes request the architect was told of while the root ran is never
// waited on, where no later read would ever answer that wait. An act Dispatch recorded before the
// wait's start leaves the wait to start.
func TestAPersonsActDeliveredBeforeTheEventThatStartsItsWaitStartsNoWait(t *testing.T) {
	act := record.EventMark{Seq: 11, At: handedBackAt.Add(time.Minute)}
	early := record.EventMark{Seq: 9, At: handedBackAt.Add(-time.Minute)}
	opened := intake.DispatchAskOpened{Key: "LEGION-100", Ask: "ask-question", Kind: "question", AuthorSession: architect.ID, Mark: handBack}
	handedBack := intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: handBack}
	changes := func(mark record.EventMark) intake.DispatchArtifact {
		return intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
			Version: 4, Reason: "name the rollback path", Mark: mark}
	}
	for _, tc := range []struct {
		name   string
		act    intake.Fact
		starts intake.Fact
		waits  bool
	}{
		{name: "a person's reply before the question it replies to",
			act: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-question", WaitingOn: "agent", Actor: alice, Mark: act}, starts: opened},
		{name: "a person's reply before the hand-back",
			act: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "agent", Actor: alice, Mark: act}, starts: handedBack},
		{name: "a person's answer of another ask before the hand-back",
			act: intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-question", Kind: "question", State: "answered", Actor: alice, Mark: act}, starts: handedBack},
		{name: "a changes request before the hand-back", act: changes(act), starts: handedBack},
		{name: "a person's reply Dispatch recorded before the hand-back",
			act: intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "agent", Actor: alice, Mark: early}, starts: handedBack, waits: true},
		{name: "a changes request Dispatch recorded before the hand-back", act: changes(early), starts: handedBack, waits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-100", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedArchitectSession(t, pool, "LEGION-100", architect.ID)
			seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})

			applyFact(t, pool, admission, "act-delivered-first", tc.act)
			applyFact(t, pool, admission, "wait-start-retried-later", tc.starts)
			got := issue(t, pool, "LEGION-100")
			if tc.waits {
				if !got.AwaitingPerson() || got.Answered() {
					t.Fatalf("root after an act before its wait's start = %#v, want it waiting unanswered", got)
				}
				return
			}
			if got.Phase != phase.Admitted || got.Awaiting != nil {
				t.Fatalf("root after %s = %#v, want it admitted with no wait", tc.name, got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
			assertNoAwaitingEffects(t, pool, "LEGION-100")
		})
	}
}

// A registration reads the approval and nothing else: the mirror row it writes is its request's,
// so it fences no question (second-review round-21 P3-2, same-model P3-2). An ordinary question its
// read never saw, delivered late, still releases the root's slot and waits on the person.
func TestARegistrationFencesNoQuestion(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectSession(t, pool, "LEGION-100", architect.ID)
	read := record.EventMark{Seq: 20, At: handedBackAt}
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
		Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4, RequestAsk: "ask-approval", RequestWaitsOnAgent: true, Since: read,
	}, admission.engine, admission); err != nil {
		t.Fatalf("register gate v4: %v", err)
	}
	if kept := issue(t, pool, "LEGION-100"); kept.Phase != phase.Admitted || kept.DeclaredSeq != 0 {
		t.Fatalf("root after a registration that kept the slot = %#v, want admitted with nothing declared", kept)
	}

	applyFact(t, pool, admission, "question-below-the-read", intake.DispatchAskOpened{
		Key: "LEGION-100", Ask: "ask-question", Kind: "question", AuthorSession: architect.ID, Mark: record.EventMark{Seq: 19}})
	got := issue(t, pool, "LEGION-100")
	if !got.AwaitingPerson() || got.Answered() || got.Awaiting.Ask != "ask-question" {
		t.Fatalf("root after a question the approval read never saw = %#v, want it waiting on that question", got)
	}
}

// A review the gate has already applied, or one of a version below its latest, changes no gate
// state, and a newer version can reach the gate before the review's own event is redelivered. The
// review still answers the wait it follows: the person reviewed the spec after the architect
// handed the work back, and a root left waiting for a review the gate already holds would wait for
// good.
func TestAReviewTheGateAlreadyAppliedStillAnswersTheWaitItFollows(t *testing.T) {
	changes := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
		Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 11, At: handedBackAt.Add(time.Minute)}}
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectSession(t, pool, "LEGION-100", architect.ID)
	seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
	// The gate applies the review, a newer version follows, and only then does the architect's
	// hand-back start the wait; the review's own event is redelivered after all of it.
	applyFact(t, pool, admission, "changes-v4", changes)
	applyFact(t, pool, admission, "hand-back", intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "human",
		Actor: architect, Mark: record.EventMark{Seq: 12, At: handedBackAt.Add(2 * time.Minute)}})
	if waiting := issue(t, pool, "LEGION-100"); !waiting.AwaitingPerson() || waiting.Answered() {
		t.Fatalf("root after the hand-back = %#v, want it waiting unanswered", waiting)
	}
	again := changes
	again.Mark = record.EventMark{Seq: 13, At: handedBackAt.Add(3 * time.Minute)}
	again.Reason = "name the rollback path"
	applyFact(t, pool, admission, "changes-v4-again", again)
	if got := issue(t, pool, "LEGION-100"); !got.Answered() {
		t.Fatalf("root after the person's second review at the same version = %#v, want it answered", got)
	}
}

// The architect registers another document for its root while the root waits on the first: the
// wait moves to the new document at the version Dispatch shows, and that document's own approval
// answers it. Comparing the two documents' version numbers would wait on a number the new one may
// never reach.
func TestARegistrationOfAnotherDocumentMovesTheWaitToIt(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectSession(t, pool, "LEGION-100", architect.ID)
	seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
	applyFact(t, pool, admission, "hand-back", intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: handBack})
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-other", intake.GateRegistered{
		Issue: "LEGION-100", ArtifactID: "artifact-second", Version: 1, DispatchVersion: 1, Since: record.EventMark{Seq: 11},
	}, admission.engine, admission); err != nil {
		t.Fatalf("register the second document: %v", err)
	}
	moved := issue(t, pool, "LEGION-100")
	if !moved.AwaitingPerson() || moved.Answered() || moved.Awaiting.Artifact != "artifact-second" || moved.Awaiting.GateVersion != 1 {
		t.Fatalf("root after a registration of another document = %#v, want it waiting on artifact-second version 1", moved)
	}
	applyFact(t, pool, admission, "approved-second", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-second",
		Kind: intake.DispatchArtifactApproved, Version: 1, Mark: record.EventMark{Seq: 12, At: handedBackAt.Add(time.Minute)}})
	if got := issue(t, pool, "LEGION-100"); !got.Answered() {
		t.Fatalf("root after the approval of the document it now waits on = %#v, want it answered", got)
	}
}

// The gate seed reads Dispatch rather than an event, so its version carries no place in the event
// log. It answers a wait only when that read also shows the approval request waiting on the agent,
// which is what a newer version does to it; otherwise the person is still being asked, and the
// wait moves to that version.
func TestASeedVersionAnswersOnlyWhenDispatchShowsTheRequestWithTheAgent(t *testing.T) {
	for _, tc := range []struct {
		name          string
		waitsOnAgent  bool
		wantAnswered  bool
		wantedVersion int
	}{
		{name: "the request waits on the agent", waitsOnAgent: true, wantAnswered: true, wantedVersion: 5},
		{name: "the request still waits on the person", wantedVersion: 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{GateVersion: 4, Artifact: "artifact-root", Since: handBack})
			applyFact(t, pool, admission, "seed-version", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
				Kind: intake.DispatchArtifactVersion, Version: 5, RequestWaitsOnAgent: tc.waitsOnAgent})
			got := issue(t, pool, "LEGION-100")
			if got.Answered() != tc.wantAnswered || got.Awaiting.GateVersion != tc.wantedVersion {
				t.Fatalf("root after the seed's version = %#v, want answered %t on version %d", got, tc.wantAnswered, tc.wantedVersion)
			}
		})
	}
}

// Round 22 same-model P2-1, the gate's version: a person opens an ask to the architect, the
// architect replies leaving the turn with the person, and the person replies handing it back —
// an ask the root could never wait on, so the architect's reply declared nothing. The architect
// then hands its approval request to the person: the wait begins at that hand-back, on the gate,
// and the person's earlier reply, which the architect already had, answers nothing.
func TestAnAnsweredHandBackDoesNotStartTheNextGateWait(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
	applyFact(t, pool, admission, "p-open", intake.DispatchAskOpened{Key: "LEGION-100", Ask: "ask-p", Kind: "question", Mark: seqMark(5)})
	applyFact(t, pool, admission, "a-reply", intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-p", WaitingOn: "human", Actor: architect, Mark: seqMark(6)})
	applyFact(t, pool, admission, "p-reply", intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-p", WaitingOn: "agent", Actor: alice, Mark: seqMark(7)})
	assertAdmittedInItsSlot(t, pool, "a reply on an ask the root cannot wait on")
	applyFact(t, pool, admission, "hand-back", requestHandedBack(20))
	got := issue(t, pool, "LEGION-100")
	if !got.AwaitingPerson() || got.Answered() || got.Awaiting.GateVersion != 4 || got.Awaiting.Since.Seq != 20 {
		t.Fatalf("root = %#v, task %q, want it waiting on version 4 from 20, unanswered", got, lastArchitectTask(t, pool, "LEGION-100"))
	}
	if task := lastArchitectTask(t, pool, "LEGION-100"); task != "" {
		t.Fatalf("architect told %q, want no task in place", task)
	}
}

// Round 22 second-review P2-1: the architect hands its approval request back and opens a question;
// a person answers the request with a changes request. Whatever order the events arrive in, and
// whether a member of the tree holds the slot meanwhile, the architect hears of that changes
// request once: by the design-changes-requested notice when the root holds its slot, a handout
// that takes in what it handed back before, or by the resume task when the request answered the
// root's wait — never both.
func TestAChangesRequestToldByNoticeIsNotToldAgainInPlace(t *testing.T) {
	request := intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-approval", Kind: "approval", State: "answered", Actor: alice, Mark: seqMark(12)}
	changes := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 4,
		Reason: "name the rollback path", Mark: seqMark(13)}
	for _, tc := range []struct {
		name    string
		guarded bool
		events  []intake.Fact
	}{
		{name: "in sequence, no guard", events: []intake.Fact{requestHandedBack(10), openedBy("ask-q", 11), request, changes}},
		{name: "in sequence, a member mid-phase", guarded: true, events: []intake.Fact{requestHandedBack(10), openedBy("ask-q", 11), request, changes}},
		{name: "reordered by a nak, no guard", events: []intake.Fact{changes, request, requestHandedBack(10), openedBy("ask-q", 11)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			runningRoot(t, pool)
			if tc.guarded {
				seedChild(t, pool, phase.Implementing)
			}
			seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
			for i, event := range tc.events {
				applyFact(t, pool, admission, fmt.Sprintf("event-%d", i), event)
			}
			if tc.guarded {
				parkChild(t, pool, admission)
			}
			if issue(t, pool, "LEGION-100").Answered() {
				nextFinished(t, pool, admission)
			}
			told := len(gateNotices(t, pool, "LEGION-100", "design-changes-requested"))
			if strings.Contains(lastArchitectTask(t, pool, "LEGION-100"), "requested changes") {
				told++
			}
			if told != 1 {
				t.Fatalf("the changes request reached the architect %d times (notices %#v, task %q), want once",
					told, gateNotices(t, pool, "LEGION-100", "design-changes-requested"), lastArchitectTask(t, pool, "LEGION-100"))
			}
		})
	}
}

// Round 22 outside-whole P2, the claim-holder flow: a person holds the root's claim, so the root
// keeps its slot while its architect asks them a question; the person answers it and releases the
// claim. Nothing the architect handed back is still with the person, so the hand-back is taken in.
// The architect then requests approval and registers the version: the root waits on its gate from
// the registration, and the person's answer before it, which the architect already had, is not
// told to it again in place.
func TestTheClaimHoldersAnswerBeforeARegistrationIsNotToldInPlace(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	personHoldsTheClaim(t, pool)
	applyFact(t, pool, admission, "question", openedBy("ask-q", 7))
	assertAdmittedInItsSlot(t, pool, "a question to the claim's holder")
	applyFact(t, pool, admission, "answer", answeredBy("ask-q", 8))
	applyThroughMirror(t, pool, admission, "released", intake.DispatchClaim{Key: "LEGION-100", Seq: 10})
	if got := assertAdmittedInItsSlot(t, pool, "the answer and the release"); got.DeclaredSeq != 0 {
		t.Fatalf("declared_seq = %d, want the answered hand-back taken in", got.DeclaredSeq)
	}
	applyFact(t, pool, admission, "register", intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 1, DispatchVersion: 1,
		Since: seqMark(20), RequestAsk: "ask-approval"})
	got := issue(t, pool, "LEGION-100")
	if !got.AwaitingPerson() || got.Answered() || got.Awaiting.GateVersion != 1 || got.Awaiting.Since.Seq != 20 {
		t.Fatalf("root after the registration = %#v, task %q, want it waiting on version 1 from 20", got, lastArchitectTask(t, pool, "LEGION-100"))
	}
	if task := lastArchitectTask(t, pool, "LEGION-100"); task != "" {
		t.Fatalf("architect told %q in place, want nothing", task)
	}
}

// A wait on the gate begins at the request's hand-back, its sequence and Dispatch's time of it,
// which the root's record keeps and `legion state` shows as the wait's since.
func TestARequestHandBackRecordsWhenTheGateWaitBegins(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	handBackOnAClosedGate(t, pool, admission)
	got := issue(t, pool, "LEGION-100")
	if !got.Awaiting.Since.At.Equal(handedBackAt) || got.Awaiting.Since.Seq != handBack.Seq {
		t.Fatalf("the gate wait begins at %+v, want the hand-back %d at %s", got.Awaiting.Since, handBack.Seq, handedBackAt)
	}
}

// A person's approval that opens the gate a root waits on answers the wait once, by that
// approval, and the architect hears of it once, from its resume: no design-approved notice beside
// it, and the admission pass that also finds the gate open answers nothing more.
func TestAnApprovalOpeningTheGateARootWaitsOnIsToldOnceByItsResume(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	handBackOnAClosedGate(t, pool, admission)
	applyFact(t, pool, admission, "approved", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactApproved, Version: 4, Mark: record.EventMark{Seq: 11, At: handedBackAt.Add(time.Minute)}})
	got := issue(t, pool, "LEGION-100")
	if !got.Answered() || got.Awaiting.AnsweredBy != record.AnsweredByApproval || got.Awaiting.AnsweredDetail != "version 4" {
		t.Fatalf("root after the approval = %#v, want it answered by the approval of version 4", got)
	}
	applyFact(t, pool, admission, "tick", intake.ControllerTick{})
	if again := issue(t, pool, "LEGION-100"); !again.Awaiting.AnsweredAt.Equal(*got.Awaiting.AnsweredAt) || again.Awaiting.AnsweredBy != got.Awaiting.AnsweredBy {
		t.Fatalf("root after the next pass = %#v, want the answer it had", again)
	}
	if opened := gateNotices(t, pool, "LEGION-100", "design-approved"); len(opened) != 0 {
		t.Fatalf("design-approved notices = %#v, want none for a root its resume tells", opened)
	}
	nextFinished(t, pool, admission)
	if task := lastArchitectTask(t, pool, "LEGION-100"); !strings.Contains(task, "was approved") {
		t.Fatalf("resume task = %q, want it to say the spec was approved", task)
	}
	starts := 0
	for _, op := range architectOps(t, pool, "LEGION-100") {
		if op == "start" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("architect starts = %d, want one resume", starts)
	}
}

// A changes request delivered after a newer hand-back of the approval request takes in only what
// was handed back at or before its review: the request handed back after the review still stands,
// so once the member holding the slot leaves, the root waits on its gate from that hand-back, as
// it would had the events arrived in order.
func TestALateChangesRequestKeepsANewerRequestHandBack(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	seedChild(t, pool, phase.Implementing)
	seedRequestedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
	applyFact(t, pool, admission, "hand-back", requestHandedBack(20))
	applyFact(t, pool, admission, "changes", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: seqMark(18)})
	if got := assertAdmittedInItsSlot(t, pool, "a guard holding the slot"); got.DeclaredSeq != 20 || got.HandoutSeq != 18 {
		t.Fatalf("declared_seq = %d, handout_seq = %d; want the hand-back at 20 kept and the handout at the review's 18", got.DeclaredSeq, got.HandoutSeq)
	}
	parkChild(t, pool, admission)
	if got := issue(t, pool, "LEGION-100"); !got.AwaitingPerson() || got.Answered() || got.Awaiting.GateVersion != 4 || got.Awaiting.Since.Seq != 20 {
		t.Fatalf("root once the member left = %#v, want it waiting on version 4 from 20", got)
	}
}
