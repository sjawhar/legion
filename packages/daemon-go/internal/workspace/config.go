// Package workspace provisions the one Jujutsu working copy for a Legion issue.
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

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
	Token            string
	CredentialHelper string
	// CredentialDir is where the one-shot clone and fetch credential, token file included, is
	// created and removed again; empty means StateDir. A pod's init container names its own
	// filesystem, because its StateDir is the tree volume every container of the tree mounts.
	CredentialDir string
}

// Workspace is the durable location and branch bookmark for one issue. Dir has the shape
// <state>/workspaces/<owner>/<repo>/<lowercase issue>.
type Workspace struct {
	Dir      string
	Bookmark string
}

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
	child := exec.CommandContext(bounded, executable, command.Argv[1:]...)
	child.Dir = command.Dir
	child.Env = mergeEnvironment(command.Env)
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
		result.TimedOut = errors.Is(bounded.Err(), context.DeadlineExceeded)
		return result, nil
	}
	return result, err
}

func mergeEnvironment(overrides []string) []string {
	environment := os.Environ()
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

func environmentKey(entry string) string {
	key, _, _ := strings.Cut(entry, "=")
	return key
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

func runChecked(ctx context.Context, run Runner, argv []string, env []string, dir string) (Result, error) {
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

func ensureFetchConfiguration(ctx context.Context, run Runner, cloneDir string, credentialEnv []string) error {
	setting, err := runChecked(ctx, run, []string{
		"jj", "config", "get", "git.abandon-unreachable-commits", "-R", cloneDir,
	}, nil, "")
	if err != nil {
		return err
	}
	if strings.TrimSpace(setting.Stdout) != "false" {
		if _, err := runChecked(ctx, run, []string{
			"jj", "config", "set", "--repo", "git.abandon-unreachable-commits", "false", "-R", cloneDir,
		}, nil, ""); err != nil {
			return err
		}
	}
	_, err = runChecked(ctx, run, []string{"jj", "git", "fetch", "-R", cloneDir}, credentialEnv, "")
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
		if _, err := runChecked(ctx, run, argv, nil, ""); err != nil {
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
		present, err := runChecked(ctx, run, probe, nil, "")
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
		rechecked, err := runChecked(ctx, run, probe, nil, "")
		if err == nil && strings.TrimSpace(rechecked.Stdout) == "" {
			continue
		}
		return commandFailure(unset, removed)
	}
	return nil
}
