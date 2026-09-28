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

// skipChecksLine matches the trailer as a message's line. Case and a trailing carriage return are
// matched because the strip is what the invariant below is derived from: a line this missed would
// be left on a code head, and the check that the head ends as the rule says compares against that
// same strip, so it would agree. Whether GitHub itself honours "Skip-Checks: true" or a CRLF
// message is undocumented and unverified here - this is defence in depth, not a claim about GitHub.
var skipChecksLine = regexp.MustCompile(`(?i)^skip-checks: ?true\s*$`)

// withoutSkipChecks is message without a skip-checks line in its final paragraph, where a trailer
// lives, with trailing space trimmed. The line can be followed there by trailers jj appended after
// an earlier push wrote it (templates.commit_trailers); GitHub honours it only as the last line, so
// such a message runs CI, but the rule is the push's to state, not GitHub's parser's. A message
// with no blank line is one paragraph, and its skip-checks line counts the same: anchoring on the
// last blank line left such a message unchanged, and markHead's read-back never runs on a message
// it does not change, so a code push carried the trailer to GitHub and skipped the CI it owed.
func withoutSkipChecks(message string) string {
	message = strings.TrimRight(message, " \t\r\n")
	body, paragraph := "", message
	if split := strings.LastIndex(message, "\n\n"); split >= 0 {
		body, paragraph = strings.TrimRight(message[:split], " \t\r\n"), message[split+2:]
	}
	lines := strings.Split(paragraph, "\n")
	var kept []string
	for _, line := range lines {
		if !skipChecksLine.MatchString(line) {
			kept = append(kept, line)
		}
	}
	switch {
	case len(kept) == len(lines):
		return message
	case len(kept) == 0:
		return body
	case body == "":
		return strings.Join(kept, "\n")
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
any other verdict or none, the .legion/ deletion, and a rewrite. The paths are read from the
push's commits, not from its net tree diff: no commit may touch a path outside those three, even
one a later commit undoes, because the daemon classifies the push from the union of its commits'
paths and a head this rule skipped over such a commit would carry no verdict at all. A trailer an
earlier push left on @- is removed. Never add or remove the trailer yourself.

Those three files are an allow-list, narrower than the sentence that opens this rule: .legion/
implement.json and .legion/architect.json are excluded although their phases are also followed by
a later push. Narrower is the safe direction and the only one. The daemon's carry rule accepts any
.legion/ path (classify.ClassifyPush), so every head this command skips is one it will carry a
verdict to; widening the set here without widening that rule is what breaks, and the row "the
implementer's handoff alone" in TestPushSkipsCIOnlyForHandoffsALaterPushFollows pins the
exclusion. Do not reconcile the two by widening this set.

Reading the paths from the commits also bounds their union at three, so the listener's cap on the
paths it publishes - past which it marks the list truncated, which the daemon reads as an
unclassifiable push and carries nothing for - can never be reached by a push this skips.

GitHub also starts no workflow for a push whose head message carries [skip ci], [ci skip],
[no ci], [skip actions] or [actions skip], anywhere in the message rather than only as its last
line. Whether a push skips is this command's to decide, so a head whose message carries one of
those is refused before anything is pushed, naming the keyword, whether or not the push skips: a
keyword on a skipping push would agree with the decision, and then the message rather than this
command would be what stopped the workflow.

Only @-'s message is read for a keyword, where the paths are read from every commit in the push.
That is a scope decision, and GitHub's documentation does not settle it: it names the head commit
for pull_request and says only "the commit message in a push" for push. It was measured instead,
on a repository whose workflows are on: [push, pull_request] with no branch filter, where the push
trigger is live on a branch: one push of two commits whose first carried [skip ci] and whose head
carried none started both push-event runs on the head, as did a control push carrying no keyword
anywhere. A keyword below the head does not suppress the head's workflows, so reading @- alone is
what GitHub reads.

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
// The permitted paths are asked once, of the commits rather than of the net tree diff, because the
// daemon classifies the push from the union of its commits' added/removed/modified paths (the
// listener's changed_paths, githubPushChangedPaths in
// packages/envoy/internal/contracts/normalize.go). A push whose commits touch another path and
// then undo it is handoff-only to a net diff and code-changing to classify.ClassifyPush, and a
// head that skips CI while the daemon refuses to carry a verdict to it ends with no verdict at all
// - a fix attempt counted against a push that changed no code, and a red that does not send the
// tree back until the next full push. Asking it of the commits also bounds the union at three
// paths, so the listener's cap on the paths it publishes, which marks a capped list truncated and
// reaches the daemon as Unknown, can never be reached by a push this skips.
//
// The net diff is then read for what only it can say: that the push changes something at all, that
// each handoff is present at @- rather than deleted, and that a review is a request for changes.
func skipsCI(jj, dir, pushed, rewritten string) (bool, error) {
	if rewritten != "" {
		return false, nil
	}
	base := pushed
	if base == "" {
		base = "heads(::@- & ::trunk())"
	}
	const permitted = `files(~".legion/plan.json" & ~".legion/test.json" & ~".legion/review.json")`
	touching, err := pushJJ(jj, dir, "log", "--no-graph", "-T", `commit_id ++ "\n"`, "-r", base+"..@- & "+permitted)
	if err != nil || touching != "" {
		return false, err
	}
	changed, err := pushJJ(jj, dir, "diff", "--name-only", "--from", base, "--to", "@-")
	if err != nil || changed == "" {
		return false, err
	}
	for _, path := range strings.Split(changed, "\n") {
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

// skipKeywords are GitHub's bracket keywords: a push whose head message carries one of them
// anywhere, not only as its last line, starts no workflow run. They are the same end state as the
// trailer through a different spelling, and nothing in the message writes them by accident - this
// repository's own release bot writes "chore: release ... [skip ci]" subjects, so the string is in
// the corpus a worker reads and copies. Case and a hyphen in place of the space are matched too:
// refusing a message GitHub would have run costs a describe, and missing one costs a code head no
// CI ran on.
var skipKeywords = regexp.MustCompile(`(?i)\[(?:skip[ -]ci|ci[ -]skip|no[ -]ci|skip[ -]actions|actions[ -]skip)\]`)

// markHead ends @-'s message with the skip-checks trailer when the push skips CI and removes one
// otherwise, describing @- only when its message changes. GitHub honours the trailer only as the
// message's last line, and jj appends templates.commit_trailers to every message it describes (a
// pane's overlay adds its Omp-Session trailer, which the message already carries from the commit),
// so the describe runs with that template empty.
//
// The property the push owes GitHub is one of the pushed message, so the message this function
// leaves on @- is asserted before anything is pushed, and the assertion runs on every path
// through it - the described message and the one already correct alike. An assertion reached only
// from the branch that describes is not a guarantee: both defects this closes arrived through a
// return that never made it there.
//
// Two things are asserted. The message must end as the rule says, naming the line it ends with
// when it does not. And it must carry none of GitHub's bracket keywords, which start no workflow
// run wherever they stand in the message rather than only as its last line: whether a push skips
// is this command's decision, and a message carrying one takes it away without the last line ever
// looking wrong.
func markHead(jj, dir string, skip bool) error {
	message, err := pushJJ(jj, dir, "log", "--no-graph", "-T", "description", "-r", "@-")
	if err != nil {
		return err
	}
	// A carriage return hides the blank line the trailer paragraph is found by, and jj drops it on
	// the way back in, so both the strip and the comparison read the message jj would store.
	message = strings.TrimRight(strings.ReplaceAll(message, "\r\n", "\n"), " \t\r\n")
	want := withoutSkipChecks(message)
	if skip {
		want += "\n\n\n" + skipChecksTrailer
	}
	if want != message {
		if _, err := pushJJ(jj, dir, "--config", `templates.commit_trailers=""`, "describe", "-r", "@-", "-m", want); err != nil {
			return err
		}
		if message, err = pushJJ(jj, dir, "log", "--no-graph", "-T", "description", "-r", "@-"); err != nil {
			return err
		}
		message = strings.TrimRight(strings.ReplaceAll(message, "\r\n", "\n"), " \t\r\n")
	}
	if message != want {
		lines := strings.Split(message, "\n")
		if skip {
			return fmt.Errorf("@-'s message ends with %q after legion push described it, not with GitHub's %q trailer, so GitHub would run the CI this push skips; nothing was pushed", lines[len(lines)-1], skipChecksTrailer)
		}
		return fmt.Errorf("@-'s message ends with %q after legion push described it, not as its message without the %q trailer; nothing was pushed", lines[len(lines)-1], skipChecksTrailer)
	}
	if keyword := skipKeywords.FindString(message); keyword != "" {
		return fmt.Errorf("@-'s message carries GitHub's %s keyword, which starts no workflow run for this push wherever it stands in the message: legion push decides whether a push skips CI, so take it out of the message and push again; nothing was pushed", keyword)
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
