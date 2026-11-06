package admit

import (
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A4's last clause, for a root that arrived from another tree: "a person's todo on a running root
// that holds no slot does not put it in the line as a root that never ran."
//
// A handed-over root holds no slot once it is running and waiting for its merge, so a person who
// moves it to todo on the dashboard leaves a record that reads, by status alone, exactly like one
// that has still to be promoted. The hand-over mark used to say the tree had never been opened for
// the whole life of that tree, so the line took it as a root that never ran and promoted it again:
// a second in_progress write, a second architect start, a new issue branch, and a resume of every
// mid-phase member, whose ResumePhase moves awaiting_merge to merging — where the merger's claim is
// live, no task is queued, and the pull request's own merge no longer closes the issue.
//
// The promotion clears the mark, so the record says the tree is open and this is a running root.
func TestAPersonsTodoOnARunningHandedOverRootDoesNotPromoteItAgain(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)

	// The handed-over root as its own promotion left it: the tree opened, the phase it carried
	// from the tree it left, and no slot, since a worker of it has no work in awaiting_merge.
	const root = "LEGION-300"
	putIssue(t, pool, record.Issue{Key: root, Tree: root, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a carried root that ran", Phase: phase.AwaitingMerge,
		Generation: 2, Status: "todo", Rank: "A", HandedOver: true})
	seedLifecycle(t, pool, root)
	seedMirror(t, pool, root, "todo")

	assertWaiting(t, pool, nil)
	tick(t, pool, admission, "tick")

	if running := issue(t, pool, root); running.Phase != phase.AwaitingMerge {
		t.Fatalf("%s stands in %s after the pass, want awaiting_merge: a running root a person set to todo is promoted by nothing", root, running.Phase)
	}
	if holders := slots(t, pool); len(holders) != 0 {
		t.Fatalf("slots = %#v, want none: a root waiting for its merge has no worker with work", holders)
	}
	if started := startsOf(t, pool, root); len(started) != 0 {
		t.Fatalf("%d supervise rows were queued for %s, want none: nothing of a running root is started again", len(started), root)
	}
}

// The mark the promotion clears is the record's own, so the root that has still to be promoted is
// still read as one: the same record, with no tree lifecycle open, stands in the line as a root
// whose tree has not been opened and is promoted once.
func TestAHandedOverRootWhoseTreeIsNotOpenIsStillPromoted(t *testing.T) {
	pool := migratedPool(t)
	admission := quiet(t, 4)

	const root = "LEGION-301"
	putIssue(t, pool, record.Issue{Key: root, Tree: root, Repo: "acme/widgets", DefaultBranch: "main",
		GitHubRepo: "acme/widgets", Project: testProject, Title: "a carried root to promote", Phase: phase.Accepting,
		Generation: 2, Status: "todo", Rank: "A", HandedOver: true, HandedOverFrom: "LEGION-700"})
	seedMirror(t, pool, root, "todo")

	assertWaiting(t, pool, []string{root})
	tick(t, pool, admission, "tick")

	promoted := issue(t, pool, root)
	if promoted.Status != "in_progress" {
		t.Fatalf("%s is %q after its promotion, want in_progress", root, promoted.Status)
	}
	if promoted.HandedOverFrom != "" {
		t.Fatalf("%s still carries the hand-over mark %q: its tree is open, so the run its record carries is this tree's own", root, promoted.HandedOverFrom)
	}
	assertWaiting(t, pool, nil)
}
