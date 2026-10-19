package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The backfill keeps a repository's spelling where a shared clone already spells it: an upgrade of
// a tmux daemon fills an issue it runs with the spelling of the shared clone whose owner and name
// match Dispatch's resolution case-insensitively, so the issue's workspace, which that clone's
// spelling named, is the one it keeps working in, its unpushed work there. With no clone, as under
// Kubernetes, whose pods provision their own, the record takes GitHub's spelling. The default
// branch is GitHub's either way.
func TestTheBackfillKeepsTheSpellingOfAnExistingClone(t *testing.T) {
	for _, tc := range []struct {
		name  string
		clone bool
		want  string
	}{
		{"a tmux daemon with the issue's clone and workspace", true, "acme/widgets"},
		{"a daemon with no clone", false, "Acme/Widgets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			pool := isolatedOutboxPool(t)
			records := record.NewStore()
			if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
				values ('LEGION-301', 'LEGION-301', 'LEGION', 'existing', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
				t.Fatal(err)
			}
			state := t.TempDir()
			before, err := workspace.Location(state, ghrepo.MustParse("acme/widgets"), "LEGION-301")
			if err != nil {
				t.Fatal(err)
			}
			if tc.clone {
				for _, dir := range []string{filepath.Join(before.Clone, ".git"), before.Dir} {
					if err := os.MkdirAll(dir, 0o700); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(before.Dir, "retained-work"), []byte("unpushed work"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@release/v7.1"})
			w := &workflowRuntime{pool: pool, records: records, defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)}
			client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-301", ResolvedRepo: "acme/widgets"}}}
			if err := backfillRepositories(ctx, pool, records, client, "LEGION", state, w.defaultBranches, quietLogger()); err != nil {
				t.Fatal(err)
			}
			repo, branch, err := w.issueRepository(ctx, "LEGION-301")
			if err != nil || repo.String() != tc.want || branch != "release/v7.1" {
				t.Fatalf("LEGION-301 is recorded on %s@%s, %v; want %s@release/v7.1", repo, branch, err, tc.want)
			}
			if !tc.clone {
				return
			}
			after, err := workspace.Location(state, repo, "LEGION-301")
			if err != nil {
				t.Fatal(err)
			}
			if after.Dir != before.Dir {
				t.Fatalf("the workspace after the backfill is %s, want the one the clone's spelling named, %s", after.Dir, before.Dir)
			}
			if work, err := os.ReadFile(filepath.Join(after.Dir, "retained-work")); err != nil || string(work) != "unpushed work" {
				t.Fatalf("the retained work after the backfill = %q, %v; want it where the issue keeps working", work, err)
			}
		})
	}
}

// Two shared clones that spell one repository in two ways leave the backfill no spelling to keep,
// so it refuses, naming both, rather than move an issue's workspace onto either by guess.
func TestTheBackfillRefusesTwoClonesOfOneRepository(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq)
		values ('LEGION-301', 'LEGION-301', 'LEGION', 'existing', 'implementing', 1, 'in_progress', 'A', 1)`); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	var clones []string
	for _, spelling := range []string{"acme/widgets", "Acme/Widgets"} {
		located, err := workspace.Location(state, ghrepo.MustParse(spelling), "LEGION-301")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(located.Clone, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		clones = append(clones, located.Clone)
	}
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "Acme/Widgets@main"})
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-301", ResolvedRepo: "acme/widgets"}}}
	err := backfillRepositories(ctx, pool, record.NewStore(), client, "LEGION", state,
		newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now), quietLogger())
	want := "fill the repository of LEGION-301: two shared clones spell acme/widgets (" + clones[1] + ", " + clones[0] + "); remove the one no issue works in and restart"
	if err == nil || err.Error() != want {
		t.Fatalf("backfill = %v, want %q", err, want)
	}
}

// A record written before the record carried its repository may already have a pull request,
// which names the repository the issue worked in. The fill takes Dispatch's resolution only when
// that pull request is on it, in any case: one on another repository (the issue's own repository
// or an ancestor's, set in Dispatch since, outranking the project's default) refuses the boot,
// naming the issue, both repositories and the fix, and fills nothing, since a record on the wrong
// repository would create branches there and disarm a stranger's pull request.
func TestTheBackfillRefusesARecordWhosePullRequestIsOnAnotherRepository(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"})
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-1", ResolvedRepo: "acme/widgets"}}}
	for _, tc := range []struct{ name, prRepo, want string }{
		{"on another repository", "zeta/tools", "fill the repository of LEGION-1: Dispatch resolves it to acme/widgets, but its pull request #7 is on zeta/tools: set the issue's repository in Dispatch to zeta/tools (dispatch issue-update --issue LEGION-1 --repo zeta/tools) and restart"},
		{"on the resolved repository in another case", "ACME/Widgets", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
				values ('LEGION-1', 'LEGION-1', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1, '', '')`); err != nil {
				t.Fatal(err)
			}
			records := record.NewStore()
			if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				return records.PutPullRequest(ctx, tx, record.PullRequest{Issue: "LEGION-1", Repo: tc.prRepo, Number: 7, Branch: "legion/LEGION-1",
					HeadSHA: "head", State: record.PullRequestOpen})
			}); err != nil {
				t.Fatal(err)
			}
			w := &workflowRuntime{pool: pool, records: records, log: quietLogger()}
			err := backfillRepositories(ctx, pool, records, client, "LEGION", t.TempDir(),
				newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now), quietLogger())
			got := recordedIssue(t, w, "LEGION-1")
			if tc.want != "" {
				if err == nil || err.Error() != tc.want {
					t.Fatalf("backfill = %v, want %q", err, tc.want)
				}
				if got.Repo != "" {
					t.Fatalf("LEGION-1 = %s after a refused backfill, want it unfilled", got.Repo)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Repo != "acme/widgets" || got.GitHubRepo != "acme/widgets" {
				t.Fatalf("LEGION-1 = %s (GitHub's %s), want acme/widgets", got.Repo, got.GitHubRepo)
			}
		})
	}
}

// A record written before the record carried its repository may have no pull request yet but a
// workspace a tmux daemon provisioned, which names the repository the issue works in. The fill
// refuses a resolution elsewhere, naming the workspace and the fix, rather than provision the issue
// anew in another repository and strand its unpushed work; a workspace under the resolved
// repository, in any case, fills.
func TestTheBackfillRefusesARecordWhoseWorkspaceIsInAnotherRepository(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/widgets": "acme/widgets@main", "zeta/tools": "zeta/tools@main"})
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-1", ResolvedRepo: "acme/widgets"}}}
	for _, tc := range []struct{ name, workspaceIn, want string }{
		{"in another repository", "zeta/tools", "fill the repository of LEGION-1: Dispatch resolves it to acme/widgets, but its workspace %s is in zeta/tools: set the issue's repository in Dispatch to zeta/tools (dispatch issue-update --issue LEGION-1 --repo zeta/tools) and restart"},
		{"in the resolved repository in another case", "ACME/Widgets", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
				values ('LEGION-1', 'LEGION-1', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1, '', '')`); err != nil {
				t.Fatal(err)
			}
			state := t.TempDir()
			location, err := workspace.Location(state, ghrepo.MustParse(tc.workspaceIn), "LEGION-1")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(location.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			records := record.NewStore()
			w := &workflowRuntime{pool: pool, records: records, log: quietLogger()}
			err = backfillRepositories(ctx, pool, records, client, "LEGION", state,
				newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now), quietLogger())
			got := recordedIssue(t, w, "LEGION-1")
			if tc.want != "" {
				if want := fmt.Sprintf(tc.want, location.Dir); err == nil || err.Error() != want {
					t.Fatalf("backfill = %v, want %q", err, want)
				}
				if got.Repo != "" {
					t.Fatalf("LEGION-1 = %s after a refused backfill, want it unfilled", got.Repo)
				}
				return
			}
			if err != nil || got.Repo != "acme/widgets" {
				t.Fatalf("backfill = %v, LEGION-1 = %s; want it filled with acme/widgets", err, got.Repo)
			}
		})
	}
}

// A repository GitHub renamed (acme/old, which GitHub answers with a 301 to acme/new) while
// Dispatch still resolves an issue to its old name: with no shared clone the record takes GitHub's
// new name; a shared clone under the old name, which the workspace would keep, refuses the boot
// naming what fixes it; and a record already on the old name with no GitHub spelling, which no
// later boot can rename, refuses the boot naming why.
func TestTheBackfillOfARepositoryGitHubRenamedNamesTheFix(t *testing.T) {
	ctx := context.Background()
	github := newNamedGitHub(t, map[string]string{"acme/old": "301 acme/new", "acme/new": "acme/new@main"})
	client := &listingDispatch{summaries: []dispatch.IssueSummary{{Key: "LEGION-1", ResolvedRepo: "acme/old"}}}
	seed := func(t *testing.T, repo, branch string) (*workflowRuntime, string) {
		t.Helper()
		pool := isolatedOutboxPool(t)
		if _, err := pool.Exec(ctx, `insert into issues (key, tree, project, title, phase, generation, status, rank, last_dispatch_seq, repo, default_branch)
			values ('LEGION-1', 'LEGION-1', 'LEGION', 'root', 'implementing', 1, 'in_progress', 'A', 1, $1, $2)`, repo, branch); err != nil {
			t.Fatal(err)
		}
		return &workflowRuntime{pool: pool, records: record.NewStore(), log: quietLogger(),
			defaultBranches: newDefaultBranches(roleTokens{}, github.url, defaultBranchTTL, time.Now)}, t.TempDir()
	}
	backfill := func(w *workflowRuntime, state string) error {
		return backfillRepositories(ctx, w.pool, w.records, client, "LEGION", state, w.defaultBranches, quietLogger())
	}

	t.Run("no shared clone", func(t *testing.T) {
		w, state := seed(t, "", "")
		if err := backfill(w, state); err != nil {
			t.Fatal(err)
		}
		if got := recordedIssue(t, w, "LEGION-1"); got.Repo != "acme/new" || got.DefaultBranch != "main" || got.GitHubRepo != "acme/new" {
			t.Fatalf("LEGION-1 = %s@%s (GitHub's %s), want acme/new@main (GitHub's acme/new)", got.Repo, got.DefaultBranch, got.GitHubRepo)
		}
	})

	t.Run("a shared clone under the old name", func(t *testing.T) {
		w, state := seed(t, "", "")
		clone, err := workspace.Location(state, ghrepo.MustParse("acme/old"), "LEGION-1")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(clone.Clone, ".git"), 0o700); err != nil {
			t.Fatal(err)
		}
		want := "fill the repository of LEGION-1: GitHub renamed acme/old to acme/new, and the shared clone " + clone.Clone +
			" still has the old name: map acme/new in Dispatch (Settings → Repositories → Projects), or remove that clone if no issue works in it, and restart"
		if err := backfill(w, state); err == nil || err.Error() != want {
			t.Fatalf("backfill = %v, want %q", err, want)
		}
		if got := recordedIssue(t, w, "LEGION-1"); got.Repo != "" || got.GitHubRepo != "" {
			t.Fatalf("LEGION-1 = %s (GitHub's %s) after a refused backfill, want it unfilled", got.Repo, got.GitHubRepo)
		}
	})

	t.Run("a record on the old name with no GitHub spelling", func(t *testing.T) {
		w, state := seed(t, "acme/old", "main")
		want := "name the repository of LEGION-1: GitHub renamed acme/old to acme/new, and a record's repository never changes; " +
			"only a pre-release build of this daemon leaves a record with no GitHub spelling, so recreate that build's database"
		if err := backfill(w, state); err == nil || err.Error() != want {
			t.Fatalf("backfill = %v, want %q", err, want)
		}
		if got := recordedIssue(t, w, "LEGION-1"); got.Repo != "acme/old" || got.GitHubRepo != "" {
			t.Fatalf("LEGION-1 = %s (GitHub's %s) after a refused backfill, want it unchanged", got.Repo, got.GitHubRepo)
		}
	})
}
