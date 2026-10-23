package admit

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

func TestAClosedRegisteredGateReleasesItsRootSlotForAnotherWaitingRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")

	if _, err := intake.ApplyFact(context.Background(), pool, "api", "registered-closed-gate",
		intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 3}, admission.engine, admission); err != nil {
		t.Fatalf("register closed gate: %v", err)
	}

	root := issue(t, pool, "LEGION-ROOT")
	if root.Phase != phase.AwaitingPerson || root.Awaiting == nil || root.Awaiting.GateVersion != 3 || root.Awaiting.AnsweredAt != nil {
		t.Fatalf("root after closed gate = %#v, want unanswered awaiting_person on gate version 3", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
	var suspended, noticed bool
	for _, effect := range effects(t, pool) {
		switch payload := effect.payload.(type) {
		case record.SuperviseRequest:
			suspended = suspended || effect.issue == "LEGION-ROOT" && payload.Op == "suspend" && payload.Role == "architect" && strings.Contains(payload.Reason, "spec version 3")
		case record.ControllerNotice:
			noticed = noticed || effect.issue == "LEGION-ROOT" && payload.Kind == record.AwaitingPersonNotice && strings.Contains(payload.Reason, "slot is released")
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
		Key: "LEGION-ROOT", Tree: "LEGION-ROOT", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "Z", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{GateVersion: 3},
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

func TestAnArchitectQuestionReleasesItsRootSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
			state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ('legion-root-architect', 'LEGION', 'LEGION-ROOT', 'LEGION-ROOT', 'architect', 1, 'session-architect', '', 'working', 0, 0, 0, 0)`); err != nil {
			t.Fatalf("seed architect claim: %v", err)
		}
	})

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "architect-question",
		intake.DispatchAskOpened{Key: "LEGION-ROOT", Ask: "ask-root", Kind: "question", AuthorSession: "session-architect"}, admission.engine, admission); err != nil {
		t.Fatalf("open architect question: %v", err)
	}
	root := issue(t, pool, "LEGION-ROOT")
	if root.Phase != phase.AwaitingPerson || root.Awaiting == nil || root.Awaiting.Ask != "ask-root" {
		t.Fatalf("root after question = %#v, want awaiting_person on ask-root", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
}

func TestAnAwaitingRootThatLosesItsHandoverOrClaimLeavesTheWorkflow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fact   intake.Fact
		reason string
	}{
		{
			name: "loses the legion label",
			fact: intake.DispatchIssue{
				Key: "LEGION-ROOT", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "in_progress",
				Title: "root", Rank: "A",
			},
			reason: "label",
		},
		{
			name:   "is claimed by a person",
			fact:   intake.DispatchClaim{Key: "LEGION-ROOT", Seq: 1, ClaimedBy: "user:alice"},
			reason: "claim",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			root := record.Issue{
				Key: "LEGION-ROOT", Tree: "LEGION-ROOT", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
				Generation: 1, Status: "in_progress", Rank: "A", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Awaiting: &record.Awaiting{Ask: "ask-root"},
			}
			putIssue(t, pool, root)

			if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", tc.name, tc.fact, admission.engine, admission); err != nil {
				t.Fatalf("apply unsafe awaiting state: %v", err)
			}
			got := issue(t, pool, root.Key)
			if got.Phase != phase.Done || got.Awaiting != nil || got.LingerUntil == nil {
				t.Fatalf("awaiting root after %s = %#v, want done, cleared awaiting state, and lingering", tc.name, got)
			}
			notices := controllerNotices(t, pool)
			if len(notices) != 1 || notices[0].payload.(record.ControllerNotice).Kind != record.UnslottedNotice || !strings.Contains(notices[0].payload.(record.ControllerNotice).Reason, tc.reason) {
				t.Fatalf("controller notices = %#v, want one unslotted notice naming %s", notices, tc.reason)
			}
		})
	}
}

func TestANewerSpecVersionWakesARootAwaitingAGate(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-RUN", "A")
	root := record.Issue{
		Key: "LEGION-ROOT", Tree: "LEGION-ROOT", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{GateVersion: 3},
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

func TestOnlyAnAskReplyThatHandsTheTurnToTheAgentWakesTheRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-RUN", "A")
	root := record.Issue{
		Key: "LEGION-ROOT", Tree: "LEGION-ROOT", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{Ask: "ask-root"},
	}
	putIssue(t, pool, root)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "reply-waits-human",
		intake.DispatchAskReply{Key: root.Key, Ask: "ask-root", WaitingOn: "human"}, admission.engine, admission); err != nil {
		t.Fatalf("apply human-turn reply: %v", err)
	}
	if got := issue(t, pool, root.Key); got.Answered() {
		t.Fatalf("root after human-turn reply = %#v, want it still unanswered", got)
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "reply-waits-agent",
		intake.DispatchAskReply{Key: root.Key, Ask: "ask-root", WaitingOn: "agent"}, admission.engine, admission); err != nil {
		t.Fatalf("apply agent-turn reply: %v", err)
	}
	if got := issue(t, pool, root.Key); !got.Answered() {
		t.Fatalf("root after agent-turn reply = %#v, want it answered", got)
	}
}

func TestAClosedGateDoesNotReleaseASlotWhileATreeMemberIsMidPhase(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	parent := "LEGION-ROOT"
	putIssue(t, pool, record.Issue{
		Key: "LEGION-CHILD", Tree: "LEGION-ROOT", Parent: &parent, Project: testProject, Title: "child", Phase: phase.Implementing,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
	})

	if _, err := intake.ApplyFact(context.Background(), pool, "api", "closed-gate-with-child",
		intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 3}, admission.engine, admission); err != nil {
		t.Fatalf("register closed gate: %v", err)
	}
	root := issue(t, pool, "LEGION-ROOT")
	if root.Phase != phase.Admitted || root.Awaiting != nil {
		t.Fatalf("root with active child = %#v, want its slot retained at admitted", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: root.Key, Index: 0, AdmittedAt: fixedNow}})
}

// A changes request resumes the root; its architect revises the spec, asks again, and registers the
// new version. That registration, closed like the first, starts a new wait and returns the slot again.
func TestAResumedRootThatRegistersANewClosedVersionReleasesItsSlotAgain(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	register := func(id string, version int) {
		t.Helper()
		if _, err := intake.ApplyFact(context.Background(), pool, "api", id,
			intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: version}, admission.engine, admission); err != nil {
			t.Fatalf("register gate version %d: %v", version, err)
		}
	}

	register("register-v3", 3)
	if got := issue(t, pool, "LEGION-ROOT"); got.Phase != phase.AwaitingPerson || got.Awaiting.GateVersion != 3 {
		t.Fatalf("root after closed v3 = %#v, want awaiting_person on version 3", got)
	}
	assertSlots(t, pool, nil)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "changes-v3",
		intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 3, Reason: "name the rollback path"},
		admission.engine, admission); err != nil {
		t.Fatalf("request changes on v3: %v", err)
	}
	resumed := issue(t, pool, "LEGION-ROOT")
	if resumed.Phase != phase.Admitted || resumed.Awaiting != nil {
		t.Fatalf("root after changes requested = %#v, want it resumed at admitted", resumed)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
	var told string
	for _, effect := range effects(t, pool) {
		if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == "LEGION-ROOT" && start.Op == "start" && start.Role == "architect" {
			told = start.Task
		}
	}
	if !strings.Contains(told, "requested changes") || !strings.Contains(told, "register_gate") {
		t.Fatalf("resumed architect's task = %q, want it told of the changes and to register the revised version", told)
	}

	register("register-v4", 4)
	again := issue(t, pool, "LEGION-ROOT")
	if again.Phase != phase.AwaitingPerson || again.Awaiting == nil || again.Awaiting.GateVersion != 4 || again.Awaiting.AnsweredAt != nil {
		t.Fatalf("root after closed v4 = %#v, want a new unanswered wait on version 4", again)
	}
	assertSlots(t, pool, nil)
	var suspends int
	for _, effect := range effects(t, pool) {
		if request, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == "LEGION-ROOT" && request.Op == "suspend" && request.Role == "architect" {
			suspends++
		}
	}
	if suspends != 2 {
		t.Fatalf("architect suspends = %d, want one per wait", suspends)
	}
}
