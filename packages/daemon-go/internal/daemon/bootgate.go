package daemon

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
)

// pluginLoadProbe is the Oh My Pi extension the load probe hands `omp models`: the shipped probe
// (packages/daemon/src/daemon/boot-probes.ts:233-238), which prints whether the plugin's
// `legion.ts` set its load marker (`Symbol.for("legion.pi-envoy.legion-loaded")`,
// packages/pi-envoy/extensions/legion.ts) — which only a plugin Oh My Pi actually loaded has done
// — and, beside it, the marker's value: the `import.meta.url` of that `legion.ts`, where the plugin
// loaded from.
//
//go:embed probe.mjs
var pluginLoadProbe []byte

const (
	loadedMarker     = "LEGION_PLUGIN_LOADED=yes"
	notLoadedMarker  = "LEGION_PLUGIN_LOADED=no"
	loadedFromMarker = "LEGION_PLUGIN_LOADED_FROM="
	pluginPackage    = "@sjawhar/pi-legion-envoy"
	// maxProbeStderr bounds how much of a failed probe's stderr a refusal quotes, keeping the tail,
	// where the error usually is (boot-probes.ts:226-232).
	maxProbeStderr = 2048
)

// probeRetry is the wait between load-probe attempts whose failure says nothing about the plugin:
// after the i-th such failure the gate waits min(initial·2^i, max), with no bound on attempts. A
// transient failure is Oh My Pi dying under host load after it loaded the plugin, or an attempt
// the budget cut off before it answered; the daemon waits the load out inside the process rather
// than exiting into a supervisor that relaunches it into the same load (DAEMON_PROBE_RETRY,
// boot-probes.ts:28-51).
type probeRetry struct{ initial, max time.Duration }

var daemonProbeRetry = probeRetry{initial: 10 * time.Second, max: 5 * time.Minute}

// pluginGate is the daemon's boot gate on the Oh My Pi plugin every pane loads. It runs before the
// daemon opens its store, so no pane launches until it passes, and it runs under the environment a
// pane will have (tmux.PaneEnvironment: the allow-listed variables, the XDG directories under
// `<state_dir>/home`), because the daemon's own HOME, profile, and XDG directories are not what a
// pane's Oh My Pi reads. Two probes, in the shipped gate's order (packages/daemon/src/daemon/
// index.ts:365-383): the contract probe reads the installed manifest and refuses a plugin that
// does not declare this daemon's `GoDaemonAPIVersion`; the load probe runs Oh My Pi the way a pane
// does and refuses a plugin it did not load — installed but disabled, or not registered — or one
// it loaded from another root than the manifest the contract probe read.
type pluginGate struct {
	// env is the pane environment the probes resolve and run under.
	env map[string]string
	// workDir is the load probe's working directory: the state directory, under which every
	// pane's workspace lives, rather than wherever the operator started the daemon.
	workDir string
	// invocation is the resolved OMP invocation (tmux.ResolveOmpInvocation), a shell fragment.
	invocation string
	// prefix is `omp_launch_prefix`.
	prefix []string
	// timeout is each load-probe attempt's budget, `slow_command_timeout_seconds`.
	timeout time.Duration
	retry   probeRetry
	log     *slog.Logger
}

// verify runs the two probes. A refusal names what the operator has to change; a gate the daemon's
// stop interrupted returns an error wrapping ctx's.
func (g pluginGate) verify(ctx context.Context) error {
	manifest, profile, err := pluginManifestPath(g.env)
	if err != nil {
		return err
	}
	version, err := verifyPluginContract(manifest, profile)
	if err != nil {
		return err
	}
	if err := g.verifyLoaded(ctx, manifest, version, profile); err != nil {
		return err
	}
	g.log.Info("boot gate: pi-legion-envoy speaks this daemon's contract and loads in a pane",
		"manifest", manifest, "version", version, "goDaemonApiVersion", api.GoDaemonAPIVersion)
	return nil
}

var (
	// profileName and windowsReservedProfile are the profile names Oh My Pi accepts and the device
	// names it refuses among them (@oh-my-pi/pi-utils src/dirs.ts:38, :49).
	profileName            = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	windowsReservedProfile = regexp.MustCompile(`(?i)^(?:CON|PRN|AUX|NUL|COM[0-9]|LPT[0-9])(?:\..*)?$`)
)

// pluginManifestPath is the installed pi-legion-envoy manifest where Oh My Pi, started under env,
// looks for its plugins, and the profile that decided it ("" for the default profile). The
// resolution is Oh My Pi's own (@oh-my-pi/pi-utils 18.1.15, src/dirs.ts), ported for the
// variables a pane can carry:
//
//   - the profile is OMP_PROFILE when it is set at all, even empty, else PI_PROFILE; trimmed, an
//     empty name or "default" is the default profile, and a name Oh My Pi would refuse is refused
//     here in its words (resolveProfileEnv, normalizeProfileName, :59-88);
//   - the config root is `.omp` under the home directory — HOME, else the account's, as
//     `os.homedir()` answers — with `profiles/<name>` under it for a named profile (:110-117);
//   - the data root is `$XDG_DATA_HOME/omp` for the default profile, or
//     `$XDG_DATA_HOME/omp/profiles/<name>` for a named one, when that directory already exists,
//     else the config root (DirResolver's constructor, :315-367);
//   - the plugins are `plugins/node_modules` under the data root (getPluginsDir,
//     getPluginsNodeModules, :606-616), and the manifest is the package's own `package.json`
//     there (legionPluginManifestPath, packages/daemon/src/daemon/boot-probes.ts:240-244).
//
// PI_CONFIG_DIR and PI_CODING_AGENT_DIR move the roots in Oh My Pi too (:281-283, :315-322);
// neither is on the pane's allow-list, so no pane carries one.
func pluginManifestPath(env map[string]string) (string, string, error) {
	requested, set := env["OMP_PROFILE"]
	if !set {
		requested = env["PI_PROFILE"]
	}
	profile := strings.TrimSpace(requested)
	if profile == "default" {
		profile = ""
	}
	if profile != "" && (profile == "." || profile == ".." || strings.HasSuffix(profile, ".") ||
		!profileName.MatchString(profile) || windowsReservedProfile.MatchString(profile)) {
		return "", "", fmt.Errorf(`Invalid OMP profile %q in the pane environment. Profile names must match %s, cannot be "." or "..", cannot end with ".", and cannot be a Windows reserved device name (CON, PRN, AUX, NUL, COM0-9, LPT0-9, or any of those with an extension).`,
			requested, profileName)
	}
	home := env["HOME"]
	if home == "" {
		account, err := user.Current()
		if err != nil {
			return "", "", fmt.Errorf("resolve the home directory Oh My Pi reads its plugins under: HOME is not set, and %w", err)
		}
		home = account.HomeDir
	}
	root := filepath.Join(home, ".omp")
	if profile != "" {
		root = filepath.Join(root, "profiles", profile)
	}
	if xdg := env["XDG_DATA_HOME"]; xdg != "" && (goruntime.GOOS == "linux" || goruntime.GOOS == "darwin") {
		candidate := filepath.Join(xdg, "omp")
		if profile != "" {
			candidate = filepath.Join(candidate, "profiles", profile)
		}
		if _, err := os.Stat(candidate); err == nil {
			root = candidate
		}
	}
	return filepath.Join(root, "plugins", "node_modules", "@sjawhar", "pi-legion-envoy", "package.json"), profile, nil
}

// profileWords names a profile the way a refusal tells the operator where to install.
func profileWords(profile string) string {
	if profile == "" {
		return "the default OMP profile"
	}
	return "OMP profile " + profile
}

// verifyPluginContract is the contract probe (verifyLegionPluginContract, boot-probes.ts:267-298,
// on the Go daemon's own field): the manifest's `legion.goDaemonApiVersion` must be this daemon's
// GoDaemonAPIVersion. A manifest that is missing, unreadable, or without the field is the same
// refusal, never a fallback — the load probe would call such a plugin merely "not loaded" and send
// the operator to `omp plugin list` when the fix is a reinstall. It answers the package version.
func verifyPluginContract(manifest, profile string) (string, error) {
	install := fmt.Sprintf("Install the @sjawhar/pi-legion-envoy release built from this daemon's commit into %s.", profileWords(profile))
	raw, err := os.ReadFile(manifest)
	var parsed any
	if err == nil {
		err = json.Unmarshal(raw, &parsed)
	}
	if err != nil {
		return "", fmt.Errorf("pi-legion-envoy manifest at %s could not be read (%v); this daemon requires a plugin speaking Go daemon API contract %d. %s",
			manifest, err, api.GoDaemonAPIVersion, install)
	}
	record, _ := parsed.(map[string]any)
	version, ok := record["version"].(string)
	if !ok {
		version = "unknown"
	}
	legion, _ := record["legion"].(map[string]any)
	declared, present := legion["goDaemonApiVersion"]
	if number, ok := declared.(float64); ok && number == api.GoDaemonAPIVersion {
		return version, nil
	}
	spoken := "none"
	if present {
		encoded, _ := json.Marshal(declared)
		spoken = string(encoded)
	}
	return "", fmt.Errorf("pi-legion-envoy at %s (package %s) speaks Go daemon API contract %s; this daemon requires %d. %s",
		manifest, version, spoken, api.GoDaemonAPIVersion, install)
}

// verifyLoaded is the load probe (verifyLegionPluginLoaded, boot-probes.ts:313-392): Oh My Pi,
// launched as a pane launches it — through the launch prefix, under the pane environment, with the
// pane's XDG directories created first as a spawn creates them — lists its models with the probe
// extension added, and passes only when the probe saw the plugin's load marker and the plugin
// loaded from the manifest's own package (verifyLoadedFrom). The retry and the classification are
// the shipped ones (retryBootProbe, killedOutcome, :99-161, :348-360).
func (g pluginGate) verifyLoaded(ctx context.Context, manifest, version, profile string) error {
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		if dir := g.env[name]; dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("boot gate: create the pane's %s: %w", name, err)
			}
		}
	}
	dir, err := os.MkdirTemp("", "legion-plugin-probe-")
	if err != nil {
		return fmt.Errorf("boot gate: create the load probe's directory: %w", err)
	}
	defer os.RemoveAll(dir)
	probe := filepath.Join(dir, "probe.mjs")
	if err := os.WriteFile(probe, pluginLoadProbe, 0o600); err != nil {
		return fmt.Errorf("boot gate: write the load probe: %w", err)
	}

	launch := tmux.WithOmpLaunchPrefix(g.prefix, g.invocation)
	environ := make([]string, 0, len(g.env))
	for _, name := range slices.Sorted(maps.Keys(g.env)) {
		environ = append(environ, name+"="+g.env[name])
	}
	stopped := func(err error) error {
		return fmt.Errorf("boot gate: the pi-legion-envoy load probe was abandoned, the daemon stopped while it ran or waited to run it again: %w", err)
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return stopped(err)
		}
		outcome := g.probeOnce(ctx, launch, probe, environ, version, profile)
		if outcome.passed {
			return verifyLoadedFrom(outcome.loadedFrom, manifest, profile)
		}
		if err := ctx.Err(); err != nil {
			return stopped(err)
		}
		if outcome.refusal != nil {
			return outcome.refusal
		}
		delay := g.retry.initial
		for i := 0; i < attempt && delay < g.retry.max; i++ {
			delay *= 2
		}
		delay = min(delay, g.retry.max)
		g.log.Warn("boot gate: the pi-legion-envoy load probe failed transiently; waiting to run it again",
			"attempt", attempt+1, "retryIn", delay.String(), "detail", outcome.detail)
		select {
		case <-ctx.Done():
			return stopped(ctx.Err())
		case <-time.After(delay):
		}
	}
}

// probeOutcome is one load-probe attempt: passed, with where the plugin loaded from; refused (an
// answer no retry changes); or neither — transient, with the detail its retry is logged with.
type probeOutcome struct {
	passed     bool
	loadedFrom string
	refusal    error
	detail     string
}

// verifyLoadedFrom holds the plugin a pane loads to the manifest the contract probe read: the
// load probe reports the URL its `legion.ts` loaded from, and the pi-legion-envoy manifest above
// that file must be the same file, links resolved, as the manifest at the plugin root the pane
// environment names. They part when something in the launch picks its own plugin root — a launch
// prefix that sets OMP_PROFILE (`env OMP_PROFILE=… --`), a dotenv file Oh My Pi reads —
// and the contract probe would otherwise have vouched for a plugin no pane runs.
func verifyLoadedFrom(location, manifest, profile string) error {
	loaded, err := url.Parse(location)
	if err != nil || loaded.Scheme != "file" || loaded.Path == "" {
		return fmt.Errorf("the pi-legion-envoy load probe reported the plugin loaded from %q, which is not a file the gate can hold to a manifest", location)
	}
	owner, err := owningManifest(loaded.Path)
	if err != nil {
		return fmt.Errorf("pi-legion-envoy loads in a pane from %s, and the gate cannot hold it to a manifest: %w", loaded.Path, err)
	}
	read, err := filepath.EvalSymlinks(manifest)
	if err != nil {
		return fmt.Errorf("boot gate: resolve the manifest %s the contract probe read: %w", manifest, err)
	}
	if owner != read {
		return fmt.Errorf("pi-legion-envoy loads in a pane from %s, but the manifest this gate held to Go daemon API contract %d is %s, at the plugin root of %s in the pane environment: the launch prefix, or a dotenv file Oh My Pi reads, selects another plugin root. Select the OMP profile in the daemon's own environment, which every pane inherits",
			owner, api.GoDaemonAPIVersion, read, profileWords(profile))
	}
	return nil
}

// owningManifest is the pi-legion-envoy `package.json` nearest above file, links resolved.
func owningManifest(file string) (string, error) {
	for dir := filepath.Dir(file); ; {
		candidate := filepath.Join(dir, "package.json")
		if raw, err := os.ReadFile(candidate); err == nil {
			var pkg struct {
				Name string `json:"name"`
			}
			if json.Unmarshal(raw, &pkg) == nil && pkg.Name == pluginPackage {
				return filepath.EvalSymlinks(candidate)
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no %s package.json above %s", pluginPackage, file)
		}
		dir = parent
	}
}

// probeOnce runs one attempt within the gate's budget. `exec` replaces the shell with the launch
// prefix and Oh My Pi, and the attempt runs in its own process group, so a timeout or the daemon's
// stop kills what is actually running — a prompting `secrets`, Oh My Pi and its children — rather
// than a shell that leaves them holding the pipes (boot-probes.ts:163-167).
func (g pluginGate) probeOnce(ctx context.Context, launch, probe string, environ []string, version, profile string) probeOutcome {
	attempt, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	cmd := exec.CommandContext(attempt, "sh", "-c", `exec `+launch+` models --extension "$1" --json >/dev/null`, "sh", probe)
	cmd.Env = environ
	cmd.Dir = g.workDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	started := time.Now()
	err := cmd.Run()
	if ctx.Err() != nil {
		return probeOutcome{}
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) && !errors.Is(err, exec.ErrWaitDelay) {
		return probeOutcome{refusal: fmt.Errorf("boot gate: run the pi-legion-envoy load probe: %w", err)}
	}
	output := stderr.String() + "\n" + stdout.String()
	tail := strings.TrimSpace(stderr.String())
	if len(tail) > maxProbeStderr {
		tail = tail[len(tail)-maxProbeStderr:]
	}
	// A budget kill is transient when the probe never answered; a probe that said "not loaded" and
	// only then hung has answered, and is judged below like any other answer.
	if errors.Is(attempt.Err(), context.DeadlineExceeded) && !strings.Contains(output, notLoadedMarker) {
		detail := fmt.Sprintf("command timed out after %s (ran %.1f s)", g.timeout, time.Since(started).Seconds())
		if tail != "" {
			detail += "\n" + tail
		}
		return probeOutcome{detail: detail}
	}
	exit := cmd.ProcessState.ExitCode()
	loaded := strings.Contains(output, loadedMarker)
	if exit == 0 && loaded {
		location := ""
		for _, line := range strings.Split(output, "\n") {
			if rest, ok := strings.CutPrefix(line, loadedFromMarker); ok {
				location = strings.TrimSpace(rest)
			}
		}
		return probeOutcome{passed: true, loadedFrom: location}
	}
	// Oh My Pi loaded the plugin and then died: under load, not for want of the plugin.
	if exit != 0 && loaded {
		return probeOutcome{detail: tail}
	}
	// The launch failed before Oh My Pi answered — the launch prefix refusing a key, an invocation
	// that is not Oh My Pi: its own refusal, since "not loaded" would send the operator to
	// `omp plugin list` when the fix is the prefix or the invocation.
	if exit != 0 && !strings.Contains(output, notLoadedMarker) {
		message := fmt.Sprintf("OMP launch probe failed (exit %d) for launch command %q", exit, launch)
		if tail != "" {
			message += ": " + tail
		}
		return probeOutcome{refusal: errors.New(message)}
	}
	list := "omp plugin list"
	if profile != "" {
		list = "OMP_PROFILE=" + profile + " " + list
	}
	return probeOutcome{refusal: fmt.Errorf("pi-legion-envoy %s is installed but not loaded by omp (disabled or unregistered): run %s", version, list)}
}
