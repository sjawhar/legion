package admit

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
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
