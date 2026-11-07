package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// One departure can end the wait of more than one merged issue above it, and closing the nearest
// one is itself a departure that may end the next one's. A chain of merged issues therefore
// closes whole when the last issue beneath it leaves, rather than one step per fact: an issue
// left in awaiting_merge is in the workflow, holds no admission slot, stands in neither part of
// the waiting line, and refuses its tree root's sign_off for as long as it stands there.
func TestAChainOfMergedIssuesClosesWhenTheLastIssueBeneathItLeaves(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, child, grandchild, leaf := "LEGION-208", "LEGION-209", "LEGION-210", "LEGION-211"
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress",
		Rank: "U", LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: root, Index: 0})
	rootKey, childKey, grandchildKey := root, child, grandchild
	for _, merged := range []struct {
		key    string
		parent *string
		number int
	}{{child, &rootKey, 43}, {grandchild, &childKey, 44}} {
		seedIssue(t, pool, record.Issue{Key: merged.key, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
			Tree: root, Parent: merged.parent, Project: "LEGION", Title: merged.key, Phase: phase.AwaitingMerge,
			Generation: 3, Status: "retro", Rank: "V" + merged.key, LastDispatchSeq: 1})
		seedPR(t, pool, record.PullRequest{State: record.PullRequestMerged, Issue: merged.key, Repo: "sjawhar/legion",
			Number: merged.number, Branch: "legion/" + merged.key, HeadSHA: "head", MergeSHA: "merge", Failing: []string{}})
	}
	seedIssue(t, pool, record.Issue{Key: leaf, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &grandchildKey, Project: "LEGION", Title: "leaf", Phase: phase.Implementing, Generation: 3,
		Status: "in_progress", Rank: "V3", LastDispatchSeq: 1})

	applyFacts(t, pool, engine)("leaf-done", intake.DispatchIssue{Key: leaf, ResolvedRepo: "sjawhar/legion", Seq: 2,
		Type: "issue.closed", Status: "done", Title: "leaf", Parent: grandchild, Rank: "V3"})

	if at := issueAt(t, pool, leaf); at != phase.Done {
		t.Fatalf("%s stands at %s, want done: the fixture's own premise", leaf, at)
	}
	if at := issueAt(t, pool, grandchild); at != phase.Done {
		t.Fatalf("%s stands at %s, want done: the issue beneath it left", grandchild, at)
	}
	if at := issueAt(t, pool, child); at != phase.Done {
		t.Fatalf("%s stands at %s, want done: closing %s was itself a departure, and %s waited for nothing else",
			child, at, grandchild, child)
	}
}
