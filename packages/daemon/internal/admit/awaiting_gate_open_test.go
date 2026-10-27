package admit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// childStarts counts what LEGION-209's first phase queued: its planner's starts, its branch rows
// and the child-status notices its architect was told.
func childStarts(t *testing.T, pool *pgxpool.Pool) (starts, branches, notices int) {
	t.Helper()
	for _, e := range effects(t, pool) {
		if e.issue != "LEGION-209" {
			continue
		}
		switch payload := e.payload.(type) {
		case record.SuperviseRequest:
			if payload.Op == "start" && payload.Role == claim.RolePlanner {
				starts++
			}
		case record.IssueBranch:
			branches++
		case record.Notice:
			if payload.Kind == "child-status" {
				notices++
			}
		}
	}
	return starts, branches, notices
}

// seedReleasedChild records LEGION-209, a child of LEGION-208 released under its closed gate and
// held at admitted.
func seedReleasedChild(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	putIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: "child", Tree: "LEGION-208", Parent: new("LEGION-208"), Phase: phase.Admitted,
		Generation: 1, Status: "todo", Rank: "W", HandedOver: true})
}

// freeTheSlot finishes LEGION-300, the root that took the slot LEGION-208 returned.
func freeTheSlot(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	done := issue(t, pool, "LEGION-300")
	done.Phase = phase.Done
	putIssue(t, pool, done)
}

// The resume of a root whose wait a person's approval answered starts each child released under
// its gate once: the resume advances the tree, and the pass's start of unblocked children reads the
// child as the resume left it. (Round 1, standard P2 and adversarial P2-2.)
func TestAResumeStartsEachReleasedChildOnce(t *testing.T) {
	pool := migratedPool(t)
	admission := awaitingRig(t, pool)
	seedReleasedChild(t, pool)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3})
	tick(t, pool, admission, "tick-1")
	if got := issue(t, pool, "LEGION-208").Phase; got != phase.AwaitingPerson {
		t.Fatalf("phase = %s, want the root waiting", got)
	}
	freeTheSlot(t, pool)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3, ApprovedVersion: new(3)})
	answerWait(t, pool, "LEGION-208", record.AnsweredByApproval, "version 3")
	tick(t, pool, admission, "tick-2")
	if got := issue(t, pool, "LEGION-209").Phase; got != phase.Planning {
		t.Fatalf("child phase = %s, want planning after its root's resume", got)
	}
	if starts, branches, notices := childStarts(t, pool); starts != 1 || branches != 1 || notices != 0 {
		t.Fatalf("planner starts = %d, branch rows = %d, unblocked notices = %d; want 1, 1, 0", starts, branches, notices)
	}
}

// An open design gate answers a root waiting on its architect's question, whatever opened it and
// in whatever order: a person's approval Dispatch recorded before the hand-back but applied after
// the wait began, a registration that reads an approval, and a registration under gates.design:
// off. Each resumes the root on the next free slot and advances its tree; none leaves it suspended
// with its gate open. The resume task says how the spec stands, so no design-approved notice is
// queued beside it. (Round 1, adversarial P2-1 and P3-1, outside-whole P3-2.)
func TestAnOpenGateAnswersAQuestionWaitWhateverOpenedIt(t *testing.T) {
	approvedAt := fixedNow.Add(-time.Minute)
	for _, tc := range []struct {
		name   string
		policy config.DesignGate
		open   intake.Fact
		task   string
	}{
		{"an approval recorded before the hand-back, applied after it", config.DesignGateRootIssues,
			intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: intake.DispatchArtifactApproved, Version: 3,
				Mark: record.EventMark{Seq: 4, At: approvedAt}},
			"was approved"},
		{"a registration that reads an approval", config.DesignGateRootIssues,
			intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 3, DispatchVersion: 3, ApprovedVersion: 3,
				ApprovedMark: record.EventMark{Seq: 4, At: approvedAt}, Since: record.EventMark{Seq: 6}},
			"was approved"},
		{"a registration with the design gate off", config.DesignGateOff,
			intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 3, DispatchVersion: 3, Since: record.EventMark{Seq: 6}},
			"the design gate is off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedSlotted(t, pool, "LEGION-208", "U")
			seedWaiting(t, pool, "LEGION-300", "V")
			seedArchitectClaim(t, pool, "LEGION-208", true)
			seedHandBack(t, pool, "LEGION-208", "ask-q", 5)
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3})
			admission := newAdmissionWithGate(t, 1, tc.policy, nil)
			tick(t, pool, admission, "tick-1")
			if got := issue(t, pool, "LEGION-208").Phase; got != phase.AwaitingPerson {
				t.Fatalf("phase = %s, want the root waiting on its question", got)
			}
			freeTheSlot(t, pool)
			applyFact(t, pool, admission, "gate-opens", tc.open)

			root := issue(t, pool, "LEGION-208")
			if root.Phase != phase.Planning || root.Awaiting != nil {
				t.Fatalf("root = %s (awaiting %+v), want it resumed into planning", root.Phase, root.Awaiting)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-208", Index: 0, AdmittedAt: fixedNow}})
			var resumes []record.SuperviseRequest
			for _, request := range superviseRequests(t, pool, "LEGION-208") {
				if request.Op == "start" && request.Role == claim.RoleArchitect {
					resumes = append(resumes, request)
				}
			}
			if len(resumes) != 1 || !strings.Contains(resumes[0].Task, tc.task) {
				t.Fatalf("architect starts = %+v, want one resume naming %q", resumes, tc.task)
			}
			if told := gateNotices(t, pool, "LEGION-208", "design-approved"); len(told) != 0 {
				t.Fatalf("design-approved notices = %+v, want the resume task alone to say it", told)
			}
		})
	}
}

// waitingRoot seeds LEGION-208 waiting on its question ask-q from hand-back seq 5, its gate v3
// closed, with LEGION-300 holding the slot it returned.
func waitingRoot(t *testing.T, policy config.DesignGate) (*pgxpool.Pool, *Admission) {
	t.Helper()
	pool := migratedPool(t)
	seedSlotted(t, pool, "LEGION-208", "U")
	seedWaiting(t, pool, "LEGION-300", "V")
	seedArchitectClaim(t, pool, "LEGION-208", true)
	seedHandBack(t, pool, "LEGION-208", "ask-q", 5)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-208", ArtifactID: "artifact-208", LatestVersion: 3})
	admission := newAdmissionWithGate(t, 1, policy, nil)
	tick(t, pool, admission, "tick-1")
	if got := issue(t, pool, "LEGION-208").Phase; got != phase.AwaitingPerson {
		t.Fatalf("phase = %s, want the root waiting on its question", got)
	}
	return pool, admission
}

// architectResumes returns the tasks of every architect start queued for LEGION-208.
func architectResumes(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var tasks []string
	for _, request := range superviseRequests(t, pool, "LEGION-208") {
		if request.Op == "start" && request.Role == claim.RoleArchitect {
			tasks = append(tasks, request.Task)
		}
	}
	return tasks
}

// A registration's read of a changes request on a root already waiting on its question applies the
// review as its live event does: placed after the wait began, it answers the wait and is named by
// the resume alone; read with no sequence, it answers nothing and is neither told nor recorded, so
// its live event decides. Either way the architect hears the request once. (Round 3, standard P3-1.)
func TestARegistrationsChangesReadOnAWaitingRootIsToldOnce(t *testing.T) {
	for _, seq := range []int64{7, 0} {
		t.Run(map[int64]string{7: "sequenced read", 0: "unsequenced read"}[seq], func(t *testing.T) {
			pool, admission := waitingRoot(t, config.DesignGateRootIssues)
			at := fixedNow.Add(-30 * time.Second)
			applyFact(t, pool, admission, "register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 3, DispatchVersion: 3,
				ChangesRequestedVersion: 3, ChangesRequestedMark: record.EventMark{Seq: seq, At: at}, ChangesReason: "split it", Since: record.EventMark{Seq: 6}})
			root := issue(t, pool, "LEGION-208")
			if answered := root.Awaiting != nil && root.Awaiting.AnsweredAt != nil; answered != (seq > 0) {
				t.Fatalf("after the registration, awaiting = %+v; want it answered only by a sequenced read", root.Awaiting)
			}
			if told := gateNotices(t, pool, "LEGION-208", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("after the registration, changes notices = %+v, want none for a waiting root", told)
			}
			applyFact(t, pool, admission, "live", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208",
				Kind: intake.DispatchArtifactChangesRequested, Version: 3, Reason: "split it", Mark: record.EventMark{Seq: 7, At: at}})
			freeTheSlot(t, pool)
			tick(t, pool, admission, "tick-2")
			named := 0
			resumes := architectResumes(t, pool)
			for _, task := range resumes {
				named += strings.Count(task, "split it")
			}
			notices := gateNotices(t, pool, "LEGION-208", "design-changes-requested")
			if len(resumes) != 1 || named+len(notices) != 1 {
				t.Fatalf("resumes = %q, changes notices = %+v; want one resume and the request told once", resumes, notices)
			}
		})
	}
}

// A changes request a person makes on a waiting root after something else answered its wait is
// named by the resume, never by a notice held beside it: when a later approval superseded it, the
// resume's design-approved wake is the newest word and nothing asks for a revision; when none did,
// the resume asks for the revision. (Round 3, adversarial P3-1.)
func TestAChangesRequestOnAnAnsweredWaitIsNamedByTheResume(t *testing.T) {
	at := fixedNow.Add(-30 * time.Second)
	review := func(kind intake.DispatchArtifactKind, seq int64, reason string) intake.DispatchArtifact {
		return intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208", Kind: kind, Version: 3, Reason: reason,
			Mark: record.EventMark{Seq: seq, At: at.Add(time.Duration(seq) * time.Second)}}
	}
	for _, tc := range []struct {
		name     string
		reviews  []intake.DispatchArtifact
		contains []string
		absent   []string
	}{
		{"a later approval superseded it",
			[]intake.DispatchArtifact{review(intake.DispatchArtifactApproved, 7, ""), review(intake.DispatchArtifactChangesRequested, 8, "wait no"),
				review(intake.DispatchArtifactApproved, 9, "")},
			[]string{workflow.DesignApprovedWake}, []string{"revise"}},
		{"no approval followed it",
			[]intake.DispatchArtifact{review(intake.DispatchArtifactApproved, 7, ""), review(intake.DispatchArtifactChangesRequested, 8, "wait no")},
			[]string{"wait no", "revise the spec"}, []string{workflow.DesignApprovedWake}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, admission := waitingRoot(t, config.DesignGateRootIssues)
			for i, fact := range tc.reviews {
				applyFact(t, pool, admission, fmt.Sprintf("review-%d", i), fact)
			}
			freeTheSlot(t, pool)
			tick(t, pool, admission, "tick-2")
			if told := gateNotices(t, pool, "LEGION-208", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("changes notices = %+v, want the resume alone to name the request", told)
			}
			resumes := architectResumes(t, pool)
			if len(resumes) != 1 {
				t.Fatalf("architect starts = %q, want one resume", resumes)
			}
			for _, want := range tc.contains {
				if !strings.Contains(resumes[0], want) {
					t.Errorf("resume %q does not say %q", resumes[0], want)
				}
			}
			for _, unwanted := range tc.absent {
				if strings.Contains(resumes[0], unwanted) {
					t.Errorf("resume %q says %q", resumes[0], unwanted)
				}
			}
		})
	}
}

// Under gates.design: off, a resume never says a person approved the spec when only the policy
// opened the gate: a changes request that answered the wait still asks for the revision, and the
// open gate is said as open, not approved. (Round 3, adversarial P3-2, standard and outside-whole
// wording P3s.)
func TestTheResumeUnderGateOffSaysNoPersonApproved(t *testing.T) {
	pool, admission := waitingRoot(t, config.DesignGateOff)
	applyFact(t, pool, admission, "changes", intake.DispatchArtifact{Key: "LEGION-208", ArtifactID: "artifact-208",
		Kind: intake.DispatchArtifactChangesRequested, Version: 3, Reason: "split it", Mark: record.EventMark{Seq: 7, At: fixedNow.Add(-time.Minute)}})
	applyFact(t, pool, admission, "register", intake.GateRegistered{Issue: "LEGION-208", ArtifactID: "artifact-208", Version: 3, DispatchVersion: 3, Since: record.EventMark{Seq: 8}})
	freeTheSlot(t, pool)
	tick(t, pool, admission, "tick-2")
	resumes := architectResumes(t, pool)
	if len(resumes) != 1 {
		t.Fatalf("architect starts = %q, want one resume", resumes)
	}
	for _, want := range []string{"revise the spec", workflow.DesignApprovedWake} {
		if !strings.Contains(resumes[0], want) {
			t.Errorf("resume %q does not say %q", resumes[0], want)
		}
	}
	for _, unwanted := range []string{"approved the spec", "is approved"} {
		if strings.Contains(resumes[0], unwanted) {
			t.Errorf("resume %q says %q under gates.design: off", resumes[0], unwanted)
		}
	}
}

// A root waiting on its design gate when the operator switches gates.design to off, with nothing
// else with the person, is answered and resumed in one admission pass: it takes the free slot
// ahead of a root that never ran, rather than waiting for the next fact or tick while that root
// takes it.
func TestAGateWaitAnsweredByTheGateSwitchedOffResumesInThatPass(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmissionWithGate(t, 1, config.DesignGateOff, nil)
	putIssue(t, pool, record.Issue{
		Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "Z", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{GateVersion: 3, Artifact: "artifact-root", Since: record.EventMark{Seq: 5}},
	})
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", ArtifactID: "artifact-root", LatestVersion: 3})
	seedWaiting(t, pool, "LEGION-NEXT", "A")
	tick(t, pool, admission, "tick-1")
	if got := issue(t, pool, "LEGION-100"); got.Phase != phase.Admitted || got.Awaiting != nil {
		t.Fatalf("root after one pass = %s (awaiting %+v), want it answered and resumed at admitted, its gate closed", got.Phase, got.Awaiting)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
	var resumes []string
	for _, request := range superviseRequests(t, pool, "LEGION-100") {
		if request.Op == "start" && request.Role == claim.RoleArchitect {
			resumes = append(resumes, request.Task)
		}
	}
	if len(resumes) != 1 || !strings.Contains(resumes[0], "closed at version 3") {
		t.Fatalf("architect starts = %q, want one resume saying the gate is closed at version 3", resumes)
	}
}

// An answered root is no root blocked on Dispatch links: its tree already runs (a root in
// awaiting_person carries its run, record.Issue.CarriesRun), so promotion resumes it whatever its
// links say, and the state's blocked list and the free-slot count agree with promotion. Round 1's
// standard P3-2 read this as missing; the run it carries already exempts it, and this pins that.
func TestAnAnsweredRootIsNotReportedAsBlocked(t *testing.T) {
	pool := migratedPool(t)
	admission := awaitingRig(t, pool)
	tick(t, pool, admission, "tick-1")
	answerWait(t, pool, "LEGION-208", record.AnsweredByAsk, "ask-q")
	// A link added while it waited, on an issue Dispatch shows unfinished; LEGION-300 holds the one
	// slot, so the answered root waits in line.
	seedMirror(t, pool, "LEGION-208", "in_progress", "LEGION-777")
	seedMirror(t, pool, "LEGION-777", "in_progress")
	inTx(t, pool, func(tx pgx.Tx) {
		blocked, err := admission.blocked(context.Background(), tx)
		if err != nil {
			t.Fatalf("read what is blocked: %v", err)
		}
		if len(blocked) != 0 {
			t.Fatalf("blocked = %+v, want the answered root left out", blocked)
		}
	})
}
