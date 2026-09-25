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
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// winitWait bounds every wait a workspace-init test makes on another process, so a broken lock
// fails the test instead of hanging it.
const winitWait = 30 * time.Second

const winitRepo = "acme/widgets"

// fakeJJ is the jj first on the tree volume's PATH. It records every invocation as one line,
// "<WINIT_TAG> <argv>" without the runner's leading --config pin, and runs the real jj with that
// pin — with WINIT_HOLD set, a clone of github.com/acme/widgets (which reaches the pod's feed)
// first writes to the directory's `held` fifo and waits on its `release` fifo. A fresh
// provisioning's first command is that clone, so a process held there is holding the repository
// lock.
const fakeJJ = `#!/bin/sh
pin=
case "$1" in --config=*) pin=$1; shift ;; esac
printf '%s %s\n' "$WINIT_TAG" "$*" >> "$WINIT_JJ_LOG"
if [ -n "$WINIT_HOLD" ] && [ "$1 $2 $3" = "git clone https://github.com/acme/widgets" ]; then
	printf held > "$WINIT_HOLD/held"
	read _ < "$WINIT_HOLD/release"
fi
exec "$WINIT_REAL_JJ" ${pin:+"$pin"} "$@"
`

// fakeGit is the git first on PATH. Its bare clone of github.com/acme/widgets — the fetch's —
// clones the local bare remote instead, whose file transport joins the https the runner allows,
// after appending the one-shot credential it was handed, the token file's path and what it held,
// to WINIT_CREDENTIAL_LOG. Every other invocation is the real git's.
const fakeGit = `#!/bin/sh
if [ "$1 $2 $3 $4" = "clone --bare --quiet https://github.com/acme/widgets" ]; then
	printf '%s %s\n' "$LEGION_PROVISIONING_TOKEN_FILE" "$(cat "$LEGION_PROVISIONING_TOKEN_FILE")" >> "$WINIT_CREDENTIAL_LOG"
	shift 4
	GIT_ALLOW_PROTOCOL="$GIT_ALLOW_PROTOCOL:file" exec "$WINIT_REAL_GIT" clone --bare --quiet "$WINIT_REMOTE" "$@"
fi
exec "$WINIT_REAL_GIT" "$@"
`

// treeVolume is one tree volume and what a pod's two init containers run against it: a PATH whose
// git clones a local bare remote in place of github.com/acme/widgets, the provisioning token file
// `fetch` is pointed at and `provision` never is, the pod's feed, a TMPDIR standing in for the
// fetching container's own filesystem, and, as in a pod, a jj config home that starts empty and no
// user configuration.
type treeVolume struct {
	root, token, feed, jjLog, credentialLog, realJJ, tmp string
	env                                                  map[string]string
}

func newTreeVolume(t *testing.T) *treeVolume {
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
	for name, script := range map[string]string{"jj": fakeJJ, "git": fakeGit} {
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
	v := &treeVolume{
		root: root, token: token, feed: filepath.Join(dir, "feed"), jjLog: filepath.Join(dir, "jj.log"),
		credentialLog: filepath.Join(dir, "credential.log"), realJJ: realJJ, tmp: tmp,
	}
	v.env = map[string]string{
		"PATH":                 bin + string(filepath.ListSeparator) + os.Getenv("PATH"),
		"TMPDIR":               tmp,
		"WINIT_REAL_JJ":        realJJ,
		"WINIT_REAL_GIT":       realGit,
		"WINIT_REMOTE":         filepath.Join(dir, "no-remote.git"),
		"WINIT_JJ_LOG":         v.jjLog,
		"WINIT_CREDENTIAL_LOG": v.credentialLog,
		"WINIT_TAG":            "",
		"WINIT_HOLD":           "",
		"JJ_USER":              "Legion test",
		"JJ_EMAIL":             "legion-test@example.invalid",
		"XDG_CONFIG_HOME":      filepath.Join(dir, "config"),
		"JJ_CONFIG":            filepath.Join(dir, "no-user-config.toml"),
	}
	return v
}

// withRemote gives the volume's git a real repository to clone: a bare remote whose main holds one
// commit, as github.com/acme/widgets would.
func (v *treeVolume) withRemote(t *testing.T) *treeVolume {
	t.Helper()
	dir := t.TempDir()
	remote := filepath.Join(dir, "remote.git")
	seed := filepath.Join(dir, "seed")
	for _, argv := range [][]string{
		{"git", "init", "--bare", "--initial-branch=main", remote},
		{v.realJJ, "git", "init", "--colocate", seed},
		{v.realJJ, "describe", "-m", "seed", "-R", seed},
		{v.realJJ, "bookmark", "set", "main", "-r", "@", "-R", seed},
		{v.realJJ, "git", "remote", "add", "origin", remote, "-R", seed},
		{v.realJJ, "git", "push", "--remote", "origin", "--bookmark", "main", "-R", seed},
	} {
		setup := exec.Command(argv[0], argv[1:]...)
		setup.Dir = dir
		setup.Env = append(os.Environ(), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")
		if output, err := setup.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, output)
		}
	}
	v.env["WINIT_REMOTE"] = remote
	return v
}

func (v *treeVolume) clone() string {
	return filepath.Join(v.root, "repos", "github.com", "acme", "widgets")
}

func (v *treeVolume) lock() string { return v.clone() + ".lock" }

func (v *treeVolume) workspace(issue string) string {
	return filepath.Join(v.root, "workspaces", "acme", "widgets", strings.ToLower(issue))
}

// fetchArgs are the first init container's; args are the second's, for issue.
func (v *treeVolume) fetchArgs() []string {
	return []string{"fetch", "--repo", winitRepo, "--feed", v.feed}
}

func (v *treeVolume) args(issue string) []string {
	return []string{"provision", "--issue", issue, "--repo", winitRepo, "--root", v.root, "--credential-helper", "!legion credential", "--feed", v.feed}
}

// runtimeOptionalEnv are the variables a runtime sets on an init container only for some
// launches; no run in these tests inherits them from whoever runs the tests.
var runtimeOptionalEnv = []string{"LEGION_PROVISION_TOKEN_FILE", "LEGION_RESUME_SESSION_FILE", "LEGION_WORKSPACE_RECOVERED_FROM", "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS"}

// setenv is the volume's environment for an in-process run of `provision`.
func (v *treeVolume) setenv(t *testing.T) {
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
func (v *treeVolume) fetch(t *testing.T) {
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

// jjCalls is every jj invocation the volume's PATH saw, "<tag> <argv>" each.
func (v *treeVolume) jjCalls(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(v.jjLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

func (v *treeVolume) jj(t *testing.T, args ...string) string {
	t.Helper()
	command := exec.Command(v.realJJ, args...)
	command.Env = append(os.Environ(), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("jj %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

func (v *treeVolume) git(t *testing.T, args ...string) string {
	t.Helper()
	output, err := exec.Command(v.env["WINIT_REAL_GIT"], args...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

// lockIsFree reports whether another process could take the repository lock right now.
func (v *treeVolume) lockIsFree(t *testing.T) bool {
	t.Helper()
	file, err := os.Open(v.lock())
	if err != nil {
		t.Fatalf("open the repository lock: %v", err)
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("flock the repository lock: %v", err)
	}
	return false
}

// Every refusal `provision` makes happens before anything touches the volume or runs a tool
// (workspace-init.ts:148-163): an init container refused on its input leaves the tree volume as it
// found it, and holds no lock another pod would wait on. It refuses to run pointed at the
// provisioning token, which `fetch` alone holds; and the command refuses no subcommand, or another.
func TestWorkspaceInitRefusesBeforeTouchingTheVolume(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(v *treeVolume) []string
		env  func(t *testing.T, v *treeVolume)
		code int
		says func(v *treeVolume) string
	}{
		{
			name: "no subcommand",
			args: func(*treeVolume) []string { return nil },
			code: 2,
			says: func(*treeVolume) string { return "usage: " + workspaceFetchUsage },
		},
		{
			name: "an unknown subcommand",
			args: func(v *treeVolume) []string { return append([]string{"init"}, v.args("LEGION-42")[1:]...) },
			code: 2,
			says: func(*treeVolume) string { return `unknown subcommand "init"` },
		},
		{
			name: "the one-container invocation, with no subcommand",
			args: func(v *treeVolume) []string { return v.args("LEGION-42")[1:] },
			code: 2,
			says: func(*treeVolume) string { return `unknown subcommand "--issue"` },
		},
		{
			name: "no --issue",
			args: func(v *treeVolume) []string { return append([]string{"provision"}, v.args("LEGION-42")[3:]...) },
			code: 1,
			says: func(*treeVolume) string { return `--issue must be a Dispatch issue key like LEGION-1 (got "")` },
		},
		{
			name: "an --issue that is not an issue key",
			args: func(v *treeVolume) []string { return v.args("nope") },
			code: 1,
			says: func(*treeVolume) string { return `--issue must be a Dispatch issue key like LEGION-1 (got "nope")` },
		},
		{
			name: "a --repo that is not owner/name",
			args: func(v *treeVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", "acme", "--root", v.root, "--credential-helper", "x", "--feed", v.feed}
			},
			code: 1,
			says: func(*treeVolume) string { return `workspace repository must be "owner/name" (got "acme")` },
		},
		{
			name: "a --repo with a .. segment",
			args: func(v *treeVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", "../x", "--root", v.root, "--credential-helper", "x", "--feed", v.feed}
			},
			code: 1,
			says: func(*treeVolume) string { return `workspace repository "../x" has a ".." segment` },
		},
		{
			name: "no --credential-helper",
			args: func(v *treeVolume) []string { return v.args("LEGION-42")[:7] },
			code: 1,
			says: func(*treeVolume) string { return "--credential-helper is required" },
		},
		{
			name: "a relative --root",
			args: func(v *treeVolume) []string {
				return []string{"provision", "--issue", "LEGION-42", "--repo", winitRepo, "--root", "legion-root", "--credential-helper", "x", "--feed", v.feed}
			},
			code: 1,
			says: func(*treeVolume) string { return `--root must be an absolute path (got "legion-root")` },
		},
		{
			name: "no --feed",
			args: func(v *treeVolume) []string { return v.args("LEGION-42")[:9] },
			code: 1,
			says: func(*treeVolume) string { return `--feed must be an absolute path (got "")` },
		},
		{
			name: "pointed at the provisioning token",
			env:  func(t *testing.T, v *treeVolume) { t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token) },
			code: 1,
			says: func(*treeVolume) string {
				return "LEGION_PROVISION_TOKEN_FILE is set: provisioning runs without the provisioning token, which `workspace-init fetch` alone holds"
			},
		},
		{
			name: "a lock wait that is not whole seconds",
			env:  func(t *testing.T, _ *treeVolume) { t.Setenv("LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS", "0.5") },
			code: 1,
			says: func(*treeVolume) string {
				return `LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS must be a positive whole number of seconds (got "0.5")`
			},
		},
		{
			name: "a lock wait of zero",
			env:  func(t *testing.T, _ *treeVolume) { t.Setenv("LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS", "0") },
			code: 1,
			says: func(*treeVolume) string {
				return `LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS must be a positive whole number of seconds (got "0")`
			},
		},
		{
			name: "no jj on PATH",
			env: func(t *testing.T, _ *treeVolume) {
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
			says: func(*treeVolume) string { return "jj is not on PATH" },
		},
		{
			name: "an unknown flag",
			args: func(v *treeVolume) []string { return append(v.args("LEGION-42"), "--bogus") },
			code: 2,
			says: func(*treeVolume) string { return "-bogus" },
		},
		{
			name: "a positional argument",
			args: func(v *treeVolume) []string { return append(v.args("LEGION-42"), "extra") },
			code: 2,
			says: func(*treeVolume) string { return `unexpected argument "extra"` },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newTreeVolume(t)
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
				t.Fatalf("the tree volume holds %v (%v), want it untouched", entries, err)
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

// Every refusal `fetch` makes happens before it runs git or writes anything: the feed is not
// created, and no one-shot credential is left in its TMPDIR.
func TestWorkspaceInitFetchRefusesBeforeFetching(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(v *treeVolume) []string
		env  func(t *testing.T, v *treeVolume)
		code int
		says func(v *treeVolume) string
	}{
		{
			name: "a --repo that is not owner/name",
			args: func(v *treeVolume) []string { return []string{"fetch", "--repo", "acme", "--feed", v.feed} },
			code: 1,
			says: func(*treeVolume) string { return `workspace repository must be "owner/name" (got "acme")` },
		},
		{
			name: "a --repo with a .. segment",
			args: func(v *treeVolume) []string { return []string{"fetch", "--repo", "../x", "--feed", v.feed} },
			code: 1,
			says: func(*treeVolume) string { return `workspace repository "../x" has a ".." segment` },
		},
		{
			name: "a relative --feed",
			args: func(*treeVolume) []string { return []string{"fetch", "--repo", winitRepo, "--feed", "feed"} },
			code: 1,
			says: func(*treeVolume) string { return `--feed must be an absolute path (got "feed")` },
		},
		{
			name: "LEGION_PROVISION_TOKEN_FILE unset",
			env:  func(t *testing.T, _ *treeVolume) { unsetenv(t, "LEGION_PROVISION_TOKEN_FILE") },
			code: 1,
			says: func(*treeVolume) string { return "LEGION_PROVISION_TOKEN_FILE is not set" },
		},
		{
			name: "a provisioning token file that is absent",
			env: func(t *testing.T, v *treeVolume) {
				t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token+".absent")
			},
			code: 1,
			says: func(v *treeVolume) string {
				return "LEGION_PROVISION_TOKEN_FILE names " + v.token + ".absent, which could not be read"
			},
		},
		{
			name: "a provisioning token file that is blank",
			env: func(t *testing.T, v *treeVolume) {
				if err := os.WriteFile(v.token, []byte(" \n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			code: 1,
			says: func(v *treeVolume) string { return "LEGION_PROVISION_TOKEN_FILE names " + v.token + ", which is empty" },
		},
		{
			name: "no git on PATH",
			env:  func(t *testing.T, _ *treeVolume) { t.Setenv("PATH", t.TempDir()) },
			code: 1,
			says: func(*treeVolume) string { return "git is not on PATH" },
		},
		{
			name: "an unknown flag",
			args: func(v *treeVolume) []string { return append(v.fetchArgs(), "--root", v.root) },
			code: 2,
			says: func(*treeVolume) string { return "-root" },
		},
		{
			name: "a positional argument",
			args: func(v *treeVolume) []string { return append(v.fetchArgs(), "extra") },
			code: 2,
			says: func(*treeVolume) string { return `unexpected argument "extra"` },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := newTreeVolume(t)
			v.setenv(t)
			t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)
			if tc.env != nil {
				tc.env(t, v)
			}
			args := v.fetchArgs()
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
			if _, err := os.Stat(v.feed); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the feed was created (%v)", err)
			}
			if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
				t.Fatalf("TMPDIR holds %v (%v), want no credential left behind", entries, err)
			}
			if _, err := os.Stat(v.credentialLog); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("git ran a clone before the refusal (%v)", err)
			}
		})
	}
}

// `fetch` clones the repository bare into the feed with the provisioning token, which its git is
// handed as a one-shot credential on the container's own filesystem (its TMPDIR) and which is gone
// once it returns; the feed never holds it.
func TestWorkspaceInitFetchFillsTheFeed(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.setenv(t)
	t.Setenv("LEGION_PROVISION_TOKEN_FILE", v.token)

	code, stdout, stderr := runWorkspaceInitHere(v.fetchArgs())
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	feed := filepath.Join(v.feed, "acme", "widgets.git")
	if want := "workspace-init fetch: https://github.com/acme/widgets into " + feed + "\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	if main := v.git(t, "--git-dir="+feed, "log", "-1", "--format=%s", "refs/heads/main"); main != "seed" {
		t.Fatalf("the feed's main is %q, want the remote's", main)
	}
	credential, err := os.ReadFile(v.credentialLog)
	if err != nil {
		t.Fatalf("the clone recorded no credential: %v", err)
	}
	tokenFile, held, _ := strings.Cut(strings.TrimSpace(string(credential)), " ")
	if held != "ghs_test" || !strings.HasPrefix(tokenFile, v.tmp+string(filepath.Separator)) {
		t.Fatalf("the clone was handed %s holding %q, want the provisioning token under TMPDIR %s", tokenFile, held, v.tmp)
	}
	if entries, err := os.ReadDir(v.tmp); err != nil || len(entries) != 0 {
		t.Fatalf("TMPDIR holds %v (%v) after fetch returned, want its credential gone", entries, err)
	}
	holdsNoToken(t, v.feed, "after fetch")
}

// A fresh tree volume, provisioned from the feed `fetch` filled: the shared clone, its origin still
// GitHub's, and the issue's jj workspace on its bookmark, the clone's credential helper the one
// named, the gh shim first on a pod's PATH and no tmux pane's `legion` launcher (a pod's PATH names
// the image's legion), the two directories the main container mounts, one log line naming the
// workspace — and the repository lock free once it is done, so the next pod's init container
// never waits on a finished one.
func TestWorkspaceInitProvisionsTheIssueWorkspace(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)

	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	workspace := v.workspace("LEGION-42")
	if want := "workspace-init: " + workspace + " on legion/LEGION-42\n"; stdout != want {
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
	if err != nil || !strings.HasSuffix(string(helpers), "\n!legion credential\n") {
		t.Fatalf("the clone's credential helpers are %q (%v), want the named helper last", helpers, err)
	}

	shim := filepath.Join(v.root, "worker-bin", "gh")
	info, err := os.Stat(shim)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("the gh shim: %v (%v), want a 0700 script", info, err)
	}
	if body, err := os.ReadFile(shim); err != nil || !strings.Contains(string(body), `exec legion gh -- "$@"`) ||
		!strings.Contains(string(body), "'"+filepath.Join(v.root, "worker-bin")+":'") {
		t.Fatalf("the gh shim is %q (%v), want it to strip its own directory and exec legion gh", body, err)
	}
	if _, err := os.Stat(filepath.Join(v.root, "bin")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the tree volume holds a legion launcher directory (%v), want only the gh shim", err)
	}
	for _, dir := range []string{"sessions", "gh"} {
		if info, err := os.Stat(filepath.Join(v.root, dir)); err != nil || !info.IsDir() {
			t.Fatalf("%s: %v (%v), want a directory", dir, info, err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, ".legion", "workspace-recovered.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a recovery marker without LEGION_WORKSPACE_RECOVERED_FROM: %v", err)
	}
	if !v.lockIsFree(t) {
		t.Fatal("the repository lock is still held after the command returned")
	}
	holdsNoToken(t, v.root, "after provisioning")
}

// The same-agent invariant, checked before the repository lock (workspace-init.ts:167-185): a
// recorded OMP session missing from the volume is a launch failure, never a fresh agent. With the
// clone gone too the volume itself was lost, which the runtime reads from exit code 3; with the
// clone present only the session is gone, exit 1. Neither provisions or takes the lock, and a
// session that is present lets the same invocation through.
func TestWorkspaceInitRefusesAResumeWhoseSessionIsGone(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	session := filepath.Join(v.root, "sessions", "legion-42-planner.jsonl")
	t.Setenv("LEGION_RESUME_SESSION_FILE", session)
	untouched := func(t *testing.T) {
		t.Helper()
		if _, err := os.Stat(v.lock()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the repository lock exists (%v), want the refusal before it", err)
		}
		if _, err := os.Stat(filepath.Join(v.root, "workspaces")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a workspace was provisioned (%v)", err)
		}
		if calls := v.jjCalls(t); calls != nil {
			t.Fatalf("jj ran %q", calls)
		}
	}

	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
	want := "Tree volume for LEGION-42 holds neither the clone (" + v.clone() + ") nor the recorded OMP session file (" + session + "): the volume was lost"
	if code != 3 || !strings.Contains(stderr, want) || stdout != "" {
		t.Fatalf("lost volume: exit %d, stdout %q, stderr %q; want 3 naming %q", code, stdout, stderr, want)
	}
	untouched(t)

	if err := os.MkdirAll(v.clone(), 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runWorkspaceInitHere(v.args("LEGION-42"))
	want = "Refusing to start LEGION-42 fresh: recorded OMP session file is missing from the tree volume: " + session
	if code != 1 || !strings.Contains(stderr, want) || stdout != "" {
		t.Fatalf("lost session: exit %d, stdout %q, stderr %q; want 1 naming %q", code, stdout, stderr, want)
	}
	untouched(t)

	if err := os.WriteFile(session, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runWorkspaceInitHere(v.args("LEGION-42"))
	if want := "workspace-init: " + v.workspace("LEGION-42") + " on legion/LEGION-42\n"; code != 0 || stdout != want {
		t.Fatalf("present session: exit %d, stdout %q, stderr %q; want 0 and %q", code, stdout, stderr, want)
	}
}

// The command side of workspace recovery (decision 11): a relaunch after a lost volume names the
// ref it recovers from, and the recreated workspace records it, with the commit it was recreated
// at, in .legion/workspace-recovered.json (workspace-init.ts:196-215).
func TestWorkspaceInitRecordsTheRecoveryMarker(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.fetch(t)
	t.Setenv("LEGION_WORKSPACE_RECOVERED_FROM", "legion/LEGION-42")

	before := time.Now().UTC().Truncate(time.Millisecond)
	code, _, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
	after := time.Now().UTC()
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	workspace := v.workspace("LEGION-42")
	body, err := os.ReadFile(filepath.Join(workspace, ".legion", "workspace-recovered.json"))
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
	if sha := v.jj(t, "log", "-r", "@", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", workspace); marker["sha"] != sha {
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
	err = writeRecoveryMarker(context.Background(), run, t.TempDir(), "legion/LEGION-42")
	if err == nil || !strings.Contains(err.Error(), "command timed out: jj log -r @ --no-graph -T commit_id") {
		t.Fatalf("writeRecoveryMarker = %v, want the timed-out jj log named", err)
	}
}

// A provisioning that fails — here, from a feed `fetch` never filled — is the command's failure,
// with the failed command named, and the lock goes with it.
func TestWorkspaceInitReportsAFailedProvisioning(t *testing.T) {
	v := newTreeVolume(t)
	v.setenv(t)

	code, stdout, stderr := runWorkspaceInitHere(v.args("LEGION-42"))
	if code != 1 || !strings.Contains(stderr, "jj git clone https://github.com/acme/widgets") || stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q; want 1 naming the failed clone", code, stdout, stderr)
	}
	if !v.lockIsFree(t) {
		t.Fatal("the repository lock is still held after the command failed")
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
func (v *treeVolume) start(t *testing.T, tag, issue string, env ...string) *initProcess {
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

func (p *initProcess) nextLine(t *testing.T) string {
	t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			t.Fatalf("workspace-init exited before its next line; stderr %q", p.stderr.String())
		}
		return line
	case <-time.After(winitWait):
		t.Fatalf("workspace-init printed nothing within %s", winitWait)
	}
	return ""
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

// holder fills the feed, starts `provision` for LEGION-42, and returns once it holds the repository
// lock: it is inside its first command, the clone, and stays there until release is called.
func (v *treeVolume) holder(t *testing.T) (p *initProcess, release func()) {
	t.Helper()
	v.fetch(t)
	hold := t.TempDir()
	for _, fifo := range []string{"held", "release"} {
		if err := syscall.Mkfifo(filepath.Join(hold, fifo), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p = v.start(t, "first", "LEGION-42", "WINIT_HOLD="+hold)
	fifo(t, filepath.Join(hold, "held"), os.O_RDONLY, p)
	return p, func() { fifo(t, filepath.Join(hold, "release"), os.O_WRONLY, p) }
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

func (v *treeVolume) waitingLine() string {
	return "workspace-init: waiting for " + v.lock() + " (another pod is provisioning acme/widgets)"
}

// Two pods of one tree admitted together provision against one shared clone. The second waits —
// saying so in its init log and running nothing — while the first provisions, then provisions
// its own workspace on the clone the first landed: every command of the first before any of the
// second's, and no second clone (workspace-init.ts:74-109).
func TestWorkspaceInitSerializesTwoProcessesOnOneVolume(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	first, release := v.holder(t)
	second := v.start(t, "second", "LEGION-43")

	if line := second.nextLine(t); line != v.waitingLine() {
		t.Fatalf("the second's first line is %q, want %q", line, v.waitingLine())
	}
	for _, call := range v.jjCalls(t) {
		if strings.HasPrefix(call, "second ") {
			t.Fatalf("the second ran %q while the first held the lock", call)
		}
	}
	release()

	if code, rest := first.wait(t); code != 0 || len(rest) != 1 || rest[0] != "workspace-init: "+v.workspace("LEGION-42")+" on legion/LEGION-42" {
		t.Fatalf("the first: exit %d, stdout %q, stderr %q", code, rest, first.stderr.String())
	}
	if code, rest := second.wait(t); code != 0 || len(rest) != 1 || rest[0] != "workspace-init: "+v.workspace("LEGION-43")+" on legion/LEGION-43" {
		t.Fatalf("the second: exit %d, stdout %q, stderr %q", code, rest, second.stderr.String())
	}
	calls := v.jjCalls(t)
	firstOfSecond := -1
	for i, call := range calls {
		if strings.HasPrefix(call, "second ") {
			firstOfSecond = i
			break
		}
	}
	if firstOfSecond <= 0 {
		t.Fatalf("jj calls %q, want the first's then the second's", calls)
	}
	for i, call := range calls {
		if want := i < firstOfSecond; strings.HasPrefix(call, "first ") != want {
			t.Fatalf("jj calls interleave at %d: %q", i, calls)
		}
		if strings.HasPrefix(call, "second git clone") {
			t.Fatalf("the second cloned again: %q", call)
		}
	}
	if want := "second config get git.abandon-unreachable-commits -R " + v.clone(); calls[firstOfSecond] != want {
		t.Fatalf("the second opened with %q, want %q on the clone the first landed", calls[firstOfSecond], want)
	}
}

// The lock is the holder's process: killed mid-provisioning, it holds nothing, and the waiting pod
// takes over at once — even though the jj it spawned is still running, and with no lease to
// expire or stale-lock judgement that could steal a live holder's lock.
func TestWorkspaceInitProceedsTheMomentTheHolderDies(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	first, _ := v.holder(t)
	second := v.start(t, "second", "LEGION-43", "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS=600")
	if line := second.nextLine(t); line != v.waitingLine() {
		t.Fatalf("the second's first line is %q, want %q", line, v.waitingLine())
	}

	if err := first.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if code, _ := first.wait(t); code != -1 {
		t.Fatalf("the killed holder exited %d", code)
	}
	if code, rest := second.wait(t); code != 0 || len(rest) != 1 || rest[0] != "workspace-init: "+v.workspace("LEGION-43")+" on legion/LEGION-43" {
		t.Fatalf("the second: exit %d, stdout %q, stderr %q", code, rest, second.stderr.String())
	}
	cloned := false
	for _, call := range v.jjCalls(t) {
		cloned = cloned || strings.HasPrefix(call, "second git clone https://github.com/acme/widgets ")
	}
	if !cloned {
		t.Fatalf("the second never cloned over the killed holder's partial clone: %q", v.jjCalls(t))
	}
}

// The provisioning token is the implement App's installation token, and every container of a tree
// mounts the tree volume under one uid: `provision`, the process that works on the volume, never
// holds it. Held mid-clone, holding the repository lock, it has written no one-shot credential to
// its TMPDIR, and no file on the tree volume holds the token — nor after it was killed there.
func TestWorkspaceInitProvisionHoldsNoToken(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	first, _ := v.holder(t)
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

// A live holder is waited on only as long as LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS says — the
// daemon sizes it from its own boot deadline — then the command fails naming the wait and the
// lock, having run nothing.
func TestWorkspaceInitGivesUpAfterTheLockWait(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.holder(t)
	second := v.start(t, "second", "LEGION-43", "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS=1")

	began := time.Now()
	code, rest := second.wait(t)
	want := "Timed out after 1 s waiting for workspace-init lock " + v.lock()
	if code != 1 || !strings.Contains(second.stderr.String(), want) {
		t.Fatalf("exit %d, stderr %q; want 1 naming %q", code, second.stderr.String(), want)
	}
	if elapsed := time.Since(began); elapsed < time.Second {
		t.Fatalf("gave up after %s, before the 1 s wait", elapsed)
	}
	if len(rest) != 1 || rest[0] != v.waitingLine() {
		t.Fatalf("stdout %q, want only %q", rest, v.waitingLine())
	}
	for _, call := range v.jjCalls(t) {
		if strings.HasPrefix(call, "second ") {
			t.Fatalf("the second ran %q", call)
		}
	}
}

// A pod deleted while its init container waits sends it SIGTERM: the wait ends then, not when the
// lock wait runs out, and nothing has run.
func TestWorkspaceInitStopsWaitingOnSIGTERM(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.holder(t)
	second := v.start(t, "second", "LEGION-43", "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS=600")
	if line := second.nextLine(t); line != v.waitingLine() {
		t.Fatalf("the second's first line is %q, want %q", line, v.waitingLine())
	}

	if err := second.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	code, _ := second.wait(t)
	if code != 1 || !strings.Contains(second.stderr.String(), "waiting for workspace-init lock "+v.lock()) {
		t.Fatalf("exit %d, stderr %q; want 1 naming the lock it stopped waiting for", code, second.stderr.String())
	}
	for _, call := range v.jjCalls(t) {
		if strings.HasPrefix(call, "second ") {
			t.Fatalf("the second ran %q", call)
		}
	}
}
