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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
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
}

// Line is one printed row of the table.
type Line struct {
	Name Name
	// Status is "present" or "missing" for an image row, "live", "reported" or "withheld" for
	// the others.
	Status string
	// Detail is the row's evidence: what was run and what it answered, the check that proves a
	// live row, who reports a deployment row, or the ruling that withholds one.
	Detail string
}

// The statuses a Line carries.
const (
	present  = "present"
	missing  = "missing"
	live     = "live"
	reported = "reported"
	withheld = "withheld"
)

// String is the line as the probe prints it, and as the probe pod's log then carries it.
func (l Line) String() string {
	return fmt.Sprintf("probe-image: capability %s: %s (%s)", l.Name, l.Status, l.Detail)
}

// commandTimeout bounds each command a check runs: a `--version` or a `--check` that has not
// answered in a minute is hanging, not slow, and the probe's own budget is not to be spent on it.
const commandTimeout = 60 * time.Second

// maxDetail caps a command's quoted output in a Line, so a chatty command cannot push the table
// past the log tail the daemon reads back.
const maxDetail = 300

// imageCheck is one image row's check: its evidence, and whether the capability is present.
type imageCheck func(ctx context.Context, img Image) (detail string, ok bool)

// imageChecks are the image rows' checks, by name. TestEveryImageRowHasACheck holds the map to
// Table's image rows, so a row added to one is added to the other.
var imageChecks = map[Name]imageCheck{
	EvalJS: evalJS, EvalPython: evalPython, Browser: browser, LSP: lsp, CodeGraph: codeGraph, Skills: skills, Toolchain: toolchain,
}

// CheckImage checks every image-site row under img and renders every row of Table, in its order:
// image rows present or missing with their evidence, live rows as live naming the check,
// deployment rows as reported by the daemon, withheld rows with their ruling. It runs after the
// launch probes (daemon.ProbeImage) passed: the eval-js and skills rows are present by those
// probes, which ran Oh My Pi — its own JavaScript runtime — and resolved the prompt-named skills.
// err is nil when every image row is present; otherwise it names every missing one, so one
// rebuild of the image fixes them all rather than the first alone. A ctx that ends ends the check
// with an error wrapping ctx's.
func CheckImage(ctx context.Context, img Image) ([]Line, error) {
	lines := make([]Line, 0, len(Table))
	var absent []string
	for _, row := range Table {
		line := Line{Name: row.Name}
		switch row.Site {
		case SiteImage:
			detail, ok := imageChecks[row.Name](ctx, img)
			if ctx.Err() != nil {
				return lines, fmt.Errorf("capability check: %w", ctx.Err())
			}
			line.Status, line.Detail = missing, detail
			if ok {
				line.Status = present
			} else {
				absent = append(absent, fmt.Sprintf("capability %s is missing: %s", row.Name, detail))
			}
		case SiteLive:
			line.Status, line.Detail = live, liveDetail(row)
		case SiteDeployment:
			line.Status, line.Detail = reported, "the daemon reports it from the deployment's configuration: "+row.Summary
		case SiteWithheld:
			line.Status, line.Detail = withheld, withheldDetail(row)
		}
		lines = append(lines, line)
	}
	if len(absent) > 0 {
		return lines, errors.New(strings.Join(absent, "; "))
	}
	return lines, nil
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
		found, ok := lookPath(img.Env, "chromium")
		if !ok {
			return "chromium is not on PATH, and PUPPETEER_EXECUTABLE_PATH names no executable", false
		}
		path, how = found, "chromium on PATH"
	case !strings.Contains(path, "/"):
		// A bare name: Oh My Pi spawns it, and the spawn looks it up on PATH.
		found, ok := lookPath(img.Env, path)
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

// codeGraphPlugin is the Oh My Pi plugin that fronts the CodeGraph CLI.
const codeGraphPlugin = "@bopstack/pi-codegraph"

// codeGraph needs both halves: the CodeGraph CLI on PATH, and its plugin enabled in the profile's
// plugin lock, where `omp plugin install` recorded it; a CLI without the plugin, or a plugin
// disabled behind the CLI, gives an agent no codegraph tool. Both failures are named at once.
func codeGraph(_ context.Context, img Image) (string, bool) {
	var evidence, why []string
	if path, ok := lookPath(img.Env, "codegraph"); ok {
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
// whose own toolchain download or GOTOOLCHAIN setting can make a present binary fail to run.
func toolchain(ctx context.Context, img Image) (string, bool) {
	paths, detail, ok := onPath(img, "go", "curl", "wget", "python3", "node", "bun", "uv")
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
	var found, absent []string
	for _, name := range names {
		if path, ok := lookPath(img.Env, name); ok {
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

// lookPath resolves name on env's PATH, as the pod's Oh My Pi would, rather than on this
// process's: the checks run where the pod's Oh My Pi runs, but it is the environment the pod
// baseline and the operator's pod env shaped that decides what a worker finds. An executable is
// a regular file with an execute bit, as exec.LookPath judges it; a relative PATH element is
// skipped, as exec.LookPath refuses the result it would give.
func lookPath(env []string, name string) (string, bool) {
	for _, dir := range filepath.SplitList(envValue(env, "PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return path, true
		}
	}
	return "", false
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
// rune boundary.
func oneLine(text string) string {
	line := strings.Join(strings.Fields(text), " ")
	if len(line) > maxDetail {
		cut := maxDetail
		for cut > 0 && !utf8.RuneStart(line[cut]) {
			cut--
		}
		line = line[:cut] + "…"
	}
	return line
}
