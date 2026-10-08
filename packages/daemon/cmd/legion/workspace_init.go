package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceProvisionUsage = "legion workspace-init provision --issue <KEY> --repo <owner>/<repo> [--root /legion] --credential-helper <git helper> --feed <dir>"

const (
	// workspaceLostExitCode reports an expected issue volume with neither its clone nor any
	// retained session. A single missing role transcript is the role launcher's refusal.
	workspaceLostExitCode = 3
	// provisionTokenFileEnv points `workspace-init fetch` at the mounted provisioning token.
	provisionTokenFileEnv = "LEGION_PROVISION_TOKEN_FILE"
	// expectVolumeEnv is the daemon's word that this issue's volume must already hold the clone
	// or a retained session: the issue has run before, so a volume with neither was lost.
	expectVolumeEnv = "LEGION_EXPECT_ISSUE_VOLUME"
)

// volumeLostError is the one failure the runtime tells apart, by workspaceLostExitCode.
type volumeLostError string

func (e volumeLostError) Error() string { return string(e) }

// workspaceInitCommands is `legion workspace-init`, the Kubernetes runtime's init containers, which
// prepare a pod's persistent volume before the main container's worker-shim starts, and the one
// list of them `legion workspace-init --help` names. An issue pod runs two, which prepare the
// issue's clone and jj workspace on the issue's own volume: `fetch` is the first, the one process of
// the pod that holds the provisioning token, in a container that mounts nothing an agent of the
// issue can write; `provision` is the second, all the volume's work, in a container the provisioning
// Secret is not mounted in. The controller's pod (`controller: daemon`) runs `controller` alone, on
// the volume its own Sandbox owns. Each one's log lines go to stdout and its refusals and failures
// to stderr — together the init log the runtime quotes — with exit 1, or 3 for a lost volume.
var workspaceInitCommands = map[string]command{
	"fetch":      runWorkspaceFetch,
	"provision":  runWorkspaceProvision,
	"controller": runWorkspaceController,
}

func runWorkspaceInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "workspace-init", workspaceInitCommands, args, stdout, stderr)
}

func runWorkspaceFetch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("workspace-init fetch", "usage: "+workspaceFetchUsage, stderr)
	repo := flags.String("repo", "", "repository as <owner>/<name> (required)")
	feed := flags.String("feed", "", "the pod's feed directory, the container's own (required)")
	if code, ok := parseWorkspaceInitFlags(flags, args, stderr); !ok {
		return code
	}
	return workspaceInitExit(flags, workspaceFetch(ctx, *repo, *feed, stdout), stderr)
}

func runWorkspaceProvision(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("workspace-init provision", "usage: "+workspaceProvisionUsage, stderr)
	issue := flags.String("issue", "", "Dispatch issue key, e.g. LEGION-1 (required)")
	repo := flags.String("repo", "", "repository as <owner>/<name> (required)")
	root := flags.String("root", "/legion", "issue volume root directory")
	credentialHelper := flags.String("credential-helper", "", "git credential helper written into the clone's config (required)")
	feed := flags.String("feed", "", "the pod's feed directory, which workspace-init fetch filled (required)")
	if code, ok := parseWorkspaceInitFlags(flags, args, stderr); !ok {
		return code
	}
	return workspaceInitExit(flags, workspaceInit(ctx, *issue, *repo, *root, *credentialHelper, *feed, stdout), stderr)
}

const workspaceControllerUsage = "legion workspace-init controller [--root /legion]"

func runWorkspaceController(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("workspace-init controller", "usage: "+workspaceControllerUsage, stderr)
	root := flags.String("root", "/legion", "the controller's volume root directory")
	if code, ok := parseWorkspaceInitFlags(flags, args, stderr); !ok {
		return code
	}
	return workspaceInitExit(flags, workspaceController(*root), stderr)
}

// workspaceController prepares the controller's volume: the sessions directory Oh My Pi's
// sessions are mounted from, and nothing else — the controller works no repository and holds no
// GitHub credential, so it gets no workspace and no gh shim. On a resume it holds the controller to
// the session it recorded, as workspace-init holds a tree agent: a session gone from the volume
// means the volume was lost (the controller's volume holds nothing else to tell a lost volume from
// a lost file), which the runtime reads from workspaceLostExitCode, so the daemon relaunches a
// fresh controller rather than resuming one no attempt can find.
func workspaceController(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("--root must be an absolute path (got %q)", root)
	}
	if _, set := os.LookupEnv(provisionTokenFileEnv); set {
		return errors.New(provisionTokenFileEnv + " is set: the controller's pod holds no provisioning token")
	}
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", sessions, err)
	}
	session, set := os.LookupEnv("LEGION_RESUME_SESSION_FILE")
	if !set {
		return nil
	}
	present, err := pathPresent(session)
	if err != nil {
		return fmt.Errorf("stat the recorded OMP session file %s: %w", session, err)
	}
	if !present {
		return volumeLostError(fmt.Sprintf("The controller's volume holds no recorded OMP session file (%s): the volume was lost", session))
	}
	return nil
}

// parseWorkspaceInitFlags parses one subcommand's flags, refusing a positional argument; a false
// ok carries the exit code.
func parseWorkspaceInitFlags(flags *flag.FlagSet, args []string, stderr io.Writer) (code int, ok bool) {
	if code, ok := parseFlags(flags, args); !ok {
		return code, false
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(stderr, "%s: unexpected argument %q\n", flags.Name(), flags.Arg(0))
		flags.Usage()
		return 2, false
	}
	return 0, true
}

// workspaceInitExit is a subcommand's exit status once it has run: 0, or its failure on stderr
// with 1, or workspaceLostExitCode for a lost volume.
func workspaceInitExit(flags *flag.FlagSet, err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "%s: %v\n", flags.Name(), err)
	if errors.As(err, new(volumeLostError)) {
		return workspaceLostExitCode
	}
	return 1
}

// workspaceInit validates everything before it touches the volume, --repo first, and refuses to
// run where the provisioning token is pointed at: this is the process that runs git and jj against
// what every agent of the issue can write. Then it installs the gh shim, creates the directories the
// main container mounts, holds a resume to the same agent, and provisions from the feed. No lock
// guards the clone: it is the issue's own, on the issue's own volume, and no other pod mounts it.
func workspaceInit(ctx context.Context, issue, repo, root, credentialHelper, feed string, stdout io.Writer) error {
	repository, err := ghrepo.Parse("--repo", repo)
	if err != nil {
		return err
	}
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
	tools, err := provisioningTools("git", "jj")
	if err != nil {
		return err
	}
	located, err := workspace.Location(root, repository, issue)
	if err != nil {
		return err
	}
	cloneDir := located.Clone

	if value, set := os.LookupEnv(expectVolumeEnv); set {
		expected, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("%s must be a boolean: %w", expectVolumeEnv, err)
		}
		if expected {
			cloned, err := pathPresent(cloneDir)
			if err != nil {
				return fmt.Errorf("stat the clone %s: %w", cloneDir, err)
			}
			if !cloned {
				sessions, err := hasRetainedSession(filepath.Join(root, "sessions"))
				if err != nil {
					return err
				}
				if !sessions {
					return volumeLostError(fmt.Sprintf("The volume of %s holds neither the clone (%s) nor retained sessions: the issue's volume was lost", issue, cloneDir))
				}
			}
		}
	}
	if err := workerbin.InstallGh(root); err != nil {
		return err
	}
	for _, dir := range []string{"sessions", "gh"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Join(root, dir), err)
		}
	}

	run := workspace.NewRunner(workspace.CommandTimeout, tools)
	provisioned, err := workspace.Provision(ctx, run, workspace.Request{
		StateDir: root, Repo: repository, Issue: issue, CredentialHelper: credentialHelper, Source: workspace.FromFeed(feed),
		Log: func(line string) { fmt.Fprintln(stdout, "workspace-init: "+line) },
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "workspace-init: %s on %s\n", provisioned.Dir, provisioned.Bookmark)
	if fromRef, set := os.LookupEnv("LEGION_WORKSPACE_RECOVERED_FROM"); set {
		return writeRecoveryMarker(ctx, run, provisioned, issue, fromRef)
	}
	return nil
}

func hasRetainedSession(dir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && path == dir {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("read retained sessions in %s: %w", dir, err)
	}
	return found, nil
}

// provisioningTools resolves the named tools a provisioning step runs from PATH — in an init
// container the image's, with no worker-bin shim or operator rc ahead of them — where the
// TypeScript runner found them (processEnvRunner, workspace-init.ts).
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

// writeRecoveryMarker is the command side of workspace recovery: a relaunch after a lost volume
// names the ref it recovers from, and the recreated workspace records it with the commit it was
// recreated at in .legion/<issue>/workspace-recovered.json (cmdWorkspaceInit's
// LEGION_WORKSPACE_RECOVERED_FROM branch, workspace-init.ts), under issue's own directory like
// every other handoff (dispatch://LEGION-565), so two trees recovering at once never touch the
// same path either. recoveredAt is an ISO instant in milliseconds, UTC, as JavaScript's
// toISOString writes it.
func writeRecoveryMarker(ctx context.Context, run workspace.Runner, ws workspace.Workspace, issue, fromRef string) error {
	result, err := workspace.RunCheckedIn(ctx, run, ws, []string{"jj", "log", "-r", "@", "--no-graph", "-T", "commit_id", "--color=never"})
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
	marker := filepath.Join(ws.Dir, handoffFile(issue, "workspace-recovered.json"))
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(marker), err)
	}
	if err := os.WriteFile(marker, body, 0o644); err != nil {
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
