// Package workspace provisions the one Jujutsu working copy for a Legion issue.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CommandTimeout is the slow-command budget both runtimes' provisioning gives every command it
// runs — a clone, a fetch, a jj operation, or a git configuration edit — each bounded
// independently.
const CommandTimeout = 5 * time.Minute

// Command is one process the provisioner runs. Every command holds the runner's slow-command
// budget so a clone, fetch, jj operation, or git configuration edit is bounded independently.
type Command struct {
	Argv    []string
	Env     []string
	Dir     string
	Timeout time.Duration
}

// Result is the process result. A non-zero ExitCode is a process failure; a non-nil Run error
// means the process could not be started or observed.
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
	Repo             string
	Issue            string
	CredentialHelper string
	// Feed is a pod's feed directory, where Fetch cloned the repository in the pod's first init
	// container: the shared clone clones and fetches from it with no credential, and Token and
	// CredentialDir stay empty. With no Feed — the tmux runtime — the shared clone clones and
	// fetches from GitHub itself with Token.
	Feed  string
	Token string
	// CredentialDir is where the one-shot clone and fetch credential, token file included, is
	// created and removed again; required with Token. The tmux daemon names its state directory.
	CredentialDir string
}

// Workspace is the durable location and branch bookmark for one issue. Dir has the shape
// <state>/workspaces/<owner>/<repo>/<lowercase issue>; Clone, the shared clone every issue
// workspace of the repository is a jj workspace of, <state>/repos/github.com/<owner>/<repo>. A
// tree volume's init containers serialize on the file beside the clone, Clone + ".lock".
type Workspace struct {
	Dir      string
	Bookmark string
	Clone    string
}

// In a pod, the one process that holds the provisioning token, Fetch, runs in a container that
// mounts nothing a tree agent can write, and every process that touches the tree volume runs in a
// container the provisioning Secret is not mounted in: that boundary, not what the runner adds
// below, is what keeps the token from a tree agent. On the tmux runtime the credentialed clone and
// fetch run in the shared clone, and panes share the daemon's uid and can read the daemon's files
// anyway, so there what the runner adds is defence, not a boundary: it keeps provisioning from
// running anything a tree agent configured in the shared clone.

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
		// jj starts the git its configuration names, and a tree agent can set that: jj migrates a
		// .jj/workspace-config.toml it finds in a working copy into the configuration it reads. A
		// --config flag outranks every configuration file, so jj starts the git boot resolved.
		git, ok := r.tools["git"]
		if !ok {
			return Result{}, errors.New("workspace command jj needs the git the daemon resolved at boot")
		}
		args = append([]string{"--config=git.executable-path=" + tomlString(git)}, args...)
	}
	child := exec.CommandContext(bounded, executable, args...)
	child.Dir = command.Dir
	env, err := pinGitConfig(without(merge(merge(os.Environ(), transportEnvironment), command.Env), "GIT_CONFIG_PARAMETERS"))
	if err != nil {
		return Result{}, err
	}
	child.Env = env
	var stdout, stderr bytes.Buffer
	child.Stdout = &stdout
	child.Stderr = &stderr
	err = child.Run()
	result := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) {
		result.ExitCode = exited.ExitCode()
		result.TimedOut = errors.Is(bounded.Err(), context.DeadlineExceeded)
		return result, nil
	}
	return result, err
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

func runCommand(ctx context.Context, run Runner, argv []string, env []string, dir string) (Result, error) {
	if run == nil {
		return Result{}, errors.New("workspace runner is required")
	}
	timeout := run.Timeout()
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
	if err != nil {
		return Result{}, fmt.Errorf("run %s: %w", strings.Join(argv, " "), err)
	}
	if result.ExitCode != 0 {
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

func ensureFetchConfiguration(ctx context.Context, run Runner, cloneDir string, remoteEnv []string) error {
	setting, err := RunChecked(ctx, run, []string{
		"jj", "config", "get", "git.abandon-unreachable-commits", "-R", cloneDir,
	}, nil, "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(setting.Stdout) != "false" {
		if _, err := RunChecked(ctx, run, []string{
			"jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false", "-R", cloneDir,
		}, nil, ""); err != nil {
			return err
		}
	}
	// The fetch takes no snapshot of the clone's working copy: a snapshot runs the working-copy
	// filter, fsmonitor, and signing programs jj's configuration names, which a tree agent can set,
	// and on the tmux runtime this fetch holds the one-shot credential.
	_, err = RunChecked(ctx, run, []string{"jj", "git", "fetch", "--ignore-working-copy", "-R", cloneDir}, remoteEnv, "")
	return err
}

// configureRepositoryCredential keeps the clone's persisted helper for worker panes after the
// one-shot clone/fetch environment has been removed. This ports workspace.ts:474-512.
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

// removeRepositoryIdentity ports workspace.ts:298-338. A per-repository identity would be shared
// by every issue workspace, while pane identity is deliberately provided through the pane's env.
func removeRepositoryIdentity(ctx context.Context, run Runner, cloneDir string) error {
	for _, key := range []string{"user.name", "user.email"} {
		probe := []string{"jj", "config", "list", "--repo", "--include-overridden", "-R", cloneDir, key}
		present, err := RunChecked(ctx, run, probe, nil, "")
		if err != nil {
			return err
		}
		if strings.TrimSpace(present.Stdout) == "" {
			continue
		}
		unset := []string{"jj", "config", "unset", "--repo", "-R", cloneDir, key}
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
