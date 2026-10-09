package delivery

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedAttributionIssues writes project ATTR and one issue per key (ATTR-<n>), and each
// links[key] URL as that issue's external link. Idempotent, so tests sharing the database can
// seed the same keys.
func seedAttributionIssues(t *testing.T, ctx context.Context, pool *store.Pool, keys []string, links map[string][]string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `insert into projects (key, name) values ('ATTR', 'Attribution') on conflict do nothing`); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	for _, key := range keys {
		number, err := strconv.Atoi(strings.TrimPrefix(key, "ATTR-"))
		if err != nil {
			t.Fatalf("issue key %q is not ATTR-<n>", key)
		}
		if _, err := pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, status, created_by, rank)
			values ($1, 'ATTR', $2, $1, 'todo', '{"kind":"system","id":"test"}', $1)
			on conflict do nothing`, key, number); err != nil {
			t.Fatalf("seed issue %s: %v", key, err)
		}
		for _, url := range links[key] {
			if _, err := pool.Exec(ctx, `
				insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github')
				on conflict do nothing`, key, url); err != nil {
				t.Fatalf("seed link %s -> %s: %v", url, key, err)
			}
		}
	}
}

func resolvedKey(t *testing.T, ctx context.Context, pool *store.Pool, facts attributionFacts) (string, attributionSource) {
	t.Helper()
	key, source, err := resolveIssueKey(ctx, pool, facts)
	if err != nil {
		t.Fatalf("resolveIssueKey(%+v): %v", facts, err)
	}
	if key == nil {
		return "", source
	}
	return *key, source
}

// TestAttributionFallsBackToAnExternalLinkNamingThePullRequest: source (a), an issue whose
// external link is the pull request's own URL.
func TestAttributionFallsBackToAnExternalLinkNamingThePullRequest(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-101"}, map[string][]string{
		"ATTR-101": {"https://github.com/acme/widgets/pull/101"},
	})
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/101", Title: "feat: no key here",
	})
	if key != "ATTR-101" || source != sourceExternalLink {
		t.Fatalf("resolved %q from %q, want ATTR-101 from %q", key, source, sourceExternalLink)
	}
}

// TestAttributionIgnoresAnExternalLinkNamingAnotherPullRequest: a link to pull/103 credits
// nothing to pull/102, though the URLs share a prefix.
func TestAttributionIgnoresAnExternalLinkNamingAnotherPullRequest(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-103"}, map[string][]string{
		"ATTR-103": {"https://github.com/acme/widgets/pull/1030"},
	})
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/103",
		Body: "Follows https://github.com/acme/widgets/pull/1030.",
	})
	if key != "" {
		t.Fatalf("resolved %q from %q, want no issue: the link names another pull request", key, source)
	}
}

// TestAttributionFallsBackToACitedGitHubIssue: source (b), the first GitHub issue the body
// cites, a full URL or a bare #N in the pull request's own repository, that an issue's external
// link names.
func TestAttributionFallsBackToACitedGitHubIssue(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-111", "ATTR-112", "ATTR-113"}, map[string][]string{
		"ATTR-111": {"https://github.com/acme/widgets/issues/77"},
		"ATTR-112": {"https://github.com/acme/other/issues/5"},
		"ATTR-113": {"https://github.com/acme/widgets/issues/88"},
	})
	for _, tc := range []struct {
		name, body, want string
	}{
		{"bare reference in the same repository", "Fixes #77.", "ATTR-111"},
		{"full URL to another repository", "See https://github.com/acme/other/issues/5 for context.", "ATTR-112"},
		{"first linked citation in body order", "Unlinked #12, then #88, then https://github.com/acme/other/issues/5.", "ATTR-113"},
		{"a pull request URL is not an issue", "Reverts https://github.com/acme/widgets/pull/77.", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, source := resolvedKey(t, ctx, pool, attributionFacts{
				Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/110", Body: tc.body,
			})
			if key != tc.want {
				t.Fatalf("resolved %q from %q, want %q", key, source, tc.want)
			}
			if tc.want != "" && source != sourceGitHubIssueLink {
				t.Fatalf("source = %q, want %q", source, sourceGitHubIssueLink)
			}
		})
	}
}

// TestAttributionFallsBackToTheHeadBranch: source (c), a key in the branch name, matched
// whatever its case.
func TestAttributionFallsBackToTheHeadBranch(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-121"}, nil)
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/120", HeadRef: "fix/attr-121-guard-the-thing",
	})
	if key != "ATTR-121" || source != sourceBranch {
		t.Fatalf("resolved %q from %q, want ATTR-121 from %q", key, source, sourceBranch)
	}
}

// TestAttributionFallsBackToCommitMessages: source (d), the first known key across the commit
// messages in commit order.
func TestAttributionFallsBackToCommitMessages(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-131", "ATTR-132"}, nil)
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/130",
		CommitMessages: []string{"wip", "refs ATTR-999 and ATTR-132", "closes ATTR-131"},
	})
	if key != "ATTR-132" || source != sourceCommitMessage {
		t.Fatalf("resolved %q from %q, want ATTR-132 from %q", key, source, sourceCommitMessage)
	}
}

// TestAttributionTakesTheSourcesInOrder: title/body, then (a), (b), (c), (d); each source is
// tried only when every earlier one found nothing.
func TestAttributionTakesTheSourcesInOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	prURL := "https://github.com/acme/widgets/pull/140"
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-141", "ATTR-142", "ATTR-143", "ATTR-144", "ATTR-145"}, map[string][]string{
		"ATTR-142": {prURL},
		"ATTR-143": {"https://github.com/acme/widgets/issues/143"},
	})
	all := attributionFacts{
		Repo: "acme/widgets", URL: prURL, Title: "feat: ATTR-141", Body: "Fixes #143.",
		HeadRef: "attr-144-branch", CommitMessages: []string{"ATTR-145"},
	}
	steps := []struct {
		drop   func(*attributionFacts)
		want   string
		source attributionSource
	}{
		{func(*attributionFacts) {}, "ATTR-141", sourceTitleBody},
		{func(f *attributionFacts) { f.Title = "feat: none" }, "ATTR-142", sourceExternalLink},
		{func(f *attributionFacts) { f.URL = "https://github.com/acme/widgets/pull/149" }, "ATTR-143", sourceGitHubIssueLink},
		{func(f *attributionFacts) { f.Body = "" }, "ATTR-144", sourceBranch},
		{func(f *attributionFacts) { f.HeadRef = "main" }, "ATTR-145", sourceCommitMessage},
		{func(f *attributionFacts) { f.CommitMessages = nil }, "", sourceNone},
	}
	facts := all
	for _, step := range steps {
		step.drop(&facts)
		key, source := resolvedKey(t, ctx, pool, facts)
		if key != step.want || source != step.source {
			t.Fatalf("with %+v resolved %q from %q, want %q from %q", facts, key, source, step.want, step.source)
		}
	}
}

// TestAttributionRefusesAKeyDispatchDoesNotHave: a key-shaped string in every source that names
// no stored issue credits nothing.
func TestAttributionRefusesAKeyDispatchDoesNotHave(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	key, source := resolvedKey(t, ctx, pool, attributionFacts{
		Repo: "acme/widgets", URL: "https://github.com/acme/widgets/pull/150",
		Title: "feat: ATTR-9150", Body: "ATTR-9151, #9152", HeadRef: "attr-9153-x", CommitMessages: []string{"ATTR-9154"},
	})
	if key != "" || source != sourceNone {
		t.Fatalf("resolved %q from %q, want no issue: no source names a stored issue", key, source)
	}
}

// TestReconcileBackfillsAttributionInputsOnce: a complete row stored before its attribution
// inputs existed is fetched once, stores them and is credited (here from its head branch); a
// row whose inputs are stored is never fetched again, on that pass or the next. The backfill
// reads only what the attribution needs: the pull request once and its commits once (one page),
// never FetchPullRequest's first-commit read, which a complete row already answered.
func TestReconcileBackfillsAttributionInputsOnce(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-161"}, nil)
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	for _, number := range []int{160, 162} {
		if _, err := pool.Exec(ctx, `
			insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, partial)
			values ('acme/widgets', $1, 'feat: history', $2, 'octocat', $3, $3, false)`,
			number, "https://github.com/acme/widgets/pull/"+strconv.Itoa(number), merged); err != nil {
			t.Fatalf("seed stored row #%d: %v", number, err)
		}
	}
	if _, err := pool.Exec(ctx, `
		update delivery_pull_requests
		set attribution_title_keys = '{}', attribution_cited_issues = '{}', attribution_branch_keys = '{}', attribution_commit_keys = '{}'
		where number = 162`); err != nil {
		t.Fatalf("store #162's inputs: %v", err)
	}

	var fetches160, commits160, firstCommit160, fetches162 atomic.Int32
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/pulls/160", func(w http.ResponseWriter, r *http.Request) {
		fetches160.Add(1)
		mustEncode(t, w, map[string]any{
			"number": 160, "title": "feat: history", "html_url": "https://github.com/acme/widgets/pull/160",
			"user": map[string]any{"login": "octocat"}, "labels": []any{map[string]any{"name": "non-task"}},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T01:00:00Z",
			"merge_commit_sha": "abc", "additions": 1, "deletions": 1, "body": "",
			"head": map[string]any{"ref": "fix/attr-161-history"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/160/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") == "1" {
			firstCommit160.Add(1)
		} else {
			commits160.Add(1)
		}
		mustEncode(t, w, []any{
			map[string]any{"commit": map[string]any{"message": "feat: history\n\nOmp-Session: ses-160", "author": map[string]any{"date": "2024-01-01T00:00:00Z"}}},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/162", func(w http.ResponseWriter, r *http.Request) {
		fetches162.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	noRuns(t, fake)

	reconcile := NewReconcile(pool, fake.newTestClient())
	reconcile.runOnce(ctx)
	reconcile.runOnce(ctx)

	pr, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where number = 160`))
	if err != nil {
		t.Fatalf("read #160: %v", err)
	}
	if pr.IssueKey == nil || *pr.IssueKey != "ATTR-161" || pr.Attribution == nil || len(pr.Attribution.BranchKeys) != 1 {
		t.Fatalf("#160 issue = %v, inputs = %+v; want ATTR-161 from its stored head branch keys", pr.IssueKey, pr.Attribution)
	}
	if len(pr.Sessions) != 1 || pr.Sessions[0] != "ses-160" {
		t.Fatalf("#160 sessions = %v, want the trailer its commits name", pr.Sessions)
	}
	if pull, commits, first := fetches160.Load(), commits160.Load(), firstCommit160.Load(); pull != 1 || commits != 1 || first != 0 {
		t.Fatalf("#160 over two passes: %d pull reads, %d commits pages, %d first-commit reads; want 1, 1, 0", pull, commits, first)
	}
	if n := fetches162.Load(); n != 0 {
		t.Fatalf("#162, whose inputs are stored, was fetched %d times; want 0", n)
	}
}

// TestCompletingAPullRequestWhoseCommitsFailLeavesItsInputsUnread: a completion whose commit
// read fails (a 500 here) still writes the row complete, but leaves its attribution inputs null
// rather than storing an empty commit-key list for good, so the backfill reads it again once the
// commits answer (the same pass's backfill too, which here still meets the 500) and stores what
// they name.
func TestCompletingAPullRequestWhoseCommitsFailLeavesItsInputsUnread(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-181"}, nil)
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 180, Title: "feat: commits fail once", URL: "https://github.com/acme/widgets/pull/180",
		Author: "octocat", CreatedAt: &merged, Partial: true,
	}); err != nil {
		t.Fatalf("seed partial #180: %v", err)
	}

	var commitsFailing atomic.Bool
	commitsFailing.Store(true)
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/pulls/180", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 180, "title": "feat: commits fail once", "html_url": "https://github.com/acme/widgets/pull/180",
			"user": map[string]any{"login": "octocat"}, "labels": []any{},
			"created_at": "2024-01-01T00:00:00Z", "merged_at": "2024-01-01T01:00:00Z",
			"merge_commit_sha": "abc", "additions": 1, "deletions": 1, "body": "",
			"head": map[string]any{"ref": "feature"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/180/commits", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") == "1" {
			mustEncode(t, w, []any{map[string]any{"commit": map[string]any{"message": "x", "author": map[string]any{"date": "2024-01-01T00:00:00Z"}}}})
			return
		}
		// A 500, which readGitHubPage does not retry (it retries only timeouts and gateway
		// timeouts), for the whole first pass: the completion's read and the backfill's.
		if commitsFailing.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		mustEncode(t, w, []any{map[string]any{"commit": map[string]any{"message": "fix ATTR-181: commits"}}})
	})
	noRuns(t, fake)

	reconcile := NewReconcile(pool, fake.newTestClient())
	reconcile.runOnce(ctx)
	first, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where number = 180`))
	if err != nil {
		t.Fatalf("read #180 after the first pass: %v", err)
	}
	if first.Partial || first.Attribution != nil || first.IssueKey != nil {
		t.Fatalf("#180 after its commits failed: partial = %v, inputs = %+v, issue = %v; want complete, inputs unread, no issue",
			first.Partial, first.Attribution, first.IssueKey)
	}

	commitsFailing.Store(false)
	reconcile.runOnce(ctx)
	second, err := ScanPullRequest(pool.QueryRow(ctx, `select `+PullRequestColumns+` from delivery_pull_requests where number = 180`))
	if err != nil {
		t.Fatalf("read #180 after the second pass: %v", err)
	}
	if second.Attribution == nil || len(second.Attribution.CommitKeys) != 1 || second.IssueKey == nil || *second.IssueKey != "ATTR-181" {
		t.Fatalf("#180 after the backfill: inputs = %+v, issue = %v; want ATTR-181 from its commit message", second.Attribution, second.IssueKey)
	}
}

// TestAttributingStoredPullRequestsWalksTheTableInBatches: each pass resolves at most its batch of
// stored rows, those checked longest ago first, so the walk comes round to every row and an
// issue that links a pull request credits it within one walk of the table.
func TestAttributingStoredPullRequestsWalksTheTableInBatches(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-191"}, nil)
	if _, err := pool.Exec(ctx, `delete from issue_external_links where url like 'https://github.com/acme/widgets/pull/19_'`); err != nil {
		t.Fatalf("clear an earlier run's links: %v", err)
	}
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	for number := 190; number < 195; number++ {
		if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
			Repo: "acme/widgets", Number: number, Title: "feat: walked", URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(number),
			Author: "octocat", CreatedAt: &merged, MergedAt: &merged, Attribution: &AttributionInputs{},
		}); err != nil {
			t.Fatalf("seed #%d: %v", number, err)
		}
	}
	checked := func() int {
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from delivery_pull_requests where attribution_checked_at is not null`).Scan(&n); err != nil {
			t.Fatalf("count checked rows: %v", err)
		}
		return n
	}
	if _, err := AttributePullRequests(ctx, pool, 2); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if n := checked(); n != 2 {
		t.Fatalf("after one pass of 2: %d rows checked, want 2", n)
	}
	if _, err := AttributePullRequests(ctx, pool, 2); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n := checked(); n != 4 {
		t.Fatalf("after two passes of 2: %d rows checked, want 4 (the walk takes the unchecked rows next)", n)
	}

	// A link added now credits #194 within the walk: one more pass takes the last unchecked row.
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-191"}, map[string][]string{
		"ATTR-191": {"https://github.com/acme/widgets/pull/194"},
	})
	changed, err := AttributePullRequests(ctx, pool, 2)
	if err != nil {
		t.Fatalf("third pass: %v", err)
	}
	var issue *string
	if err := pool.QueryRow(ctx, `select issue_key from delivery_pull_requests where number = 194`).Scan(&issue); err != nil {
		t.Fatalf("read #194: %v", err)
	}
	if changed != 1 || issue == nil || *issue != "ATTR-191" {
		t.Fatalf("third pass changed %d rows, #194 issue = %v; want 1 and ATTR-191", changed, issue)
	}
}

// TestReconcileCreditsAPullRequestAnIssueLinksLater: a pull request stored with no issue is
// credited once an issue's external link names it, by the next pass and with no GitHub call.
func TestReconcileCreditsAPullRequestAnIssueLinksLater(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-171"}, nil)
	// deliveryTestPool clears only the delivery tables: drop the link an earlier run added.
	if _, err := pool.Exec(ctx, `delete from issue_external_links where url = 'https://github.com/acme/widgets/pull/170'`); err != nil {
		t.Fatalf("clear an earlier run's link: %v", err)
	}
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
		Repo: "acme/widgets", Number: 170, Title: "feat: unlinked", URL: "https://github.com/acme/widgets/pull/170",
		Author: "octocat", CreatedAt: &merged, MergedAt: &merged, Attribution: &AttributionInputs{},
	}); err != nil {
		t.Fatalf("seed #170: %v", err)
	}

	var pullFetches atomic.Int32
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/pulls/", func(w http.ResponseWriter, r *http.Request) {
		pullFetches.Add(1)
		w.WriteHeader(http.StatusNotFound)
	})
	noRuns(t, fake)
	reconcile := NewReconcile(pool, fake.newTestClient())

	reconcile.runOnce(ctx)
	issueOf := func() *string {
		var issue *string
		if err := pool.QueryRow(ctx, `select issue_key from delivery_pull_requests where number = 170`).Scan(&issue); err != nil {
			t.Fatalf("read #170: %v", err)
		}
		return issue
	}
	if issue := issueOf(); issue != nil {
		t.Fatalf("#170 issue = %s before any link names it, want none", *issue)
	}

	seedAttributionIssues(t, ctx, pool, []string{"ATTR-171"}, map[string][]string{
		"ATTR-171": {"https://github.com/acme/widgets/pull/170"},
	})
	reconcile.runOnce(ctx)
	if issue := issueOf(); issue == nil || *issue != "ATTR-171" {
		t.Fatalf("#170 issue = %v after ATTR-171 linked it, want ATTR-171", issue)
	}
	if n := pullFetches.Load(); n != 0 {
		t.Fatalf("pull request fetched %d times; want 0 (stored inputs need no GitHub call)", n)
	}
}

// noRuns answers both workflows' run listings with no runs.
func noRuns(t *testing.T, fake *fakeGitHub) {
	t.Helper()
	for _, workflow := range []string{"deploy.yml", "pr-checks.yml"} {
		fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2F"+workflow+"/runs", func(w http.ResponseWriter, r *http.Request) {
			mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
		})
	}
}
