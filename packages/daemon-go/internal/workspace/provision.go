package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
)

// Provision ports packages/workspace/src/workspace.ts:402-515. It updates an existing working
// copy, obtains the shared clone through a temporary sibling, protects unreachable worker commits
// before every fetch, resolves a bookmark before adding, and leaves pane credentials on the clone.
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
	if err := ensureRepoClone(ctx, run, workspace.Clone, "https://github.com/"+request.Repo, source.env); err != nil {
		return Workspace{}, err
	}
	if err := ensureFetchConfiguration(ctx, run, workspace.Clone, source); err != nil {
		return Workspace{}, err
	}
	if err := source.remove(); err != nil {
		return Workspace{}, err
	}

	if !exists {
		if err := createWorkspace(ctx, run, workspace); err != nil {
			return Workspace{}, err
		}
	}
	if err := configureRepositoryCredential(ctx, run, workspace.Clone, request.CredentialHelper); err != nil {
		return Workspace{}, err
	}
	if err := removeRepositoryIdentity(ctx, run, workspace.Clone); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

// Bookmark is the jj bookmark an issue's workspace is on: its branch.
func Bookmark(issue string) string { return "legion/" + issue }

// Location is the deterministic workspace location Provision creates for one issue, and the shared
// clone it is a jj workspace of. A `.` or `..` issue is refused: joined under the repository's
// workspaces directory it names that directory, or its owner's, and Remove deletes a workspace
// directory whole.
func Location(stateDir, repository, issue string) (Workspace, error) {
	owner, repo, err := ghrepo.Split("workspace repository", repository)
	if err != nil {
		return Workspace{}, err
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
		Dir:      filepath.Join(stateDir, "workspaces", owner, repo, strings.ToLower(issue)),
		Bookmark: Bookmark(issue),
		Clone:    filepath.Join(stateDir, "repos", "github.com", owner, repo),
		Repo:     owner + "/" + repo,
	}, nil
}

// located is whether workspace's directory and clone are the pair Location derives for the issue
// its directory names: <state>/workspaces/<owner>/<repo>/<issue> beside
// <state>/repos/github.com/<owner>/<repo>.
func located(workspace Workspace) bool {
	repo, owner := filepath.Base(workspace.Clone), filepath.Base(filepath.Dir(workspace.Clone))
	stateDir := filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(workspace.Clone))))
	want, err := Location(stateDir, owner+"/"+repo, filepath.Base(workspace.Dir))
	return err == nil && want.Dir == workspace.Dir && want.Clone == workspace.Clone
}
