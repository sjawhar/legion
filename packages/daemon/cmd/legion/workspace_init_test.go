package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// winitWait bounds every wait a workspace-init test makes on another process, so a hung init
// container fails the test instead of hanging it.
const winitWait = 30 * time.Second

const winitRepo = "acme/widgets"

// winitHelper is the credential helper every provisioned clone gets; a test function's local
// `workspace` path shadows the package where the clone is checked.
const winitHelper = workspace.GitHubCredentialHelper

// fakeJJ is the jj first on the issue volume's PATH. It records every invocation as one line,
// "<WINIT_TAG> <argv>" without the runner's leading --config pin, and runs the real jj with that
// pin — with WINIT_HOLD set, a clone of github.com/acme/widgets (which reaches the pod's feed)
// opens the directory's `release` fifo, writes its pid to its `held` fifo, waits on `release`
// until the test closes its end there, and exits without cloning. A fresh provisioning's first
// command is that clone, so a process held there is mid-provisioning.
const fakeJJ = `#!/bin/sh
pin=
case "$1" in --config=*) pin=$1; shift ;; esac
printf '%s %s\n' "$WINIT_TAG" "$*" >> "$WINIT_JJ_LOG"
if [ -n "$WINIT_HOLD" ] && [ "$1 $2 $3" = "git clone https://github.com/acme/widgets" ]; then
	exec 3< "$WINIT_HOLD/release"
	printf '%s' "$$" > "$WINIT_HOLD/held"
	read _ <&3
	exit 1
fi
PATH="${PATH#*:}" exec "$WINIT_REAL_JJ" ${pin:+"$pin"} "$@"
`

// fakeCodegraph is a recording `codegraph` stub first on the issue volume's PATH: it never does
// anything but log its own argv, so workspace-init calling it at all is the regression this
// guards, caught at once rather than by a timeout. Every real codegraph invocation runs with
// codegraphEnvironment()'s minimal environment (PATH/HOME/TMPDIR/DO_NOT_TRACK only), so the log
// path can't come from the test's own environment the way the jj/git stubs' do: it is derived
// from the stub's own invocation path ($0 is "<dir>/bin/codegraph"; stripping "/bin/codegraph"
// leaves "<dir>", matching v.codegraphLog).
const fakeCodegraph = `#!/bin/sh
printf '%s\n' "$*" >> "${0%/bin/codegraph}/codegraph.log"
`

// issueVolume is one issue's volume and what a pod's two init containers run against it: a PATH
// whose git clones a local bare remote in place of github.com/acme/widgets, the provisioning token
// file `fetch` is pointed at and `provision` never is, the pod's feed, a TMPDIR standing in for the
// fetching container's own filesystem, and, as in a pod, a jj config home that starts empty and no
// user configuration. defaultBranch is the remote's default branch, which `provision` is told.
type issueVolume struct {
	root, token, feed, jjLog, credentialLog, codegraphLog, realJJ, tmp, bin string
	defaultBranch                                                           string
	env                                                                     map[string]string
}

func newIssueVolume(t *testing.T) *issueVolume {
	t.Helper()
	realJJ, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("workspace-init's tests drive a real jj: %v", err)
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("workspace-init's tests drive a real git: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	tmp := filepath.Join(dir, "tmp")
	for _, made := range []string{bin, tmp} {
		if err := os.Mkdir(made, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, script := range map[string]string{"jj": fakeJJ, "git": fakeGit, "codegraph": fakeCodegraph} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	token := filepath.Join(dir, "provision-token")
	if err := os.WriteFile(token, []byte("ghs_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "legion")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	v := &issueVolume{
		root: root, token: token, feed: filepath.Join(dir, "feed"), jjLog: filepath.Join(dir, "jj.log"),
		credentialLog: filepath.Join(dir, "credential.log"), codegraphLog: filepath.Join(dir, "codegraph.log"),
		realJJ: realJJ, tmp: tmp, bin: bin, defaultBranch: "main",
	}
	v.env = map[string]string{
		"PATH":                 bin + string(filepath.ListSeparator) + os.Getenv("PATH"),
		"TMPDIR":               tmp,
		"WINIT_REAL_JJ":        realJJ,
		"WINIT_REAL_GIT":       realGit,
		"WINIT_REMOTE":         filepath.Join(dir, "no-remote.git"),
		"WINIT_JJ_LOG":         v.jjLog,
		"WINIT_CREDENTIAL_LOG": v.credentialLog,
		"WINIT_CLONE_FAILURES": "",
		"WINIT_TAG":            "",
		"WINIT_HOLD":           "",
		"JJ_USER":              "Legion test",
		"JJ_EMAIL":             "legion-test@example.invalid",
		"XDG_CONFIG_HOME":      filepath.Join(dir, "config"),
		"JJ_CONFIG":            filepath.Join(dir, "no-user-config.toml"),
	}
	return v
}

// withRemote gives the volume's git a real repository to clone: a bare remote whose default branch,
// defaultBranch, holds one commit, as github.com/acme/widgets would.
func (v *issueVolume) withRemote(t *testing.T, defaultBranch string) *issueVolume {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	seed := filepath.Join(dir, "seed")
	for _, argv := range [][]string{
		{"git", "init", "--bare", "--initial-branch=" + defaultBranch, remote},
		{v.realJJ, "git", "init", "--colocate", seed},
		{v.realJJ, "describe", "-m", "seed", "-R", seed},
		{v.realJJ, "bookmark", "set", defaultBranch, "-r", "@", "-R", seed},
		{v.realJJ, "git", "remote", "add", "origin", remote, "-R", seed},
		{v.realJJ, "git", "push", "--remote", "origin", "--bookmark", defaultBranch, "-R", seed},
	} {
		setup := exec.Command(argv[0], argv[1:]...)
		setup.Dir = dir
		setup.Env = append(os.Environ(), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")
		if output, err := setup.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, output)
		}
	}
	v.env["WINIT_REMOTE"] = remote
	v.defaultBranch = defaultBranch
	return v
}

func (v *issueVolume) clone() string {
	return filepath.Join(v.root, "repos", "github.com", "acme", "widgets")
}

func (v *issueVolume) workspace(issue string) string {
	return filepath.Join(v.root, "workspaces", "acme", "widgets", strings.ToLower(issue))
}

// fetchArgs are the first init container's; args are the second's, for issue.
func (v *issueVolume) fetchArgs() []string {
	return []string{"fetch", "--repo", winitRepo, "--feed", v.feed}
}

func (v *issueVolume) args(issue string) []string {
	return []string{"provision", "--issue", issue, "--repo", winitRepo, "--root", v.root, "--credential-helper", winitHelper, "--feed", v.feed, "--default-branch", v.defaultBranch}
}

// runtimeOptionalEnv are the variables a runtime sets on an init container only for some
// launches; no run in these tests inherits them from whoever runs the tests.
var runtimeOptionalEnv = []string{"LEGION_PROVISION_TOKEN_FILE", "LEGION_EXPECT_ISSUE_VOLUME", "LEGION_WORKSPACE_RECOVERED_FROM"}

// setenv is the volume's environment for an in-process run of `provision`.
func (v *issueVolume) setenv(t *testing.T) {
	t.Helper()
	for _, name := range runtimeOptionalEnv {
		unsetenv(t, name)
	}
	for name, value := range v.env {
		t.Setenv(name, value)
	}
}

// fetch runs the pod's first init container in-process, pointed at the provisioning token, and
// fills the feed; the environment is `provision`'s again once it returns.
func (v *issueVolume) fetch(t *testing.T) {
	t.Helper()
	v.setenv(t)
	t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)
	code, _, stderr := runWorkspaceInitHere(v.fetchArgs())
	unsetenv(t, "LEGION_PROVISION_TOKEN_FILE")
	if code != 0 {
		t.Fatalf("workspace-init fetch: exit %d, stderr %q", code, stderr)
	}
}

func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func runWorkspaceInitHere(args []string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	code = run(context.Background(), append([]string{"legion", "workspace-init"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

// logLines is every invocation one of the volume's PATH stubs recorded to path, one call per
// line; nil when the stub was never run.
func logLines(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// jjCalls is every jj invocation the volume's PATH saw, "<tag> <argv>" each.
func (v *issueVolume) jjCalls(t *testing.T) []string { return logLines(t, v.jjLog) }

// codegraphCalls is every codegraph invocation the volume's PATH saw, one argv per line; nil
// when the stub was never run.
func (v *issueVolume) codegraphCalls(t *testing.T) []string { return logLines(t, v.codegraphLog) }

func (v *issueVolume) jj(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command(v.realJJ, args...)
	command.Env = append(os.Environ(), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("jj %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func (v *issueVolume) git(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command(v.env["WINIT_REAL_GIT"], args...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

// Every refusal `provision` makes happens before anything touches the volume or runs a tool
// (cmdWorkspaceInit's flag and token checks, workspace-init.ts): an init container refused on its
// input leaves the issue volume as it found it. It refuses to run pointed at the provisioning
// token, which `fetch` alone holds; and the command refuses no subcommand, or another.
func TestWorkspaceInitRefusesBeforeTouchingTheVolume(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(v *issueVolume) []string
		env  func(t *testing.T, v *issueVolume)
		code int
		says func(v *issueVolume) string
	}{
		{
			name: "no subcommand",
			args: func(*issueVolume) []string { return nil },
			code: 2,
			says: func(*issueVolume) string { return subcommandUsage("workspace-init", workspaceInitCommands) },
		},
		{
			name: "an unknown subcommand",
			args: func(v *issueVolume) []string { return append([]string{"init"}, v.args("LEGION-42")[1:]...) },
			code: 2,
			says: func(*issueVolume) string { return `unknown subcommand "init"` },
		},
		{
			name: "the one-container invocation, with no subcommand",
			args: func(v *issueVolume) []string { return v.args("LEGION-42")[1:] },
			code: 2,
			says: func(*issueVolume) string { return `unknown subcommand "--issue"` },
		},
		{
			name: "no --issue",
			args: func(v *issueVolume) []string { return append([]string{"provision"}, v.args("LEGION-42")[3:]...) },
			code: 1,
			says: func(*issueVolume) string { return `--issue must be a Dispatch issue key like LEGION-1 (got "")` },
		},
		{
			name: "an --issue that is not an issue key",
			args: func(v *issueVolume) []string { return v.args("nope") },
			code: 1,
			says: func(*issueVolume) string { return `--issue must be a Dispatch issue key like LEGION-1 (got "nope")` },
		},
		{
			name: "a --repo that is not owner/name",
			args: func(v *issueVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", "acme", "--root", v.root, "--credential-helper", "x", "--feed", v.feed}
			},
			code: 1,
			says: func(*issueVolume) string { return `--repo must be "owner/name" (got "acme")` },
		},
		{
			name: "a --repo with a .. segment",
			args: func(v *issueVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", "../x", "--root", v.root, "--credential-helper", "x", "--feed", v.feed}
			},
			code: 1,
			says: func(*issueVolume) string { return `--repo "../x" has a ".." segment` },
		},
		{
			name: "a --repo holding whitespace",
			args: func(v *issueVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", "acme/wid gets", "--root", v.root, "--credential-helper", "x", "--feed", v.feed}
			},
			code: 1,
			says: func(*issueVolume) string { return `--repo "acme/wid gets" holds whitespace` },
		},
		{
			// --repo is read first: every other input here is wrong too.
			name: "a bad --repo, before every other input",
			args: func(*issueVolume) []string {
				return []string{"provision", "--issue", "nope", "--repo", "../..", "--root", "legion-root", "--feed", "feed"}
			},
			env:  func(t *testing.T, v *issueVolume) { t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token) },
			code: 1,
			says: func(*issueVolume) string { return `--repo "../.." has a ".." segment` },
		},
		{
			name: "no --credential-helper",
			args: func(v *issueVolume) []string { return v.args("LEGION-42")[:7] },
			code: 1,
			says: func(*issueVolume) string { return "--credential-helper is required" },
		},
		{
			name: "a relative --root",
			args: func(v *issueVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", winitRepo, "--root", "legion-root", "--credential-helper", "x", "--feed", v.feed}
			},
			code: 1,
			says: func(*issueVolume) string { return `--root must be an absolute path (got "legion-root")` },
		},
		{
			name: "no --feed",
			args: func(v *issueVolume) []string { return v.args("LEGION-42")[:9] },
			code: 1,
			says: func(*issueVolume) string { return `--feed must be an absolute path (got "")` },
		},
		{
			name: "no --default-branch",
			args: func(v *issueVolume) []string { return v.args("LEGION-42")[:11] },
			code: 1,
			says: func(*issueVolume) string { return "--default-branch is required" },
		},
		{
			name: "pointed at the provisioning token",
			env:  func(t *testing.T, v *issueVolume) { t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token) },
			code: 1,
			says: func(*issueVolume) string {
				return "LEGION_PROVISION_TOKEN_FILE is set: provisioning runs without the provisioning token, which `workspace-init fetch` alone holds"
			},
		},
		{
			name: "no jj on PATH",
			env: func(t *testing.T, _ *issueVolume) {
				git, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin)
			},
			code: 1,
			says: func(*issueVolume) string { return "jj is not on PATH" },
		},
		{
			name: "an unknown flag",
			args: func(v *issueVolume) []string { return append(v.args("LEGION-42"), "--bogus") },
			code: 2,
			says: func(*issueVolume) string { return "-bogus" },
		},
		{
			name: "a positional argument",
			args: func(v *issueVolume) []string { return append(v.args("LEGION-42"), "extra") },
			code: 2,
			says: func(*issueVolume) string { return `unexpected argument "extra"` },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newIssueVolume(t)
			v.setenv(t)
			if tc.env != nil {
				tc.env(t, v)
			}
			args := v.args("LEGION-42")
			if tc.args != nil {
				args = tc.args(v)
			}
			code, stdout, stderr := runWorkspaceInitHere(args)
			if code != tc.code || !strings.Contains(stderr, tc.says(v)) {
				t.Fatalf("exit %d, stderr %q; want %d naming %q", code, stderr, tc.code, tc.says(v))
			}
			if stdout != "" {
				t.Fatalf("stdout %q, want nothing", stdout)
			}
			if entries, err := os.ReadDir(v.root); err != nil || len(entries) != 0 {
				t.Fatalf("the issue volume holds %v (%v), want it untouched", entries, err)
			}
			if _, err := os.Stat("legion-root"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a relative --root was created: %v", err)
			}
			if calls := v.jjCalls(t); calls != nil {
				t.Fatalf("jj ran %q before the refusal", calls)
			}
		})
	}
}

// A fresh issue volume, provisioned from the feed `fetch` filled: the issue's clone, its origin
// still GitHub's, and the issue's jj workspace on its bookmark, the clone's credential helper the
// one named, no gh shim and no tmux pane's `legion` launcher on the volume (a pod's PATH names the
// image's gh and legion, and its credential is the gh files its role Secret projects), the
// sessions directory the main container mounts, one log line naming the workspace. The permanent
// guard on the pod path: a recording `codegraph` stub is first on PATH throughout, and this
// container must never call it (#1647) — only a synchronous warm-up regressing back onto the init
// container's launch path would.
func TestWorkspaceInitProvisionsTheIssueWorkspace(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	v.fetch(t)

	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	workspace := v.workspace("LEGION-42")
	if want := "workspace-init: " + workspace + " on legion/LEGION-42 from main\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	if listed := v.jj(t, "workspace", "list", "-R", v.clone()); !strings.Contains(listed, "legion-42:") {
		t.Fatalf("the clone's workspaces are %q, want legion-42 among them", listed)
	}
	if parent := v.jj(t, "log", "-r", "@-", "--no-graph", "-T", "description.first_line()", "--ignore-working-copy", "-R", workspace); parent != "seed" {
		t.Fatalf("the workspace sits on %q, want the remote's main", parent)
	}
	if origin := v.git(t, "--git-dir="+filepath.Join(v.clone(), ".git"), "remote", "get-url", "origin"); origin != "https://github.com/acme/widgets" {
		t.Fatalf("the clone's origin is %q, want GitHub's", origin)
	}
	helpers, err := exec.Command("git", "--git-dir="+filepath.Join(v.clone(), ".git"), "config", "--get-all", "credential.helper").Output()
	if err != nil || !strings.HasSuffix(string(helpers), "\n"+winitHelper+"\n") {
		t.Fatalf("the clone's credential helpers are %q (%v), want the named helper last", helpers, err)
	}

	for _, dir := range []string{"worker-bin", "bin", "gh"} {
		if _, err := os.Stat(filepath.Join(v.root, dir)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the issue volume holds %s (%v), want no shim, launcher or gh directory of the daemon's", dir, err)
		}
	}
	if info, err := os.Stat(filepath.Join(v.root, "sessions")); err != nil || !info.IsDir() {
		t.Fatalf("sessions: %v (%v), want a directory", info, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".legion", "LEGION-42", "workspace-recovered.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a recovery marker without LEGION_WORKSPACE_RECOVERED_FROM: %v", err)
	}
	if calls := v.codegraphCalls(t); calls != nil {
		t.Fatalf("workspace-init called codegraph %q, want it to build no index on the pod's registration path", calls)
	}
	holdsNoToken(t, v.root, "after provisioning")
}

// A repository whose default branch is not main, and holds a slash, provisions at that branch: the
// init log names it, the workspace's parent is the remote's head of it, and the issue's bookmark
// is the fresh working copy's.
func TestWorkspaceInitProvisionsAtADefaultBranchWithASlash(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "release/v7.1")
	v.fetch(t)

	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	workspace := v.workspace("LEGION-42")
	if want := "workspace-init: " + workspace + " on legion/LEGION-42 from release/v7.1\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	head := v.git(t, "--git-dir="+v.env["WINIT_REMOTE"], "rev-parse", "refs/heads/release/v7.1")
	if parent := v.jj(t, "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", workspace); parent != head {
		t.Fatalf("the workspace's @- is %s, want release/v7.1's %s", parent, head)
	}
	if bookmark, at := v.jj(t, "log", "-r", "legion/LEGION-42", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", v.clone()),
		v.jj(t, "log", "-r", "@", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", workspace); bookmark != at {
		t.Fatalf("legion/LEGION-42 is on %s, want the workspace's working copy %s", bookmark, at)
	}
}

// staleLock leaves a git lock file at path, last written age ago, as a git or jj process killed or
// interrupted mid-write leaves one beside the file it was writing.
func staleLock(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// removedLine matches provision's log line for the stale lock it removed at path, last written
// about age ago (whole minutes; the seconds are the test's own run time).
func removedLine(path string, age time.Duration) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^workspace-init: removed the stale git lock ` + regexp.QuoteMeta(path) +
		`, last written ` + regexp.QuoteMeta(strings.TrimSuffix(age.String(), "0s")) + `\d+s ago`)
}

// provisioned runs provision for issue on the volume and fails the test unless it succeeds.
func (v *issueVolume) provisioned(t *testing.T, issue string) string {
	t.Helper()
	code, stdout, stderr := runWorkspaceInitHere(v.args(issue))
	if code != 0 {
		t.Fatalf("provision %s: exit %d, stdout %q, stderr %q", issue, code, stdout, stderr)
	}
	return stdout
}

// A git or jj process interrupted mid-write leaves its lock beside the file it was writing, and
// every later git or jj write to that file fails on it until someone deletes it (LEGION-731: a
// worktree's index.lock failed every provisioning of an issue's pods with "Could not acquire lock
// for index file"). provision runs before any role container of the issue's pod starts, on a
// volume no other pod mounts, so nothing can hold a lock there: it removes the workspace's stale
// index.lock, logging its path and age, and the update-stale that failed on it succeeds.
func TestWorkspaceInitRemovesTheWorkspacesStaleIndexLock(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	v.fetch(t)
	v.provisioned(t, "LEGION-42")
	// The working copy is rebased from outside the workspace onto a commit carrying a file, so the
	// next provisioning's update-stale checks out a new tree, which rewrites the worktree's index.
	if err := os.WriteFile(filepath.Join(v.clone(), "added.txt"), []byte("added\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.jj(t, "describe", "-m", "adds a file", "-R", v.clone())
	v.jj(t, "rebase", "-r", "legion-42@", "-d", "@", "--ignore-working-copy", "-R", v.clone())
	lock := filepath.Join(v.clone(), ".git", "worktrees", "legion-42", "index.lock")
	staleLock(t, lock, 31*time.Minute)

	stdout := v.provisioned(t, "LEGION-42")
	if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s after provisioning: %v, want it removed", lock, err)
	}
	if want := removedLine(lock, 31*time.Minute); !want.MatchString(stdout) {
		t.Fatalf("stdout %q, want a line matching %q", stdout, want)
	}
	if _, err := os.Stat(filepath.Join(v.workspace("LEGION-42"), "added.txt")); err != nil {
		t.Fatalf("the workspace after provisioning: %v, want the rebased tree update-stale checks out", err)
	}
}

// provision removes the issue's own locks and no other: another workspace's git worktree entry in
// the clone — an issue's that shared it, or a worktree an agent added — keeps its lock as it was,
// however old.
func TestWorkspaceInitLeavesAnotherWorkspacesLockAlone(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	v.fetch(t)
	v.provisioned(t, "LEGION-42")
	v.provisioned(t, "LEGION-43")
	other := filepath.Join(v.clone(), ".git", "worktrees", "legion-43", "index.lock")
	staleLock(t, other, 31*time.Minute)
	before, err := os.Lstat(other)
	if err != nil {
		t.Fatal(err)
	}

	stdout := v.provisioned(t, "LEGION-42")
	after, err := os.Lstat(other)
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("LEGION-43's %s after LEGION-42's provisioning: %v (%v), want it left as it was", other, after, err)
	}
	if strings.Contains(stdout, other) {
		t.Fatalf("stdout %q names LEGION-43's lock", stdout)
	}
}

// The clone's own git directory holds locks beside what git and jj write there — its default
// workspace's index, packed-refs, a ref — and the clone is as much the issue's alone as its
// workspace: one per issue volume, which no other pod mounts. provision removes each stale one,
// logging it, and provisions.
func TestWorkspaceInitRemovesTheClonesStaleLocks(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	v.fetch(t)
	v.provisioned(t, "LEGION-42")
	git := filepath.Join(v.clone(), ".git")
	locks := []string{
		filepath.Join(git, "index.lock"),
		filepath.Join(git, "packed-refs.lock"),
		filepath.Join(git, "refs", "heads", "legion", "LEGION-42.lock"),
	}
	for _, lock := range locks {
		staleLock(t, lock, 2*time.Hour)
	}

	stdout := v.provisioned(t, "LEGION-42")
	for _, lock := range locks {
		if _, err := os.Lstat(lock); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s after provisioning: %v, want it removed", lock, err)
		}
		if want := removedLine(lock, 2*time.Hour); !want.MatchString(stdout) {
			t.Errorf("stdout %q, want a line matching %q", stdout, want)
		}
	}
}

// A stale lock provision cannot remove fails it, naming the file, before any git or jj runs on
// the clone: it would fail the provisioning anyway, on a message that names the lock alone.
func TestWorkspaceInitFailsOnALockItCannotRemove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root removes a file from a directory it cannot write")
	}
	v := newIssueVolume(t).withRemote(t, "main")
	v.fetch(t)
	v.provisioned(t, "LEGION-42")
	entry := filepath.Join(v.clone(), ".git", "worktrees", "legion-42")
	lock := filepath.Join(entry, "index.lock")
	staleLock(t, lock, 31*time.Minute)
	if err := os.Chmod(entry, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(entry, 0o755) })
	calls := len(v.jjCalls(t))

	code, _, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
	if code != 1 || !strings.Contains(stderr, "remove the stale git lock "+lock) {
		t.Fatalf("exit %d, stderr %q, want exit 1 naming %s", code, stderr, lock)
	}
	if ran := v.jjCalls(t)[calls:]; len(ran) != 0 {
		t.Fatalf("jj ran %q after the lock could not be removed, want nothing run", ran)
	}
}

// The clone's .git and its worktrees directory are real directories, as git creates them, and both
// are on the volume every agent of the issue writes. Either one replaced with a symlink would take
// the sweep, and the worktree writes after it, to whatever directory it names: there an entry
// whose gitdir names the issue's workspace counts as the workspace's own. provision refuses it,
// naming the symlink, before it removes anything or runs jj.
func TestWorkspaceInitRefusesASymlinkedGitDirectory(t *testing.T) {
	for _, linked := range []string{".git", filepath.Join(".git", "worktrees")} {
		t.Run(linked, func(t *testing.T) {
			v := newIssueVolume(t).withRemote(t, "main")
			v.fetch(t)
			v.provisioned(t, "LEGION-42")
			path := filepath.Join(v.clone(), linked)
			elsewhere := filepath.Join(t.TempDir(), "elsewhere")
			if err := os.Rename(path, elsewhere); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, path); err != nil {
				t.Fatal(err)
			}
			evil := filepath.Join(v.clone(), ".git", "worktrees", "evil")
			if err := os.MkdirAll(evil, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(evil, "gitdir"), []byte(filepath.Join(v.workspace("LEGION-42"), ".git")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			victim := filepath.Join(evil, "victim.lock")
			staleLock(t, victim, 31*time.Minute)
			calls := len(v.jjCalls(t))

			code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
			if want := "refusing the git worktrees of " + v.clone() + ": " + path + " is a symlink"; code != 1 || !strings.Contains(stderr, want) {
				t.Fatalf("exit %d, stdout %q, stderr %q, want exit 1 with %q", code, stdout, stderr, want)
			}
			if _, err := os.Lstat(victim); err != nil {
				t.Fatalf("%s after the refusal: %v, want it left where the symlink points", victim, err)
			}
			if ran := v.jjCalls(t)[calls:]; len(ran) != 0 {
				t.Fatalf("jj ran %q after the refusal, want nothing run", ran)
			}
		})
	}
}

// Shared init distinguishes loss of the issue's storage from one role's missing transcript.
// A clone or a retained session proves the volume still holds data; only the role launcher
// decides whether its own session exists.
func TestWorkspaceInitDetectsLostVolumeWithoutRoleSessionDependency(t *testing.T) {
	for _, content := range []string{"empty", "clone", "session"} {
		t.Run(content, func(t *testing.T) {
			v := newIssueVolume(t).withRemote(t, "main")
			v.fetch(t)
			t.Setenv("LEGION_EXPECT_ISSUE_VOLUME", "true")
			switch content {
			case "clone":
				if err := os.MkdirAll(v.clone(), 0o700); err != nil {
					t.Fatal(err)
				}
			case "session":
				dir := filepath.Join(v.root, "sessions")
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "retained.jsonl"), []byte("{}\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			code, _, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
			if content == "empty" {
				if code != 3 {
					t.Fatalf("expected missing issue storage to exit 3, got %d: %s", code, stderr)
				}
				if entries, err := os.ReadDir(v.root); err != nil || len(entries) != 0 {
					t.Fatalf("lost storage touched the volume: %v (%v)", entries, err)
				}
				return
			}
			if code != 0 {
				t.Fatalf("shared init blocked a role-independent %s: exit %d: %s", content, code, stderr)
			}
			if _, err := os.Stat(v.workspace("LEGION-42")); err != nil {
				t.Fatalf("intact volume did not provision the issue workspace: %v", err)
			}
		})
	}
}

// The command side of workspace recovery (decision 11): a relaunch after a lost volume names the
// ref it recovers from, and the recreated workspace records it, with the commit it was recreated
// at, in .legion/<issue>/workspace-recovered.json (cmdWorkspaceInit's LEGION_WORKSPACE_RECOVERED_FROM
// branch, workspace-init.ts). The commit is read uncolored, so an operator's `ui.color = "always"`
// never wraps it in escape codes.
func TestWorkspaceInitRecordsTheRecoveryMarker(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	v.fetch(t)
	t.Setenv("LEGION_WORKSPACE_RECOVERED_FROM", "legion/LEGION-42")
	colored := filepath.Join(t.TempDir(), "color-always.toml")
	if err := os.WriteFile(colored, []byte("[ui]\ncolor = \"always\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JJ_CONFIG", colored)

	before := time.Now().UTC().Truncate(time.Millisecond)
	code, _, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
	after := time.Now().UTC()
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	workspace := v.workspace("LEGION-42")
	body, err := os.ReadFile(filepath.Join(workspace, ".legion", "LEGION-42", "workspace-recovered.json"))
	if err != nil {
		t.Fatalf("read the recovery marker: %v", err)
	}
	var marker map[string]string
	if err := json.Unmarshal(body, &marker); err != nil || len(marker) != 4 {
		t.Fatalf("the recovery marker is %s (%v), want exactly recoveredAt, fromRef, sha, reason", body, err)
	}
	if marker["fromRef"] != "legion/LEGION-42" || marker["reason"] != "volume-missing" {
		t.Fatalf("the recovery marker is %s", body)
	}
	if sha := v.jj(t, "log", "-r", "@", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "--color=never", "-R", workspace); marker["sha"] != sha {
		t.Fatalf("the recovery marker names commit %q, want the recreated working copy's %q", marker["sha"], sha)
	}
	at, err := time.Parse("2006-01-02T15:04:05.000Z", marker["recoveredAt"])
	if err != nil || at.Before(before) || at.After(after) {
		t.Fatalf("recoveredAt %q (%v), want an ISO instant in milliseconds, UTC, during the run", marker["recoveredAt"], err)
	}
}

// The recovery marker's jj log runs under the same checked runner as provisioning, so a jj that
// outlives the budget is reported as timed out, naming the command, never as an exit status.
func TestWorkspaceInitReportsATimedOutRecoveryMarkerCommand(t *testing.T) {
	jj := filepath.Join(t.TempDir(), "jj")
	if err := os.WriteFile(jj, []byte("#!/bin/sh\nexec sleep 5\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	run := workspace.NewRunner(100*time.Millisecond, map[string]string{"jj": jj, "git": git})
	clone, dir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(clone, ".jj", "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".jj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".jj", "repo"), []byte(filepath.Join(clone, ".jj", "repo")), 0o644); err != nil {
		t.Fatal(err)
	}
	err = writeRecoveryMarker(context.Background(), run, workspace.Workspace{Dir: dir, Clone: clone}, "LEGION-42", "legion/LEGION-42")
	if err == nil || !strings.Contains(err.Error(), "command timed out: jj log -r @ --no-graph -T commit_id") {
		t.Fatalf("writeRecoveryMarker = %v, want the timed-out jj log named", err)
	}
}

// A provisioning that fails — here, from a feed `fetch` never filled — is the command's failure,
// with the failed command named.
func TestWorkspaceInitReportsAFailedProvisioning(t *testing.T) {
	v := newIssueVolume(t)
	v.setenv(t)

	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
	if code != 1 || !strings.Contains(stderr, "jj git clone https://github.com/acme/widgets") || stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 1 naming the failed clone", code, stdout, stderr)
	}
}

// initProcess is one workspace-init running as its own process, as a pod's init container does.
type initProcess struct {
	cmd    *exec.Cmd
	lines  chan string
	stderr lockedBuffer
	waited bool
}

// lockedBuffer is a process's stderr, readable while the process still writes it.
type lockedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// start runs workspace-init for issue as a separate process whose jj invocations carry tag, in
// its own process group so a test can kill it with whatever it spawned.
func (v *issueVolume) start(t *testing.T, tag, issue string, env ...string) *initProcess {
	t.Helper()
	p := &initProcess{lines: make(chan string, 16)}
	p.cmd = exec.Command(os.Args[0], append([]string{"workspace-init"}, v.args(issue)...)...)
	overrides := maps.Clone(v.env)
	overrides[testMainEnv] = "1"
	overrides["WINIT_TAG"] = tag
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		overrides[name] = value
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[name]; !overridden && !slices.Contains(runtimeOptionalEnv, name) {
			p.cmd.Env = append(p.cmd.Env, entry)
		}
	}
	for name, value := range overrides {
		p.cmd.Env = append(p.cmd.Env, name+"="+value)
	}
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.cmd.Stderr = &p.stderr
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start workspace-init %s: %v", issue, err)
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			p.lines <- scanner.Text()
		}
		close(p.lines)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		if !p.waited {
			for range p.lines {
			}
			_ = p.cmd.Wait()
		}
	})
	return p
}

// wait is the process's exit status and the rest of its stdout, once it exits.
func (p *initProcess) wait(t *testing.T) (code int, rest []string) {
	t.Helper()
	deadline := time.After(winitWait)
	for done := false; !done; {
		select {
		case line, ok := <-p.lines:
			if !ok {
				done = true
				break
			}
			rest = append(rest, line)
		case <-deadline:
			t.Fatalf("workspace-init did not exit within %s", winitWait)
		}
	}
	p.waited = true
	err := p.cmd.Wait()
	var exited *exec.ExitError
	if err != nil && !errors.As(err, &exited) {
		t.Fatalf("wait for workspace-init: %v", err)
	}
	return p.cmd.ProcessState.ExitCode(), rest
}

// holder fills the feed, starts `provision` for LEGION-42, and returns once it is held inside its
// first command, the clone (fakeJJ's WINIT_HOLD), where it stays until the test ends. That jj
// leads a process group of its own (procgroup.Configure), which no kill of workspace-init reaches,
// so the test holds the one writing end of the jj's `release` fifo (O_RDWR opens a fifo without
// waiting for a reader) and the hold ends when that end closes: in holder's cleanup, which runs
// once start's has ended workspace-init and waits, where /proc shows it, until the jj has exited;
// or as the kernel closes it, when the test binary dies first.
func (v *issueVolume) holder(t *testing.T) *initProcess {
	t.Helper()
	v.fetch(t)
	hold := t.TempDir()
	for _, fifo := range []string{"held", "release"} {
		if err := syscall.Mkfifo(filepath.Join(hold, fifo), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	release, err := os.OpenFile(filepath.Join(hold, "release"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	jj := 0
	t.Cleanup(func() {
		_ = release.Close()
		if jj == 0 {
			return
		}
		script := filepath.Join(v.bin, "jj")
		testwait.Eventually(t, "the held jj to exit", func() bool {
			cmdline, err := os.ReadFile("/proc/" + strconv.Itoa(jj) + "/cmdline")
			return err != nil || !strings.Contains(string(cmdline), script)
		})
	})
	p := v.start(t, "first", "LEGION-42", "WINIT_HOLD="+hold)
	held := fifo(t, filepath.Join(hold, "held"), os.O_RDONLY, p)
	if jj, err = strconv.Atoi(held); err != nil {
		t.Fatalf("the held jj wrote %q to its held fifo, want its pid", held)
	}
	return p
}

// fifo opens one end of a fifo — which blocks until the holder opens the other — reads it to EOF
// when reading, and closes it, bounded by winitWait and by the holder's staying alive and silent.
func fifo(t *testing.T, path string, flag int, holder *initProcess) string {
	t.Helper()
	type opened struct {
		read []byte
		err  error
	}
	done := make(chan opened, 1)
	go func() {
		var result opened
		file, err := os.OpenFile(path, flag, 0)
		if err == nil {
			if flag == os.O_RDONLY {
				result.read, err = io.ReadAll(file)
			}
			_ = file.Close()
		}
		result.err = err
		done <- result
	}()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("%s: %v", path, result.err)
		}
		return string(result.read)
	case line, ok := <-holder.lines:
		t.Fatalf("the holder printed %q (open: %v) before reaching %s; stderr %q", line, ok, path, holder.stderr.String())
	case <-time.After(winitWait):
		t.Fatalf("nothing opened the other end of %s within %s", path, winitWait)
	}
	return ""
}

// The provisioning token is the implement App's installation token, and every container of an
// issue pod mounts the issue's volume under one uid: `provision`, the process that works on the
// volume, never holds it. Held mid-clone, it has written no one-shot credential to its TMPDIR, and
// no file on the volume holds the token — nor after it was killed there.
func TestWorkspaceInitProvisionHoldsNoToken(t *testing.T) {
	v := newIssueVolume(t).withRemote(t, "main")
	first := v.holder(t)
	if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
		t.Errorf("while provision's clone runs, TMPDIR holds %v (%v), want no credential", entries, err)
	}
	holdsNoToken(t, v.root, "while provision's clone runs")

	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	first.wait(t)
	holdsNoToken(t, v.root, "after provision was killed mid-clone")
}

// holdsNoToken fails when any file under dir contains the provisioning token.
func holdsNoToken(t *testing.T, dir, when string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(body, []byte("ghs_test")) {
			t.Errorf("%s, %s holds the provisioning token", when, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// A held provisioning's jj runs in a process group of its own (procgroup.Configure), which neither
// a test's kill of workspace-init nor start's kill of workspace-init's group reaches: the hold ends
// with the test that took it, or the fake jj, its fifos gone with the test's TempDir, waits for a
// release forever. Each scenario runs as a subtest, and once it has ended no process runs that
// volume's fake jj.
func TestWorkspaceInitHoldEndsWithItsTest(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skipf("no /proc to find a held jj in: %v", err)
	}
	for _, tc := range []struct {
		name string
		kill bool
	}{{"the test kills workspace-init", true}, {"start's cleanup kills workspace-init", false}} {
		var jj string
		t.Run(tc.name, func(t *testing.T) {
			v := newIssueVolume(t).withRemote(t, "main")
			jj = filepath.Join(v.bin, "jj")
			first := v.holder(t)
			if tc.kill {
				if err := first.cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				first.wait(t)
			}
		})
		if jj == "" {
			continue
		}
		if left := killProcessesRunning(t, jj); len(left) != 0 {
			t.Errorf("%s: after its subtest ended, its fake jj still runs: %s", tc.name, strings.Join(left, "; "))
		}
	}
}

// killProcessesRunning is every process whose command line names path, as "pid <n>: <argv>", each
// killed once found so a failing run leaves none behind.
func killProcessesRunning(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		argv := strings.ReplaceAll(strings.TrimRight(string(cmdline), "\x00"), "\x00", " ")
		if strings.Contains(argv, path) {
			found = append(found, "pid "+entry.Name()+": "+argv)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	return found
}
