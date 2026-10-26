package admit

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

var (
	alice     = dispatch.Actor{Kind: "user", ID: "alice"}
	architect = dispatch.Actor{Kind: "session", ID: "session-architect"}
	outsider  = dispatch.Actor{Kind: "session", ID: "session-elsewhere"}
	settler   = dispatch.Actor{Kind: "system", ID: "document-settlement"}
)

// awaitingRoot records LEGION-ROOT waiting on waiting, with no slot, beside LEGION-RUN holding the
// one slot, so an answer puts LEGION-ROOT at the front of the waiting line without resuming it.
func awaitingRoot(t *testing.T, pool *pgxpool.Pool, waiting record.Awaiting) {
	t.Helper()
	seedSlotted(t, pool, "LEGION-RUN", "A")
	putIssue(t, pool, record.Issue{
		Key: "LEGION-ROOT", Tree: "LEGION-ROOT", Project: testProject, Title: "root", Phase: phase.AwaitingPerson,
		Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Awaiting: &waiting,
	})
	if waiting.GateVersion != 0 {
		seedGate(t, pool, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: waiting.GateVersion})
	}
}

func seedGate(t *testing.T, pool *pgxpool.Pool, gate record.DesignGate) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, gate); err != nil {
			t.Fatalf("seed gate: %v", err)
		}
	})
}

func applyFact(t *testing.T, pool *pgxpool.Pool, admission *Admission, id string, fact intake.Fact) {
	t.Helper()
	if _, err := intake.ApplyFact(context.Background(), pool, "dispatch", id, fact, admission.engine, admission); err != nil {
		t.Fatalf("apply %s: %v", id, err)
	}
}

func gateNotices(t *testing.T, pool *pgxpool.Pool, key string, kind record.NoticeKind) []record.Notice {
	t.Helper()
	var got []record.Notice
	for _, effect := range effects(t, pool) {
		if notice, ok := effect.payload.(record.Notice); ok && effect.issue == key && notice.Kind == kind {
			got = append(got, notice)
		}
	}
	return got
}

// A person who asks for changes between the architect's `dispatch request-approval` and its
// `register_gate` has answered the request before the root could wait on it: register_gate's read
// carries the changes request, so the root keeps its slot at admitted and its architect is told at
// once, whether a gate was registered before or this is the first registration.
func TestAChangesRequestBeforeRegistrationKeepsTheSlotAndTellsTheArchitect(t *testing.T) {
	for _, tc := range []struct {
		name  string
		prior *record.DesignGate
	}{
		{name: "a gate registered at an earlier version", prior: &record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 3}},
		{name: "the first registration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			if tc.prior != nil {
				seedGate(t, pool, *tc.prior)
			}
			applyFact(t, pool, admission, "spec-v4", intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactVersion, Version: 4})
			applyFact(t, pool, admission, "changes-v4", intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root",
				Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path"})

			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
				Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, ChangesRequestedVersion: 4, ChangesReason: "name the rollback path",
			}, admission.engine, admission); err != nil {
				t.Fatalf("register gate v4: %v", err)
			}

			root := issue(t, pool, "LEGION-ROOT")
			if root.Phase != phase.Admitted || root.Awaiting != nil {
				t.Fatalf("root after registering a version a person already asked changes of = %#v, want admitted with no wait", root)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
			assertNoAwaitingEffects(t, pool, "LEGION-ROOT")
			told := gateNotices(t, pool, "LEGION-ROOT", "design-changes-requested")
			if len(told) == 0 || told[len(told)-1].Version != 4 || told[len(told)-1].Reason != "name the rollback path" {
				t.Fatalf("design-changes-requested notices = %#v, want the last at version 4 with the reason", told)
			}
		})
	}
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
			fact:     intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, ApprovedVersion: 4},
			live:     intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 4},
			resumeAt: phase.Planning,
		},
		{
			name: "a person requested changes before the registration read Dispatch",
			fact: intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4,
				ChangesRequestedVersion: 4, ChangesReason: "name the rollback path"},
			live: intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root",
				Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path"},
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
			answered := issue(t, pool, "LEGION-ROOT")
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
			resumed := issue(t, pool, "LEGION-ROOT")
			if resumed.Phase != tc.resumeAt || resumed.Awaiting != nil {
				t.Fatalf("root once a slot freed = %#v, want %s with no wait", resumed, tc.resumeAt)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
			if told := gateNotices(t, pool, "LEGION-ROOT", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("design-changes-requested notices = %#v, want none: the resume task names the answer", told)
			}
		})
	}
}

// handBackOnAClosedGate leaves LEGION-ROOT waiting on its gate at version 4, its slot taken by
// LEGION-NEXT, after its own architect handed the approval request back to the person.
func handBackOnAClosedGate(t *testing.T, pool *pgxpool.Pool, admission *Admission) {
	t.Helper()
	handBackOnAClosedGateAt(t, pool, admission, record.EventMark{})
}

// handBackOnAClosedGateAt is handBackOnAClosedGate with the hand-back's event at mark, where the
// wait begins.
func handBackOnAClosedGateAt(t *testing.T, pool *pgxpool.Pool, admission *Admission, mark record.EventMark) {
	t.Helper()
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4})
	applyFact(t, pool, admission, "hand-back", intake.DispatchAskReply{
		Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: mark})
	if waiting := issue(t, pool, "LEGION-ROOT"); waiting.Phase != phase.AwaitingPerson || waiting.Answered() || waiting.Awaiting.Since != mark {
		t.Fatalf("root after its architect handed the request back = %#v, want it waiting unanswered from the hand-back", waiting)
	}
}

// A hand-back whose event is delivered after register_gate's read already covered it starts no
// second wait. The architect hands the approval request back at seq 10; register_gate reads the
// issue at seq 10 and starts the wait on version 4 before that event arrives; the gate seed's read
// answers the wait (a version 5, or a changes request after the hand-back), and admission resumes
// the root in a freed slot. The seq-10 hand-back, delivered now, is the act the registration
// covered: a wait started from it would wait on an act the person already answered, and Dispatch,
// waiting on the agent, would never answer it. A late question at or below the fence starts none
// either, while a new hand-back past it starts the next wait.
func TestALateHandBackARegistrationCoveredStartsNoSecondWait(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answer  intake.DispatchArtifact
		version int
	}{
		{name: "a newer version answered the wait", version: 5,
			answer: intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactVersion, Version: 5}},
		{name: "a changes request answered the wait", version: 4,
			answer: intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
				Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 11, At: handedBackAt.Add(time.Minute)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)
			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
				Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4, Since: handBack,
			}, admission.engine, admission); err != nil {
				t.Fatalf("register gate v4: %v", err)
			}
			if waiting := issue(t, pool, "LEGION-ROOT"); waiting.Phase != phase.AwaitingPerson || waiting.WaitFenceSeq != handBack.Seq {
				t.Fatalf("root after the registration = %#v, want awaiting_person with wait fence %d", waiting, handBack.Seq)
			}
			applyFact(t, pool, admission, "gate-seed", tc.answer)
			apply(t, pool, admission, "next-finished", intake.DispatchIssue{
				Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done",
				Title: "LEGION-NEXT", Rank: "B", HandedOver: true,
			}, admission.engine)
			if resumed := issue(t, pool, "LEGION-ROOT"); resumed.Phase != phase.Admitted || resumed.Awaiting != nil || resumed.WaitFenceSeq != handBack.Seq {
				t.Fatalf("root once a slot freed = %#v, want admitted with no wait and its wait fence kept", resumed)
			}

			applyFact(t, pool, admission, "late-hand-back", intake.DispatchAskReply{
				Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: handBack})
			applyFact(t, pool, admission, "late-question", intake.DispatchAskOpened{
				Key: "LEGION-ROOT", Ask: "ask-question", Kind: "question", AuthorSession: architect.ID, Mark: record.EventMark{Seq: 9}})
			if late := issue(t, pool, "LEGION-ROOT"); late.Phase != phase.Admitted || late.Awaiting != nil {
				t.Fatalf("root after a hand-back and a question the registration covered = %#v, want it still admitted with no wait", late)
			}

			next := record.EventMark{Seq: 12, At: handedBackAt.Add(2 * time.Minute)}
			applyFact(t, pool, admission, "new-hand-back", intake.DispatchAskReply{
				Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: next})
			again := issue(t, pool, "LEGION-ROOT")
			if again.Phase != phase.AwaitingPerson || again.Answered() || again.Awaiting.GateVersion != tc.version || again.Awaiting.Since != next ||
				again.WaitFenceSeq != next.Seq {
				t.Fatalf("root after a new hand-back past the fence = %#v, want waiting unanswered on version %d from seq %d", again, tc.version, next.Seq)
			}
		})
	}
}

// A registration that keeps the slot still raises the gate fence to its read. The architect hands
// the request back at seq 9; at seq 10 a session's note moves the turn to the agent; register_gate
// reads seq 10, sees the request waiting on the agent, and keeps the slot, starting no wait. The
// seq-9 hand-back, delivered now, is an act that read covered: a wait started from it would wait on
// version 4 while Dispatch waits on the agent, and nothing would answer it.
func TestARegistrationThatKeepsTheSlotStillFencesTheHandBackItCovered(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)
	read := record.EventMark{Seq: 10, At: handedBackAt.Add(time.Minute)}
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
		Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4, RequestWaitsOnAgent: true, Since: read,
	}, admission.engine, admission); err != nil {
		t.Fatalf("register gate v4: %v", err)
	}
	if kept := issue(t, pool, "LEGION-ROOT"); kept.Phase != phase.Admitted || kept.Awaiting != nil || kept.GateFenceSeq != read.Seq {
		t.Fatalf("root after a registration that kept the slot = %#v, want admitted with no wait and gate fence %d", kept, read.Seq)
	}
	applyFact(t, pool, admission, "late-hand-back", intake.DispatchAskReply{
		Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: record.EventMark{Seq: 9, At: handedBackAt}})
	if late := issue(t, pool, "LEGION-ROOT"); late.Phase != phase.Admitted || late.Awaiting != nil {
		t.Fatalf("root after a hand-back the registration's read covered = %#v, want it still admitted with no wait", late)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
}

// A registration whose read is older than the gate's newest version changes no gate state, but it
// still queues the gate seed at the gate's version: that read of Dispatch is what catches a
// review whose own event was lost.
func TestAStaleRegistrationStillQueuesItsGateSeed(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	seedGate(t, pool, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 5})
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
		Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4,
	}, admission.engine, admission); err != nil {
		t.Fatalf("register gate v4: %v", err)
	}
	var seeds []record.GateSeed
	for _, effect := range effects(t, pool) {
		if seed, ok := effect.payload.(record.GateSeed); ok && effect.issue == "LEGION-ROOT" {
			seeds = append(seeds, seed)
		}
	}
	if len(seeds) != 1 || seeds[0].Version != 5 || seeds[0].ArtifactID != "artifact-root" {
		t.Fatalf("gate seeds = %#v, want one at the gate's version 5", seeds)
	}
	var gate *record.DesignGate
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if gate, err = record.NewStore().Gate(context.Background(), tx, "LEGION-ROOT"); err != nil {
			t.Fatalf("read gate: %v", err)
		}
	})
	if gate == nil || gate.LatestVersion != 5 {
		t.Fatalf("gate after a stale registration = %#v, want it still at version 5", gate)
	}
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
		Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "agent", Actor: outsider})

	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
		Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, RequestWaitsOnAgent: true,
	}, admission.engine, admission); err != nil {
		t.Fatalf("register gate v4: %v", err)
	}
	got := issue(t, pool, "LEGION-ROOT")
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
			fact: intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 5, RequestWaitsOnAgent: true, DispatchVersion: 5}},
		{name: "only the caller names version 5",
			fact: intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 5, DispatchVersion: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			handBackOnAClosedGate(t, pool, admission)
			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v5", tc.fact, admission.engine, admission); err != nil {
				t.Fatalf("register gate v5: %v", err)
			}
			got := issue(t, pool, "LEGION-ROOT")
			if got.Answered() != tc.answered || tc.answered && got.Awaiting.GateVersion != 5 {
				t.Fatalf("root after the registration = %#v, want answered %t (at version 5 when answered)", got, tc.answered)
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
			applyFact(t, pool, admission, "late-v3-review", intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root",
				Kind: kind, Version: 3, Reason: "name the rollback path"})
			if got := issue(t, pool, "LEGION-ROOT"); got.Phase != phase.AwaitingPerson || got.Answered() {
				t.Fatalf("root after a late v3 review over a wait on v4 = %#v, want it still waiting unanswered", got)
			}
		})
	}
}

// The architect is told a changes request once, whichever of the live event, the registration's
// read of Dispatch and the gate seed's read reaches the daemon first, and whichever of the others
// follows: the gate records the review told, by version, reason and place in the issue's Dispatch
// event log, and the other reads find it there, also a read that shows the review's time and no
// sequence (a review Dispatch recorded before it kept one). Another review at the same version is a
// new act of a person and is told, with another reason or with the same one.
func TestAChangesRequestIsToldOnceWhicheverReadCarriesIt(t *testing.T) {
	reviewed := fixedNow.Add(-time.Hour)
	changes := intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root",
		Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 9, At: reviewed}}
	seeded := changes
	registration := intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4,
		ChangesRequestedVersion: 4, ChangesReason: "name the rollback path", ChangesRequestedMark: changes.Mark}
	second := changes
	second.Reason, second.Mark = "and name who runs the restore", record.EventMark{Seq: 12, At: reviewed.Add(time.Minute)}
	repeated := changes
	repeated.Mark = record.EventMark{Seq: 12, At: reviewed.Add(time.Minute)}
	timeOnly := changes
	timeOnly.Mark = record.EventMark{At: reviewed}

	for _, tc := range []struct {
		name    string
		busy    bool
		order   []intake.Fact
		reasons []string
	}{
		{name: "the live event, then the registration", order: []intake.Fact{changes, registration, seeded},
			reasons: []string{"name the rollback path"}},
		{name: "the registration, then the live event", order: []intake.Fact{registration, seeded, changes},
			reasons: []string{"name the rollback path"}},
		// The live event reached no gate, and a child mid-phase keeps the root at admitted, so no
		// resume task names the request: the seed's read is the only thing that can tell it.
		{name: "only the seed carries it, on a root a child keeps in its slot", busy: true,
			order:   []intake.Fact{intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4}, seeded},
			reasons: []string{"name the rollback path"}},
		// A registration whose read predates the review the live event told rebuilds the gate from
		// Dispatch; it keeps the told record, so the seed's read of that review tells nothing.
		{name: "the live event, then a registration whose read predates it, then the seed", busy: true,
			order:   []intake.Fact{changes, intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4}, seeded},
			reasons: []string{"name the rollback path"}},
		// A registration of another document starts with no told record, so that document's own
		// review, even with the same reason, is told.
		{name: "another document registered after the first one's review was told", busy: true,
			order: []intake.Fact{changes,
				intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-other", Version: 4},
				intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-other", Kind: intake.DispatchArtifactChangesRequested,
					Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 14, At: reviewed.Add(2 * time.Minute)}}},
			reasons: []string{"name the rollback path", "name the rollback path"}},
		// A registration whose read saw v4 commits after a live v5 changes request was told: it
		// rolls nothing back, so the seed's read of the v5 review tells nothing again.
		{name: "a registration of an older version after a newer review was told", busy: true,
			order: []intake.Fact{
				intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 5, Reason: "name the rollback path", Mark: changes.Mark},
				intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4},
				intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 5, Reason: "name the rollback path", Mark: changes.Mark}},
			reasons: []string{"name the rollback path"}},
		// The caller registers v4 while Dispatch already shows v5, whose changes request the live
		// event told first: the gate is registered at v5, keeping the told record.
		{name: "a caller's older version with Dispatch at the newer one", busy: true,
			order: []intake.Fact{
				intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 5, Reason: "name the rollback path", Mark: changes.Mark},
				intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 5},
				intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested, Version: 5, Reason: "name the rollback path", Mark: changes.Mark}},
			reasons: []string{"name the rollback path"}},
		{name: "a second review at the same version", order: []intake.Fact{changes, second},
			reasons: []string{"name the rollback path", "and name who runs the restore"}},
		// The live event carries the review's sequence; the seed reads it from a Dispatch that shows
		// its time alone. The time is the event's own, so the seed's read is the same review.
		{name: "the live event with its sequence, then the seed's read with its time alone", order: []intake.Fact{changes, timeOnly},
			reasons: []string{"name the rollback path"}},
		{name: "the seed's read with its time alone, then the live event with its sequence", order: []intake.Fact{timeOnly, changes},
			reasons: []string{"name the rollback path"}},
		// A person who repeats a review's version and wording later has written another review,
		// with its own event, and it is told once; the seed's read of that later review is the same
		// review again.
		{name: "the same reason again at the same version, a later review", order: []intake.Fact{changes, repeated, repeated},
			reasons: []string{"name the rollback path", "name the rollback path"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4})
			if tc.busy {
				parent := "LEGION-ROOT"
				putIssue(t, pool, record.Issue{
					Key: "LEGION-CHILD", Tree: "LEGION-ROOT", Parent: &parent, Project: testProject, Title: "child", Phase: phase.Implementing,
					Generation: 1, Status: "in_progress", Rank: "B", HandedOver: true, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				})
			}
			for i, fact := range tc.order {
				source := "dispatch"
				if _, ok := fact.(intake.GateRegistered); ok {
					source = "api"
				}
				if _, err := intake.ApplyFact(context.Background(), pool, source, fmt.Sprintf("fact-%d", i), fact, admission.engine, admission); err != nil {
					t.Fatalf("apply %#v: %v", fact, err)
				}
			}
			if root := issue(t, pool, "LEGION-ROOT"); root.Phase != phase.Admitted {
				t.Fatalf("root after the reads = %#v, want it still at admitted, told by a notice rather than a resume", root)
			}
			told := gateNotices(t, pool, "LEGION-ROOT", "design-changes-requested")
			var reasons []string
			for _, notice := range told {
				reasons = append(reasons, notice.Reason)
			}
			if !reflect.DeepEqual(reasons, tc.reasons) {
				t.Fatalf("design-changes-requested notices = %#v, want one per review: %#v", told, tc.reasons)
			}
		})
	}
}

// The gate seed's own read of a changes request answers a wait a registration started before the
// request reached the root, without telling the architect a second time; on a gate a racing
// registration opened, it closes the gate again and tells the architect.
func TestASeededChangesRequestAnswersAWaitAndClosesAGateARaceOpened(t *testing.T) {
	seeded := intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
		Version: 4, Reason: "name the rollback path"}

	t.Run("a root waiting on the gate", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		awaitingRoot(t, pool, record.Awaiting{GateVersion: 4})
		applyFact(t, pool, admission, "seed-changes-v4", seeded)
		if got := issue(t, pool, "LEGION-ROOT"); !got.Answered() {
			t.Fatalf("root after the seed read a changes request = %#v, want answered", got)
		}
		if told := gateNotices(t, pool, "LEGION-ROOT", "design-changes-requested"); len(told) != 0 {
			t.Fatalf("design-changes-requested notices = %#v, want none: the resume task tells it", told)
		}
	})

	t.Run("a gate a racing registration opened", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		approved := 4
		seedSlotted(t, pool, "LEGION-ROOT", "A")
		seedGate(t, pool, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: &approved})
		applyFact(t, pool, admission, "seed-changes-v4", seeded)
		var gate *record.DesignGate
		inTx(t, pool, func(tx pgx.Tx) {
			var err error
			if gate, err = record.NewStore().Gate(context.Background(), tx, "LEGION-ROOT"); err != nil {
				t.Fatalf("read gate: %v", err)
			}
		})
		if gate == nil || gate.ApprovedVersion != nil || gate.LatestVersion != 4 {
			t.Fatalf("gate after the seed read a changes request = %#v, want closed at version 4", gate)
		}
		if told := gateNotices(t, pool, "LEGION-ROOT", "design-changes-requested"); len(told) != 1 || told[0].Reason != "name the rollback path" {
			t.Fatalf("design-changes-requested notices = %#v, want one with the reason", told)
		}
	})
}

// handBack is the architect's hand-back of the approval request whose event Dispatch appended at
// seq, its transaction begun at handedBackAt: the wait it starts begins there.
var (
	handedBackAt = fixedNow.Add(-time.Hour)
	handBack     = record.EventMark{Seq: 10, At: handedBackAt}
)

// A changes request Dispatch recorded before the root's wait began answers nothing: the person gave
// it before the request was handed back to them. One recorded after it answers the wait, also when
// its transaction began before the hand-back's and committed after it, so that its time is the
// earlier one: the issue's event sequence, not Dispatch's time, orders the two. A review carrying
// no sequence was recorded before Dispatch kept one (its migration 0117), so before every wait,
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
			applyFact(t, pool, admission, "changes-v4", intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root",
				Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path", Mark: tc.mark})
			if got := issue(t, pool, "LEGION-ROOT"); got.Phase != phase.AwaitingPerson || got.Answered() != tc.answered {
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
				Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4,
				ChangesRequestedVersion: 4, ChangesReason: "name the rollback path", ChangesRequestedMark: tc.mark,
			}, admission.engine, admission); err != nil {
				t.Fatalf("register gate v4: %v", err)
			}
			if got := issue(t, pool, "LEGION-ROOT"); got.Phase != phase.AwaitingPerson || got.Answered() != tc.answered {
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
	registered := intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4,
		Since: record.EventMark{Seq: 10}}
	// Dispatch's time of the review is before the daemon's clock reads the registration.
	review := intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
		Version: 4, Reason: "name the rollback path", Mark: record.EventMark{Seq: 11, At: fixedNow.Add(-time.Second)}}
	for _, source := range []string{"the live event", "the gate seed"} {
		t.Run(source, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", registered, admission.engine, admission); err != nil {
				t.Fatalf("register gate v4: %v", err)
			}
			waiting := issue(t, pool, "LEGION-ROOT")
			if waiting.Phase != phase.AwaitingPerson || waiting.Awaiting.Since != registered.Since || waiting.Answered() {
				t.Fatalf("root after the registration = %#v, want it waiting unanswered from the read's sequence", waiting)
			}
			applyFact(t, pool, admission, source, review)
			if got := issue(t, pool, "LEGION-ROOT"); !got.Answered() {
				t.Fatalf("root after %s of a review the registration's read missed = %#v, want it answered", source, got)
			}
		})
	}
}

// An approval answers a wait whenever it opens the gate, given before the wait began or not: a root
// can wait on a gate the record still shows closed after an approval whose own event was lost, and
// were that approval skipped when the gate seed or the boot catch-up applies it, the root would stay
// suspended with its gate open and nothing left to answer it. Applied again, it answers nothing
// more, and the root resumes once. An approval that opens nothing (the gate already open, or an
// older version approved) answers nothing.
func TestAnApprovalThatOpensTheGateAnswersAWaitWhateverItsTime(t *testing.T) {
	approval := intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved,
		Version: 4, Mark: record.EventMark{Seq: 8, At: handedBackAt.Add(-time.Minute)}}

	t.Run("an approval before the wait, applied by the seed and the catch-up", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmission(t, 1, nil)
		handBackOnAClosedGateAt(t, pool, admission, handBack)
		applyFact(t, pool, admission, "seed-approved-v4", approval)
		answered := issue(t, pool, "LEGION-ROOT")
		if !answered.Answered() {
			t.Fatalf("root after the seed applied an approval given before its wait = %#v, want it answered", answered)
		}
		applyFact(t, pool, admission, "catch-up-approved-v4", approval)
		if again := issue(t, pool, "LEGION-ROOT"); !again.Answered() || !again.Awaiting.AnsweredAt.Equal(*answered.Awaiting.AnsweredAt) {
			t.Fatalf("root after the approval applied again = %#v, want its first answer kept", again)
		}
		apply(t, pool, admission, "next-finished", intake.DispatchIssue{
			Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done",
			Title: "LEGION-NEXT", Rank: "B", HandedOver: true,
		}, admission.engine)
		if resumed := issue(t, pool, "LEGION-ROOT"); resumed.Phase != phase.Planning || resumed.Awaiting != nil {
			t.Fatalf("root once a slot freed = %#v, want planning with no wait", resumed)
		}
		starts := 0
		for _, effect := range effects(t, pool) {
			if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == "LEGION-ROOT" && start.Op == "start" && start.Role == "architect" {
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
		{name: "an approval of a gate already open", gate: record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: new(4)},
			fact: approval},
		{name: "an approval of an older version", gate: record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4},
			fact: intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 3, Mark: record.EventMark{Seq: 11}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{GateVersion: 4, Since: handBack})
			seedGate(t, pool, tc.gate)
			applyFact(t, pool, admission, "approval", tc.fact)
			if got := issue(t, pool, "LEGION-ROOT"); got.Phase != phase.AwaitingPerson || got.Answered() {
				t.Fatalf("root after %s = %#v, want it still waiting unanswered", tc.name, got)
			}
		})
	}
}

// A registration Dispatch shows approved at a later version than the one registered (a person's
// answer to a decision block carried the approval to it) opens the gate at that later version, and
// the architect is told that version.
func TestAnApprovalAtALaterVersionTellsTheArchitectThatVersion(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v3-approved-v4",
		intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 3, ApprovedVersion: 4}, admission.engine, admission); err != nil {
		t.Fatalf("register gate: %v", err)
	}
	if told := gateNotices(t, pool, "LEGION-ROOT", "design-approved"); len(told) != 1 || told[0].Version != 4 {
		t.Fatalf("design-approved notices = %#v, want one at version 4", told)
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
		{name: "a person's reply on the approval request", fact: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "agent", Actor: alice}, wakes: true},
		{name: "a person's answer to a question", fact: intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-question", Kind: "question", State: "answered", Actor: alice}, wakes: true},
		{name: "a session's progress note", fact: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "agent", Actor: outsider}},
		{name: "the architect's own reply", fact: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "human", Actor: architect}},
		{name: "a person's hand-back", fact: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "human", Actor: alice}},
		{name: "a person's review", fact: intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-approval", Kind: "approval", State: "answered", Actor: alice}},
		{name: "a session's resolution of a question", fact: intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-question", Kind: "question", State: "resolved", Actor: architect}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{GateVersion: 3})
			applyFact(t, pool, admission, "act", tc.fact)
			if got := issue(t, pool, "LEGION-ROOT"); got.Answered() != tc.wakes {
				t.Fatalf("root after %s = %#v, want answered %t", tc.name, got, tc.wakes)
			}
		})
	}
}

// Only a person's act answers a root waiting on its question: a person's answer, resolution or
// reply in its thread, the last whatever the ask's state. A session's reply, whatever turn it
// leaves, and a session's or Dispatch's own resolution leave it waiting.
func TestOnlyAPersonsActAnswersARootWaitingOnItsQuestion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fact  intake.Fact
		wakes bool
	}{
		{name: "a person's reply", fact: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-root", WaitingOn: "agent", Actor: alice}, wakes: true},
		{name: "a person's answer", fact: intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-root", Kind: "question", State: "answered", Actor: alice}, wakes: true},
		{name: "a person's resolution", fact: intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-root", Kind: "question", State: "resolved", Actor: alice}, wakes: true},
		{name: "a person's reply under the resolved ask", fact: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-root", Actor: alice}, wakes: true},
		{name: "a session's reply on a person's behalf", fact: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-root", WaitingOn: "agent", Actor: outsider}},
		{name: "a session's reply that leaves the turn with the person", fact: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-root", WaitingOn: "human", Actor: outsider}},
		{name: "a session's resolution", fact: intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-root", Kind: "question", State: "resolved", Actor: architect}},
		{name: "settlement's retraction", fact: intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-root", Kind: "question", State: "resolved", Actor: settler}},
		{name: "a person's reply on another ask", fact: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-other", WaitingOn: "agent", Actor: alice}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{Ask: "ask-root"})
			applyFact(t, pool, admission, "act", tc.fact)
			if got := issue(t, pool, "LEGION-ROOT"); got.Answered() != tc.wakes {
				t.Fatalf("root after %s = %#v, want answered %t", tc.name, got, tc.wakes)
			}
		})
	}
}

// The architect hands an ask on its admitted root back to the person: on a registered closed
// gate the root waits on that gate's latest version, otherwise on the ask itself, and returns its
// slot either way. A hand-back by a session outside the tree, or by a person, changes nothing.
func TestAnArchitectsHandBackReturnsTheSlotToWaitOnThePerson(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gate  *record.DesignGate
		reply intake.DispatchAskReply
		want  *record.Awaiting
	}{
		{name: "the approval request handed back on a closed gate", gate: &record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4},
			reply: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "human", Actor: architect}, want: &record.Awaiting{GateVersion: 4}},
		{name: "a human-turn reply on a question with no gate",
			reply: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-question", WaitingOn: "human", Actor: architect}, want: &record.Awaiting{Ask: "ask-question"}},
		{name: "a reply from outside the tree",
			reply: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-question", WaitingOn: "human", Actor: outsider}},
		{name: "a person's own hand-back",
			reply: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-question", WaitingOn: "human", Actor: alice}},
		{name: "the architect's progress note",
			reply: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-question", WaitingOn: "agent", Actor: architect}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)
			if tc.gate != nil {
				seedGate(t, pool, *tc.gate)
			}
			applyFact(t, pool, admission, "hand-back", tc.reply)

			root := issue(t, pool, "LEGION-ROOT")
			if tc.want == nil {
				if root.Phase != phase.Admitted || root.Awaiting != nil {
					t.Fatalf("root after %s = %#v, want it still admitted", tc.name, root)
				}
				assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
				assertNoAwaitingEffects(t, pool, "LEGION-ROOT")
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
	applyFact(t, pool, admission, "reply", intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "agent", Actor: alice})
	apply(t, pool, admission, "run-finished", intake.DispatchIssue{
		Key: "LEGION-RUN", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done", Title: "LEGION-RUN", Rank: "A", HandedOver: true,
	}, admission.engine)
	var task string
	for _, effect := range effects(t, pool) {
		if start, ok := effect.payload.(record.SuperviseRequest); ok && effect.issue == "LEGION-ROOT" && start.Op == "start" && start.Role == "architect" {
			task = start.Task
		}
	}
	if !strings.Contains(task, "replied") || !strings.Contains(task, "thread") {
		t.Fatalf("resumed architect's task = %q, want it to name a person's reply and the thread", task)
	}
}

// A child admission re-enters at its root's promotion keeps the Dispatch claim its record holds,
// even when its last recorded event is newer than the claim: the synthetic fact names the claim the
// record holds, so nothing takes the holder away.
func TestAStrandedChildReenteredAtItsRootsPromotionKeepsItsClaim(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-ACTIVE", "A")
	const root, child = "LEGION-WAIT", "LEGION-CHILD"
	parent := root
	putIssue(t, pool, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: root, Tree: root,
		Phase: phase.Admitted, Generation: 1, Status: "todo", Rank: "B", HandedOver: true, LastDispatchSeq: 1})
	putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject, Title: "child", Tree: root,
		Parent: &parent, Phase: phase.Done, Generation: 1, Status: "todo", Rank: "C", LastDispatchSeq: 7, ClaimedBy: "user:alice", ClaimSeq: 5})

	apply(t, pool, admission, "active-done", intake.DispatchIssue{Key: "LEGION-ACTIVE", ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated", Status: "done", Title: "LEGION-ACTIVE", Rank: "A"}, admission.engine)
	got := issue(t, pool, child)
	if got.Phase != phase.Admitted || got.ClaimedBy != "user:alice" || got.ClaimSeq < 5 {
		t.Fatalf("child re-entered at its root's promotion = %#v, want admitted with user:alice's claim", got)
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
			stale: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "agent", Actor: alice, Mark: record.EventMark{Seq: 19}},
			fresh: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "agent", Actor: alice, Mark: record.EventMark{Seq: 21}},
		},
		{
			name:  "an answered ask",
			stale: intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-question", Kind: "question", State: "answered", Actor: alice, Mark: record.EventMark{Seq: 19}},
			fresh: intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-question", Kind: "question", State: "answered", Actor: alice, Mark: record.EventMark{Seq: 21}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{GateVersion: 4, Since: record.EventMark{Seq: 20}})

			applyFact(t, pool, admission, "act-before-the-wait", tc.stale)
			if got := issue(t, pool, "LEGION-ROOT"); got.Answered() {
				t.Fatalf("root after an act Dispatch recorded before the wait began = %#v, want it still waiting", got)
			}
			applyFact(t, pool, admission, "act-after-the-wait", tc.fresh)
			if got := issue(t, pool, "LEGION-ROOT"); !got.Answered() {
				t.Fatalf("root after the person's next act = %#v, want it answered", got)
			}
		})
	}
}

// Dispatch's intake retries a delivery, so a person's answer can reach the daemon before the
// ask.opened it closes. The root records that close, so the question, delivered after it, starts
// no wait: nothing would ever answer one on an ask the person has already answered. Another
// question of the root's still starts its wait.
func TestAQuestionAPersonHasAlreadyAnsweredStartsNoWait(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)

	applyFact(t, pool, admission, "answer-delivered-first", intake.DispatchAskClosed{
		Key: "LEGION-ROOT", Ask: "ask-question", Kind: "question", State: "answered", Actor: alice, Mark: record.EventMark{Seq: 11}})
	applyFact(t, pool, admission, "question-opened-retried-later", intake.DispatchAskOpened{
		Key: "LEGION-ROOT", Ask: "ask-question", Kind: "question", AuthorSession: architect.ID, Mark: record.EventMark{Seq: 10}})
	if got := issue(t, pool, "LEGION-ROOT"); got.Phase != phase.Admitted || got.Awaiting != nil {
		t.Fatalf("root after the answer and the question it closed = %#v, want it admitted with no wait", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})

	applyFact(t, pool, admission, "another-question", intake.DispatchAskOpened{
		Key: "LEGION-ROOT", Ask: "ask-other", Kind: "question", AuthorSession: architect.ID, Mark: record.EventMark{Seq: 12}})
	got := issue(t, pool, "LEGION-ROOT")
	if !got.AwaitingPerson() || got.Answered() || got.Awaiting.Ask != "ask-other" {
		t.Fatalf("root after a question nobody answered = %#v, want it waiting on ask-other", got)
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
	opened := intake.DispatchAskOpened{Key: "LEGION-ROOT", Ask: "ask-question", Kind: "question", AuthorSession: architect.ID, Mark: handBack}
	handedBack := intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "human", Actor: architect, Mark: handBack}
	changes := func(mark record.EventMark) intake.DispatchArtifact {
		return intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
			Version: 4, Reason: "name the rollback path", Mark: mark}
	}
	for _, tc := range []struct {
		name   string
		act    intake.Fact
		starts intake.Fact
		waits  bool
	}{
		{name: "a person's reply before the question it replies to",
			act: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-question", WaitingOn: "agent", Actor: alice, Mark: act}, starts: opened},
		{name: "a person's reply before the hand-back",
			act: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "agent", Actor: alice, Mark: act}, starts: handedBack},
		{name: "a person's answer of another ask before the hand-back",
			act: intake.DispatchAskClosed{Key: "LEGION-ROOT", Ask: "ask-question", Kind: "question", State: "answered", Actor: alice, Mark: act}, starts: handedBack},
		{name: "a changes request before the hand-back", act: changes(act), starts: handedBack},
		{name: "a person's reply Dispatch recorded before the hand-back",
			act: intake.DispatchAskReply{Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "agent", Actor: alice, Mark: early}, starts: handedBack, waits: true},
		{name: "a changes request Dispatch recorded before the hand-back", act: changes(early), starts: handedBack, waits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4})

			applyFact(t, pool, admission, "act-delivered-first", tc.act)
			applyFact(t, pool, admission, "wait-start-retried-later", tc.starts)
			got := issue(t, pool, "LEGION-ROOT")
			if tc.waits {
				if !got.AwaitingPerson() || got.Answered() {
					t.Fatalf("root after an act before its wait's start = %#v, want it waiting unanswered", got)
				}
				return
			}
			if got.Phase != phase.Admitted || got.Awaiting != nil {
				t.Fatalf("root after %s = %#v, want it admitted with no wait", tc.name, got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
			assertNoAwaitingEffects(t, pool, "LEGION-ROOT")
		})
	}
}

// A registration reads the approval and nothing else, so it fences only what would start a wait
// on the gate. An ordinary question its read never saw, delivered late, still releases the root's
// slot and waits on the person.
func TestARegistrationDoesNotFenceAQuestionItNeverRead(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmission(t, 1, nil)
	seedSlotted(t, pool, "LEGION-ROOT", "A")
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)
	read := record.EventMark{Seq: 20, At: handedBackAt}
	if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
		Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4, RequestWaitsOnAgent: true, Since: read,
	}, admission.engine, admission); err != nil {
		t.Fatalf("register gate v4: %v", err)
	}
	if kept := issue(t, pool, "LEGION-ROOT"); kept.Phase != phase.Admitted || kept.GateFenceSeq != read.Seq || kept.WaitFenceSeq != 0 {
		t.Fatalf("root after a registration that kept the slot = %#v, want admitted with gate fence %d and no wait fence", kept, read.Seq)
	}

	applyFact(t, pool, admission, "question-below-the-read", intake.DispatchAskOpened{
		Key: "LEGION-ROOT", Ask: "ask-question", Kind: "question", AuthorSession: architect.ID, Mark: record.EventMark{Seq: 19}})
	got := issue(t, pool, "LEGION-ROOT")
	if !got.AwaitingPerson() || got.Answered() || got.Awaiting.Ask != "ask-question" {
		t.Fatalf("root after a question the approval read never saw = %#v, want it waiting on that question", got)
	}
}

// A registration whose read of Dispatch began before a review the daemon has already applied at
// that version is the older reader: the person acted after it. The gate keeps that review — a
// told changes request is not told again, an approval is not rolled back — and no wait starts,
// since only the person's next act could answer one.
func TestARegistrationDoesNotHideTheReviewTheGateAlreadyHolds(t *testing.T) {
	reviewed := record.EventMark{Seq: 11, At: handedBackAt.Add(time.Minute)}
	for _, tc := range []struct {
		name string
		gate record.DesignGate
		open bool
	}{
		{
			name: "a changes request the architect was told of",
			gate: record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4,
				ToldChangesVersion: new(4), ToldChangesReason: "name the rollback path", ToldChangesMark: reviewed},
		},
		{
			name: "an approval that opened the gate",
			gate: record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4,
				ApprovedVersion: new(4), ApprovedMark: reviewed},
			open: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)
			seedGate(t, pool, tc.gate)

			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
				Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, DispatchVersion: 4,
				ChangesRequestedVersion: 4, ChangesReason: "name the rollback path",
				ChangesRequestedMark: record.EventMark{Seq: 9, At: handedBackAt.Add(-time.Minute)},
				Since:                record.EventMark{Seq: 10, At: handedBackAt},
			}, admission.engine, admission); err != nil {
				t.Fatalf("register gate v4: %v", err)
			}

			got := issue(t, pool, "LEGION-ROOT")
			if got.Phase != phase.Admitted || got.Awaiting != nil {
				t.Fatalf("root after a registration older than the review the gate holds = %#v, want it admitted with no wait", got)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
			if told := gateNotices(t, pool, "LEGION-ROOT", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("design-changes-requested notices = %#v, want none: the architect already heard the review the gate holds", told)
			}
			if gate := storedGate(t, pool, "LEGION-ROOT"); classify.DesignGateOpen(*gate) != tc.open {
				t.Fatalf("gate after the registration = %#v, want open=%t", gate, tc.open)
			}
		})
	}
}

// A review the person has already followed with another at that version changes nothing: the
// daemon applied the newer one first, and this one reached it late. An approval delivered after
// the changes request that superseded it does not reopen the gate or answer the wait, and a
// changes request delivered after a newer approval does not close it or tell the architect.
func TestAReviewAnotherSupersededChangesNoGateState(t *testing.T) {
	superseded := record.EventMark{Seq: 18, At: handedBackAt.Add(-time.Minute)}
	newer := record.EventMark{Seq: 19, At: handedBackAt}
	for _, tc := range []struct {
		name string
		gate record.DesignGate
		late intake.DispatchArtifact
		open bool
	}{
		{
			name: "an approval older than the changes request the gate records",
			gate: record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4,
				ToldChangesVersion: new(4), ToldChangesReason: "name the rollback path", ToldChangesMark: newer},
			late: intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 4, Mark: superseded},
		},
		{
			name: "a changes request older than the approval the gate records",
			gate: record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4,
				ApprovedVersion: new(4), ApprovedMark: newer},
			late: intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
				Version: 4, Reason: "name the rollback path", Mark: superseded},
			open: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			awaitingRoot(t, pool, record.Awaiting{GateVersion: 4, Since: record.EventMark{Seq: 20, At: handedBackAt.Add(time.Minute)}})
			seedGate(t, pool, tc.gate)

			applyFact(t, pool, admission, "superseded-review", tc.late)

			if got := issue(t, pool, "LEGION-ROOT"); got.Answered() {
				t.Fatalf("root after a review the person had already followed with another = %#v, want it still waiting", got)
			}
			if told := gateNotices(t, pool, "LEGION-ROOT", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("design-changes-requested notices = %#v, want none for a superseded review", told)
			}
			if gate := storedGate(t, pool, "LEGION-ROOT"); classify.DesignGateOpen(*gate) != tc.open {
				t.Fatalf("gate after the superseded review = %#v, want open=%t", gate, tc.open)
			}
		})
	}
}

// storedGate is the issue's design gate, which must be recorded.
func storedGate(t *testing.T, pool *pgxpool.Pool, key string) *record.DesignGate {
	t.Helper()
	var gate *record.DesignGate
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if gate, err = record.NewStore().Gate(context.Background(), tx, key); err != nil {
			t.Fatalf("read gate: %v", err)
		}
	})
	if gate == nil {
		t.Fatalf("issue %s has no design gate", key)
	}
	return gate
}

// A review of a version below the gate's latest changes nothing on the gate that owns the
// document: the person has a newer version to review. A late approval of an older version does not
// clear the record that the architect was told the newer version's changes request, so nothing
// tells it again; a late changes request of an older version does not close a gate open at the
// newer one, which would refuse READY while Dispatch shows the newer version approved.
func TestAReviewOfAnOlderVersionChangesNoGateState(t *testing.T) {
	reviewed := record.EventMark{Seq: 11, At: handedBackAt}
	late := record.EventMark{Seq: 9, At: handedBackAt.Add(-time.Minute)}
	for _, tc := range []struct {
		name string
		gate record.DesignGate
		late intake.DispatchArtifact
		open bool
	}{
		{
			name: "an approval of the older version after the newer one's told changes request",
			gate: record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 2,
				ToldChangesVersion: new(2), ToldChangesReason: "name the rollback path", ToldChangesMark: reviewed},
			late: intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactApproved, Version: 1, Mark: late},
		},
		{
			name: "a changes request of the older version after the newer one's approval",
			gate: record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 2, ApprovedVersion: new(2), ApprovedMark: reviewed},
			late: intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
				Version: 1, Reason: "name the rollback path", Mark: late},
			open: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedGate(t, pool, tc.gate)

			applyFact(t, pool, admission, "late-older-review", tc.late)

			gate := storedGate(t, pool, "LEGION-ROOT")
			if classify.DesignGateOpen(*gate) != tc.open || gate.LatestVersion != 2 {
				t.Fatalf("gate after a late review of version 1 = %#v, want it at version 2 with open=%t", gate, tc.open)
			}
			if !tc.open && (gate.ToldChangesVersion == nil || *gate.ToldChangesVersion != 2 || gate.ToldChangesMark != reviewed) {
				t.Fatalf("gate after a late approval of version 1 = %#v, want the told changes request of version 2 kept", gate)
			}
			if told := gateNotices(t, pool, "LEGION-ROOT", "design-changes-requested"); len(told) != 0 {
				t.Fatalf("design-changes-requested notices = %#v, want none for a review of an older version", told)
			}
		})
	}
}
