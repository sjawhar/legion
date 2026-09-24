package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Provision ports packages/workspace/src/workspace.ts:402-515. It updates an existing working
// copy, obtains the shared clone through a temporary sibling, protects unreachable worker commits
// before every fetch, resolves a bookmark before adding, and leaves pane credentials on the clone.
func Provision(ctx context.Context, run Runner, request Request) (Workspace, error) {
	workspace, err := Location(request.StateDir, request.Repo, request.Issue)
	if err != nil {
		return Workspace{}, err
	}
	if request.CredentialHelper == "" {
		return Workspace{}, fmt.Errorf("workspace credential helper is required")
	}
	if request.CredentialDir == "" {
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

	credential, err := newProvisioningCredential(request.CredentialDir, request.Token)
	if err != nil {
		return Workspace{}, err
	}
	defer func() {
		_ = credential.remove()
	}()
	if err := ensureRepoClone(ctx, run, workspace.Clone, "https://github.com/"+request.Repo, credential.env); err != nil {
		return Workspace{}, err
	}
	if err := ensureFetchConfiguration(ctx, run, workspace.Clone, credential.env); err != nil {
		return Workspace{}, err
	}
	if err := credential.remove(); err != nil {
		return Workspace{}, err
	}
	credential.dir = ""

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
// clone it is a jj workspace of.
func Location(stateDir, repository, issue string) (Workspace, error) {
	owner, repo, err := splitRepository(repository)
	if err != nil {
		return Workspace{}, err
	}
	if stateDir == "" {
		return Workspace{}, fmt.Errorf("workspace state directory is required")
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
