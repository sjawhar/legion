package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	legionclaim "github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workspaceProvisionUsage = "legion workspace-init provision --issue <KEY> --repo <owner>/<repo> --default-branch <name> [--root /legion] --credential-helper <git helper> --feed <dir>"

// provisionTokenFileEnv points `workspace-init fetch` at the mounted provisioning token.
const provisionTokenFileEnv = "LEGION_PROVISION_TOKEN_FILE"

// secretFileFromEnv is the secret file the environment variable name points at, once it reads
// (config.ReadSecretPointer); an error when name is not set or its file does not read.
func secretFileFromEnv(name string) (string, error) {
	file, set := os.LookupEnv(name)
	if !set {
		return "", errors.New(name + " is not set")
	}
	if _, err := config.ReadSecretPointer(name, file); err != nil {
		return "", err
	}
	return file, nil
}

// workspaceInitCommands is `legion workspace-init`, the Kubernetes runtime's init containers, which
// prepare a pod's disk before the main container's worker-shim starts, and the repository cache's
// process, and the one list of them `legion workspace-init --help` names. An issue pod runs two,
// which prepare the issue's clone and jj workspace on the pod's own disk: `fetch` is the first, the
// one process of the pod that holds the provisioning token, in a container that mounts nothing an
// agent of the issue can write; `provision` is the second, all the disk's work, in a container the
// provisioning Secret is not mounted in. The controller's pod (`controller: daemon`) runs
// `controller` alone, on the volume its own Sandbox owns. A repository cache's pod runs `cache` as
// its one container, on its own volume, until it is stopped. Each one's log lines go to stdout and
// its refusals and failures to stderr — together the init log the runtime quotes — with exit 1.
var workspaceInitCommands = map[string]command{
	"fetch":      runWorkspaceFetch,
	"provision":  runWorkspaceProvision,
	"controller": runWorkspaceController,
	"cache":      runWorkspaceCache,
}

func runWorkspaceInit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runSubcommand(ctx, "workspace-init", workspaceInitCommands, args, stdout, stderr)
}

func runWorkspaceFetch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("workspace-init fetch", "usage: "+workspaceFetchUsage, stderr)
	repo := flags.String("repo", "", "repository as <owner>/<name> (required)")
	feed := flags.String("feed", "", "the pod's feed directory, the container's own (required)")
	cacheURL := flags.String("cache-url", os.Getenv(cacheURLEnv), "the repository cache to fill the feed from before GitHub (default $"+cacheURLEnv+", set by the runtime while the cache is ready)")
	if code, ok := parseWorkspaceInitFlags(flags, args, stderr); !ok {
		return code
	}
	return workspaceInitExit(flags, workspaceFetch(ctx, *repo, *feed, *cacheURL, stdout), stderr)
}

func runWorkspaceProvision(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("workspace-init provision", "usage: "+workspaceProvisionUsage, stderr)
	issue := flags.String("issue", "", "Dispatch issue key, e.g. LEGION-1 (required)")
	repo := flags.String("repo", "", "repository as <owner>/<name> (required)")
	defaultBranch := flags.String("default-branch", "", "the repository's default branch as GitHub names it, where an issue with no branch starts (required)")
	root := flags.String("root", "/legion", "issue volume root directory")
	credentialHelper := flags.String("credential-helper", "", "git credential helper written into the clone's config (required)")
	feed := flags.String("feed", "", "the pod's feed directory, which workspace-init fetch filled (required)")
	if code, ok := parseWorkspaceInitFlags(flags, args, stderr); !ok {
		return code
	}
	return workspaceInitExit(flags, workspaceInit(ctx, *issue, *repo, *defaultBranch, *root, *credentialHelper, *feed, stdout), stderr)
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
// GitHub credential, so it gets no workspace and no gh files. It holds the controller to no
// session: every session is a row in the session database, which the role launcher looks up
// before it starts the child (internal/launcher).
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
// with 1.
func workspaceInitExit(flags *flag.FlagSet, err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	fmt.Fprintf(stderr, "%s: %v\n", flags.Name(), err)
	return 1
}

// workspaceInit validates everything before it touches the disk, --repo first, and refuses to
// run where the provisioning token is pointed at: this is the process that runs git and jj against
// what every agent of the issue can write. Then it creates the directories the main container
// mounts, removes the git locks an interrupted git or jj left, and provisions from the feed, an
// issue with no branch at defaultBranch. No lock guards the clone: it is the issue's own, on the
// pod's own disk, and no other pod mounts it. For the same reason, and because no role container
// of the pod has started, no process holds a git lock on the disk while this runs, so every one
// there is stale and would fail each later write of what it locks (LEGION-731) — the kubelet
// restarts a failed init container on the same pod, and so on the same disk, so a first attempt
// killed mid-fetch leaves exactly those locks for the retry to clear.
func workspaceInit(ctx context.Context, issue, repo, defaultBranch, root, credentialHelper, feed string, stdout io.Writer) error {
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
	if defaultBranch == "" {
		return fmt.Errorf("--default-branch is required: %s", workspaceProvisionUsage)
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

	if err := os.MkdirAll(filepath.Join(root, "sessions"), 0o700); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Join(root, "sessions"), err)
	}

	log := func(line string) { fmt.Fprintln(stdout, "workspace-init: "+line) }
	if err := workspace.RemoveStaleGitLocks(located, time.Now(), log); err != nil {
		return err
	}
	run := workspace.NewRunner(workspace.CommandTimeout, tools)
	provisioned, err := workspace.Provision(ctx, run, workspace.Request{
		StateDir: root, Repo: repository, Issue: issue, DefaultBranch: defaultBranch, CredentialHelper: credentialHelper,
		Source: workspace.FromFeed(feed), Log: log,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "workspace-init: %s on %s from %s\n", provisioned.Dir, provisioned.Bookmark, provisioned.DefaultBranch)
	return nil
}

// provisioningTools resolves the named tools a provisioning step runs from PATH — in an init
// container the image's, with no operator rc ahead of them — where the
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
