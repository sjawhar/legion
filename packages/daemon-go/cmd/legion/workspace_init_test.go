package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// testMainEnv makes this package's test binary the real `legion`: with it set, TestMain is main()
// — its argv, its signal handling, its exit status — so a test can run workspace-init as separate
// processes contending for one tree volume, the way two pods' init containers do.
const testMainEnv = "LEGION_CMD_TEST_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(testMainEnv) == "1" {
		main()
	}
	os.Exit(m.Run())
}

// winitWait bounds every wait a workspace-init test makes on another process, so a broken lock
// fails the test instead of hanging it.
const winitWait = 30 * time.Second

const winitRepo = "acme/widgets"

// fakeJJ is the jj first on the tree volume's PATH. It records every invocation as one line,
// "<WINIT_TAG> <argv>", and runs the real jj — except that a clone of github.com/acme/widgets
// clones the local bare remote, and with WINIT_HOLD set it first says so through the directory's
// `held` fifo and waits on its `release` fifo. A fresh provisioning's first command is that clone,
// so a process held there is holding the repository lock.
const fakeJJ = `#!/bin/sh
printf '%s %s\n' "$WINIT_TAG" "$*" >> "$WINIT_JJ_LOG"
if [ "$1 $2 $3" = "git clone https://github.com/acme/widgets" ]; then
	if [ -n "$WINIT_HOLD" ]; then
		echo held > "$WINIT_HOLD/held"
		read _ < "$WINIT_HOLD/release"
	fi
	shift 3
	exec "$WINIT_REAL_JJ" git clone "$WINIT_REMOTE" "$@"
fi
exec "$WINIT_REAL_JJ" "$@"
`

// treeVolume is one tree volume and what workspace-init runs against it: the provisioning token
// file, and a PATH whose jj clones a local bare remote in place of github.com/acme/widgets.
type treeVolume struct {
	root, token, jjLog, realJJ string
	env                        map[string]string
}

func newTreeVolume(t *testing.T) *treeVolume {
	t.Helper()
	realJJ, err := exec.LookPath("jj")
	if err != nil {
		t.Fatalf("workspace-init's tests drive a real jj: %v", err)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "jj"), []byte(fakeJJ), 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "provision-token")
	if err := os.WriteFile(token, []byte("ghs_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "legion")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	v := &treeVolume{root: root, token: token, jjLog: filepath.Join(dir, "jj.log"), realJJ: realJJ}
	v.env = map[string]string{
		"PATH":                        bin + string(filepath.ListSeparator) + os.Getenv("PATH"),
		"LEGION_PROVISION_TOKEN_FILE": token,
		"WINIT_REAL_JJ":               realJJ,
		"WINIT_REMOTE":                filepath.Join(dir, "no-remote.git"),
		"WINIT_JJ_LOG":                v.jjLog,
		"WINIT_TAG":                   "",
		"WINIT_HOLD":                  "",
		"JJ_USER":                     "Legion test",
		"JJ_EMAIL":                    "legion-test@example.invalid",
	}
	return v
}

// withRemote gives the volume's jj a real repository to clone: a bare remote whose main holds one
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

func (v *treeVolume) args(issue string) []string {
	return []string{"--issue", issue, "--repo", winitRepo, "--root", v.root, "--credential-helper", "!legion credential"}
}

// setenv is the volume's environment for an in-process run, with none of the optional variables a
// runtime sets inherited from whoever runs the tests.
func (v *treeVolume) setenv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"LEGION_RESUME_SESSION_FILE", "LEGION_WORKSPACE_RECOVERED_FROM", "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS"} {
		unsetenv(t, name)
	}
	for name, value := range v.env {
		t.Setenv(name, value)
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

// Every refusal the command makes happens before anything touches the volume or runs a tool
// (workspace-init.ts:148-163): an init container refused on its input leaves the tree volume as it
// found it, and holds no lock another pod would wait on.
func TestWorkspaceInitRefusesBeforeTouchingTheVolume(t *testing.T) {
	for _, tc := range []struct {
		name string
		args func(v *treeVolume) []string
		env  func(t *testing.T, v *treeVolume)
		code int
		says func(v *treeVolume) string
	}{
		{
			name: "no --issue",
			args: func(v *treeVolume) []string { return v.args("LEGION-42")[2:] },
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
				return []string{"--issue", "LEGION-42", "--repo", "acme", "--root", v.root, "--credential-helper", "x"}
			},
			code: 1,
			says: func(*treeVolume) string { return `--repo must be <owner>/<name> (got "acme")` },
		},
		{
			name: "no --credential-helper",
			args: func(v *treeVolume) []string { return v.args("LEGION-42")[:6] },
			code: 1,
			says: func(*treeVolume) string { return "--credential-helper is required" },
		},
		{
			name: "a relative --root",
			args: func(*treeVolume) []string {
				return []string{"--issue", "LEGION-42", "--repo", winitRepo, "--root", "legion-root", "--credential-helper", "x"}
			},
			code: 1,
			says: func(*treeVolume) string { return `--root must be an absolute path (got "legion-root")` },
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

// A fresh tree volume: the shared clone and the issue's jj workspace on its bookmark, the clone's
// credential helper the one named, the gh shim first on a pod's PATH, the two directories the main
// container mounts, one log line naming the workspace — and the repository lock free once it is
// done, so the next pod's init container never waits on a finished one.
func TestWorkspaceInitProvisionsTheIssueWorkspace(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.setenv(t)

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
}

// The same-agent invariant, checked before the repository lock (workspace-init.ts:167-185): a
// recorded OMP session missing from the volume is a launch failure, never a fresh agent. With the
// clone gone too the volume itself was lost, which the runtime reads from exit code 3; with the
// clone present only the session is gone, exit 1. Neither provisions or takes the lock, and a
// session that is present lets the same invocation through.
func TestWorkspaceInitRefusesAResumeWhoseSessionIsGone(t *testing.T) {
	v := newTreeVolume(t).withRemote(t)
	v.setenv(t)
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
	v.setenv(t)
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

// A provisioning that fails is the command's failure, with the failed command named, and the
// lock goes with it.
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
	overrides := map[string]string{testMainEnv: "1", "WINIT_TAG": tag}
	for name, value := range v.env {
		if _, set := overrides[name]; !set {
			overrides[name] = value
		}
	}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		overrides[name] = value
	}
	optional := map[string]bool{"LEGION_RESUME_SESSION_FILE": true, "LEGION_WORKSPACE_RECOVERED_FROM": true, "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS": true}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if _, overridden := overrides[name]; !overridden && !optional[name] {
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

// holder starts workspace-init for LEGION-42 and returns once it holds the repository lock: it is
// inside its first command, the clone, and stays there until release is called.
func (v *treeVolume) holder(t *testing.T) (p *initProcess, release func()) {
	t.Helper()
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

// fifo opens one end of a fifo — which blocks until the holder opens the other — drains it when
// reading, and closes it, bounded by winitWait and by the holder's staying alive and silent.
func fifo(t *testing.T, path string, flag int, holder *initProcess) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		file, err := os.OpenFile(path, flag, 0)
		if err == nil {
			if flag == os.O_RDONLY {
				_, err = io.Copy(io.Discard, file)
			}
			_ = file.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	case line, ok := <-holder.lines:
		t.Fatalf("the holder printed %q (open: %v) before reaching %s; stderr %q", line, ok, path, holder.stderr.String())
	case <-time.After(winitWait):
		t.Fatalf("nothing opened the other end of %s within %s", path, winitWait)
	}
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
