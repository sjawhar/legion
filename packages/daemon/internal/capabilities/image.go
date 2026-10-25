package capabilities

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/lookpath"
	"github.com/sjawhar/legion/daemon/internal/ompdirs"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// Image is what the image-site checks read.
type Image struct {
	// Launch is the Oh My Pi invocation as a shell fragment, run as `exec <Launch> <args>` under
	// `sh -c`: the probe's --omp or LEGION_OMP_PATH. The shell parses it as a pod's shim parses
	// its launch, so a prefix in it runs as it would there.
	Launch string
	// Env is the environment the checks run under (PATH, PUPPETEER_EXECUTABLE_PATH, HOME,
	// OMP_PROFILE, PI_CONFIG_FILES, …): the one the pod's Oh My Pi starts with, after the pod
	// baseline and the operator's pod env shaped it. A binary is looked up on Env's PATH, never
	// this process's, and a variable is read from Env, so a value the operator's pod env sets is
	// checked as Oh My Pi would honour it.
	Env []string
	// WorkDir is where the checks run.
	WorkDir string
	// ClaudeRoot is the claude-envoy package root the image carries, which a phase worker on the
	// `claude` backend loads as Claude Code's plugin directory, and ClaudeCodePin the directory of
	// the package.json and package-lock.json a pod installs Claude Code from at launch: the
	// claude-code row's two facts (claudeCode), named by `legion probe-image --claude-root` and
	// `--claude-code-pin`.
	ClaudeRoot    string
	ClaudeCodePin string
}

// commandTimeout bounds each command a check runs: a `--version` or a `--check` that has not
// answered in a minute is hanging, not slow, and the probe's own budget is not to be spent on it.
const commandTimeout = 60 * time.Second

// maxDetail caps a command's quoted output in a State's detail, so a chatty command cannot push
// the table past the log tail the daemon reads back.
const maxDetail = 300

// imageCheck is one image row's check: its evidence, and whether the capability is present.
type imageCheck func(ctx context.Context, img Image) (detail string, ok bool)

// imageChecks are the image rows' checks, by name. TestEveryImageRowHasACheck holds the map to
// Table's image rows, so a row added to one is added to the other.
var imageChecks = map[Name]imageCheck{
	EvalJS: evalJS, EvalPython: evalPython, Browser: browser, LSP: lsp, CodeGraph: codeGraph, Skills: skills, Toolchain: toolchain, ClaudeCode: claudeCode,
}

// CheckImage checks every image-site row under img and renders every row of Table, in its order:
// image rows present or missing with their evidence, live rows reported as what each session
// reports at its start and `legion state` renders, deployment rows reported as what the daemon
// reports from the deployment's configuration, withheld rows with their ruling. It runs after the
// launch probes (daemon.ProbeImage) passed: the eval-js and skills rows are present by those
// probes, which ran Oh My Pi — its own JavaScript runtime — and resolved the prompt-named skills.
// err is nil when every image row's check passed; otherwise it names every missing one, so one
// rebuild of the image fixes them all rather than the first alone. A ctx that ends ends the check
// with an error wrapping ctx's.
func CheckImage(ctx context.Context, img Image) ([]State, error) {
	var absent []string
	states, err := render(ctx, renderer{
		image: func(row Capability) State {
			detail, ok := imageChecks[row.Name](ctx, img)
			if !ok {
				absent = append(absent, fmt.Sprintf("capability %s is missing: %s", row.Name, detail))
				return State{Name: row.Name, Status: StatusMissing, Detail: detail}
			}
			return State{Name: row.Name, Status: StatusPresent, Detail: detail}
		},
		live: func(row Capability) State {
			return State{Name: row.Name, Status: StatusReported, Detail: "each session reports it at start; `legion state` renders it: " + row.Summary}
		},
		deployment: func(row Capability) State {
			return State{Name: row.Name, Status: StatusReported, Detail: "the daemon reports it from the deployment's configuration: " + row.Summary}
		},
	})
	if err != nil {
		return states, err
	}
	if len(absent) > 0 {
		return states, errors.New(strings.Join(absent, "; "))
	}
	return states, nil
}

// ReadModelFallback runs `<Launch> config get retry.modelFallback --json` under img and answers
// bootprobe.ModelFallbackOn or bootprobe.ModelFallbackOff, the JSON's `value`: whether the pod's
// Oh My Pi, under the operator's configuration, falls back to another model when the agent's own
// is unavailable. An error names the command and its output.
func ReadModelFallback(ctx context.Context, img Image) (string, error) {
	const args = "config get retry.modelFallback --json"
	command := img.Launch + " " + args
	r := img.launch(ctx, args)
	if r.err != nil {
		return "", fmt.Errorf("%s: %w", command, r.err)
	}
	if r.exit != 0 {
		return "", fmt.Errorf("%s exited %d: %s", command, r.exit, r.output())
	}
	var answer struct {
		Value *bool `json:"value"`
	}
	if err := json.NewDecoder(strings.NewReader(r.stdout)).Decode(&answer); err != nil || answer.Value == nil {
		return "", fmt.Errorf("%s printed no JSON object with a boolean value: %s", command, r.output())
	}
	if *answer.Value {
		return bootprobe.ModelFallbackOn, nil
	}
	return bootprobe.ModelFallbackOff, nil
}

// evalJS: Oh My Pi is its JavaScript runtime, and the launch probes ran it.
func evalJS(context.Context, Image) (string, bool) {
	return "Oh My Pi is its JavaScript runtime, and the launch probes ran it", true
}

// skills: the launch probes' load resolved every skill the role prompts name.
func skills(context.Context, Image) (string, bool) {
	return "the launch probes resolved every skill the role prompts name", true
}

// evalPython asks Oh My Pi itself whether its Python is available: `omp setup python --check
// --json` answers `{"available": true}`, or `{"available": false, …}` and exit 1 when it is not.
// The JSON is the verdict, not the exit code, so the detail says what Oh My Pi said.
func evalPython(ctx context.Context, img Image) (string, bool) {
	r := img.launch(ctx, "setup python --check --json")
	if r.err != nil {
		return "omp setup python --check --json: " + r.err.Error(), false
	}
	var answer struct {
		Available *bool `json:"available"`
	}
	if err := json.NewDecoder(strings.NewReader(r.stdout)).Decode(&answer); err != nil || answer.Available == nil {
		return fmt.Sprintf("omp setup python --check --json exited %d without a JSON available field: %s", r.exit, r.output()), false
	}
	if !*answer.Available {
		return "omp setup python --check reports available=false", false
	}
	return "omp setup python --check reports available=true", true
}

// browser runs the Chromium Oh My Pi's browser tools would launch, resolved as Oh My Pi resolves
// it — the executable PUPPETEER_EXECUTABLE_PATH names, else `chromium` on PATH — with --version.
// Running it, not merely finding it, is the point: a shared library the image lacks fails the
// loader, which no stat shows, and a wrong PUPPETEER_EXECUTABLE_PATH in the operator's pod env
// fails here rather than in every worker's first browser call.
func browser(ctx context.Context, img Image) (string, bool) {
	path, how := envValue(img.Env, "PUPPETEER_EXECUTABLE_PATH"), "PUPPETEER_EXECUTABLE_PATH"
	switch {
	case path == "":
		found, ok := lookpath.On(envValue(img.Env, "PATH"), "chromium")
		if !ok {
			return "chromium is not on PATH, and PUPPETEER_EXECUTABLE_PATH names no executable", false
		}
		path, how = found, "chromium on PATH"
	case !strings.Contains(path, "/"):
		// A bare name: Oh My Pi spawns it, and the spawn looks it up on PATH.
		found, ok := lookpath.On(envValue(img.Env, "PATH"), path)
		if !ok {
			return fmt.Sprintf("%s (%s) is not on PATH", path, how), false
		}
		path = found
	case !filepath.IsAbs(path):
		path = filepath.Join(img.WorkDir, path)
	}
	r := img.run(ctx, path, "--version")
	if r.err != nil || r.exit != 0 {
		return fmt.Sprintf("%s (%s) --version: %s", path, how, r.failure()), false
	}
	return fmt.Sprintf("%s (%s) --version: %s", path, how, r.output()), true
}

// lsp: the language servers Oh My Pi's LSP tools start for Go, TypeScript and Python.
func lsp(_ context.Context, img Image) (string, bool) {
	_, detail, ok := onPath(img, "gopls", "typescript-language-server", "pyright-langserver")
	return detail, ok
}

// claudeCode: what a phase worker on the `claude` backend needs of the image, which carries no
// Claude Code — its licence grants no redistribution and the image is public — so the row
// measures the two facts that stand in for it, the same two the Claude lane of the launch probes
// proved before this check (internal/daemon's verifyClaude): the image carries the claude-envoy
// plugin that worker loads (CheckClaudePlugin), and it carries a pin a pod can install Claude Code
// from and verify what it installed against (ReadClaudeCodePin). Beside them, npm and node on
// PATH, as the lsp and toolchain rows find theirs: the start script's install runs npm and its
// version check runs node (claudeCodeBlock, internal/runtime/sandbox/plugins.go). Every image
// carries all of it whatever a deployment's worker_backends names — one image serves both
// harnesses — so every probe of every image checks this row; whether the registry answers a
// pod's fetch is the pod's own launch to find out, which fails loudly when it does not. Every
// failure is named at once, as codegraph names both of its halves.
func claudeCode(_ context.Context, img Image) (string, bool) {
	var evidence, why []string
	if img.ClaudeRoot == "" {
		why = append(why, "no claude-envoy root was given to check")
	} else if err := CheckClaudePlugin(img.ClaudeRoot); err != nil {
		why = append(why, err.Error())
	} else {
		evidence = append(evidence, img.ClaudeRoot+" holds "+strings.Join(claudePluginFiles, " and "))
	}
	if img.ClaudeCodePin == "" {
		why = append(why, "no Claude Code pin was given to check")
	} else if version, err := ReadClaudeCodePin(img.ClaudeCodePin); err != nil {
		why = append(why, err.Error())
	} else {
		evidence = append(evidence, fmt.Sprintf("%s pins %s %s with the sha512 integrity of %s", img.ClaudeCodePin, claudeCodePackage, version, claudeCodePlatformPackage))
	}
	if _, detail, ok := onPath(img, "npm", "node"); ok {
		evidence = append(evidence, detail)
	} else {
		why = append(why, detail)
	}
	if len(why) > 0 {
		return strings.Join(why, "; "), false
	}
	return strings.Join(evidence, "; "), true
}

// claudePluginFiles are the two files under the claude-envoy root a worker on the `claude` backend
// loads (packages/daemon/internal/claudelaunch), slash-separated: the built channel server, the
// worker's one MCP server, and the generated Legion plugin's manifest, which makes `legion/` the
// second plugin directory Claude Code loads.
var claudePluginFiles = []string{"dist/envoy-channel.js", "legion/.claude-plugin/plugin.json"}

// CheckClaudePlugin holds the claude-envoy root to the files a worker on the `claude` backend
// loads from it (claudePluginFiles), each a regular file: a directory at either path is no file a
// worker could load. The error names the first that is not, and why.
func CheckClaudePlugin(root string) error {
	for _, name := range claudePluginFiles {
		file := filepath.Join(root, filepath.FromSlash(name))
		info, err := os.Stat(file)
		if err == nil && !info.Mode().IsRegular() {
			err = fmt.Errorf("%s is not a regular file (mode %s)", file, info.Mode())
		}
		if err != nil {
			return fmt.Errorf("the claude-envoy root %s holds no %s (%v): a worker on the `claude` backend loads it as Claude Code's plugin directory",
				root, filepath.Base(file), err)
		}
	}
	return nil
}

// claudeCodePackage is the npm package the Claude Code pin pins, and claudeCodePlatformPackage the
// per-platform package of it whose binary a pod's install runs (claudeCodeBinary,
// internal/runtime/sandbox/plugins.go): linux-x64, as the worker image builds for linux/amd64
// alone, and the install runs no scripts, so the package's own `bin` stays a placeholder stub.
const (
	claudeCodePackage         = "@anthropic-ai/claude-code"
	claudeCodePlatformPackage = "@anthropic-ai/claude-code-linux-x64"
)

// exactVersion is a version npm resolves to itself alone: no range, tag, URL or path.
var exactVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// ReadClaudeCodePin reads the Claude Code pin in dir — packages/daemon/docker/claude-code's
// package.json and package-lock.json, which the image carries and a Claude role's start script
// runs `npm ci --ignore-scripts --omit=dev` from, then holds the installed binary's --version to
// (claudeCodeBlock) — and answers the version it pins when a pod could install and verify that
// release from it: package.json pins claudeCodePackage at an exact version, since a range would let
// the version check refuse a release the lockfile allowed; the lockfile records that same
// version for the root package, as `npm ci` refuses a lockfile out of step with its package.json;
// and it locks both claudeCodePackage and claudeCodePlatformPackage at that version, each with a
// sha512 integrity, which is what `npm ci` holds every fetched tarball to — the checksum of the
// binary a pod runs. Reading the pin is all it does: whether the registry still serves those
// tarballs is a pod's launch to find, never the image's build or a boot probe's (verifyClaude,
// internal/daemon/bootgate.go, says why). The error names the file and what it lacks.
func ReadClaudeCodePin(dir string) (string, error) {
	manifest, lock := filepath.Join(dir, "package.json"), filepath.Join(dir, "package-lock.json")
	var pinned struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if err := readPinFile(manifest, &pinned); err != nil {
		return "", err
	}
	version := pinned.Dependencies[claudeCodePackage]
	if !exactVersion.MatchString(version) {
		return "", fmt.Errorf("the Claude Code pin %s pins %s at %q, not an exact version", manifest, claudeCodePackage, version)
	}
	var locked struct {
		Packages map[string]struct {
			Version      string            `json:"version"`
			Integrity    string            `json:"integrity"`
			Dependencies map[string]string `json:"dependencies"`
		} `json:"packages"`
	}
	if err := readPinFile(lock, &locked); err != nil {
		return "", err
	}
	if recorded := locked.Packages[""].Dependencies[claudeCodePackage]; recorded != version {
		return "", fmt.Errorf("the Claude Code lockfile %s records %s at %q where %s pins %s: npm ci refuses a lockfile out of step with its package.json",
			lock, claudeCodePackage, recorded, manifest, version)
	}
	for _, name := range []string{claudeCodePackage, claudeCodePlatformPackage} {
		entry, ok := locked.Packages["node_modules/"+name]
		switch {
		case !ok:
			return "", fmt.Errorf("the Claude Code lockfile %s locks no %s", lock, name)
		case entry.Version != version:
			return "", fmt.Errorf("the Claude Code lockfile %s locks %s at %q, not the %s %s pins", lock, name, entry.Version, version, manifest)
		case !strings.HasPrefix(entry.Integrity, "sha512-"):
			return "", fmt.Errorf("the Claude Code lockfile %s carries no sha512 integrity for %s (%q): npm ci would have nothing to verify its tarball against",
				lock, name, entry.Integrity)
		}
	}
	return version, nil
}

// readPinFile decodes the Claude Code pin's JSON file at path into v; the error names the file.
func readPinFile(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("the Claude Code pin cannot be read: %v", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("the Claude Code pin %s is not JSON: %v", path, err)
	}
	return nil
}

// codeGraphPlugin is the Oh My Pi plugin that fronts the CodeGraph CLI.
const codeGraphPlugin = "@bopstack/pi-codegraph"

// codeGraph needs both halves: the CodeGraph CLI on PATH, and its plugin enabled in the profile's
// plugin lock, where `omp plugin install` recorded it; a CLI without the plugin, or a plugin
// disabled behind the CLI, gives an agent no codegraph tool. Both failures are named at once.
func codeGraph(_ context.Context, img Image) (string, bool) {
	var evidence, why []string
	if path, ok := lookpath.On(envValue(img.Env, "PATH"), "codegraph"); ok {
		evidence = append(evidence, path+" on PATH")
	} else {
		why = append(why, "codegraph is not on PATH")
	}
	if lock, err := pluginLock(img.Env, img.WorkDir); err != nil {
		why = append(why, err.Error())
	} else if detail, ok := pluginEnabled(lock, codeGraphPlugin); ok {
		evidence = append(evidence, detail)
	} else {
		why = append(why, detail)
	}
	if len(why) > 0 {
		return strings.Join(why, "; "), false
	}
	return strings.Join(evidence, "; "), true
}

// pluginLock is the profile's plugin lock, `plugins/omp-plugins.lock.json` under the profile root
// Oh My Pi, started under env in workDir, keeps its plugins in (ompdirs.ProfileRoot). The error is
// Oh My Pi's own refusal of the profile env names, or no home directory to resolve under.
func pluginLock(env []string, workDir string) (string, error) {
	root, _, err := ompdirs.ProfileRoot(envMap(env), workDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "plugins", "omp-plugins.lock.json"), nil
}

// pluginEnabled reads whether lock records name enabled, with the detail either way.
func pluginEnabled(lock, name string) (string, bool) {
	raw, err := os.ReadFile(lock)
	if err != nil {
		return fmt.Sprintf("cannot read the plugin lock %s: %v", lock, err), false
	}
	var recorded struct {
		Plugins map[string]struct {
			Enabled bool `json:"enabled"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		return fmt.Sprintf("the plugin lock %s is not the lock's JSON: %v", lock, err), false
	}
	plugin, ok := recorded.Plugins[name]
	switch {
	case !ok:
		return fmt.Sprintf("%s is not in the plugin lock %s", name, lock), false
	case !plugin.Enabled:
		return fmt.Sprintf("%s is disabled in the plugin lock %s", name, lock), false
	}
	return fmt.Sprintf("%s enabled in %s", name, lock), true
}

// toolchain: the generic toolchain on PATH, and `go version` running, since Go is the one of them
// whose own toolchain download or GOTOOLCHAIN setting can make a present binary fail to run. flock
// is on the list because the install Legion renders for an installing launch serialises on the
// issue's shared bun install cache with it (installBlock,
// packages/daemon/internal/runtime/sandbox/plugins.go), so a role container whose image lacks it
// fails every installing launch. psql is on it because `bun run e2e`, the Dispatch browser suite a
// reader-facing Dispatch change must pass before it merges, reaches psql through
// packages/dispatch/e2e/psql.ts, which seed.ts calls for every spec's database reset, and the
// local stack `bun run dispatch:local` (packages/dispatch/e2e/local-instance.ts) refuses to start
// without psql on PATH, so a role whose image lacks psql cannot run the suite its own change owes.
// redis-cli, sqlite3, nc and dig are the clients a role's own work needs and an unprivileged
// container cannot apt-install for itself: a repository's own SQLite fixture or database (and Oh
// My Pi's own profile databases, which are SQLite where a role's session is not), a reachability
// check, a name that does not resolve.
// A missing tool fails the publish naming the tool; a deployment that layers its own image on the
// published one inherits the five; a deployment still pinning an older digest keeps the old list,
// because the probe runs that image's own `legion`.
func toolchain(ctx context.Context, img Image) (string, bool) {
	paths, detail, ok := onPath(img, "go", "curl", "wget", "python3", "node", "bun", "uv", "kubectl", "jq", "ssh-keygen", "flock", "psql", "redis-cli", "sqlite3", "nc", "dig")
	if !ok {
		return detail, false
	}
	// paths are in the names' order, so the first is go's.
	r := img.run(ctx, paths[0], "version")
	if r.err != nil || r.exit != 0 {
		return fmt.Sprintf("%s; go version: %s", detail, r.failure()), false
	}
	return fmt.Sprintf("%s; %s", detail, r.output()), true
}

// onPath is the verdict on every name being on img's PATH: where each found one is, in names'
// order, and the detail — where each is, or which are not.
func onPath(img Image, names ...string) ([]string, string, bool) {
	searchPath := envValue(img.Env, "PATH")
	var found, absent []string
	for _, name := range names {
		if path, ok := lookpath.On(searchPath, name); ok {
			found = append(found, path)
		} else {
			absent = append(absent, name)
		}
	}
	switch len(absent) {
	case 0:
		return found, "on PATH: " + strings.Join(found, ", "), true
	case 1:
		return found, absent[0] + " is not on PATH", false
	}
	return found, strings.Join(absent, ", ") + " are not on PATH", false
}

// envValue is name's value in env, the last assignment winning as it does for the process env
// starts, and "" when env holds none.
func envValue(env []string, name string) string {
	prefix := name + "="
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], prefix); ok {
			return v
		}
	}
	return ""
}

// envMap is env as a process reads it: each NAME=value pair by name, the last assignment winning.
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, pair := range env {
		if name, value, ok := strings.Cut(pair, "="); ok {
			m[name] = value
		}
	}
	return m
}

// ran is one command's run: its exit code, what it printed, and the error when it could not run
// to an exit of its own — it never started, or commandTimeout killed it.
type ran struct {
	exit           int
	stdout, stderr string
	err            error
}

// output is what the command printed, stderr then stdout, as one line.
func (r ran) output() string { return oneLine(r.stderr + "\n" + r.stdout) }

// failure is why a command did not succeed: the run's own error, or its exit and its output.
func (r ran) failure() string {
	if r.err != nil {
		return r.err.Error()
	}
	if out := r.output(); out != "" {
		return fmt.Sprintf("exited %d: %s", r.exit, out)
	}
	return fmt.Sprintf("exited %d with no output", r.exit)
}

// launch runs `exec <Launch> <args>` under `sh -c`, so the shell parses the launch as a pod's shim
// has it parsed, and exec replaces the shell with Oh My Pi, so a kill reaches it.
func (img Image) launch(ctx context.Context, args string) ran {
	return img.run(ctx, "sh", "-c", "exec "+img.Launch+" "+args)
}

// run runs one command within commandTimeout, under img's environment, in its directory, in a
// process group of its own, so a timeout kills what is actually running rather than a shell that
// leaves it holding the pipes (procgroup). Its stdin is empty: nothing a check runs may wait on
// a terminal.
func (img Image) run(ctx context.Context, name string, args ...string) ran {
	attempt, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(attempt, name, args...)
	cmd.Env = img.Env
	cmd.Dir = img.WorkDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	procgroup.Configure(cmd)
	err := cmd.Run()
	r := ran{stdout: stdout.String(), stderr: stderr.String()}
	switch {
	case ctx.Err() == nil && errors.Is(attempt.Err(), context.DeadlineExceeded):
		r.err = fmt.Errorf("timed out after %s", commandTimeout)
	case procgroup.Err(err) != nil:
		r.err = procgroup.Err(err)
	default:
		r.exit = cmd.ProcessState.ExitCode()
	}
	return r
}

// oneLine is text as one printed line: its whitespace runs collapsed, and cut at maxDetail on a
// rune boundary with an ellipsis after the cut.
func oneLine(text string) string {
	line := strings.Join(strings.Fields(text), " ")
	if len(line) > maxDetail {
		line = truncate(line, maxDetail) + "…"
	}
	return line
}
