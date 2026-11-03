package workflow

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
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

// renamedLegion is the repository equivalence the tests of a configured rename read: sjawhar/legion
// as the records of a tree opened before the transfer keep it, and cybermonklabs/legion as GitHub
// names it after.
var renamedLegion = ghrepo.MustRenames(ghrepo.MustRename("sjawhar/legion", "cybermonklabs/legion"))

// renamedEngine is the workflow tests' engine (testEngine) under the repository equivalence
// renames: the one field of Config a configured rename changes, set on the engine's own copy,
// which New stores, so these tests read the one config every other workflow test reads.
func renamedEngine(renames ghrepo.Renames) *Engine {
	engine := testEngine(config.DesignGateRootIssues, nil)
	engine.cfg.RepositoryRenames = renames
	return engine
}

// seedRenamedTree seeds LEGION-209, a child of the open tree recorded on sjawhar/legion and in
// reviewing with its reviewer's and its implementer's rows, as a tree opened before its
// repository's transfer stands. prRepo, when it names one, is the repository its pull request #42
// is recorded on — the one GitHub named when it was opened.
func seedRenamedTree(t *testing.T, pool *pgxpool.Pool, prRepo string) {
	t.Helper()
	openTree(t, pool)
	parent := "LEGION-208"
	seedIssue(t, pool, record.Issue{Key: "LEGION-209", Repo: "sjawhar/legion", DefaultBranch: "main", GitHubRepo: "sjawhar/legion", Tree: "LEGION-208", Project: "LEGION", Title: "child", Parent: &parent,
		Phase: phase.Reviewing, Generation: 1, Status: "needs_review", Rank: "V", LastDispatchSeq: 1})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleImplementer, Claim: "implement-claim"})
	seedPhase(t, pool, record.PhaseRow{Issue: "LEGION-209", Role: claim.RoleReviewer, Claim: "review-claim", HandoffCommit: "head", Summary: "reviewed"})
	if prRepo == "" {
		return
	}
	seedPR(t, pool, record.PullRequest{State: record.PullRequestOpen, Issue: "LEGION-209", Repo: prRepo, Number: 42, Branch: "legion/LEGION-209",
		HeadSHA: "head", HeadUpdatedAt: renamedUpdated.Add(-time.Hour), Failing: []string{}, Generation: 1})
}

// renamedUpdated is the clock every lifecycle fact of the rename tests carries.
var renamedUpdated = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// A tree recorded under the repository a transfer moved takes the GitHub facts naming the one it
// moved to: the issue's record keeps sjawhar/legion for its whole life, GitHub's events name
// cybermonklabs/legion, and a configured rename (Config.RepositoryRenames) makes the two one
// repository wherever the engine compares them — issueForBranch for a pull request opened on the
// issue's branch, and ownPullRequest for the settlement, the review and the merge of the pull
// request that opened under the new name. Each fact is driven three ways: under the pair, where it
// applies; under no pair against the repository the records name, which is what it does today and
// what the first case is held to; and under no pair against the repository it moved to, the
// negative control, where it changes nothing at all, as any other repository's fact does
// (TestAGitHubFactFromAnotherRepositoryChangesNothing).
func TestAGitHubFactNamingARenamedRepositoryAppliesToTheTreeRecordedUnderItsOldName(t *testing.T) {
	for _, tc := range []struct {
		name string
		// prRepo is the repository the pull request is recorded on before the fact, empty for the
		// opened fact, which records it itself.
		prRepo func(repo string) string
		fact   func(repo string) intake.Fact
		// want asserts the record change the fact makes, repo being the repository it named.
		want func(t *testing.T, repo string, after recordSnapshot)
	}{
		{
			name:   "a pull request opened",
			prRepo: func(string) string { return "" },
			fact: func(repo string) intake.Fact {
				return intake.PullRequestOpened{Repo: repo, Number: 42, Branch: "legion/LEGION-209", HeadSHA: "head", UpdatedAt: renamedUpdated}
			},
			want: func(t *testing.T, repo string, after recordSnapshot) {
				t.Helper()
				if pr := after.PR; pr == nil || pr.Repo != repo || pr.Number != 42 || pr.Branch != "legion/LEGION-209" || pr.State != record.PullRequestOpen {
					t.Fatalf("LEGION-209's pull request = %+v, want #42 open on %s from its branch", pr, repo)
				}
			},
		},
		{
			name:   "a checks settlement",
			prRepo: func(repo string) string { return repo },
			fact: func(repo string) intake.Fact {
				return intake.PullRequestChecks{Repo: repo, Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 7}},
					Generation: 9, Snapshot: "red-head", Failing: []string{"ci"}, Cancelled: []string{}}
			},
			want: func(t *testing.T, _ string, after recordSnapshot) {
				t.Helper()
				pr := after.PR
				if pr == nil || pr.CheckedHead != "head" || len(pr.Failing) != 1 || pr.Failing[0] != "ci" {
					t.Fatalf("LEGION-209's pull request = %+v, want the red settlement of head recorded on it", pr)
				}
				if after.Issue == nil || after.Issue.Phase != phase.Reviewing {
					t.Fatalf("LEGION-209 is in %+v, want the settlement to have left it in reviewing: a settlement moves no issue", after.Issue)
				}
			},
		},
		{
			name:   "a review",
			prRepo: func(repo string) string { return repo },
			fact: func(repo string) intake.Fact {
				return intake.PullRequestReview{Repo: repo, Number: 42, ID: 7, State: "approved", CommitID: "head", HeadSHA: "head",
					Author: testReviewApp, SubmittedAt: renamedUpdated}
			},
			want: func(t *testing.T, _ string, after recordSnapshot) {
				t.Helper()
				if pr := after.PR; pr == nil || pr.ReviewSeen.ID != 7 {
					t.Fatalf("LEGION-209's pull request = %+v, want the review it has seen recorded as 7", pr)
				}
				reviewer := phaseRowIn(t, after.Phases, claim.RoleReviewer)
				if reviewer.Decision == nil || reviewer.Decision.State != "approved" || reviewer.Decision.Head != "head" {
					t.Fatalf("the reviewer's row = %+v, want its round decided by an approval of head", reviewer)
				}
			},
		},
		{
			name:   "a merge",
			prRepo: func(repo string) string { return repo },
			fact: func(repo string) intake.Fact {
				return intake.PullRequestMerged{Repo: repo, Number: 42, MergeSHA: "merge"}
			},
			want: func(t *testing.T, _ string, after recordSnapshot) {
				t.Helper()
				if pr := after.PR; pr == nil || pr.State != record.PullRequestMerged || pr.MergeSHA != "merge" {
					t.Fatalf("LEGION-209's pull request = %+v, want it recorded merged at merge", pr)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("the pair configured, the fact naming the repository it moved to", func(t *testing.T) {
				pool := migratedPool(t)
				seedRenamedTree(t, pool, tc.prRepo("cybermonklabs/legion"))
				applyFacts(t, pool, renamedEngine(renamedLegion))("renamed", tc.fact("cybermonklabs/legion"))
				tc.want(t, "cybermonklabs/legion", snapshot(t, pool))
			})
			t.Run("no pair configured, the fact naming the repository the records name", func(t *testing.T) {
				pool := migratedPool(t)
				seedRenamedTree(t, pool, tc.prRepo("sjawhar/legion"))
				applyFacts(t, pool, renamedEngine(ghrepo.Renames{}))("matching", tc.fact("sjawhar/legion"))
				tc.want(t, "sjawhar/legion", snapshot(t, pool))
			})
			t.Run("no pair configured, the fact naming the repository it moved to", func(t *testing.T) {
				pool := migratedPool(t)
				seedRenamedTree(t, pool, tc.prRepo("cybermonklabs/legion"))
				before := snapshot(t, pool)
				applyFacts(t, pool, renamedEngine(ghrepo.Renames{}))("unconfigured", tc.fact("cybermonklabs/legion"))
				if after := snapshot(t, pool); !reflect.DeepEqual(after, before) {
					t.Fatalf("%s from cybermonklabs/legion with no rename configured changed the record:\nbefore %#v\nafter  %#v", tc.name, before, after)
				}
			})
		})
	}
}

// A pull request already open when its repository was transferred takes the GitHub facts naming
// the repository it moved to: its row keeps the name it opened under (sjawhar/legion) for its
// whole life, while every event after the transfer names cybermonklabs/legion, so the row is
// found only when the read is tried under both names of the configured pair
// (ghrepo.Under, read by pullRequest and pullRequestByBranch). This is the tree the
// parent issue holds Legion to: one whose pull request was open at the transfer pushes a commit,
// gets that commit's CI result, a review and its merge, with no restart of the tree. Each fact is
// driven the three ways its sibling above drives them — under the pair, where it applies; under
// no pair against the name the row keeps, which holds the assertion to what the engine does
// today; and under no pair against the name it moved to, the negative control, where it changes
// nothing at all.
func TestAPullRequestOpenAtTheTransferTakesTheFactsNamingTheRepositoryItMovedTo(t *testing.T) {
	for _, tc := range []struct {
		name string
		fact func(repo string) intake.Fact
		// want asserts the record change the fact makes to the pull request recorded on
		// sjawhar/legion, whichever name the fact carried.
		want func(t *testing.T, after recordSnapshot)
	}{
		{
			name: "a push of a commit",
			fact: func(repo string) intake.Fact {
				return intake.Push{Repo: repo, Branch: "legion/LEGION-209", Before: "head", After: "pushed-head", Pusher: "author", Sender: "author"}
			},
			want: func(t *testing.T, after recordSnapshot) {
				t.Helper()
				pr := after.PR
				if pr == nil || len(pr.Pushes) != 1 || pr.Pushes[0].SHA != "pushed-head" || pr.Pushes[0].Before != "head" {
					t.Fatalf("LEGION-209's pull request = %+v, want the push of pushed-head recorded on it", pr)
				}
			},
		},
		{
			name: "the synchronize of that commit",
			fact: func(repo string) intake.Fact {
				return intake.PullRequestSynchronized{Repo: repo, Number: 42, Branch: "legion/LEGION-209", Before: "head", HeadSHA: "pushed-head", Sender: "author", UpdatedAt: renamedUpdated}
			},
			want: func(t *testing.T, after recordSnapshot) {
				t.Helper()
				if pr := after.PR; pr == nil || pr.HeadSHA != "pushed-head" {
					t.Fatalf("LEGION-209's pull request = %+v, want its head moved to pushed-head", pr)
				}
			},
		},
		{
			name: "that commit's CI result",
			fact: func(repo string) intake.Fact {
				return intake.PullRequestChecks{Repo: repo, Number: 42, HeadSHA: "head", CheckRuns: []record.AttemptRun{{Name: "ci", ID: 7}},
					Generation: 9, Snapshot: "red-head", Failing: []string{"ci"}, Cancelled: []string{}}
			},
			want: func(t *testing.T, after recordSnapshot) {
				t.Helper()
				pr := after.PR
				if pr == nil || pr.CheckedHead != "head" || len(pr.Failing) != 1 || pr.Failing[0] != "ci" {
					t.Fatalf("LEGION-209's pull request = %+v, want the red settlement of head recorded on it", pr)
				}
				if after.Issue == nil || after.Issue.Phase != phase.Reviewing {
					t.Fatalf("LEGION-209 is in %+v, want the settlement to have left it in reviewing: a settlement moves no issue", after.Issue)
				}
			},
		},
		{
			name: "a review",
			fact: func(repo string) intake.Fact {
				return intake.PullRequestReview{Repo: repo, Number: 42, ID: 7, State: "approved", CommitID: "head", HeadSHA: "head",
					Author: testReviewApp, SubmittedAt: renamedUpdated}
			},
			want: func(t *testing.T, after recordSnapshot) {
				t.Helper()
				if pr := after.PR; pr == nil || pr.ReviewSeen.ID != 7 {
					t.Fatalf("LEGION-209's pull request = %+v, want the review it has seen recorded as 7", pr)
				}
				reviewer := phaseRowIn(t, after.Phases, claim.RoleReviewer)
				if reviewer.Decision == nil || reviewer.Decision.State != "approved" || reviewer.Decision.Head != "head" {
					t.Fatalf("the reviewer's row = %+v, want its round decided by an approval of head", reviewer)
				}
			},
		},
		{
			name: "its merge",
			fact: func(repo string) intake.Fact {
				return intake.PullRequestMerged{Repo: repo, Number: 42, MergeSHA: "merge"}
			},
			want: func(t *testing.T, after recordSnapshot) {
				t.Helper()
				if pr := after.PR; pr == nil || pr.State != record.PullRequestMerged || pr.MergeSHA != "merge" {
					t.Fatalf("LEGION-209's pull request = %+v, want it recorded merged at merge", pr)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("the pair configured, the fact naming the repository it moved to", func(t *testing.T) {
				pool := migratedPool(t)
				seedRenamedTree(t, pool, "sjawhar/legion")
				applyFacts(t, pool, renamedEngine(renamedLegion))("renamed", tc.fact("cybermonklabs/legion"))
				tc.want(t, snapshot(t, pool))
			})
			t.Run("no pair configured, the fact naming the repository the row keeps", func(t *testing.T) {
				pool := migratedPool(t)
				seedRenamedTree(t, pool, "sjawhar/legion")
				applyFacts(t, pool, renamedEngine(ghrepo.Renames{}))("matching", tc.fact("sjawhar/legion"))
				tc.want(t, snapshot(t, pool))
			})
			t.Run("no pair configured, the fact naming the repository it moved to", func(t *testing.T) {
				pool := migratedPool(t)
				seedRenamedTree(t, pool, "sjawhar/legion")
				before := snapshot(t, pool)
				applyFacts(t, pool, renamedEngine(ghrepo.Renames{}))("unconfigured", tc.fact("cybermonklabs/legion"))
				if after := snapshot(t, pool); !reflect.DeepEqual(after, before) {
					t.Fatalf("%s from cybermonklabs/legion with no rename configured changed the record:\nbefore %#v\nafter  %#v", tc.name, before, after)
				}
			})
		})
	}
}

// phaseRowIn is role's row among rows, which a snapshot already read; handoff_test.go's
// phaseRowOf reads the record itself.
func phaseRowIn(t *testing.T, rows []record.PhaseRow, role claim.Role) record.PhaseRow {
	t.Helper()
	for _, row := range rows {
		if row.Role == role {
			return row
		}
	}
	t.Fatalf("no %s row among %+v", role, rows)
	return record.PhaseRow{}
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
