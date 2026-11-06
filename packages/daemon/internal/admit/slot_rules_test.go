package admit

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// slotTree is the running tree every slot-rule case stands in: its root LEGION-1, its lifecycle
// open under the workflow, and its design gate registered. approved opens that gate, which is
// what takes the slot away from the root itself, and leaves the root in accepting, where a
// parent whose own phases are behind it waits: at admitted with an open gate the root's own
// first phase can start, and a pass with a slot to spare would start it beside the member each
// case is about.
func slotTree(t *testing.T, pool *pgxpool.Pool, approved bool) {
	t.Helper()
	stands, status := phase.Admitted, "in_progress"
	gate := record.DesignGate{Issue: "LEGION-1", ArtifactID: "7d1e3a5c-9b2f-4c6e-8a40-3f5d7b9e1c26", LatestVersion: 1}
	if approved {
		stands, status = phase.Accepting, "needs_review"
		gate.ApprovedVersion = new(1)
		gate.ApprovedMark = record.EventMark{Seq: 4, At: fixedNow.Add(-time.Hour)}
	}
	putIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: "LEGION-1", Tree: "LEGION-1", Phase: stands, Generation: 1, Status: status, DispatchStatus: status, Rank: "A", HandedOver: true})
	seedLifecycle(t, pool, "LEGION-1")
	inTx(t, pool, func(tx pgx.Tx) {
		if err := record.NewStore().PutGate(context.Background(), tx, gate); err != nil {
			t.Fatalf("seed gate: %v", err)
		}
	})
}

// slotMember records key in LEGION-1's tree at stands, with the Dispatch status a member in that
// phase shows, and a working claim of the phase's own role, so a pass that moved a claim or a
// status would be seen.
func slotMember(t *testing.T, pool *pgxpool.Pool, key string, stands phase.Phase, status string, ready *int) {
	t.Helper()
	parent := "LEGION-1"
	var pending *record.ReadyPending
	if ready != nil {
		pending = &record.ReadyPending{Step: record.DesignStepSpec, Version: *ready}
	}
	putIssue(t, pool, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: key, Tree: "LEGION-1", Parent: &parent, Phase: stands, Generation: 1, Status: status, DispatchStatus: status, Rank: "B",
		ReadyPending: pending, LastDispatchSeq: 3})
	role := workflow.RoleFor(stands)
	if role == "" || role == claim.RoleArchitect {
		role = claim.RoleImplementer
	}
	token, err := claim.NewToken("legion", key, role)
	if err != nil {
		t.Fatalf("claim token: %v", err)
	}
	inTx(t, pool, func(tx pgx.Tx) {
		if _, err := tx.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session, session_file, state,
			launch_failures, prompt_failures, prompt_retires, uncertain_streak)
			values ($1, 'legion', 'LEGION-1', $2, $3, 1, 'ses-worker', '/tmp/worker.jsonl', 'working', 0, 0, 0, 0)`, string(token), key, string(role)); err != nil {
			t.Fatalf("put claim: %v", err)
		}
	})
}

// takeSlot gives key one of the cap's slots, as a promotion would.
func takeSlot(t *testing.T, pool *pgxpool.Pool, key string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		records := record.NewStore()
		held, err := records.Slots(context.Background(), tx)
		if err != nil {
			t.Fatalf("list slots: %v", err)
		}
		if err := records.PutSlot(context.Background(), tx, record.Slot{Issue: key, Index: len(held), AdmittedAt: fixedNow}); err != nil {
			t.Fatalf("put slot: %v", err)
		}
	})
}

// oweStart queues the start of key's worker as a transition does (workflow.StartOrOwe): the
// relaunch row, and the task row the outbox holds unclaimable until key holds a slot. It is what
// puts an issue whose work came back in the waiting line.
func oweStart(t *testing.T, pool *pgxpool.Pool, key string, stands phase.Phase) {
	t.Helper()
	role := workflow.RoleFor(stands)
	inTx(t, pool, func(tx pgx.Tx) {
		if err := workflow.StartOrOwe(context.Background(), tx, record.NewStore(), key, record.SuperviseRequest{Op: "start", Tree: "LEGION-1",
			Role: role, Generation: 1, Phase: stands, Task: "Continue " + key + ".", Assignment: "assignment-" + key}, fixedNow); err != nil {
			t.Fatalf("owe %s a start: %v", key, err)
		}
	})
}

// holdsSlot says whether key holds one of the project's admission slots.
func holdsSlot(t *testing.T, pool *pgxpool.Pool, key string) bool {
	t.Helper()
	for _, slot := range slots(t, pool) {
		if slot.Issue == key {
			return true
		}
	}
	return false
}

// claimStates is the state of every claim in the store, by token.
func claimStates(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select token, state from claims`)
	if err != nil {
		t.Fatalf("list claims: %v", err)
	}
	defer rows.Close()
	states := map[string]string{}
	for rows.Next() {
		var token, state string
		if err := rows.Scan(&token, &state); err != nil {
			t.Fatalf("scan claim: %v", err)
		}
		states[token] = state
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate claims: %v", err)
	}
	return states
}

// dispatchStatuses is the status of every row of the Dispatch mirror, by key.
func dispatchStatuses(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `select key, status from dispatch_issues`)
	if err != nil {
		t.Fatalf("list the mirror: %v", err)
	}
	defer rows.Close()
	statuses := map[string]string{}
	for rows.Next() {
		var key, status string
		if err := rows.Scan(&key, &status); err != nil {
			t.Fatalf("scan mirror row: %v", err)
		}
		statuses[key] = status
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the mirror: %v", err)
	}
	return statuses
}

// A1. One slot rule per state, each read by one admission pass over a tree that runs.
//
// An issue in a state the spec's first list names — planning, implementing, testing, reviewing,
// retro, merging with no READY the gate holds, and a root at admitted whose gate has never
// opened — holds a slot as soon as one is free. An issue in a state of the second list — merging
// with a READY the closed gate holds, a child at admitted, a root at admitted after its gate
// opened, accepting, awaiting_merge, held, awaiting_person and done — holds none after one pass,
// and neither does any issue of a tree that lingers.
//
// The slot is accounting and nothing else: the pass suspends no claim, moves no phase and writes
// no Dispatch status, which the three unchanged reads below pin.
//
// Each state of the second list is read under a cap of none, which is the release rule on its
// own: with a slot to spare the same pass would go on to start whatever of that tree can start
// next, and the question here is which state holds a slot, not what the line does afterwards.
func TestSlotPerState(t *testing.T) {
	const member = "LEGION-2"
	for _, tc := range []struct {
		name  string
		seed  func(t *testing.T, pool *pgxpool.Pool)
		issue string
		// wants says the issue must hold a slot after the pass. Each case starts in the state it
		// names, and the pass is the whole of the rule.
		wants bool
	}{
		{name: "planning", issue: member, wants: true, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Planning, "in_progress", nil)
			oweStart(t, pool, member, phase.Planning)
		}},
		{name: "implementing", issue: member, wants: true, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Implementing, "in_progress", nil)
			oweStart(t, pool, member, phase.Implementing)
		}},
		{name: "testing", issue: member, wants: true, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Testing, "testing", nil)
			oweStart(t, pool, member, phase.Testing)
		}},
		{name: "reviewing", issue: member, wants: true, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Reviewing, "needs_review", nil)
			oweStart(t, pool, member, phase.Reviewing)
		}},
		{name: "retro", issue: member, wants: true, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Retro, "retro", nil)
			oweStart(t, pool, member, phase.Retro)
		}},
		{name: "merging with no pending READY", issue: member, wants: true, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Merging, "retro", nil)
			oweStart(t, pool, member, phase.Merging)
		}},
		{name: "a root at admitted whose gate never opened", issue: "LEGION-9", wants: true, seed: func(t *testing.T, pool *pgxpool.Pool) {
			seedWaiting(t, pool, "LEGION-9", "A")
		}},
		{name: "merging with a READY the gate holds", issue: member, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Merging, "retro", new(2))
			takeSlot(t, pool, member)
		}},
		{name: "a child at admitted", issue: member, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, false)
			slotMember(t, pool, member, phase.Admitted, "todo", nil)
			takeSlot(t, pool, member)
		}},
		{name: "a root at admitted after its gate opened", issue: "LEGION-1", seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			root := issue(t, pool, "LEGION-1")
			root.Phase, root.Status = phase.Admitted, "in_progress"
			putIssue(t, pool, root)
			takeSlot(t, pool, "LEGION-1")
		}},
		{name: "accepting", issue: member, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Accepting, "needs_review", nil)
			takeSlot(t, pool, member)
		}},
		{name: "awaiting_merge", issue: member, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.AwaitingMerge, "retro", nil)
			takeSlot(t, pool, member)
		}},
		{name: "held", issue: member, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Held, "in_progress", nil)
			takeSlot(t, pool, member)
		}},
		{name: "done", issue: member, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Done, "done", nil)
			takeSlot(t, pool, member)
		}},
		{name: "an issue of a lingering tree", issue: member, seed: func(t *testing.T, pool *pgxpool.Pool) {
			slotTree(t, pool, true)
			slotMember(t, pool, member, phase.Implementing, "in_progress", nil)
			takeSlot(t, pool, member)
			root := issue(t, pool, "LEGION-1")
			until := fixedNow.Add(time.Hour)
			root.Phase, root.Status, root.LingerUntil = phase.Done, "done", &until
			putIssue(t, pool, root)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			// A state that needs a slot is read with slots to spare; one that needs none is
			// read with no cap at all, so the pass releases and promotes nothing.
			cap := 0
			if tc.wants {
				cap = 4
			}
			admission := quiet(t, cap)
			tc.seed(t, pool)
			phases := map[string]phase.Phase{}
			for _, key := range []string{"LEGION-1", member, "LEGION-9"} {
				if got := maybeIssue(t, pool, key); got != nil {
					phases[key] = got.Phase
				}
			}
			claims, statuses := claimStates(t, pool), dispatchStatuses(t, pool)

			applyFact(t, pool, admission, "slot-rule", intake.ControllerTick{})

			if held := holdsSlot(t, pool, tc.issue); held != tc.wants {
				t.Fatalf("%s holds a slot = %t, want %t; slots = %#v", tc.issue, held, tc.wants, slots(t, pool))
			}
			for key, stood := range phases {
				if got := issue(t, pool, key); got.Phase != stood {
					t.Fatalf("%s moved from %s to %s, want the slot rule to move no phase", key, stood, got.Phase)
				}
			}
			for token, state := range claimStates(t, pool) {
				if state != claims[token] {
					t.Fatalf("claim %s = %s, was %s; want the slot rule to suspend nothing", token, state, claims[token])
				}
			}
			for key, status := range dispatchStatuses(t, pool) {
				if status != statuses[key] {
					t.Fatalf("the mirror row of %s = %s, was %s; want the slot rule to write no Dispatch status", key, status, statuses[key])
				}
			}
		})
	}
}

// A1's awaiting_person case, which only a root reaches: a root enters that wait while it holds a
// slot (workflow's decideAwait), and the pass that records the wait takes the slot back.
func TestARootWaitingOnAPersonHoldsNoSlot(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 2)
	putIssue(t, pool, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
		Title: "LEGION-1", Tree: "LEGION-1", Phase: phase.AwaitingPerson, Generation: 1, Status: "in_progress", DispatchStatus: "in_progress",
		Rank: "A", HandedOver: true, Awaiting: &record.Awaiting{Ask: "ask-q", Since: record.EventMark{Seq: 9}}})
	seedLifecycle(t, pool, "LEGION-1")
	takeSlot(t, pool, "LEGION-1")

	applyFact(t, pool, admission, "tick", intake.ControllerTick{})

	if holdsSlot(t, pool, "LEGION-1") {
		t.Fatalf("slots = %#v, want none: a root waiting on a person holds no slot", slots(t, pool))
	}
	if got := issue(t, pool, "LEGION-1"); got.Phase != phase.AwaitingPerson || got.Answered() {
		t.Fatalf("root = %#v, want it still waiting on its person", got)
	}
}

// A2. The cap counts issues, so one tree holds as many slots as it has issues at work: three
// while its three children work, and four once the root works too.
//
// The root stands in accepting while its children work, which is where a parent whose own
// phases are behind it waits: it holds no slot there, so the three the tree holds are its
// children's, one each at its first phase. The root's own implementing then takes a fourth.
func TestATreeHoldsOneSlotForEachIssueAtWork(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 8)
	slotTree(t, pool, true)
	root := issue(t, pool, "LEGION-1")
	root.Phase, root.Status = phase.Accepting, "needs_review"
	putIssue(t, pool, root)
	children := []string{"LEGION-2", "LEGION-3", "LEGION-4"}
	for i, child := range children {
		parent := "LEGION-1"
		putIssue(t, pool, record.Issue{Key: child, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: testProject,
			Title: child, Tree: "LEGION-1", Parent: &parent, Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo",
			Rank: string(rune('B' + i)), LastDispatchSeq: 2})
	}

	applyFact(t, pool, admission, "released", intake.ControllerTick{})

	for _, child := range children {
		if got := issue(t, pool, child); got.Phase != phase.Planning {
			t.Fatalf("%s = %#v, want it planning at its own slot", child, got)
		}
	}
	if got := slots(t, pool); len(got) != 3 {
		t.Fatalf("slots = %#v, want three: one for each child at work, and none for the root, which waits", got)
	}
	if holdsSlot(t, pool, "LEGION-1") {
		t.Fatalf("slots = %#v, want none for the root: it waits in accepting while its children work", slots(t, pool))
	}

	// The root's own first phase begins: a fourth issue of the tree is at work, so the tree
	// holds a fourth slot.
	root = issue(t, pool, "LEGION-1")
	root.Phase, root.Status = phase.Implementing, "in_progress"
	putIssue(t, pool, root)
	oweStart(t, pool, "LEGION-1", phase.Implementing)

	applyFact(t, pool, admission, "root-implementing", intake.ControllerTick{})

	if got := slots(t, pool); len(got) != 4 {
		t.Fatalf("slots = %#v, want four: the three children and the root", got)
	}
	if !holdsSlot(t, pool, "LEGION-1") {
		t.Fatalf("slots = %#v, want one for the root now that it works", slots(t, pool))
	}
}
