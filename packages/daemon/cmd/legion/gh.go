package main

import (
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/sjawhar/legion/daemon/internal/ghroute"
)

const ghUsage = "usage: legion gh [gh args…]  (the gh first on a Legion pane's or pod's PATH: runs LEGION_GH_PATH with GH_CONFIG_DIR set to the gh directory of the owner the command names, under LEGION_GH_OWNERS_DIR)"

// runGh is `legion gh`, the gh a pane or pod runs. It reads the owner of the repository the
// command acts on as gh resolves it (ghroute.TargetOf) and execs the real gh (LEGION_GH_PATH) as `gh`
// with its arguments untouched and GH_CONFIG_DIR set to that owner's gh directory under
// LEGION_GH_OWNERS_DIR; a command naming no repository, in a working directory whose git remotes
// name none, keeps GH_CONFIG_DIR, the issue's owner's. A command it cannot read for certain, one
// naming two owners, and one naming an owner the bundle holds no directory for are refused, exit
// 2, naming why, and the real gh never runs: no token but the target owner's is ever sent.
//
// The git gh runs itself (a clone, a fetch of a fork's parent, `pr checkout`) authenticates
// through the credential helper gh names after its own executable, GH_PATH: runGh sets it to the
// `gh` wrapper first on PATH, so that helper is `legion gh auth git-credential`, which answers as
// `legion git-credential` does, from the directory of the owner in git's path (useHttpPath set for
// github.com through GIT_CONFIG_COUNT), never with GH_CONFIG_DIR's token; when PATH's first gh is
// no such wrapper it refuses. The child also runs with gh's update check and prompts off, which a TTY
// would otherwise reach. Each of the three variables is runtime-set and required; an unset one is
// refused, exit 2, naming it. With no LEGION_GH_PATH, --help prints this command's usage, so
// `legion gh --help` outside a pane answers as every legion command does.
func runGh(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	env := processEnvironment()
	if env["LEGION_GH_PATH"] == "" && len(args) == 1 && helpRequested(args[0]) {
		fmt.Fprintln(stderr, ghUsage)
		return 0
	}
	for _, name := range []string{"LEGION_GH_PATH", "GH_CONFIG_DIR", "LEGION_GH_OWNERS_DIR"} {
		if env[name] == "" {
			fmt.Fprintf(stderr, "legion gh: %s is unset: a Legion pane or pod sets it, and nothing else runs this gh\n%s\n", name, ghUsage)
			return 2
		}
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "git-credential" {
		return runGitCredential(ctx, args[2:], stdout, stderr)
	}
	gh := env["LEGION_GH_PATH"]
	wrapper, err := ghWrapperPath()
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	target, err := ghroute.TargetOf(args, env, func(path []string) (string, error) { return ghroute.ReadHelp(gh, path) }, ghroute.ReadGitRemotes)
	if err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	dir := env["GH_CONFIG_DIR"]
	if target.Owner != "" {
		if dir, err = ownerGHConfigDir(env["LEGION_GH_OWNERS_DIR"], target.Owner); err != nil {
			fmt.Fprintf(stderr, "legion gh: %v\n", err)
			return 2
		}
	}
	set := map[string]string{"GH_CONFIG_DIR": dir, "GH_PATH": wrapper, "GH_NO_UPDATE_NOTIFIER": "1", "GH_PROMPT_DISABLED": "1"}
	childEnv := make([]string, 0, len(env)+len(set)+3)
	for _, pair := range os.Environ() {
		if name, _, _ := strings.Cut(pair, "="); set[name] == "" {
			childEnv = append(childEnv, pair)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(set)) {
		childEnv = append(childEnv, name+"="+set[name])
	}
	if childEnv, err = withGitHubUseHTTPPath(childEnv); err != nil {
		fmt.Fprintf(stderr, "legion gh: %v\n", err)
		return 2
	}
	err = syscall.Exec(gh, append([]string{"gh"}, args...), childEnv)
	fmt.Fprintf(stderr, "legion gh: run %s: %v\n", gh, err)
	return 1
}

// ghWrapperScript is the `gh` wrapper both runtimes install first on PATH: a script whose one
// command execs `legion gh` with its arguments (packages/daemon/docker/gh, workerbin.Install),
// the legion path absolute, so the shell's exec reads it as no option, and either a bare word of
// the characters a shell reads literally (shellprefix's safe set) or quoted, in single quotes
// (shellprefix.Literal) or in double quotes with no expansion.
var ghWrapperScript = regexp.MustCompile(`^#!/bin/sh\nexec ('/(?:[^']|'\\'')*'|"/[^"$` + "`" + `\\]*"|/[A-Za-z0-9_./:-]*) gh "\$@"\n?$`)

// ghWrapperMaxBytes bounds the wrapper read: a script longer is refused whole rather than matched
// by its start. A wrapper whose quoted legion executable path takes it past the bound (a path near
// PATH_MAX, or one whose single quotes, four bytes each once quoted, grow it that far) is refused
// too, which refuses every command `legion gh` would hand the real gh rather than letting one
// through; `legion gh auth git-credential`, answered before the check, answers by git's path alone.
const ghWrapperMaxBytes = 4096

// ghWrapperPath is the `gh` first on PATH, the wrapper that runs `legion gh`, as an absolute
// path: gh names its git credential helper after it (GH_PATH). It is refused when PATH's first gh
// is no such wrapper (isGhWrapper), or there is none: the real gh itself would answer every
// github.com request with GH_CONFIG_DIR's token, and a machine's own wrapper with whatever it
// chooses.
func ghWrapperPath() (string, error) {
	wrapper, err := exec.LookPath("gh")
	if err == nil {
		wrapper, err = filepath.Abs(wrapper)
	}
	if err != nil {
		return "", fmt.Errorf("no gh on PATH runs `legion gh` (%v), so the git gh runs would not authenticate through it; run it as a Legion pane's gh", err)
	}
	file, err := os.Open(wrapper)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", wrapper, err)
	}
	defer file.Close()
	ok, err := isGhWrapper(file)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", wrapper, err)
	}
	if !ok {
		return "", fmt.Errorf("the first gh on PATH, %s, is not the wrapper that runs `legion gh`, so the git gh runs would not authenticate through it; run it as a Legion pane's gh", wrapper)
	}
	return wrapper, nil
}

// isGhWrapper says whether r holds, whole, a script ghWrapperScript accepts: one longer than
// ghWrapperMaxBytes is none.
func isGhWrapper(r io.Reader) (bool, error) {
	script, err := io.ReadAll(io.LimitReader(r, ghWrapperMaxBytes+1))
	if err != nil {
		return false, err
	}
	return len(script) <= ghWrapperMaxBytes && ghWrapperScript.Match(script), nil
}

// withGitHubUseHTTPPath adds credential.https://github.com.useHttpPath=true to env's GIT_CONFIG_COUNT
// entries, which git reads as it reads `-c`, so the git gh runs tells its credential helper the
// repository's path, whose owner `legion git-credential` answers for. A GIT_CONFIG_COUNT that is no
// count is refused.
func withGitHubUseHTTPPath(env []string) ([]string, error) {
	count := 0
	for _, pair := range env {
		if value, ok := strings.CutPrefix(pair, "GIT_CONFIG_COUNT="); ok {
			n, err := strconv.Atoi(value)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("GIT_CONFIG_COUNT is %q, which is no count, so git's configuration cannot be extended; unset it", value)
			}
			count = n
		}
	}
	key, value := fmt.Sprintf("GIT_CONFIG_KEY_%d", count), fmt.Sprintf("GIT_CONFIG_VALUE_%d", count)
	out := make([]string, 0, len(env)+3)
	for _, pair := range env {
		if name, _, _ := strings.Cut(pair, "="); name != "GIT_CONFIG_COUNT" && name != key && name != value {
			out = append(out, pair)
		}
	}
	return append(out, key+"=credential.https://github.com.useHttpPath", value+"=true", fmt.Sprintf("GIT_CONFIG_COUNT=%d", count+1)), nil
}
