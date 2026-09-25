package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var registeredWorkspace = regexp.MustCompile(`(?i)already (?:registered|exists)`)

// createWorkspace ports workspace.ts:198-296. It resolves a bookmark before pruning or adding a
// workspace: a conflicted bookmark must not leave a registered working copy behind.
//
// The workspace starts at the issue's bookmark legion/<KEY> when it resolves to one commit. When no
// local bookmark exists, origin's row decides:
//   - An untracked row is the issue's own branch, pushed from another clone before this workspace
//     existed: a repository's fixture, or the branch a tree pushed before its volume was lost. A
//     fresh clone tracks main alone, so such a branch is only a remote row. It is tracked, which
//     creates the local bookmark at its commit, and the workspace starts there, so its commits are
//     there and the issue's next push moves it.
//   - A tracked row with no local bookmark is a local deletion (`jj bookmark delete` in the shared
//     clone) never pushed, where tracking changes nothing. It is refused by name, with the
//     operator's three ways out: restore the bookmark (`jj bookmark set`); cancel the deletion
//     (`jj bookmark forget`, which leaves the row untracked, so the next provisioning adopts
//     origin's branch); or start from main instead, by deleting the branch on GitHub (the pull
//     request's Delete branch button, or `gh api -X DELETE`), after which the next provisioning's
//     fetch drops the row. A push of the deletion from the shared clone is not offered: the clone
//     authenticates only through `legion credential`, which needs a tree's grant that no operator
//     shell holds, and `jj git push --deleted` would push every pending deletion in the shared
//     clone, other issues' branches with it.
//   - No row at all is a brand-new issue, or a merged branch GitHub deleted. The workspace starts
//     at main, with the bookmark created on it.
func createWorkspace(ctx context.Context, run Runner, workspace Workspace) error {
	cloneDir := workspace.Clone
	workspaceName := filepath.Base(workspace.Dir)
	commits, err := bookmarkCommits(ctx, run, cloneDir, "bookmarks(exact:"+workspace.Bookmark+")", "Bookmark "+workspace.Bookmark, workspace.Dir)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		remote := workspace.Bookmark + "@origin"
		pattern := `exact:"` + workspace.Bookmark + `", exact:"origin"`
		tracked, err := bookmarkCommits(ctx, run, cloneDir, "tracked_remote_bookmarks("+pattern+")", "Remote bookmark "+remote, workspace.Dir)
		if err != nil {
			return err
		}
		if len(tracked) == 1 {
			return fmt.Errorf("Bookmark %s was deleted in the shared clone %s and the deletion never pushed, while %s is tracked at %s; workspace %s was not created. "+
				"Restore it: `jj bookmark set %[1]s -r %[3]s -R %[2]s`. "+
				"Cancel the deletion, and the next provisioning adopts origin's branch: `jj bookmark forget %[1]s -R %[2]s`. "+
				"Start from main instead: delete the branch on GitHub (the pull request's Delete branch button, or `gh api -X DELETE repos/%[6]s/git/refs/heads/%[1]s`), and the next provisioning starts at main",
				workspace.Bookmark, cloneDir, remote, tracked[0], workspace.Dir, workspace.Repo)
		}
		untracked, err := bookmarkCommits(ctx, run, cloneDir, "untracked_remote_bookmarks("+pattern+")", "Remote bookmark "+remote, workspace.Dir)
		if err != nil {
			return err
		}
		if len(untracked) == 1 {
			if _, err := RunChecked(ctx, run, []string{"jj", "bookmark", "track", remote, "-R", cloneDir}, nil, ""); err != nil {
				return err
			}
			commits = untracked
		}
	}
	if err := os.MkdirAll(filepath.Dir(workspace.Dir), 0o700); err != nil {
		return fmt.Errorf("create workspace parent: %w", err)
	}
	prune := []string{"git", "--git-dir=" + filepath.Join(cloneDir, ".git"), "worktree", "prune"}
	_, _ = runCommand(ctx, run, prune, nil, "")

	revision := "main"
	if len(commits) == 1 {
		revision = commits[0]
	}
	add := []string{
		"jj", "workspace", "add", workspace.Dir, "--name", workspaceName, "--revision", revision, "-R", cloneDir,
	}
	result, err := runCommand(ctx, run, add, nil, "")
	if err != nil {
		return fmt.Errorf("run %s: %w", strings.Join(add, " "), err)
	}
	forgotten := result.ExitCode != 0
	if forgotten {
		if !registeredWorkspace.MatchString(result.Stderr) {
			return commandFailure(add, result)
		}
		if _, err := RunChecked(ctx, run, []string{"jj", "workspace", "forget", workspaceName, "-R", cloneDir}, nil, ""); err != nil {
			return err
		}
		_, _ = runCommand(ctx, run, prune, nil, "")
		if _, err := RunChecked(ctx, run, add, nil, ""); err != nil {
			return err
		}
	}
	if len(commits) == 1 {
		return nil
	}
	_, err = RunChecked(ctx, run, []string{"jj", "bookmark", "set", workspace.Bookmark, "-r", "@"}, nil, workspace.Dir)
	return err
}

// bookmarkCommits lists the commits revset names in the shared clone, one per line: none when the
// bookmark does not exist, one normally, and more when it is conflicted, which is refused naming
// what (the bookmark) before anything is pruned, added, or registered.
func bookmarkCommits(ctx context.Context, run Runner, cloneDir, revset, what, workspaceDir string) ([]string, error) {
	resolve := []string{"jj", "log", "-r", revset, "--no-graph", "-T", `commit_id ++ "\n"`, "--ignore-working-copy", "-R", cloneDir}
	resolved, err := runCommand(ctx, run, resolve, nil, "")
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", strings.Join(resolve, " "), err)
	}
	if resolved.ExitCode != 0 {
		return nil, fmt.Errorf("%s could not be resolved; workspace %s was not created: %w", what, workspaceDir, commandFailure(resolve, resolved))
	}
	commits := nonEmptyLines(resolved.Stdout)
	if len(commits) > 1 {
		return nil, fmt.Errorf("%s is conflicted (%s); workspace %s was not created", what, strings.Join(commits, ", "), workspaceDir)
	}
	return commits, nil
}

func nonEmptyLines(value string) []string {
	var lines []string
	for _, line := range strings.Split(value, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	return lines
}

// AdoptWorkingCopyCommand ports workspace.ts:365-380. Tmux and a pod shim must use the same
// revset so only an undescribed working copy changes author.
func AdoptWorkingCopyCommand(dir string) []string {
	return []string{"jj", "metaedit", "--update-author", "-r", `@ & description(exact:"")`, "-R", dir}
}

// ownCommitsRevset ports workspace.ts:533-541: only commits unique to a closing workspace are
// abandoned; all commits reached by another working copy, a bookmark, a remote, or a tag survive.
func ownCommitsRevset(workspaceName string) string {
	return "::" + workspaceName + "@ ~ ::(working_copies() ~ " + workspaceName + "@) ~ ::(bookmarks() | remote_bookmarks() | tags())"
}

// Remove ports workspace.ts:544-618. The workspace directory goes first so a crash leaves the
// registered-but-missing state that Provision repairs with forget, prune, and add. workspace is
// Location's, which names the clone; any other is refused before anything is removed.
func Remove(ctx context.Context, run Runner, workspace Workspace) error {
	if !located(workspace) {
		return fmt.Errorf("workspace to remove (%#v) is not a workspace Location names", workspace)
	}
	cloneDir, workspaceName := workspace.Clone, filepath.Base(workspace.Dir)
	cloneExists, err := pathExists(filepath.Join(cloneDir, ".jj"))
	if err != nil {
		return err
	}
	registered := false
	var commits []string
	repoArgs := []string{"--ignore-working-copy", "-R", cloneDir}
	if cloneExists {
		listed, err := RunChecked(ctx, run, []string{"jj", "workspace", "list", "-T", `name ++ "\n"`, "--ignore-working-copy", "-R", cloneDir}, nil, "")
		if err != nil {
			return err
		}
		for _, name := range nonEmptyLines(listed.Stdout) {
			if name == workspaceName {
				registered = true
				break
			}
		}
	}
	if registered {
		own, err := RunChecked(ctx, run, []string{"jj", "log", "-r", ownCommitsRevset(workspaceName), "--no-graph", "-T", `commit_id ++ "\n"`, "--ignore-working-copy", "-R", cloneDir}, nil, "")
		if err != nil {
			return err
		}
		commits = nonEmptyLines(own.Stdout)
	}
	directoryExists, err := pathExists(workspace.Dir)
	if err != nil {
		return err
	}
	if !registered && !directoryExists {
		return nil
	}
	if err := os.RemoveAll(workspace.Dir); err != nil {
		return fmt.Errorf("remove workspace directory: %w", err)
	}
	if registered {
		if len(commits) > 0 {
			if _, err := RunChecked(ctx, run, append([]string{"jj", "abandon", "-r", strings.Join(commits, " | ")}, repoArgs...), nil, ""); err != nil {
				return err
			}
		}
		if _, err := RunChecked(ctx, run, append([]string{"jj", "workspace", "forget", workspaceName}, repoArgs...), nil, ""); err != nil {
			return err
		}
	}
	if cloneExists {
		if _, err := RunChecked(ctx, run, []string{"git", "--git-dir=" + filepath.Join(cloneDir, ".git"), "worktree", "prune"}, nil, ""); err != nil {
			return err
		}
	}
	return nil
}
