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
func createWorkspace(ctx context.Context, run Runner, cloneDir string, workspace Workspace) error {
	workspaceName := filepath.Base(workspace.Dir)
	resolve := []string{
		"jj", "log", "-r", "bookmarks(exact:" + workspace.Bookmark + ")", "--no-graph", "-T", `commit_id ++ "\n"`,
		"--ignore-working-copy", "-R", cloneDir,
	}
	resolved, err := runCommand(ctx, run, resolve, nil, "")
	if err != nil {
		return fmt.Errorf("run %s: %w", strings.Join(resolve, " "), err)
	}
	if resolved.ExitCode != 0 {
		return fmt.Errorf("Bookmark %s could not be resolved; workspace %s was not created: %w", workspace.Bookmark, workspace.Dir, commandFailure(resolve, resolved))
	}
	commits := nonEmptyLines(resolved.Stdout)
	if len(commits) > 1 {
		return fmt.Errorf("Bookmark %s is conflicted (%s); workspace %s was not created", workspace.Bookmark, strings.Join(commits, ", "), workspace.Dir)
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
		if _, err := runChecked(ctx, run, []string{"jj", "workspace", "forget", workspaceName, "-R", cloneDir}, nil, ""); err != nil {
			return err
		}
		_, _ = runCommand(ctx, run, prune, nil, "")
		if _, err := runChecked(ctx, run, add, nil, ""); err != nil {
			return err
		}
	}
	if len(commits) == 1 {
		return nil
	}
	_, err = runChecked(ctx, run, []string{"jj", "bookmark", "set", workspace.Bookmark, "-r", "@"}, nil, workspace.Dir)
	return err
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
// registered-but-missing state that Provision repairs with forget, prune, and add.
func Remove(ctx context.Context, run Runner, workspace Workspace) error {
	cloneDir, workspaceName, err := cloneForWorkspace(workspace.Dir)
	if err != nil {
		return err
	}
	cloneExists, err := pathExists(filepath.Join(cloneDir, ".jj"))
	if err != nil {
		return err
	}
	registered := false
	var commits []string
	repoArgs := []string{"--ignore-working-copy", "-R", cloneDir}
	if cloneExists {
		listed, err := runChecked(ctx, run, []string{"jj", "workspace", "list", "-T", `name ++ "\n"`, "--ignore-working-copy", "-R", cloneDir}, nil, "")
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
		own, err := runChecked(ctx, run, []string{"jj", "log", "-r", ownCommitsRevset(workspaceName), "--no-graph", "-T", `commit_id ++ "\n"`, "--ignore-working-copy", "-R", cloneDir}, nil, "")
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
			if _, err := runChecked(ctx, run, append([]string{"jj", "abandon", "-r", strings.Join(commits, " | ")}, repoArgs...), nil, ""); err != nil {
				return err
			}
		}
		if _, err := runChecked(ctx, run, append([]string{"jj", "workspace", "forget", workspaceName}, repoArgs...), nil, ""); err != nil {
			return err
		}
	}
	if cloneExists {
		if _, err := runChecked(ctx, run, []string{"git", "--git-dir=" + filepath.Join(cloneDir, ".git"), "worktree", "prune"}, nil, ""); err != nil {
			return err
		}
	}
	return nil
}

func cloneForWorkspace(directory string) (cloneDir, workspaceName string, err error) {
	workspaceName = filepath.Base(directory)
	repo := filepath.Base(filepath.Dir(directory))
	owner := filepath.Base(filepath.Dir(filepath.Dir(directory)))
	workspaces := filepath.Dir(filepath.Dir(filepath.Dir(directory)))
	if filepath.Base(workspaces) != "workspaces" || workspaceName == "." || repo == "." || owner == "." {
		return "", "", fmt.Errorf("workspace directory %q does not have the expected state/workspaces/<owner>/<repo>/<issue> shape", directory)
	}
	stateDir := filepath.Dir(workspaces)
	return filepath.Join(stateDir, "repos", "github.com", owner, repo), workspaceName, nil
}
