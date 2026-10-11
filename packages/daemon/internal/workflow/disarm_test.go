package workflow

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A READY names the head the merger arms with --match-head-commit, and GitHub checks that head
// only when auto-merge is enabled: a later push by anyone with write access leaves it armed for a
// head nobody reviewed. So a push that moves an awaiting_merge pull request past its READY head
// queues a disarm of that pull request. The READY head arriving again, or a push outside
// awaiting_merge, queues none.
func TestAPushPastTheREADYHeadDisarmsTheMerge(t *testing.T) {
	for _, tc := range []struct {
		name   string
		at     phase.Phase
		status string
		head   string
		want   []record.AutoMergeDisable
	}{
		{"a new head in awaiting_merge", phase.AwaitingMerge, "retro", "moved", []record.AutoMergeDisable{{Number: 42}}},
		{"the READY head again", phase.AwaitingMerge, "retro", "head", nil},
		{"a new head in reviewing", phase.Reviewing, "needs_review", "moved", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.at, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
			applyRefusingNothing(t, pool, readyEngine(""),
				intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: tc.head})
			if got := autoMergeDisables(t, pool); !slices.Equal(got, tc.want) {
				t.Fatalf("auto-merge disables %v, want %v", got, tc.want)
			}
		})
	}
}

// A READY withdrawn because the head's own CI turned red, or because the head started conflicting
// with its base, sends the issue back to implementing: an auto-merge armed for that READY must not
// outlive it, so the withdrawal queues a disarm too.
func TestAWithdrawnREADYDisarmsTheMerge(t *testing.T) {
	const required = "pr-checks-result"
	for _, tc := range []struct {
		name string
		fact intake.Fact
	}{
		{"red CI", intake.PullRequestChecks{Repo: "sjawhar/legion", Number: 42, HeadSHA: "head",
			CheckRuns: []record.AttemptRun{{Name: required, ID: 1}}, Generation: 1, Snapshot: "red", Failing: []string{required}}},
		{"a conflicting head", intake.PullRequestMergeability{Repo: "sjawhar/legion", Number: 42, Base: "main", Mergeable: record.MergeabilityConflicting}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head", Required: []string{required}})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
			applyRefusingNothing(t, pool, readyEngine(""), tc.fact)
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the withdrawal the issue is in %s, want implementing", got)
			}
			if got, want := autoMergeDisables(t, pool), []record.AutoMergeDisable{{Number: 42}}; !slices.Equal(got, want) {
				t.Fatalf("auto-merge disables %v, want %v", got, want)
			}
		})
	}
}

// autoMergeDisables is every auto-merge disable the outbox holds, in order.
func autoMergeDisables(t *testing.T, pool *pgxpool.Pool) []record.AutoMergeDisable {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select (payload->>'number')::int from outbox where kind = 'auto_merge_disable' order by id")
	if err != nil {
		t.Fatalf("read auto-merge disables: %v", err)
	}
	defer rows.Close()
	var disables []record.AutoMergeDisable
	for rows.Next() {
		var disable record.AutoMergeDisable
		if err := rows.Scan(&disable.Number); err != nil {
			t.Fatalf("scan auto-merge disable: %v", err)
		}
		disables = append(disables, disable)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate auto-merge disables: %v", err)
	}
	return disables
}
