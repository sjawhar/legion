package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
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

var skipChecksLine = regexp.MustCompile(`^skip-checks: ?true\s*$`)

// withoutSkipChecks is message without a skip-checks line in its final paragraph, where a trailer
// lives, with trailing space trimmed. The line can be followed there by trailers jj appended after
// an earlier push wrote it (templates.commit_trailers); GitHub honours it only as the last line, so
// such a message runs CI, but the rule is the push's to state, not GitHub's parser's.
func withoutSkipChecks(message string) string {
	message = strings.TrimRight(message, " \t\n")
	split := strings.LastIndex(message, "\n\n")
	if split < 0 {
		return message
	}
	var kept []string
	for _, line := range strings.Split(message[split+2:], "\n") {
		if !skipChecksLine.MatchString(line) {
			kept = append(kept, line)
		}
	}
	if len(kept) == len(strings.Split(message[split+2:], "\n")) {
		return message
	}
	body := strings.TrimRight(message[:split], " \t\n")
	if len(kept) == 0 {
		return body
	}
	return body + "\n\n" + strings.Join(kept, "\n")
}

// pushHelp is `legion push`'s rule, as its help states it.
const pushHelp = `usage: legion push [--workspace <dir>]

Pushes @- to legion/<LEGION_ISSUE>, the worker skill's push of the issue branch: it refuses unless
@- descends from legion/<issue>@origin, or from the tip recorded before rewriting pushed commits,
then sets the bookmark and pushes.

A push skips CI only when it changes nothing but handoffs whose phase guarantees a later push to
the branch: the planner's .legion/plan.json, the tester's .legion/test.json, and a reviewer's
.legion/review.json whose verdict is "changes_requested". Its head commit then ends with GitHub's
"skip-checks: true" trailer, so the push starts no workflow, and the Go daemon carries the code
head's verdict to it. Every other push runs CI in full: one carrying code, a reviewer round with
any other verdict or none, the .legion/ deletion, and a rewrite. No commit of the push may touch a
path outside .legion/, even one a later commit undoes: the daemon classifies the push from the
union of its commits' paths, so a head this rule skipped over such a commit would carry no verdict
at all. A trailer an earlier push left on @- is removed. Never add or remove the trailer yourself.

GitHub honours the trailer only as the message's last line, so the push describes @- with jj's
templates.commit_trailers empty (the trailers @- already carries stay above it) and reads the
message back: when it does not end as this rule says, nothing is pushed and the command exits 1
naming the line it ends with.
`

// runPush is `legion push`, whose rule pushHelp states: the pane decides whether a push skips CI,
// from the paths it changes and the handoffs it carries, so no later push is ever missing and no
// head a human may merge is left without checks.
func runPush(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("push", stderr)
	flags.Usage = func() { fmt.Fprint(stderr, pushHelp) }
	workspaceFlag := flags.String("workspace", "", "workspace directory (default $LEGION_WORKSPACE, else the current directory)")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, pushHelp)
		}
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
		skip, err := skipsCI(jj, dir, pushed, rewritten)
		if err != nil {
			return err
		}
		if err := markHead(jj, dir, skip); err != nil {
			return err
		}
		if skip {
			fmt.Fprintf(stdout, "legion push: the push carries only handoffs a later push follows, so @- ends with %q and GitHub starts no CI for it\n", skipChecksTrailer)
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

// skipsCI is whether pushing @- onto the remote branch at pushed (none when the branch is not on
// GitHub yet) changes nothing but handoffs whose phase guarantees a later push: the planner's plan
// (the implementer's code follows), the tester's test (the reviewer's handoff or the implementer's
// fix follows) and a reviewer's request for changes (the implementer's fix follows). Any other
// push may leave the head a human merges, which must carry its own checks. A rewrite never skips:
// GitHub lists a forced push's commits since the merge base, so the daemon carries nothing across
// it.
//
// The daemon asks the same question of the same push from the union of its commits'
// added/removed/modified paths (the listener's changed_paths, githubPushChangedPaths in
// packages/envoy/internal/contracts/normalize.go), not from the net tree diff, so this asks it that
// way too: a push whose commits touch a path outside .legion/ and then undo it is handoff-only to a
// net diff and code-changing to classify.ClassifyPush, and a head that skips CI while the daemon
// refuses to carry a verdict to it ends with no verdict at all — a fix attempt counted against a
// push that changed no code, and a red that does not send the tree back until the next full push.
func skipsCI(jj, dir, pushed, rewritten string) (bool, error) {
	if rewritten != "" {
		return false, nil
	}
	base := pushed
	if base == "" {
		base = "heads(::@- & ::trunk())"
	}
	touching, err := pushJJ(jj, dir, "log", "--no-graph", "-T", `commit_id ++ "\n"`, "-r", base+`..@- & files(~".legion/**")`)
	if err != nil || touching != "" {
		return false, err
	}
	changed, err := pushJJ(jj, dir, "diff", "--name-only", "--from", base, "--to", "@-")
	if err != nil || changed == "" {
		return false, err
	}
	for _, path := range strings.Split(changed, "\n") {
		if path != ".legion/plan.json" && path != ".legion/test.json" && path != ".legion/review.json" {
			return false, nil
		}
		// A deleted handoff is no handoff: `jj file show` of a path @- does not hold fails.
		content, err := pushJJ(jj, dir, "file", "show", "-r", "@-", fmt.Sprintf("root:%q", path))
		if err != nil {
			return false, nil
		}
		if path == ".legion/review.json" {
			var review struct {
				Verdict string `json:"verdict"`
			}
			if json.Unmarshal([]byte(content), &review) != nil || review.Verdict != "changes_requested" {
				return false, nil
			}
		}
	}
	return true, nil
}

// markHead ends @-'s message with the skip-checks trailer when the push skips CI and removes one
// otherwise, describing @- only when its message changes. GitHub honours the trailer only as the
// message's last line, and jj appends templates.commit_trailers to every message it describes (a
// pane's overlay adds its Omp-Session trailer, which the message already carries from the commit),
// so the describe runs with that template empty. The message is then read back: the property the
// push owes GitHub is one of the pushed message, so a head whose message does not end as the rule
// says is refused before anything is pushed, naming the line it ends with.
func markHead(jj, dir string, skip bool) error {
	message, err := pushJJ(jj, dir, "log", "--no-graph", "-T", "description", "-r", "@-")
	if err != nil {
		return err
	}
	want := withoutSkipChecks(message)
	if skip {
		want += "\n\n\n" + skipChecksTrailer
	}
	if want == strings.TrimRight(message, " \t\n") {
		return nil
	}
	if _, err := pushJJ(jj, dir, "--config", `templates.commit_trailers=""`, "describe", "-r", "@-", "-m", want); err != nil {
		return err
	}
	described, err := pushJJ(jj, dir, "log", "--no-graph", "-T", "description", "-r", "@-")
	if err != nil {
		return err
	}
	if described != want {
		lines := strings.Split(described, "\n")
		if skip {
			return fmt.Errorf("@-'s message ends with %q after legion push described it, not with GitHub's %q trailer, so GitHub would run the CI this push skips; nothing was pushed", lines[len(lines)-1], skipChecksTrailer)
		}
		return fmt.Errorf("@-'s message ends with %q after legion push described it, not as its message without the %q trailer; nothing was pushed", lines[len(lines)-1], skipChecksTrailer)
	}
	return nil
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
