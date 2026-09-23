package main

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceInitUsage = "legion workspace-init --issue <KEY> --repo <owner>/<repo> [--root /legion] --credential-helper <git helper>"

const (
	// workspaceLostExitCode is the status that tells the runtime the tree volume itself was lost —
	// neither the shared clone nor the recorded OMP session is on it — rather than that one launch
	// failed (packages/daemon/src/daemon/runtime.ts:87).
	workspaceLostExitCode = 3
	// lockWaitEnv bounds how long this init container waits for another pod's provisioning of the
	// same repository. The daemon sets it on every pod from its own registration deadline, so the
	// wait never gives up on a pod the daemon would still tolerate.
	lockWaitEnv = "LEGION_WORKSPACE_INIT_LOCK_WAIT_SECONDS"
	// defaultLockWaitSeconds is for an invocation no daemon sized: three slow-command budgets, a
	// live holder's clone and fetch at full budget plus its local commands (workspace-init.ts:61).
	defaultLockWaitSeconds = 3 * 300
	// provisionCommandTimeout bounds each command provisioning runs, the daemon's slow-command
	// budget (internal/daemon/outbox.go, workspace.NewRunner).
	provisionCommandTimeout = 5 * time.Minute
)

var lockWaitPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// volumeLostError is the one failure the runtime tells apart, by workspaceLostExitCode.
type volumeLostError string

func (e volumeLostError) Error() string { return string(e) }

// runWorkspaceInit is `legion workspace-init`, the Kubernetes runtime's init container: it prepares
// an issue's jj workspace on the tree's persistent volume before the main container's worker-shim
// starts (packages/daemon/src/cli/workspace-init.ts). Its log lines go to stdout and its refusals
// and failures to stderr — together the init log the runtime quotes — with exit 1, or 3 for a lost
// volume.
func runWorkspaceInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("workspace-init", stderr)
	issue := flags.String("issue", "", "Dispatch issue key, e.g. LEGION-1 (required)")
	repo := flags.String("repo", "", "repository as <owner>/<name> (required)")
	root := flags.String("root", "/legion", "tree volume root directory")
	credentialHelper := flags.String("credential-helper", "", "git credential helper written into the shared clone's config (required)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "legion workspace-init: unexpected argument %q: %s\n", flags.Arg(0), workspaceInitUsage)
		return 2
	}
	err := workspaceInit(ctx, *issue, *repo, *root, *credentialHelper, stdout)
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "legion workspace-init: %v\n", err)
	if errors.As(err, new(volumeLostError)) {
		return workspaceLostExitCode
	}
	return 1
}

// workspaceInit validates everything, and reads the provisioning token, before it touches the
// volume; then installs the gh shim, creates the directories the main container mounts, holds a
// resume to the same agent, and provisions under the repository lock, which it holds until it
// returns.
func workspaceInit(ctx context.Context, issue, repo, root, credentialHelper string, stdout io.Writer) error {
	if !legionclaim.IsIssueKey(issue) {
		return fmt.Errorf("--issue must be a Dispatch issue key like LEGION-1 (got %q)", issue)
	}
	if _, _, ok := splitRepoFlag(repo); !ok {
		return fmt.Errorf("--repo must be <owner>/<name> (got %q)", repo)
	}
	if !filepath.IsAbs(root) {
		return fmt.Errorf("--root must be an absolute path (got %q)", root)
	}
	if credentialHelper == "" {
		return fmt.Errorf("--credential-helper is required: %s", workspaceInitUsage)
	}
	tokenFile, set := os.LookupEnv("LEGION_PROVISION_TOKEN_FILE")
	if !set {
		return errors.New("LEGION_PROVISION_TOKEN_FILE is not set")
	}
	token, err := config.ReadSecretPointer("LEGION_PROVISION_TOKEN_FILE", tokenFile)
	if err != nil {
		return err
	}
	lockWait, err := workspaceInitLockWait()
	if err != nil {
		return err
	}
	tools, err := provisioningTools()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve this legion executable for the gh shim: %w", err)
	}
	cloneDir, err := workspace.CloneDir(root, repo)
	if err != nil {
		return err
	}

	if err := tmux.InstallWorkerBin(root, executable); err != nil {
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
	run := workspace.NewRunner(provisionCommandTimeout, tools)
	// The provisioning token is the implement App's installation token, and every container of the
	// tree mounts the volume under one uid. Its one-shot credential therefore goes on this
	// container's own filesystem, never under root: no agent of the tree can read it, and a kill
	// mid-clone leaves it only in this container.
	provisioned, err := workspace.Provision(ctx, run, workspace.Request{
		StateDir: root, Repo: repo, Issue: issue, Token: token, CredentialHelper: credentialHelper,
		CredentialDir: os.TempDir(),
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

// provisioningTools resolves the git and jj provisioning runs from PATH — in the init container the
// image's, with no worker-bin shim or operator rc ahead of them — where the TypeScript runner found
// them (workspace-init.ts:36-45).
func provisioningTools() (map[string]string, error) {
	tools := map[string]string{}
	for _, tool := range []string{"git", "jj"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			return nil, fmt.Errorf("%s is not on PATH: %w", tool, err)
		}
		tools[tool] = path
	}
	return tools, nil
}

// lockRepository serializes provisioning across a tree volume's init containers: every issue's
// pod provisions against one shared clone, and two pods admitted together would otherwise run the
// clone, the fetch, and the git config writes against it at once (git's config lock refuses the
// second writer). The lock is flock(2) on <clone>.lock, the only state two pods share, held on this
// process's own descriptor until release — or until the process dies, since the kernel drops the
// lock with its last descriptor and no child inherits it (Go opens every file close-on-exec). No
// lease, no mtime, no takeover: a live holder holds, however long it takes; a dead one holds
// nothing. A refused non-blocking attempt logs one line, so a pod stuck behind another's
// provisioning says so in its init log, then waits, bounded by waitSeconds and by ctx
// (workspace-init.ts:74-139).
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
	fd := int(file.Fd())
	err = flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return release, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		release()
		return nil, fmt.Errorf("flock workspace-init lock %s: %w", lockPath, err)
	}
	fmt.Fprintf(log, "workspace-init: waiting for %s (another pod is provisioning %s)\n", lockPath, repo)
	acquired := make(chan error, 1)
	go func() { acquired <- flock(fd, syscall.LOCK_EX) }()
	timer := time.NewTimer(time.Duration(waitSeconds) * time.Second)
	defer timer.Stop()
	select {
	case err := <-acquired:
		if err != nil {
			release()
			return nil, fmt.Errorf("flock workspace-init lock %s: %w", lockPath, err)
		}
		return release, nil
	case <-timer.C:
		err = fmt.Errorf("Timed out after %d s waiting for workspace-init lock %s", waitSeconds, lockPath)
	case <-ctx.Done():
		err = fmt.Errorf("stopped waiting for workspace-init lock %s: %w", lockPath, context.Cause(ctx))
	}
	// The blocked flock still uses the descriptor: close it once that call returns, whether it
	// took the lock or not.
	go func() {
		<-acquired
		release()
	}()
	return nil, err
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
	argv := []string{"jj", "log", "-r", "@", "--no-graph", "-T", "commit_id"}
	result, err := run.Run(ctx, workspace.Command{Argv: argv, Dir: dir, Timeout: run.Timeout()})
	if err != nil {
		return fmt.Errorf("run %s: %w", strings.Join(argv, " "), err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("%s in %s exited %d: %s", strings.Join(argv, " "), dir, result.ExitCode, strings.TrimSpace(result.Stderr))
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
