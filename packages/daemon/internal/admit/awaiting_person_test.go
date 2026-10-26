package admit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// awaitingArchitect is the architect claim token of a root of the test project.
func awaitingArchitect(t *testing.T, issue string) claim.Token {
	t.Helper()
	project, err := claim.ProjectToken(testProject)
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	token, err := claim.NewToken(project, issue, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("architect token of %s: %v", issue, err)
	}
	return token
}

// seedArchitectClaim records the root's architect working a task it has taken, which is the one
// state in which a root may give its slot back: nothing it was given is still owed to it.
func seedArchitectClaim(t *testing.T, pool *pgxpool.Pool, issue string, confirmed bool) {
	t.Helper()
	token := awaitingArchitect(t, issue)
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $3, 'architect', 1, 'ses_architect', '', 'working', 0, 0, 0, 0)`,
		string(token), testProject, issue); err != nil {
		t.Fatalf("record the architect claim of %s: %v", issue, err)
	}
	var confirmedAt *time.Time
	if confirmed {
		at := fixedNow
		confirmedAt = &at
	}
	if _, err := pool.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at, generation, phase, confirmed_at)
		values ($1, 'outbox:7', 'Decompose it.', now(), 1, 'admitted', $2)`, string(token), confirmedAt); err != nil {
		t.Fatalf("record the architect's task of %s: %v", issue, err)
	}
}

// seedHandBack records that the root's architect handed a question back at seq, and the mirror row
// of that question, still with the person.
func seedHandBack(t *testing.T, pool *pgxpool.Pool, issue, ask string, seq int64) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutRootAsk(context.Background(), tx, record.RootAsk{Issue: issue, Ask: ask, Kind: record.AskQuestion,
			Open: true, Seq: seq, WaitingOn: "human", WaitingOnSeq: seq, ByTree: true}); err != nil {
			t.Fatalf("record the question of %s: %v", issue, err)
		}
		if err := records.Declare(context.Background(), tx, issue, seq); err != nil {
			t.Fatalf("declare the hand-back of %s: %v", issue, err)
		}
	})
}

// seedDispatchClaim records who the mirror of Dispatch's issues shows holding the issue's claim.
func seedDispatchClaim(t *testing.T, pool *pgxpool.Pool, issue, holder string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		if err := records.PutDispatchIssue(context.Background(), tx, record.DispatchIssueRow{
			Key: issue, Project: testProject, Status: "in_progress", Seq: 8}); err != nil {
			t.Fatalf("record the Dispatch mirror of %s: %v", issue, err)
		}
		if err := records.PutDispatchClaim(context.Background(), tx, issue, holder, 9); err != nil {
			t.Fatalf("record the Dispatch claim of %s: %v", issue, err)
		}
	})
}

// awaitingRig is a slotted root whose architect handed question ask-q back, with a second root
// waiting in line behind it, and the admission that decides them on every fact.
func awaitingRig(t *testing.T, pool *pgxpool.Pool) *Admission {
	t.Helper()
	seedSlotted(t, pool, "LEGION-208", "U")
	seedWaiting(t, pool, "LEGION-300", "V")
	seedArchitectClaim(t, pool, "LEGION-208", true)
	seedHandBack(t, pool, "LEGION-208", "ask-q", 5)
	return newAdmission(t, 1, nil)
}

// supervise returns every supervise request queued for issue, in order.
func superviseRequests(t *testing.T, pool *pgxpool.Pool, issue string) []record.SuperviseRequest {
	t.Helper()
	var got []record.SuperviseRequest
	for _, e := range effects(t, pool) {
		if e.issue != issue {
			continue
		}
		if request, ok := e.payload.(record.SuperviseRequest); ok {
			got = append(got, request)
		}
	}
	return got
}

// A root whose architect handed a question to a person, and whose tree is otherwise idle, moves to
// awaiting_person: its architect is suspended with its session kept, the controller is told, and
// the slot it gave back goes to the root waiting behind it.
func TestARootWaitingOnAPersonReturnsItsSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := awaitingRig(t, pool)
	tick(t, pool, admission, "tick-1")

	root := issue(t, pool, "LEGION-208")
	if root.Phase != phase.AwaitingPerson || root.Awaiting == nil || root.Awaiting.Ask != "ask-q" || root.Awaiting.Since.Seq != 5 {
		t.Fatalf("root = %+v (awaiting %+v), want it waiting on ask-q from 5", root, root.Awaiting)
	}
	if root.Awaiting.AnsweredAt != nil {
		t.Fatalf("awaiting = %+v, want it unanswered", root.Awaiting)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-300", Index: 0, AdmittedAt: fixedNow}})
	requests := superviseRequests(t, pool, "LEGION-208")
	if len(requests) != 1 || requests[0].Op != "suspend" || requests[0].Role != claim.RoleArchitect {
		t.Fatalf("supervise requests = %+v, want one architect suspend", requests)
	}
	var notices int
	for _, e := range effects(t, pool) {
		if notice, ok := e.payload.(record.ControllerNotice); ok && notice.Kind == record.AwaitingPersonNotice {
			notices++
			if !strings.Contains(notice.Reason, "LEGION-208") {
				t.Fatalf("awaiting-person notice %q, want it to name the root", notice.Reason)
			}
		}
	}
	if notices != 1 {
		t.Fatalf("awaiting-person notices = %d, want one", notices)
	}
}

// A person's answer puts the waiting root at the front of the line, ahead of every root that never
// ran, and the next free slot resumes its architect in its own session, told what answered it. The
// resume is a handout: what it handed back before is cleared, so only its next hand-back starts
// the next wait.
func TestAnAnsweredRootTakesTheNextSlotAndResumesItsArchitect(t *testing.T) {
	pool := migratedPool(t)
	admission := awaitingRig(t, pool)
	tick(t, pool, admission, "tick-1")
	if got := issue(t, pool, "LEGION-208").Phase; got != phase.AwaitingPerson {
		t.Fatalf("phase = %s, want the root waiting", got)
	}
	// The root waiting behind it finishes, freeing the slot the answered root takes.
	done := issue(t, pool, "LEGION-300")
	done.Phase = phase.Done
	putIssue(t, pool, done)
	answerWait(t, pool, "LEGION-208", record.AnsweredByAsk, "ask-q")
	assertWaiting(t, pool, []string{"LEGION-208"})

	tick(t, pool, admission, "tick-2")
	root := issue(t, pool, "LEGION-208")
	if root.Phase != phase.Admitted || root.Awaiting != nil {
		t.Fatalf("root = %+v (awaiting %+v), want it admitted again with no wait", root, root.Awaiting)
	}
	if root.Status != "in_progress" {
		t.Fatalf("status = %s, want the status it ran with kept", root.Status)
	}
	if root.DeclaredSeq != 0 || root.HandoutSeq < 5 {
		t.Fatalf("handout_seq = %d, declared_seq = %d, want the resume recorded as a handout", root.HandoutSeq, root.DeclaredSeq)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-208", Index: 0, AdmittedAt: fixedNow}})
	requests := superviseRequests(t, pool, "LEGION-208")
	if len(requests) != 2 || requests[1].Op != "start" || requests[1].Role != claim.RoleArchitect {
		t.Fatalf("supervise requests = %+v, want the suspend then the architect's resume", requests)
	}
	if !strings.Contains(requests[1].Task, "ask-q") {
		t.Fatalf("resume task %q, want it to name the ask that was answered", requests[1].Task)
	}
	// The resume writes no Dispatch status: the root kept the one it ran with.
	for _, e := range effects(t, pool) {
		if write, ok := e.payload.(record.StatusWrite); ok && e.issue == "LEGION-208" {
			t.Fatalf("status write %+v, want none for a root that kept its status", write)
		}
	}
}

// A hand-back the daemon made after a person's earlier answer begins the wait at that newest
// hand-back, so the answer before it does not end the new wait: the root waits on the person
// again rather than resuming at once. (Round 22's declared_seq finding.)
func TestAnAnsweredHandBackDoesNotStartTheNextWait(t *testing.T) {
	for _, tc := range []struct {
		name string
		// guarded holds the root's tree busy while the second question is opened, so the decision
		// comes a pass later, when the guard clears.
		guarded bool
	}{
		{name: "decided on the pass that opens the second question"},
		{name: "decided once the guard clears", guarded: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedSlotted(t, pool, "LEGION-208", "U")
			seedArchitectClaim(t, pool, "LEGION-208", true)
			admission := newAdmission(t, 1, nil)
			if tc.guarded {
				putIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
					Project: testProject, Title: "child", Tree: "LEGION-208", Parent: new("LEGION-208"), Phase: phase.Implementing,
					Generation: 1, Status: "in_progress", Rank: "V", HandedOver: true})
			}
			// The first question is handed back, answered by the person, and the second is handed
			// back after that answer.
			seedHandBack(t, pool, "LEGION-208", "ask-q1", 5)
			inTx(t, pool, func(tx pgx.Tx) {
				records := record.NewStore()
				if err := records.PutRootAsk(context.Background(), tx, record.RootAsk{Issue: "LEGION-208", Ask: "ask-q1",
					Kind: record.AskQuestion, Open: false, Seq: 7, WaitingOn: "human", WaitingOnSeq: 5, ClosedBy: record.ClosedByPerson, ClosedSeq: 7,
					ByTree: true, PersonAnswerSeq: 7}); err != nil {
					t.Fatalf("record the answered question: %v", err)
				}
			})
			seedHandBack(t, pool, "LEGION-208", "ask-q2", 9)
			if tc.guarded {
				tick(t, pool, admission, "tick-guarded")
				if got := issue(t, pool, "LEGION-208").Phase; got != phase.Admitted {
					t.Fatalf("phase = %s, want the root held by its tree's member", got)
				}
				member := issue(t, pool, "LEGION-209")
				member.Phase, member.Status = phase.Done, "done"
				putIssue(t, pool, member)
			}
			tick(t, pool, admission, "tick-1")
			root := issue(t, pool, "LEGION-208")
			if root.Phase != phase.AwaitingPerson || root.Awaiting == nil || root.Awaiting.Ask != "ask-q2" {
				t.Fatalf("root = %+v (awaiting %+v), want it waiting on the second question", root, root.Awaiting)
			}
			if root.Awaiting.AnsweredAt != nil {
				t.Fatalf("awaiting = %+v, want the earlier answer to end no wait", root.Awaiting)
			}
			if root.Awaiting.Since.Seq != 9 {
				t.Fatalf("the wait begins at %d, want the newest hand-back 9", root.Awaiting.Since.Seq)
			}
		})
	}
}

// A hand-back nothing of which is still with the person is taken in on the pass that sees it: the
// running architect already has the person's answer, so when the question later returns to the
// person by no hand-back of the architect's (Dispatch's settlement restoring it), no wait starts
// from the old hand-back and the architect is not resumed with an answer it already read.
func TestAHandBackNoLongerWithThePersonIsTakenIn(t *testing.T) {
	pool := migratedPool(t)
	seedSlotted(t, pool, "LEGION-208", "U")
	seedArchitectClaim(t, pool, "LEGION-208", true)
	seedHandBack(t, pool, "LEGION-208", "ask-q", 5)
	answered := record.RootAsk{Issue: "LEGION-208", Ask: "ask-q", Kind: record.AskQuestion, Open: false, Seq: 7, WaitingOn: "human",
		WaitingOnSeq: 5, ClosedBy: record.ClosedByPerson, ClosedSeq: 7, ByTree: true, PersonAnswerSeq: 7}
	putAsk := func(ask record.RootAsk) {
		t.Helper()
		inTx(t, pool, func(tx pgx.Tx) {
			if err := record.NewStore().PutRootAsk(context.Background(), tx, ask); err != nil {
				t.Fatalf("record ask-q: %v", err)
			}
		})
	}
	putAsk(answered)
	admission := newAdmission(t, 1, nil)
	tick(t, pool, admission, "tick-answered")
	if root := issue(t, pool, "LEGION-208"); root.Phase != phase.Admitted || root.DeclaredSeq != 0 {
		t.Fatalf("root = %+v, want it admitted with its answered hand-back taken in", root)
	}

	restored := answered
	restored.Open, restored.Seq = true, 9
	putAsk(restored)
	tick(t, pool, admission, "tick-restored")
	if root := issue(t, pool, "LEGION-208"); root.Phase != phase.Admitted || root.Awaiting != nil {
		t.Fatalf("root = %+v (awaiting %+v), want it admitted, waiting on nothing", root, root.Awaiting)
	}
	if requests := superviseRequests(t, pool, "LEGION-208"); len(requests) != 0 {
		t.Fatalf("supervise requests = %+v, want the architect neither suspended nor resumed", requests)
	}
}

// A root the Dispatch consumer has not caught up to decides nothing: its listing is ahead of the
// record, so a question it shows may already be answered. The hold's release decides it.
func TestAHeldRootIsNotDecidedBeforeTheConsumerCatchesUp(t *testing.T) {
	pool := migratedPool(t)
	admission := awaitingRig(t, pool)
	admission.pending = map[string]dispatch.IssueSummary{"LEGION-208": {Key: "LEGION-208", Status: "todo", LastSeq: 20}}
	admission.target = 100
	tick(t, pool, admission, "tick-held")
	if got := issue(t, pool, "LEGION-208").Phase; got != phase.Admitted {
		t.Fatalf("phase = %s, want the held root left where it stands", got)
	}

	position := intake.DispatchConsumerPosition{AckFloorStream: 100, Idle: true}
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "position-1", position, admission); err != nil {
		t.Fatalf("apply the consumer position: %v", err)
	}
	if got := issue(t, pool, "LEGION-208").Phase; got != phase.AwaitingPerson {
		t.Fatalf("phase = %s, want the released root decided", got)
	}
}

// A root whose architect has not taken the work it was given keeps its slot: suspending it then
// would cancel that work. The confirmation of the task is itself a fact, so the wait starts there
// rather than at the next tick.
func TestAWaitHeldByUntakenWorkStartsAtTheTaskConfirmation(t *testing.T) {
	pool := migratedPool(t)
	seedSlotted(t, pool, "LEGION-208", "U")
	seedArchitectClaim(t, pool, "LEGION-208", false)
	seedHandBack(t, pool, "LEGION-208", "ask-q", 5)
	admission := newAdmission(t, 1, nil)
	tick(t, pool, admission, "tick-1")
	if got := issue(t, pool, "LEGION-208").Phase; got != phase.Admitted {
		t.Fatalf("phase = %s, want the root to keep its slot while its task is untaken", got)
	}

	confirmed := fixedNow
	if _, err := pool.Exec(context.Background(), `update pending_task_deliveries set confirmed_at = $1`, confirmed); err != nil {
		t.Fatalf("confirm the architect's task: %v", err)
	}
	fact := intake.TaskConfirmed{Issue: "LEGION-208", Role: claim.RoleArchitect, Generation: 1}
	if _, err := intake.ApplyFact(context.Background(), pool, "supervise", "task-confirmed-1", fact, admission); err != nil {
		t.Fatalf("apply the task's confirmation: %v", err)
	}
	if got := issue(t, pool, "LEGION-208").Phase; got != phase.AwaitingPerson {
		t.Fatalf("phase = %s, want the wait to start once the task was taken", got)
	}
}

// An awaiting root whose Dispatch claim a person or an outside session took cannot resume in its
// own session, so the daemon takes it out of the workflow and tells the controller; one the
// daemon's own session or a session of its tree holds resumes as it is.
func TestAnAwaitingRootAnOutsiderClaimedLeavesTheWorkflow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		holder string
		leaves bool
	}{
		{"a person", "user:alice", true},
		{"a session outside the tree", "session:ses_elsewhere", true},
		{"the daemon's own session", "session:" + dispatch.DaemonSession("LEGION-208"), false},
		{"nobody", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := awaitingRig(t, pool)
			tick(t, pool, admission, "tick-1")
			if got := issue(t, pool, "LEGION-208").Phase; got != phase.AwaitingPerson {
				t.Fatalf("phase = %s, want the root waiting", got)
			}
			seedDispatchClaim(t, pool, "LEGION-208", tc.holder)
			tick(t, pool, admission, "tick-2")
			root := issue(t, pool, "LEGION-208")
			if !tc.leaves {
				if root.Phase != phase.AwaitingPerson {
					t.Fatalf("phase = %s, want the root still waiting", root.Phase)
				}
				return
			}
			if root.Phase != phase.Done || root.Awaiting != nil {
				t.Fatalf("root = %+v (awaiting %+v), want it out of the workflow", root, root.Awaiting)
			}
			var unslotted int
			for _, e := range effects(t, pool) {
				if notice, ok := e.payload.(record.ControllerNotice); ok && notice.Kind == record.UnslottedNotice {
					unslotted++
					if !strings.Contains(notice.Reason, "awaits a person") {
						t.Fatalf("unslotted notice %q, want it to say the root was waiting on a person", notice.Reason)
					}
				}
			}
			if unslotted != 1 {
				t.Fatalf("unslotted notices = %d, want one", unslotted)
			}
		})
	}
}

// A root waiting on a person is a running tree: a session's lifecycle write on it is set back and
// its architect told, as on a root that holds its slot.
func TestASessionsStatusWriteOnARootWaitingOnAPersonIsSetBack(t *testing.T) {
	pool := migratedPool(t)
	admission := awaitingRig(t, pool)
	engine := admission.engine
	tick(t, pool, admission, "tick-1")
	if got := issue(t, pool, "LEGION-208").Phase; got != phase.AwaitingPerson {
		t.Fatalf("phase = %s, want the root waiting", got)
	}
	apply(t, pool, admission, "session-write", intake.DispatchIssue{Key: "LEGION-208", Seq: 30, Type: "issue.updated", Status: "done",
		Title: "root", Rank: "U", HandedOver: handed, ActorSession: "ses_elsewhere", ResolvedRepo: "acme/widgets"}, engine)
	root := issue(t, pool, "LEGION-208")
	if root.Phase != phase.AwaitingPerson || root.Status != "in_progress" {
		t.Fatalf("root = %+v, want the session's write set back and the wait kept", root)
	}
	var reasserted int
	for _, e := range effects(t, pool) {
		if write, ok := e.payload.(record.StatusWrite); ok && e.issue == "LEGION-208" && write.Status == "in_progress" {
			reasserted++
		}
	}
	if reasserted != 1 {
		t.Fatalf("status writes setting the root back = %d, want one", reasserted)
	}
}

// answerWait records a person's answer of the root's wait, as the workflow's answer handlers do.
func answerWait(t *testing.T, pool *pgxpool.Pool, key string, by record.Answer, detail string) {
	t.Helper()
	root := issue(t, pool, key)
	if root.Awaiting == nil {
		t.Fatalf("issue %s holds no wait to answer", key)
	}
	at := fixedNow
	root.Awaiting.AnsweredAt, root.Awaiting.AnsweredBy, root.Awaiting.AnsweredDetail = &at, by, detail
	putIssue(t, pool, root)
}
