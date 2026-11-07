package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/classify"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The documents of the steps these tests gate: artifact-root is the root's primary document,
// which the spec step binds to, and these are its decisions and Verification documents.
// register_gate carries no step of its own yet, so a registration writes the row the document it
// names already holds and records the spec step for a document no row holds: a step other than
// spec is given its row once with PutGate, to bind the document to the step, and every fact after
// that is the engine's own.
const (
	decisionsDoc = "artifact-decisions"
	criteriaDoc  = "artifact-criteria"
)

// A step the deployment gates that nobody has registered holds the tree's work and asks nobody:
// the root keeps its slot, no awaiting-person notice reaches the controller, and no phase starts,
// because the record knows no document of that step to hand to a person. The same root with that
// step registered and still closed does give its slot back, which is what tells the two apart,
// and it starts once the step is registered and approved.
func TestUnregisteredStepHoldsTheWorkAndRaisesNoWait(t *testing.T) {
	steps := []record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec}
	t.Run("the step nobody registered", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmissionWithSteps(t, 1, steps, slog.New(slog.NewTextHandler(io.Discard, nil)))
		runningRoot(t, pool)

		// The spec step is registered with the approval its read of Dispatch found, so it is open
		// and the work stands on the decisions step, which has no row at all.
		applyFact(t, pool, admission, "register-spec", intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root",
			Version: 3, ApprovedVersion: 3, RequestAsk: "ask-approval", Since: record.EventMark{Seq: 5}})

		assertAdmittedInItsSlot(t, pool, "a registration of the spec step while the decisions step is unregistered")
		assertNoAwaitingEffects(t, pool, "LEGION-100")
		if starts := phaseStarts(t, pool, "LEGION-100", claim.RolePlanner, phase.Planning); starts != 0 {
			t.Fatalf("the root's planner was started %d times with the decisions step unregistered, want none", starts)
		}

		// Registering and approving the decisions step is what starts the tree.
		seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", Step: record.DesignStepDecisions, ArtifactID: decisionsDoc, LatestVersion: 1})
		applyFact(t, pool, admission, "register-decisions", intake.GateRegistered{Issue: "LEGION-100", Step: record.DesignStepDecisions, ArtifactID: decisionsDoc, Version: 2})
		if got := issue(t, pool, "LEGION-100"); got.Phase != phase.Admitted {
			t.Fatalf("the root = %s with the decisions step registered and closed, want it admitted", got.Phase)
		}
		applyFact(t, pool, admission, "approve-decisions", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: decisionsDoc,
			Kind: intake.DispatchArtifactApproved, Version: 2, Actor: alice, Mark: seqMark(11)})
		if got := issue(t, pool, "LEGION-100"); got.Phase != phase.Planning {
			t.Fatalf("the root = %s once the decisions step is registered and approved, want planning", got.Phase)
		}
	})

	t.Run("the step registered and unapproved", func(t *testing.T) {
		pool := migratedPool(t)
		admission := newAdmissionWithSteps(t, 1, steps, slog.New(slog.NewTextHandler(io.Discard, nil)))
		runningRoot(t, pool)
		seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", Step: record.DesignStepDecisions, ArtifactID: decisionsDoc, LatestVersion: 1})

		applyFact(t, pool, admission, "register-spec", intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root",
			Version: 3, ApprovedVersion: 3, RequestAsk: "ask-approval", Since: record.EventMark{Seq: 5}})
		applyFact(t, pool, admission, "register-decisions", intake.GateRegistered{Issue: "LEGION-100", Step: record.DesignStepDecisions, ArtifactID: decisionsDoc,
			Version: 2, RequestAsk: "ask-decisions", Since: record.EventMark{Seq: 6}})

		waiting := issue(t, pool, "LEGION-100")
		if waiting.Phase != phase.AwaitingPerson || waiting.Awaiting == nil || waiting.Awaiting.Artifact != decisionsDoc ||
			waiting.Awaiting.GateVersion != 2 {
			t.Fatalf("the root = %#v, want it waiting on the decisions document at version 2", waiting.Awaiting)
		}
		assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
		told := false
		for _, effect := range effects(t, pool) {
			if payload, ok := effect.payload.(record.ControllerNotice); ok && effect.issue == "LEGION-100" && payload.Kind == record.AwaitingPersonNotice {
				told = true
			}
		}
		if !told {
			t.Fatalf("outbox effects = %#v, want the awaiting-person notice a registered closed step raises", effects(t, pool))
		}
	})
}

// A hand-over carries a copy of every step of its old root's gate, each keeping its own step, so
// a tree gated on several steps arrives approved on each of them as the old root was, and the new
// root's phases run on from where the old tree left them.
func TestGatedStepsCarryEveryStepToAHandedOverChild(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmissionWithSteps(t, 1, []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	const root, child = "LEGION-208", "LEGION-209"
	until := fixedNow.Add(time.Hour)
	parent := root
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutIssue(context.Background(), tx, record.Issue{Key: root, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: root, Project: testProject, Title: root, Phase: phase.Done, Generation: 1, Status: "done", LingerUntil: &until}),
			records.PutIssue(context.Background(), tx, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Tree: root, Project: testProject, Title: child, Parent: &parent, Phase: phase.Merging, Generation: 1, Status: "retro", Rank: "B",
				LastDispatchSeq: 1}),
			records.PutGate(context.Background(), tx, record.DesignGate{Issue: root, Step: record.DesignStepCriteria,
				ArtifactID: criteriaDoc, LatestVersion: 2, ApprovedVersion: new(2)}),
			records.PutGate(context.Background(), tx, record.DesignGate{Issue: root, Step: record.DesignStepSpec,
				ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: new(4)}),
			records.PutHandover(context.Background(), tx, record.Handover{Issue: child, Tree: root, Generation: 1}),
		} {
			if put != nil {
				t.Fatalf("seed: %v", put)
			}
		}
	})

	apply(t, pool, admission, "handover", intake.DispatchIssue{Key: child, ResolvedRepo: "acme/widgets", Seq: 2, Type: "issue.updated",
		Status: "todo", Title: child, Parent: root, Rank: "B", HandedOver: true, DaemonActor: true}, admission.engine)

	if got := issue(t, pool, child); got.Tree != child || got.Generation != 2 || got.Phase != phase.Merging {
		t.Fatalf("the handed-over child = %#v, want its own tree at generation 2, still merging", got)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		gates, err := records.GatesOf(context.Background(), tx, child)
		if err != nil || len(gates) != 2 {
			t.Fatalf("the carried gates = %#v, %v; want one row for each step the old root held", gates, err)
		}
		for step, want := range map[record.DesignStep]record.DesignGate{
			record.DesignStepCriteria: {Issue: child, Step: record.DesignStepCriteria, ArtifactID: criteriaDoc, LatestVersion: 2, ApprovedVersion: new(2)},
			record.DesignStepSpec:     {Issue: child, Step: record.DesignStepSpec, ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: new(4)},
		} {
			got := gates[step]
			if got.Issue != want.Issue || got.Step != want.Step || got.ArtifactID != want.ArtifactID || got.LatestVersion != want.LatestVersion ||
				got.ApprovedVersion == nil || *got.ApprovedVersion != *want.ApprovedVersion {
				t.Fatalf("the carried %s gate = %#v, want %#v", step, got, want)
			}
		}
		if handedRoot, err := records.HandedOverRoot(context.Background(), tx, child); err != nil || handedRoot == nil ||
			handedRoot.Generation != 2 || !handedRoot.GateCarried {
			t.Fatalf("the handed-over root = %#v, %v; want generation 2 with its gate carried", handedRoot, err)
		}
	})
	if starts := phaseStarts(t, pool, child, claim.RoleMerger, phase.Merging); starts != 1 {
		t.Fatalf("the carried root's merger was started %d times, want once: every carried step is open, so its phase runs", starts)
	}
}

// A step nobody has registered holds the tree's work and nothing more: the wait a registered
// step after it raised still reaches the person, and the root gives its slot back for the next
// root in line. Reading the unregistered step as the step the person has would leave this root
// holding the slot while a person holds its approval request, with no wait left to answer.
func TestUnregisteredStepDoesNotHideTheWaitARegisteredStepRaises(t *testing.T) {
	pool := migratedPool(t)
	admission := newAdmissionWithSteps(t, 1, []record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	runningRoot(t, pool)

	// The spec step is registered, unapproved, and its approval request is with the person; the
	// decisions step has no row at all.
	applyFact(t, pool, admission, "register-spec", intake.GateRegistered{Issue: "LEGION-100", ArtifactID: "artifact-root",
		Version: 3, RequestAsk: "ask-approval", Since: record.EventMark{Seq: 5}})

	waiting := issue(t, pool, "LEGION-100")
	if waiting.Phase != phase.AwaitingPerson || waiting.Awaiting == nil || waiting.Awaiting.Artifact != "artifact-root" ||
		waiting.Awaiting.GateVersion != 3 {
		t.Fatalf("the root = %#v, want it waiting on its primary document at version 3", waiting.Awaiting)
	}
	assertSlots(t, pool, []record.Slot{{Issue: "LEGION-NEXT", Index: 0, AdmittedAt: fixedNow}})
}

// The approval a re-admission keeps for the next architect's start carries the step it is of, so
// the start names the document the person reviewed. Under the decisions step and the spec, with
// the decisions document approved below its latest version, that architect is sent to the
// decisions document at the decisions document's versions, not to the spec.
func TestGatedStepsKeepTheStaleApprovalOfItsOwnDocument(t *testing.T) {
	ctx := context.Background()
	pool := migratedPool(t)
	admission := newAdmissionWithSteps(t, 1, []record.DesignStep{record.DesignStepDecisions, record.DesignStepSpec},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	parkedRoot(t, pool)
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		for _, put := range []error{
			records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-ROOT", Step: record.DesignStepDecisions,
				ArtifactID: decisionsDoc, LatestVersion: 3, ApprovedVersion: new(2)}),
			records.PutGate(ctx, tx, record.DesignGate{Issue: "LEGION-ROOT", Step: record.DesignStepSpec,
				ArtifactID: "artifact-root", LatestVersion: 5, ApprovedVersion: new(5)}),
		} {
			if put != nil {
				t.Fatalf("seed the gates: %v", put)
			}
		}
	})

	readmitRoot(t, pool, admission)

	want := "The decisions document is approved at version 2 and is now at version 3: request approval of version 3, " +
		"as skill://legion-architect/references/design-gate.md (\"The armed gate\") says."
	starts := architectStartRows(t, pool)
	if len(starts) != 1 || starts[0].Task != want {
		t.Fatalf("architect starts %#v, want one whose task is %q", starts, want)
	}
	if kept := starts[0].Kept; kept == nil || kept.Step != record.DesignStepDecisions || kept.Approved != 2 || kept.Latest != 3 {
		t.Fatalf("the start's kept record %#v, want the decisions step's own pair", kept)
	}
}

// The deployment's step list is read at the boot, before the catch-up applies what Dispatch
// recorded while the daemon was down, so the pass that advances the trees a dropped step
// released decides no tree whose root a hold holds. Such a root's spec stands at the version the
// record still shows, which the architect has revised since: started there, the tree would run
// on a version nobody approved, and nothing moves a running phase back. Held, it waits for the
// pass after its catch-up's release, which reads what the catch-up applied and leaves it where
// it stands. With nothing held — the same boot, the same record — that pass is what starts the
// root, which is what it is for.
func TestABootOnAShorterListHoldsARootItsCatchUpHasNotReached(t *testing.T) {
	for _, tc := range []struct {
		name string
		// catchUp is whether the boot holds the root for the catch-up of its event window.
		catchUp bool
		// boots is where the boot's own pass leaves the root.
		boots phase.Phase
	}{
		{name: "the boot's catch-up holds the root", catchUp: true, boots: phase.Admitted},
		{name: "nothing holds the root", boots: phase.Planning},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			// This boot gates the spec alone. The tree was admitted under the decisions step as
			// well, whose row it still holds closed and which this deployment no longer reads,
			// so the spec row the record shows open is all that holds its work now.
			admission := newAdmissionWithSteps(t, 1, []record.DesignStep{record.DesignStepSpec},
				slog.New(slog.NewTextHandler(io.Discard, nil)))
			seedSlotted(t, pool, "LEGION-100", "A")
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", Step: record.DesignStepDecisions,
				ArtifactID: decisionsDoc, LatestVersion: 1})
			seedGate(t, pool, record.DesignGate{Issue: "LEGION-100", Step: record.DesignStepSpec,
				ArtifactID: "artifact-root", LatestVersion: 4, ApprovedVersion: new(4)})
			if tc.catchUp {
				admission.HoldForCatchUp("LEGION-100")
			}

			// The boot's own pass, with the Dispatch consumer already at the position it
			// measured, so the catch-up hold is the only hold in it.
			reconcileWithPosition(t, pool, admission, []dispatch.IssueSummary{
				{Key: "LEGION-100", ResolvedRepo: "acme/widgets", Title: "LEGION-100", Status: "in_progress",
					Rank: "A", HandedOver: handed, LastSeq: 1},
			}, 1, 1, false)
			if got := issue(t, pool, "LEGION-100").Phase; got != tc.boots {
				t.Fatalf("the root after a boot that drops a step = %s, want %s", got, tc.boots)
			}
			if !tc.catchUp {
				return
			}

			// The catch-up applies the window: version 5 of the spec, written while the daemon
			// was down, closes the gate the boot's pass read as open. Then the hold goes and the
			// pass intake.CatchUpApplied drives decides the tree on the record as it now stands.
			applyFact(t, pool, admission, "spec-v5", intake.DispatchArtifact{Key: "LEGION-100", ArtifactID: "artifact-root",
				Kind: intake.DispatchArtifactVersion, Version: 5, Mark: seqMark(7)})
			admission.ReleaseCatchUp("LEGION-100")
			applyFact(t, pool, admission, "catch-up-applied", intake.CatchUpApplied{Issue: "LEGION-100", Through: 7})

			if got := issue(t, pool, "LEGION-100").Phase; got != phase.Admitted {
				t.Fatalf("the root once its catch-up applied version 5 = %s, want it still admitted: no person has approved that version", got)
			}
			if gate := storedGate(t, pool, "LEGION-100"); gate.LatestVersion != 5 || classify.DesignGateOpen(*gate) {
				t.Fatalf("the spec gate after the catch-up = %#v, want it at version 5 and closed", gate)
			}
		})
	}
}
