package tmux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
)

// maxWindowNameLength bounds a window name well inside tmux's own limit (runtime-tmux.ts:41).
const maxWindowNameLength = 160

// treeName is the window an issue's panes share: the key lowercased, or when too long, a prefix
// and a hash of the whole (runtime-tmux.ts:43-49).
func treeName(issue string) string {
	full := strings.ToLower(issue)
	if len(full) <= maxWindowNameLength {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := hex.EncodeToString(sum[:])[:16]
	return full[:maxWindowNameLength-len(suffix)-1] + "-" + suffix
}

// envName is a name a shell accepts as a variable.
var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// runtimeOwned are the variables every pane gets from the runtime itself. A spec that sets one
// is refused rather than one silently winning.
var runtimeOwned = map[string]bool{
	"LEGION_DAEMON_API": true, "LEGION_TREE": true, "LEGION_ISSUE": true, "LEGION_ROLE": true,
	"LEGION_GENERATION": true, "LEGION_PROJECT": true, "LEGION_DAEMON_URL": true,
	"LEGION_STATE_DIR": true, "LEGION_WORKSPACE": true, "ENVOY_NATS_URL": true, "ENVOY_URL": true,
	"GIT_TERMINAL_PROMPT": true, "XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true,
	"XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "LEGION_BOOT_TOKEN_FILE": true,
	"LEGION_GH_PATH": true, "LEGION_GIT_PATH": true, "LEGION_JJ_PATH": true,
	"DISPATCH_URL": true, "DISPATCH_TOKEN_FILE": true, "PI_SHELL_PREFIX": true,
}

// validateSpawnSpec refuses a spec the runtime cannot honour exactly, before anything touches the
// disk or tmux. Above all, nothing may reach a pane as a -e value that is a secret, or that
// overrides a variable the runtime sets: a secret travels in Secrets and reaches the pane as a
// 0600 file and a `<NAME>_FILE` pointer, never as a value in tmux's argv.
//
// providerKeys are the names the shim will export into OMP's environment from the provider-env
// directory. The shim skips a NAME whose NAME_FILE pointer the pane carries and refuses to start
// over a NAME the pane already carries (internal/shim/config.go:59-98), so a spec that collides
// with one is refused here rather than launch a pane without its key.
func validateSpawnSpec(spec runtime.SpawnSpec, providerKeys []string) error {
	if spec.Claim == "" {
		return errors.New("spawn: no claim token")
	}
	token := string(spec.Claim)
	if token == "." || token == ".." || strings.ContainsAny(token, "/\x00") {
		return fmt.Errorf("spawn %q: the claim token names the pane's secret files and must be one file name", token)
	}
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("spawn %s: "+format, append([]any{token}, args...)...)
	}
	for _, field := range []struct{ name, value string }{
		{"project", spec.Project}, {"tree", spec.Tree}, {"issue", spec.Issue},
		{"role", string(spec.Role)}, {"boot token", spec.BootToken}, {"workspace", spec.Workspace},
	} {
		if field.value == "" {
			return refuse("no %s", field.name)
		}
	}
	if !filepath.IsAbs(spec.Workspace) {
		return refuse("workspace %q is not an absolute path", spec.Workspace)
	}
	if len(spec.Prompt.RolePromptPaths) == 0 {
		return refuse("no role prompt")
	}
	for _, name := range sortedKeys(spec.Env) {
		switch {
		case !envName.MatchString(name):
			return refuse("Env name %q is not an environment variable name", name)
		case runtimeOwned[name]:
			return refuse("Env sets %s, which the runtime sets itself", name)
		case isSecretLikeName(name) && !strings.HasSuffix(name, "_FILE"):
			return refuse("Env carries %s, a credential-shaped name; a secret travels in Secrets, as a file", name)
		}
	}
	for _, name := range sortedKeys(spec.Secrets) {
		pointer := name + "_FILE"
		switch {
		case !envName.MatchString(name):
			return refuse("secret %q is not an environment variable name", name)
		case runtimeOwned[pointer]:
			return refuse("secret %s's pointer %s is a variable the runtime sets itself", name, pointer)
		case spec.Env[pointer] != "":
			return refuse("secret %s's pointer %s is also set in Env", name, pointer)
		}
	}
	for _, key := range providerKeys {
		_, inEnv := spec.Env[key]
		_, isSecret := spec.Secrets[key]
		_, pointerInEnv := spec.Env[key+"_FILE"]
		switch {
		case inEnv:
			return refuse("provider key %s is also set in Env", key)
		case isSecret || pointerInEnv:
			return refuse("provider key %s would not reach OMP: the pane carries %s_FILE", key, key)
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// secretFile is one secret's file: the variable its pointer is named after, and the path.
type secretFile struct {
	name, path, value string
}

// secretFiles are the files a spec's secrets are written to, under `<state_dir>/secrets`: the
// boot token as `<claim>`, each other secret as `<claim>-<lowercased name>`, so one claim token
// names every file its pane holds (runtime-tmux.ts:619-635, secrets.ts:63-69). The boot token's
// pointer is LEGION_BOOT_TOKEN_FILE and comes first; the rest follow sorted by name.
func secretFiles(stateDir string, spec runtime.SpawnSpec) []secretFile {
	dir := filepath.Join(stateDir, "secrets")
	files := []secretFile{{name: "LEGION_BOOT_TOKEN", path: filepath.Join(dir, string(spec.Claim)), value: spec.BootToken}}
	for _, name := range sortedKeys(spec.Secrets) {
		files = append(files, secretFile{
			name:  name,
			path:  filepath.Join(dir, string(spec.Claim)+"-"+strings.ToLower(name)),
			value: spec.Secrets[name],
		})
	}
	return files
}

// writeSecretFiles writes each secret to its 0600 file in a 0700 directory, re-applying both modes
// on every write — a directory's mkdir mode is masked and ignored when it exists, and a file's is
// applied only on create (secrets.ts:83-98). The caller owns the files' lifetimes.
func writeSecretFiles(stateDir string, files []secretFile) error {
	dir := filepath.Join(stateDir, "secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	for _, file := range files {
		if err := os.WriteFile(file.path, []byte(file.value), 0o600); err != nil {
			return fmt.Errorf("write the %s file: %w", file.name, err)
		}
		if err := os.Chmod(file.path, 0o600); err != nil {
			return fmt.Errorf("write the %s file: %w", file.name, err)
		}
	}
	return nil
}

// DispatchTokenFileName is the daemon-held Dispatch bearer every pane reads through
// DISPATCH_TOKEN_FILE, as the shipped daemon names it under `<state_dir>/secrets`.
const DispatchTokenFileName = "dispatch-token"

// WriteDispatchTokenFile writes the Dispatch bearer as a 0600 file in the 0700 secrets directory,
// returning the path every pane receives. The value never enters tmux's argv or a pane's
// environment.
func WriteDispatchTokenFile(stateDir, token string) (string, error) {
	path := filepath.Join(stateDir, "secrets", DispatchTokenFileName)
	if err := writeSecretFiles(stateDir, []secretFile{{name: "DISPATCH_TOKEN", path: path, value: token}}); err != nil {
		return "", err
	}
	return path, nil
}

// paneInputs are the runtime's own values a pane's -e pairs carry.
type paneInputs struct {
	stateDir, daemonURL, envoyURL, dispatchURL, dispatchTokenFile string
	natsURLs                                                      []string
	// tools are the daemon-resolved gh, git, and jj, keyed by the variable that names each.
	tools map[string]string
}

// panePairs are a pane's -e pairs, in one order: the variables every Legion pane is told (the
// shipped set, processes.ts:4562-4579, LEGION_DAEMON_API=go, which picks the plugin's Go client,
// and PI_SHELL_PREFIX, which keeps this daemon's gh and legion first in the agent's bash tool),
// the four XDG base directories under `<state_dir>/home`, the spec's own variables sorted, then a
// `<NAME>_FILE` pointer per secret file. PATH is never among them — tmux would replace it
// (LEGION-91) — and neither is any secret's value.
func panePairs(spec runtime.SpawnSpec, in paneInputs, files []secretFile) []string {
	var pairs []string
	add := func(name, value string) { pairs = append(pairs, "-e", name+"="+value) }
	add("LEGION_DAEMON_API", "go")
	add("LEGION_TREE", spec.Tree)
	add("LEGION_ISSUE", spec.Issue)
	add("LEGION_ROLE", string(spec.Role))
	add("LEGION_GENERATION", strconv.FormatUint(spec.Generation, 10))
	add("LEGION_PROJECT", spec.Project)
	add("LEGION_DAEMON_URL", in.daemonURL)
	add("LEGION_STATE_DIR", in.stateDir)
	add("LEGION_WORKSPACE", spec.Workspace)
	if len(in.natsURLs) > 0 {
		add("ENVOY_NATS_URL", strings.Join(in.natsURLs, ","))
	}
	add("ENVOY_URL", in.envoyURL)
	if in.dispatchURL != "" {
		add("DISPATCH_URL", in.dispatchURL)
		add("DISPATCH_TOKEN_FILE", in.dispatchTokenFile)
	}
	for _, name := range slices.Sorted(maps.Keys(in.tools)) {
		add(name, in.tools[name])
	}
	add("PI_SHELL_PREFIX", shellprefix.For(workerBinDir(in.stateDir), legionBinDir(in.stateDir)))
	add("GIT_TERMINAL_PROMPT", "0")
	for _, dir := range xdgDirectories(in.stateDir) {
		add(dir[0], dir[1])
	}
	for _, name := range sortedKeys(spec.Env) {
		if name != "PATH" {
			add(name, spec.Env[name])
		}
	}
	for _, file := range files {
		add(file.name+"_FILE", file.path)
	}
	return pairs
}

// paneReport is what a `-P -F` report names: the new window (new-window only), the pane, its pid.
type paneReport struct {
	window, pane string
	pid          int
}

var (
	windowID = regexp.MustCompile(`^@\d+$`)
	paneID   = regexp.MustCompile(`^%\d+$`)
)

// readPaneReport reads new-window's `"#{window_id} #{pane_id} #{pane_pid}"` or split-window's
// `"#{pane_id} #{pane_pid}"`. A malformed report is a launch failure naming the command and
// tmux's stderr, never the report itself (tmux.ts:234-260).
func readPaneReport(command string, res result, expectWindow bool) (paneReport, error) {
	tokens := strings.Fields(res.stdout)
	var report paneReport
	if expectWindow {
		if len(tokens) == 0 || !windowID.MatchString(tokens[0]) {
			return paneReport{}, fmt.Errorf("tmux %s did not report a window id%s", command, failure(res))
		}
		report.window, tokens = tokens[0], tokens[1:]
	}
	if len(tokens) == 0 || !paneID.MatchString(tokens[0]) {
		return paneReport{}, fmt.Errorf("tmux %s did not report a pane id%s", command, failure(res))
	}
	report.pane = tokens[0]
	if len(tokens) > 1 {
		report.pid, _ = strconv.Atoi(tokens[1])
	}
	if report.pid <= 0 {
		return paneReport{}, fmt.Errorf("tmux %s did not report a pane pid%s", command, failure(res))
	}
	return report, nil
}

// Spawn starts a fresh agent for spec's claim: the pane's secrets written as files, then — under
// the launch lock — a pane in the issue's window, or a new window for it, running
// `legion worker-shim` around OMP. The returned locator carries the incarnation read at launch.
// A spec with a ResumeSessionFile is refused: continuing a recorded agent is Resume's, so there is
// one path to `--resume`.
func (r *Runtime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile != "" {
		return runtime.Locator{}, fmt.Errorf("spawn %s: a ResumeSessionFile is Resume's to set", spec.Claim)
	}
	return r.launch(ctx, spec)
}

// Resume starts the agent loc's claim recorded, again, from spec.ResumeSessionFile — after
// waiting, up to the stop grace, for loc's incarnation to be gone: one claim, one process. The
// exact zero Locator means no previous incarnation is known (a claim suspended across a daemon
// restart) and there is nothing to wait for. A session file that is missing is a refusal, never a
// fresh agent: the claim resumes the agent it recorded or none (runtime.ts:331-356).
func (r *Runtime) Resume(ctx context.Context, loc runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile == "" {
		return runtime.Locator{}, fmt.Errorf("resume %s: no session file to resume from", spec.Claim)
	}
	if loc != (runtime.Locator{}) {
		if loc.Claim != spec.Claim {
			return runtime.Locator{}, fmt.Errorf("resume %s: the previous locator is %s's", spec.Claim, loc.Claim)
		}
		if err := r.awaitGone(ctx, loc); err != nil {
			return runtime.Locator{}, err
		}
	}
	return r.launch(ctx, spec)
}

// awaitGone waits, up to the stop grace, until loc's incarnation is no longer its pane's process,
// then stops watching it.
func (r *Runtime) awaitGone(ctx context.Context, loc runtime.Locator) error {
	pane, inc, err := paneOf(loc)
	if err != nil {
		return fmt.Errorf("resume %s: %w", loc.Claim, err)
	}
	v, err := r.verifyPane(ctx, pane, inc)
	if err != nil {
		return fmt.Errorf("resume %s: %w", loc.Claim, err)
	}
	if v.verified() || v.reason == reasonListingFailed {
		if v, err = r.awaitNotRunning(ctx, pane, inc, r.stopGrace); err != nil {
			return fmt.Errorf("resume %s: %w", loc.Claim, err)
		}
	}
	switch v.reason {
	case reasonVerified:
		return fmt.Errorf("resume %s: its previous incarnation %s is still running in pane %s", loc.Claim, loc.Incarnation, pane)
	case reasonListingFailed:
		return fmt.Errorf("resume %s: cannot confirm its previous incarnation is gone: %s", loc.Claim, describeVerdict(pane, inc, v))
	}
	r.untrack(loc)
	return nil
}

// launch is Spawn's and Resume's shared half: every check that needs no tmux, the secret files,
// then the pane.
func (r *Runtime) launch(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if err := validateSpawnSpec(spec, r.providerKeys); err != nil {
		return runtime.Locator{}, err
	}
	for _, path := range append(append([]string{}, spec.Prompt.RolePromptPaths...), spec.Prompt.DeploymentInstructionsPath) {
		if path == "" {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			return runtime.Locator{}, fmt.Errorf("spawn %s: prompt file: %w", spec.Claim, err)
		}
	}
	if info, err := os.Stat(spec.Workspace); err != nil || !info.IsDir() {
		return runtime.Locator{}, fmt.Errorf("spawn %s: workspace %s is not a directory", spec.Claim, spec.Workspace)
	}
	if spec.ResumeSessionFile != "" {
		if _, err := os.Stat(spec.ResumeSessionFile); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				verb := "respawning"
				if spec.Issue == spec.Tree && spec.Role == claim.RoleArchitect {
					verb = "resurrecting"
				}
				return runtime.Locator{}, fmt.Errorf("Refusing to start %s fresh while %s: recorded OMP session file is missing: %s",
					spec.Issue, verb, spec.ResumeSessionFile)
			}
			return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
		}
		r.log.Info("tmux runtime: resuming a recorded OMP session", "claim", spec.Claim, "session", spec.ResumeSessionFile)
	}
	for _, dir := range xdgDirectories(r.stateDir) {
		if err := os.MkdirAll(dir[1], 0o700); err != nil {
			return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
		}
	}
	files := secretFiles(r.stateDir, spec)
	if err := writeSecretFiles(r.stateDir, files); err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	path := r.paneEnv["PATH"]
	if spec.Env["PATH"] != "" {
		path = spec.Env["PATH"]
	}
	path = workerPath(path, r.stateDir)
	inner := innerCommand(r.ompPrefix, r.ompInvocation, spec.ResumeSessionFile, spec.Prompt)
	command := shimShellCommand(r.socket, path, spec.Workspace, r.legion, r.streamAddress, files[0].path, r.providerEnvDir, inner)
	pairs := panePairs(spec, paneInputs{
		stateDir: r.stateDir, daemonURL: r.daemonURL, envoyURL: r.envoyURL, natsURLs: r.natsURLs,
		dispatchURL: r.dispatchURL, dispatchTokenFile: r.dispatchToken, tools: r.tools,
	}, files)
	return r.openPane(ctx, spec, paneCommand(pairs, command))
}

// openPane puts the pane on the server under the launch lock: into the window of a pane of the
// same issue that still verifies as its recorded process, else into a new window for the issue.
// The pane is recorded — its incarnation read, the process watched — before the lock is released,
// so the next spawn on the issue can share its window and no orphan sweep mistakes it for one.
func (r *Runtime) openPane(ctx context.Context, spec runtime.SpawnSpec, pane []string) (runtime.Locator, error) {
	r.launchMu.Lock()
	defer r.launchMu.Unlock()

	if err := r.refuseLiveIncarnation(ctx, spec.Claim); err != nil {
		return runtime.Locator{}, err
	}
	window, err := r.issueWindow(ctx, spec.Issue)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	var report paneReport
	if window != "" {
		report, err = r.splitWindow(ctx, window, pane)
	} else {
		report, err = r.openWindow(ctx, treeName(spec.Issue), pane)
	}
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	ticks, alive, err := r.startTicks(report.pid)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	if !alive {
		// The pane closes itself with its process; there is nothing left to record or reap.
		return runtime.Locator{}, fmt.Errorf("spawn %s: pane %s exited before its process identity could be recorded (/proc/%d/stat unreadable)",
			spec.Claim, report.pane, report.pid)
	}
	loc := runtime.Locator{
		Runtime:     runtime.RuntimeTmux,
		Claim:       spec.Claim,
		Incarnation: incarnation{pid: report.pid, ticks: ticks}.String(),
		Tmux:        &runtime.TmuxLocator{Window: report.window, Pane: report.pane},
	}
	r.track(loc, spec.Issue)
	return loc, nil
}

// refuseLiveIncarnation refuses a launch for a claim whose watched process still runs: one claim,
// one process. A watched process that no longer verifies is dropped; one that cannot be verified
// blocks the launch, since it may be running.
func (r *Runtime) refuseLiveIncarnation(ctx context.Context, token claim.Token) error {
	for _, entry := range r.trackedProcesses() {
		if entry.locator.Claim != token {
			continue
		}
		pane, inc, err := paneOf(entry.locator)
		if err != nil {
			return err
		}
		v, err := r.verifyPane(ctx, pane, inc)
		if err != nil {
			return fmt.Errorf("spawn %s: %w", token, err)
		}
		switch v.reason {
		case reasonVerified:
			return fmt.Errorf("spawn %s: the claim's process %s is still running in pane %s; stop or resume it", token, entry.locator.Incarnation, pane)
		case reasonListingFailed:
			return fmt.Errorf("spawn %s: cannot tell whether the claim's process %s still runs: %s", token, entry.locator.Incarnation, describeVerdict(pane, inc, v))
		}
		r.untrack(entry.locator)
	}
	return nil
}

// issueWindow is the window a new pane for issue splits into: the window of the first watched pane
// on the issue that still verifies as its recorded process (runtime-tmux.ts:664-691). A live
// window alone proves nothing — after a server is recreated the same @N names another issue's
// window — and a pane that cannot be verified is simply not shared; a fresh window costs nothing
// but a window. "" is a new window.
func (r *Runtime) issueWindow(ctx context.Context, issue string) (string, error) {
	for _, entry := range r.trackedProcesses() {
		if entry.issue != issue {
			continue
		}
		pane, inc, err := paneOf(entry.locator)
		if err != nil {
			return "", err
		}
		v, err := r.verifyPane(ctx, pane, inc)
		if err != nil {
			return "", err
		}
		if v.verified() {
			return entry.locator.Tmux.Window, nil
		}
	}
	return "", nil
}

// ensureSession makes the private session exist, returning true only when this call created it —
// its caller then owns killing the bootstrap window once its own window is open (tmux.ts:262-300).
// The bootstrap pane is deliberately unmarked: reconciliation reaps marked daemon windows, not
// the private server's own placeholder or a human pane. The launch lock serializes callers.
//
// Unlike the shipped ensureSession, the session is not marked with the owner option. tmux reads
// `#{@legion_owner}` for a window or pane by falling back from the window's options to its
// session's, so a marked session made every window in it read as the daemon's — a window a human
// opened by hand included, which reconciliation then reaped. Only the windows the daemon opens
// carry the marker.
func (r *Runtime) ensureSession(ctx context.Context) (bool, error) {
	res, err := r.run(ctx, hasSessionArgv(r.socket, r.socket))
	if err != nil {
		return false, err
	}
	if res.exitCode == 0 {
		return false, nil
	}
	res, err = r.run(ctx, newSessionArgv(r.socket, r.socket))
	if err != nil {
		return false, err
	}
	if res.exitCode != 0 {
		return false, fmt.Errorf("tmux new-session failed (exit %d)%s", res.exitCode, failure(res))
	}
	return true, nil
}

// openWindow opens a window named name running pane, in the private session, which it makes
// exist first. If the window fails and the session turns out to have vanished in between — its
// last pane exiting as this one opened — the session is recreated and the window tried once more;
// any other failure is the launch's (runtime-tmux.ts:313-359).
func (r *Runtime) openWindow(ctx context.Context, name string, pane []string) (paneReport, error) {
	created, err := r.ensureSession(ctx)
	if err != nil {
		return paneReport{}, err
	}
	report, first := r.openWindowIn(ctx, name, pane, created)
	if first == nil {
		return report, nil
	}
	if created && !noServerStderr.MatchString(first.Error()) {
		return paneReport{}, first
	}
	recreated, err := r.ensureSession(ctx)
	if err != nil {
		return paneReport{}, err
	}
	if !recreated {
		return paneReport{}, first
	}
	report, err = r.openWindowIn(ctx, name, pane, true)
	if err != nil {
		return paneReport{}, fmt.Errorf("%w on the second attempt, after the session vanished and was recreated (first attempt: %v)", err, first)
	}
	r.log.Warn("tmux runtime: the session vanished as a window opened; recreated it and opened the window on a second attempt",
		"window", name, "first", first.Error())
	return report, nil
}

// openWindowIn is one new-window: the creator of the session kills its bootstrap window right
// after, before the result is read. The pane marks its own window before it starts the shim, so a
// crash that loses the report still leaves a reapable owner marker; after the report this method
// repeats the marker and treats a failure as a launch failure (tmux.ts:197-226, 302-357).
func (r *Runtime) openWindowIn(ctx context.Context, name string, pane []string, createdSession bool) (paneReport, error) {
	res, err := r.run(ctx, newWindowArgv(r.socket, r.socket, name, pane))
	if err != nil {
		return paneReport{}, err
	}
	if createdSession {
		cleanup, err := r.run(ctx, killBootstrapWindowArgv(r.socket, r.socket))
		if err != nil {
			return paneReport{}, err
		}
		if cleanup.exitCode != 0 {
			return paneReport{}, fmt.Errorf("tmux bootstrap window cleanup failed (exit %d)%s", cleanup.exitCode, failure(cleanup))
		}
	}
	if res.exitCode != 0 {
		return paneReport{}, fmt.Errorf("tmux new-window failed (exit %d)%s", res.exitCode, failure(res))
	}
	report, err := readPaneReport("new-window", res, true)
	if err != nil {
		return paneReport{}, err
	}
	marker, err := r.run(ctx, markWindowOwnerArgv(r.socket, report.window, r.socket))
	if err != nil {
		return paneReport{}, err
	}
	if marker.exitCode != 0 {
		if _, err := r.run(ctx, killWindowArgv(r.socket, report.window)); err != nil {
			return paneReport{}, err
		}
		return paneReport{}, fmt.Errorf("tmux window ownership marker failed (exit %d)%s", marker.exitCode, failure(marker))
	}
	return report, nil
}

// splitWindow splits pane into window and tiles the layout (tmux.ts:359-384).
func (r *Runtime) splitWindow(ctx context.Context, window string, pane []string) (paneReport, error) {
	res, err := r.run(ctx, splitWindowArgv(r.socket, window, pane))
	if err != nil {
		return paneReport{}, err
	}
	if res.exitCode != 0 {
		return paneReport{}, fmt.Errorf("tmux split-window failed (exit %d)%s", res.exitCode, failure(res))
	}
	report, err := readPaneReport("split-window", res, false)
	if err != nil {
		return paneReport{}, err
	}
	report.window = window
	if res, err := r.run(ctx, selectLayoutArgv(r.socket, window)); err != nil {
		return paneReport{}, err
	} else if res.exitCode != 0 {
		r.log.Warn("tmux runtime: select-layout failed after a split", "window", window, "detail", failure(res))
	}
	return report, nil
}
