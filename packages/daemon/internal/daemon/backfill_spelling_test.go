package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

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
