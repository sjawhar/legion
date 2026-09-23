package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// paneEnvAllowList is every variable a pane process reads from the daemon's own environment,
// verbatim from the shipped PANE_ENV_ALLOW_LIST (packages/daemon/src/daemon/environment.ts:
// 180-245): Oh My Pi and its plugins (HOME, the XDG base directories, OMP_PROFILE/PI_PROFILE,
// SECRETSD_SOCK for the secretsd OMP extension), jj/git/gh, mise and the `mise x` every pane runs,
// tmux itself (TMUX_TMPDIR, SHELL), locale and proxy policy. Nothing else the daemon was started
// with reaches the private server or a pane — not its provider keys (those reach OMP alone, as
// daemon-held files: readProviderKeyNames), not the secret store's config or age identity, not an
// operator's TMUX or SSH agent, not a LEGION_* value inherited from an outer pane. A name joins
// this list with the process that reads it named above, never as a prefix or a wildcard.
var paneEnvAllowList = []string{
	// identity, locale, terminal
	"HOME", "USER", "LOGNAME", "SHELL", "TERM", "TZ", "LANG", "LANGUAGE",
	"LC_ADDRESS", "LC_ALL", "LC_COLLATE", "LC_CTYPE", "LC_IDENTIFICATION", "LC_MEASUREMENT",
	"LC_MESSAGES", "LC_MONETARY", "LC_NAME", "LC_NUMERIC", "LC_PAPER", "LC_TELEPHONE", "LC_TIME",
	// directories
	"TMPDIR", "TMUX_TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME",
	"XDG_RUNTIME_DIR", "XDG_STATE_HOME",
	// OMP profile selection
	"OMP_PROFILE", "PI_PROFILE",
	// mise's tool store
	"MISE_CACHE_DIR", "MISE_CONFIG_DIR", "MISE_DATA_DIR", "MISE_STATE_DIR",
	// the secretsd socket override
	"SECRETSD_SOCK",
	// outbound network policy
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy",
	"SSL_CERT_DIR", "SSL_CERT_FILE", "NODE_EXTRA_CA_CERTS",
	// the PATH panes run under
	"PATH",
}

// secretLikeName is the second line of defence behind the allow-list (environment.ts:247-260): a
// trailing credential segment, optionally followed by _FILE, or PRIVATE_KEY anywhere, in any case.
// The segment must end the name: TOKENIZER, X_PATH, and X_KEYBOARD are not credentials.
var secretLikeName = regexp.MustCompile(`(?i)(?:_SECRET|_TOKEN|_GRANT|_KEY|_PASSWORD|_PASSWD|_PAT|_CREDENTIALS)(?:_FILE)?$|PRIVATE_KEY`)

func isSecretLikeName(name string) bool { return secretLikeName.MatchString(name) }

// xdgHome is the home the four XDG base directories of every pane live under: the daemon's own,
// never the operator's (LEGION-206 P1 — a pane that read the operator's XDG_CONFIG_HOME would run
// with their jj, gh, and mise configuration).
func xdgHome(stateDir string) string { return filepath.Join(stateDir, "home") }

// readProviderKeyNames is the key names in the provider-env directory, which the runtime reads once
// at construction — boot, before any pane. Every entry must be a file named for an environment
// variable that no pane already carries: the shim refuses to start over a name its environment
// holds, and skips without a word a name whose NAME_FILE pointer it holds
// (internal/shim/config.go:59-98), so either would open panes whose OMP lacks the key.
func readProviderKeyNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("tmux runtime: read the provider-env directory: %w", err)
	}
	carried := map[string]bool{}
	for _, name := range paneEnvAllowList {
		carried[name] = true
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case !entry.Type().IsRegular() || !envName.MatchString(name):
			return nil, fmt.Errorf("tmux runtime: provider-env entry %q is not named for an environment variable", name)
		case carried[name] || runtimeOwned[name]:
			return nil, fmt.Errorf("tmux runtime: provider key %s is a variable every pane already carries", name)
		case runtimeOwned[name+"_FILE"]:
			return nil, fmt.Errorf("tmux runtime: provider key %s would not reach OMP: every pane carries %s_FILE", name, name)
		}
		names = append(names, name)
	}
	return names, nil
}

// xdgDirectories are the four XDG base directories under the daemon's home, in the order the
// panes' -e pairs carry them, at the standard offsets from a home directory.
func xdgDirectories(stateDir string) [][2]string {
	home := xdgHome(stateDir)
	return [][2]string{
		{"XDG_CONFIG_HOME", filepath.Join(home, ".config")},
		{"XDG_CACHE_HOME", filepath.Join(home, ".cache")},
		{"XDG_DATA_HOME", filepath.Join(home, ".local", "share")},
		{"XDG_STATE_HOME", filepath.Join(home, ".local", "state")},
	}
}

// PaneEnvironment is the environment the daemon's private tmux server is forked under, every tmux
// command runs with, and every pane therefore inherits beneath its -e pairs: the allow-listed
// names read from environ (the daemon's own, `os.Environ()` form), less any credential-shaped
// name, with the four XDG base directories moved under `<state_dir>/home`
// (environment.ts:270-294). A boot probe that must see what a pane sees runs under it too.
func PaneEnvironment(environ []string, stateDir string) map[string]string {
	allowed := make(map[string]bool, len(paneEnvAllowList))
	for _, name := range paneEnvAllowList {
		allowed[name] = true
	}
	env := map[string]string{}
	for _, entry := range environ {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !allowed[name] || isSecretLikeName(name) {
			continue
		}
		env[name] = value
	}
	env["PATH"] = workerPath(env["PATH"], stateDir)
	for _, dir := range xdgDirectories(stateDir) {
		env[dir[0]] = dir[1]
	}
	return env
}

// miseInvocation is the one configured invocation shape the daemon launches through mise.
var miseInvocation = regexp.MustCompile(`^mise x (\S+) -- omp$`)

// ResolveOmpInvocation is the OMP launch fragment every pane and boot probe runs, resolved from
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
func ResolveOmpInvocation(invocation string, env func(string) string) (string, error) {
	configured, err := configuredPath(env, "LEGION_OMP_PATH")
	if err != nil {
		return "", err
	}
	if configured != "" {
		resolved, ok := resolveExecutable(configured)
		if !ok {
			return "", fmt.Errorf("LEGION_OMP_PATH is not an executable: %s", configured)
		}
		return shellPath(resolved), nil
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
	return shellPath(mise) + " x " + match[1] + " -- " + shellPath(omp), nil
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

// shellEscaped is the set of characters tmux's `-s` output escapes inside a value with a backslash
// (tmux 3.7c, cmd-show-environment.c): dollar, backtick, double quote, backslash.
func shellEscaped(c byte) bool { return c == '$' || c == '`' || c == '"' || c == '\\' }

// parseShellEnvironment reads `show-environment -s` output — the whole text, never line by line —
// into the names of the table's entries (tmux.ts:62-120). tmux prints each entry as
// `NAME="value"; export NAME;` and a newline, escaping each dollar, backtick, double quote, and
// backslash inside the value with a backslash; a value's own newlines are left in place for a
// UTF-8 client and printed as `_` for any other. A variable unset for new panes prints as
// `unset NAME;` — a marker that reaches no pane, so not a name here. A name is everything up to
// the first `=`: tmux refuses `=` in a name, but a space, `"`, or `;` in one is fine.
//
// Marker or entry is decided by structure, never by the `unset ` prefix alone: a line is a marker
// only when it is exactly `unset <NAME>;` and a newline and holds no `=`, so an entry named
// `unset X` is the entry it is and a marker missing its `;` swallows nothing. The value closes at
// the first unescaped `"`, and the entry only on the exact `"; export <the same NAME>;` — so a
// multi-line value's continuation line, whatever it holds (`HOME;=x`, `key = value`, a PEM's
// `=`-padded last line, even the escaped text `"; export X;`), is never read as a name. Anything
// else is a refusal naming the table and the byte offset, never the text there, which is a value.
func parseShellEnvironment(dump string, table envTable) ([]string, error) {
	names := []string{}
	fail := func(offset int, what string) ([]string, error) {
		return nil, fmt.Errorf("tmux show-environment -s (%s): %s at byte %d, cannot read the table", table, what, offset)
	}
	i := 0
	for i < len(dump) {
		lineEnd := strings.IndexByte(dump[i:], '\n')
		line := dump[i:]
		if lineEnd >= 0 {
			lineEnd += i
			line = dump[i:lineEnd]
		}
		if strings.HasPrefix(line, "unset ") && !strings.Contains(line, "=") {
			// No `=` on the line, so it cannot open an entry: it is a marker or nothing.
			if lineEnd < 0 || !strings.HasSuffix(line, ";") || line == "unset ;" {
				return fail(i, "malformed unset marker")
			}
			i = lineEnd + 1
			continue
		}
		eq := strings.IndexByte(dump[i:], '=')
		if eq >= 0 {
			eq += i
		}
		if eq < 0 || eq == i || (lineEnd >= 0 && lineEnd < eq) {
			return fail(i, "expected NAME=")
		}
		name := dump[i:eq]
		if eq+1 >= len(dump) || dump[eq+1] != '"' {
			return fail(eq+1, "expected `\"` after NAME=")
		}
		j := eq + 2
		for {
			if j >= len(dump) {
				return fail(j, "unterminated value")
			}
			if dump[j] == '\\' {
				if j+1 >= len(dump) || !shellEscaped(dump[j+1]) {
					return fail(j, "unknown escape in value")
				}
				j += 2
				continue
			}
			if dump[j] == '"' {
				break
			}
			j++
		}
		trailer := "\"; export " + name + ";\n"
		if !strings.HasPrefix(dump[j:], trailer) {
			return fail(j, "expected `\"; export NAME;` closing the entry")
		}
		names = append(names, name)
		i = j + len(trailer)
	}
	return names, nil
}
