package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// pushRig is a pane workspace for LEGION-7 with a bare GitHub stand-in as origin, main pushed there.
// Its jj runs under the pane's own overlay (packages/pi-envoy/src/legion/jj-attribution.ts), which
// appends an Omp-Session trailer to every message jj describes, as a pane's jj does: a rig without
// it proves the push only from a shell.
type pushRig struct {
	t                 *testing.T
	jj, workspace, gh string
}

func newPushRig(t *testing.T) pushRig {
	t.Helper()
	workspace, jj := handoffRepo(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("git is required: %v", err)
	}
	remote := filepath.Join(t.TempDir(), "origin.git")
	if output, err := exec.Command(git, "init", "--bare", "--initial-branch=main", remote).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, output)
	}
	overlay := filepath.Join(t.TempDir(), "jj-attribution.toml")
	if err := os.WriteFile(overlay, []byte(`[templates]`+"\n"+`commit_trailers = '"Omp-Session: ses-pane"'`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JJ_CONFIG", overlay)
	t.Setenv("JJ_USER", "Legion test")
	t.Setenv("JJ_EMAIL", "legion-test@example.invalid")
	t.Setenv("LEGION_ISSUE", "LEGION-7")
	t.Setenv("LEGION_ROLE", "tester")
	t.Setenv("LEGION_JJ_PATH", jj)
	t.Setenv("LEGION_WORKSPACE", workspace)
	t.Setenv("TMPDIR", t.TempDir())
	r := pushRig{t: t, jj: jj, workspace: workspace, gh: remote}
	r.run("git", "remote", "add", "origin", remote)
	r.commit("base", map[string]string{"README.md": "smoke\n"})
	r.run("bookmark", "set", "main", "-r", "@-")
	r.run("git", "push", "--bookmark", "main")
	return r
}

func (r pushRig) run(args ...string) string {
	r.t.Helper()
	return handoffJJ(r.t, r.jj, r.workspace, args...)
}

// commit writes files (an empty content deletes the file) and commits them as @- with message.
func (r pushRig) commit(message string, files map[string]string) {
	r.t.Helper()
	for name, content := range files {
		path := filepath.Join(r.workspace, name)
		if content == "" {
			if err := os.Remove(path); err != nil {
				r.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			r.t.Fatal(err)
		}
	}
	r.run("commit", "-m", message)
}

// push runs `legion push` from the workspace and returns its exit code and stderr.
func (r pushRig) push() (int, string) {
	r.t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "push"}, &out, &errb)
	return code, out.String() + errb.String()
}

// pushed is the message of the commit the remote branch names, read from the GitHub stand-in.
func (r pushRig) pushed() string {
	r.t.Helper()
	output, err := exec.Command("git", "--git-dir="+r.gh, "log", "-1", "--format=%B", "legion/LEGION-7").CombinedOutput()
	if err != nil {
		r.t.Fatalf("read the pushed branch: %v\n%s", err, output)
	}
	return strings.TrimRight(string(output), "\n")
}

// A push skips GitHub's CI only when it changes nothing but handoffs whose phase guarantees a later
// push: the planner's plan, the tester's test, and a reviewer's request for changes. Every other push
// may leave the head a human merges, so it runs in full: code, the implementer's handoff alone, a
// reviewer round that approves or names no verdict, and the .legion/ deletion. The trailer a
// skipped push left on its commit never rides along on a later push of that commit carrying more.
func TestPushSkipsCIOnlyForHandoffsALaterPushFollows(t *testing.T) {
	r := newPushRig(t)
	skipped := func(message string) bool { return strings.HasSuffix(message, "\n\n\nskip-checks: true") }
	// Every pushed message keeps the pane's attribution trailer and carries the skip-checks line at
	// most once, as its last line.
	for _, step := range []struct {
		name    string
		files   map[string]string
		message string
		skip    bool
	}{
		{"the planner's first push", map[string]string{".legion/plan.json": "{}\n"}, "plan: record handoff", true},
		{"the implementer's code and handoff", map[string]string{"widget.txt": "one\n", ".legion/implement.json": "{}\n"}, "implement: record handoff", false},
		{"the tester's handoff", map[string]string{".legion/test.json": "{}\n"}, "test: record handoff", true},
		{"a reviewer's request for changes", map[string]string{".legion/review.json": `{"verdict":"changes_requested"}` + "\n"}, "review: record handoff", true},
		{"a code push onto a skipped head", map[string]string{"widget.txt": "two\n"}, "widget: fix the line", false},
		{"the tester's second handoff", map[string]string{".legion/test.json": `{"round":2}` + "\n"}, "test: record handoff", true},
		{"a reviewer's approval", map[string]string{".legion/review.json": `{"verdict":"approved"}` + "\n"}, "review: record handoff", false},
		{"a reviewer round with no verdict", map[string]string{".legion/review.json": `{"round":3}` + "\n"}, "review: record handoff", false},
		{"the implementer's handoff alone", map[string]string{".legion/implement.json": `{"round":3}` + "\n"}, "implement: record handoff", false},
		{"code under a message that says skip-checks", map[string]string{".legion/review.json": `{"verdict":"changes_requested","round":4}` + "\n", "widget.txt": "three\n"}, "review: record handoff\n\n\nskip-checks: true", false},
		{"a push deleting only the tester's handoff", map[string]string{".legion/test.json": ""}, "drop the test handoff", false},
		{"the .legion/ deletion", map[string]string{".legion/plan.json": "", ".legion/implement.json": "", ".legion/review.json": ""}, "delete .legion/", false},
	} {
		r.commit(step.message, step.files)
		code, output := r.push()
		pushed := r.pushed()
		if code != 0 || skipped(pushed) != step.skip || strings.Count(pushed, "skip-checks") != map[bool]int{true: 1}[step.skip] ||
			!strings.Contains(pushed, "Omp-Session: ses-pane") {
			t.Fatalf("%s: legion push = %d %q, pushed %q; want skip-checks %t", step.name, code, output, pushed, step.skip)
		}
	}
}

// Another role's push moves the shared remote branch at once; a chain that does not descend from
// it is refused, and nothing moves on GitHub.
func TestPushRefusesAChainTheRemoteBranchIsAheadOf(t *testing.T) {
	r := newPushRig(t)
	r.commit("widget: add the line", map[string]string{"widget.txt": "one\n"})
	if code, output := r.push(); code != 0 {
		t.Fatalf("first push = %d %q", code, output)
	}
	fork := r.run("log", "--no-graph", "-T", "commit_id", "-r", "@-")
	r.commit("implement: record handoff", map[string]string{".legion/implement.json": "{}\n"})
	if code, output := r.push(); code != 0 {
		t.Fatalf("the other role's push = %d %q", code, output)
	}
	theirs := r.pushed()
	r.run("new", fork)
	r.commit("test: record handoff", map[string]string{".legion/test.json": "{}\n"})
	code, output := r.push()
	if code != 1 || !strings.Contains(output, "which @- does not descend from") || r.pushed() != theirs {
		t.Fatalf("a push behind the remote = %d %q, remote %q; want a refusal leaving %q", code, output, r.pushed(), theirs)
	}
}

// A rewrite of pushed commits (a conflict-forced rebase, a squash into a pushed commit) is pushed
// over the tip recorded before it. GitHub lists a forced push's commits since the merge base, so
// the daemon carries no verdict across it: it runs in full, even when its head is a handoff a later
// push follows, and the recorded tip is removed.
func TestPushRunsARewriteInFull(t *testing.T) {
	r := newPushRig(t)
	r.commit("test: record handoff", map[string]string{".legion/test.json": "{}\n"})
	if code, output := r.push(); code != 0 || !strings.HasSuffix(r.pushed(), "skip-checks: true") {
		t.Fatalf("the tester's push = %d %q, pushed %q; want it skipped", code, output, r.pushed())
	}
	tipFile := filepath.Join(os.Getenv("TMPDIR"), "legion-LEGION-7-tester-rewritten-tip")
	tip := r.run("log", "--no-graph", "-T", "commit_id", "-r", `remote_bookmarks(exact:"legion/LEGION-7", exact:"origin")`)
	if err := os.WriteFile(tipFile, []byte(tip), 0o600); err != nil {
		t.Fatal(err)
	}
	// The rewritten chain replaces the pushed handoff commit with another on its parent.
	r.run("new", "@--")
	r.commit("test: record handoff again\n\n\nskip-checks: true", map[string]string{".legion/test.json": `{"rewritten":true}` + "\n"})
	code, output := r.push()
	if code != 0 || r.pushed() != "test: record handoff again\n\nOmp-Session: ses-pane" {
		t.Fatalf("the rewrite's push = %d %q, pushed %q; want it run in full", code, output, r.pushed())
	}
	if _, err := os.Stat(tipFile); !os.IsNotExist(err) {
		t.Fatalf("the recorded tip is still there after the push: %v", err)
	}
}

// The daemon classifies a push from the union of its commits' added/removed/modified paths (the
// listener's changed_paths, packages/envoy/internal/contracts/normalize.go), not from the net tree
// diff, so a push whose commits touch code and then undo it is code-changing to the daemon. Were
// the skip decision the net diff, that head would skip CI and `classify.ClassifyPush` would refuse
// to carry a verdict to it, leaving it with none: a fix attempt counted against a push that changed
// no code, and a red that does not send the tree back. It runs in full instead.
func TestPushRunsAPushWhoseCommitsTouchCodeInFull(t *testing.T) {
	r := newPushRig(t)
	r.commit("widget: add the line", map[string]string{"widget.txt": "one\n"})
	if code, output := r.push(); code != 0 {
		t.Fatalf("the code push = %d %q", code, output)
	}
	tip := r.run("log", "--no-graph", "-T", "commit_id", "-r", `remote_bookmarks(exact:"legion/LEGION-7", exact:"origin")`)
	r.commit("scratch: add a file", map[string]string{"scratch.txt": "scratch\n"})
	r.commit("scratch: drop it again", map[string]string{"scratch.txt": ""})
	r.commit("test: record handoff", map[string]string{".legion/test.json": "{}\n"})
	// The premise: the net tree diff of the whole push is the tester's handoff alone, so a skip
	// decision taken from it skips.
	if net := r.run("diff", "--name-only", "--from", tip, "--to", "@-"); net != ".legion/test.json" {
		t.Fatalf("the push's net diff is %q; want the tester's handoff alone", net)
	}
	code, output := r.push()
	pushed := r.pushed()
	if code != 0 || strings.Contains(pushed, "skip-checks") || !strings.Contains(pushed, "Omp-Session: ses-pane") {
		t.Fatalf("the push = %d %q, pushed %q; want it run in full", code, output, pushed)
	}
}
