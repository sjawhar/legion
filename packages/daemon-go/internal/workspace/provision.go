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
	owner, repo, err := splitRepository(request.Repo)
	if err != nil {
		return Workspace{}, err
	}
	if request.StateDir == "" {
		return Workspace{}, fmt.Errorf("workspace state directory is required")
	}
	if request.Issue == "" || request.Issue != filepath.Base(request.Issue) {
		return Workspace{}, fmt.Errorf("workspace issue must be a single path component")
	}
	if request.CredentialHelper == "" {
		return Workspace{}, fmt.Errorf("workspace credential helper is required")
	}

	cloneDir := filepath.Join(request.StateDir, "repos", "github.com", owner, repo)
	workspace := Workspace{
		Dir:      filepath.Join(request.StateDir, "workspaces", owner, repo, strings.ToLower(request.Issue)),
		Bookmark: "legion/" + request.Issue,
	}
	exists, err := pathExists(workspace.Dir)
	if err != nil {
		return Workspace{}, err
	}
	if exists {
		if _, err := runChecked(ctx, run, []string{"jj", "workspace", "update-stale"}, nil, workspace.Dir); err != nil {
			return Workspace{}, err
		}
	}

	credential, err := newProvisioningCredential(request.StateDir, request.Token)
	if err != nil {
		return Workspace{}, err
	}
	defer func() {
		_ = credential.remove()
	}()
	if err := ensureRepoClone(ctx, run, cloneDir, owner, repo, credential.env); err != nil {
		return Workspace{}, err
	}
	if err := ensureFetchConfiguration(ctx, run, cloneDir, credential.env); err != nil {
		return Workspace{}, err
	}
	if err := credential.remove(); err != nil {
		return Workspace{}, err
	}
	credential.dir = ""

	if !exists {
		if err := createWorkspace(ctx, run, cloneDir, workspace); err != nil {
			return Workspace{}, err
		}
	}
	if err := configureRepositoryCredential(ctx, run, cloneDir, request.CredentialHelper); err != nil {
		return Workspace{}, err
	}
	if err := removeRepositoryIdentity(ctx, run, cloneDir); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

func splitRepository(repository string) (owner, repo string, err error) {
	parts := strings.Split(repository, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] != filepath.Base(parts[0]) || parts[1] != filepath.Base(parts[1]) {
		return "", "", fmt.Errorf("workspace repository must be owner/repository, got %q", repository)
	}
	return parts[0], parts[1], nil
}
