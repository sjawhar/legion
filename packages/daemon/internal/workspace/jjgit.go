package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// The plumbing a release's two halves share: the shim's save of an issue workspace's unpushed work
// (internal/shim) and the launcher's restore of it into the pod that replaces the released one
// (internal/runtime/sandbox's launcher is the daemon's side; internal/launcher runs in the pod).
// Both read the same jj workspace with the same git, so one copy of each answer keeps them from
// drifting apart: a save that found the git directory one way and a restore that found it another
// would put the work back somewhere the save never took it from.

// GitDir is the git directory behind a jj workspace, read from jj's own files rather than from a
// git that would search upward from wherever it was started: <workspace>/.jj/repo names the
// repository (a directory in the repository's own workspace, a file holding its path in every
// other), and <repo>/store/git_target names the git directory relative to <repo>/store. Each
// caller wraps the error with the prefix its own component reports under.
func GitDir(workspace string) (string, error) {
	repo := filepath.Join(workspace, ".jj", "repo")
	info, err := os.Stat(repo)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", repo, err)
	}
	if !info.IsDir() {
		named, err := os.ReadFile(repo)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", repo, err)
		}
		repo = absoluteUnder(filepath.Join(workspace, ".jj"), strings.TrimSpace(string(named)))
	}
	target, err := os.ReadFile(filepath.Join(repo, "store", "git_target"))
	if err != nil {
		return "", fmt.Errorf("read the workspace's git directory: %w", err)
	}
	return absoluteUnder(filepath.Join(repo, "store"), strings.TrimSpace(string(target))), nil
}

// absoluteUnder resolves path as jj writes it: absolute as it stands, relative to base otherwise.
func absoluteUnder(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(base, path))
}

// RefUpdates is the `git update-ref --stdin` input that creates or deletes one temporary ref per
// saved head, named prefix followed by the head's place in tips, in the order the heads were read.
// verb is `create` or `delete`; a delete names no commit. The save and the restore each pass the
// prefix their own refs live under, which is the only difference between them.
func RefUpdates(verb, prefix string, tips []string) []byte {
	var out strings.Builder
	for index, tip := range tips {
		out.WriteString(verb + " " + prefix + strconv.Itoa(index))
		if verb == "create" {
			out.WriteString(" " + tip)
		}
		out.WriteString("\n")
	}
	return []byte(out.String())
}

// OutputLines is out's non-empty lines, trimmed: what a jj or git command's output is read as,
// and what a multi-line report is joined back from for a notice that must be one line.
func OutputLines(out string) []string {
	var kept []string
	for line := range strings.SplitSeq(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	return kept
}

// commandWaitDelay bounds the wait for a jj or git child's output once that child has exited or
// been killed at its budget: a process of its own that holds the pipe open past it is left behind
// rather than kept waited on.
const commandWaitDelay = time.Second

// RunCommand runs one jj or git command of the workspace within the caller's budget and returns
// its trimmed standard output. dir is the directory the command runs in, which is the workspace
// its -R or --git-dir names: jj searches upward from the directory it runs in for the closest
// .jj and reads that repository's configuration before it applies -R, so a command left standing
// wherever the caller's process stands would read a repository nobody named (docs/testing.md).
// stdin is fed to the command, and nil is an empty standard input. started is when the caller's
// budget began, which a timeout's report names. A failure's report ends with a note on each
// stale git lock it names (staleGitLockNote). The command timed out only when the budget ended
// it: once the command has exited on its own, commandWaitDelay can hold Run open past the budget
// for a child still holding the command's pipes, and the command's own exit is then its answer. A
// command that never started because an earlier one on the same budget spent it timed out too,
// while a command the caller stopped by cancelling its context, rather than the budget ending
// it, reports its exit, not a timeout.
//
// The save (internal/shim) and the restore (internal/launcher) both run here: they run the same
// two tools over the same workspace, and a restore wedged on a stale index.lock or on a git
// child that holds the pipe past the budget needs the same note and the same WaitDelay the save
// has.
func RunCommand(ctx context.Context, budget time.Duration, started time.Time, env []string, dir string, stdin []byte, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = env
	cmd.Dir = dir
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = commandWaitDelay
	killed := procgroup.KilledByContext(cmd)
	err := cmd.Run()
	command := strings.Join(args, " ")
	// The budget ended the command only when its kill is what ended it (procgroup.KilledByContext,
	// never the deadline having passed by the time Run returned) and the budget is what ended ctx,
	// which is how execRunner reads its own TimedOut (config.go).
	timedOut := (killed() && errors.Is(ctx.Err(), context.DeadlineExceeded)) ||
		(cmd.Process == nil && errors.Is(err, context.DeadlineExceeded))
	if runErr := procgroup.Err(err); runErr != nil && !timedOut {
		return "", fmt.Errorf("run %s: %w", command, runErr)
	}
	if !timedOut && cmd.ProcessState.ExitCode() == 0 {
		return strings.TrimSpace(stdout.String()), nil
	}
	report := strings.TrimSpace(stderr.String())
	if note := staleGitLockNote(report, time.Now()); note != "" {
		report += "\n" + note
	}
	if timedOut {
		message := fmt.Sprintf("Command timed out after %s s (ran %.1f s): %s",
			strconv.FormatFloat(budget.Seconds(), 'f', -1, 64), time.Since(started).Seconds(), command)
		if report != "" {
			message += "\n" + report
		}
		return "", errors.New(message)
	}
	return "", fmt.Errorf("Command failed (exit %d): %s\n%s", cmd.ProcessState.ExitCode(), command, report)
}

// gitLockReports are the ways a git or jj failure names the lock file it could not take: jj's git
// backend (gitoxide), and git's own lockfile API, which every git jj runs reports through.
var gitLockReports = []*regexp.Regexp{
	regexp.MustCompile(`The lockfile at '([^']+)' might need manual deletion`),
	regexp.MustCompile(`Unable to create '([^']+)': File exists`),
}

// staleGitLockNote is one line for each git lock file output names that is still on disk: its
// path, how long ago it was last written, and what becomes of it. A git or jj interrupted
// mid-write leaves its lock, and every later write of what it locks fails on it. In a pod no
// process removes it while the pod runs, since another role's git, in a container whose
// processes this one cannot see, may hold it; replacing the issue's pod does (`workspace-init
// provision`, cmd/legion/workspace_init.go). Under tmux nothing removes it. The Legion tools'
// staleGitLockNote (packages/pi-shared/src/git-lock.ts) words the same note for its own jj
// calls. Empty when output names no lock, or each one it names is already gone.
func staleGitLockNote(output string, now time.Time) string {
	var locks, notes []string
	for _, report := range gitLockReports {
		for _, match := range report.FindAllStringSubmatch(output, -1) {
			if !slices.Contains(locks, match[1]) {
				locks = append(locks, match[1])
			}
		}
	}
	for _, lock := range locks {
		info, err := os.Lstat(lock)
		if err != nil {
			continue
		}
		notes = append(notes, fmt.Sprintf("git lock %s was last written %s ago and blocks this command. "+
			"Do not remove it while the issue's pod runs: another role's git, which this container cannot see, may hold it. "+
			"Replacing the issue's pod removes it. "+
			"Under the tmux runtime nothing removes it: remove it by hand only once no git or jj process on the host is using the repository.",
			lock, now.Sub(info.ModTime()).Round(time.Second)))
	}
	return strings.Join(notes, "\n")
}
