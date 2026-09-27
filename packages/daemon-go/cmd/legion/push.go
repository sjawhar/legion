package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// skipChecksTrailer is GitHub's instruction to start no push or pull_request workflow run for the
// head commit of a push: its message ends with two empty lines and this line (GitHub's "Skipping
// workflow runs"). A skipped head gets no checks, so a required check waits on it and the head
// cannot merge; the Go daemon carries the code head's settlement to it instead
// (classify.SettlementFor).
const skipChecksTrailer = "skip-checks: true"

var trailingSkipChecks = regexp.MustCompile(`(?:\s*\n)?skip-checks: ?true\s*$`)

// runPush is `legion push`: the worker skill's push procedure for the issue branch
// (skills/legion-worker/SKILL.md, "Every role pushes its own commits"), run by the pane rather than
// typed by the agent, which also decides whether the push carries code. It refuses unless @-
// descends from the remote branch (or from the tip a rewrite recorded), then pushes @- to
// legion/<issue>. A fast-forward that changes only .legion/ and leaves .legion/ in the tree - a
// handoff push - ends @-'s message with GitHub's skip-checks trailer, so the push starts no CI;
// every other push, the .legion/ deletion and a rewrite among them, runs in full, and a trailer an
// earlier push left on @- is removed.
func runPush(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("push", stderr)
	workspaceFlag := flags.String("workspace", "", "workspace directory (default $LEGION_WORKSPACE, else the current directory)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: legion push [--workspace <dir>]")
		return 2
	}
	if err := push(*workspaceFlag, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "legion push: %v\n", err)
		return 1
	}
	return 0
}

func push(workspaceFlag string, stdout, stderr io.Writer) error {
	issue := os.Getenv("LEGION_ISSUE")
	if issue == "" {
		return errors.New("LEGION_ISSUE is not set")
	}
	jj := os.Getenv("LEGION_JJ_PATH")
	if jj == "" || !filepath.IsAbs(jj) {
		return errors.New("LEGION_JJ_PATH is not an absolute path; the Legion daemon names the jj it resolved at boot on every pane")
	}
	dir := workspaceFlag
	if dir == "" {
		dir = os.Getenv("LEGION_WORKSPACE")
	}
	dir, err := resolveWorkspace(dir)
	if err != nil {
		return err
	}
	bookmark := workspace.Bookmark(issue)
	remote := fmt.Sprintf(`remote_bookmarks(exact:%q, exact:"origin")`, bookmark)
	tipFile := filepath.Join(os.TempDir(), fmt.Sprintf("legion-%s-%s-rewritten-tip", issue, os.Getenv("LEGION_ROLE")))
	rewritten := ""
	if recorded, err := os.ReadFile(tipFile); err == nil {
		rewritten = strings.TrimSpace(string(recorded))
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read the recorded rewritten tip %s: %w", tipFile, err)
	}
	ancestry := "::@-"
	if rewritten != "" {
		ancestry = "(::@- | " + rewritten + ")"
	}
	behind, err := pushJJ(jj, dir, "log", "--no-graph", "-T", `commit_id.short() ++ "\n"`, "-r", remote+" ~ "+ancestry)
	if err != nil {
		return err
	}
	if behind != "" {
		return fmt.Errorf("%s@origin is at %s, which @- does not descend from: another role pushed since your chain was made; fetch and put your commits on top of it", bookmark, strings.ReplaceAll(behind, "\n", ", "))
	}
	head, err := pushJJ(jj, dir, "log", "--no-graph", "-T", "commit_id", "-r", "@-")
	if err != nil {
		return err
	}
	pushed, err := pushJJ(jj, dir, "log", "--no-graph", "-T", "commit_id", "-r", remote)
	if err != nil {
		return err
	}
	if head != pushed {
		handoffOnly, err := handoffOnlyPush(jj, dir, pushed, rewritten)
		if err != nil {
			return err
		}
		if err := markHead(jj, dir, handoffOnly); err != nil {
			return err
		}
		if handoffOnly {
			fmt.Fprintf(stdout, "legion push: the push changes only .legion/, so @- ends with %q and GitHub starts no CI for it\n", skipChecksTrailer)
		}
	}
	if _, err := pushJJ(jj, dir, "bookmark", "set", bookmark, "-r", "@-", "--allow-backwards"); err != nil {
		return err
	}
	// jj reports the push on stderr, which the agent reads as the push's outcome.
	gitPush := exec.Command(jj, "-R", dir, "git", "push", "--bookmark", bookmark)
	gitPush.Dir, gitPush.Stdout, gitPush.Stderr = dir, stderr, stderr
	if err := gitPush.Run(); err != nil {
		return fmt.Errorf("jj git push --bookmark %s: %w", bookmark, err)
	}
	if err := os.Remove(tipFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the recorded rewritten tip %s: %w", tipFile, err)
	}
	return nil
}

// handoffOnlyPush is whether pushing @- onto the remote branch at pushed (none when the branch is
// not on GitHub yet) changes only .legion/ and leaves .legion/ in the tree. A rewrite is not one:
// GitHub lists a forced push's commits since the merge base, so the daemon carries no settlement
// across it, and the head must run its own CI.
func handoffOnlyPush(jj, dir, pushed, rewritten string) (bool, error) {
	if rewritten != "" {
		return false, nil
	}
	base := pushed
	if base == "" {
		base = "heads(::@- & ::trunk())"
	}
	changed, err := pushJJ(jj, dir, "diff", "--name-only", "--from", base, "--to", "@-")
	if err != nil {
		return false, err
	}
	if changed == "" {
		return false, nil
	}
	for _, path := range strings.Split(changed, "\n") {
		if !strings.HasPrefix(path, ".legion/") {
			return false, nil
		}
	}
	// The .legion/ deletion is the merge head: every changed path is under .legion/, and it runs
	// in full.
	kept, err := pushJJ(jj, dir, "file", "list", "-r", "@-", `root:".legion"`)
	if err != nil {
		return false, err
	}
	return kept != "", nil
}

// markHead ends @-'s message with the skip-checks trailer when the push is handoff-only and
// removes one otherwise, describing @- only when its message changes.
func markHead(jj, dir string, handoffOnly bool) error {
	message, err := pushJJ(jj, dir, "log", "--no-graph", "-T", "description", "-r", "@-")
	if err != nil {
		return err
	}
	want := strings.TrimRight(trailingSkipChecks.ReplaceAllString(message, ""), " \t\n")
	if handoffOnly {
		want += "\n\n\n" + skipChecksTrailer
	}
	if want == strings.TrimRight(message, " \t\n") {
		return nil
	}
	_, err = pushJJ(jj, dir, "describe", "-r", "@-", "-m", want)
	return err
}

// pushJJ runs the boot-resolved jj on the workspace and returns its trimmed output, stderr
// included in the error when it fails.
func pushJJ(jj, dir string, args ...string) (string, error) {
	command := exec.Command(jj, append([]string{"-R", dir}, args...)...)
	command.Dir = dir
	var failure strings.Builder
	command.Stderr = &failure
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("jj %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(failure.String()))
	}
	return strings.TrimSpace(string(output)), nil
}
