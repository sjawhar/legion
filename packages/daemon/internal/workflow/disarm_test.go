package workflow

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A READY names the head the merger arms with --match-head-commit, and GitHub checks that head
// only when auto-merge is enabled: a later push by anyone with write access leaves it armed for a
// head nobody reviewed. Arming is legitimate only for the READY head of an awaiting_merge pull
// request, so every head move but a push landing on that head queues a disarm. In awaiting_merge
// the move withdraws the READY as a red or a conflict does: the issue goes back to implementing
// with its implementer started, and the architect hears a checks-red naming the move, so the new
// head comes back through testing, review and a new READY. Elsewhere the issue stays where it is.
func TestAPushPastTheREADYHeadWithdrawsTheREADY(t *testing.T) {
	for _, tc := range []struct {
		name                string
		at                  phase.Phase
		status              string
		head                string
		disarmed, withdrawn bool
	}{
		{"a new head in awaiting_merge", phase.AwaitingMerge, "retro", "moved", true, true},
		{"the READY head again", phase.AwaitingMerge, "retro", "head", false, false},
		{"a new head in reviewing", phase.Reviewing, "needs_review", "moved", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-208", Project: "LEGION", Title: "root",
				Phase: tc.at, Generation: 1, Status: tc.status, Rank: "U"})
			seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
				Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleImplementer, Claim: "implement-claim"})
			seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
			applyRefusingNothing(t, pool, readyEngine(""),
				intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: tc.head})
			var want []record.AutoMergeDisable
			if tc.disarmed {
				want = []record.AutoMergeDisable{{Number: 42}}
			}
			if got := autoMergeDisables(t, pool); !slices.Equal(got, want) {
				t.Fatalf("auto-merge disables %v, want %v", got, want)
			}
			if !tc.withdrawn {
				if got := issuePhase(t, pool); got != tc.at {
					t.Fatalf("the issue is in %s, want it left in %s", got, tc.at)
				}
				return
			}
			if got := issuePhase(t, pool); got != phase.Implementing {
				t.Fatalf("after the head moved the issue is in %s, want implementing", got)
			}
			if task := implementerTask(t, pool); !strings.Contains(task, "moved") {
				t.Fatalf("implementer task %q, want it started naming the moved head", task)
			}
			if got := noticeKinds(t, pool, "LEGION-208"); !containsNotice(got, "checks-red") {
				t.Fatalf("notices %v, want the architect told checks-red", got)
			}
		})
	}
}

// A person setting an awaiting_merge child back to todo re-enters it in a new run, with its pull
// request still open and, until something disarms it, still armed for the old READY. The re-entry
// disarms it, and so does every push after, since no head of the new run is an accepted READY.
func TestAReenteredChildsPullRequestIsDisarmedAndStaysDisarmedOnPush(t *testing.T) {
	pool := migratedPool(t)
	ctx := context.Background()
	seedIssue(t, pool, record.Issue{Key: "LEGION-207", Tree: "LEGION-207", Project: "LEGION", Title: "root", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "U", LastDispatchSeq: 3})
	seedSlot(t, pool, record.Slot{Issue: "LEGION-207", Index: 0, AdmittedAt: time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)})
	parent := "LEGION-207"
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-207", Parent: &parent, Project: "LEGION", Title: "child",
		Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "V", LastDispatchSeq: 4})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
	engine := readyEngine("")
	if _, err := intake.ApplyFact(ctx, pool, "dispatch", "child-back-to-todo", intake.DispatchIssue{
		Key: "LEGION-208", Seq: 5, Type: "issue.updated", Status: "todo", Title: "child", Parent: "LEGION-207", Rank: "V",
	}, engine, admissionStub{}); err != nil {
		t.Fatalf("ApplyFact child todo: %v", err)
	}
	if got, want := autoMergeDisables(t, pool), []record.AutoMergeDisable{{Number: 42}}; !slices.Equal(got, want) {
		t.Fatalf("auto-merge disables after the re-entry %v, want %v", got, want)
	}
	applyRefusingNothing(t, pool, engine,
		intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "moved"})
	if got, want := autoMergeDisables(t, pool), []record.AutoMergeDisable{{Number: 42}, {Number: 42}}; !slices.Equal(got, want) {
		t.Fatalf("auto-merge disables after a push to the re-entered child %v, want %v", got, want)
	}
}

// A tree that lingers after its close takes no transition, so a push past an awaiting child's READY
// head leaves the child in awaiting_merge, and its arm, which would otherwise merge the moved head,
// is disarmed all the same.
func TestAPushPastTheREADYHeadInALingeringTreeDisarmsWithoutATransition(t *testing.T) {
	pool := migratedPool(t)
	until := time.Now().Add(time.Hour)
	seedIssue(t, pool, record.Issue{Key: "LEGION-207", Tree: "LEGION-207", Project: "LEGION", Title: "root", Phase: phase.Done, Generation: 1, Status: "done", Rank: "U", LingerUntil: &until})
	parent := "LEGION-207"
	seedIssue(t, pool, record.Issue{Key: "LEGION-208", Tree: "LEGION-207", Parent: &parent, Project: "LEGION", Title: "child",
		Phase: phase.AwaitingMerge, Generation: 1, Status: "retro", Rank: "V"})
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-208", Repo: "sjawhar/legion",
		Number: 42, Branch: "legion/LEGION-208", HeadSHA: "head"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-208", Role: claim.RoleMerger, Claim: "merge-claim", HandoffCommit: "head", Summary: "READY #42 at head"})
	applyRefusingNothing(t, pool, readyEngine(""),
		intake.PullRequestSynchronized{Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", HeadSHA: "moved"})
	if got := issuePhase(t, pool); got != phase.AwaitingMerge {
		t.Fatalf("the lingering tree's child is in %s, want it left in awaiting_merge", got)
	}
	if got, want := autoMergeDisables(t, pool), []record.AutoMergeDisable{{Number: 42}}; !slices.Equal(got, want) {
		t.Fatalf("auto-merge disables %v, want %v", got, want)
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
