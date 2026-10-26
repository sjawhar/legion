package admit

import (
	"context"
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
