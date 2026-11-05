package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedHeadIssue records one issue the attribution can name, in the project that owns
// acme/widgets (seedDeliveryProject): a head row is attributed only to an issue that resolves
// to the pull request's own repository (headAttribution), so an issue of no repository is
// attributed to nobody. The key is HEAD-<n>, which is what a failure names; the number is n in
// this fixture's band of the shared project.
func seedHeadIssue(t *testing.T, ctx context.Context, pool *store.Pool, number int) string {
	t.Helper()
	key := fmt.Sprintf("HEAD-%d", number)
	seedDeliveryProject(t, ctx, pool)
	if _, err := pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ($1, $2, $3, $1, 'todo', '{"kind":"system","id":"test"}'::jsonb, $1)
		on conflict do nothing
	`, key, deliveryProject, headIssueBand+number); err != nil {
		t.Fatalf("seed issue %s: %v", key, err)
	}
	return key
}

// seedGadgetsIssue writes project GDGT, maps acme/gadgets to it as its default and writes
// GDGT-<number>: the second repository a two-key title needs, since repo_projects' primary key
// is the repository and acme/widgets already belongs to another project.
func seedGadgetsIssue(t *testing.T, ctx context.Context, pool *store.Pool, number int) string {
	t.Helper()
	key := fmt.Sprintf("GDGT-%d", number)
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name) values ('GDGT', 'Gadgets') on conflict (key) do nothing
	`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into repo_projects (repo, project, created_by, is_default)
		values ('acme/gadgets', 'GDGT', '{"kind":"system","id":"test"}'::jsonb, true)
		on conflict (repo) do nothing
	`); err != nil {
		t.Fatalf("map the repository to the project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ($1, 'GDGT', $2, $1, 'todo', '{"kind":"system","id":"test"}'::jsonb, $1)
		on conflict do nothing
	`, key, number); err != nil {
		t.Fatalf("seed issue %s: %v", key, err)
	}
	return key
}

func readHead(t *testing.T, ctx context.Context, pool *store.Pool, repo string, number int) PullRequestHead {
	t.Helper()
	var head PullRequestHead
	if err := pool.QueryRow(ctx, `
		select repo, number, head_sha, head_ref, base_ref, state, issue_key, attribution_source, checked_at
		from pull_request_heads where repo = $1 and number = $2
	`, repo, number).Scan(&head.Repo, &head.Number, &head.HeadSHA, &head.HeadRef, &head.BaseRef,
		&head.State, &head.IssueKey, &head.AttributionSource, &head.CheckedAt); err != nil {
		t.Fatalf("read the head of %s#%d: %v", repo, number, err)
	}
	return head
}

// The reconcile re-reads an open pull request nobody has sent an event for in five minutes and
// writes the head GitHub reports, with the issue the attribution picks. A pull request its
// title already attributes costs no commits read: the commit messages are the last of the five
// sources, and the event path skips them in the same case.
func TestReconcileRereadsAStalePullRequestHead(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)
	seedHeadIssue(t, ctx, pool, 1)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/42", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{
			"number": 42, "title": "HEAD-1 the work", "body": "", "html_url": "https://github.com/acme/widgets/pull/42",
			"state": "open", "merged": false, "updated_at": "2026-02-01T00:00:00Z",
			"head": map[string]any{
				"ref": "legion/HEAD-1", "sha": "2222222222222222222222222222222222222222",
				"repo": map[string]any{"full_name": "acme/widgets"},
			},
			"base": map[string]any{"ref": "main"},
		})
	})
	var commitReads atomic.Int32
	fake.handle("GET /repos/acme/widgets/pulls/42/commits", func(w http.ResponseWriter, _ *http.Request) {
		commitReads.Add(1)
		mustEncode(t, w, []any{})
	})
	client := fake.newTestClient()

	// The row stands at an older head, last checked ten minutes ago.
	stale := time.Now().Add(-10 * time.Minute)
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: "acme/widgets", Number: 42, HeadSHA: "1111111111111111111111111111111111111111",
		HeadRef: "legion/HEAD-1", BaseRef: "main", State: "open", CheckedAt: &stale,
	}); err != nil {
		t.Fatalf("store the stale head: %v", err)
	}
	// A row checked just now is not in the batch.
	fresh := time.Now()
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: "acme/widgets", Number: 43, HeadSHA: "4444444444444444444444444444444444444444",
		State: "open", CheckedAt: &fresh,
	}); err != nil {
		t.Fatalf("store the fresh head: %v", err)
	}
	batch, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	if len(batch) != 1 || batch[0].Number != 42 {
		t.Fatalf("the stale batch = %+v, want only #42", batch)
	}

	reconcile := NewReconcile(pool, client, nil)
	if err := reconcile.reconcilePullRequestHeads(ctx, time.Now()); err != nil {
		t.Fatalf("reconcile the pull request heads: %v", err)
	}
	head := readHead(t, ctx, pool, "acme/widgets", 42)
	if head.HeadSHA != "2222222222222222222222222222222222222222" || head.State != "open" {
		t.Fatalf("the re-read head = %+v, want the new head still open", head)
	}
	if head.IssueKey == nil || *head.IssueKey != "HEAD-1" || head.AttributionSource != "title_body" {
		t.Fatalf("the re-read head's attribution = %v from %q, want HEAD-1 from its title", head.IssueKey, head.AttributionSource)
	}
	if head.CheckedAt == nil || head.CheckedAt.Before(stale.Add(time.Minute)) {
		t.Fatalf("the re-read head's checked_at = %v, want it moved forward", head.CheckedAt)
	}
	if reads := commitReads.Load(); reads != 0 {
		t.Fatalf("the re-read paged the commit messages %d times, want none: its title already attributes it", reads)
	}
	// The pass is idempotent: nothing is stale a second time.
	again, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads again: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a second pass would re-read %+v, want nothing", again)
	}
}

// The read-through stores every pull request GitHub names for one commit, each attributed as the
// stored pull requests are.
func TestReadThroughCommitPullsStoresEveryPullRequestNamedForACommit(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedHeadIssue(t, ctx, pool, 2)
	seedHeadIssue(t, ctx, pool, 1)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{
			map[string]any{
				"number": 7, "title": "HEAD-2 the work", "body": "", "html_url": "https://github.com/acme/widgets/pull/7",
				"state": "open", "merged": false, "updated_at": "2026-02-01T00:00:00Z",
				"head": map[string]any{
					"ref": "legion/HEAD-2", "sha": "5555555555555555555555555555555555555555",
					"repo": map[string]any{"full_name": "acme/widgets"},
				},
				"base": map[string]any{"ref": "main"},
			},
			map[string]any{
				"number": 8, "title": "An unrelated change", "body": "", "html_url": "https://github.com/acme/widgets/pull/8",
				"state": "closed", "merged_at": "2026-02-02T00:00:00Z", "updated_at": "2026-02-02T00:00:00Z",
				"head": map[string]any{
					"ref": "fix", "sha": "5555555555555555555555555555555555555555",
					"repo": map[string]any{"full_name": "acme/widgets"},
				},
				"base": map[string]any{"ref": "main"},
			},
		})
	})
	// Nothing in #8's title, body or branch names an issue, so the read-through reads its commit
	// messages, the fifth source; they name none either.
	fake.handle("GET /repos/acme/widgets/pulls/8/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{map[string]any{"commit": map[string]any{"message": "fix: a typo"}}})
	})
	written, err := ReadThroughCommitPulls(ctx, pool, fake.newTestClient(), "acme/widgets",
		"5555555555555555555555555555555555555555", time.Now())
	if err != nil {
		t.Fatalf("read through the commit's pull requests: %v", err)
	}
	if written != 2 {
		t.Fatalf("the read-through wrote %d rows, want 2", written)
	}
	open := readHead(t, ctx, pool, "acme/widgets", 7)
	if open.State != "open" || open.IssueKey == nil || *open.IssueKey != "HEAD-2" {
		t.Fatalf("#7 = %+v, want an open row attributed to HEAD-2", open)
	}
	merged := readHead(t, ctx, pool, "acme/widgets", 8)
	if merged.State != "merged" || merged.IssueKey != nil {
		t.Fatalf("#8 = %+v, want a merged row attributed to nobody", merged)
	}
	// Only a head of a pull request attributed to the issue answers for it.
	attributed, err := HeadAttributedTo(ctx, pool, "HEAD-2", "5555555555555555555555555555555555555555")
	if err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(HEAD-2) = %v, %v; want true", attributed, err)
	}
	other, err := HeadAttributedTo(ctx, pool, "HEAD-1", "5555555555555555555555555555555555555555")
	if err != nil || other {
		t.Fatalf("HeadAttributedTo(HEAD-1) = %v, %v; want false", other, err)
	}
}

// A pull request's title, body and branch are text any account that can open one writes, and an
// attributed head row is what makes a `before_merge` result read current, so an issue that
// resolves to another repository attributes nothing: the row is still written, with no issue.
func TestAPullRequestNamingAnIssueOfAnotherRepositoryAttributesNobody(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedHeadIssue(t, ctx, pool, 3)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/other/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{map[string]any{
			"number": 11, "title": "HEAD-3 the work", "body": "", "html_url": "https://github.com/acme/other/pull/11",
			"state": "open", "merged": false, "updated_at": "2026-02-01T00:00:00Z",
			"head": map[string]any{
				"ref": "legion/HEAD-3", "sha": "7777777777777777777777777777777777777777",
				"repo": map[string]any{"full_name": "acme/other"},
			},
			"base": map[string]any{"ref": "main"},
		}})
	})
	fake.handle("GET /repos/acme/other/pulls/11/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{map[string]any{"commit": map[string]any{"message": "work"}}})
	})
	if _, err := ReadThroughCommitPulls(ctx, pool, fake.newTestClient(), "acme/other",
		"7777777777777777777777777777777777777777", time.Now()); err != nil {
		t.Fatalf("read through the commit's pull requests: %v", err)
	}
	head := readHead(t, ctx, pool, "acme/other", 11)
	if head.IssueKey != nil || head.AttributionSource != "none" {
		t.Fatalf("#11 = %+v, want a row attributed to nobody: HEAD-3 resolves to acme/widgets", head)
	}
	attributed, err := HeadAttributedTo(ctx, pool, "HEAD-3", "7777777777777777777777777777777777777777")
	if err != nil || attributed {
		t.Fatalf("HeadAttributedTo(HEAD-3) = %v, %v; want false", attributed, err)
	}
}

// A title that names another repository's issue before this repository's own must not cost the
// pull request its attribution. The attribution takes the first ranked candidate and nothing
// after it (attributionSelect's `limit 1`), so a title naming two issues loses the one that
// belongs here, and an attributed head is exactly what makes a `before_merge` result at that
// head read current (HeadAttributedTo, the question api/results_currency.go asks). Both writers
// of the row answer for it: the `pull_request` envelope and the read-through a recorded result
// makes.
func TestAPullRequestTitledAfterAnotherRepositorysIssueKeepsItsOwn(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	foreign := seedHeadIssue(t, ctx, pool, 4)
	own := seedGadgetsIssue(t, ctx, pool, 4)

	const sha = "8888888888888888888888888888888888888888"
	title := foreign + ": the other repository's issue, and " + own + " (this one)"

	fake := newFakeGitHub(t)
	// While the defect stands the four cheap sources name nobody, so both paths page the commit
	// messages, the fifth. How often they do is no part of this test: what the row holds is.
	fake.handle("GET /repos/acme/gadgets/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{})
	})
	fake.handle("GET /repos/acme/gadgets/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{map[string]any{
			"number": 22, "title": title, "body": "", "html_url": "https://github.com/acme/gadgets/pull/22",
			"state": "open", "merged": false, "updated_at": "2026-02-05T00:00:00Z",
			"head": map[string]any{
				"ref": "legion/" + own, "sha": sha,
				"repo": map[string]any{"full_name": "acme/gadgets"},
			},
			"base": map[string]any{"ref": "main"},
		}})
	})
	client := fake.newTestClient()

	// The envelope path: #21, reported by a `pull_request` event.
	intake := newIntake(nil, pool, deliveryTestLeases(t, pool), client, events.NewBroker(), nil)
	if _, err := intake.recordPullRequestHead(ctx, map[string]string{
		"kind": "pr", "action": "opened", "merged": "false", "repo": "acme/gadgets", "number": "21",
		"title": title, "body": "", "head_ref": "legion/" + own, "head_repo": "acme/gadgets",
		"head_sha": sha, "base_ref": "main", "url": "https://github.com/acme/gadgets/pull/21",
		"updated_at": "2026-02-05T00:00:00Z",
	}, 21); err != nil {
		t.Fatalf("record the head the envelope reports: %v", err)
	}
	fromEnvelope := readHead(t, ctx, pool, "acme/gadgets", 21)
	if fromEnvelope.IssueKey == nil || *fromEnvelope.IssueKey != own || fromEnvelope.AttributionSource != "title_body" {
		t.Fatalf("#21 = %+v, want %s from its title: %s resolves to acme/widgets, %s to this repository",
			fromEnvelope, own, foreign, own)
	}

	// The read-through path: #22, the same title, learned from the commit alone.
	if _, err := ReadThroughCommitPulls(ctx, pool, client, "acme/gadgets", sha, time.Now()); err != nil {
		t.Fatalf("read through the commit's pull requests: %v", err)
	}
	fromReadThrough := readHead(t, ctx, pool, "acme/gadgets", 22)
	if fromReadThrough.IssueKey == nil || *fromReadThrough.IssueKey != own || fromReadThrough.AttributionSource != "title_body" {
		t.Fatalf("#22 = %+v, want %s from its title", fromReadThrough, own)
	}

	// What the results route asks of this package.
	attributed, err := HeadAttributedTo(ctx, pool, own, sha)
	if err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s) = %v, %v; want true", own, attributed, err)
	}
	elsewhere, err := HeadAttributedTo(ctx, pool, foreign, sha)
	if err != nil || elsewhere {
		t.Fatalf("HeadAttributedTo(%s) = %v, %v; want false: it is another repository's issue",
			foreign, elsewhere, err)
	}
}

// The three reads a result's currency turns on, against this package's REST fake: the pull
// requests GitHub names for a commit (delivery's own, through readGitHubPage's bounded retry),
// whether a commit is on a branch, and the default branch. A repository the installation cannot
// read is ErrRepositoryUnreadable whichever read asks, which the currency read takes as unknown
// rather than as older.
func TestCommitPullsAndCommitOnBranchReadThroughTheApp(t *testing.T) {
	fastPageRetries(t)
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/commits/{sha}/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("sha") == "0000000000000000000000000000000000000000" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"No commit found"}`)
			return
		}
		mustEncode(t, w, []any{map[string]any{
			"number": 3, "title": "A change", "body": "b", "html_url": "https://github.com/acme/widgets/pull/3",
			"state": "closed", "merged_at": "2026-02-02T00:00:00Z", "updated_at": "2026-02-02T00:00:00Z",
			"head": map[string]any{"ref": "topic", "sha": "6666666666666666666666666666666666666666"},
			"base": map[string]any{"ref": "main"},
		}})
	})
	fake.handle("GET /repos/acme/widgets/compare/{basehead}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("basehead") {
		case "main...6666666666666666666666666666666666666666":
			mustEncode(t, w, map[string]any{"status": "behind"})
		case "main...7777777777777777777777777777777777777777":
			mustEncode(t, w, map[string]any{"status": "identical"})
		default:
			mustEncode(t, w, map[string]any{"status": "diverged"})
		}
	})
	fake.handle("GET /repos/acme/hidden/compare/{basehead}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	fake.handle("GET /repos/acme/hidden/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	})
	fake.handle("GET /repos/acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{"full_name": "acme/widgets", "default_branch": "main"})
	})
	client := fake.newTestClient()
	ctx := t.Context()
	token, err := client.RepositoryToken(ctx, "acme", "widgets")
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}

	pulls, err := fetchCommitPullPayloads(ctx, client, token, "acme", "widgets", "6666666666666666666666666666666666666666")
	if err != nil {
		t.Fatalf("fetch the pull requests of a commit: %v", err)
	}
	if len(pulls) != 1 || pulls[0].Number != 3 || pulls[0].MergedAt == nil || pulls[0].Base.Ref != "main" {
		t.Fatalf("the pull requests of a commit = %+v, want one merged pull request #3 onto main", pulls)
	}
	if none, err := fetchCommitPullPayloads(ctx, client, token, "acme", "widgets", "0000000000000000000000000000000000000000"); err != nil || len(none) != 0 {
		t.Fatalf("the pull requests of a commit the repository lacks = %+v, %v; want none and no error", none, err)
	}
	if _, err := fetchCommitPullPayloads(ctx, client, token, "acme", "hidden", "6666666666666666666666666666666666666666"); !errors.Is(err, githubapp.ErrRepositoryUnreadable) {
		t.Fatalf("the pull requests of a commit of an unreadable repository = %v, want ErrRepositoryUnreadable", err)
	}

	for sha, want := range map[string]bool{
		"6666666666666666666666666666666666666666": true,
		"7777777777777777777777777777777777777777": true,
		"8888888888888888888888888888888888888888": false,
	} {
		got, err := client.CommitOnBranch(ctx, token, "acme", "widgets", "main", sha)
		if err != nil {
			t.Fatalf("CommitOnBranch(%s): %v", sha, err)
		}
		if got != want {
			t.Fatalf("CommitOnBranch(%s) = %v, want %v", sha, got, want)
		}
	}
	if branch, err := client.DefaultBranch(ctx, token, "acme", "widgets"); err != nil || branch != "main" {
		t.Fatalf("DefaultBranch = %q, %v; want main", branch, err)
	}
	if _, err := client.CommitOnBranch(ctx, token, "acme", "hidden", "main", "6666666666666666666666666666666666666666"); !errors.Is(err, githubapp.ErrRepositoryUnreadable) {
		t.Fatalf("CommitOnBranch on an unreadable repository = %v, want ErrRepositoryUnreadable", err)
	}
}

// Security, the read-through path: anyone may open a pull request from a fork of a public
// repository, at any commit of their choosing, with any title. A fork's head is another
// repository's, so the row is written but attributed to nobody, and a result recorded at that
// commit is never made current by it.
func TestAForkPullRequestNamedForACommitMakesNoResultCurrent(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedHeadIssue(t, ctx, pool, 4)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{map[string]any{
			"number": 21, "title": "HEAD-4 the work", "body": "Fixes HEAD-4",
			"html_url": "https://github.com/acme/widgets/pull/21",
			"state":    "open", "merged": false, "updated_at": "2026-02-03T00:00:00Z",
			"head": map[string]any{
				"ref": "legion/HEAD-4", "sha": "9999999999999999999999999999999999999999",
				"repo": map[string]any{"full_name": "stranger/widgets"},
			},
			"base": map[string]any{"ref": "main"},
		}})
	})
	fake.handle("GET /repos/acme/widgets/pulls/21/commits", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the read-through paged a fork's commit messages; a fork attributes nobody whatever they say")
		mustEncode(t, w, []any{})
	})
	if _, err := ReadThroughCommitPulls(ctx, pool, fake.newTestClient(), "acme/widgets",
		"9999999999999999999999999999999999999999", time.Now()); err != nil {
		t.Fatalf("read through the commit's pull requests: %v", err)
	}
	head := readHead(t, ctx, pool, "acme/widgets", 21)
	if head.IssueKey != nil || head.AttributionSource != "none" {
		t.Fatalf("#21 = %+v, want a row attributed to nobody: its head is a fork's", head)
	}
	attributed, err := HeadAttributedTo(ctx, pool, "HEAD-4", "9999999999999999999999999999999999999999")
	if err != nil || attributed {
		t.Fatalf("HeadAttributedTo(HEAD-4) = %v, %v; want false", attributed, err)
	}
}

// Security, the envelope path: the same fork, reported by a `pull_request` event. An envelope
// that names no head repository at all — one an older publisher made — attributes nobody
// either, and leaves the row unstamped for the reconcile to settle from GitHub's own answer.
func TestAForkPullRequestEnvelopeMakesNoResultCurrent(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedHeadIssue(t, ctx, pool, 5)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("an envelope whose head is not this repository's paged its commit messages; it attributes nobody whatever they say")
		mustEncode(t, w, []any{})
	})
	intake := newIntake(nil, pool, nil, fake.newTestClient(), nil, nil)
	envelope := func(number int, fields map[string]string) map[string]string {
		payload := map[string]string{
			"kind": "pr", "action": "opened", "repo": "acme/widgets", "number": strconv.Itoa(number),
			"title": "HEAD-5 the work", "body": "", "head_ref": "legion/HEAD-5",
			"head_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "base_ref": "main",
			"merged": "false", "url": "https://github.com/acme/widgets/pull/" + strconv.Itoa(number),
			"updated_at": "2026-02-04T00:00:00Z",
		}
		for key, value := range fields {
			payload[key] = value
		}
		return payload
	}
	if _, err := intake.recordPullRequestHead(ctx, envelope(22, map[string]string{"head_repo": "stranger/widgets"}), 22); err != nil {
		t.Fatalf("record the fork's head: %v", err)
	}
	if _, err := intake.recordPullRequestHead(ctx, envelope(23, nil), 23); err != nil {
		t.Fatalf("record the head of an envelope naming no head repository: %v", err)
	}

	fork := readHead(t, ctx, pool, "acme/widgets", 22)
	if fork.IssueKey != nil || fork.AttributionSource != "none" {
		t.Fatalf("#22 = %+v, want a row attributed to nobody: its head is a fork's", fork)
	}
	unsaid := readHead(t, ctx, pool, "acme/widgets", 23)
	if unsaid.IssueKey != nil || unsaid.AttributionSource != "none" {
		t.Fatalf("#23 = %+v, want a row attributed to nobody: the envelope names no head repository", unsaid)
	}
	if unsaid.CheckedAt != nil {
		t.Fatalf("#23's checked_at = %v, want it unstamped so the reconcile reads the head repository itself", unsaid.CheckedAt)
	}
	attributed, err := HeadAttributedTo(ctx, pool, "HEAD-5", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil || attributed {
		t.Fatalf("HeadAttributedTo(HEAD-5) = %v, %v; want false", attributed, err)
	}

	// The same pull request from a branch of this repository is attributed, so the refusal is
	// the head's repository and nothing else.
	if _, err := intake.recordPullRequestHead(ctx, envelope(24, map[string]string{"head_repo": "Acme/Widgets"}), 24); err != nil {
		t.Fatalf("record the head of a branch of this repository: %v", err)
	}
	own := readHead(t, ctx, pool, "acme/widgets", 24)
	if own.IssueKey == nil || *own.IssueKey != "HEAD-5" || own.AttributionSource != "title_body" {
		t.Fatalf("#24 = %+v, want HEAD-5 from its title: its head is a branch of this repository", own)
	}
}

// An attribution the read-through cannot make from a title, body or branch is not deleted by
// it. The read-through reads the commit messages, the fifth source, exactly as the event path
// does, so a merged row attributed by them — which the reconcile never revisits — keeps its
// issue when another result's read-through lists it again. #32 is the same row when that read
// fails with something other than a rate limit: an incomplete read attributes nobody, and
// nobody is not an answer it may write over the issue the row holds.
func TestAReadThroughKeepsAnAttributionOnlyTheCommitMessagesName(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedHeadIssue(t, ctx, pool, 6)

	merged := time.Date(2026, 2, 5, 0, 0, 0, 0, time.UTC)
	issue := "HEAD-6"
	for _, number := range []int{31, 32} {
		if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
			Repo: "acme/widgets", Number: number, HeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			HeadRef: "topic", BaseRef: "main", State: "merged", IssueKey: &issue,
			AttributionSource: string(sourceCommitMessage), AttributionComplete: true,
			EventAt: &merged, CheckedAt: &merged,
		}); err != nil {
			t.Fatalf("store the merged head of #%d attributed by its commit messages: %v", number, err)
		}
	}

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/commits/{sha}/pulls", func(w http.ResponseWriter, _ *http.Request) {
		pull := func(number int) map[string]any {
			return map[string]any{
				"number": number, "title": "Tidy the walk", "body": "",
				"html_url": fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
				"state":    "closed", "merged_at": "2026-02-05T00:00:00Z", "updated_at": "2026-02-05T00:00:00Z",
				"head": map[string]any{
					"ref": "topic", "sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
					"repo": map[string]any{"full_name": "acme/widgets"},
				},
				"base": map[string]any{"ref": "main"},
			}
		}
		mustEncode(t, w, []any{pull(31), pull(32)})
	})
	fake.handle("GET /repos/acme/widgets/pulls/31/commits", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, []any{map[string]any{"commit": map[string]any{"message": "HEAD-6: tidy the walk"}}})
	})
	// #32's commit messages are refused, which is no rate limit: the read-through writes the row
	// incomplete, so it stamps nothing and deletes nothing.
	fake.handle("GET /repos/acme/widgets/pulls/32/commits", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	})
	if _, err := ReadThroughCommitPulls(ctx, pool, fake.newTestClient(), "acme/widgets",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", time.Now()); err != nil {
		t.Fatalf("read through the commit's pull requests: %v", err)
	}
	head := readHead(t, ctx, pool, "acme/widgets", 31)
	if head.IssueKey == nil || *head.IssueKey != "HEAD-6" || head.AttributionSource != string(sourceCommitMessage) {
		t.Fatalf("#31 = %+v, want HEAD-6 kept from its commit messages", head)
	}
	attributed, err := HeadAttributedTo(ctx, pool, "HEAD-6", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(HEAD-6) = %v, %v; want true: the merged head still answers for the issue", attributed, err)
	}
	unread := readHead(t, ctx, pool, "acme/widgets", 32)
	if unread.IssueKey == nil || *unread.IssueKey != "HEAD-6" || unread.AttributionSource != string(sourceCommitMessage) {
		t.Fatalf("#32 = %+v, want HEAD-6 kept: a read that could not page the commit messages attributes nobody and deletes nobody", unread)
	}
	if unread.CheckedAt == nil || !unread.CheckedAt.Equal(merged) {
		t.Fatalf("#32's checked_at = %v, want the stamp it had: this read-through was no complete read of it", unread.CheckedAt)
	}
}

// A row whose repository the App cannot read is stamped, not warned about forever: the reconcile
// orders the never-read first, so an unstamped row holds the front of every batch.
func TestAnUnreadableRepositoryStampsThePullRequestHead(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	seedDeliverySettings(t, ctx, pool)

	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/hidden/installation", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	fake.handle("GET /repos/acme/walled/pulls/51", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
	})
	for _, repo := range []string{"acme/hidden", "acme/walled"} {
		if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
			Repo: repo, Number: 51, HeadSHA: "cccccccccccccccccccccccccccccccccccccccc",
			HeadRef: "topic", BaseRef: "main", State: "open",
		}); err != nil {
			t.Fatalf("store %s's never-read head: %v", repo, err)
		}
	}

	now := time.Now()
	reconcile := NewReconcile(pool, fake.newTestClient(), nil)
	if err := reconcile.reconcilePullRequestHeads(ctx, now); err != nil {
		t.Fatalf("reconcile the pull request heads: %v", err)
	}
	for _, repo := range []string{"acme/hidden", "acme/walled"} {
		if head := readHead(t, ctx, pool, repo, 51); head.CheckedAt == nil {
			t.Fatalf("%s#51 = %+v, want it stamped read so it leaves the front of the batch", repo, head)
		}
	}
	stale, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, now)
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	if len(stale) != 0 {
		t.Fatalf("the next pass would re-read %+v, want nothing within the stale window", stale)
	}
}

// A merge reported by a `pull_request` envelope that names no head repository is attributed by
// the next reconcile pass, and by that pass alone. The envelope attributes nobody, since a head
// it cannot show to be this repository's is a fork's as far as Dispatch knows, and nothing but
// the reconcile ever looks at a merged row again: without the merged arm of
// ListStalePullRequestHeads the issue's results at its own merged head would read `older` for
// good. HeadAttributedTo is the currency read api/results.go's currencyReader makes of this
// package, so the test asserts currency through it rather than standing an API up.
func TestReconcileAttributesAMergedHeadTheEnvelopeLeftUnattributed(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	settings := seedDeliverySettings(t, ctx, pool)
	issue := seedHeadIssue(t, ctx, pool, 7)

	const mergedHead = "cccccccccccccccccccccccccccccccccccccccc"
	var pullReads atomic.Int32
	fake := newFakeGitHub(t)
	// GitHub's own answer always names head.repo, which is what attributes the row.
	fake.handle("GET /repos/acme/widgets/pulls/61", func(w http.ResponseWriter, _ *http.Request) {
		pullReads.Add(1)
		mustEncode(t, w, map[string]any{
			"number": 61, "title": "HEAD-7 the work", "body": "",
			"html_url": "https://github.com/acme/widgets/pull/61",
			"state":    "closed", "merged": true, "merged_at": "2026-02-06T00:00:00Z",
			"updated_at": "2026-02-06T00:00:00Z",
			"head": map[string]any{
				"ref": "legion/HEAD-7", "sha": mergedHead,
				"repo": map[string]any{"full_name": "acme/widgets"},
			},
			"base": map[string]any{"ref": "main"},
		})
	})
	// #64's head is a fork's, so GitHub's answer attributes it to nobody either: the row is
	// taken once all the same, and stamped, which is the whole of the bound.
	var forkReads atomic.Int32
	fake.handle("GET /repos/acme/widgets/pulls/64", func(w http.ResponseWriter, _ *http.Request) {
		forkReads.Add(1)
		mustEncode(t, w, map[string]any{
			"number": 64, "title": "HEAD-7 the work", "body": "",
			"html_url": "https://github.com/acme/widgets/pull/64",
			"state":    "closed", "merged": true, "merged_at": "2026-02-06T00:00:00Z",
			"updated_at": "2026-02-06T00:00:00Z",
			"head": map[string]any{
				"ref": "legion/HEAD-7", "sha": "ffffffffffffffffffffffffffffffffffffffff",
				"repo": map[string]any{"full_name": "stranger/widgets"},
			},
			"base": map[string]any{"ref": "main"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("#%s's commit messages were paged; its title attributes it, and an envelope naming no head repository attributes nobody whatever they say", r.PathValue("number"))
		mustEncode(t, w, []any{})
	})
	client := fake.newTestClient()

	// The merge, as an envelope a publisher older than head_repo made. Its author is outside the
	// delivery population, so the envelope writes the head row and nothing else.
	intake := newIntake(nil, pool, nil, client, nil, nil)
	wrote, err := intake.handlePullRequestEnvelope(ctx, settings, map[string]string{
		"kind": "pr", "action": "closed", "merged": "true", "repo": "acme/widgets", "number": "61",
		"title": "HEAD-7 the work", "body": "", "head_ref": "legion/HEAD-7", "head_sha": mergedHead,
		"base_ref": "main", "author": "stranger", "updated_at": "2026-02-06T00:00:00Z",
		"url": "https://github.com/acme/widgets/pull/61",
	})
	if err != nil || wrote {
		t.Fatalf("handle the merge envelope = %v, %v; want it to write the head row alone", wrote, err)
	}
	stored := readHead(t, ctx, pool, "acme/widgets", 61)
	if stored.State != "merged" || stored.HeadSHA != mergedHead {
		t.Fatalf("#61 = %+v, want the merged head the envelope reported", stored)
	}
	if stored.IssueKey != nil || stored.AttributionSource != "none" {
		t.Fatalf("#61 = %+v, want it attributed to nobody: the envelope names no head repository", stored)
	}
	if stored.CheckedAt != nil {
		t.Fatalf("#61's checked_at = %v, want it unstamped: no read of GitHub has reached it", stored.CheckedAt)
	}
	if attributed, err := HeadAttributedTo(ctx, pool, issue, mergedHead); err != nil || attributed {
		t.Fatalf("HeadAttributedTo(%s) before the pass = %v, %v; want false", issue, attributed, err)
	}

	// The bound, from the other side: a merged row that carries an attribution, and one already
	// read and left unattributed, are no business of the pass.
	eventAt := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: "acme/widgets", Number: 62, HeadSHA: "dddddddddddddddddddddddddddddddddddddddd",
		HeadRef: "legion/HEAD-7", BaseRef: "main", State: "merged", IssueKey: &issue,
		AttributionSource: "title_body", EventAt: &eventAt,
	}); err != nil {
		t.Fatalf("store the attributed merged head: %v", err)
	}
	read := time.Now().Add(-time.Hour)
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: "acme/widgets", Number: 63, HeadSHA: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		HeadRef: "topic", BaseRef: "main", State: "merged", EventAt: &eventAt, CheckedAt: &read,
	}); err != nil {
		t.Fatalf("store the merged head read while it was open: %v", err)
	}
	if err := UpsertPullRequestHead(ctx, pool, PullRequestHead{
		Repo: "acme/widgets", Number: 64, HeadSHA: "ffffffffffffffffffffffffffffffffffffffff",
		HeadRef: "legion/HEAD-7", BaseRef: "main", State: "merged", EventAt: &eventAt,
	}); err != nil {
		t.Fatalf("store the merged head of a fork's pull request: %v", err)
	}
	batch, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	// Not fatal: the pass below is the behaviour this test is for, and its own assertions say
	// what an empty batch costs the issue.
	if len(batch) != 2 || batch[0].Number != 61 || batch[1].Number != 64 {
		t.Errorf("the batch = %+v, want #61 and #64: an attributed merged row and one already read are not taken", batch)
	}

	reconcile := NewReconcile(pool, client, nil)
	if err := reconcile.reconcilePullRequestHeads(ctx, time.Now()); err != nil {
		t.Fatalf("reconcile the pull request heads: %v", err)
	}
	settled := readHead(t, ctx, pool, "acme/widgets", 61)
	if settled.IssueKey == nil || *settled.IssueKey != issue || settled.AttributionSource != "title_body" {
		t.Fatalf("#61 after the pass = %+v, want %s from its title", settled, issue)
	}
	if settled.State != "merged" || settled.HeadSHA != mergedHead {
		t.Fatalf("#61 after the pass = %+v, want the merged head unchanged", settled)
	}
	if settled.CheckedAt == nil {
		t.Fatalf("#61 after the pass = %+v, want it stamped read", settled)
	}
	attributed, err := HeadAttributedTo(ctx, pool, issue, mergedHead)
	if err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s) after the pass = %v, %v; want true: a result at the merged head is current, not older", issue, attributed, err)
	}
	if unread := readHead(t, ctx, pool, "acme/widgets", 62); unread.CheckedAt != nil {
		t.Fatalf("#62 = %+v, want it never read: it carries an attribution already", unread)
	}
	if reads := pullReads.Load(); reads != 1 {
		t.Fatalf("the pass read #61 from GitHub %d times, want once", reads)
	}
	fork := readHead(t, ctx, pool, "acme/widgets", 64)
	if fork.IssueKey != nil || fork.AttributionSource != "none" {
		t.Fatalf("#64 after the pass = %+v, want it still attributed to nobody: its head is a fork's", fork)
	}
	if fork.CheckedAt == nil {
		t.Fatalf("#64 after the pass = %+v, want it stamped read even though the read attributed nobody", fork)
	}

	// Once, and once only: the refresh stamps the row whether or not it could attribute it, so
	// neither row comes back to the list.
	if err := reconcile.reconcilePullRequestHeads(ctx, time.Now()); err != nil {
		t.Fatalf("reconcile a second time: %v", err)
	}
	if reads := pullReads.Load(); reads != 1 {
		t.Fatalf("a second pass read #61 from GitHub again (%d reads in all), want the row taken once", reads)
	}
	if reads := forkReads.Load(); reads != 1 {
		t.Fatalf("a second pass read #64 from GitHub again (%d reads in all), want the row taken once though it is still attributed to nobody", reads)
	}
	again, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads again: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a third pass would read %+v, want nothing", again)
	}
}

// The other side of that merged arm: an envelope naming no head repository is an incomplete
// read, so it attributes nobody but writes over nobody's issue either. The listener ships from
// its own release workflow, so until it is redeployed every envelope it publishes is such a
// one — and the merge of a pull request an earlier `opened` envelope attributed and stamped
// would otherwise land as "attributed to nobody, read already": out of the merged arm, which
// takes only rows no read has reached, and `older` for good at the issue's own merged head.
func TestAMergeEnvelopeNamingNoHeadRepositoryKeepsTheStoredAttribution(t *testing.T) {
	fastPageRetries(t)
	pool, ctx := deliveryTestPool(t)
	issue := seedHeadIssue(t, ctx, pool, 8)

	const openHead = "1010101010101010101010101010101010101010"
	const mergedHead = "1212121212121212121212121212121212121212"
	fake := newFakeGitHub(t)
	fake.handle("GET /repos/acme/widgets/pulls/{number}/commits", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("#%s's commit messages were paged; its title attributes it, and an envelope naming no head repository attributes nobody whatever they say", r.PathValue("number"))
		mustEncode(t, w, []any{})
	})
	intake := newIntake(nil, pool, nil, fake.newTestClient(), nil, nil)
	envelope := func(action, merged, headSHA, updatedAt string, fields map[string]string) map[string]string {
		payload := map[string]string{
			"kind": "pr", "action": action, "merged": merged, "repo": "acme/widgets", "number": "71",
			"title": "HEAD-8 the work", "body": "", "head_ref": "legion/HEAD-8", "head_sha": headSHA,
			"base_ref": "main", "url": "https://github.com/acme/widgets/pull/71", "updated_at": updatedAt,
		}
		for key, value := range fields {
			payload[key] = value
		}
		return payload
	}

	// The `opened` envelope of a listener that carries head_repo: a complete read, which
	// attributes the row from its title and stamps it.
	if _, err := intake.recordPullRequestHead(ctx, envelope("opened", "false", openHead,
		"2026-02-07T00:00:00Z", map[string]string{"head_repo": "acme/widgets"}), 71); err != nil {
		t.Fatalf("record the opened head: %v", err)
	}
	opened := readHead(t, ctx, pool, "acme/widgets", 71)
	if opened.IssueKey == nil || *opened.IssueKey != issue || opened.AttributionSource != "title_body" {
		t.Fatalf("#71 = %+v, want %s from its title", opened, issue)
	}
	if opened.CheckedAt == nil {
		t.Fatalf("#71's checked_at = %v, want it stamped: the envelope was a complete read", opened.CheckedAt)
	}

	// The merge, from the same listener before its redeploy: no head_repo, so an incomplete read.
	if _, err := intake.recordPullRequestHead(ctx, envelope("closed", "true", mergedHead,
		"2026-02-08T00:00:00Z", nil), 71); err != nil {
		t.Fatalf("record the merged head of an envelope naming no head repository: %v", err)
	}
	settled := readHead(t, ctx, pool, "acme/widgets", 71)
	if settled.State != "merged" || settled.HeadSHA != mergedHead {
		t.Fatalf("#71 after the merge = %+v, want the merged head the envelope reported", settled)
	}
	if settled.IssueKey == nil || *settled.IssueKey != issue || settled.AttributionSource != "title_body" {
		t.Fatalf("#71 after the merge = %+v, want %s still: an incomplete write deletes no attribution", settled, issue)
	}
	attributed, err := HeadAttributedTo(ctx, pool, issue, mergedHead)
	if err != nil || !attributed {
		t.Fatalf("HeadAttributedTo(%s) at the merged head = %v, %v; want true: a result there is current, not older", issue, attributed, err)
	}

	// And the reconcile is no safety net to lean on here: the row is stamped and attributed, so
	// the merged arm never takes it.
	batch, err := ListStalePullRequestHeads(ctx, pool, PullRequestHeadBatch, time.Now())
	if err != nil {
		t.Fatalf("list the stale heads: %v", err)
	}
	if len(batch) != 0 {
		t.Fatalf("the stale batch = %+v, want nothing: the merged row is attributed and stamped already", batch)
	}
}
