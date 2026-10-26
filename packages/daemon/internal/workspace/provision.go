package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// Provision ports provisionIssueWorkspace (packages/workspace/src/workspace.ts). It updates an
// existing working copy, obtains the shared clone through a temporary sibling, protects unreachable
// worker commits before every fetch, resolves a bookmark before adding, and leaves pane credentials
// on the clone.
// The clone and the fetch reach the repository through request.Source: a pod's feed with no
// credential, or GitHub with the one-shot credential. An issue with no branch starts at
// request.DefaultBranch, which the request must name. Every command on the shared clone runs under
// request.StoreLock, the `jj workspace add` that registers the issue's workspace with no files
// among them. Only the checkout (`jj sparse reset`), which writes the issue's own working copy and
// nothing of the clone's, runs with the lock let go, holding one of request.CheckoutSlot's places
// instead.
func Provision(ctx context.Context, run Runner, request Request) (Workspace, error) {
	workspace, err := Location(request.StateDir, request.Repo, request.Issue)
	if err != nil {
		return Workspace{}, err
	}
	if _, err := guardWorkspace(workspace.Dir, workspace.Clone, false); err != nil {
		return Workspace{}, err
	}
	if request.CredentialHelper == "" {
		return Workspace{}, fmt.Errorf("workspace credential helper is required")
	}
	if request.Log == nil {
		return Workspace{}, errors.New("workspace request names no log")
	}
	if request.DefaultBranch == "" {
		return Workspace{}, errors.New("workspace default branch is required")
	}
	workspace.DefaultBranch = request.DefaultBranch
	if request.Source == nil {
		return Workspace{}, errors.New("workspace request names no way to the repository: FromFeed or FromGitHub")
	}
	if err := request.Source.check(request.Repo); err != nil {
		return Workspace{}, err
	}

	store := &storeLock{locker: request.StoreLock}
	store.lock()
	defer store.unlock()
	exists, err := pathExists(workspace.Dir)
	if err != nil {
		return Workspace{}, err
	}
	if exists {
		if _, err := RunCheckedIn(ctx, run, workspace, []string{"jj", "workspace", "update-stale"}); err != nil {
			return Workspace{}, err
		}
	}
	source, err := request.Source.open(request.Repo, workspace.Bookmark, request.DefaultBranch)
	if err != nil {
		return Workspace{}, err
	}
	defer func() {
		_ = source.remove()
	}()
	if err := ensureRepoClone(ctx, run, WallClock{}, workspace.Clone, GitHubURL(request.Repo), request.DefaultBranch, source); err != nil {
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
	checkout := !exists
	if exists {
		// A workspace whose checkout never finished — registered with no files, then the checkout
		// failed or the daemon stopped — is checked out now, or its pane would start in an empty
		// working copy.
		whole, err := CheckedOut(ctx, run, workspace)
		if err != nil {
			return Workspace{}, err
		}
		checkout = !whole
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
	// to git; without this, jj's default auto-track snapshots it into the workspace's own change
	// (confirmed empirically: `jj status` under an empty HOME showed `.codegraph/.gitignore`
	// newly added after a bare `codegraph init`). `.git/info/exclude` is local to this shared
	// clone, read by every workspace of it (git worktrees share one `info/exclude` through their
	// common git directory) and by jj (confirmed the same way: with the line added first, the
	// same `codegraph init` left `jj status` clean), so this needs no global git configuration
	// anywhere a workspace of this clone is used.
	if err := excludeCodegraphDirectory(workspace.Clone); err != nil {
		return Workspace{}, err
	}
	// The checkout writes the issue's working copy, every file of the tree, into the workspace the
	// registration made with no files: the one step whose time follows the repository's size, and
	// it writes nothing of the shared clone's, so it runs with the store lock let go, beside other
	// issues' checkouts, holding one of the places the daemon bounds checkouts by.
	store.unlock()
	if checkout {
		release, err := request.checkoutPlace(ctx)
		if err != nil {
			return Workspace{}, fmt.Errorf("check out workspace %s: wait for a checkout's place: %w", workspace.Dir, err)
		}
		_, err = RunCheckedIn(ctx, run, workspace, []string{"jj", "sparse", "reset"})
		release()
		if err != nil {
			return Workspace{}, fmt.Errorf("check out workspace %s: %w", workspace.Dir, err)
		}
	}
	return workspace, nil
}

// CheckedOut is whether workspace's checkout finished: its sparse patterns are the whole tree
// (`.`). Provision registers a workspace with no files (`jj workspace add --sparse-patterns
// empty`) and checks it out after (`jj sparse reset`), so a workspace whose checkout failed or was
// cut short — a jj error, a full disk, a daemon stopped mid-checkout — holds no pattern and no
// file until a provision checks it out. The read never snapshots the working copy
// (--ignore-working-copy): a pane may be writing it, and a snapshot would run the working-copy
// filters, fsmonitor and signing programs a tree agent can configure. A directory with no `.jj` is
// refused, as every jj command in a workspace is (guardWorkspace).
func CheckedOut(ctx context.Context, run Runner, workspace Workspace) (bool, error) {
	listed, err := RunCheckedIn(ctx, run, workspace, []string{"jj", "sparse", "list", "--ignore-working-copy", "--color=never"})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(listed.Stdout) == ".", nil
}

// excludeCodegraphDirectory appends ".codegraph/" to cloneDir's ".git/info/exclude" unless a
// line already matches it exactly, so repeated provisioning of the same clone writes it once.
func excludeCodegraphDirectory(cloneDir string) error {
	excludePath := filepath.Join(cloneDir, ".git", "info", "exclude")
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
	content := string(existing)
	if len(content) > 0 && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += ".codegraph/\n"
	if err := os.WriteFile(excludePath, []byte(content), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", excludePath, err)
	}
	return nil
}

// Bookmark is the jj bookmark an issue's workspace is on: its branch (claim.IssueBranch).
func Bookmark(issue string) string { return claim.IssueBranch(issue) }

// layoutDepth is how many directories below the state directory a shared clone and a workspace
// each sit (Location): repos/<host>/<owner>/<name>, and workspaces/<owner>/<name>/<issue>.
const layoutDepth = 4

// CloneRoot is the directory under stateDir that holds every repository's shared clone, each at
// <owner>/<name> below it (Location).
func CloneRoot(stateDir string) string {
	return filepath.Join(stateDir, "repos", "github.com")
}

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
		Clone:    filepath.Join(CloneRoot(stateDir), repository.Owner(), repository.Name()),
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
