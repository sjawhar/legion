package ghtool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/ghroute"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// env is the whole environment a call's gh runs with, configDir its GH_CONFIG_DIR: PATH of gh's
// and git's directories, HOME and the XDG directories at the answer's empty ones, the wrapper
// GH_PATH names, gh's prompts, update check, spinner and colour off, and nothing of the daemon's
// own environment.
func (a *Answer) env(configDir string) []string {
	path := filepath.Dir(a.host.GH)
	if a.host.Git != "" && filepath.Dir(a.host.Git) != path {
		path += string(filepath.ListSeparator) + filepath.Dir(a.host.Git)
	}
	return []string{
		"PATH=" + path,
		"HOME=" + a.home,
		"XDG_CONFIG_HOME=" + a.config,
		"XDG_CACHE_HOME=" + a.cache,
		"XDG_DATA_HOME=" + a.data,
		"XDG_STATE_HOME=" + a.state,
		"GH_CONFIG_DIR=" + configDir,
		"GH_PATH=" + a.wrapper,
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_SPINNER_DISABLED=1",
		"NO_COLOR=1",
	}
}

// route reads the owner argv targets as a pane's `legion gh` reads it (ghroute.TargetOf, from the
// real gh's own help, read under the call's environment with the issue owner's directory, as a
// pane reads it), and answers the target, the owner the call runs as and that owner's gh directory:
// the bundle's default, the issue's owner, for a command naming none. The working directory is
// the answer's empty one, which has no git remotes. A command TargetOf refuses, and one naming an
// owner the bundle holds no directory for, are refused.
func (a *Answer) route(ctx context.Context, argv []string) (ghroute.Target, string, string, error) {
	env := a.env(ghconfig.OwnerDir(a.owners, a.bundle.Default))
	vars := make(map[string]string, len(env))
	for _, pair := range env {
		name, value, _ := strings.Cut(pair, "=")
		vars[name] = value
	}
	noRemotes := func() ([]ghroute.Remote, error) { return nil, nil }
	target, err := ghroute.TargetOf(argv, vars, a.help(ctx, env), noRemotes)
	if err != nil {
		return ghroute.Target{}, "", "", err
	}
	owner := target.Owner
	if owner == "" {
		owner = a.bundle.Default
	}
	if _, ok := a.bundle.Owners[owner]; !ok {
		return ghroute.Target{}, "", "", fmt.Errorf("%q is not an owner of this project: the agent's credentials hold no gh directory for it", owner)
	}
	return target, owner, ghconfig.OwnerDir(a.owners, owner), nil
}

// help is the real gh's help of a command path, `gh <path> --help`, run as a call runs, refused
// when gh answers it with an error, as ghroute.ReadHelp refuses it for a pane.
func (a *Answer) help(ctx context.Context, env []string) func(path []string) (string, error) {
	return func(path []string) (string, error) {
		cmd := exec.CommandContext(ctx, a.host.GH, append(slices.Clone(path), "--help")...)
		cmd.Env, cmd.Dir = env, a.cwd
		procgroup.Configure(cmd)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("gh answered its help with %v: %s", err, strings.TrimSpace(stderr.String()))
		}
		if stdout.Len() == 0 {
			return "", errors.New("gh printed no help")
		}
		return stdout.String(), nil
	}
}

// runGH runs gh with argv under the call's environment, configDir its GH_CONFIG_DIR, in the
// answer's empty working directory with no stdin, its stdout and stderr interleaved and cut at
// bounds.GHOutputChars, and stopped, its whole process group, at ctx's end (the gh_seconds bound).
func (a *Answer) runGH(ctx context.Context, argv []string, configDir string, bounds config.AnswerBounds) Result {
	cmd := exec.CommandContext(ctx, a.host.GH, argv...)
	cmd.Env, cmd.Dir = a.env(configDir), a.cwd
	procgroup.Configure(cmd)
	out := &cappedOutput{limit: bounds.GHOutputChars}
	cmd.Stdout, cmd.Stderr = out, out
	err := cmd.Run()
	if err := procgroup.Err(err); err != nil {
		return Result{Output: fmt.Sprintf("gh could not run: %v", err), Exit: -1}
	}
	output := out.String()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		output += fmt.Sprintf("\n[stopped: gh ran past its %s bound (answers.bounds.gh_seconds)]", bounds.GHCall)
	}
	return Result{Output: output, Exit: cmd.ProcessState.ExitCode(), Ran: true}
}

// cappedOutput keeps the first limit characters written to it and counts the rest. A character is
// a UTF-8 sequence, counted by its leading byte, so a sequence split across writes counts once.
type cappedOutput struct {
	limit int
	kept  []byte
	chars int
}

var _ io.Writer = (*cappedOutput)(nil)

func (c *cappedOutput) Write(p []byte) (int, error) {
	for _, b := range p {
		if b&0xC0 != 0x80 {
			c.chars++
		}
		if c.chars <= c.limit {
			c.kept = append(c.kept, b)
		}
	}
	return len(p), nil
}

// String is what was kept, and a note of how many characters were cut after it.
func (c *cappedOutput) String() string {
	if c.chars <= c.limit {
		return string(c.kept)
	}
	return fmt.Sprintf("%s\n[cut: %d more characters]", c.kept, c.chars-c.limit)
}
