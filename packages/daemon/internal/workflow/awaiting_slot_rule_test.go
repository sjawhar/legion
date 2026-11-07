package workflow

import (
	"context"
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

// The spec's rule, restored: a root enters awaiting_person only while it holds a slot, so only a
// root whose design gate has never opened, whose architect is writing the spec and holds the
// whole of the tree's work (dispatch://INFRA-980, "Which issue needs a slot", line 65).
//
// A root whose gate has opened waits for its children instead, and the slot row it still holds
// in this pass is one admission's own release takes back later in the same pass. Sending such a
// root into awaiting_person takes every first-phase member of its tree out of the waiting line
// (record.Waiting drops the members of a root waiting on a person), so the children it is
// waiting for can never start; the controller is told its slot is released though it holds none
// of its own work; and the next pass answers the wait as an approval, because the gate is open,
// which clears what the architect handed to the person.
func TestARootWhoseGateHasOpenedEntersNoWaitOnAPerson(t *testing.T) {
	root := record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Phase: phase.Admitted,
		Generation: 1, Status: "in_progress", DispatchStatus: "in_progress", HandedOver: true, DeclaredSeq: 9}
	approved := 1
	opened := record.DesignGate{Issue: root.Key, Step: record.DesignStepSpec, ArtifactID: "spec", LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true}
	rows := map[record.DesignStep]record.DesignGate{record.DesignStepSpec: opened}

	decision := decideAwait(awaitView{Root: root, Slotted: true, Gates: rows})
	if decision.Act != actStay {
		t.Fatalf("the decision for a root whose gate has opened = %+v, want it left where it stands: it holds no slot to hand back, and its children are waiting for theirs", decision)
	}

	// The control: the same root before its gate ever opened, where the architect is writing the
	// spec and does hold the slot, is not stopped by this rule — whatever the rest of the rules
	// then decide, rule 0 is not what answers it.
	writing := opened
	writing.ApprovedVersion, writing.EverOpened = nil, false
	if decision := decideAwait(awaitView{Root: root, Slotted: true, Gates: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: writing}}); decision.Why == noSlotOfItsOwn {
		t.Fatalf("a root whose gate has never opened and holds the slot is stopped by the slot rule: that root is the one the wait is for")
	}
}

// noSlotOfItsOwn is rule 0's answer for a root that holds no admission slot for work of its own.
const noSlotOfItsOwn = "it holds no admission slot for work of its own"

// A root re-admitted and still waiting in line for its slot holds none, so it enters no wait on
// a person however its hand-back stands. Re-admission clears the mark (record.Store.StartHandout),
// but the architect of the generation before is live until the linger's queued suspend runs, and
// a declaration of its makes the mark non-zero again, so rule 1 does not keep such a root out.
// Rule 0 does, and only because it reads the root's own slot row: the wait is that slot going
// back, and a root that was never promoted has nothing to give. Were it to enter the wait, the
// next pass would promote it as an answered root rather than through its admission — no
// lifecycle opened, no issue branch written — and the pass after that would read an in_progress
// root of no running tree and let it linger (admit's releaseUnslotted). (Round 4, the reviewer's
// medium finding on rule 0.)
func TestAQueuedRootWithAHandBackEntersNoWaitOnAPerson(t *testing.T) {
	view := awaitRootView()
	view.Root.Generation, view.Root.Status, view.Root.DispatchStatus = 2, "todo", "todo"
	view.Slotted = false
	if decision := decideAwait(view); decision.Act != actStay || decision.Why != noSlotOfItsOwn {
		t.Fatalf("the decision for a re-admitted root still waiting in line = %+v, want it left where it stands: it holds no slot to hand back", decision)
	}

	// The control: the same root once admission promoted it and gave it the slot enters the wait.
	view.Slotted = true
	if decision := decideAwait(view); decision.Act != actEnter {
		t.Fatalf("the decision for the same root holding its slot = %+v, want it to enter the wait", decision)
	}
}

// ReconcileAwaiting reads the slot rows of the record, so the rule above holds of the pass and
// not only of its decision: over one record holding two roots that have each handed a question
// to a person, it moves the one admission promoted and leaves the one still waiting in line.
func TestReconcileAwaitingMovesOnlyTheRootHoldingItsSlot(t *testing.T) {
	const slottedKey, queuedKey = "LEGION-208", "LEGION-300"
	pool := migratedPool(t)
	slotted := askRoot()
	slotted.HandedOver = true
	queued := record.Issue{Key: queuedKey, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: queuedKey,
		Project: "LEGION", Title: "a root waiting in line", Phase: phase.Admitted, Generation: 2, Status: "todo", DispatchStatus: "todo",
		Rank: "V", HandedOver: true, LastDispatchSeq: 1}
	handedBack := record.EventMark{Seq: 5, At: time.Date(2026, 9, 22, 23, 0, 0, 0, time.UTC)}
	for _, root := range []record.Issue{slotted, queued} {
		seedIssue(t, pool, root)
		seedArchitectWorking(t, pool, root)
		seedRecord(t, pool, func(tx pgx.Tx) error {
			store := record.NewStore()
			if err := store.PutRootAsk(context.Background(), tx, record.RootAsk{Issue: root.Key, Ask: "ask-" + root.Key,
				Kind: record.AskQuestion, Open: true, Seq: 5, WaitingOn: "human", WaitingOnSeq: 5, ByTree: true}); err != nil {
				return err
			}
			return store.Declare(context.Background(), tx, root.Key, handedBack)
		})
	}
	seedSlot(t, pool, record.Slot{Issue: slottedKey, Index: 0, AdmittedAt: time.Date(2026, 9, 22, 22, 0, 0, 0, time.UTC)})

	entered := reconcileRoots(t, pool, slottedKey, queuedKey)
	if len(entered) != 1 || entered[0] != slottedKey {
		t.Fatalf("the roots that entered the wait = %v, want only the one holding a slot", entered)
	}
	if got := phaseOf(t, pool, queuedKey); got != phase.Admitted {
		t.Fatalf("the root still waiting in line = %s, want it left admitted: it holds no slot to hand back", got)
	}
	if got := phaseOf(t, pool, slottedKey); got != phase.AwaitingPerson {
		t.Fatalf("the root holding its slot = %s, want it waiting on its person", got)
	}
}

// reconcileRoots runs one ReconcileAwaiting pass over keys, read from the record as they stand,
// under the root-issues design gate, and returns the roots that entered the wait.
func reconcileRoots(t *testing.T, pool *pgxpool.Pool, keys ...string) []string {
	t.Helper()
	var entered []string
	seedRecord(t, pool, func(tx pgx.Tx) error {
		ctx, _ := intake.WithCommitHooks(context.Background())
		store := record.NewStore()
		issues := make([]record.Issue, 0, len(keys))
		for _, key := range keys {
			issue, err := store.Issue(ctx, tx, key)
			if err != nil {
				return err
			}
			issues = append(issues, *issue)
		}
		var err error
		entered, _, err = testEngine(config.DesignGateRootIssues, nil).ReconcileAwaiting(ctx, tx, "LEGION", issues, nil)
		return err
	})
	return entered
}

// seedArchitectWorking records root's architect claim working a task it took, so the root's
// architect owes nothing (decideAwait's rule 5) and the wait may begin.
func seedArchitectWorking(t *testing.T, pool *pgxpool.Pool, root record.Issue) {
	t.Helper()
	project, err := claim.ProjectToken(root.Project)
	if err != nil {
		t.Fatalf("project token: %v", err)
	}
	token, err := claim.NewToken(project, root.Key, claim.RoleArchitect)
	if err != nil {
		t.Fatalf("architect token: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file,
		state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, $2, $3, $4, 'architect', $5, 'session-architect', '', 'working', 0, 0, 0, 0)`,
		string(token), root.Project, root.Tree, root.Key, int64(root.Generation)); err != nil {
		t.Fatalf("seed the architect's claim: %v", err)
	}
}
