// Package omplaunch is how a Legion process launches a local Oh My Pi: the OMP invocation resolved
// from omp_invocation or LEGION_OMP_PATH, the operator's launch prefix before it, and the one
// --append-system-prompt word. A tmux pane, the daemon's boot gate on the plugin every pane loads,
// and `legion controller start` on the operator's machine each launch it this way, so none of
// them depends on the tmux runtime to do it.
package omplaunch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// miseInvocation is the one configured invocation shape the daemon launches through mise.
var miseInvocation = regexp.MustCompile(`^mise x (\S+) -- omp$`)

// ResolveInvocation is the OMP launch fragment every pane and boot probe runs, resolved from
// the configured invocation and the daemon's own environment (environment.ts:312-341).
// `LEGION_OMP_PATH` names a binary directly — the worker image, a build under test — and is used
// as its resolved real path. Otherwise the invocation must be `mise x <tool> -- omp`: mise resolves
// the configured tool's own `bin/omp`, and the fragment still runs it through `mise x` so the
// tool's declared environment is activated. The result is a shell fragment: each path is quoted
// for the pane's shell.
//
// An empty invocation — the file set no omp_invocation — is refused by name unless LEGION_OMP_PATH
// is set. The shipped daemon falls back to its pinned default there; the Go daemon holds no copy of
// the pin, whose one home is packages/daemon/src/daemon/omp-pin.ts.
func ResolveInvocation(invocation string, env func(string) string) (string, error) {
	configured, err := configuredPath(env, "LEGION_OMP_PATH")
	if err != nil {
		return "", err
	}
	if configured != "" {
		resolved, ok := resolveExecutable(configured)
		if !ok {
			return "", fmt.Errorf("LEGION_OMP_PATH is not an executable: %s", configured)
		}
		return shellprefix.Word(resolved), nil
	}
	if invocation == "" {
		return "", errors.New("omp_invocation is not set: set it to 'mise x <tool> -- omp', or set LEGION_OMP_PATH to an absolute executable path")
	}
	match := miseInvocation.FindStringSubmatch(invocation)
	if match == nil {
		return "", errors.New("OMP invocation must be 'mise x <tool> -- omp'. Set LEGION_OMP_PATH to an absolute executable path.")
	}
	mise, err := resolveMise(env)
	if err != nil {
		return "", err
	}
	omp, err := resolveMiseOmp(mise, match[1])
	if err != nil {
		return "", err
	}
	return shellprefix.Word(mise) + " x " + match[1] + " -- " + shellprefix.Word(omp), nil
}

// resolveMiseOmp resolves the configured tool's own OMP executable before any pane exists. `mise
// x <tool> -- omp` alone is not enough: an operator's PATH can put an OMP wrapper ahead of mise's
// activated bin. The pane still runs through `mise x` for the tool's declared environment, but it
// names this executable directly, so its binary and the boot probe's binary cannot diverge.
func resolveMiseOmp(mise, tool string) (string, error) {
	output, err := exec.Command(mise, "where", tool).Output()
	if err != nil {
		return "", fmt.Errorf("mise could not resolve OMP tool %s: %w", tool, err)
	}
	install := strings.TrimSpace(string(output))
	omp, ok := resolveExecutable(filepath.Join(install, "bin", "omp"))
	if !ok {
		return "", fmt.Errorf("mise tool %s has no executable bin/omp under %s", tool, install)
	}
	return omp, nil
}

// configuredPath reads a `LEGION_<TOOL>_PATH` override: unset or empty is no override, and a set
// one must be absolute (environment.ts:139-146).
func configuredPath(env func(string) string, variable string) (string, error) {
	configured := env(variable)
	if configured == "" {
		return "", nil
	}
	if !filepath.IsAbs(configured) {
		return "", fmt.Errorf("%s must be an absolute executable path", variable)
	}
	return configured, nil
}

// resolveMise is mise's absolute path: `LEGION_MISE_PATH`, else the first executable `mise` on the
// daemon's PATH (environment.ts:148-156, 471-476).
func resolveMise(env func(string) string) (string, error) {
	missing := errors.New("Missing required daemon tool: mise (set LEGION_MISE_PATH to an absolute executable path)")
	configured, err := configuredPath(env, "LEGION_MISE_PATH")
	if err != nil {
		return "", err
	}
	if configured != "" {
		if resolved, ok := resolveExecutable(configured); ok {
			return resolved, nil
		}
		return "", missing
	}
	for _, dir := range filepath.SplitList(env("PATH")) {
		if dir == "" {
			continue
		}
		if resolved, ok := resolveExecutable(filepath.Join(dir, "mise")); ok {
			return resolved, nil
		}
	}
	return "", missing
}

// resolveExecutable is path's real path when it names an executable file (environment.ts:122-137).
func resolveExecutable(path string) (string, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	return real, true
}

// doubleQuoteEscaper escapes the four characters a shell still interprets inside double quotes.
var doubleQuoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")

// SystemPromptArgument is the one `--append-system-prompt` word every pane's OMP receives, and
// the operator-launched controller's (`legion controller start`). OMP's
// flag is last-wins, so every fragment rides a single value, in order: the role prompt files, the
// addressing text, then the deployment instructions file, separated by a blank line. It is one
// double-quoted word holding `$(cat <files>)`, so the pane's own shell reads the files — their
// size and quoting never pass through tmux's argv — and the addressing text is escaped for the
// double quotes (runtime-tmux.ts:79-103). The caller has checked there is a role prompt.
func SystemPromptArgument(parts runtime.PromptParts) string {
	quoted := make([]string, len(parts.RolePromptPaths))
	for i, path := range parts.RolePromptPaths {
		quoted[i] = shellprefix.Word(path)
	}
	fragments := []string{"$(cat " + strings.Join(quoted, " ") + ")"}
	if parts.Addressing != "" {
		fragments = append(fragments, doubleQuoteEscaper.Replace(parts.Addressing))
	}
	if parts.DeploymentInstructionsPath != "" {
		fragments = append(fragments, "$(cat "+shellprefix.Word(parts.DeploymentInstructionsPath)+")")
	}
	return `--append-system-prompt "` + strings.Join(fragments, "\n\n") + `"`
}

// WithPrefix prepends the configured launch prefix, each element quoted on its own, to
// the OMP invocation — a fragment already fit for the shell (runtime-tmux.ts:105-118). Every pane
// runs it, and so does the daemon's boot gate, which must launch Oh My Pi exactly as a pane will.
func WithPrefix(prefix []string, invocation string) string {
	if len(prefix) == 0 {
		return invocation
	}
	quoted := make([]string, len(prefix))
	for i, word := range prefix {
		quoted[i] = shellprefix.Word(word)
	}
	return strings.Join(quoted, " ") + " " + invocation
}
