package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// Provision ports provisionIssueWorkspace (packages/workspace/src/workspace.ts). It updates an
// existing working copy, obtains the shared clone through a temporary sibling, protects unreachable
// worker commits before every fetch, resolves a bookmark before adding, and leaves pane credentials
// on the clone.
// The clone and the fetch reach the repository through request.Source: a pod's feed with no
// credential, or GitHub with the one-shot credential.
func Provision(ctx context.Context, run Runner, request Request) (Workspace, error) {
	workspace, err := Location(request.StateDir, request.Repo, request.Issue)
	if err != nil {
		return Workspace{}, err
	}
	if request.CredentialHelper == "" {
		return Workspace{}, fmt.Errorf("workspace credential helper is required")
	}
	if request.Log == nil {
		return Workspace{}, errors.New("workspace request names no log")
	}
	if request.Source == nil {
		return Workspace{}, errors.New("workspace request names no way to the repository: FromFeed or FromGitHub")
	}
	if err := request.Source.check(request.Repo); err != nil {
		return Workspace{}, err
	}

	exists, err := pathExists(workspace.Dir)
	if err != nil {
		return Workspace{}, err
	}
	if exists {
		if _, err := RunChecked(ctx, run, []string{"jj", "workspace", "update-stale"}, nil, workspace.Dir); err != nil {
			return Workspace{}, err
		}
	}
	source, err := request.Source.open(request.Repo, workspace.Bookmark)
	if err != nil {
		return Workspace{}, err
	}
	defer func() {
		_ = source.remove()
	}()
	if err := ensureRepoClone(ctx, run, workspace.Clone, GitHubURL(request.Repo), source.env); err != nil {
		return Workspace{}, err
	}
	if err := ensureFetchConfiguration(ctx, run, workspace.Clone, source); err != nil {
		return Workspace{}, err
	}
	if err := source.remove(); err != nil {
		return Workspace{}, err
	}

	if exists {
		if err := restoreGitWorktree(ctx, run, workspace, request.Log); err != nil {
			return Workspace{}, err
		}
	} else if err := createWorkspace(ctx, run, workspace, request.Log); err != nil {
		return Workspace{}, err
	}
	// Every workspace provisioning touches has its git worktree entry locked, one added before
	// provisioning locked any included, so a bare `git worktree prune` that cannot see it skips it.
	if err := lockGitWorktree(workspace.Clone, workspace.Dir); err != nil {
		return Workspace{}, err
	}
	if err := configureRepositoryCredential(ctx, run, workspace.Clone, request.CredentialHelper); err != nil {
		return Workspace{}, err
	}
	if err := removeRepositoryIdentity(ctx, run, workspace.Clone); err != nil {
		return Workspace{}, err
	}
	// Keeps every future `codegraph init`/`index` in this workspace — the host warm-up's, and a
	// worker's own `codegraph` tool call inside a pod, which this function also provisions
	// (cmd/legion workspace-init) — from ever getting its `.codegraph/` tracked. CodeGraph's own
	// generated `.codegraph/.gitignore` is `*` then `!.gitignore`, so that one file stays visible
	// to git, and jj's default auto-track snapshots it into the workspace's own change on a host
	// with no global ignore for `.codegraph/`. The clone's git directory's `info/exclude` is
	// read by every workspace of that clone (git worktrees share one `info/exclude` through
	// their common git directory) and by jj the same way, so this needs no global git
	// configuration anywhere a workspace of this clone is used.
	if err := excludeCodegraphDirectory(ctx, run, workspace.Clone); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

// excludeCodegraphDirectory appends ".codegraph/" to the clone's git directory's "info/exclude"
// unless a line already matches it exactly, so repeated provisioning of the same clone writes it
// once. The directory comes from `jj git root`, not a hardcoded `<cloneDir>/.git`: a
// non-colocated clone has no `.git` at all (its backing repository lives under
// `.jj/repo/store/git`), and the hardcoded path would both miss the directory jj and git
// actually read and leave a stray, unused `.git/` behind. The write only appends: once the
// exact-line check above has passed, there is nothing already in the file this could clobber,
// and O_APPEND never truncates on a path shared with other provisioning of the same clone.
func excludeCodegraphDirectory(ctx context.Context, run Runner, cloneDir string) error {
	root, err := RunChecked(ctx, run, onClone(cloneDir, "git", "root"), nil, "")
	if err != nil {
		return err
	}
	gitDir := strings.TrimSpace(root.Stdout)
	if !filepath.IsAbs(gitDir) {
		return fmt.Errorf("jj git root -R %s printed %q, want an absolute path", cloneDir, gitDir)
	}
	excludePath := filepath.Join(gitDir, "info", "exclude")
	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", excludePath, err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if line == ".codegraph/" {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(excludePath), err)
	}
	file, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", excludePath, err)
	}
	defer file.Close()
	prefix := ""
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	if _, err := file.WriteString(prefix + ".codegraph/\n"); err != nil {
		return fmt.Errorf("write %s: %w", excludePath, err)
	}
	return nil
}

// Bookmark is the jj bookmark an issue's workspace is on: its branch.
func Bookmark(issue string) string { return "legion/" + issue }

// Location is the deterministic workspace location Provision creates for one issue, and the shared
// clone it is a jj workspace of. The repository arrives parsed (ghrepo.Parse); a `.` or `..`
// issue is refused: joined under the repository's workspaces directory it names that directory,
// or its owner's, and Remove deletes a workspace directory whole.
func Location(stateDir string, repository ghrepo.Repository, issue string) (Workspace, error) {
	if repository.IsZero() {
		return Workspace{}, errors.New("workspace repository is required")
	}
	if stateDir == "" {
		return Workspace{}, fmt.Errorf("workspace state directory is required")
	}
	if issue == "." || issue == ".." {
		return Workspace{}, fmt.Errorf("workspace issue %q is a %q segment; want an issue key", issue, issue)
	}
	if issue == "" || issue != filepath.Base(issue) {
		return Workspace{}, fmt.Errorf("workspace issue must be a single path component")
	}
	return Workspace{
		Dir:      filepath.Join(stateDir, "workspaces", repository.Owner(), repository.Name(), strings.ToLower(issue)),
		Bookmark: Bookmark(issue),
		Clone:    filepath.Join(stateDir, "repos", "github.com", repository.Owner(), repository.Name()),
		Repo:     repository,
	}, nil
}

// located is whether workspace's directory and clone are the pair Location derives for the issue
// its directory names: <state>/workspaces/<owner>/<repo>/<issue> beside
// <state>/repos/github.com/<owner>/<repo>. The repository is read back from the clone's path, a
// string like any other input, so it is parsed.
func located(workspace Workspace) bool {
	repo, owner := filepath.Base(workspace.Clone), filepath.Base(filepath.Dir(workspace.Clone))
	stateDir := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(workspace.Clone))))
	repository, err := ghrepo.Parse("workspace repository", owner+"/"+repo)
	if err != nil {
		return false
	}
	want, err := Location(stateDir, repository, filepath.Base(workspace.Dir))
	return err == nil && want.Dir == workspace.Dir && want.Clone == workspace.Clone
}
