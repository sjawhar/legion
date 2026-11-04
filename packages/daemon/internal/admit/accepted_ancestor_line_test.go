package admit

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
)

// A6's hold, read over the records the line is actually read over. An issue beneath an ancestor
// the daemon has accepted waits at admitted until that ancestor closes: starting it would let
// the ancestor merge a head that does not carry its work, which is the whole of the rule
// release_children and rerun_child refuse under (CHILD_UNDER_ACCEPTED_PARENT) and the
// child-status notice promises a person.
//
// The chain between the issue and that ancestor may hold a child that is already done. Every
// caller of record.Waiting reads the project's live records (record.Store.LiveIssues), which
// leave out a done issue that holds no slot, while the two doors walk the whole tree
// (record.Store.TreeIssues, record.AcceptedAncestorOf) and see it. So the hold has to survive a
// gap in the chain: an ancestor the walk cannot resolve is not an ancestor that is not there.
func TestAnIssueBeneathAnAcceptedAncestorIsHeldThoughTheParentBetweenThemIsDone(t *testing.T) {
	root, between, grandchild := "LEGION-10", "LEGION-11", "LEGION-12"
	parentOfChild, parentOfGrandchild := root, between
	// The root's own reviewed head is accepted and its merge awaited; the grandchild is the
	// issue a person moved to todo, recorded at admitted beneath the done child.
	issues := []record.Issue{
		{Key: root, Tree: root, Project: testProject, Title: root, Phase: phase.AwaitingMerge, Generation: 1,
			Status: "retro", DispatchStatus: "retro", Rank: "A", HandedOver: true,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
		{Key: grandchild, Tree: root, Parent: &parentOfGrandchild, Project: testProject, Title: grandchild,
			Phase: phase.Admitted, Generation: 1, Status: "todo", DispatchStatus: "todo", Rank: "C", HandedOver: true,
			Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets"},
	}
	// The child between them is done and holds no slot, so LiveIssues leaves it out; it is named
	// here only to say what the chain holds.
	_ = record.Issue{Key: between, Tree: root, Parent: &parentOfChild, Phase: phase.Done}

	approved := 1
	facts := record.LineFacts{
		Lifecycles: map[string]treelifecycle.Lifecycle{root: {Tree: root, Authority: treelifecycle.AuthorityWorkflow}},
		Gates: map[string]record.DesignGate{
			root: {Issue: root, LatestVersion: 1, ApprovedVersion: &approved, EverOpened: true,
				ApprovedMark: record.EventMark{Seq: 3, At: fixedNow.Add(-time.Hour)}},
		},
	}

	for _, entry := range record.Waiting(issues, nil, facts) {
		if entry.Issue.Key == grandchild {
			t.Fatalf("%s stands in the waiting line beneath %s, which awaits its merge: a done issue between them is a gap in the chain, not the end of it", grandchild, root)
		}
	}
}
