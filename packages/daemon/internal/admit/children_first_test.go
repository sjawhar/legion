package admit

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The tree every test here drives: a root whose architect has written its spec, and the children
// it filed under that root before the gate opened.
const (
	cfRoot   = "LEGION-1"
	cfFirst  = "LEGION-2"
	cfSecond = "LEGION-3"
)

// seedMirrorChild writes what Dispatch says of a child — its status, its parent and, in order,
// the keys it waits on — as a listing of the project writes it (blockers.Observed). Its sequence
// is 1, as seedMirror's is, so a later event of the same key wins the row's fence.
func seedMirrorChild(t *testing.T, pool *pgxpool.Pool, key, status, parent string, waits ...string) {
	t.Helper()
	inTx(t, pool, func(tx pgx.Tx) {
		row := record.DispatchIssueRow{Key: key, Project: testProject, Status: status, Blockers: waits, Seq: 1, Parent: parent}
		if err := record.NewStore().PutDispatchIssue(context.Background(), tx, row); err != nil {
			t.Fatalf("seed the mirror of %s: %v", key, err)
		}
	})
}

// cfTree seeds cfRoot as a running tree whose architect holds the slot at admitted, its design
// gate registered at version 1 and not yet approved, and returns the admission of this cap.
func cfTree(t *testing.T, pool *pgxpool.Pool, cap int) *Admission {
	t.Helper()
	seedSlotted(t, pool, cfRoot, "A")
	seedMirror(t, pool, cfRoot, "in_progress")
	seedGate(t, pool, record.DesignGate{Issue: cfRoot, ArtifactID: "artifact-1", LatestVersion: 1})
	return quiet(t, cap)
}

// openGate applies a person's approval of the root's spec, the fact that opens its design gate.
func openGate(t *testing.T, pool *pgxpool.Pool, admission *Admission, id string) {
	t.Helper()
	applyFact(t, pool, admission, id, intake.DispatchArtifact{Key: cfRoot, ArtifactID: "artifact-1",
		Kind: intake.DispatchArtifactApproved, Version: 1, Mark: record.EventMark{Seq: 4, At: reviewedAt}})
}

// releaseChild applies the todo event the architect's release_children makes for a child of
// parent, which is how a child enters its tree.
func releaseChild(t *testing.T, pool *pgxpool.Pool, admission *Admission, id, key, parent string, seq int64) {
	t.Helper()
	applyMirrored(t, pool, admission, id, intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: seq,
		Type: "issue.updated", Status: "todo", Title: key, Parent: parent, Rank: key}, admission.engine)
}

// moveChild applies a person's or an architect's move of key to status.
func moveChild(t *testing.T, pool *pgxpool.Pool, admission *Admission, id, key, parent, status string, seq int64) {
	t.Helper()
	applyMirrored(t, pool, admission, id, intake.DispatchIssue{Key: key, ResolvedRepo: "acme/widgets", Seq: seq,
		Type: "issue.updated", Status: status, Title: key, Parent: parent, Rank: key}, admission.engine)
}

// childrenFirstNotices is every children-first notice queued for key, in outbox order.
func childrenFirstNotices(t *testing.T, pool *pgxpool.Pool, key string) []record.Notice {
	t.Helper()
	return gateNotices(t, pool, key, workflow.ChildrenFirstNoticeKind)
}

// blockedNow is what the daemon reports under admission.blocked, read exactly as a controller
// tick notice carries it.
func blockedNow(t *testing.T, pool *pgxpool.Pool, admission *Admission) []record.BlockedIssue {
	t.Helper()
	var blocked []record.BlockedIssue
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if blocked, err = admission.blocked(context.Background(), tx); err != nil {
			t.Fatalf("read what is blocked: %v", err)
		}
	})
	return blocked
}

// A5. A root whose gate opens with two Dispatch children in backlog does its own work after
// theirs: it starts no phase while either is open, it holds no slot while it waits, its
// architect is told once, admission.blocked names it with both children, and it starts planning
// at the next free slot once both have left the workflow. The children are released in the
// architect's design-approved turn, after the gate opened, which is exactly the window the
// tree's own members cannot answer for — neither child is a member of the tree until it is
// released.
func TestARootWaitsForTheChildrenItFiledBeforeItsGateOpened(t *testing.T) {
	pool := migratedPool(t)
	admission := cfTree(t, pool, 3)
	seedMirrorChild(t, pool, cfFirst, "backlog", cfRoot)
	seedMirrorChild(t, pool, cfSecond, "backlog", cfRoot)

	openGate(t, pool, admission, "gate-approved")

	root := issue(t, pool, cfRoot)
	if root.Phase != phase.Admitted {
		t.Fatalf("the root at its gate's opening = %s, want it still at admitted while its children are open", root.Phase)
	}
	assertSlots(t, pool, nil)
	told := childrenFirstNotices(t, pool, cfRoot)
	if len(told) != 1 {
		t.Fatalf("children-first notices = %+v, want exactly one", told)
	}
	for _, want := range []string{cfRoot, cfFirst + " (backlog)", cfSecond + " (backlog)"} {
		if !strings.Contains(told[0].Reason, want) {
			t.Fatalf("the children-first notice %q does not name %q", told[0].Reason, want)
		}
	}
	if want := []record.BlockedIssue{{Issue: cfRoot, BlockedBy: []string{cfFirst, cfSecond}}}; !reflect.DeepEqual(blockedNow(t, pool, admission), want) {
		t.Fatalf("blocked = %#v, want %#v", blockedNow(t, pool, admission), want)
	}

	// The architect's design-approved turn releases the wave. Each child takes a slot of its own
	// and starts planning; the root still waits, and is told nothing more.
	releaseChild(t, pool, admission, "release-first", cfFirst, cfRoot, 5)
	releaseChild(t, pool, admission, "release-second", cfSecond, cfRoot, 6)
	for _, key := range []string{cfFirst, cfSecond} {
		if got := issue(t, pool, key); got.Phase != phase.Planning || got.Tree != cfRoot {
			t.Fatalf("%s after its release = %#v, want it planning in %s's tree", key, got, cfRoot)
		}
	}
	if got := issue(t, pool, cfRoot).Phase; got != phase.Admitted {
		t.Fatalf("the root while its children work = %s, want it still at admitted", got)
	}
	if told := childrenFirstNotices(t, pool, cfRoot); len(told) != 1 {
		t.Fatalf("children-first notices while the wait goes on = %+v, want still exactly one", told)
	}

	// Each child leaves the workflow. The root starts its own planning only once the last has.
	moveChild(t, pool, admission, "park-first", cfFirst, cfRoot, "backlog", 7)
	if got := issue(t, pool, cfRoot).Phase; got != phase.Admitted {
		t.Fatalf("the root with one child still open = %s, want it still at admitted", got)
	}
	moveChild(t, pool, admission, "park-second", cfSecond, cfRoot, "backlog", 8)
	if got := issue(t, pool, cfRoot); got.Phase != phase.Planning {
		t.Fatalf("the root once both children left = %#v, want it planning", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: cfRoot, Index: 0, AdmittedAt: fixedNow}})
}

// A5. A parent below the root waits for its own child exactly as the root does: the released
// child of the root starts, and its own unreleased child holds its first phase while the root's
// wait on it goes on.
func TestAParentBelowTheRootWaitsForItsOwnChild(t *testing.T) {
	pool := migratedPool(t)
	admission := cfTree(t, pool, 3)
	const grandchild = "LEGION-4"
	seedMirrorChild(t, pool, cfFirst, "backlog", cfRoot)
	seedMirrorChild(t, pool, grandchild, "backlog", cfFirst)

	openGate(t, pool, admission, "gate-approved")
	releaseChild(t, pool, admission, "release-first", cfFirst, cfRoot, 5)

	if got := issue(t, pool, cfFirst); got.Phase != phase.Admitted {
		t.Fatalf("the released child with an open child of its own = %#v, want it held at admitted", got)
	}
	assertSlots(t, pool, nil)
	told := childrenFirstNotices(t, pool, cfFirst)
	if len(told) != 1 || !strings.Contains(told[0].Reason, grandchild+" (backlog)") {
		t.Fatalf("the child's children-first notices = %+v, want one naming %s", told, grandchild)
	}

	moveChild(t, pool, admission, "icebox-grandchild", grandchild, cfFirst, "icebox", 6)
	if got := issue(t, pool, cfFirst); got.Phase != phase.Planning {
		t.Fatalf("the child once its own child went to icebox = %#v, want it planning", got)
	}
	if got := issue(t, pool, cfRoot); got.Phase != phase.Admitted {
		t.Fatalf("the root while its child works = %#v, want it still at admitted", got)
	}
}

// A5. A mirror row written before the release — the boot listing's view of a child the daemon
// has never recorded — holds the parent just as a live event's row does: the parent comes from
// that listing, and the wait is complete from it. A parked child, one the tree ran and took out
// of the workflow, holds nothing.
func TestAListedChildHoldsItsParentAndAParkedOneDoesNot(t *testing.T) {
	pool := migratedPool(t)
	admission := cfTree(t, pool, 3)
	const parked = "LEGION-5"
	seedMirrorChild(t, pool, cfFirst, "backlog", cfRoot)
	seedMirrorChild(t, pool, parked, "backlog", cfRoot)
	// The parked child is a member of the root's tree that already left the workflow: the record
	// says so, and the identical backlog row of the mirror must not be read as unreleased work.
	parent := cfRoot
	putIssue(t, pool, record.Issue{Key: parked, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: testProject, Title: parked, Tree: cfRoot, Parent: &parent, Phase: phase.Admitted, Generation: 1,
		Status: "backlog", DispatchStatus: "backlog", Rank: "E", LastDispatchSeq: 1})

	openGate(t, pool, admission, "gate-approved")

	told := childrenFirstNotices(t, pool, cfRoot)
	if len(told) != 1 {
		t.Fatalf("children-first notices = %+v, want exactly one", told)
	}
	if strings.Contains(told[0].Reason, parked) {
		t.Fatalf("the children-first notice %q names the parked child %s, which holds nothing", told[0].Reason, parked)
	}
	if want := []record.BlockedIssue{{Issue: cfRoot, BlockedBy: []string{cfFirst}}}; !reflect.DeepEqual(blockedNow(t, pool, admission), want) {
		t.Fatalf("blocked = %#v, want %#v", blockedNow(t, pool, admission), want)
	}

	// Moving the one unreleased child to icebox ends the wait with no release at all.
	moveChild(t, pool, admission, "icebox-first", cfFirst, cfRoot, "icebox", 6)
	if got := issue(t, pool, cfRoot); got.Phase != phase.Planning {
		t.Fatalf("the root once its unreleased child went to icebox = %#v, want it planning", got)
	}
}

// A5. A child a Dispatch blocked_by link holds against an issue above it on its own parent chain
// is a cycle: the child waits for its parent and the parent waits for the child. The daemon
// names the child, the ancestor and the link, in one fixed sentence, and does nothing else about
// it.
func TestAChildBlockedByItsOwnParentIsNamedAsACycle(t *testing.T) {
	pool := migratedPool(t)
	admission := cfTree(t, pool, 3)
	seedMirrorChild(t, pool, cfFirst, "backlog", cfRoot, cfRoot)

	openGate(t, pool, admission, "gate-approved")

	told := childrenFirstNotices(t, pool, cfRoot)
	if len(told) != 1 {
		t.Fatalf("children-first notices = %+v, want exactly one", told)
	}
	if want := workflow.ChildCycleReason(cfFirst, cfRoot); !strings.Contains(told[0].Reason, want) {
		t.Fatalf("the children-first notice %q does not say %q", told[0].Reason, want)
	}
	want := []record.BlockedIssue{{Issue: cfRoot, BlockedBy: []string{cfFirst}}, {Issue: cfFirst, BlockedBy: []string{cfRoot}}}
	if got := blockedNow(t, pool, admission); !reflect.DeepEqual(got, want) {
		t.Fatalf("blocked = %#v, want the parent named with its open child and the child with the ancestor its link names, %#v", got, want)
	}
	if got := issue(t, pool, cfRoot).Phase; got != phase.Admitted {
		t.Fatalf("the root of a cycle = %s, want it left waiting: breaking the link is a person's decision", got)
	}
}

// A5. A root with no Dispatch children at all takes the next free slot at its gate's opening and
// starts planning, which is the control for every wait above.
func TestARootWithNoChildrenStartsPlanningAtItsGatesOpening(t *testing.T) {
	pool := migratedPool(t)
	admission := cfTree(t, pool, 3)

	openGate(t, pool, admission, "gate-approved")

	if got := issue(t, pool, cfRoot); got.Phase != phase.Planning {
		t.Fatalf("a childless root at its gate's opening = %#v, want it planning", got)
	}
	assertSlots(t, pool, []record.Slot{{Issue: cfRoot, Index: 0, AdmittedAt: fixedNow}})
	if told := childrenFirstNotices(t, pool, cfRoot); len(told) != 0 {
		t.Fatalf("children-first notices = %+v, want none: nothing hangs under it", told)
	}
}

// A5. One children-first notice per parent per run: every fact, tick and boot runs the admission
// pass again, and a child filed later during one wait is no second telling, while a run the tree
// has not had yet — the one a re-admission opens, and the one a hand-over gives a sub-parent in a
// tree of its own — is.
func TestTheChildrenFirstNoticeIsToldOncePerIssueAndGeneration(t *testing.T) {
	pool := migratedPool(t)
	admission := cfTree(t, pool, 3)
	seedMirrorChild(t, pool, cfFirst, "backlog", cfRoot)

	openGate(t, pool, admission, "gate-approved")
	for i, id := range []string{"tick-1", "tick-2", "tick-3"} {
		tick(t, pool, admission, id)
		if told := childrenFirstNotices(t, pool, cfRoot); len(told) != 1 {
			t.Fatalf("children-first notices after tick %d = %+v, want still exactly one", i+1, told)
		}
	}
	// A child filed during the wait is named by no second notice: the architect already knows
	// the root waits, and admission.blocked names what for.
	seedMirrorChild(t, pool, cfSecond, "backlog", cfRoot)
	tick(t, pool, admission, "tick-4")
	if told := childrenFirstNotices(t, pool, cfRoot); len(told) != 1 {
		t.Fatalf("children-first notices after a second child was filed = %+v, want still exactly one", told)
	}
	if want := []record.BlockedIssue{{Issue: cfRoot, BlockedBy: []string{cfFirst, cfSecond}}}; !reflect.DeepEqual(blockedNow(t, pool, admission), want) {
		t.Fatalf("blocked = %#v, want both children named, %#v", blockedNow(t, pool, admission), want)
	}

	// The telling is per run, so the mark of this one says nothing about the next.
	var told bool
	inTx(t, pool, func(tx pgx.Tx) {
		var err error
		if told, err = record.NewStore().TellChildrenFirst(context.Background(), tx, cfRoot, cfRoot, issue(t, pool, cfRoot).Generation+1, fixedNow); err != nil {
			t.Fatalf("mark the next generation told: %v", err)
		}
	})
	if !told {
		t.Fatalf("the next generation's mark was already taken, want each run told on its own")
	}

	// A sub-parent waits the same way, and the run its mark names is its tree's. The root
	// releases its first child, which has a child of its own still open.
	const grandchild = "LEGION-4"
	seedMirrorChild(t, pool, grandchild, "backlog", cfFirst)
	releaseChild(t, pool, admission, "release-first", cfFirst, cfRoot, 9)
	if told := childrenFirstNotices(t, pool, cfFirst); len(told) != 1 {
		t.Fatalf("the sub-parent's children-first notices = %+v, want exactly one", told)
	}

	// Its tree is re-admitted, which raises the root's generation and leaves every member's
	// where it stands: the new run's architect has heard nothing, so the wait is told again
	// though nothing of the sub-parent's own record changed.
	readmitted := issue(t, pool, cfRoot).Generation + 1
	if _, err := pool.Exec(context.Background(), "update issues set generation = $2 where key = $1", cfRoot, int64(readmitted)); err != nil {
		t.Fatalf("open the tree's next run: %v", err)
	}
	tick(t, pool, admission, "tick-5")
	if told := childrenFirstNotices(t, pool, cfFirst); len(told) != 2 {
		t.Fatalf("the sub-parent's children-first notices after its tree's re-admission = %+v, want a second: this run's architect has not heard what holds it", told)
	}

	// The tree finishes and hands the sub-parent over as a root of its own, whose run is
	// numbered from its own record (readmit), which here is the number the tree it left is on.
	// Keyed on the generation alone the old tree's mark would answer for this tree's first run
	// and its architect would never be told. Its own architect has written that tree's spec and a
	// person has approved it, which is what puts a root's own children-first wait in the line at
	// all (record.ChildrenFirstCandidates).
	handedOver := issue(t, pool, cfFirst).Generation + 1
	if handedOver != readmitted {
		t.Fatalf("the handed-over sub-parent's run = %d and the tree it left is on %d, want the fixture's own premise: the two numbers collide", handedOver, readmitted)
	}
	if _, err := pool.Exec(context.Background(), "update issues set tree = key, generation = $2, handed_over_from = $3 where key = $1",
		cfFirst, int64(handedOver), cfRoot); err != nil {
		t.Fatalf("hand the sub-parent over as a root of its own: %v", err)
	}
	seedGate(t, pool, record.DesignGate{Issue: cfFirst, ArtifactID: "artifact-2", LatestVersion: 1, ApprovedVersion: new(1), EverOpened: true})
	tick(t, pool, admission, "tick-6")
	if told := childrenFirstNotices(t, pool, cfFirst); len(told) != 3 {
		t.Fatalf("the children-first notices of the handed-over sub-parent = %+v, want a third: the run of its own tree is one no architect has been told of", told)
	}
}

// A5. The waiting line itself never offers a slot to a parent whose children are open, whatever
// else is in the line: the rule is read where the line is read, from the pass's own judgement,
// and never from a stored flag.
func TestTheWaitingLineLeavesOutAParentWhoseChildrenAreOpen(t *testing.T) {
	pool := migratedPool(t)
	admission := cfTree(t, pool, 3)
	seedMirrorChild(t, pool, cfFirst, "backlog", cfRoot)
	// A root of its own that an unfinished Dispatch issue holds stays in the line and is passed
	// over at promotion, so the line here is never empty for want of candidates: what it must
	// not hold is the parent.
	seedWaiting(t, pool, "LEGION-900", "Z")
	seedMirror(t, pool, "LEGION-900", "todo", "LEGION-BLOCKER")
	seedMirror(t, pool, "LEGION-BLOCKER", "in_progress")

	openGate(t, pool, admission, "gate-approved")

	assertWaiting(t, pool, []string{"LEGION-900"})
	inTx(t, pool, func(tx pgx.Tx) {
		issues, err := record.NewStore().LiveIssues(context.Background(), tx, testProject)
		if err != nil {
			t.Fatalf("list the issues: %v", err)
		}
		facts, err := record.ReadLineFacts(context.Background(), tx, record.NewStore(), testProject, issues, nil)
		if err != nil {
			t.Fatalf("read the line facts: %v", err)
		}
		waits, err := record.ChildWaitsOf(context.Background(), tx, record.NewStore(), testProject, issues, facts)
		if err != nil {
			t.Fatalf("judge the child waits: %v", err)
		}
		if wait, waiting := waits[cfRoot]; !waiting || len(wait.Open) != 1 || wait.Open[0].Key != cfFirst || wait.Open[0].Status != "backlog" {
			t.Fatalf("the judgement of %s = %+v, want it waiting on %s in backlog", cfRoot, waits[cfRoot], cfFirst)
		}
	})
}
