package admit

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
		{
			name:     "the request Dispatch shows waiting on the architect",
			fact:     intake.GateRegistered{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, RequestWaitsOnAgent: true},
			resumeAt: phase.Admitted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedWaiting(t, pool, "LEGION-NEXT", "B")
			seedArchitectClaim(t, pool, "LEGION-ROOT", architect.ID)
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 4})
			applyFact(t, pool, admission, "hand-back", intake.DispatchAskReply{
				Key: "LEGION-ROOT", Ask: "ask-approval", WaitingOn: "human", Actor: architect})
			if waiting := issue(t, pool, "LEGION-ROOT"); waiting.Phase != phase.AwaitingPerson || waiting.Answered() {
				t.Fatalf("root after its architect handed the request back = %#v, want it waiting unanswered", waiting)
			}

			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", tc.fact, admission.engine, admission); err != nil {
				t.Fatalf("register gate v4: %v", err)
			}
			answered := issue(t, pool, "LEGION-ROOT")
			if !answered.Answered() {
				t.Fatalf("root after the registration carried the person's act = %#v, want it answered", answered)
			}
			if tc.live.ArtifactID != "" {
				applyFact(t, pool, admission, "live-review", tc.live)
				applyFact(t, pool, admission, "seeded-review", func() intake.DispatchArtifact {
					seeded := tc.live
					seeded.Seed = true
					return seeded
				}())
			}

			apply(t, pool, admission, "next-finished", intake.DispatchIssue{
				Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 1, Type: "issue.closed", Status: "done",
				Title: "LEGION-NEXT", Rank: "B", HandedOver: true,
			}, admission.engine)
			resumed := issue(t, pool, "LEGION-ROOT")
			if resumed.Phase != tc.resumeAt || resumed.Awaiting != nil {
				t.Fatalf("root once a slot freed = %#v, want %s with no wait", resumed, tc.resumeAt)
			}
			assertSlots(t, pool, []record.Slot{{Issue: "LEGION-ROOT", Index: 0, AdmittedAt: fixedNow}})
		})
	}
}

// A changes request the architect was already told of, which register_gate reads from Dispatch
// afterwards, tells it nothing new: the gate records the request's version, so the registration
// records it again without a second notice. A changes request no event carried is still told.
func TestARegistrationTellsAChangesRequestTheArchitectHasNotHeard(t *testing.T) {
	for _, tc := range []struct {
		name string
		live bool
		want int
	}{
		{name: "the live event told it first", live: true, want: 1},
		{name: "no event carried the request", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := newAdmission(t, 1, nil)
			seedSlotted(t, pool, "LEGION-ROOT", "A")
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-ROOT", ArtifactID: "artifact-root", LatestVersion: 3})
			applyFact(t, pool, admission, "spec-v4", intake.DispatchArtifact{
				Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactVersion, Version: 4})
			if tc.live {
				applyFact(t, pool, admission, "changes-v4", intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root",
					Kind: intake.DispatchArtifactChangesRequested, Version: 4, Reason: "name the rollback path"})
			}

			if _, err := intake.ApplyFact(context.Background(), pool, "api", "register-v4", intake.GateRegistered{
				Issue: "LEGION-ROOT", ArtifactID: "artifact-root", Version: 4, ChangesRequestedVersion: 4, ChangesReason: "name the rollback path",
			}, admission.engine, admission); err != nil {
				t.Fatalf("register gate v4: %v", err)
			}
			told := gateNotices(t, pool, "LEGION-ROOT", "design-changes-requested")
			if len(told) != tc.want || told[0].Version != 4 || told[0].Reason != "name the rollback path" {
				t.Fatalf("design-changes-requested notices = %#v, want %d at version 4 with the reason", told, tc.want)
			}
		})
	}
}

// The gate seed's own read of a changes request (Seed) answers a wait a registration started
// before the request reached the root, without telling the architect a second time; on a gate a
// racing registration opened, it closes the gate again and tells the architect.
func TestASeededChangesRequestAnswersAWaitAndClosesAGateARaceOpened(t *testing.T) {
	seeded := intake.DispatchArtifact{Key: "LEGION-ROOT", ArtifactID: "artifact-root", Kind: intake.DispatchArtifactChangesRequested,
		Version: 4, Reason: "name the rollback path", Seed: true}

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
