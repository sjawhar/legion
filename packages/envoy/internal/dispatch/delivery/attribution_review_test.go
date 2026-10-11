package delivery

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// TestAttributionMatchesGitHubURLsWhateverTheirCase: GitHub spells an owner and a repository as
// their owners do (`Acme/Widgets`), and Dispatch stores an issue's GitHub link lowercased, so an
// external link naming the pull request (source 2) and one naming a GitHub issue its body cites
// (source 3) credit it whatever case either side spells them in.
func TestAttributionMatchesGitHubURLsWhateverTheirCase(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	// URLs no other test links: a link's URL is unique across every issue, and the tests share
	// one database.
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-201", "ATTR-202"}, map[string][]string{
		"ATTR-201": {"https://github.com/acme/widgets/pull/201"},
		"ATTR-202": {"https://github.com/acme/widgets/issues/277"},
	})

	key, source := resolvedKey(t, ctx, pool, attributionFacts{
		Repo: "Acme/Widgets", URL: "https://github.com/Acme/Widgets/pull/201",
	})
	if key != "ATTR-201" || source != sourceExternalLink {
		t.Fatalf("a mixed-case pull request URL = %q (%s), want ATTR-201 from its external link", key, source)
	}
	key, source = resolvedKey(t, ctx, pool, attributionFacts{
		Repo: "Acme/Widgets", URL: "https://github.com/Acme/Widgets/pull/202", Body: "Closes #277",
	})
	if key != "ATTR-202" || source != sourceGitHubIssueLink {
		t.Fatalf("a bare #277 in a mixed-case repository = %q (%s), want ATTR-202 from the cited issue's link", key, source)
	}
}

// TestAttributingStoredPullRequestsTakesEverySourceInOrder: the bulk resolve reads the same five
// sources in the same order as one pull request's. Every row holds inputs naming issues from
// several sources, and each row's inputs start one source later than the row before, so each
// source wins exactly one row; handing any two inputs over in another order credits some row from
// the wrong source.
func TestAttributingStoredPullRequestsTakesEverySourceInOrder(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	if _, err := pool.Exec(ctx, `delete from issue_external_links where url like 'https://github.com/acme/widgets/pull/21_' or url = 'https://github.com/acme/widgets/issues/213'`); err != nil {
		t.Fatalf("clear an earlier run's links: %v", err)
	}
	seedAttributionIssues(t, ctx, pool, []string{"ATTR-211", "ATTR-212", "ATTR-213", "ATTR-214", "ATTR-215"}, map[string][]string{
		"ATTR-212": {"https://github.com/acme/widgets/pull/211"},
		"ATTR-213": {"https://github.com/acme/widgets/issues/213"},
	})
	cited := []string{"https://github.com/acme/widgets/issues/213"}
	rows := []struct {
		number int
		inputs AttributionInputs
		want   string
	}{
		// Every source names an issue: the title and body win.
		{210, AttributionInputs{TitleKeys: []string{"ATTR-211"}, CitedIssues: cited, BranchKeys: []string{"ATTR-214"}, CommitKeys: []string{"ATTR-215"}}, "ATTR-211"},
		// ATTR-212's link names #211 itself: the external link wins over what the body cites.
		{211, AttributionInputs{TitleKeys: []string{}, CitedIssues: cited, BranchKeys: []string{"ATTR-214"}, CommitKeys: []string{"ATTR-215"}}, "ATTR-212"},
		{212, AttributionInputs{TitleKeys: []string{}, CitedIssues: cited, BranchKeys: []string{"ATTR-214"}, CommitKeys: []string{"ATTR-215"}}, "ATTR-213"},
		{213, AttributionInputs{TitleKeys: []string{}, CitedIssues: []string{}, BranchKeys: []string{"ATTR-214"}, CommitKeys: []string{"ATTR-215"}}, "ATTR-214"},
		{214, AttributionInputs{TitleKeys: []string{}, CitedIssues: []string{}, BranchKeys: []string{}, CommitKeys: []string{"ATTR-215"}}, "ATTR-215"},
	}
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	for _, row := range rows {
		inputs := row.inputs
		if err := UpsertPullRequest(ctx, pool, DeliveryPullRequest{
			Repo: "acme/widgets", Number: row.number, Title: "feat: every source", URL: "https://github.com/acme/widgets/pull/" + strconv.Itoa(row.number),
			Author: "octocat", CreatedAt: &merged, MergedAt: &merged, Attribution: &inputs,
		}); err != nil {
			t.Fatalf("seed #%d: %v", row.number, err)
		}
	}

	if _, err := AttributePullRequests(ctx, pool, 100); err != nil {
		t.Fatalf("attribute: %v", err)
	}
	for _, row := range rows {
		var issue *string
		if err := pool.QueryRow(ctx, `select issue_key from delivery_pull_requests where repo = 'acme/widgets' and number = $1`, row.number).Scan(&issue); err != nil {
			t.Fatalf("read #%d: %v", row.number, err)
		}
		if issue == nil || *issue != row.want {
			got := "none"
			if issue != nil {
				got = *issue
			}
			t.Errorf("#%d issue = %s, want %s", row.number, got, row.want)
		}
	}
}

// TestTheAttributionBackfillStaysWithinItsCallBudget: with more unread rows than one pass's budget
// covers, a pass makes no more than attributionBackfillCalls GitHub calls for them, the rest wait
// for later passes, and the workflow runs are read before the backfill spends anything.
func TestTheAttributionBackfillStaysWithinItsCallBudget(t *testing.T) {
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	merged := time.Date(2024, 1, 1, 1, 0, 0, 0, time.UTC)
	const rows = attributionBackfillCalls // twice what the budget reads at two calls a row
	for number := 1000; number < 1000+rows; number++ {
		if _, err := pool.Exec(ctx, `
			insert into delivery_pull_requests (repo, number, title, url, author, created_at, merged_at, partial)
			values ('acme/widgets', $1, 'feat: history', $2, 'octocat', $3, $3, false)`,
			number, "https://github.com/acme/widgets/pull/"+strconv.Itoa(number), merged); err != nil {
			t.Fatalf("seed #%d: %v", number, err)
		}
	}

	var calls, runListings atomic.Int32
	var callsBeforeRuns atomic.Int32
	callsBeforeRuns.Store(-1)
	fake := newFakeGitHub(t)
	fake.handle("POST /graphql", func(w http.ResponseWriter, r *http.Request) {
		mustEncode(t, w, searchResponseJSON(0, nil, false, ""))
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		number, _ := strconv.Atoi(r.PathValue("number"))
		mustEncode(t, w, map[string]any{
			"number": number, "title": "feat: history", "html_url": "https://github.com/acme/widgets/pull/" + r.PathValue("number"),
			"body": "", "head": map[string]any{"ref": "feature"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		mustEncode(t, w, []any{map[string]any{"commit": map[string]any{"message": "feat: history"}}})
	})
	for _, workflow := range []string{"deploy.yml", "pr-checks.yml"} {
		fake.handle("GET /repos/acme/widgets/actions/workflows/.github%2Fworkflows%2F"+workflow+"/runs", func(w http.ResponseWriter, r *http.Request) {
			if runListings.Add(1) == 1 {
				callsBeforeRuns.Store(calls.Load())
			}
			mustEncode(t, w, map[string]any{"total_count": 0, "workflow_runs": []any{}})
		})
	}

	NewReconcile(pool, fake.newTestClient()).runOnce(ctx)

	if n := calls.Load(); n > attributionBackfillCalls || n == 0 {
		t.Fatalf("one pass made %d attribution calls; want between 1 and the budget of %d", n, attributionBackfillCalls)
	}
	if before := callsBeforeRuns.Load(); before != 0 {
		t.Fatalf("%d attribution calls ran before the workflow runs were read; want the backfill after them", before)
	}
	var read, unread int
	if err := pool.QueryRow(ctx, `
		select count(*) filter (where attribution_title_keys is not null), count(*) filter (where attribution_title_keys is null)
		from delivery_pull_requests where number >= 1000`).Scan(&read, &unread); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if read != int(calls.Load())/2 || unread != rows-read || unread == 0 {
		t.Fatalf("after one pass: %d rows read, %d unread, %d calls; want two calls a row read and the rest left for later passes", read, unread, calls.Load())
	}
}
