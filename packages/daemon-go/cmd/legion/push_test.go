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

// A push is skipped by GitHub's CI only when every path it changes is under .legion/ and it leaves
// .legion/ in the tree: a handoff push. The push that carries code, the .legion/ deletion that is
// the merge head, and a push onto a branch whose last push was a handoff all run in full, and the
// trailer a handoff push left on its commit never rides along on a later push of that commit
// carrying more.
func TestPushMarksOnlyAHandoffOnlyPushSkipChecks(t *testing.T) {
	r := newPushRig(t)
	skipped := func(message string) bool { return strings.HasSuffix(message, "\n\n\nskip-checks: true") }

	r.commit("plan: record handoff", map[string]string{".legion/plan.json": "{}\n"})
	if code, output := r.push(); code != 0 || !skipped(r.pushed()) {
		t.Fatalf("the planner's first push = %d %q, pushed %q; want the new branch's handoff-only head skip-checks", code, output, r.pushed())
	}

	r.commit("widget: add the line", map[string]string{"widget.txt": "one\n"})
	r.commit("implement: record handoff", map[string]string{".legion/implement.json": "{}\n"})
	if code, output := r.push(); code != 0 || skipped(r.pushed()) {
		t.Fatalf("the implementer's code and handoff push = %d %q, pushed %q; want it to run CI", code, output, r.pushed())
	}

	r.commit("test: record handoff", map[string]string{".legion/test.json": "{}\n"})
	if code, output := r.push(); code != 0 || !skipped(r.pushed()) || !strings.HasPrefix(r.pushed(), "test: record handoff") {
		t.Fatalf("the tester's handoff push = %d %q, pushed %q; want its handoff commit marked skip-checks", code, output, r.pushed())
	}

	r.commit("widget: fix the line", map[string]string{"widget.txt": "two\n"})
	if code, output := r.push(); code != 0 || skipped(r.pushed()) {
		t.Fatalf("a code push onto the handoff head = %d %q, pushed %q; want it to run CI", code, output, r.pushed())
	}

	r.commit("review: record handoff\n\n\nskip-checks: true", map[string]string{".legion/review.json": "{}\n", "widget.txt": "three\n"})
	if code, output := r.push(); code != 0 || skipped(r.pushed()) || r.pushed() != "review: record handoff" {
		t.Fatalf("a push carrying code on a commit that says skip-checks = %d %q, pushed %q; want the trailer removed", code, output, r.pushed())
	}

	r.commit("delete .legion/", map[string]string{".legion/plan.json": "", ".legion/implement.json": "", ".legion/test.json": "", ".legion/review.json": ""})
	if code, output := r.push(); code != 0 || skipped(r.pushed()) {
		t.Fatalf("the .legion/ deletion push = %d %q, pushed %q; want the merge head to run CI", code, output, r.pushed())
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
