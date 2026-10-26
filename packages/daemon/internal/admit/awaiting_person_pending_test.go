package admit

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A root waiting on its architect's question is answered by a person's answer of the root's
// approval request too: no gate may carry that answer on (none registered, or one already open at
// the version approved), and the root would otherwise wait until the person also answered the
// question.
func TestAPersonsAnswerOfTheApprovalRequestAnswersAQuestionWait(t *testing.T) {
	answer := intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-approval", Kind: "approval", State: "answered", Actor: alice,
		Mark: record.EventMark{Seq: 11, At: handedBackAt.Add(time.Minute)}}
	for _, tc := range []struct {
		name string
		gate *record.DesignGate
	}{
		{name: "no gate registered"},
		{name: "a gate already open at the version approved", gate: &record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root",
			LatestVersion: 4, ApprovedVersion: new(4), ApprovedMark: record.EventMark{Seq: 5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{Ask: "ask-root", Since: handBack})
			if tc.gate != nil {
				seedGate(t, pool, *tc.gate)
			}
			applyFact(t, pool, admission, "approval-answered", answer)
			if got := issue(t, pool, "LEGION-ROOT"); !got.Answered() {
				t.Fatalf("root waiting on its question after the person answered the approval request = %#v, want answered", got)
			}
		})
	}
}

// A wait the root would have started while it already waited, or once answered before it resumed,
// is held pending: the resumed architect is told which asks, and which approval request, are still
// with the person, and to hand each back, which starts the next wait and returns the slot. One the
// person answered meanwhile is not named.
func TestTheResumedArchitectIsToldEveryWaitStillWithThePerson(t *testing.T) {
	answerQ1 := intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-q1", Kind: "question", State: "answered", Actor: alice,
		Mark: record.EventMark{Seq: 20, At: handedBackAt.Add(time.Minute)}}
	opened := func(ask string, seq int64) intake.DispatchAskOpened {
		return intake.DispatchAskOpened{Key: "LEGION-ROOT", Ask: ask, Kind: "question", AuthorSession: architect.ID, Mark: record.EventMark{Seq: seq}}
	}
	for _, tc := range []struct {
		name   string
		before []intake.Fact
		after  []intake.Fact
		named  []string
		absent []string
	}{
		{name: "a second question opened while the root waited",
			before: []intake.Fact{opened("ask-q2", 11)}, named: []string{"ask ask-q2", "Still with the person"}},
		{name: "a question opened once the root was answered, before it resumed",
			after: []intake.Fact{opened("ask-q3", 25)}, named: []string{"ask ask-q3"}},
		{name: "a closed gate registered while the root waited on its question",
			before: []intake.Fact{intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4,
				Since: record.EventMark{Seq: 12}}},
			named: []string{"approval request for spec version 4", "register_gate"}},
		// The person's answer of the second question answers the wait (any act of theirs on the
		// root's asks does) and is what the task names; the first question, answered after it, is
		// no longer with the person.
		{name: "a second question the person answered before the resume",
			before: []intake.Fact{opened("ask-q2", 11), intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-q2", Kind: "question", State: "answered",
				Actor: alice, Mark: record.EventMark{Seq: 15}}},
			named: []string{"ask ask-q2"}, absent: []string{"Still with the person"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{Ask: "ask-q1", Since: handBack})
			seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)
			for i, fact := range tc.before {
				source := "dispatch"
				if _, ok := fact.(intake.GateRegistered); ok {
					source = "api"
				}
				if _, err := intake.ApplyFact(t.Context(), pool, source, "before-"+string(rune('a'+i)), fact, admission.engine, admission); err != nil {
					t.Fatalf("apply %T: %v", fact, err)
				}
			}
			applyFact(t, pool, admission, "answer-q1", answerQ1)
			for i, fact := range tc.after {
				applyFact(t, pool, admission, "after-"+string(rune('a'+i)), fact)
			}
			task := resumedTask(t, pool, admission)
			for _, want := range tc.named {
				if !strings.Contains(task, want) {
					t.Fatalf("resumed architect's task = %q, want it to name %q", task, want)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(task, unwanted) {
					t.Fatalf("resumed architect's task = %q, want it not to name %q", task, unwanted)
				}
			}
		})
	}
}

// A guard that keeps an admitted root's slot when its wait would start — a member of its tree
// mid-phase, or a Dispatch claim a person holds — holds that wait pending, and it starts as soon as
// the guard clears: the member leaves the workflow, or the person releases the claim. The root
// returns its slot then, as it would have at the wait's start.
func TestAWaitAGuardHeldStartsWhenTheGuardClears(t *testing.T) {
	handBackFact := intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: handBack}
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, pool *pgxpool.Pool)
		clear func(t *testing.T, pool *pgxpool.Pool, admission *Admission)
	}{
		{name: "a member of the tree ran a phase",
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				parent := "LEGION-ROOT"
				putIssue(t, pool, record.Issue{Key: "LEGION-CHILD", Tree: "LEGION-ROOT", Parent: &parent, Project: testProject, Title: "child",
					Phase: phase.Planning, Generation: 1, Status: "in_progress", Rank: "C", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"})
			},
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				apply(t, pool, admission, "child-parked", intake.DispatchIssue{Key: "LEGION-CHILD", ResolvedRepo: "acme/widgets", Seq: 30,
					Type: "issue.updated", Status: "backlog", Title: "child", Rank: "C", Parent: "LEGION-ROOT"}, admission.engine)
			}},
		{name: "a person held the root's claim",
			seed: func(t *testing.T, pool *pgxpool.Pool) {
				root := issue(t, pool, "LEGION-ROOT")
				root.ClaimedBy, root.ClaimSeq = "user:bob", 3
				putIssue(t, pool, root)
			},
			clear: func(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
				applyFact(t, pool, admission, "claim-released", intake.DispatchClaim{Key: "LEGION-ROOT", Seq: 30, ClaimedBy: ""})
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4})
			tc.seed(t, pool)

			applyFact(t, pool, admission, "hand-back", handBackFact)
			if kept := issue(t, pool, "LEGION-ROOT"); kept.Phase != phase.Admitted {
				t.Fatalf("root after a hand-back the guard held = %#v, want it admitted with its slot", kept)
			}

			tc.clear(t, pool, admission)
			got := issue(t, pool, "LEGION-ROOT")
			if !got.AwaitingPerson() || got.Awaiting.GateVersion != 4 || got.Awaiting.Since != handBack {
				t.Fatalf("root once the guard cleared = %#v, want it waiting on spec version 4 from the hand-back", got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
		})
	}
}
