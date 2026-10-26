package tmux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
	"github.com/sjawhar/legion/daemon/internal/runtime/workerbin"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// maxWindowNameLength bounds a window name well inside tmux's own limit
// (MAX_TMUX_WINDOW_NAME_LENGTH, runtime-tmux.ts).
const maxWindowNameLength = 160

// treeName is the window an issue's panes share: the key lowercased, or when too long, a prefix
// and a hash of the whole (treeName, runtime-tmux.ts).
func treeName(issue string) string {
	full := strings.ToLower(issue)
	if len(full) <= maxWindowNameLength {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := hex.EncodeToString(sum[:])[:16]
	return full[:maxWindowNameLength-len(suffix)-1] + "-" + suffix
}

// controllerWindow is the window the daemon's own controller (`controller: daemon`) runs in, and
// the working directory under the state directory it starts in: it works on no issue, so it shares
// no issue's window, and no issue key, which always holds a `-`, is ever this name.
const controllerWindow = "controller"

// windowKey is what spec's pane shares a window by: its issue, or the controller's own window.
func windowKey(spec runtime.SpawnSpec) string {
	if spec.Role == claim.RoleController {
		return controllerWindow
	}
	return spec.Issue
}

// runtimeOwned are the variables every pane gets from the runtime itself: exactly the names
// panePairs sets for a tree pane (TestRuntimeOwnedIsWhatEveryPaneIsToldByTheRuntime), which the
// shared validator refuses in a spec's Env and as a secret's pointer. A controller pane is told
// controllerOwned in place of the tree, issue and workspace.
var runtimeOwned = map[string]bool{
	"LEGION_TREE": true, "LEGION_ISSUE": true, "LEGION_ROLE": true,
	"LEGION_GENERATION": true, "LEGION_PROJECT": true, "LEGION_DAEMON_URL": true,
	"LEGION_STATE_DIR": true, "LEGION_WORKSPACE": true, "ENVOY_NATS_URL": true, "ENVOY_URL": true,
	"GIT_TERMINAL_PROMPT": true, "XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true, "XDG_DATA_HOME": true,
	"XDG_STATE_HOME": true, "LEGION_BOOT_TOKEN_FILE": true, "DISPATCH_URL": true, "DISPATCH_TOKEN_FILE": true,
	"GH_CONFIG_DIR": true, "GH_TOKEN": true, "GITHUB_TOKEN": true, "GH_HOST": true,
	"LEGION_GH_OWNERS_DIR": true, "LEGION_GH_PATH": true,
	"PI_SHELL_PREFIX": true, "PI_CONFIG_FILES": true,
}

// controllerOwned are the variables a controller pane is told in place of a tree pane's
// LEGION_TREE, LEGION_ISSUE and LEGION_WORKSPACE: the controller marker the plugin classifies the
// session by, and LEGION_SLACK_CHANNELS, which the plugin requires of the controller the daemon
// launches — always empty here, since only a controller enrolled with the secrets broker
// (runtime.kubernetes.agent_secrets) may have Slack channels (config.checkSlackChannels).
var controllerOwned = map[string]bool{"LEGION_CONTROLLER": true, "LEGION_SLACK_CHANNELS": true}

// ownedNames is every variable the runtime sets on some pane itself: a spec may set none of them.
var ownedNames = func() map[string]bool {
	owned := map[string]bool{}
	for _, set := range []map[string]bool{runtimeOwned, controllerOwned} {
		for name := range set {
			owned[name] = true
		}
	}
	return owned
}()

// validateSpawnSpec refuses a spec the runtime cannot honour exactly, before anything touches the
// disk or tmux: the shared refusal (runtime.ValidateSpawnSpec) — nothing may reach a pane as a -e
// value that is a secret, or that overrides a variable the runtime sets — then tmux's own. A claim
// token names the pane's secret files, so it is one file name.
//
// providerKeys are the names the shim will export into OMP's environment from the provider-env
// directory. The shim skips a NAME whose NAME_FILE pointer the pane carries and refuses to start
// over a NAME the pane already carries (internal/shim/config.go:59-98), so a spec that collides
// with one is refused here rather than launch a pane without its key. operatorEnv is the
// operator's pane variables (Options.PaneEnv): a spec variable or secret pointer of the same name
// would have one of the two silently win, so it is refused too.
func validateSpawnSpec(spec runtime.SpawnSpec, providerKeys []string, operatorEnv map[string]string) error {
	if err := runtime.ValidateSpawnSpec(spec, ownedNames); err != nil {
		return err
	}
	// The token names the pane's secret files, and it is one file name: the shared validation
	// above holds it to the one claim.NewToken derives, which has no separator in it.
	token := string(spec.Claim)
	refuse := func(format string, args ...any) error {
		return fmt.Errorf("spawn %s: "+format, append([]any{token}, args...)...)
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
	for _, name := range sortedKeys(spec.Env) {
		if _, set := operatorEnv[name]; set {
			return refuse("Env sets %s, which runtime.tmux.pane_env sets on every pane", name)
		}
	}
	for _, name := range sortedKeys(spec.Secrets) {
		if _, set := operatorEnv[name+"_FILE"]; set {
			return refuse("secret %s's pointer %s_FILE is set on every pane by runtime.tmux.pane_env", name, name)
		}
	}
	return nil
}

// workspaceDir is where spec's agent works, under the daemon's state directory: the issue's jj
// workspace the daemon provisioned at `<state_dir>/workspaces/<owner>/<repo>/<issue>`
// (workspace.Location), which must exist; for a configuration with no repository,
// `<state_dir>/workspaces/<issue>`, made here; and for the controller, which works no checkout,
// `<state_dir>/controller`, made here.
func (r *Runtime) workspaceDir(spec runtime.SpawnSpec) (string, error) {
	if spec.Role == claim.RoleController || spec.Repository.IsZero() {
		dir := filepath.Join(r.stateDir, "workspaces", spec.Issue)
		if spec.Role == claim.RoleController {
			dir = filepath.Join(r.stateDir, controllerWindow)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return "", fmt.Errorf("the workspace: %w", err)
		}
		return dir, nil
	}
	working, err := workspace.Location(r.stateDir, spec.Repository, spec.Issue)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(working.Dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("workspace %s does not exist: the issue's workspace was never provisioned", working.Dir)
	case err != nil:
		return "", fmt.Errorf("the workspace: %w", err)
	case !info.IsDir():
		return "", fmt.Errorf("workspace %s is not a directory", working.Dir)
	}
	return working.Dir, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// secretFiles are the files a spec's secrets are written to, under `<state_dir>/secrets`: the
// boot token as `<claim>`, each other secret as `<claim>-<lowercased name>`, so one claim token
// names every file its pane holds (TmuxRuntime.paneEnvPairs, runtime-tmux.ts; extraSecretName,
// secrets.ts). The boot token's pointer is LEGION_BOOT_TOKEN_FILE and comes first; the rest follow
// sorted by name.
func secretFiles(stateDir string, spec runtime.SpawnSpec) []runtime.SecretFile {
	dir := runtime.SecretsDir(stateDir)
	files := []runtime.SecretFile{{Variable: "LEGION_BOOT_TOKEN", Path: filepath.Join(dir, string(spec.Claim)), Value: spec.BootToken}}
	for _, name := range sortedKeys(spec.Secrets) {
		files = append(files, runtime.SecretFile{
			Variable: name,
			Path:     filepath.Join(dir, string(spec.Claim)+"-"+strings.ToLower(name)),
			Value:    spec.Secrets[name],
		})
	}
	return files
}

// PaneInputs are the runtime's own values a pane's -e pairs carry.
type PaneInputs struct {
	StateDir, Workspace, DaemonURL, EnvoyURL, DispatchURL, DispatchTokenFile string
	NATSURLs                                                                 []string
	// GHOwnersDir is the claim's gh bundle root (runtime.GHConfigDir), the pane's
	// LEGION_GH_OWNERS_DIR, GHDefault the bundle's default owner, whose directory is the pane's
	// GH_CONFIG_DIR, and GH the real gh the pane's `legion gh` runs, its LEGION_GH_PATH; an empty
	// GHDefault is the controller of a project with no GitHub owner, whose GH_CONFIG_DIR is the
	// empty root itself, so its gh acts as nobody; an empty GHOwnersDir is none, and the pane is
	// then told no gh variable at all: every pane of a runtime whose GitHubCredential function is
	// nil.
	GHOwnersDir, GHDefault, GH string
	// OperatorEnv is the operator's pane variables (runtime.tmux.pane_env) and PiConfigFiles the
	// operator's Oh My Pi overlays (runtime.tmux.pi_config_files); nil for none.
	OperatorEnv   map[string]string
	PiConfigFiles []string
}

// panePairs are a pane's -e pairs, in one order: the variables every Legion pane is told (the
// identity — a tree pane's tree and issue, or the controller's marker LEGION_CONTROLLER=1 — the
// daemon, state and, for a tree pane, workspace variables; for the controller, its empty
// LEGION_SLACK_CHANNELS; the Envoy and Dispatch variables; for a pane with a GHOwnersDir,
// GH_CONFIG_DIR, its default owner's directory of gh files, with GH_TOKEN, GITHUB_TOKEN and GH_HOST
// set to the empty string, which gh ignores, so no token in the daemon's environment outranks the
// file — a non-empty GH_TOKEN would — then LEGION_GH_OWNERS_DIR, the bundle's root, and
// LEGION_GH_PATH, the real gh the `legion gh` wrapper first on the pane's PATH runs;
// PI_SHELL_PREFIX, which keeps this daemon's launcher directory (its legion and gh) first in the
// agent's bash tool; PI_CONFIG_FILES, the operator's overlays then the turn-scoping overlay
// writeTurnScopeOverlay writes (podsafety.TurnScopeOverlay), last so its keys outrank theirs), the
// four XDG base directories under `<state_dir>/home`, the operator's pane variables sorted, the
// spec's own variables sorted, then a `<NAME>_FILE` pointer per secret file. PATH is never among
// them — tmux would replace it (LEGION-91) — and neither is any secret's value. No other tool path
// is told: a pane's git and jj are whatever its PATH, the daemon's, gives.
func panePairs(spec runtime.SpawnSpec, in PaneInputs, files []runtime.SecretFile) []string {
	var pairs []string
	add := func(name, value string) { pairs = append(pairs, "-e", name+"="+value) }
	controller := spec.Role == claim.RoleController
	if controller {
		add("LEGION_CONTROLLER", "1")
	} else {
		add("LEGION_TREE", spec.Tree)
		add("LEGION_ISSUE", spec.Issue)
	}
	add("LEGION_ROLE", string(spec.Role))
	add("LEGION_GENERATION", strconv.FormatUint(spec.Generation, 10))
	add("LEGION_PROJECT", spec.Project)
	add("LEGION_DAEMON_URL", in.DaemonURL)
	add("LEGION_STATE_DIR", in.StateDir)
	if controller {
		add("LEGION_SLACK_CHANNELS", "")
	} else {
		add("LEGION_WORKSPACE", in.Workspace)
	}
	if len(in.NATSURLs) > 0 {
		add("ENVOY_NATS_URL", strings.Join(in.NATSURLs, ","))
	}
	add("ENVOY_URL", in.EnvoyURL)
	if in.DispatchURL != "" {
		add("DISPATCH_URL", in.DispatchURL)
		add("DISPATCH_TOKEN_FILE", in.DispatchTokenFile)
	}
	if in.GHOwnersDir != "" {
		add("GH_CONFIG_DIR", ghconfig.OwnerDir(in.GHOwnersDir, in.GHDefault))
		add("GH_TOKEN", "")
		add("GITHUB_TOKEN", "")
		add("GH_HOST", "")
		add("LEGION_GH_OWNERS_DIR", in.GHOwnersDir)
		add("LEGION_GH_PATH", in.GH)
	}
	add("PI_SHELL_PREFIX", shellprefix.For(workerbin.LauncherDir(in.StateDir)))
	overlays := append(slices.Clone(in.PiConfigFiles), filepath.Join(in.StateDir, podsafety.TurnScopeFile))
	add("PI_CONFIG_FILES", strings.Join(overlays, ":"))
	add("GIT_TERMINAL_PROMPT", "0")
	for _, dir := range xdgDirectories(in.StateDir) {
		add(dir[0], dir[1])
	}
	for _, name := range sortedKeys(in.OperatorEnv) {
		add(name, in.OperatorEnv[name])
	}
	for _, name := range sortedKeys(spec.Env) {
		if name != "PATH" {
			add(name, spec.Env[name])
		}
	}
	for _, file := range files {
		add(file.Variable+"_FILE", file.Path)
	}
	return pairs
}

// makePaneHome makes the four XDG base directories every pane is told, under the daemon's home.
func makePaneHome(stateDir string) error {
	for _, dir := range xdgDirectories(stateDir) {
		if err := os.MkdirAll(dir[1], 0o700); err != nil {
			return err
		}
	}
	return nil
}

// writeTurnScopeOverlay writes podsafety.TurnScopeOverlay under stateDir, read-only, so panePairs
// can name it last in a pane's PI_CONFIG_FILES, after the operator's own overlays
// (runtime.tmux.pi_config_files): the same overlay a pod's shim writes (podsafety.Apply), and the
// two keys supervise.Machine.Quiesce's own promise depends on regardless of runtime (LEGION-462),
// which no operator overlay outranks here.
func writeTurnScopeOverlay(stateDir string) error {
	return podsafety.WriteReadOnly(filepath.Join(stateDir, podsafety.TurnScopeFile), podsafety.TurnScopeOverlay)
}

// PaneVariables is a launch's environment without the launch, for a process this runtime does not
// start that must be told exactly what a pane is: the rigs under packages/pi-legion/scripts, which
// run Oh My Pi under it. It makes the pane's home as a launch does (makePaneHome) and writes the
// turn-scoping overlay as a launch does (writeTurnScopeOverlay), then returns the variables
// panePairs tells a pane for spec, NAME=value in their order, files being the secret files they
// point at.
func PaneVariables(spec runtime.SpawnSpec, in PaneInputs, files []runtime.SecretFile) ([]string, error) {
	if err := makePaneHome(in.StateDir); err != nil {
		return nil, err
	}
	if err := writeTurnScopeOverlay(in.StateDir); err != nil {
		return nil, err
	}
	pairs := panePairs(spec, in, files)
	variables := make([]string, 0, len(pairs)/2)
	for i := 1; i < len(pairs); i += 2 {
		variables = append(variables, pairs[i])
	}
	return variables, nil
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
// tmux's stderr, never the report itself (parsePaneReport, tmux.ts).
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
	return r.launch(ctx, spec, false)
}

// Resume starts the agent prev's claim recorded, again, from spec.ResumeSessionFile — after
// waiting, up to the stop grace, for prev's incarnation to be gone: one claim, one process. A nil
// prev means no previous incarnation is known (a claim suspended across a daemon restart) and
// there is nothing to wait for; under runtime.tmux.take_over, so does a prev of the runtime this
// one takes over (foreign), whose process this runtime cannot see, and without the key such a prev
// is refused, naming the key (paneOf). The session is resumed where it is: at its recorded path,
// or, by its name, in another working directory's folder of the recorded path's sessions directory
// or of the pane's own (sessionFileOf), where a session carried over from a pod lands. One found
// nowhere starts nothing. Over a foreign prev the error wraps runtime.ErrSessionFileMissing, which
// the claim's machine answers by forgetting the session and spawning a fresh agent: the session was
// another runtime's, and the operator chose the takeover. Over any other prev, or none, it is a
// refusal, never a fresh agent: the claim resumes the agent it recorded or none
// (assertResumeSessionFile, runtime.ts), and the refusal is charged as any launch failure is.
func (r *Runtime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile == "" {
		return runtime.Locator{}, fmt.Errorf("resume %s: no session file to resume from", spec.Claim)
	}
	if err := (runtime.Known{Claim: spec.Claim, Locator: prev}).Validate(); err != nil {
		return runtime.Locator{}, fmt.Errorf("resume: %w", err)
	}
	takenOver := prev != nil && r.foreign(*prev)
	if prev != nil && !takenOver {
		if err := r.awaitGone(ctx, *prev); err != nil {
			return runtime.Locator{}, err
		}
	}
	return r.launch(ctx, spec, takenOver)
}

// awaitGone waits, up to the stop grace, until loc's incarnation is no longer its pane's process,
// then stops watching it.
func (r *Runtime) awaitGone(ctx context.Context, loc runtime.Locator) error {
	pane, inc, err := r.paneOf(loc)
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
// the claim's gh bundle, then the pane. takenOver is a resume over a previous incarnation of the
// runtime this one takes over (Resume), whose session found nowhere is runtime.ErrSessionFileMissing
// rather than a refusal.
func (r *Runtime) launch(ctx context.Context, spec runtime.SpawnSpec, takenOver bool) (runtime.Locator, error) {
	if err := validateSpawnSpec(spec, r.providerKeys, r.operatorEnv); err != nil {
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
	backend := runtime.Of(spec.Backend)
	launcher, ok := r.launchers[backend]
	if !ok {
		return runtime.Locator{}, fmt.Errorf("spawn %s: its role runs on %s, and the daemon resolved no %s launch (worker_backends)",
			spec.Claim, backend, runtime.Backends[backend].Name)
	}
	workDir, err := r.workspaceDir(spec)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	if spec.ResumeSessionFile != "" {
		harness := runtime.Backends[backend].Name
		found, searched, err := sessionFileOf(spec.ResumeSessionFile, func() (string, error) {
			env := maps.Clone(r.paneEnv)
			maps.Copy(env, r.operatorEnv)
			dir, err := launcher.SessionsDir(spec, Host{StateDir: r.stateDir, Legion: r.legion}, env, workDir)
			if err != nil {
				return "", fmt.Errorf("the pane's %s sessions directory: %w", harness, err)
			}
			return dir, nil
		})
		switch {
		case err != nil:
			return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
		case found == "" && takenOver:
			return runtime.Locator{}, fmt.Errorf("resume %s: %w: no %s session at %s, and no other folder of %s holds that session",
				spec.Claim, runtime.ErrSessionFileMissing, harness, spec.ResumeSessionFile, searchedIn(searched))
		case found == "":
			verb := "respawning"
			if claim.IsTreeArchitect(spec.Role, spec.Issue, spec.Tree) {
				verb = "resurrecting"
			}
			return runtime.Locator{}, fmt.Errorf("Refusing to start %s fresh while %s: recorded %s session file is missing: %s, and no other folder of %s holds that session",
				spec.Issue, verb, harness, spec.ResumeSessionFile, searchedIn(searched))
		case found != spec.ResumeSessionFile:
			r.log.Warn("tmux runtime: the recorded session is in another folder; resuming it there",
				"claim", spec.Claim, "backend", backend, "recorded", spec.ResumeSessionFile, "session", found)
			spec.ResumeSessionFile = found
		}
		r.log.Info("tmux runtime: resuming a recorded "+harness+" session", "claim", spec.Claim, "backend", backend, "session", spec.ResumeSessionFile)
	}
	if err := makePaneHome(r.stateDir); err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	if err := writeTurnScopeOverlay(r.stateDir); err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	files := secretFiles(r.stateDir, spec)
	if err := runtime.WriteSecretFiles(r.stateDir, files); err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	ghRoot, ghDefault, ghWriter := "", "", strings.ToLower(spec.Repository.Owner())
	if r.gitHubCredential != nil {
		ghRoot = runtime.GHConfigDir(r.stateDir, spec.Claim)
		bundle, err := r.gitHubCredential(ctx, spec.Role, ghWriter)
		switch {
		case spec.Role == claim.RoleController && errors.Is(err, runtime.ErrNoGitHubOwner):
			// A project with no GitHub owner to mint for still runs its controller, with gh acting
			// as nobody, as `legion controller start` and the sandbox runtime do there: its gh
			// directory empty, so no token an earlier launch wrote outlives this one.
			if err := emptyGitHubBundle(ghRoot); err != nil {
				return runtime.Locator{}, fmt.Errorf("spawn %s: empty the github credential: %w", spec.Claim, err)
			}
			r.log.Info("tmux runtime: no GitHub owner to act for; the controller's gh acts as nobody", "claim", spec.Claim)
		case err != nil:
			return runtime.Locator{}, fmt.Errorf("spawn %s: write the github credential: %w", spec.Claim, err)
		default:
			if _, err := ghconfig.WriteBundle(ghRoot, bundle); err != nil {
				return runtime.Locator{}, fmt.Errorf("spawn %s: write the github credential: %w", spec.Claim, err)
			}
			ghDefault = bundle.Default
			written := bundle.Owners[ghDefault]
			r.log.Info("tmux runtime: github credential written", "claim", spec.Claim, "role", spec.Role, "app", written.App,
				"expiresAt", written.ExpiresAt, "owners", bundle.OwnerNames())
		}
	}
	path := r.paneEnv["PATH"]
	if spec.Env["PATH"] != "" {
		path = spec.Env["PATH"]
	}
	path = workerbin.Path(path, r.stateDir)
	inner, backendPairs, err := launcher.Launch(spec, Host{StateDir: r.stateDir, Legion: r.legion})
	if err != nil {
		return runtime.Locator{}, err
	}
	command := shimShellCommand(r.socket, path, workDir, r.legion, r.streamAddress, files[0].Path, r.providerEnvDir, inner)
	pairs := panePairs(spec, PaneInputs{
		StateDir: r.stateDir, Workspace: workDir, DaemonURL: r.daemonURL, EnvoyURL: r.envoyURL, NATSURLs: r.natsURLs,
		DispatchURL: r.dispatchURL, DispatchTokenFile: r.dispatchToken, GHOwnersDir: ghRoot, GHDefault: ghDefault, GH: r.gh,
		OperatorEnv: r.operatorEnv, PiConfigFiles: r.piConfigFiles,
	}, files)
	return r.openPane(ctx, spec, ghWriter, paneCommand(append(pairs, backendPairs...), command))
}

// openPane puts the pane on the server under the launch lock: into the window of a pane of the
// same issue — or, for the controller, of the controller's own window — that still verifies as its
// recorded process, else into a new window named for it (windowKey). The pane is recorded — its
// incarnation read, the process watched with ghWriter, the owner its launch minted the gh bundle's
// write token for ("" for none) — before the lock is released, so the next spawn on the issue can
// share its window and no orphan sweep mistakes it for one.
func (r *Runtime) openPane(ctx context.Context, spec runtime.SpawnSpec, ghWriter string, pane []string) (runtime.Locator, error) {
	r.launchMu.Lock()
	defer r.launchMu.Unlock()

	if err := r.refuseLiveIncarnation(ctx, spec.Claim); err != nil {
		return runtime.Locator{}, err
	}
	key := windowKey(spec)
	window, err := r.issueWindow(ctx, key)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	var report paneReport
	if window != "" {
		report, err = r.splitWindow(ctx, window, pane)
	} else {
		report, err = r.openWindow(ctx, treeName(key), pane)
	}
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("spawn %s: %w", spec.Claim, err)
	}
	if err := r.awaitPaneCommand(ctx, report); err != nil {
		return runtime.Locator{}, errors.Join(fmt.Errorf("spawn %s: %w", spec.Claim, err), r.killUnrecorded(ctx, report))
	}
	ticks, alive, err := r.startTicks(report.pid)
	if err != nil {
		return runtime.Locator{}, errors.Join(fmt.Errorf("spawn %s: %w", spec.Claim, err), r.killUnrecorded(ctx, report))
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
	r.track(loc, key, ghWriter)
	return loc, nil
}

// awaitPaneCommand waits, bounded by the command timeout, until the pane's process runs the pane's
// command. tmux reports a new pane's pid at fork, and until that child execs its command it is a
// copy of the server, which a probe reads as not running its harness; supervision takes that for a death
// and relaunches the claim over its still-starting process (LEGION-274). A process that exits
// meanwhile is left to the caller's identity read, which reports it; one still the server's copy
// when the bound runs out is an error, and the caller kills its pane (killUnrecorded), so no
// process the daemon never recorded runs on.
func (r *Runtime) awaitPaneCommand(ctx context.Context, report paneReport) error {
	deadline := time.Now().Add(r.commandTimeout)
	for {
		fork, err := r.serverFork(report.pid)
		if err != nil || !fork {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pane %s pid %d did not start its command within %s", report.pane, report.pid, r.commandTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// killUnrecorded kills report's pane after an error left its process unrecorded: no locator is
// returned for it, so nothing else would end it (runtime.Runtime's Spawn and Resume). It runs even
// once ctx has ended, since the caller's deadline may be what ended the launch. A pane already gone
// is gone; any other failure is returned, so the launch error says the process may still run.
func (r *Runtime) killUnrecorded(ctx context.Context, report paneReport) error {
	res, err := r.run(context.WithoutCancel(ctx), killPaneArgv(r.socket, report.pane))
	if err == nil && (res.exitCode == 0 || paneGoneStderr.MatchString(res.stderr)) {
		return nil
	}
	if err == nil {
		err = fmt.Errorf("tmux exited %d: %s", res.exitCode, strings.TrimSpace(res.stderr))
	}
	return fmt.Errorf("kill pane %s, whose process pid %d was never recorded: %w", report.pane, report.pid, err)
}

// refuseLiveIncarnation refuses a launch for a claim whose watched process still runs: one claim,
// one process. A watched process that no longer verifies is dropped; one that cannot be verified
// blocks the launch, since it may be running.
func (r *Runtime) refuseLiveIncarnation(ctx context.Context, token claim.Token) error {
	for _, entry := range r.trackedProcesses() {
		if entry.locator.Claim != token {
			continue
		}
		pane, inc, err := r.paneOf(entry.locator)
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
// on the issue that still verifies as its recorded process (TmuxRuntime.probedWindowId,
// runtime-tmux.ts). A live window alone proves nothing — after a server is recreated the same @N
// names another issue's window — and a pane that cannot be verified is simply not shared; a fresh
// window costs nothing but a window. "" is a new window.
func (r *Runtime) issueWindow(ctx context.Context, issue string) (string, error) {
	for _, entry := range r.trackedProcesses() {
		if entry.issue != issue {
			continue
		}
		pane, inc, err := r.paneOf(entry.locator)
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
// its caller then owns killing the bootstrap window once its own window is open (ensureSession,
// tmux.ts). The bootstrap pane is deliberately unmarked: reconciliation reaps marked daemon
// windows, not the private server's own placeholder or a human pane. The launch lock serializes
// callers.
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
// any other failure is the launch's (TmuxRuntime.openWindow, runtime-tmux.ts).
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
// repeats the marker and treats a failure as a launch failure (markOwner and openWindow, tmux.ts).
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

// splitWindow splits pane into window and tiles the layout (splitWindow, tmux.ts).
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
