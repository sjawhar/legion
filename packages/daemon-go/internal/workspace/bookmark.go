package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var (
	registeredWorkspace = regexp.MustCompile(`(?i)already (?:registered|exists)`)
	commitID            = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// createWorkspace ports workspace.ts's createWorkspace. It resolves a bookmark before pruning or
// adding a workspace: a conflicted bookmark must not leave a registered working copy behind.
//
// One read of the issue's bookmark legion/<KEY>, its local row and origin's (readBookmark),
// decides where the workspace starts:
//   - A local bookmark conflicted with a deletion whose origin row is tracked with no commit (GitHub
//     deleted the branch after the bookmark moved on without a push) is set aside when the move
//     added at least one commit beyond what it removed and none of them is described
//     (undescribedMove): jj pushes no undescribed commit, so the move was never meant to reach
//     GitHub, and this is the unmoved merged bookmark in intent. Once main resolves, the local
//     bookmark is deleted, the commits' ids are logged oldest first (they stay visible, the clone
//     never abandoning unreachable commits), and the workspace starts at main. A move backwards,
//     which added nothing, keeps the conflict's refusal.
//   - A conflicted local bookmark is refused by name, with its sides. That includes a conflict with
//     a deleted side, which `bookmarks(exact:)` resolves to the other side alone: a local deletion
//     never pushed and then origin's branch moving, or a local move never pushed and then the
//     branch deleted on GitHub. The refusal names two ways out: keep an added commit
//     (`jj bookmark set`, which jj refuses for the removed side or main), or start from main with
//     `jj bookmark delete`, after deleting the branch on GitHub when origin has it: the GitHub
//     deletion alone leaves the local side in conflict with a deletion when both sides moved.
//   - A local bookmark on one commit is where the workspace starts, whether or not origin has the
//     branch yet.
//   - With no local bookmark, a conflicted origin row is refused: only concurrent fetches leave
//     one, and the next provisioning's fetch sets the row to origin's branch as it is then.
//   - With no local bookmark, an untracked origin row is the issue's own branch, pushed from another
//     clone before this workspace existed: a repository's fixture, or the branch a tree pushed
//     before its volume was lost. A fresh clone tracks main alone, so such a branch is only a remote
//     row. It is tracked, which creates the local bookmark at its commit, and the workspace starts
//     there, so its commits are there and the issue's next push moves it. The bookmark is read
//     again after the track: a fetch between the read and the track leaves it on a newer commit,
//     where a workspace at the listed one could never move it, so that provisioning is refused and
//     the next one starts at the bookmark.
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
//     at main, resolved to one commit first (mainCommit), with the bookmark created on it.
func createWorkspace(ctx context.Context, run Runner, workspace Workspace, log func(string)) error {
	cloneDir := workspace.Clone
	workspaceName := filepath.Base(workspace.Dir)
	remote := workspace.Bookmark + "@origin"
	rows, err := readBookmark(ctx, run, workspace, workspace.Bookmark)
	if err != nil {
		return err
	}
	local, origin := rows.local, rows.origin
	var setAside []string
	undescribed := false
	if local.conflict && local.hasDeletedSide() && origin.tracked && !origin.present {
		if setAside, undescribed, err = undescribedMove(ctx, run, workspace, local); err != nil {
			return err
		}
	}
	var revision string
	switch {
	case undescribed:
		// Set aside once main resolves, below.
	case local.conflict:
		keep, commit := "its added commit", local.added[0]
		if len(local.added) > 1 {
			keep, commit = "one of its added commits", "<commit>"
		}
		startAtMain := fmt.Sprintf("`jj bookmark delete %s --ignore-working-copy -R %s`", workspace.Bookmark, cloneDir)
		if origin.present {
			startAtMain = fmt.Sprintf("delete the branch on GitHub (the pull request's Delete branch button, or `gh api -X DELETE repos/%s/git/refs/heads/%s`) and run %s", workspace.Repo, workspace.Bookmark, startAtMain)
		}
		return fmt.Errorf("Bookmark %s is conflicted %s; workspace %s was not created. Keep %s: `jj bookmark set %s -r %s --ignore-working-copy -R %s`. Start from main instead: %s, and the next provisioning starts at main",
			workspace.Bookmark, local.sides(), workspace.Dir, keep, workspace.Bookmark, commit, cloneDir, startAtMain)
	case local.present:
		revision = local.added[0]
	case origin.conflict:
		return fmt.Errorf("Remote bookmark %s is conflicted %s, which concurrent fetches leave; workspace %s was not created. Provision again: the next provisioning's fetch sets the row to origin's branch as it is then",
			remote, origin.sides(), workspace.Dir)
	case origin.present && origin.tracked:
		return fmt.Errorf("Bookmark %s was deleted in the shared clone %s and the deletion never pushed, while %s is tracked at %s; workspace %s was not created. "+
			"Restore it: `jj bookmark set %[1]s -r %[3]s --ignore-working-copy -R %[2]s`. "+
			"Cancel the deletion, and the next provisioning adopts origin's branch: `jj bookmark forget %[1]s --ignore-working-copy -R %[2]s`. "+
			"Start from main instead: delete the branch on GitHub (the pull request's Delete branch button, or `gh api -X DELETE repos/%[6]s/git/refs/heads/%[1]s`), and the next provisioning starts at main",
			workspace.Bookmark, cloneDir, remote, origin.added[0], workspace.Dir, workspace.Repo)
	case origin.present:
		if _, err := RunChecked(ctx, run, onClone(cloneDir, "bookmark", "track", remote), nil, ""); err != nil {
			return err
		}
		revision = origin.added[0]
		tracked, err := readBookmark(ctx, run, workspace, workspace.Bookmark)
		if err != nil {
			return err
		}
		if now := tracked.local; !now.present || now.conflict || now.added[0] != revision {
			return fmt.Errorf("Bookmark %s moved from %s to %s while it was being tracked; workspace %s was not created. Provision again: the next provisioning starts at origin's branch as it is then",
				remote, revision, listed(now.added), workspace.Dir)
		}
	}
	fromMain := revision == ""
	if fromMain {
		if revision, err = mainCommit(ctx, run, workspace); err != nil {
			return err
		}
		// After main resolves, so the one line naming the set-aside commits is never a start that
		// did not happen.
		if undescribed {
			if _, err := RunChecked(ctx, run, onClone(cloneDir, "bookmark", "delete", workspace.Bookmark), nil, ""); err != nil {
				return err
			}
			log(fmt.Sprintf("Bookmark %s was moved after its last push onto commits nobody described (%s), and GitHub deleted its branch: workspace %s starts at main. Those commits stay visible in the shared clone %s (git.abandon-unreachable-commits is false), recoverable by id",
				workspace.Bookmark, listed(setAside), workspace.Dir, cloneDir))
		}
	}
	if err := os.MkdirAll(filepath.Dir(workspace.Dir), 0o700); err != nil {
		return fmt.Errorf("create workspace parent: %w", err)
	}
	prune := []string{"git", "--git-dir=" + filepath.Join(cloneDir, ".git"), "worktree", "prune"}
	_, _ = runCommand(ctx, run, prune, nil, "")

	// The one jj command on the shared clone not built by onClone: `jj workspace add` refuses
	// --ignore-working-copy (on 0.44 and 0.45, after registering the workspace and creating its
	// directory), so the add snapshots the clone's own working copy. Every other command
	// provisioning and removal run on the clone is onClone's, and every command a refusal prints
	// carries --ignore-working-copy.
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
		if _, err := RunChecked(ctx, run, onClone(cloneDir, "workspace", "forget", workspaceName), nil, ""); err != nil {
			return err
		}
		_, _ = runCommand(ctx, run, prune, nil, "")
		if _, err := RunChecked(ctx, run, add, nil, ""); err != nil {
			return err
		}
	}
	if !fromMain {
		return nil
	}
	_, err = RunChecked(ctx, run, []string{"jj", "bookmark", "set", workspace.Bookmark, "-r", "@"}, nil, workspace.Dir)
	return err
}

// undescribedMove is what the local bookmark's move added beyond what it removed
// (`<removed>..<added>`), oldest first, and whether that is at least one commit and none of them
// described. jj pushes no undescribed commit, so such a move was never meant to reach GitHub; a
// move that added nothing (a bookmark moved backwards off its last push) is not one, and keeps the
// conflict's refusal.
func undescribedMove(ctx context.Context, run Runner, workspace Workspace, local bookmarkRow) ([]string, bool, error) {
	revset := "(" + strings.Join(local.removed, " | ") + ")..(" + strings.Join(local.added, " | ") + ")"
	read := onClone(workspace.Clone, "log", "-r", revset, "--reversed", "--no-graph", "-T", `commit_id ++ "|" ++ if(description, "1", "0") ++ "\n"`)
	result, err := RunChecked(ctx, run, read, nil, "")
	if err != nil {
		return nil, false, err
	}
	var commits []string
	for _, line := range nonEmptyLines(result.Stdout) {
		id, described, ok := strings.Cut(line, "|")
		if !ok || !commitID.MatchString(id) || described != "0" && described != "1" {
			return nil, false, fmt.Errorf("Bookmark %s's move %s printed %q, not a commit id and 0 or 1; workspace %s was not created", workspace.Bookmark, revset, line, workspace.Dir)
		}
		if described == "1" {
			return nil, false, nil
		}
		commits = append(commits, id)
	}
	return commits, len(commits) > 0, nil
}

// mainCommit is the commit a workspace with no issue branch starts at: main's, resolved to one
// commit id as the issue bookmark is, since a `jj workspace add --revision main` jj cannot resolve
// still registers the workspace, parented on the root commit, before it reports the error. Each
// main that does not resolve is refused by name before anything is added: a conflicted one, with
// origin's to keep (`--allow-backwards`, since jj refuses to move a bookmark sideways off two
// local moves); one deleted in the shared clone while origin's is tracked, with origin's to
// restore; and an absent one, a repository whose default branch is another.
func mainCommit(ctx context.Context, run Runner, workspace Workspace) (string, error) {
	rows, err := readBookmark(ctx, run, workspace, "main")
	if err != nil {
		return "", err
	}
	switch main, origin := rows.local, rows.origin; {
	case main.conflict:
		return "", fmt.Errorf("Bookmark main is conflicted %s; workspace %s was not created. Keep origin's: `jj bookmark set main -r main@origin --allow-backwards --ignore-working-copy -R %s`, and the next provisioning starts there",
			main.sides(), workspace.Dir, workspace.Clone)
	case !main.present && origin.present && origin.tracked && !origin.conflict:
		return "", fmt.Errorf("Bookmark main was deleted in the shared clone %s while main@origin is tracked at %s; workspace %s was not created. Restore it: `jj bookmark set main -r main@origin --ignore-working-copy -R %[1]s`, and the next provisioning starts there",
			workspace.Clone, origin.added[0], workspace.Dir)
	case !main.present:
		return "", fmt.Errorf("Bookmark main is not in the shared clone %s; workspace %s was not created. An issue with no branch starts at main, so the repository's default branch must be main",
			workspace.Clone, workspace.Dir)
	default:
		return main.added[0], nil
	}
}

// bookmarkRow is one row of `jj bookmark list --all-remotes`: whether the bookmark exists there,
// whether it is conflicted, whether a remote row is tracked, and the commits it adds and removes. A
// present row that is not conflicted adds one commit and removes none; an absent row adds none.
type bookmarkRow struct {
	present, conflict, tracked bool
	added, removed             []string
}

// sides is a conflicted row's sides for a refusal: its added and removed commits, and whether one
// side is a deletion, which jj leaves out of the added commits.
func (r bookmarkRow) sides() string {
	sides := fmt.Sprintf("(adds %s; removes %s)", listed(r.added), listed(r.removed))
	if r.hasDeletedSide() {
		sides += ", one side a deletion"
	}
	return sides
}

// hasDeletedSide is whether a conflicted row's sides include a deletion, which jj leaves out of
// its added commits.
func (r bookmarkRow) hasDeletedSide() bool {
	return len(r.added) <= len(r.removed)
}

// bookmarkRows is a bookmark's local row and origin's. A row jj does not list is the zero row:
// absent.
type bookmarkRows struct {
	local, origin bookmarkRow
}

// bookmarkRowTemplate prints one line per row, its fields separated by "|": where it is (local,
// origin, git), then present, conflict and tracked as 1 or 0, then the added and the removed commit
// ids, comma-separated.
const bookmarkRowTemplate = `if(remote, remote, "local") ++ "|" ++ if(present, "1", "0") ++ "|" ++ if(conflict, "1", "0") ++ "|" ++ if(tracked, "1", "0") ++ "|" ++ added_targets.map(|c| c.commit_id()).join(",") ++ "|" ++ removed_targets.map(|c| c.commit_id()).join(",") ++ "\n"`

// readBookmark reads the bookmark name in the shared clone (the issue's, or main), its local row and
// origin's, in one `jj bookmark list`. A bookmark that exists nowhere lists nothing. Every row must have the
// template's shape, with 0/1 flags, full commit ids, one added commit on a present row that is not
// conflicted and at least one on a conflicted row, and one row per place: a commit jj cannot load
// prints an error value where its id goes, which a workspace add would take as a revision.
func readBookmark(ctx context.Context, run Runner, workspace Workspace, name string) (bookmarkRows, error) {
	list := onClone(workspace.Clone, "bookmark", "list", "--all-remotes", "exact:"+name, "-T", bookmarkRowTemplate)
	result, err := runCommand(ctx, run, list, nil, "")
	if err != nil {
		return bookmarkRows{}, fmt.Errorf("run %s: %w", strings.Join(list, " "), err)
	}
	if result.ExitCode != 0 {
		return bookmarkRows{}, fmt.Errorf("Bookmark %s could not be resolved; workspace %s was not created: %w", name, workspace.Dir, commandFailure(list, result))
	}
	var rows bookmarkRows
	seen := map[string]bool{}
	for _, line := range nonEmptyLines(result.Stdout) {
		where, row, ok := parseBookmarkRow(line)
		if !ok || seen[where] {
			return bookmarkRows{}, fmt.Errorf("Bookmark %s's row %q is not the shape %s prints; workspace %s was not created", name, line, strings.Join(list, " "), workspace.Dir)
		}
		seen[where] = true
		switch where {
		case "local":
			rows.local = row
		case "origin":
			rows.origin = row
		}
	}
	return rows, nil
}

// parseBookmarkRow reads one line bookmarkRowTemplate printed, reporting whether it has the shape.
func parseBookmarkRow(line string) (string, bookmarkRow, bool) {
	fields := strings.Split(line, "|")
	if len(fields) != 6 {
		return "", bookmarkRow{}, false
	}
	var flags [3]bool
	for i, field := range fields[1:4] {
		switch field {
		case "1":
			flags[i] = true
		case "0":
		default:
			return "", bookmarkRow{}, false
		}
	}
	row := bookmarkRow{present: flags[0], conflict: flags[1], tracked: flags[2], added: commitList(fields[4]), removed: commitList(fields[5])}
	for _, id := range slices.Concat(row.added, row.removed) {
		if !commitID.MatchString(id) {
			return "", bookmarkRow{}, false
		}
	}
	switch {
	case row.conflict && len(row.added) == 0:
		return "", bookmarkRow{}, false
	case row.present && !row.conflict && (len(row.added) != 1 || len(row.removed) != 0):
		return "", bookmarkRow{}, false
	}
	return fields[0], row, true
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
	if cloneExists {
		listed, err := RunChecked(ctx, run, onClone(cloneDir, "workspace", "list", "-T", `name ++ "\n"`), nil, "")
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
		own, err := RunChecked(ctx, run, onClone(cloneDir, "log", "-r", ownCommitsRevset(workspaceName), "--no-graph", "-T", `commit_id ++ "\n"`), nil, "")
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
			if _, err := RunChecked(ctx, run, onClone(cloneDir, "abandon", "-r", strings.Join(commits, " | ")), nil, ""); err != nil {
				return err
			}
		}
		if _, err := RunChecked(ctx, run, onClone(cloneDir, "workspace", "forget", workspaceName), nil, ""); err != nil {
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
