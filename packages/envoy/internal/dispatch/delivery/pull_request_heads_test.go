package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/githubapp"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// seedHeadIssue records one issue the attribution can name, with a project of its own. The key
// is HEAD-<n>, since an issue's number is its key's.
func seedHeadIssue(t *testing.T, ctx context.Context, pool *store.Pool, number int) string {
	t.Helper()
	key := fmt.Sprintf("HEAD-%d", number)
	if _, err := pool.Exec(ctx, `
		insert into projects (key, name) values ('HEAD', 'Heads') on conflict (key) do nothing
	`); err != nil {
		t.Fatalf("seed the project: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, status, created_by, rank)
		values ($1, 'HEAD', $2, $1, 'todo', '{"kind":"system","id":"test"}'::jsonb, $1)
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
// writes the head GitHub reports, with the issue the attribution picks.
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
			"head": map[string]any{"ref": "legion/HEAD-1", "sha": "2222222222222222222222222222222222222222"},
			"base": map[string]any{"ref": "main"},
		})
	})
	fake.handle("GET /repos/acme/widgets/pulls/42/commits", func(w http.ResponseWriter, _ *http.Request) {
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
				"head": map[string]any{"ref": "legion/HEAD-2", "sha": "5555555555555555555555555555555555555555"},
				"base": map[string]any{"ref": "main"},
			},
			map[string]any{
				"number": 8, "title": "An unrelated change", "body": "", "html_url": "https://github.com/acme/widgets/pull/8",
				"state": "closed", "merged_at": "2026-02-02T00:00:00Z", "updated_at": "2026-02-02T00:00:00Z",
				"head": map[string]any{"ref": "fix", "sha": "5555555555555555555555555555555555555555"},
				"base": map[string]any{"ref": "main"},
			},
		})
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

// The App's two new reads, against this package's REST fake: the pull requests of a commit, and
// whether a commit is on a branch. A repository the installation cannot read is
// ErrRepositoryUnreadable, which the currency read takes as unknown rather than as older.
func TestCommitPullsAndCommitOnBranchReadThroughTheApp(t *testing.T) {
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
	fake.handle("GET /repos/acme/widgets", func(w http.ResponseWriter, _ *http.Request) {
		mustEncode(t, w, map[string]any{"full_name": "acme/widgets", "default_branch": "main"})
	})
	client := fake.newTestClient()
	ctx := t.Context()
	token, err := client.RepositoryToken(ctx, "acme", "widgets")
	if err != nil {
		t.Fatalf("mint a token: %v", err)
	}

	pulls, err := client.CommitPulls(ctx, token, "acme", "widgets", "6666666666666666666666666666666666666666")
	if err != nil {
		t.Fatalf("CommitPulls: %v", err)
	}
	if len(pulls) != 1 || pulls[0].Number != 3 || !pulls[0].Merged || pulls[0].BaseRef != "main" {
		t.Fatalf("CommitPulls = %+v, want one merged pull request #3 onto main", pulls)
	}
	if none, err := client.CommitPulls(ctx, token, "acme", "widgets", "0000000000000000000000000000000000000000"); err != nil || len(none) != 0 {
		t.Fatalf("CommitPulls for a commit the repository lacks = %+v, %v; want none and no error", none, err)
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
