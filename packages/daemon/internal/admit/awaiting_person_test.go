package admit

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A registration of a closed gate whose approval request waits on the person is the architect
// handing the spec to the person: the root, its architect running the turn it took, gives its slot
// to the next root, its architect suspended after that turn and the controller told.
func TestAClosedRegisteredGateReleasesItsRootSlotForAnotherWaitingRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectClaim(t, pool, "LEGION-100", "session-architect")

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

// A person who approved the spec before the architect registered it has the gate open at
// registration: the root starts planning in its own slot, and nothing suspends or releases it.
func TestARegistrationDispatchAlreadyApprovedKeepsTheRootsSlotAndStartsPlanning(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")

	if _, err := intake.ApplyFact(context.Background(), pool, "api", "registered-approved-gate",
		intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root", Version: 3, ApprovedVersion: 3}, admission.engine, admission); err != nil {
		t.Fatalf("register approved gate: %v", err)
	}

	root := issue(t, pool, "LEGION-100")
	if root.Phase != phase.Planning || root.Awaiting != nil {
		t.Fatalf("root after approved registration = %#v, want planning with no awaiting state", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
	assertNoAwaitingEffects(t, pool, "LEGION-100")
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

func TestAnArchitectQuestionReleasesItsRootSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectClaim(t, pool, "LEGION-100", "session-architect")

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "architect-question",
		intake.DispatchAskOpened{Key: "LEGION-100", Ask: "ask-root", Kind: "question", AuthorSession: "session-architect", Mark: record.EventMark{Seq: 5}},
		admission.engine, admission); err != nil {
		t.Fatalf("open architect question: %v", err)
	}
	root := issue(t, pool, "LEGION-100")
	if root.Phase != phase.AwaitingPerson || root.Awaiting == nil || root.Awaiting.Ask != "ask-root" {
		t.Fatalf("root after question = %#v, want awaiting_person on ask-root", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
}

// The architect whose claim a person refused asks that person, on the issue, whether to release
// it, and waits (prompts/go/architect-root.md). That root could never be resumed while the person
// holds the claim, so it keeps its slot at admitted rather than returning it, which the sweep
// would then end.
func TestAnArchitectQuestionWhileAPersonHoldsTheClaimKeepsTheSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectClaim(t, pool, "LEGION-100", "session-architect")
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "person-claims",
		intake.DispatchClaim{Key: "LEGION-100", Seq: 2, ClaimedBy: "user:alice"}, admission.engine, admission); err != nil {
		t.Fatalf("apply the person's claim: %v", err)
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "architect-asks-the-holder",
		intake.DispatchAskOpened{Key: "LEGION-100", Ask: "ask-release", Kind: "question", AuthorSession: "session-architect", Mark: record.EventMark{Seq: 7}},
		admission.engine, admission); err != nil {
		t.Fatalf("open the architect's question: %v", err)
	}
	root := issue(t, pool, "LEGION-100")
	if root.Phase != phase.Admitted || root.Awaiting != nil || root.Lingers() {
		t.Fatalf("root after asking the claim holder = %#v, want it still admitted, not waiting and not lingering", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
	assertNoAwaitingEffects(t, pool, "LEGION-100")
}

// A person who claimed the issue before it was handed to Legion: Dispatch's issue.claimed event
// found no record to apply to, and the hand-over event that records the root carries that claim.
// The root takes the holder with it, so the architect's question to that person keeps the slot on
// the live path exactly as a boot's claim read would.
func TestAClaimAPersonTookBeforeHandoverFencesTheRootsRelease(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	apply(t, pool, admission, "handed-over-claimed", intake.DispatchIssue{
		Key: "LEGION-100", ResolvedRepo: "acme/widgets", Seq: 6, Type: "issue.updated", Status: "todo",
		Title: "root", Rank: "A", HandedOver: handed, ClaimedBy: "user:alice",
	}, admission.engine)
	root := issue(t, pool, "LEGION-100")
	if root.ClaimedBy != "user:alice" || root.ClaimSeq != 6 {
		t.Fatalf("recorded root = %#v, want the person's claim at sequence 6", root)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
	seedArchitectClaim(t, pool, "LEGION-100", "session-architect")

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "architect-asks-the-holder",
		intake.DispatchAskOpened{Key: "LEGION-100", Ask: "ask-release", Kind: "question", AuthorSession: "session-architect", Mark: record.EventMark{Seq: 7}},
		admission.engine, admission); err != nil {
		t.Fatalf("open the architect's question: %v", err)
	}
	if got := issue(t, pool, "LEGION-100"); got.Phase != phase.Admitted || got.Awaiting != nil || got.Lingers() {
		t.Fatalf("root after asking the claim holder = %#v, want it still admitted in its slot", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
}

// A person who claims a root already recorded, seen on a later issue event rather than an
// issue.claimed one: the observation records the holder at its sequence, so the architect's
// question to that person keeps the slot. An older observation never takes the claim back.
func TestAClaimOnALaterIssueEventFencesTheRootsRelease(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	putIssue(t, pool, record.Issue{
		Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.Admitted,
		Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", HandedOver: true, LastDispatchSeq: 4,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
	})
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutSlot(context.Background(), tx, record.Slot{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("seed slot: %v", err)
		}
	})
	seedArchitectClaim(t, pool, "LEGION-100", "session-architect")

	apply(t, pool, admission, "claimed-and-renamed", intake.DispatchIssue{
		Key: "LEGION-100", ResolvedRepo: "acme/widgets", Seq: 6, Type: "issue.updated", Status: "in_progress",
		Title: "root, renamed", Rank: "A", HandedOver: handed, ClaimedBy: "user:alice",
	}, admission.engine)
	if got := issue(t, pool, "LEGION-100"); got.ClaimedBy != "user:alice" || got.ClaimSeq != 6 {
		t.Fatalf("root after a claimed issue event = %#v, want user:alice at sequence 6", got)
	}
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "older-unclaimed",
		intake.DispatchClaim{Key: "LEGION-100", Seq: 5}, admission.engine, admission); err != nil {
		t.Fatalf("apply an older release: %v", err)
	}
	if got := issue(t, pool, "LEGION-100"); got.ClaimedBy != "user:alice" {
		t.Fatalf("root after an older release = %#v, want user:alice still", got)
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "architect-asks-the-holder",
		intake.DispatchAskOpened{Key: "LEGION-100", Ask: "ask-release", Kind: "question", AuthorSession: "session-architect", Mark: record.EventMark{Seq: 7}},
		admission.engine, admission); err != nil {
		t.Fatalf("open the architect's question: %v", err)
	}
	if got := issue(t, pool, "LEGION-100"); got.Phase != phase.Admitted || got.Awaiting != nil || got.Lingers() {
		t.Fatalf("root after asking the claim holder = %#v, want it still admitted in its slot", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
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
				Key: "LEGION-100", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.updated", Status: "in_progress",
				Title: "root", Rank: "A",
			},
			reason: "label",
		},
		{
			name:   "is claimed by a person",
			fact:   intake.DispatchClaim{Key: "LEGION-100", Seq: 1, ClaimedBy: "user:alice"},
			reason: "claim",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			root := record.Issue{
				Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
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

// A session's reply that leaves the turn with the person answers nothing; a person's reply, which
// hands the turn to the agent, answers the root.
func TestOnlyAnAskReplyThatHandsTheTurnToTheAgentWakesTheRoot(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-RUN", "A")
	root := record.Issue{
		Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &record.Awaiting{Ask: "ask-root"},
	}
	putIssue(t, pool, root)

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "reply-waits-human",
		intake.DispatchAskReply{Key: root.Key, Ask: "ask-root", WaitingOn: "human", Actor: architect}, admission.engine, admission); err != nil {
		t.Fatalf("apply human-turn reply: %v", err)
	}
	if got := issue(t, pool, root.Key); got.Answered() {
		t.Fatalf("root after human-turn reply = %#v, want it still unanswered", got)
	}

	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "reply-waits-agent",
		intake.DispatchAskReply{Key: root.Key, Ask: "ask-root", WaitingOn: "agent", Actor: alice}, admission.engine, admission); err != nil {
		t.Fatalf("apply agent-turn reply: %v", err)
	}
	if got := issue(t, pool, root.Key); !got.Answered() {
		t.Fatalf("root after agent-turn reply = %#v, want it answered", got)
	}
}

func TestAClosedGateDoesNotReleaseASlotWhileATreeMemberIsMidPhase(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-100", "A")
	seedArchitectClaim(t, pool, "LEGION-100", "session-architect")
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
	seedArchitectClaim(t, pool, "LEGION-100", "session-architect")
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

// Only the root architect's own ordinary question returns the slot. An approval ask belongs to the
// design gate, which register_gate handles, and a question a session outside the tree opened on
// the root does not stop a tree that session has no part in.
func TestOnlyTheTreesOwnQuestionReleasesTheSlot(t *testing.T) {
	for _, tc := range []struct {
		name string
		ask  intake.DispatchAskOpened
	}{
		{name: "an approval ask", ask: intake.DispatchAskOpened{Key: "LEGION-100", Ask: "ask-approval", Kind: "approval", AuthorSession: "session-architect",
			Mark: record.EventMark{Seq: 5}}},
		{name: "a question from outside the tree", ask: intake.DispatchAskOpened{Key: "LEGION-100", Ask: "ask-foreign", Kind: "question", AuthorSession: "session-elsewhere",
			Mark: record.EventMark{Seq: 5}}},
		{name: "a question a person opened", ask: intake.DispatchAskOpened{Key: "LEGION-100", Ask: "ask-person", Kind: "question", Mark: record.EventMark{Seq: 5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-100", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedArchitectClaim(t, pool, "LEGION-100", "session-architect")

			if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", tc.name, tc.ask, admission.engine, admission); err != nil {
				t.Fatalf("open %s: %v", tc.name, err)
			}
			if got := issue(t, pool, "LEGION-100"); got.Phase != phase.Admitted || got.Awaiting != nil {
				t.Fatalf("root after %s = %#v, want it still admitted", tc.name, got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-100", Index: 0, AdmittedAt: fixedNow}})
			assertNoAwaitingEffects(t, pool, "LEGION-100")
		})
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

// A person who moves a waiting root out of the workflow ends its tree through the engine's leave,
// as for any root: it lingers with its awaiting state cleared, and nothing resumes it.
func TestAnAwaitingRootMovedOutOfTheWorkflowLeavesThroughTheEngine(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	putIssue(t, pool, record.Issue{
		Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "A", HandedOver: true,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Awaiting: &record.Awaiting{Ask: "ask-root"},
	})

	apply(t, pool, admission, "moved-to-backlog", intake.DispatchIssue{
		Key: "LEGION-100", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "backlog",
		Title: "root", Rank: "A", HandedOver: handed,
	}, admission.engine)
	got := issue(t, pool, "LEGION-100")
	if got.Phase != phase.Done || got.Awaiting != nil || got.LingerUntil == nil || got.Status != "backlog" {
		t.Fatalf("root moved to backlog = %#v, want done, lingering, its awaiting state cleared, at backlog", got)
	}
	for _, effect := range effects(t, pool) {
		if notice, ok := effect.payload.(record.ControllerNotice); ok && notice.Kind == record.UnslottedNotice {
			t.Fatalf("outbox effects = %#v, want the engine's leave, not admission's unslotted sweep", effects(t, pool))
		}
	}
}

// A person who sets a waiting root back to todo has Dispatch showing todo while the record waits.
// Once answered and promoted, it is a running tree again, so its status is written in_progress,
// over the todo Dispatch shows, as a fresh root's promotion writes it.
func TestAnAnsweredRootAPersonSetToTodoIsWrittenInProgressAtItsResume(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-RUN", "A")
	putIssue(t, pool, record.Issue{
		Key: "LEGION-100", Tree: "LEGION-100", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", Rank: "B", HandedOver: true, LastDispatchSeq: 1,
		Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Awaiting: &record.Awaiting{Ask: "ask-root"},
	})

	apply(t, pool, admission, "person-sets-todo", intake.DispatchIssue{
		Key: "LEGION-100", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "todo",
		Title: "root", Rank: "B", HandedOver: handed,
	}, admission.engine)
	if got := issue(t, pool, "LEGION-100"); !got.AwaitingPerson() || got.Status != "todo" {
		t.Fatalf("root after a person set todo = %#v, want it still waiting, at todo", got)
	}
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", "answered",
		intake.DispatchAskClosed{Key: "LEGION-100", Ask: "ask-root", Kind: "question", State: "answered", Actor: alice}, admission.engine, admission); err != nil {
		t.Fatalf("answer the question: %v", err)
	}
	apply(t, pool, admission, "run-finished", intake.DispatchIssue{
		Key: "LEGION-RUN", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done",
		Title: "LEGION-RUN", Rank: "A", HandedOver: true,
	}, admission.engine)

	resumed := issue(t, pool, "LEGION-100")
	if resumed.Phase != phase.Admitted || resumed.Status != "in_progress" {
		t.Fatalf("resumed root = %#v, want admitted at in_progress", resumed)
	}
	var writes []record.StatusWrite
	for _, effect := range effects(t, pool) {
		if write, ok := effect.payload.(record.StatusWrite); ok && effect.issue == "LEGION-100" {
			writes = append(writes, write)
		}
	}
	if len(writes) != 1 || writes[0] != (record.StatusWrite{Status: "in_progress", ObservedStatus: "todo"}) {
		t.Fatalf("status writes for the resumed root = %#v, want one in_progress over todo", writes)
	}
}
