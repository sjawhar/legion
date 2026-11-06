// Package rescue finds the work an issue pod's workspaces hold that their remote lacks, and pushes
// it to rescue branches on that remote, before the daemon replaces the pod. The pod's disk is the
// pod's own and goes with it (docs/kubernetes.md, "The disk dies with its pod"), so this is the
// last place that work can be read from.
//
// It runs inside the pod, in a role's shim (`rescue-unpushed`, shim.rescue), as that role and with
// that role's own GitHub credential: the shared clone's credential helper answers git from the
// role's gh files, so the daemon handles no secret of its own for it. It changes nothing in a
// workspace: no file, no index, no ref of the clone, no jj operation. What it writes is git
// objects, which change nothing anyone reads, and the branches it pushes to the remote.
package rescue

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// The statuses a workspace's check ends in, the runtime's own words for them.
const (
	Clean   = runtime.RescueClean
	Rescued = runtime.RescueRescued
	Failed  = runtime.RescueFailed
)

// ownHeads is the jj revset of the work on a workspace's own line, reduced to its heads: the
// ancestors of its working-copy commit and of the bookmarks its issue owns (ownedBookmarks), less
// every empty commit with no description. It is the first of the two sources of a workspace's own
// work; the second is what was made in the pod: every visible head and every local git branch
// that no commit the clone held when it was made contains (visibleWorkHeads, madeInThePod). That
// is what holds a role's side line and the merge it left when its working copy moved back. Whether
// origin holds a head is git's question, asked of what the remote advertises now (unpushed), so
// nothing here subtracts what jj last saw remote.
//
// Neither source is the whole clone. A pod's shared clone keeps every commit it ever fetched
// visible (workspace-init sets git.abandon-unreachable-commits false), so jj's visible heads hold
// old merges, other trees' handoffs and the pre-rebase copies of rewritten branches, which origin
// deleted or rewrote and no role of the pod wrote: rescuing them pushed one branch for each, and
// the pod's own work was one of sixteen.
//
// An empty commit with no description — jj's working-copy commit before anything is written —
// holds no work, and dropping it makes the commit under it a head. An empty commit with a
// description is kept: an empty merge of two pushed commits is the one record of that merge, as it
// is when a worker merges, leaves the issue's bookmark behind and its push says "Nothing changed".
// refs/jj/keep is not read: it holds the obsolete predecessor of every rewritten commit.
func ownHeads(bookmarks []string) string {
	own := "@"
	for _, b := range bookmarks {
		own += ` | bookmarks(exact:` + jjString(b) + `)`
	}
	return `heads(::(` + own + `) ~ (empty() & description(exact:"")))`
}

// ownedBookmarks is the bookmarks the issue a workspace's directory names owns: its branch,
// `legion/<KEY>`, read back from the directory's name (workspace.Location lays it out as the issue
// key in lowercase). A directory whose name is no issue key owns none.
func ownedBookmarks(dir string) []string {
	issue := strings.ToUpper(filepath.Base(dir))
	if !claim.IsIssueKey(issue) {
		return nil
	}
	return []string{claim.IssueBranch(issue)}
}

// jjString is name as a jj string literal: quoted, with `\` and `"` escaped, so a bookmark name is
// the one name it is in a revset.
func jjString(name string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(name) + `"`
}

// visibleWorkHeads is every head jj can see that holds something: with the clone's local git
// branches, what madeInThePod reduces to the work the pod made.
const visibleWorkHeads = `heads((::visible_heads()) ~ (empty() & description(exact:"")))`

// madeInThePod is the commits of made that the clone at clone did not hold when it was made: none
// of the commits provisioning recorded then (workspace.RecordHeld, read back with workspace.Held)
// contains them. That record is the one thing that tells a commit the pod made from one it came
// with, whatever the commit says of itself: its committer time is what git or jj was told
// (`git rebase` and `git am` with --committer-date-is-author-date keep the author's,
// GIT_COMMITTER_DATE and JJ_TIMESTAMP set any), a reflog expires and jj's export writes the same
// entry for a bookmark it fetched as for one a role moved, and the clone's operation log can be
// abandoned. Provisioning writes the record once, in the one place it creates the clone, so a
// re-creation of a workspace in the live pod changes nothing of it.
//
// A commit someone else made, which the pod fetched while it ran and origin deleted since, is
// among what this keeps, and is rescued: noise, and never a loss. A clone with no record, made
// by an earlier release, or one whose record cannot be read, keeps every one of made, as the check
// did before it read the record: noise too, and never a loss.
func (r runner) madeInThePod(clone string, made []string) ([]string, error) {
	recorded, ok, err := workspace.Held(clone)
	if err != nil || !ok {
		return made, nil
	}
	held, err := r.objects(clone, recorded, "commit")
	if err != nil {
		return nil, err
	}
	return r.unpushed(clone, made, held)
}

// gitPins is configuration every git this package starts reads last: no hook and no file-system
// monitor runs, whatever the clone's configuration names, and no replacement object (`git replace`,
// a graft among them) stands in for a commit, so a commit grafted to have a role's work as its
// parent never makes that work look held, and the push sends the commits as they are.
var gitPins = []string{"--no-replace-objects", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}

// Request is one check of every workspace on a pod's disk.
type Request struct {
	// StateDir is the disk's root: workspaces under <StateDir>/workspaces/<owner>/<repo>/<issue>,
	// and the shared clone each is a jj workspace of under <StateDir>/repos/github.com/<owner>/<repo>.
	StateDir string
	// Branch is the prefix of every rescue branch: the push of the n-th one is to <Branch>-<n>,
	// with n the least number from 1 that names no branch the remote has and none this check has
	// pushed already.
	Branch string
	// WorkspaceTimeout bounds one workspace's check and push.
	WorkspaceTimeout time.Duration
	// Deadline bounds the whole check: a workspace not reached by then is an error, not a pass.
	Deadline time.Time
	// Env is the environment git and jj run in: the role's own, which names its credential.
	Env []string
}

// StateDirOf is the disk's root a workspace directory is on: four directories up, as
// workspace.Location lays a workspace out (<state>/workspaces/<owner>/<repo>/<issue>).
func StateDirOf(workspace string) string {
	return filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Clean(workspace)))))
}

// Report is a check's entries as the runtime boundary carries them (runtime.WorkspaceRescue).
func Report(workspaces []shimwire.RescueWorkspace) []runtime.WorkspaceRescue {
	report := make([]runtime.WorkspaceRescue, len(workspaces))
	for i, w := range workspaces {
		report[i] = runtime.WorkspaceRescue{Dir: w.Dir, Status: w.Status, Error: w.Error}
		for _, b := range w.Branches {
			report[i].Branches = append(report[i].Branches, runtime.RescuedBranch{Name: b.Name, Commit: b.Commit})
		}
	}
	return report
}

// Run checks every workspace on the disk, in path order, and pushes what each holds that its remote
// lacks. Every workspace gets one entry. A disk with no workspace yields none, which is no work to
// lose.
func Run(ctx context.Context, req Request) []shimwire.RescueWorkspace {
	dirs, err := workspaces(req.StateDir)
	if err != nil {
		return []shimwire.RescueWorkspace{{Dir: req.StateDir, Status: Failed, Error: err.Error()}}
	}
	used := map[string]bool{}
	results := make([]shimwire.RescueWorkspace, 0, len(dirs))
	// seen is every worktree a check has taken already: each workspace is its own check's, and a
	// further worktree of a clone (git worktree list) is taken by the first check of that clone.
	seen := map[string]bool{}
	for _, dir := range dirs {
		seen[dir] = true
	}
	for _, dir := range dirs {
		if !req.Deadline.IsZero() && !time.Now().Before(req.Deadline) {
			results = append(results, shimwire.RescueWorkspace{Dir: dir, Status: Failed,
				Error: "not checked: the check's whole time ran out before it reached this workspace"})
			continue
		}
		bound := req.WorkspaceTimeout
		if !req.Deadline.IsZero() {
			bound = min(bound, time.Until(req.Deadline))
		}
		one, cancel := context.WithTimeout(ctx, bound)
		result := check(one, req, dir, used, seen)
		if one.Err() != nil && result.Status == Failed {
			result.Error = fmt.Sprintf("the check ran out its %s: %s", bound.Round(time.Second), result.Error)
		}
		cancel()
		results = append(results, result)
	}
	return results
}

// workspaces is every workspace directory on the disk: a real directory three levels below
// workspaces/, never a symlink and never a file an agent left beside its workspace.
func workspaces(stateDir string) ([]string, error) {
	if stateDir == "" || !filepath.IsAbs(stateDir) {
		return nil, fmt.Errorf("the disk's root %q is not an absolute path", stateDir)
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "workspaces", "*", "*", "*"))
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, match := range matches {
		if info, err := os.Lstat(match); err == nil && info.IsDir() {
			dirs = append(dirs, match)
		}
	}
	slices.Sort(dirs)
	return dirs, nil
}

// check is one workspace's check and push. The workspace's repository is the one its git names
// (its common git directory), so a directory under workspaces/ that is no git worktree is an error
// rather than a pass: whatever it holds, nothing can push it.
//
// What the remote holds is read from the remote alone: a remote-tracking ref is only as fresh as
// the pod's last fetch, so a commit of the workspace's own that only a stale one names — the
// issue's branch since force-pushed or deleted on the remote — is a commit the remote may no
// longer have, and is rescued.
//
// The candidates are the work that is the workspace's own, and never a commit the clone held when
// it was made. They come from two sources. The workspace's own line: the ancestors of its
// working-copy commit and of its issue's bookmark (ownHeads, ownedBookmarks), and in git the
// issue's branch, every local tag, every entry of the stash, and the HEAD of the workspace and of
// every other worktree of the repository (git worktree list, the clone's own included), each a
// worktree on the pod's disk. And what was made in the pod: every visible jj head and every local
// git branch that no commit the clone held when it was made contains (madeInThePod), a side line a
// role left, a merge it left behind when its working copy moved back, and a commit git made in the
// workspace's detached HEAD that jj then moved away from among them. A candidate that is jj's
// empty working-copy commit stands for the commit under it (holdingWork). Each worktree's files
// that no commit holds become one commit on that worktree's HEAD.
//
// The report also names each rescue branch of this prefix the remote holds already: an earlier
// check of the same pod pushed it, in a round whose replacement did not happen then, and the issue
// must still be told of it once the pod goes.
func check(ctx context.Context, req Request, dir string, used, seen map[string]bool) shimwire.RescueWorkspace {
	result := shimwire.RescueWorkspace{Dir: dir}
	fail := func(err error) shimwire.RescueWorkspace {
		result.Status, result.Branches, result.Error = Failed, nil, err.Error()
		return result
	}
	run := runner{ctx: ctx, env: req.Env}
	common, err := run.git(dir, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fail(fmt.Errorf("%s is not a git worktree, so nothing in it can be pushed: %w", dir, err))
	}
	clone := common
	if filepath.Base(common) == ".git" {
		clone = filepath.Dir(common)
	}

	listed, err := run.git(clone, nil, "ls-remote", "origin")
	if err != nil {
		return fail(err)
	}
	remoteBranches := map[string]bool{}
	var advertised []string
	for line := range strings.Lines(listed) {
		sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		advertised = append(advertised, sha)
		if branch, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
			remoteBranches[branch] = true
			if strings.HasPrefix(branch, req.Branch+"-") {
				result.Branches = append(result.Branches, shimwire.RescueBranch{Name: branch, Commit: sha})
			}
		}
	}
	held, err := run.objects(clone, advertised, "commit", "tag")
	if err != nil {
		return fail(err)
	}

	owned := ownedBookmarks(dir)
	branches, err := run.git(clone, nil, "for-each-ref", "--format=%(objectname)", "refs/heads")
	if err != nil {
		return fail(err)
	}
	made := fields(branches)
	refs, err := run.git(clone, nil, "for-each-ref",
		"--format=%(if)%(*objectname)%(then)%(*objectname)%(else)%(objectname)%(end)", "refs/tags")
	if err != nil {
		return fail(err)
	}
	candidates := fields(refs)
	for _, b := range owned {
		if commit, err := run.git(clone, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+b+"^{commit}"); err == nil {
			candidates = append(candidates, commit)
		}
	}
	if _, err := run.git(clone, nil, "rev-parse", "--verify", "--quiet", "refs/stash"); err == nil {
		stash, err := run.git(clone, nil, "log", "--walk-reflogs", "--format=%H", "refs/stash")
		if err != nil {
			return fail(err)
		}
		candidates = append(candidates, fields(stash)...)
	}
	worktrees, err := run.worktrees(clone)
	if err != nil {
		return fail(err)
	}
	// The workspace itself, then every worktree of the repository no check has taken yet.
	mine := []string{dir}
	for _, w := range worktrees {
		if !seen[w] {
			seen[w] = true
			mine = append(mine, w)
		}
	}
	heads := map[string]string{}
	for _, w := range mine {
		head, err := run.git(w, nil, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
		if err != nil {
			continue // an unborn HEAD: a worktree with no git-visible commit yet
		}
		heads[w] = head
		candidates = append(candidates, head)
	}
	if info, err := os.Lstat(filepath.Join(dir, ".jj")); err == nil && info.IsDir() {
		jjHeadsOut, err := run.jj(dir, "log", "--no-graph", "-r", ownHeads(owned), "-T", `commit_id ++ "\n"`)
		if err != nil {
			return fail(err)
		}
		candidates = append(candidates, fields(jjHeadsOut)...)
		visible, err := run.jj(dir, "log", "--no-graph", "-r", visibleWorkHeads, "-T", `commit_id ++ "\n"`)
		if err != nil {
			return fail(err)
		}
		made = append(made, fields(visible)...)
	}
	made, err = run.objects(clone, made, "commit")
	if err != nil {
		return fail(err)
	}
	made, err = run.holdingWork(clone, made)
	if err != nil {
		return fail(err)
	}
	made, err = run.madeInThePod(clone, made)
	if err != nil {
		return fail(err)
	}
	candidates = append(candidates, made...)
	candidates, err = run.objects(clone, candidates, "commit")
	if err != nil {
		return fail(err)
	}
	candidates, err = run.holdingWork(clone, candidates)
	if err != nil {
		return fail(err)
	}

	tips, err := run.unpushed(clone, candidates, held)
	if err != nil {
		return fail(err)
	}
	for _, w := range mine {
		edits, err := run.edits(w, heads[w])
		if err != nil {
			return fail(err)
		}
		if edits == "" {
			continue
		}
		tree, err := run.git(clone, nil, "rev-parse", edits+"^{tree}")
		if err != nil {
			return fail(err)
		}
		same := false
		for _, tip := range tips {
			if t, err := run.git(clone, nil, "rev-parse", tip+"^{tree}"); err == nil && t == tree {
				same = true
				break
			}
		}
		if !same {
			tips = append(tips, edits)
		}
	}
	if len(tips) > 1 {
		independent, err := run.git(clone, nil, append([]string{"merge-base", "--independent"}, tips...)...)
		if err != nil {
			return fail(err)
		}
		tips = fields(independent)
		slices.Sort(tips)
	}
	if len(tips) > 0 {
		refspecs := make([]string, 0, len(tips))
		var pushed []shimwire.RescueBranch
		for _, tip := range tips {
			name := ""
			for n := 1; ; n++ {
				name = req.Branch + "-" + strconv.Itoa(n)
				if !remoteBranches[name] && !used[name] {
					break
				}
			}
			used[name] = true
			refspecs = append(refspecs, tip+":refs/heads/"+name)
			pushed = append(pushed, shimwire.RescueBranch{Name: name, Commit: tip})
		}
		if _, err := run.git(clone, []string{"GIT_TERMINAL_PROMPT=0"}, append([]string{"push", "--atomic", "--quiet", "origin"}, refspecs...)...); err != nil {
			return fail(fmt.Errorf("push %s: %w", strings.Join(refspecs, " "), err))
		}
		result.Branches = append(result.Branches, pushed...)
	}
	result.Status = Clean
	if len(result.Branches) > 0 {
		result.Status = Rescued
	}
	return result
}

// worktrees is every worktree of the repository whose directory is there, as `git worktree list`
// names it, the clone's own working tree included; a bare repository's entry has none.
func (r runner) worktrees(clone string) ([]string, error) {
	out, err := r.git(clone, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var dirs []string
	current, bare := "", false
	flush := func() {
		if current != "" && !bare {
			if info, err := os.Lstat(current); err == nil && info.IsDir() {
				dirs = append(dirs, filepath.Clean(current))
			}
		}
		current, bare = "", false
	}
	for line := range strings.Lines(out + "\n") {
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			current = strings.TrimPrefix(line, "worktree ")
		case line == "bare":
			bare = true
		case line == "":
			flush()
		}
	}
	flush()
	return dirs, nil
}

// runner runs git and jj under one workspace's bound, in the role's environment.
type runner struct {
	ctx context.Context
	env []string
}

func (r runner) git(dir string, env []string, args ...string) (string, error) {
	return r.command(dir, env, nil, "git", append(slices.Clone(gitPins), args...)...)
}

// jj runs a read of jj's view that writes nothing: no snapshot of the working copy, and no new
// operation, since it reads at the operation the store is at.
func (r runner) jj(dir string, args ...string) (string, error) {
	return r.command(dir, nil, nil, "jj", append([]string{"--ignore-working-copy", "--at-op=@", "--no-pager", "--color=never", "-R", dir}, args...)...)
}

func (r runner) command(dir string, env []string, stdin io.Reader, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(r.ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(slices.Clone(r.env), env...)
	cmd.Stdin = stdin
	procgroup.Configure(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := procgroup.Err(cmd.Run()); err != nil {
		return "", fmt.Errorf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		return "", fmt.Errorf("%s %s: exit %d: %s", name, strings.Join(args, " "), code, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// objects is the shas the clone has an object of one of kinds for, in the order given: what the
// remote advertises is held only where the clone has it, since a local commit can be an ancestor of
// nothing else, and a candidate is a commit or nothing rev-list may be handed.
func (r runner) objects(clone string, shas []string, kinds ...string) ([]string, error) {
	if len(shas) == 0 {
		return nil, nil
	}
	out, err := r.command(clone, nil, strings.NewReader(strings.Join(shas, "\n")+"\n"), "git",
		append(slices.Clone(gitPins), "cat-file", "--batch-check=%(objectname) %(objecttype)")...)
	if err != nil {
		return nil, err
	}
	var kept []string
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		sha, kind, _ := strings.Cut(scanner.Text(), " ")
		if slices.Contains(kinds, kind) {
			kept = append(kept, sha)
		}
	}
	return kept, nil
}

// unpushed is the candidates no held commit contains, each once, in the order given.
func (r runner) unpushed(clone string, candidates, held []string) ([]string, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, c := range candidates {
		input.WriteString(c + "\n")
	}
	for _, h := range held {
		input.WriteString("^" + h + "\n")
	}
	out, err := r.command(clone, nil, strings.NewReader(input.String()), "git", append(slices.Clone(gitPins), "rev-list", "--stdin")...)
	if err != nil {
		return nil, err
	}
	listed := map[string]bool{}
	for _, sha := range fields(out) {
		listed[sha] = true
	}
	var tips []string
	for _, c := range candidates {
		if listed[c] && !slices.Contains(tips, c) {
			tips = append(tips, c)
		}
	}
	return tips, nil
}

// holdingWork is candidates, each once, with every commit that has no message and the tree of its
// one parent replaced by that parent, as often as it takes. That commit is jj's working-copy commit
// before anything is written, as ownHeads drops it on jj's side, and a git ref can name it: jj
// exports a bookmark set on it, as provisioning sets the issue's on the working copy of a workspace
// it starts at the default branch. A merge is kept whatever it holds.
func (r runner) holdingWork(clone string, candidates []string) ([]string, error) {
	for {
		if len(candidates) == 0 {
			return nil, nil
		}
		listed, err := r.git(clone, nil, append([]string{"log", "--no-walk=unsorted", "--format=%H %T %P%x09%s"}, candidates...)...)
		if err != nil {
			return nil, err
		}
		replaced := map[string]string{}
		for line := range strings.Lines(listed) {
			ids, subject, _ := strings.Cut(strings.TrimRight(line, "\n"), "\t")
			commit := strings.Fields(ids)
			if subject != "" || len(commit) != 3 {
				continue
			}
			parentTree, err := r.git(clone, nil, "rev-parse", commit[2]+"^{tree}")
			if err != nil {
				return nil, err
			}
			if parentTree == commit[1] {
				replaced[commit[0]] = commit[2]
			}
		}
		var next []string
		for _, c := range candidates {
			if p, ok := replaced[c]; ok {
				c = p
			}
			if !slices.Contains(next, c) {
				next = append(next, c)
			}
		}
		if len(replaced) == 0 {
			return next, nil
		}
		candidates = next
	}
}

// edits is one commit holding the workspace's files as they stand, on top of head, when they
// differ from head's tree; "" when they do not. It is built in an index of its own, from a copy of
// the workspace's index where it has one, so neither the workspace's index nor any ref moves. A
// file .gitignore ignores is left out, as a role's own push would leave it.
func (r runner) edits(dir, head string) (string, error) {
	scratch, err := os.MkdirTemp("", "legion-rescue-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(scratch)
	index := filepath.Join(scratch, "index")
	env := []string{"GIT_INDEX_FILE=" + index}
	own, err := r.git(dir, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return "", err
	}
	copied := false
	if head != "" {
		if body, err := os.ReadFile(own); err == nil {
			copied = os.WriteFile(index, body, 0o600) == nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	if !copied {
		base := []string{"read-tree", "--empty"}
		if head != "" {
			base = []string{"read-tree", head}
		}
		if _, err := r.git(dir, env, base...); err != nil {
			return "", err
		}
	}
	if _, err := r.git(dir, env, "add", "--all", "--", "."); err != nil {
		return "", err
	}
	tree, err := r.git(dir, env, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"-c", "user.name=Legion rescue", "-c", "user.email=rescue@invalid", "commit-tree", tree,
		"-m", "Edits left in the workspace " + dir + " when its pod was replaced"}
	if head != "" {
		was, err := r.git(dir, nil, "rev-parse", head+"^{tree}")
		if err != nil {
			return "", err
		}
		if was == tree {
			return "", nil
		}
		args = append(args, "-p", head)
	} else if tree == emptyTree {
		return "", nil
	}
	return r.command(dir, nil, nil, "git", append(slices.Clone(gitPins), args...)...)
}

// emptyTree is git's tree with nothing in it.
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// fields is out's lines, each trimmed, the empty ones dropped.
func fields(out string) []string {
	return strings.Fields(out)
}
