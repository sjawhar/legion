package admit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The tests below pin each finding of review rounds 18, 20 and 21 that the level-triggered
// awaiting_person design closes: a root waits on a person when, at any fact, its architect handed
// something back since its last handout, something is still with the person, no guard keeps the
// slot, and the architect has taken its work (workflow's ReconcileAwaiting).

// runningRoot records LEGION-100 holding the one slot with its architect running a turn it took,
// and LEGION-NEXT waiting for a slot.
func runningRoot(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectClaim(t, pool, "LEGION-100", architect.ID)
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
	return intake.DispatchAskReply{Key: "LEGION-100", Ask: "ask-approval", Kind: "approval", WaitingOn: "human", Actor: architect, Mark: seqMark(seq)}
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
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
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
				apply(t, pool, admission, "released-on-an-edit", intake.DispatchIssue{Key: "LEGION-100", ResolvedRepo: "acme/widgets", Seq: 30,
					Type: "issue.updated", Status: "in_progress", Title: "LEGION-100, renamed", Rank: "A", HandedOver: true}, admission.engine)
			},
			again: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				applyFact(t, pool, admission, "released", intake.DispatchClaim{Key: "LEGION-100", Seq: 30})
			}},
		{name: "issue.released", guard: personHoldsTheClaim,
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				applyFact(t, pool, admission, "released", intake.DispatchClaim{Key: "LEGION-100", Seq: 30})
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
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
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

func personHoldsTheClaim(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	root := issue(t, pool, "LEGION-100")
	root.ClaimedBy, root.ClaimSeq, root.DispatchStatus, root.LastDispatchSeq = "user:bob", 3, "in_progress", 3
	putIssue(t, pool, root)
}

// Round 21 second-review P2-1: a member of the tree is mid-phase; the architect hands the approval
// request back, then opens a question; a person answers another question; the member leaves. The
// person's act came after the hand-back, so the architect is told of it where it stands, its slot
// kept, and of everything still with the person: the request and the question.
func TestAGateWaitHeldByAGuardIsToldDespiteAnActOnAnotherAsk(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	seedChild(t, pool, phase.Implementing)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
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
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
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
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
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
// question when no gate names that request; when one does, its review event answers it, whether it
// opens the gate or finds it already open.
func TestAPersonsAnswerOfTheApprovalRequest(t *testing.T) {
	answer := intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-approval", Kind: "approval", State: "answered", Actor: alice, Mark: seqMark(11)}
	review := intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 4, Mark: seqMark(11)}
	for _, tc := range []struct {
		name     string
		gate     *record.DesignGate
		byAnswer bool
	}{
		{name: "no_gate", byAnswer: true},
		{name: "gate_already_open", gate: &record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4,
			ApprovedVersion: new(4), ApprovedMark: seqMark(5)}},
		{name: "gate_closed", gate: &record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{Ask: "ask-root", Since: handBack})
			if tc.gate != nil {
				seedGate(t, pool, *tc.gate)
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
		seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
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
		seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
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

// Round 20 outside-whole P2-1 and its variants: what is still with the person when the root resumes
// is named in its task — a second question of the same turn, a question opened while it was
// answered and not yet resumed, a closed gate registered while it waited on its question — and an
// item the person answered before the resume is not.
func TestTheResumeTaskNamesEveryItemStillWithThePerson(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before []intake.Fact
		after  []intake.Fact
		named  []string
		absent []string
	}{
		{name: "a second question opened in the same turn",
			before: []intake.Fact{openedBy("ask-q2", 11)}, named: []string{"ask ask-q2", "Still with the person"}},
		{name: "a question opened once the root was answered, before it resumed",
			after: []intake.Fact{openedBy("ask-q3", 25)}, named: []string{"ask ask-q3"}},
		{name: "a closed gate registered while the root waited on its question",
			before: []intake.Fact{intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4,
				RequestAsk: "ask-approval", Since: seqMark(12)}},
			named: []string{"approval request for spec version 4", "register_gate"}},
		{name: "a second question the person answered before the resume",
			before: []intake.Fact{openedBy("ask-q2", 11), answeredBy("ask-q2", 15)},
			named:  []string{"ask ask-q2"}, absent: []string{"Still with the person"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			runningRoot(t, pool)
			applyFact(t, pool, admission, "q1", openedBy("ask-q1", 10))
			for i, fact := range tc.before {
				source := "dispatch"
				if _, ok := fact.(intake.GateRegistered); ok {
					source = "api"
				}
				if _, err := intake.ApplyFact(context.Background(), pool, source, fmt.Sprintf("before-%d", i), fact, admission.engine, admission); err != nil {
					t.Fatalf("apply %T: %v", fact, err)
				}
			}
			applyFact(t, pool, admission, "answer-q1", answeredBy("ask-q1", 20))
			for i, fact := range tc.after {
				applyFact(t, pool, admission, fmt.Sprintf("after-%d", i), fact)
			}
			nextFinished(t, pool, admission)
			task := lastArchitectTask(t, pool, "LEGION-100")
			for _, want := range tc.named {
				if !strings.Contains(task, want) {
					t.Fatalf("resume task = %q, want it to name %q", task, want)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(task, unwanted) {
					t.Fatalf("resume task = %q, want it not to name %q", task, unwanted)
				}
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
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 4})
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

// Round 18 same-model P2-1: settlement restoring a decision block it retracted reopens the ask's
// mirror row but declares nothing: no agent asked anything, and the root keeps its slot.
func TestARestoredAskIsMirroredButDeclaresNothing(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	runningRoot(t, pool)
	mirrorAsk(t, pool, record.RootAsk{Issue: "LEGION-100", Ask: "ask-q", Kind: "question", Seq: 6, ByTree: true, ClosedSeq: 6, ClosedBy: record.ClosedBySettlement})
	applyFact(t, pool, admission, "restored", intake.DispatchAskRestored{Key: "LEGION-100", Ask: "ask-q", Kind: "question", Open: true, Mark: seqMark(15)})
	got := assertAdmittedInItsSlot(t, pool, "the restore")
	if got.DeclaredSeq != 0 {
		t.Fatalf("root after the restore = %#v, want nothing declared", got)
	}
	var row *record.RootAsk
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if row, err = record.NewStore().RootAsk(context.Background(), tx, "LEGION-100", "ask-q"); err != nil {
			t.Fatalf("read the mirror: %v", err)
		}
	})
	if row == nil || !row.WithHuman() || row.Seq != 15 {
		t.Fatalf("mirror after the restore = %#v, want it open with the person at 15", row)
	}
}

// Round 18 outside-whole P1-3 class: a registration of another document while the root waits on
// the first rebinds the gate's approval request to the new document's, so the old request's
// answer, a review of a document no gate names now, answers nothing; the new document's approval
// answers the wait.
func TestARegistrationOfAnotherDocumentRebindsTheRequest(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	handBackOnAClosedGate(t, pool, admission)
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-other", intake.GateRegistered{Issue: "LEGION-100",
		ArtifactID: "artifact-second", Version: 1, DispatchVersion: 1, RequestAsk: "ask-second", Since: seqMark(11)}, admission.engine, admission); err != nil {
		t.Fatalf("register the second document: %v", err)
	}
	if gate := storedGate(t, pool, "LEGION-100"); gate.RequestAsk != "ask-second" || gate.ArtifactID != "artifact-second" || classify.DesignGateOpen(*gate) {
		t.Fatalf("gate after the second registration = %#v, want it closed on artifact-second with request ask-second", gate)
	}
	applyFact(t, pool, admission, "old-request-answered", intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-approval", Kind: "approval",
		State: "answered", Actor: alice, Mark: seqMark(12)})
	if got := issue(t, pool, "LEGION-100"); got.Answered() || got.Awaiting.Artifact != "artifact-second" {
		t.Fatalf("root after the old request's answer = %#v, want it still waiting on artifact-second", got)
	}
	applyFact(t, pool, admission, "approved-second", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-second",
		Kind: intake.DispatchArtifactApproved, Version: 1, Mark: seqMark(13)})
	if got := issue(t, pool, "LEGION-100"); !got.Answered() {
		t.Fatalf("root after the second document's approval = %#v, want it answered", got)
	}
}
