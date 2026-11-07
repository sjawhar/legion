package workflow

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// basePushPhases is the phase each issue seedBasePushTree records is in, which a push to a base
// leaves exactly as it is.
var basePushPhases = map[string]phase.Phase{
	"LEGION-208": phase.Reviewing, "LEGION-209": phase.Testing, "LEGION-210": phase.Testing, "LEGION-211": phase.Testing,
}

// basesMoved is the commit each recorded pull request's base was last pushed to, by issue.
func basesMoved(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(t.Context(), "select issue, base_moved from pull_requests order by issue")
	if err != nil {
		t.Fatalf("read the moved bases: %v", err)
	}
	defer rows.Close()
	moved := map[string]string{}
	for rows.Next() {
		var issue, base string
		if err := rows.Scan(&issue, &base); err != nil {
			t.Fatalf("read the moved bases: %v", err)
		}
		moved[issue] = base
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the moved bases: %v", err)
	}
	return moved
}

// assertBasesMoved fails unless each recorded pull request's base was last pushed to the commit
// want names it, by issue.
func assertBasesMoved(t *testing.T, pool *pgxpool.Pool, want map[string]string) {
	t.Helper()
	got := basesMoved(t, pool)
	if len(got) != len(want) {
		t.Fatalf("moved bases = %v, want %v", got, want)
	}
	for issue, moved := range want {
		if got[issue] != moved {
			t.Fatalf("moved bases = %v, want %v", got, want)
		}
	}
}

// assertPhasesLeftAsThey fails unless every issue of the tree is still in the phase it was seeded
// in: a base that moved decides nothing of a phase.
func assertPhasesLeftAsThey(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for issue, want := range basePushPhases {
		var got string
		if err := pool.QueryRow(t.Context(), "select phase from issues where key = $1", issue).Scan(&got); err != nil {
			t.Fatalf("read the phase of %s: %v", issue, err)
		}
		if phase.Phase(got) != want {
			t.Fatalf("%s is in %s after the push to a base, want it left in %s", issue, got, want)
		}
	}
	assertOutboxKinds(t, pool, []string{})
}

// seedBasePushTree records a root and three children, each with a pull request: two open on main,
// one open on a release branch and one closed on main, so a push to main has exactly two open pull
// requests to reach and two rows it must leave alone.
func seedBasePushTree(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	root := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: root, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root, Project: "LEGION",
		Title: "root", Phase: basePushPhases[root], Generation: 1, Status: "needs_review", Rank: "U"})
	for _, child := range []string{"LEGION-209", "LEGION-210", "LEGION-211"} {
		parent := root
		seedIssue(t, pool, record.Issue{Key: child, Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: root, Project: "LEGION",
			Title: "child", Parent: &parent, Phase: basePushPhases[child], Generation: 1, Status: "testing", Rank: "V", LastDispatchSeq: 1})
	}
	for _, pr := range []record.PullRequest{
		{Issue: root, Repo: "sjawhar/legion", Number: 42, Branch: "legion/LEGION-208", Base: "main", HeadSHA: "head-208", Failing: []string{}, State: record.PullRequestOpen},
		{Issue: "LEGION-209", Repo: "sjawhar/legion", Number: 43, Branch: "legion/LEGION-209", Base: "main", HeadSHA: "head-209", Failing: []string{}, State: record.PullRequestOpen},
		{Issue: "LEGION-210", Repo: "sjawhar/legion", Number: 44, Branch: "legion/LEGION-210", Base: "release/1.0", HeadSHA: "head-210", Failing: []string{}, State: record.PullRequestOpen},
		{Issue: "LEGION-211", Repo: "sjawhar/legion", Number: 45, Branch: "legion/LEGION-211", Base: "main", HeadSHA: "head-211", Failing: []string{}, State: record.PullRequestClosed},
	} {
		seedPR(t, pool, pr)
	}
}

// One push to a base reaches every open pull request recorded on that base, and no other: both open
// on main record the commit the push left main at, while the one on another base and the one closed
// on main are left as they were. The push decides nothing else - every issue stays in the phase it
// was in, and nothing is queued - since what a moved base says is only that the next read of those
// pull requests is needed.
func TestABasePushMarksEveryOpenPullRequestOnThatBase(t *testing.T) {
	pool := migratedPool(t)
	seedBasePushTree(t, pool)

	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
		intake.BasePush{Repo: "sjawhar/legion", Branch: "main", After: "main-moved"})

	assertBasesMoved(t, pool, map[string]string{"LEGION-208": "main-moved", "LEGION-209": "main-moved", "LEGION-210": "", "LEGION-211": ""})
	assertPhasesLeftAsThey(t, pool)
}

// A push to a branch no pull request is recorded on - every push of the other automation a mapped
// repository carries, and every push to a repository Legion has no open pull request in - changes
// nothing: the handler's one lookup finds no row and the transaction ends there.
func TestAPushToABaseNothingIsRecordedOnChangesNothing(t *testing.T) {
	pool := migratedPool(t)
	seedBasePushTree(t, pool)

	applyRefusingNothing(t, pool, testEngine(config.DesignGateRootIssues, nil),
		intake.BasePush{Repo: "sjawhar/legion", Branch: "bot/nightly-sweep-7", After: "sweep-head"},
		intake.BasePush{Repo: "sjawhar/legion", Branch: "release/2.0", After: "release-head"})

	assertBasesMoved(t, pool, map[string]string{"LEGION-208": "", "LEGION-209": "", "LEGION-210": "", "LEGION-211": ""})
	assertPhasesLeftAsThey(t, pool)
}
