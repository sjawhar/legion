package workflow

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A GitHub fact applies to an issue only when it names the issue's repository: every fact kind
// from another repository leaves the issue, its pull request record and the outbox as they were,
// whether the other repository's pull request is on the issue's branch or a record names the other
// repository for the issue (one written before the record carried the issue's repository).
func TestAGitHubFactFromAnotherRepositoryChangesNothing(t *testing.T) {
	updated := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, recorded := range []string{"acme/widgets", "acme/other"} {
		facts := []intake.Fact{
			intake.PullRequestOpened{Repo: "acme/other", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "other-head", UpdatedAt: updated},
			intake.PullRequestOpened{Repo: "acme/other", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "other-head", UpdatedAt: updated, Reopened: true},
			intake.PullRequestSynchronized{Repo: "acme/other", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "other-head", UpdatedAt: updated},
			intake.Push{Repo: "acme/other", Branch: "legion/LEGION-209", Before: "head", After: "other-head"},
			intake.PullRequestReview{Repo: "acme/other", Number: 42, ID: 7, State: "approved", CommitID: "head", HeadSHA: "head", Author: testReviewApp, SubmittedAt: updated},
			intake.PullRequestChecks{Repo: "acme/other", Number: 42, HeadSHA: "head", Failing: []string{"unit"}, Cancelled: []string{}, Snapshot: "red", Generation: 9},
			intake.RequiredChecks{Repo: "acme/other", Number: 42, Names: []string{"unit"}, Workflows: []record.RequiredWorkflow{}},
			intake.PullRequestMergeability{Repo: "acme/other", Number: 42, Base: "main", Mergeable: record.MergeabilityConflicting},
			intake.PullRequestMerged{Repo: "acme/other", Number: 42, MergeSHA: "merge"},
			intake.PullRequestClosed{Repo: "acme/other", Number: 42, HeadSHA: "head", UpdatedAt: updated},
		}
		for i, fact := range facts {
			t.Run(recorded+"/"+reflect.TypeOf(fact).Name(), func(t *testing.T) {
				pool := migratedPool(t)
				openTree(t, pool)
				parent := "LEGION-208"
				seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent,
					Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "V", LastDispatchSeq: 1})
				seedPR(t, pool, record.PullRequest{Issue: "LEGION-209", Repo: recorded, Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head",
					HeadUpdatedAt: updated.Add(-time.Hour), Failing: []string{}, Generation: 1, State: record.PullRequestOpen})
				before := snapshot(t, pool)
				applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))("other-repository-"+string(rune('a'+i)), fact)
				if after := snapshot(t, pool); !reflect.DeepEqual(after, before) {
					t.Fatalf("%T from acme/other changed the record:\nbefore %#v\nafter  %#v", fact, before, after)
				}
			})
		}
	}
}

// GitHub owner and repository names are case-insensitive, so a pull request GitHub spells
// Acme/Widgets is the pull request of an issue whose record keeps acme/widgets, the spelling the
// backfill kept from its clone.
func TestAPullRequestInAnotherSpellingOfTheIssuesRepositoryIsItsOwn(t *testing.T) {
	pool := migratedPool(t)
	openTree(t, pool)
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent,
		Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V", LastDispatchSeq: 1})
	applyFacts(t, pool, testEngine(config.DesignGateRootIssues, nil))("opened", intake.PullRequestOpened{
		Repo: "Acme/Widgets", Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head", UpdatedAt: time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)})
	var pr *record.PullRequest
	seedRecord(t, pool, func(tx pgx.Tx) error {
		var err error
		pr, err = record.NewStore().PullRequest(context.Background(), tx, "LEGION-209")
		return err
	})
	if pr == nil || pr.Number != 42 || pr.Repo != "Acme/Widgets" {
		t.Fatalf("LEGION-209's pull request = %+v, want #42 on Acme/Widgets", pr)
	}
}

type recordSnapshot struct {
	Issue  *record.Issue
	PR     *record.PullRequest
	Phases []record.PhaseRow
	Outbox int
}

func snapshot(t *testing.T, pool *pgxpool.Pool) recordSnapshot {
	t.Helper()
	var got recordSnapshot
	seedRecord(t, pool, func(tx pgx.Tx) error {
		records := record.NewStore()
		var err error
		if got.Issue, err = records.Issue(context.Background(), tx, "LEGION-209"); err != nil {
			return err
		}
		if got.PR, err = records.PullRequest(context.Background(), tx, "LEGION-209"); err != nil {
			return err
		}
		if got.Phases, err = records.Phases(context.Background(), tx, "LEGION-209"); err != nil {
			return err
		}
		return tx.QueryRow(context.Background(), "select count(*) from outbox").Scan(&got.Outbox)
	})
	return got
}
