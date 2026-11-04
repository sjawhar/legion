package workflow

import (
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A merged issue's close waits for the issues beneath it and happens once the last of them
// leaves. The wait is no longer a tree root's alone: openBeneath reads record.Beneath, the
// descendants of any issue, where the rule it replaced (openBeside) returned nothing for a
// non-root. So an intermediate parent can now stand in awaiting_merge waiting for a grandchild,
// and what must follow is the close when that grandchild goes.
//
// It matters beyond the one issue: an issue in awaiting_merge is in the workflow, so while it
// stands there its tree's root cannot be accepted (SIGNOFF_CHILD_IN_WORKFLOW) or closed, and the
// issue itself holds no admission slot and stands in no part of the waiting line, so nothing
// else brings it back.
func TestAMergedChildClosesWhenTheIssueBeneathItLeaves(t *testing.T) {
	pool := migratedPool(t)
	now := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "LEGION", Linger: time.Hour, ProductionCheckBound: 48 * time.Hour,
		Clock: func() time.Time { return now }}, nil)
	root, child, grandchild := "LEGION-208", "LEGION-209", "LEGION-210"
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 7, Status: "in_progress",
		Rank: "U", LastDispatchSeq: 1})
	seedSlot(t, pool, record.Slot{Issue: root, Index: 0})
	rootKey, childKey := root, child
	seedIssue(t, pool, record.Issue{Key: child, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &rootKey, Project: "LEGION", Title: "child", Phase: phase.AwaitingMerge, Generation: 3,
		Status: "retro", Rank: "V1", LastDispatchSeq: 1})
	seedIssue(t, pool, record.Issue{Key: grandchild, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion",
		Tree: root, Parent: &childKey, Project: "LEGION", Title: "grandchild", Phase: phase.Implementing, Generation: 3,
		Status: "in_progress", Rank: "V2", LastDispatchSeq: 1})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: child, Repo: "sjawhar/legion", Number: 43,
		Branch: "legion/LEGION-209", HeadSHA: "head", Failing: []string{}})

	apply := applyFacts(t, pool, engine)
	apply("merged", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 43, MergeSHA: "merge"})
	if at := issueAt(t, pool, child); at != phase.AwaitingMerge {
		t.Fatalf("%s stands at %s after its merge, want awaiting_merge while %s is still in the workflow", child, at, grandchild)
	}

	// The grandchild leaves, as a person's move of it to done does.
	apply("grandchild-done", intake.DispatchIssue{Key: grandchild, ResolvedRepo: "sjawhar/legion", Seq: 2,
		Type: "issue.closed", Status: "done", Title: "grandchild", Parent: child, Rank: "V2"})
	if at := issueAt(t, pool, grandchild); at != phase.Done {
		t.Fatalf("%s stands at %s, want done: the fixture's own premise", grandchild, at)
	}
	if at := issueAt(t, pool, child); at != phase.Done {
		t.Fatalf("%s stands at %s after the last issue beneath it left, want done: a merged issue closes when the last issue beneath it leaves, and while it stands in awaiting_merge its root can be neither accepted nor closed", child, at)
	}
}
