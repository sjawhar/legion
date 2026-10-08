// Package workspace provisions the one Jujutsu working copy for a Legion issue.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// CommandTimeout is the slow-command budget both runtimes' provisioning gives every command it
// runs — the shared clone's own clone, a fetch, a jj operation, or a git configuration edit —
// each bounded independently. The fetch's one clone is the exception, under FetchTimeout instead
// (below).
const CommandTimeout = 5 * time.Minute

// FetchTimeout is the outer bound for the fetch's clone: long enough for a large repository's
// slow but steadily progressing transfer to finish. Every other provisioning command keeps
// CommandTimeout: this widens the one command whose duration follows the repository's size and
// the network's speed, not a fixed step in provisioning.
const FetchTimeout = 30 * time.Minute

// Command is one process the provisioner runs, each bounded independently of every other: most
// hold the runner's own slow-command budget (RunChecked), and the fetch's clone holds a wider
// bound of its own instead (runCheckedTimeout, FetchTimeout) — never a budget shared across the
// whole provisioning sequence.
type Command struct {
	Argv    []string
	Env     []string
	Dir     string
	Timeout time.Duration
	// Clone is the shared clone whose workspace Dir is, for a jj command run in a workspace's own
	// directory (RunCheckedIn, which also names Dir with -R): the runner refuses one whose `.jj`
	// is missing or not a real directory, or whose `.jj/repo` names any other repository
	// (disarmLegacyConfig). "" for a command that opens the shared clone itself (-R, onClone) or
	// no repository.
	Clone string
}

// Result is the process result. A non-zero ExitCode, or TimedOut, is a process failure; a
// non-nil Run error means the process could not be started or observed.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	TimedOut bool
}

// Runner is injected so provisioning's exact argv, environment, directory, and timeout are
// testable without a network. Its Timeout is the daemon's slow-command budget.
type Runner interface {
	Timeout() time.Duration
	Run(context.Context, Command) (Result, error)
}

// Request contains the credential and identity of the repository to provision. Repo is the
// configured GitHub owner/repository name, for example "sjawhar/legion-smoke".
type Request struct {
	StateDir         string
	Repo             ghrepo.Repository
	Issue            string
	CredentialHelper string
	// Source is how the shared clone reaches the repository: FromFeed or FromGitHub.
	Source Source
	// Log takes the one line provisioning logs: the commits it set aside when it started a merged
	// issue's workspace at main (createWorkspace).
	Log func(line string)
}

// Workspace is the durable location and branch bookmark for one issue. Dir has the shape
// <state>/workspaces/<owner>/<repo>/<lowercase issue>; Clone, the shared clone every issue
// workspace of the repository is a jj workspace of, <state>/repos/github.com/<owner>/<repo>. A
// tree volume's init containers serialize on the file beside the clone, Clone + ".lock". Repo is
// the repository.
type Workspace struct {
	Dir      string
	Bookmark string
	Clone    string
	Repo     ghrepo.Repository
}

// In a pod, the one process that holds the provisioning token, Fetch, runs in a container that
// mounts nothing a tree agent can write, and every process that touches the tree volume runs in a
// container the provisioning Secret is not mounted in: that boundary, not what the runner adds
// below, is what keeps the token from a tree agent. On the tmux runtime the credentialed clone and
// fetch run in the shared clone, and panes share the daemon's uid and can read the daemon's files
// anyway, so there what the runner adds is defence, not a boundary: it holds the settings it names
// below, and does not claim that nothing else the shared clone's configuration names can run.

// pinnedGitConfig is git configuration every process provisioning starts reads last, after the
// shared clone's and after the command's own: no hook runs, wherever the clone's hooks directory
// or its core.hooksPath points. git would read GIT_CONFIG_PARAMETERS (its `-c`) after the pins, so
// no process provisioning starts inherits it.
var pinnedGitConfig = [][2]string{{"core.hooksPath", "/dev/null"}}

// transportEnvironment is set on every process provisioning starts before the command's own
// environment: git reaches a remote over https alone, so a url.<base>.insteadOf the tree wrote
// cannot turn a clone or fetch into an ext:: command, an ssh command, or a local path. A command
// that reaches a local repository on purpose — the shared clone's, from a pod's feed — names the
// file transport alone instead.
var transportEnvironment = []string{"GIT_ALLOW_PROTOCOL=https"}

type execRunner struct {
	timeout time.Duration
	// tools maps each command's name to the executable boot resolved for it.
	tools map[string]string
}

// NewRunner returns the production process runner. The supplied timeout is applied to every
// command separately, rather than bounding the whole provisioning sequence.
func NewRunner(timeout time.Duration, tools map[string]string) Runner {
	return execRunner{timeout: timeout, tools: tools}
}

func (r execRunner) Timeout() time.Duration {
	return r.timeout
}

func (r execRunner) Run(ctx context.Context, command Command) (Result, error) {
	if command.Timeout <= 0 {
		return Result{}, fmt.Errorf("workspace command has no timeout: %s", strings.Join(command.Argv, " "))
	}
	bounded, cancel := context.WithTimeout(ctx, command.Timeout)
	defer cancel()

	executable, ok := r.tools[command.Argv[0]]
	if !ok {
		return Result{}, fmt.Errorf("workspace command %s is not a tool the daemon resolved at boot", command.Argv[0])
	}
	args := command.Argv[1:]
	if command.Argv[0] == "jj" {
		guarded, err := guardWorkspace(jjWorkspaceRoot(command), command.Clone, true)
		if err != nil {
			return Result{}, err
		}
		if err := disarmLegacyConfig(guarded.jjDir, guarded.repo); err != nil {
			return Result{}, err
		}
		// jj starts the git its configuration names, and a tree agent can still set that where
		// disarmLegacyConfig does not reach: on the tmux runtime a pane shares the config home jj
		// keeps a repository's configuration in. A --config flag outranks every configuration
		// file, so jj starts the git boot resolved.
		git, ok := r.tools["git"]
		if !ok {
			return Result{}, errors.New("workspace command jj needs the git the daemon resolved at boot")
		}
		args = append([]string{"--config=git.executable-path=" + tomlString(git)}, args...)
	}
	child := exec.CommandContext(bounded, executable, args...)
	child.Dir = command.Dir
	env, err := pinGitConfig(without(merge(merge(runtime.WithoutNATSSeeds(os.Environ()), transportEnvironment), command.Env), "GIT_CONFIG_PARAMETERS"))
	if err != nil {
		return Result{}, err
	}
	child.Env = env
	// A command that reaches a remote over https can have git spawn git-remote-https, a helper
	// holding the same stdout/stderr pipes: exec.CommandContext alone only kills the direct child
	// when bounded expires, and that helper, now reparented, can keep the pipe open forever,
	// leaving Wait (and so Run) never returning. procgroup.Configure's Setpgid plus Cancel signals
	// the whole process group instead, killing the helper too; after that, Wait normally returns
	// an ordinary *exec.ExitError for the signaled process.
	procgroup.Configure(child)
	var stdout, stderr bytes.Buffer
	child.Stdout = &stdout
	child.Stderr = &stderr
	err = child.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if runErr := procgroup.Err(err); runErr != nil {
		return result, runErr
	}
	result.ExitCode = child.ProcessState.ExitCode()
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		result.TimedOut = errors.Is(bounded.Err(), context.DeadlineExceeded)
	}
	return result, nil
}

// guardedWorkspace is what a jj command has opened after guardWorkspace held each untrusted path
// to the Location layout. disarmLegacyConfig only removes the legacy files jj would migrate.
type guardedWorkspace struct {
	jjDir string
	repo  string
}

// guardWorkspace holds every refusal a jj command can reach, in order: the workspace and clone
// layout components; the .jj a workspace command opens; the shared clone's .jj and .jj/repo; and
// the workspace's .jj/repo pointer. Callers that will write a workspace path but not yet open jj
// (Provision, Remove and RemoveFinished) pass requireJJ false: the same layout guard refuses a
// symlinked target before it creates, renames or removes anything outside the layout.
//
// jj 0.38 and later keep a repository's and a workspace's own configuration in the config home,
// under the id an id file inside `.jj` names. While that id is missing, jj migrates the legacy file
// beside it into the config home and reads it (lib/src/secure_config.rs, maybe_load_config and
// maybe_migrate_legacy_config; automatic until jj 0.49). Those are a workspace's
// `.jj/workspace-config.toml` beside `.jj/workspace-config-id`, and its repository's
// `config.toml` beside `config-id`. Removing the legacy file leaves jj to open an empty
// configuration of its own, never one a tree agent wrote. The worker image's jj is 0.45; the tmux
// daemon refuses a host jj older than 0.38 (internal/daemon's resolveTools).
//
// A workspace with no `.jj` is refused when requireJJ and clone are set: it was provisioned with
// one, and without it a jj command lacking -R walks up to an ancestor holding a `.jj`
// (cli_util.rs, find_workspace_dir). RunCheckedIn names its workspace with -R, under which jj
// never walks up; the refusal names the missing `.jj` before jj runs at all. A missing `.jj` with
// no clone (a command that opens the clone itself with -R, or `jj git clone`, which opens none) is
// left for jj.
//
// This holds against a tree-written path present before the command begins. A write in the instant
// between this guard and jj's own read is outside it: a hostile role already has every sibling
// workspace on the volume to delete directly.
func guardWorkspace(root, clone string, requireJJ bool) (guardedWorkspace, error) {
	if root == "" {
		return guardedWorkspace{}, nil
	}
	for _, dir := range []string{root, clone} {
		if err := refuseSymlinkedLayout(root, dir); err != nil {
			return guardedWorkspace{}, err
		}
	}
	if !requireJJ {
		return guardedWorkspace{}, nil
	}
	jjDir := filepath.Join(root, ".jj")
	info, err := os.Lstat(jjDir)
	if errors.Is(err, fs.ErrNotExist) && clone == "" {
		return guardedWorkspace{}, nil
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return guardedWorkspace{}, fmt.Errorf("refusing to run jj in %s: it has no .jj of its own, which a workspace of the shared clone %s always has", root, clone)
	case err != nil || !info.IsDir():
		return guardedWorkspace{}, fmt.Errorf("refusing to run jj in %s: its .jj is not a real directory (%v)", root, describeEntry(info, err))
	}
	repo, err := sharedRepository(jjDir, clone)
	if err != nil {
		return guardedWorkspace{}, err
	}
	return guardedWorkspace{jjDir: jjDir, repo: repo}, nil
}

// disarmLegacyConfig removes each legacy configuration file jj would migrate. guardWorkspace has
// already held jjDir and repo to a real workspace and the shared clone, respectively. An empty
// jjDir is a command that opens no workspace yet (jj git clone): it is an invariant that disarming
// only operates on a path the guard actually resolved, never a relative path that falls through to
// the daemon's current working directory.
func disarmLegacyConfig(jjDir, repo string) error {
	if jjDir == "" {
		return nil
	}
	legacy := map[string][2]string{
		jjDir: {"workspace-config-id", "workspace-config.toml"},
		repo:  {"config-id", "config.toml"},
	}
	for dir, names := range legacy {
		if _, err := os.Stat(filepath.Join(dir, names[0])); !errors.Is(err, fs.ErrNotExist) {
			continue
		}
		file := filepath.Join(dir, names[1])
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s, which jj would migrate into the configuration it reads: %w", file, err)
		}
	}
	return nil
}

// refuseSymlinkedLayout refuses an operation at root when dir, or any of the directories above it
// up to the state directory (layoutDepth of them in all), is a symlink, naming it. Provisioning
// creates every one as a real directory; a symlink there would make jj open, and workspace
// creation, removal or the legacy-file disarm write into, a copy outside the workspace layout
// below the state directory. The state directory itself is not checked: an operator may reach it
// through a symlink. A component that does not exist is no symlink, so it is passed over.
func refuseSymlinkedLayout(root, dir string) error {
	for range layoutDepth {
		if dir == "" || dir == filepath.Dir(dir) {
			return nil
		}
		info, err := os.Lstat(dir)
		switch {
		case err == nil && info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("refusing workspace operation in %s: %s is a symlink, and no directory of the workspace layout below the state directory is one", root, dir)
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("refusing workspace operation in %s: %w", root, err)
		}
		dir = filepath.Dir(dir)
	}
	return nil
}

// sharedRepository is the repository directory jj opens for the workspace whose `.jj` is jjDir,
// held to the shared clone's own `.jj/repo`: clone's when the command names one (a workspace of
// it), else the root's own (the command opens the clone itself). The clone's `.jj` and its
// `.jj/repo` must each be a real directory, not a symlink or a file. A workspace names its
// repository in its `.jj/repo` file, which a tree agent can rewrite. The path that file names is
// built as jj builds it (workspaceRepository, never cleaned) and stat'ed by the kernel, which
// resolves it physically as jj's canonicalize does; it must be the same directory as the clone's
// own (os.SameFile), or the command is refused, naming both, before jj opens a repository Legion
// did not provision and before anything there is removed.
func sharedRepository(jjDir, clone string) (string, error) {
	own := filepath.Join(jjDir, "repo")
	if clone != "" {
		cloneJJ := filepath.Join(clone, ".jj")
		if info, err := os.Lstat(cloneJJ); err != nil || !info.IsDir() {
			return "", fmt.Errorf("refusing to run jj in %s: %s is not the shared clone's own .jj directory (%v)", filepath.Dir(jjDir), cloneJJ, describeEntry(info, err))
		}
		own = filepath.Join(cloneJJ, "repo")
	}
	ownInfo, err := os.Lstat(own)
	if err != nil || !ownInfo.IsDir() {
		return "", fmt.Errorf("refusing to run jj in %s: %s is not the shared clone's own repository directory (%v)", filepath.Dir(jjDir), own, describeEntry(ownInfo, err))
	}
	if clone == "" {
		return own, nil
	}
	expected, err := filepath.EvalSymlinks(own)
	if err != nil {
		return "", fmt.Errorf("refusing to run jj in %s: resolve the shared clone's %s: %w", filepath.Dir(jjDir), own, err)
	}
	target, err := workspaceRepository(jjDir)
	if err != nil {
		return "", fmt.Errorf("refusing to run jj in %s: read its .jj/repo: %w", filepath.Dir(jjDir), err)
	}
	if opened, err := os.Stat(target); err != nil || !os.SameFile(opened, ownInfo) {
		named := target
		if resolved, err := filepath.EvalSymlinks(target); err == nil {
			named = resolved
		}
		return "", fmt.Errorf("refusing to run jj in %s: its .jj/repo names %s, not the shared clone's %s", filepath.Dir(jjDir), named, expected)
	}
	return expected, nil
}

// workspaceRepository is the path jj opens as the repository of the workspace whose `.jj` is
// jjDir, built as jj 0.45's DefaultWorkspaceLoader builds it (lib/src/workspace.rs): `.jj/repo`
// itself unless it is a regular file, else the path that file holds, appended to `.jj` unless it
// is absolute. It is never cleaned: jj canonicalizes it physically, each symlink resolved before a
// `..` after it, and filepath.Join would cancel `link/..` before anything resolved `link`, naming
// a different directory than the one jj opens. A `.jj/repo` that is neither a directory nor a
// regular file (a FIFO, a socket, a device) is refused before anything reads it: reading a FIFO
// blocks, and nothing here runs under the command's deadline.
func workspaceRepository(jjDir string) (string, error) {
	repo := filepath.Join(jjDir, "repo")
	info, err := os.Stat(repo)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return repo, nil
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s is neither a directory nor a regular file (mode %s)", repo, info.Mode().Type())
	}
	pointer, err := os.ReadFile(repo)
	if err != nil {
		return "", err
	}
	if target := string(pointer); filepath.IsAbs(target) {
		return target, nil
	}
	return jjDir + string(filepath.Separator) + string(pointer), nil
}

// describeEntry says what a path that should be a directory is instead.
func describeEntry(info fs.FileInfo, err error) string {
	switch {
	case err != nil:
		return err.Error()
	case info.Mode()&fs.ModeSymlink != 0:
		return "a symlink"
	default:
		return "not a directory"
	}
}

// jjWorkspaceRoot is the workspace a jj command opens: the value of its last -R (or
// --repository) flag, else the directory it runs in, which every caller in this package names as
// the workspace root itself.
func jjWorkspaceRoot(command Command) string {
	root := command.Dir
	for i, arg := range command.Argv {
		switch {
		case (arg == "-R" || arg == "--repository") && i+1 < len(command.Argv):
			root = command.Argv[i+1]
		case strings.HasPrefix(arg, "--repository="):
			root = strings.TrimPrefix(arg, "--repository=")
		}
	}
	return root
}

// merge is base with each override entry replacing the entry of the same name, or appended.
func merge(base, overrides []string) []string {
	environment := append([]string(nil), base...)
	positions := make(map[string]int, len(environment))
	for index, entry := range environment {
		positions[environmentKey(entry)] = index
	}
	for _, entry := range overrides {
		key := environmentKey(entry)
		if index, found := positions[key]; found {
			environment[index] = entry
			continue
		}
		positions[key] = len(environment)
		environment = append(environment, entry)
	}
	return environment
}

// without is environment with no entry named key.
func without(environment []string, key string) []string {
	return slices.DeleteFunc(environment, func(entry string) bool { return environmentKey(entry) == key })
}

// pinGitConfig appends pinnedGitConfig after the GIT_CONFIG_COUNT/GIT_CONFIG_KEY_n/
// GIT_CONFIG_VALUE_n pairs environment already carries, so git reads the pins after every other
// entry. They travel in the environment rather than as -c flags because jj, not this code, starts
// the git that clones and fetches.
func pinGitConfig(environment []string) ([]string, error) {
	count := 0
	for _, entry := range environment {
		if value, ok := strings.CutPrefix(entry, "GIT_CONFIG_COUNT="); ok {
			parsed, err := strconv.Atoi(value)
			if err != nil || parsed < 0 {
				return nil, fmt.Errorf("GIT_CONFIG_COUNT is %q, not a count of configuration pairs", value)
			}
			count = parsed
		}
	}
	pins := []string{"GIT_CONFIG_COUNT=" + strconv.Itoa(count+len(pinnedGitConfig))}
	for offset, pair := range pinnedGitConfig {
		index := strconv.Itoa(count + offset)
		pins = append(pins, "GIT_CONFIG_KEY_"+index+"="+pair[0], "GIT_CONFIG_VALUE_"+index+"="+pair[1])
	}
	return merge(environment, pins), nil
}

func environmentKey(entry string) string {
	key, _, _ := strings.Cut(entry, "=")
	return key
}

// tomlString is value as one TOML basic string, the form jj reads a --config value in.
func tomlString(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// runCommand runs argv with the runner's own slow-command budget: run is nil-checked before
// run.Timeout() is ever read, so a nil Runner always returns the error below rather than a panic.
func runCommand(ctx context.Context, run Runner, argv []string, env []string, dir string) (Result, error) {
	if run == nil {
		return Result{}, errors.New("workspace runner is required")
	}
	return runCommandTimeout(ctx, run, argv, env, dir, run.Timeout())
}

// runCommandTimeout is runCommand, but for timeout instead of the runner's own slow-command
// budget: runCommand itself calls it with run.Timeout(); runCheckedTimeout calls it with an
// explicit value instead, for the fetch's clone, that path's one caller (see FetchTimeout for why
// it needs one).
func runCommandTimeout(ctx context.Context, run Runner, argv []string, env []string, dir string, timeout time.Duration) (Result, error) {
	if run == nil {
		return Result{}, errors.New("workspace runner is required")
	}
	if timeout <= 0 {
		return Result{}, errors.New("workspace runner timeout must be positive")
	}
	return run.Run(ctx, Command{Argv: argv, Env: env, Dir: dir, Timeout: timeout})
}

// RunChecked runs argv with the runner's budget, env over the process environment, in dir: a
// process that could not start, exited non-zero, or outlived the budget is an error naming the
// command.
func RunChecked(ctx context.Context, run Runner, argv []string, env []string, dir string) (Result, error) {
	result, err := runCommand(ctx, run, argv, env, dir)
	return checkedResult(argv, result, err)
}

// RunCheckedIn is RunChecked for a jj command run in ws's own workspace. It names the workspace
// with -R, so jj opens ws.Dir itself and never searches its parent directories for a `.jj`, and
// names ws's shared clone so the runner can hold the workspace's `.jj` and `.jj/repo` to it
// (Command.Clone, guardWorkspace). Both Location paths must be non-empty: otherwise -R "" is
// exactly the rootless command an empty guard cannot bind to a workspace.
func RunCheckedIn(ctx context.Context, run Runner, ws Workspace, argv []string) (Result, error) {
	if run == nil {
		return Result{}, errors.New("workspace runner is required")
	}
	if ws.Dir == "" {
		return Result{}, errors.New("workspace directory is required")
	}
	if ws.Clone == "" {
		return Result{}, errors.New("workspace clone directory is required")
	}
	argv = append(slices.Clip(argv), "-R", ws.Dir)
	result, err := run.Run(ctx, Command{Argv: argv, Dir: ws.Dir, Clone: ws.Clone, Timeout: run.Timeout()})
	return checkedResult(argv, result, err)
}

// runCheckedTimeout is RunChecked, but for timeout instead of the runner's own slow-command
// budget (see FetchTimeout).
func runCheckedTimeout(ctx context.Context, run Runner, argv []string, env []string, dir string, timeout time.Duration) (Result, error) {
	result, err := runCommandTimeout(ctx, run, argv, env, dir, timeout)
	return checkedResult(argv, result, err)
}

// checkedResult is RunChecked's and runCheckedTimeout's shared answer: a process that could not
// start, exited non-zero, or outlived its budget is an error naming the command. A clean exit
// whose I/O draining outlived WaitDelay (exec.ErrWaitDelay) is not this: execRunner.Run already
// reads that as the process's own true ExitCode, so it passes through the same as any other
// answer.
func checkedResult(argv []string, result Result, err error) (Result, error) {
	if err != nil {
		return Result{}, fmt.Errorf("run %s: %w", strings.Join(argv, " "), err)
	}
	if result.ExitCode != 0 || result.TimedOut {
		return Result{}, commandFailure(argv, result)
	}
	return result, nil
}

func commandFailure(argv []string, result Result) error {
	command := strings.Join(argv, " ")
	if result.TimedOut {
		return fmt.Errorf("command timed out: %s\n%s", command, strings.TrimSpace(result.Stderr))
	}
	return fmt.Errorf("command failed (exit %d): %s\n%s", result.ExitCode, command, strings.TrimSpace(result.Stderr))
}

// onClone is a jj command against the shared clone: never a snapshot of its working copy, which
// would run the working-copy filter, fsmonitor and signing programs a tree agent can configure,
// and never colored, so every read parses. `jj workspace add` is the one command on the clone jj
// refuses --ignore-working-copy on (createWorkspace). cloneDir must be non-empty, so it never
// builds -R "" — jjWorkspaceRoot treats that exactly as no -R.
func onClone(cloneDir string, args ...string) ([]string, error) {
	if cloneDir == "" {
		return nil, errors.New("workspace clone directory is required")
	}
	return append(append([]string{"jj"}, args...), "--ignore-working-copy", "--color=never", "-R", cloneDir), nil
}

// runCheckedOnClone is RunChecked for an onClone command, including its empty-clone refusal.
func runCheckedOnClone(ctx context.Context, run Runner, cloneDir string, args ...string) (Result, error) {
	argv, err := onClone(cloneDir, args...)
	if err != nil {
		return Result{}, err
	}
	return RunChecked(ctx, run, argv, nil, "")
}

func ensureFetchConfiguration(ctx context.Context, run Runner, cloneDir string, source remote) error {
	setting, err := runCheckedOnClone(ctx, run, cloneDir, "config", "get", "git.abandon-unreachable-commits")
	if err != nil {
		return err
	}
	if strings.TrimSpace(setting.Stdout) != "false" {
		if _, err := runCheckedOnClone(ctx, run, cloneDir, "config", "set", "--repo", "git.abandon-unreachable-commits", "false"); err != nil {
			return err
		}
	}
	// The fetch takes no snapshot of the clone's working copy: a snapshot runs the working-copy
	// filter, fsmonitor, and signing programs jj's configuration names, which a tree agent can set,
	// and on the tmux runtime this fetch holds the one-shot credential.
	fetch := []string{"git", "fetch"}
	for _, branch := range source.branches {
		fetch = append(fetch, "--branch", "exact:"+branch)
	}
	argv, err := onClone(cloneDir, fetch...)
	if err != nil {
		return err
	}
	_, err = RunChecked(ctx, run, argv, source.env, "")
	return err
}

// GitHubCredentialHelper is the `credential.helper` every shared clone gets: `gh auth
// git-credential`, the helper `gh auth setup-git` itself writes, which answers git from the
// hosts.yml under the process's GH_CONFIG_DIR (ghconfig.HostsFile). The `gh` in it is resolved on
// PATH when git runs it, so the one it reaches is the process's own, and the value names no path
// of this machine's that the clone would carry to another.
const GitHubCredentialHelper = "!gh auth git-credential"

// ConfigureRepositoryCredential writes GitHubCredentialHelper as the shared clone's helper
// (configureRepositoryCredential): the daemon runs it at boot over a clone an earlier daemon
// provisioned, whose helper still names that daemon's `legion credential`, so a pane's git answers
// from its GH_CONFIG_DIR as a clone provisioned now does.
func ConfigureRepositoryCredential(ctx context.Context, run Runner, cloneDir string) error {
	return configureRepositoryCredential(ctx, run, cloneDir, GitHubCredentialHelper)
}

// configureRepositoryCredential keeps the clone's persisted helper for worker panes after the
// one-shot clone/fetch environment has been removed: the clone's own credential.helper, and the
// one for github.com, each reset and then set to credentialHelper, with credential.interactive
// off so a helper that answers nothing fails rather than prompts. This ports the helper writes in
// workspace.ts's provisionIssueWorkspace.
func configureRepositoryCredential(ctx context.Context, run Runner, cloneDir, credentialHelper string) error {
	gitDir := cloneDir + "/.git"
	for _, argv := range [][]string{
		{"git", "--git-dir=" + gitDir, "config", "--replace-all", "credential.helper", ""},
		{"git", "--git-dir=" + gitDir, "config", "--add", "credential.helper", credentialHelper},
		{"git", "--git-dir=" + gitDir, "config", "--replace-all", "credential.https://github.com.helper", ""},
		{"git", "--git-dir=" + gitDir, "config", "--add", "credential.https://github.com.helper", credentialHelper},
		{"git", "--git-dir=" + gitDir, "config", "credential.interactive", "false"},
	} {
		if _, err := RunChecked(ctx, run, argv, nil, ""); err != nil {
			return err
		}
	}
	return nil
}

// removeRepositoryIdentity ports workspace.ts's removeRepoScopedIdentity. A per-repository
// identity would be shared by every issue workspace, while pane identity is deliberately provided
// through the pane's env.
func removeRepositoryIdentity(ctx context.Context, run Runner, cloneDir string) error {
	for _, key := range []string{"user.name", "user.email"} {
		probe, err := onClone(cloneDir, "config", "list", "--repo", "--include-overridden", key)
		if err != nil {
			return err
		}
		present, err := RunChecked(ctx, run, probe, nil, "")
		if err != nil {
			return err
		}
		if strings.TrimSpace(present.Stdout) == "" {
			continue
		}
		unset, err := onClone(cloneDir, "config", "unset", "--repo", key)
		if err != nil {
			return err
		}
		removed, err := runCommand(ctx, run, unset, nil, "")
		if err != nil {
			return fmt.Errorf("run %s: %w", strings.Join(unset, " "), err)
		}
		if removed.ExitCode == 0 {
			continue
		}
		rechecked, err := RunChecked(ctx, run, probe, nil, "")
		if err == nil && strings.TrimSpace(rechecked.Stdout) == "" {
			continue
		}
		return commandFailure(unset, removed)
	}
	return nil
}
