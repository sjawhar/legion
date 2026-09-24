package workspace

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// Provision ports packages/workspace/src/workspace.ts:402-515. It updates an existing working
// copy, obtains the shared clone through a temporary sibling, protects unreachable worker commits
// before every fetch, resolves a bookmark before adding, and leaves pane credentials on the clone.
// The clone and the fetch reach the repository through request.Feed with no credential — a pod's
// second init container, which the provisioning Secret is not mounted in — or, with no feed, from
// GitHub with the one-shot credential.
func Provision(ctx context.Context, run Runner, request Request) (Workspace, error) {
	workspace, err := Location(request.StateDir, request.Repo, request.Issue)
	if err != nil {
		return Workspace{}, err
	}
	if request.CredentialHelper == "" {
		return Workspace{}, fmt.Errorf("workspace credential helper is required")
	}
	var feed string
	switch {
	case request.Feed != "" && (request.Token != "" || request.CredentialDir != ""):
		return Workspace{}, errors.New("workspace request names a feed and a provisioning token; provisioning from a feed holds no credential")
	case request.Feed != "":
		if feed, err = FeedRepository(request.Feed, request.Repo); err != nil {
			return Workspace{}, err
		}
	case request.CredentialDir == "":
		return Workspace{}, fmt.Errorf("workspace credential directory is required")
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

	var source remote
	if feed != "" {
		source = feedRemote(feed, request.Repo)
	} else if source, err = newProvisioningCredential(request.CredentialDir, request.Token); err != nil {
		return Workspace{}, err
	}
	defer func() {
		_ = source.remove()
	}()
	if err := ensureRepoClone(ctx, run, workspace.Clone, "https://github.com/"+request.Repo, source.env); err != nil {
		return Workspace{}, err
	}
	if err := ensureFetchConfiguration(ctx, run, workspace.Clone, source.env); err != nil {
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

// splitRepository is owner/repository's two names. A `.` or `..` segment is refused: joined under
// the state directory it names another directory than the repository's, and provisioning removes
// an incomplete clone at that path.
func splitRepository(repository string) (owner, repo string, err error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] != filepath.Base(parts[0]) || parts[1] != filepath.Base(parts[1]) {
		return "", "", fmt.Errorf("workspace repository must be owner/repository, got %q", repository)
	}
	for _, part := range parts {
		if part == "." || part == ".." {
			return "", "", fmt.Errorf("workspace repository %q has a %q segment; want owner/repository", repository, part)
		}
	}
	return parts[0], parts[1], nil
}

// Bookmark is the jj bookmark an issue's workspace is on: its branch.
func Bookmark(issue string) string { return "legion/" + issue }

// Location is the deterministic workspace location Provision creates for one issue, and the shared
// clone it is a jj workspace of. A `.` or `..` issue is refused: joined under the repository's
// workspaces directory it names that directory, or its owner's, and Remove deletes a workspace
// directory whole.
func Location(stateDir, repository, issue string) (Workspace, error) {
	owner, repo, err := splitRepository(repository)
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
