package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testTimeout = 30 * time.Second

// recordingRunner executes real jj and git commands but replaces only the GitHub clone URL with
// the local bare remote. That keeps provisioning's argv and credential environment observable
// while the fixture never reaches a network.
type recordingRunner struct {
	t       *testing.T
	remote  string
	timeout time.Duration

	mu        sync.Mutex
	commands  []Command
	killClone bool
}

func (r *recordingRunner) Timeout() time.Duration { return r.timeout }

func (r *recordingRunner) Run(ctx context.Context, command Command) (Result, error) {
	r.mu.Lock()
	r.commands = append(r.commands, Command{
		Argv:    append([]string(nil), command.Argv...),
		Env:     append([]string(nil), command.Env...),
		Dir:     command.Dir,
		Timeout: command.Timeout,
	})
	r.mu.Unlock()

	if r.killClone && isClone(command.Argv) {
		destination := command.Argv[4]
		if err := os.MkdirAll(filepath.Join(destination, ".jj"), 0o755); err != nil {
			return Result{}, fmt.Errorf("create partial clone: %w", err)
		}
		return Result{ExitCode: 137, Stderr: "clone interrupted"}, nil
	}

	actual := command
	actual.Argv = append([]string(nil), command.Argv...)
	if isClone(actual.Argv) {
		actual.Argv[3] = r.remote
	}
	return executeCommand(ctx, actual)
}

func (r *recordingRunner) Calls() []Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Command(nil), r.commands...)
}

func isClone(argv []string) bool {
	return len(argv) == 5 && argv[0] == "jj" && argv[1] == "git" && argv[2] == "clone"
}

func executeCommand(ctx context.Context, command Command) (Result, error) {
	commandCtx, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()

	child := exec.CommandContext(commandCtx, command.Argv[0], command.Argv[1:]...)
	child.Dir = command.Dir
	child.Env = append(os.Environ(), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")
	child.Env = append(child.Env, command.Env...)
	var stdout, stderr bytes.Buffer
	child.Stdout = &stdout
	child.Stderr = &stderr
	err := child.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		result.ExitCode = exited.ExitCode()
		result.TimedOut = errors.Is(commandCtx.Err(), context.DeadlineExceeded)
		return result, nil
	}
	return result, err
}

func localBareRemote(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	runSetup(t, root, "git", "init", "--bare", "--initial-branch=main", remote)

	seed := filepath.Join(root, "seed")
	runSetup(t, root, "jj", "git", "init", "--colocate", seed)
	runSetup(t, root, "jj", "bookmark", "set", "main", "-R", seed)
	runSetup(t, root, "jj", "git", "remote", "add", "origin", remote, "-R", seed)
	runSetup(t, root, "jj", "git", "push", "--remote", "origin", "--bookmark", "main", "--allow-empty-description", "-R", seed)
	return remote
}

func runSetup(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = dir
	command.Env = append(os.Environ(), "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, output)
	}
	return string(output)
}

func newLocalRunner(t *testing.T) *recordingRunner {
	t.Helper()
	return &recordingRunner{t: t, remote: localBareRemote(t), timeout: testTimeout}
}

func provisionRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		StateDir:         filepath.Join(t.TempDir(), "state"),
		Repo:             "acme/widgets",
		Issue:            "WIDGETS-42",
		Token:            "test-installation-token",
		CredentialHelper: "!/opt/legion/bin/legion credential",
	}
}

func commandEnv(command Command, key string) string {
	prefix := key + "="
	for _, entry := range command.Env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func commandWith(argv []string, prefix ...string) bool {
	if len(argv) < len(prefix) {
		return false
	}
	for index, value := range prefix {
		if argv[index] != value {
			return false
		}
	}
	return true
}

func findCall(t *testing.T, calls []Command, prefix ...string) Command {
	t.Helper()
	for _, call := range calls {
		if commandWith(call.Argv, prefix...) {
			return call
		}
	}
	t.Fatalf("no command begins %q in %#v", prefix, calls)
	return Command{}
}

func assertProvisioningEnvironment(t *testing.T, command Command) {
	t.Helper()
	for key, want := range map[string]string{
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_COUNT":    "2",
		"GIT_CONFIG_KEY_0":    "credential.helper",
		"GIT_CONFIG_VALUE_0":  "",
		"GIT_CONFIG_KEY_1":    "credential.interactive",
		"GIT_CONFIG_VALUE_1":  "true",
	} {
		if got := commandEnv(command, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if askpass := commandEnv(command, "GIT_ASKPASS"); askpass == "" {
		t.Error("provisioning command has no GIT_ASKPASS")
	}
	tokenFile := commandEnv(command, "LEGION_PROVISIONING_TOKEN_FILE")
	if tokenFile == "" {
		t.Error("provisioning command has no token file pointer")
	}
	if commandEnv(command, "LEGION_PROVISIONING_TOKEN") != "" {
		t.Error("provisioning command carries the token value instead of a file pointer")
	}
	if command.Timeout != testTimeout {
		t.Errorf("command timeout = %s, want slow-command budget %s", command.Timeout, testTimeout)
	}
}

func TestProvisionClonesThroughTemporarySiblingWithCredentialReset(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)

	workspace, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	clone := findCall(t, run.Calls(), "jj", "git", "clone")
	if got, want := clone.Argv[3], "https://github.com/acme/widgets"; got != want {
		t.Errorf("clone remote = %q, want %q", got, want)
	}
	if clone.Argv[4] == filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets") {
		t.Errorf("clone landed at final path %q instead of a temporary sibling", clone.Argv[4])
	}
	if !strings.HasPrefix(clone.Argv[4], filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets")+".clone-") {
		t.Errorf("clone target = %q, want temporary sibling", clone.Argv[4])
	}
	assertProvisioningEnvironment(t, clone)
	fetch := findCall(t, run.Calls(), "jj", "git", "fetch")
	assertProvisioningEnvironment(t, fetch)

	if want := filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets"); workspace.Dir == want {
		t.Errorf("workspace Dir = clone directory %q", workspace.Dir)
	}
	if _, err := os.Stat(filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets", ".jj")); err != nil {
		t.Fatalf("shared clone was not renamed into place: %v", err)
	}
	if _, err := os.Stat(workspace.Dir); err != nil {
		t.Fatalf("workspace was not created: %v", err)
	}
	if workspace.Bookmark != "legion/WIDGETS-42" {
		t.Errorf("bookmark = %q", workspace.Bookmark)
	}
	if tokenFile := commandEnv(clone, "LEGION_PROVISIONING_TOKEN_FILE"); tokenFile != "" {
		if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("provisioning token file remains after clone: %v", err)
		}
	}
	if resolved := strings.TrimSpace(runSetup(t, workspace.Dir, "jj", "log", "-r", workspace.Bookmark, "--no-graph", "-T", "commit_id")); resolved == "" {
		t.Error("missing bookmark was not created on the fresh workspace")
	}
}

func TestProvisionKilledCloneLeavesNoFinalDirectory(t *testing.T) {
	run := newLocalRunner(t)
	run.killClone = true
	req := provisionRequest(t)

	if _, err := Provision(context.Background(), run, req); err == nil {
		t.Fatal("Provision succeeded after its clone was killed")
	}

	final := filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets")
	if _, err := os.Stat(final); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("interrupted clone left final directory: %v", err)
	}
	parent := filepath.Dir(final)
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("read clone parent: %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "widgets.clone-") {
			t.Errorf("interrupted clone left temporary sibling %q", entry.Name())
		}
	}
}

func TestProvisionUsesBookmarkCommitAndRefusesConflictBeforeWorkspaceAdd(t *testing.T) {
	t.Run("one bookmark commit", func(t *testing.T) {
		run := newLocalRunner(t)
		req := provisionRequest(t)
		first, err := Provision(context.Background(), run, req)
		if err != nil {
			t.Fatalf("initial provision: %v", err)
		}
		commit := runSetup(t, first.Dir, "jj", "log", "-r", "@", "--no-graph", "-T", "commit_id")
		commit = strings.TrimSpace(commit)
		runSetup(t, req.StateDir, "jj", "workspace", "forget", strings.ToLower(req.Issue), "-R", filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets"))
		if err := os.RemoveAll(first.Dir); err != nil {
			t.Fatalf("remove forgotten workspace directory: %v", err)
		}

		before := len(run.Calls())
		second, err := Provision(context.Background(), run, req)
		if err != nil {
			t.Fatalf("re-provision from bookmark: %v", err)
		}
		if second != first {
			t.Errorf("workspace = %#v, want %#v", second, first)
		}
		calls := run.Calls()[before:]
		add := findCall(t, calls, "jj", "workspace", "add")
		if got := add.Argv[7]; got != commit {
			t.Errorf("workspace revision = %q, want bookmark commit %q", got, commit)
		}
		for _, call := range calls {
			if commandWith(call.Argv, "jj", "bookmark", "set") {
				t.Errorf("resolving bookmark should not be moved: %#v", call.Argv)
			}
		}
	})

	t.Run("conflicted bookmark", func(t *testing.T) {
		run := newLocalRunner(t)
		req := provisionRequest(t)
		workspace, err := Provision(context.Background(), run, req)
		if err != nil {
			t.Fatalf("initial provision: %v", err)
		}
		clone := filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets")
		runSetup(t, workspace.Dir, "jj", "new", "-m", "later work")
		operation := strings.TrimSpace(runSetup(t, clone, "jj", "op", "log", "--no-graph", "-T", "id.short()", "--limit", "1"))
		runSetup(t, workspace.Dir, "jj", "bookmark", "set", workspace.Bookmark, "-r", "@")
		runSetup(t, clone, "jj", "--at-op", operation, "bookmark", "set", workspace.Bookmark, "-r", "main", "--allow-backwards")
		runSetup(t, clone, "jj", "workspace", "forget", strings.ToLower(req.Issue), "-R", clone)
		if err := os.RemoveAll(workspace.Dir); err != nil {
			t.Fatalf("remove conflict workspace: %v", err)
		}

		before := len(run.Calls())
		_, err = Provision(context.Background(), run, req)
		if err == nil || !strings.Contains(err.Error(), "Bookmark legion/WIDGETS-42 is conflicted") {
			t.Fatalf("conflicted provision error = %v", err)
		}
		for _, call := range run.Calls()[before:] {
			if commandWith(call.Argv, "jj", "workspace", "add") {
				t.Errorf("conflicted bookmark reached workspace add: %#v", call.Argv)
			}
		}
		if _, statErr := os.Stat(workspace.Dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("conflicted bookmark created workspace directory: %v", statErr)
		}
	})
}

func TestProvisionForgetsRegisteredMissingWorkspace(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	workspace, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	if err := os.RemoveAll(workspace.Dir); err != nil {
		t.Fatalf("remove workspace directory: %v", err)
	}

	before := len(run.Calls())
	if _, err := Provision(context.Background(), run, req); err != nil {
		t.Fatalf("re-provision registered missing workspace: %v", err)
	}
	findCall(t, run.Calls()[before:], "jj", "workspace", "forget")
	if _, err := os.Stat(workspace.Dir); err != nil {
		t.Fatalf("missing workspace was not re-added: %v", err)
	}
}

func TestProvisionKeepsWorkspaceWhenFetchDeletesMergedBookmark(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	workspace, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	clone := filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets")
	if err := os.WriteFile(filepath.Join(workspace.Dir, "feature.txt"), []byte("merged work\n"), 0o644); err != nil {
		t.Fatalf("write worker change: %v", err)
	}
	runSetup(t, workspace.Dir, "jj", "status")
	commit := strings.TrimSpace(runSetup(t, workspace.Dir, "jj", "log", "-r", workspace.Bookmark, "--no-graph", "-T", "commit_id"))
	runSetup(t, clone, "jj", "git", "push", "--remote", "origin", "--bookmark", workspace.Bookmark, "--allow-empty-description")
	runSetup(t, req.StateDir, "git", "--git-dir="+run.remote, "branch", "-D", workspace.Bookmark)

	if _, err := Provision(context.Background(), run, req); err != nil {
		t.Fatalf("provision after remote branch deletion: %v", err)
	}
	reads, writes := 0, 0
	for _, call := range run.Calls() {
		if commandWith(call.Argv, "jj", "config", "get") && len(call.Argv) > 3 && call.Argv[3] == "git.abandon-unreachable-commits" {
			reads++
		}
		if commandWith(call.Argv, "jj", "config", "set") && len(call.Argv) > 4 && call.Argv[4] == "git.abandon-unreachable-commits" {
			writes++
		}
	}
	if reads != 2 || writes != 1 {
		t.Errorf("abandon-unreachable setting operations = %d reads, %d writes; want 2 reads and 1 write", reads, writes)
	}
	if _, err := os.Stat(filepath.Join(workspace.Dir, "feature.txt")); err != nil {
		t.Fatalf("fetch deleting bookmark lost working-copy content: %v", err)
	}
	if listed := strings.TrimSpace(runSetup(t, clone, "jj", "log", "-r", "bookmarks(exact:"+workspace.Bookmark+")", "--no-graph", "-T", "commit_id")); listed != "" {
		t.Errorf("bookmark remains after merged branch deletion: %s", listed)
	}
	ancestors := runSetup(t, workspace.Dir, "jj", "log", "-r", "ancestors(@)", "--no-graph", "-T", "commit_id")
	if !strings.Contains(ancestors, commit) {
		t.Errorf("working copy no longer descends from merged commit %s: %s", commit, ancestors)
	}
}

func TestRemoveForgetsTheWorkspace(t *testing.T) {
	run := newLocalRunner(t)
	workspace, err := Provision(context.Background(), run, provisionRequest(t))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if err := Remove(context.Background(), run, workspace); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(workspace.Dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("workspace remains after Remove: %v", err)
	}
}

func TestAdoptWorkingCopyCommandMatchesTheShippedRevset(t *testing.T) {
	got := AdoptWorkingCopyCommand("/state/workspaces/acme/widgets/widgets-42")
	want := []string{
		"jj", "metaedit", "--update-author", "-r", `@ & description(exact:"")`, "-R", "/state/workspaces/acme/widgets/widgets-42",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("AdoptWorkingCopyCommand = %#v, want %#v", got, want)
	}
}

func TestLocationMatchesProvisionedWorkspacePath(t *testing.T) {
	working, err := Location("/state", "acme/widgets", "WIDGETS-42")
	if err != nil {
		t.Fatalf("Location: %v", err)
	}
	if working.Dir != "/state/workspaces/acme/widgets/widgets-42" || working.Bookmark != "legion/WIDGETS-42" {
		t.Fatalf("Location = %#v, want workspace path and bookmark", working)
	}
}
