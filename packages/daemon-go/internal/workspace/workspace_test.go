package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const testTimeout = 30 * time.Second

// recordingRunner runs every command through the production runner, NewRunner, with the real jj
// and git, but replaces the GitHub clone URL with the local bare remote. That keeps
// provisioning's argv and credential environment observable, and what the runner adds to every
// process in force, while the fixture never reaches a network.
type recordingRunner struct {
	t       *testing.T
	remote  string
	timeout time.Duration
	runner  Runner

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
	// A clone from a pod's feed reaches it through the insteadOf its own environment names.
	if isClone(actual.Argv) && !slices.Contains(command.Env, "GIT_ALLOW_PROTOCOL=file") {
		actual.Argv[3] = r.remote
	}
	if commandWith(actual.Argv, "git", "clone", "--bare", "--quiet", "https://github.com/acme/widgets") {
		actual.Argv[4] = r.remote
	}
	return r.runner.Run(ctx, actual)
}

func (r *recordingRunner) Calls() []Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Command(nil), r.commands...)
}

func isClone(argv []string) bool {
	return len(argv) == 5 && argv[0] == "jj" && argv[1] == "git" && argv[2] == "clone"
}

// testTools are the git and jj the tests' runner starts, resolved from PATH as boot resolves them.
// Each is a wrapper that adds git's file transport to whatever allow-list the runner set: the
// local bare remote stands in for github.com, which the runner reaches over https alone.
func testTools(t *testing.T) map[string]string {
	t.Helper()
	tools := map[string]string{}
	dir := t.TempDir()
	for _, tool := range []string{"git", "jj"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatalf("provisioning's tests drive a real %s: %v", tool, err)
		}
		wrapper := filepath.Join(dir, tool)
		script := "#!/bin/sh\n[ -z \"${GIT_ALLOW_PROTOCOL+set}\" ] || export GIT_ALLOW_PROTOCOL=\"$GIT_ALLOW_PROTOCOL:file\"\nexec '" + path + "' \"$@\"\n"
		if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		tools[tool] = wrapper
	}
	return tools
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

// runSetup runs a fixture command, never under a GIT_CONFIG_PARAMETERS a test planted for
// provisioning's processes alone.
func runSetup(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = dir
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_CONFIG_PARAMETERS=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "JJ_USER=Legion test", "JJ_EMAIL=legion-test@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(argv, " "), err, output)
	}
	return string(output)
}

// newLocalRunner is a runner against a fresh local bare remote. jj reads no configuration of the
// user's who runs the tests: its user configuration is empty and its config home, where jj keeps a
// repository's configuration, is the test's own, as in a pod's init container.
func newLocalRunner(t *testing.T) *recordingRunner {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", home)
	t.Setenv("JJ_CONFIG", filepath.Join(home, "no-user-config.toml"))
	t.Setenv("JJ_USER", "Legion test")
	t.Setenv("JJ_EMAIL", "legion-test@example.invalid")
	return &recordingRunner{t: t, remote: localBareRemote(t), timeout: testTimeout, runner: NewRunner(testTimeout, testTools(t))}
}

// provisionRequest is a host provisioning's request, the tmux runtime's: the one-shot credential
// goes under the state directory.
func provisionRequest(t *testing.T) Request {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	return Request{
		StateDir:         state,
		Repo:             "acme/widgets",
		Issue:            "WIDGETS-42",
		Token:            "test-installation-token",
		CredentialHelper: "!/opt/legion/bin/legion credential",
		CredentialDir:    state,
	}
}

// Where the one-shot credential goes is every caller's decision, and a request reaches the
// repository one way: through a pod's feed with no credential, or from GitHub with the token and a
// credential directory. A request that names neither, or both, is refused before provisioning runs
// anything or touches the state directory.
func TestProvisionRefusesARequestWithNoOneWayToTheRepository(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Request)
		want string
	}{
		{"a token and no credential directory", func(r *Request) { r.CredentialDir = "" }, "workspace credential directory is required"},
		{"a feed and a token", func(r *Request) { r.Feed = "/var/run/legion/feed" }, "workspace request names a feed and a provisioning token"},
		{"a feed and a credential directory", func(r *Request) { r.Feed, r.Token = "/var/run/legion/feed", "" }, "workspace request names a feed and a provisioning token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newLocalRunner(t)
			req := provisionRequest(t)
			tc.edit(&req)
			if _, err := Provision(context.Background(), run, req); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Provision = %v, want a refusal naming %q", err, tc.want)
			}
			if calls := run.Calls(); len(calls) != 0 {
				t.Errorf("Provision ran %#v before refusing", calls)
			}
			if _, err := os.Stat(req.StateDir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("Provision touched the state directory before refusing: %v", err)
			}
		})
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

// assertCredentialedEnvironment fails unless command was handed the provisioning token as a file
// pointer, never as a value in its environment, under the slow-command budget.
func assertCredentialedEnvironment(t *testing.T, command Command) {
	t.Helper()
	if commandEnv(command, "LEGION_PROVISIONING_TOKEN_FILE") == "" {
		t.Errorf("%q has no token file pointer", command.Argv)
	}
	for _, entry := range command.Env {
		if strings.Contains(entry, "test-installation-token") {
			t.Errorf("%q carries the token value in its environment: %s", command.Argv, entry)
		}
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
	assertCredentialedEnvironment(t, clone)
	fetch := findCall(t, run.Calls(), "jj", "git", "fetch")
	assertCredentialedEnvironment(t, fetch)

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

// fetchRequest is a pod's first init container's request: the feed and the credential directory are
// the container's own.
func fetchRequest(t *testing.T) FetchRequest {
	t.Helper()
	return FetchRequest{
		Repo: "acme/widgets", Token: "test-installation-token", CredentialDir: t.TempDir(), Feed: filepath.Join(t.TempDir(), "feed"),
	}
}

// readOnly takes every write permission away from dir's tree, as a read-only mount does, until
// the test ends.
func readOnly(t *testing.T, dir string) {
	t.Helper()
	chmod := func(writable bool) error {
		return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.Type()&os.ModeSymlink != 0 {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			mode := info.Mode().Perm() &^ 0o222
			if writable {
				mode |= 0o200
			}
			return os.Chmod(path, mode)
		})
	}
	if err := chmod(false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = chmod(true) })
}

// Fetch clones the repository bare into the feed with the one-shot credential, and reads no git
// configuration but its own: not the system's, not the image user's, and not what git's `-c`
// (GIT_CONFIG_PARAMETERS) names in its environment — here each names a template directory, which
// would plant a file in every repository git creates. Its credential is gone once it returns.
func TestFetchClonesBareReadingNoConfigurationButItsOwn(t *testing.T) {
	run := newLocalRunner(t)
	template := t.TempDir()
	if err := os.WriteFile(filepath.Join(template, "planted"), []byte("from a configuration Fetch must not read\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(config, []byte("[init]\n\ttemplateDir = "+template+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	t.Setenv("GIT_CONFIG_SYSTEM", config)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'init.templatedir'='"+template+"'")
	req := fetchRequest(t)

	feed, err := Fetch(context.Background(), run, req)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if want := filepath.Join(req.Feed, "acme", "widgets.git"); feed != want {
		t.Fatalf("Fetch cloned into %s, want %s", feed, want)
	}
	if _, err := os.Stat(filepath.Join(feed, "planted")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Fetch's git read a configuration other than its own: the template planted a file (%v)", err)
	}
	if bare := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "--is-bare-repository")); bare != "true" {
		t.Errorf("the feed is bare: %s", bare)
	}
	if main := strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+feed, "rev-parse", "refs/heads/main")); main != strings.TrimSpace(runSetup(t, feed, "git", "--git-dir="+run.remote, "rev-parse", "main")) {
		t.Errorf("the feed's main is %s, not the remote's", main)
	}
	clone := findCall(t, run.Calls(), "git", "clone")
	if want := []string{"git", "clone", "--bare", "--quiet", "https://github.com/acme/widgets", feed}; !slices.Equal(clone.Argv, want) {
		t.Errorf("Fetch ran %q, want %q", clone.Argv, want)
	}
	assertCredentialedEnvironment(t, clone)
	if entries, err := os.ReadDir(req.CredentialDir); err != nil || len(entries) != 0 {
		t.Errorf("the one-shot credential outlived Fetch: %v (%v)", entries, err)
	}
}

// A pod's second init container provisions from the feed Fetch filled, which is read-only there,
// and holds no credential: the shared clone's clone and fetch reach https://github.com/<repo> —
// the remote its origin keeps naming — at the feed, and no command is handed a token file, an
// askpass, or a credential helper. The next pod's fetch brings what the remote gained since.
func TestProvisionFromAFeedHoldsNoCredential(t *testing.T) {
	run := newLocalRunner(t)
	state := filepath.Join(t.TempDir(), "state")
	provision := func(issue string) (Workspace, []Command) {
		t.Helper()
		fetch := fetchRequest(t)
		if _, err := Fetch(context.Background(), run, fetch); err != nil {
			t.Fatalf("Fetch: %v", err)
		}
		readOnly(t, fetch.Feed)
		before := len(run.Calls())
		working, err := Provision(context.Background(), run, Request{
			StateDir: state, Repo: "acme/widgets", Issue: issue, CredentialHelper: "!/opt/legion/bin/legion credential", Feed: fetch.Feed,
		})
		if err != nil {
			t.Fatalf("provision %s from the feed: %v", issue, err)
		}
		return working, run.Calls()[before:]
	}

	first, calls := provision("WIDGETS-42")
	for _, call := range calls {
		for _, entry := range call.Env {
			if key := environmentKey(entry); key == "LEGION_PROVISIONING_TOKEN_FILE" || key == "GIT_ASKPASS" || strings.HasPrefix(entry, "GIT_CONFIG_KEY_0=credential.") {
				t.Errorf("%q was handed %s", call.Argv, entry)
			}
		}
	}
	if clone := findCall(t, calls, "jj", "git", "clone"); clone.Argv[3] != "https://github.com/acme/widgets" {
		t.Errorf("the shared clone was cloned from %s", clone.Argv[3])
	}
	cloneDir := filepath.Join(state, "repos", "github.com", "acme", "widgets")
	if origin := strings.TrimSpace(runSetup(t, cloneDir, "git", "--git-dir="+filepath.Join(cloneDir, ".git"), "remote", "get-url", "origin")); origin != "https://github.com/acme/widgets" {
		t.Errorf("the shared clone's origin is %s, want GitHub's", origin)
	}
	if _, err := os.Stat(first.Dir); err != nil {
		t.Fatalf("the workspace was not created: %v", err)
	}

	advanceRemote(t, run.remote)
	head := strings.TrimSpace(runSetup(t, state, "git", "--git-dir="+run.remote, "rev-parse", "main"))
	provision("WIDGETS-43")
	if fetched := strings.TrimSpace(runSetup(t, cloneDir, "jj", "log", "-r", "main@origin", "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", cloneDir)); fetched != head {
		t.Errorf("after the next pod's fetch the shared clone's main@origin is %s, want the remote's %s", fetched, head)
	}
}

// The feed is GitHub as it stood when the pod's workspace-fetch ran, and a tree agent can push to
// the shared clone's origin in the meantime. Provisioning from the feed moves only main and the
// issue's own bookmark, so a bookmark another agent pushed after the snapshot keeps its target
// and its tracking — its next push is not refused as stale.
func TestProvisionFromAFeedKeepsABookmarkPushedAfterTheSnapshot(t *testing.T) {
	run := newLocalRunner(t)
	state := filepath.Join(t.TempDir(), "state")
	request := func(issue, feed string) Request {
		return Request{StateDir: state, Repo: "acme/widgets", Issue: issue, CredentialHelper: "!/opt/legion/bin/legion credential", Feed: feed}
	}
	first := fetchRequest(t)
	if _, err := Fetch(context.Background(), run, first); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	working, err := Provision(context.Background(), run, request("WIDGETS-42", first.Feed))
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	stale := fetchRequest(t)
	if _, err := Fetch(context.Background(), run, stale); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if err := os.WriteFile(filepath.Join(working.Dir, "pushed.txt"), []byte("an agent's work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	push := exec.Command("jj", "git", "push", "--named", "legion/WIDGETS-99=@", "--allow-empty-description")
	push.Dir = working.Dir
	push.Env = append(os.Environ(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=url."+run.remote+".insteadOf", "GIT_CONFIG_VALUE_0=https://github.com/acme/widgets")
	if output, err := push.CombinedOutput(); err != nil {
		t.Fatalf("a tree agent's push: %v\n%s", err, output)
	}
	pushed := strings.TrimSpace(runSetup(t, working.Dir, "jj", "log", "-r", "legion/WIDGETS-99", "--no-graph", "-T", "commit_id", "--ignore-working-copy"))

	if _, err := Provision(context.Background(), run, request("WIDGETS-43", stale.Feed)); err != nil {
		t.Fatalf("provision from the older feed: %v", err)
	}
	clone := filepath.Join(state, "repos", "github.com", "acme", "widgets")
	for _, revset := range []string{`bookmarks(exact:"legion/WIDGETS-99")`, `remote_bookmarks(exact:"legion/WIDGETS-99", exact:"origin")`} {
		if got := strings.TrimSpace(runSetup(t, clone, "jj", "log", "-r", revset, "--no-graph", "-T", "commit_id", "--ignore-working-copy", "-R", clone)); got != pushed {
			t.Errorf("%s is %q after provisioning from the older feed, want the pushed %s", revset, got, pushed)
		}
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

// pushRemoteBranch pushes branch to the bare remote from a clone of its own: one commit on main
// adding file. It returns the commit's id.
func pushRemoteBranch(t *testing.T, remote, branch, file string) string {
	t.Helper()
	scratch := t.TempDir()
	runSetup(t, scratch, "git", "clone", "--quiet", remote, "other")
	other := filepath.Join(scratch, "other")
	runSetup(t, other, "git", "checkout", "--quiet", "-b", branch)
	if err := os.WriteFile(filepath.Join(other, file), []byte("pushed from another clone\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSetup(t, other, "git", "add", file)
	runSetup(t, other, "git", "-c", "user.email=legion-test@example.invalid", "-c", "user.name=Legion test", "commit", "--quiet", "-m", "pushed from another clone")
	runSetup(t, other, "git", "push", "--quiet", "origin", branch)
	return strings.TrimSpace(runSetup(t, other, "git", "rev-parse", "HEAD"))
}

// An issue branch another clone pushed before the issue's first workspace existed — a repository's
// fixture, or the branch a tree pushed before its volume was lost — is where the workspace starts:
// provisioning tracks legion/<KEY>@origin and adds the workspace at it, on the host's path and on
// a pod's feed. The branch's commits are in the workspace, the local bookmark is the remote's, and
// the issue's next push moves that branch.
func TestProvisionAdoptsAnIssueBranchOnlyTheRemoteHas(t *testing.T) {
	for _, path := range []string{"host", "feed"} {
		t.Run(path, func(t *testing.T) {
			run := newLocalRunner(t)
			pushed := pushRemoteBranch(t, run.remote, "legion/WIDGETS-42", "fixture.txt")
			req := provisionRequest(t)
			if path == "feed" {
				fetch := fetchRequest(t)
				if _, err := Fetch(context.Background(), run, fetch); err != nil {
					t.Fatalf("Fetch: %v", err)
				}
				req = Request{StateDir: req.StateDir, Repo: req.Repo, Issue: req.Issue, CredentialHelper: req.CredentialHelper, Feed: fetch.Feed}
			}
			working, err := Provision(context.Background(), run, req)
			if err != nil {
				t.Fatalf("Provision: %v", err)
			}
			if _, err := os.Stat(filepath.Join(working.Dir, "fixture.txt")); err != nil {
				t.Errorf("the workspace does not hold the branch the remote already had: %v", err)
			}
			if parent := strings.TrimSpace(runSetup(t, working.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--ignore-working-copy")); parent != pushed {
				t.Errorf("the workspace's @- is %s, want the remote branch's %s", parent, pushed)
			}
			if local := strings.TrimSpace(runSetup(t, working.Dir, "jj", "log", "-r", `bookmarks(exact:"legion/WIDGETS-42")`, "--no-graph", "-T", "commit_id", "--ignore-working-copy")); local != pushed {
				t.Errorf("the local legion/WIDGETS-42 is %q, want the remote's %s", local, pushed)
			}

			if err := os.WriteFile(filepath.Join(working.Dir, "work.txt"), []byte("the implementer's change\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			runSetup(t, working.Dir, "jj", "describe", "-m", "the implementer's change")
			runSetup(t, working.Dir, "jj", "bookmark", "set", "legion/WIDGETS-42", "-r", "@")
			push := exec.Command("jj", "git", "push", "--bookmark", "legion/WIDGETS-42")
			push.Dir = working.Dir
			push.Env = append(os.Environ(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=url."+run.remote+".insteadOf", "GIT_CONFIG_VALUE_0=https://github.com/acme/widgets")
			if output, err := push.CombinedOutput(); err != nil {
				t.Fatalf("the issue's push of legion/WIDGETS-42: %v\n%s", err, output)
			}
			parents := strings.TrimSpace(runSetup(t, req.StateDir, "git", "--git-dir="+run.remote, "log", "-1", "--format=%P", "legion/WIDGETS-42"))
			if parents != pushed {
				t.Errorf("after the push the remote branch's head has parents %q, want the adopted %s", parents, pushed)
			}
		})
	}
}

// A merged pull request's branch, deleted on GitHub, leaves no remote row: an issue workspace
// provisioned after that — its directory gone, as when a tree's volume is lost — starts from main
// as a brand-new issue's does, and the merged branch is not brought back.
func TestProvisionAfterTheMergedBranchIsDeletedStartsFromMain(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	first, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	clone := filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets")
	if err := os.WriteFile(filepath.Join(first.Dir, "merged.txt"), []byte("merged work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSetup(t, first.Dir, "jj", "describe", "-m", "merged work")
	merged := strings.TrimSpace(runSetup(t, first.Dir, "jj", "log", "-r", first.Bookmark, "--no-graph", "-T", "commit_id"))
	runSetup(t, clone, "jj", "git", "push", "--remote", "origin", "--bookmark", first.Bookmark)
	runSetup(t, req.StateDir, "git", "--git-dir="+run.remote, "branch", "-D", first.Bookmark)
	runSetup(t, clone, "jj", "workspace", "forget", strings.ToLower(req.Issue), "-R", clone)
	if err := os.RemoveAll(first.Dir); err != nil {
		t.Fatal(err)
	}

	second, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision after the merged branch was deleted: %v", err)
	}
	main := strings.TrimSpace(runSetup(t, clone, "jj", "log", "-r", "main@origin", "--no-graph", "-T", "commit_id", "--ignore-working-copy"))
	if parent := strings.TrimSpace(runSetup(t, second.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--ignore-working-copy")); parent != main {
		t.Errorf("the new workspace's @- is %s, want main's %s", parent, main)
	}
	if _, err := os.Stat(filepath.Join(second.Dir, "merged.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the new workspace holds the merged branch's file: %v", err)
	}
	if local := strings.TrimSpace(runSetup(t, second.Dir, "jj", "log", "-r", `bookmarks(exact:"legion/WIDGETS-42")`, "--no-graph", "-T", "commit_id", "--ignore-working-copy")); local == merged || local == "" {
		t.Errorf("the local legion/WIDGETS-42 is %q: want it created on the new working copy, never the merged %s", local, merged)
	}
}

// A local legion/<KEY> deleted in the shared clone (`jj bookmark delete`) and never pushed leaves
// origin's row tracked with no local bookmark. Tracking it changes nothing, and starting at main
// would put a second branch beside the remote's, which the next push would drop. Provisioning
// refuses that state by name, every time, before anything is registered. Forgetting the bookmark,
// one of the two ways out the refusal names, leaves an untracked row after the next fetch, which
// is adopted.
func TestProvisionRefusesALocalDeletionNeverPushed(t *testing.T) {
	run := newLocalRunner(t)
	req := provisionRequest(t)
	first, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("initial provision: %v", err)
	}
	clone := filepath.Join(req.StateDir, "repos", "github.com", "acme", "widgets")
	if err := os.WriteFile(filepath.Join(first.Dir, "pushed.txt"), []byte("pushed work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSetup(t, first.Dir, "jj", "describe", "-m", "pushed work")
	pushed := strings.TrimSpace(runSetup(t, first.Dir, "jj", "log", "-r", first.Bookmark, "--no-graph", "-T", "commit_id"))
	runSetup(t, clone, "jj", "git", "push", "--remote", "origin", "--bookmark", first.Bookmark)
	runSetup(t, clone, "jj", "bookmark", "delete", first.Bookmark)
	runSetup(t, clone, "jj", "workspace", "forget", strings.ToLower(req.Issue), "-R", clone)
	if err := os.RemoveAll(first.Dir); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		before := len(run.Calls())
		_, err := Provision(context.Background(), run, req)
		if err == nil || !strings.Contains(err.Error(), "Bookmark legion/WIDGETS-42 was deleted in the shared clone") ||
			!strings.Contains(err.Error(), "jj bookmark set legion/WIDGETS-42 -r legion/WIDGETS-42@origin") ||
			!strings.Contains(err.Error(), "jj bookmark forget legion/WIDGETS-42") {
			t.Fatalf("attempt %d: provision error = %v, want the local deletion refused with both ways out", attempt, err)
		}
		for _, call := range run.Calls()[before:] {
			if commandWith(call.Argv, "jj", "workspace", "add") || commandWith(call.Argv, "jj", "bookmark", "track") {
				t.Errorf("attempt %d ran %q", attempt, call.Argv)
			}
		}
		if _, statErr := os.Stat(first.Dir); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("attempt %d created the workspace directory: %v", attempt, statErr)
		}
		if listed := runSetup(t, clone, "jj", "workspace", "list", "-R", clone); strings.Contains(listed, strings.ToLower(req.Issue)+":") {
			t.Errorf("attempt %d registered the workspace:\n%s", attempt, listed)
		}
	}

	runSetup(t, clone, "jj", "bookmark", "forget", first.Bookmark, "-R", clone)
	working, err := Provision(context.Background(), run, req)
	if err != nil {
		t.Fatalf("provision after the bookmark was forgotten: %v", err)
	}
	if parent := strings.TrimSpace(runSetup(t, working.Dir, "jj", "log", "-r", "@-", "--no-graph", "-T", "commit_id", "--ignore-working-copy")); parent != pushed {
		t.Errorf("after the forget the workspace's @- is %s, want the pushed %s", parent, pushed)
	}
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
	if working.Dir != "/state/workspaces/acme/widgets/widgets-42" || working.Bookmark != "legion/WIDGETS-42" ||
		working.Clone != "/state/repos/github.com/acme/widgets" {
		t.Fatalf("Location = %#v, want workspace path, bookmark, and shared clone", working)
	}
}

// A `.` or `..` segment would put the shared clone somewhere else under the state directory, and
// provisioning removes an incomplete clone there (`--repo ../..` removed the tree volume's root):
// every path the package derives from a repository refuses one, naming it.
func TestARepositoryWithADotSegmentIsRefused(t *testing.T) {
	for _, tc := range []struct{ repo, segment string }{
		{"../x", ".."}, {"acme/..", ".."}, {"./..", "."}, {"../..", ".."}, {"acme/.", "."},
	} {
		want := `workspace repository "` + tc.repo + `" has a "` + tc.segment + `" segment`
		if _, err := Location("/state", tc.repo, "WIDGETS-42"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Location(%q) = %v, want an error naming %q", tc.repo, err, want)
		}
	}
}

// A `.` or `..` issue would name the repository's workspaces directory, or its owner's, as the
// issue's workspace, and Remove deletes the workspace directory whole: Location refuses one, naming
// it, and Remove removes nothing but a workspace Location names.
func TestAnIssueWithADotSegmentIsRefused(t *testing.T) {
	state := t.TempDir()
	for _, issue := range []string{".", ".."} {
		want := `workspace issue "` + issue + `" is a "` + issue + `" segment`
		if _, err := Location(state, "acme/widgets", issue); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Location(%q) = %v, want an error naming %q", issue, err, want)
		}
	}
	sentinel := filepath.Join(state, "workspaces", "acme", "other", "widgets-7", "work.txt")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("another repository's workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := newLocalRunner(t)
	clone := filepath.Join(state, "repos", "github.com", "acme", "widgets")
	for _, dir := range []string{
		filepath.Join(state, "workspaces", "acme", "widgets") + "/..",
		filepath.Join(state, "workspaces", "acme", "widgets") + "/.",
		filepath.Join(state, "workspaces", "acme"),
		filepath.Join(state, "workspaces", "acme", "other", "widgets-7"),
	} {
		err := Remove(context.Background(), run, Workspace{Dir: dir, Bookmark: "legion/WIDGETS-42", Clone: clone})
		if err == nil || !strings.Contains(err.Error(), "is not a workspace Location names") {
			t.Errorf("Remove(%s) = %v, want it refused as no workspace Location names", dir, err)
		}
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("Remove deleted another repository's workspace: %v", err)
	}
	if calls := run.Calls(); len(calls) != 0 {
		t.Errorf("Remove ran %#v before refusing", calls)
	}
}
