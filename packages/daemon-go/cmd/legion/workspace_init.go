package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceProvisionUsage = "legion workspace-init provision --issue <KEY> --repo <owner>/<repo> [--root /legion] --credential-helper <git helper> --feed <dir>"

const (
	// workspaceLostExitCode is the status that tells the runtime the tree volume itself was lost —
	// neither the shared clone nor the recorded OMP session is on it — rather than that one launch
	// failed (packages/daemon/src/daemon/runtime.ts:87).
	workspaceLostExitCode = 3
	// lockWaitEnv bounds how long this init container waits for another pod's provisioning of the
	// same repository. The daemon sets it on every pod from its own registration deadline, so the
	// wait never gives up on a pod the daemon would still tolerate.
	lockWaitEnv = "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS"
	// provisionTokenFileEnv points `workspace-init fetch` at the mounted provisioning token.
	provisionTokenFileEnv = "LEGION_PROVISION_TOKEN_FILE"
	// defaultLockWaitSeconds is for an invocation no daemon sized: three slow-command budgets, a
	// live holder's clone and fetch at full budget plus its local commands (workspace-init.ts:61).
	defaultLockWaitSeconds = 3 * int64(workspace.CommandTimeout/time.Second)
)

var lockWaitPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// volumeLostError is the one failure the runtime tells apart, by workspaceLostExitCode.
type volumeLostError string

func (e volumeLostError) Error() string { return string(e) }

// runWorkspaceInit is `legion workspace-init`, the Kubernetes runtime's two init containers, which
// prepare an issue's jj workspace on the tree's persistent volume before the main container's
// worker-shim starts (packages/daemon/src/cli/workspace-init.ts). `fetch` is the first: the one
// process of the pod that holds the provisioning token, in a container that mounts nothing a tree
// agent can write. `provision` is the second: all the tree volume's work, in a container the
// provisioning Secret is not mounted in. Each one's log lines go to stdout and its refusals and
// failures to stderr — together the init log the runtime quotes — with exit 1, or 3 for a lost
// volume.
func runWorkspaceInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "usage: %s\n       %s\n", workspaceFetchUsage, workspaceProvisionUsage)
		return 2
	}
	var run func() error
	switch args[0] {
	case "fetch":
		flags := newFlags("workspace-init fetch", stderr)
		repo := flags.String("repo", "", "repository as <owner>/<name> (required)")
		feed := flags.String("feed", "", "the pod's feed directory, the container's own (required)")
		if code, ok := parseWorkspaceInitFlags(flags, args[1:], workspaceFetchUsage, stderr); !ok {
			return code
		}
		run = func() error { return workspaceFetch(ctx, *repo, *feed, stdout) }
	case "provision":
		flags := newFlags("workspace-init provision", stderr)
		issue := flags.String("issue", "", "Dispatch issue key, e.g. LEGION-1 (required)")
		repo := flags.String("repo", "", "repository as <owner>/<name> (required)")
		root := flags.String("root", "/legion", "tree volume root directory")
		credentialHelper := flags.String("credential-helper", "", "git credential helper written into the shared clone's config (required)")
		feed := flags.String("feed", "", "the pod's feed directory, which `workspace-init fetch` filled (required)")
		if code, ok := parseWorkspaceInitFlags(flags, args[1:], workspaceProvisionUsage, stderr); !ok {
			return code
		}
		run = func() error { return workspaceInit(ctx, *issue, *repo, *root, *credentialHelper, *feed, stdout) }
	default:
		fmt.Fprintf(stderr, "legion workspace-init: unknown subcommand %q\n", args[0])
		return 2
	}
	err := run()
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "legion workspace-init %s: %v\n", args[0], err)
	if errors.As(err, new(volumeLostError)) {
		return workspaceLostExitCode
	}
	return 1
}

// parseWorkspaceInitFlags parses one subcommand's flags, refusing a positional argument; a false
// ok carries the exit code.
func parseWorkspaceInitFlags(flags *flag.FlagSet, args []string, usage string, stderr io.Writer) (code int, ok bool) {
	if err := flags.Parse(args); err != nil {
		return 2, false
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "legion %s: unexpected argument %q: %s\n", flags.Name(), flags.Arg(0), usage)
		return 2, false
	}
	return 0, true
}

// workspaceInit validates everything before it touches the volume, and refuses to run where the
// provisioning token is pointed at: this is the process that runs git and jj against what every
// agent of the tree can write. Then it installs the gh shim, creates the directories the main
// container mounts, holds a resume to the same agent, and provisions from the feed under the
// repository lock, which it holds until it returns.
func workspaceInit(ctx context.Context, issue, repo, root, credentialHelper, feed string, stdout io.Writer) error {
	if !legionclaim.IsIssueKey(issue) {
		return fmt.Errorf("--issue must be a Dispatch issue key like LEGION-1 (got %q)", issue)
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("--root must be an absolute path (got %q)", root)
	}
	if credentialHelper == "" {
		return fmt.Errorf("--credential-helper is required: %s", workspaceProvisionUsage)
	}
	if !filepath.IsAbs(feed) {
		return fmt.Errorf("--feed must be an absolute path (got %q)", feed)
	}
	if _, set := os.LookupEnv(provisionTokenFileEnv); set {
		return errors.New(provisionTokenFileEnv + " is set: provisioning runs without the provisioning token, which `workspace-init fetch` alone holds")
	}
	lockWait, err := workspaceInitLockWait()
	if err != nil {
		return err
	}
	tools, err := provisioningTools("git", "jj")
	if err != nil {
		return err
	}
	located, err := workspace.Location(root, repo, issue)
	if err != nil {
		return err
	}
	cloneDir := located.Clone

	if err := workerbin.InstallGh(root); err != nil {
		return err
	}
	for _, dir := range []string{"sessions", "gh"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Join(root, dir), err)
		}
	}
	// A recorded session gone from the volume is a launch failure, never a silent fresh agent —
	// which is what OMP does with a missing --resume path. Checked before the repository lock: a
	// doomed pod must not hold the shared clone's lock while it fails.
	if session, set := os.LookupEnv("LEGION_RESUME_SESSION_FILE"); set {
		present, err := pathPresent(session)
		if err != nil {
			return fmt.Errorf("stat the recorded OMP session file %s: %w", session, err)
		}
		if !present {
			cloned, err := pathPresent(cloneDir)
			if err != nil {
				return fmt.Errorf("stat the shared clone %s: %w", cloneDir, err)
			}
			if !cloned {
				return volumeLostError(fmt.Sprintf("Tree volume for %s holds neither the clone (%s) nor the recorded OMP session file (%s): the volume was lost", issue, cloneDir, session))
			}
			return fmt.Errorf("Refusing to start %s fresh: recorded OMP session file is missing from the tree volume: %s", issue, session)
		}
	}

	release, err := lockRepository(ctx, cloneDir+".lock", repo, lockWait, stdout)
	if err != nil {
		return err
	}
	defer release()
	run := workspace.NewRunner(workspace.CommandTimeout, tools)
	provisioned, err := workspace.Provision(ctx, run, workspace.Request{
		StateDir: root, Repo: repo, Issue: issue, CredentialHelper: credentialHelper, Source: workspace.FromFeed(feed),
		Log: func(line string) { fmt.Fprintln(stdout, "workspace-init: "+line) },
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "workspace-init: %s on %s\n", provisioned.Dir, provisioned.Bookmark)
	if fromRef, set := os.LookupEnv("LEGION_WORKSPACE_RECOVERED_FROM"); set {
		return writeRecoveryMarker(ctx, run, provisioned.Dir, fromRef)
	}
	return nil
}

// workspaceInitLockWait is LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS: a positive whole number of
// seconds, 900 when unset.
func workspaceInitLockWait() (int64, error) {
	value, set := os.LookupEnv(lockWaitEnv)
	if !set {
		return defaultLockWaitSeconds, nil
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if !lockWaitPattern.MatchString(value) || err != nil || seconds > int64(math.MaxInt64/time.Second) {
		return 0, fmt.Errorf("%s must be a positive whole number of seconds (got %q)", lockWaitEnv, value)
	}
	return seconds, nil
}

// provisioningTools resolves the named tools a provisioning step runs from PATH — in an init
// container the image's, with no worker-bin shim or operator rc ahead of them — where the
// TypeScript runner found them (workspace-init.ts:36-45).
func provisioningTools(names ...string) (map[string]string, error) {
	tools := map[string]string{}
	for _, tool := range names {
		path, err := exec.LookPath(tool)
		if err != nil {
			return nil, fmt.Errorf("%s is not on PATH: %w", tool, err)
		}
		tools[tool] = path
	}
	return tools, nil
}

// lockPollInterval is how often a waiting init container tries the repository lock again.
const lockPollInterval = 250 * time.Millisecond

// lockRepository serializes provisioning across a tree volume's init containers: every issue's
// pod provisions against one shared clone, and two pods admitted together would otherwise run the
// clone, the fetch, and the git config writes against it at once (git's config lock refuses the
// second writer). The lock is flock(2) on <clone>.lock, the only state two pods share, held on this
// process's own descriptor until release — or until the process dies, since the kernel drops the
// lock with its last descriptor and no child inherits it (Go opens every file close-on-exec). No
// lease, no mtime, no takeover: a live holder holds, however long it takes; a dead one holds
// nothing. Every attempt is non-blocking (flock(2) promises waiters no order, so polling gives up
// nothing); the first refused one logs one line, so a pod stuck behind another's provisioning says
// so in its init log, and the attempts continue every lockPollInterval, bounded by waitSeconds and
// by ctx (workspace-init.ts:74-139).
func lockRepository(ctx context.Context, lockPath, repo string, waitSeconds int64, log io.Writer) (release func(), err error) {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(lockPath), err)
	}
	// Read-write: where flock(2) is emulated with POSIX locks (NFS), an exclusive lock needs a
	// descriptor open for writing.
	file, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workspace-init lock %s: %w", lockPath, err)
	}
	release = func() { _ = file.Close() }
	deadline := time.Now().Add(time.Duration(waitSeconds) * time.Second)
	for attempt := 0; ; attempt++ {
		err := flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return release, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			release()
			return nil, fmt.Errorf("flock workspace-init lock %s: %w", lockPath, err)
		}
		if attempt == 0 {
			fmt.Fprintf(log, "workspace-init: waiting for %s (another pod is provisioning %s)\n", lockPath, repo)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			release()
			return nil, fmt.Errorf("Timed out after %d s waiting for workspace-init lock %s", waitSeconds, lockPath)
		}
		select {
		case <-ctx.Done():
			release()
			return nil, fmt.Errorf("stopped waiting for workspace-init lock %s: %w", lockPath, context.Cause(ctx))
		case <-time.After(min(lockPollInterval, remaining)):
		}
	}
}

func flock(fd, how int) error {
	for {
		if err := syscall.Flock(fd, how); !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// writeRecoveryMarker is the command side of workspace recovery: a relaunch after a lost volume
// names the ref it recovers from, and the recreated workspace records it with the commit it was
// recreated at in .legion/workspace-recovered.json (workspace-init.ts:196-215). recoveredAt is an
// ISO instant in milliseconds, UTC, as JavaScript's toISOString writes it.
func writeRecoveryMarker(ctx context.Context, run workspace.Runner, dir, fromRef string) error {
	result, err := workspace.RunChecked(ctx, run, []string{"jj", "log", "-r", "@", "--no-graph", "-T", "commit_id", "--color=never"}, nil, dir)
	if err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		RecoveredAt string `json:"recoveredAt"`
		FromRef     string `json:"fromRef"`
		SHA         string `json:"sha"`
		Reason      string `json:"reason"`
	}{time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), fromRef, strings.TrimSpace(result.Stdout), "volume-missing"})
	if err != nil {
		return err
	}
	markerDir := filepath.Join(dir, ".legion")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", markerDir, err)
	}
	if err := os.WriteFile(filepath.Join(markerDir, "workspace-recovered.json"), body, 0o644); err != nil {
		return fmt.Errorf("write the recovery marker: %w", err)
	}
	return nil
}

func pathPresent(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}
