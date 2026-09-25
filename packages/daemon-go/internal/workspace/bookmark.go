package workspace

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var registeredWorkspace = regexp.MustCompile(`(?i)already (?:registered|exists)`)

// createWorkspace ports workspace.ts's createWorkspace. It resolves a bookmark before pruning or
// adding a workspace: a conflicted bookmark must not leave a registered working copy behind.
//
// One read of the issue's bookmark legion/<KEY>, its local row and origin's (issueBookmark),
// decides where the workspace starts:
//   - A conflicted local bookmark is refused by name, with its targets. That includes a local
//     deletion never pushed after origin's branch moved, which jj keeps as a conflict with a deleted
//     side and resolves to origin's new commit alone.
//   - A local bookmark on one commit is where the workspace starts.
//   - With no local bookmark, an untracked origin row is the issue's own branch, pushed from another
//     clone before this workspace existed: a repository's fixture, or the branch a tree pushed
//     before its volume was lost. A fresh clone tracks main alone, so such a branch is only a remote
//     row. It is tracked, which creates the local bookmark at its commit, and the workspace starts
//     there, so its commits are there and the issue's next push moves it.
//   - With no local bookmark, a tracked origin row is a local deletion (`jj bookmark delete` in the
//     shared clone) never pushed, where tracking changes nothing. It is refused by name, with the
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
	remote := workspace.Bookmark + "@origin"
	rows, err := issueBookmark(ctx, run, workspace)
	if err != nil {
		return err
	}
	var revision string
	switch local, origin := rows["local"], rows["origin"]; {
	case local.conflict:
		return fmt.Errorf("Bookmark %s is conflicted (adds %s; removes %s); workspace %s was not created. Resolve it with `jj bookmark set %s -r <commit> -R %s`",
			workspace.Bookmark, listed(local.added), listed(local.removed), workspace.Dir, workspace.Bookmark, cloneDir)
	case local.present:
		revision = local.added[0]
	case origin.conflict:
		return fmt.Errorf("Remote bookmark %s is conflicted (adds %s; removes %s); workspace %s was not created", remote, listed(origin.added), listed(origin.removed), workspace.Dir)
	case origin.present && origin.tracked:
		return fmt.Errorf("Bookmark %s was deleted in the shared clone %s and the deletion never pushed, while %s is tracked at %s; workspace %s was not created. "+
			"Restore it: `jj bookmark set %[1]s -r %[3]s -R %[2]s`. "+
			"Cancel the deletion, and the next provisioning adopts origin's branch: `jj bookmark forget %[1]s -R %[2]s`. "+
			"Start from main instead: delete the branch on GitHub (the pull request's Delete branch button, or `gh api -X DELETE repos/%[6]s/git/refs/heads/%[1]s`), and the next provisioning starts at main",
			workspace.Bookmark, cloneDir, remote, origin.added[0], workspace.Dir, workspace.Repo)
	case origin.present:
		if _, err := RunChecked(ctx, run, []string{"jj", "bookmark", "track", remote, "--ignore-working-copy", "-R", cloneDir}, nil, ""); err != nil {
			return err
		}
		revision = origin.added[0]
	}
	if err := os.MkdirAll(filepath.Dir(workspace.Dir), 0o700); err != nil {
		return fmt.Errorf("create workspace parent: %w", err)
	}
	prune := []string{"git", "--git-dir=" + filepath.Join(cloneDir, ".git"), "worktree", "prune"}
	_, _ = runCommand(ctx, run, prune, nil, "")

	add := []string{
		"jj", "workspace", "add", workspace.Dir, "--name", workspaceName, "--revision", cmp.Or(revision, "main"), "-R", cloneDir,
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
	if revision != "" {
		return nil
	}
	_, err = RunChecked(ctx, run, []string{"jj", "bookmark", "set", workspace.Bookmark, "-r", "@"}, nil, workspace.Dir)
	return err
}

// bookmarkRow is one row of `jj bookmark list --all-remotes`: whether the bookmark exists there,
// whether it is conflicted, whether a remote row is tracked, and the commits it adds and removes
// (one added commit, none removed, when it is not conflicted).
type bookmarkRow struct {
	present, conflict, tracked bool
	added, removed             []string
}

// bookmarkRowTemplate prints one line per row, its fields separated by "|": where it is (local,
// origin, git), then present, conflict and tracked as 1 or 0, then the added and the removed commit
// ids, comma-separated.
const bookmarkRowTemplate = `if(remote, remote, "local") ++ "|" ++ if(present, "1", "0") ++ "|" ++ if(conflict, "1", "0") ++ "|" ++ if(tracked, "1", "0") ++ "|" ++ added_targets.map(|c| c.commit_id()).join(",") ++ "|" ++ removed_targets.map(|c| c.commit_id()).join(",") ++ "\n"`

// issueBookmark reads the issue's bookmark in the shared clone, its local row and every remote's, in
// one `jj bookmark list`, keyed by where each row is. A bookmark that exists nowhere lists nothing.
func issueBookmark(ctx context.Context, run Runner, workspace Workspace) (map[string]bookmarkRow, error) {
	list := []string{"jj", "bookmark", "list", "--all-remotes", "exact:" + workspace.Bookmark, "-T", bookmarkRowTemplate, "--ignore-working-copy", "-R", workspace.Clone}
	listed, err := runCommand(ctx, run, list, nil, "")
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", strings.Join(list, " "), err)
	}
	if listed.ExitCode != 0 {
		return nil, fmt.Errorf("Bookmark %s could not be resolved; workspace %s was not created: %w", workspace.Bookmark, workspace.Dir, commandFailure(list, listed))
	}
	rows := map[string]bookmarkRow{}
	for _, line := range nonEmptyLines(listed.Stdout) {
		fields := strings.Split(line, "|")
		if len(fields) != 6 {
			return nil, fmt.Errorf("Bookmark %s's row %q is not the shape %s prints; workspace %s was not created", workspace.Bookmark, line, strings.Join(list, " "), workspace.Dir)
		}
		rows[fields[0]] = bookmarkRow{
			present: fields[1] == "1", conflict: fields[2] == "1", tracked: fields[3] == "1",
			added: commitList(fields[4]), removed: commitList(fields[5]),
		}
	}
	return rows, nil
}

func commitList(field string) []string {
	if field == "" {
		return nil
	}
	return strings.Split(field, ",")
}

// listed is a row's commits for a refusal: comma-separated, or "nothing".
func listed(commits []string) string {
	if len(commits) == 0 {
		return "nothing"
	}
	return strings.Join(commits, ", ")
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
