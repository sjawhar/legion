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

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
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

// agentsProbe is the Oh My Pi extension the pi.agents probe hands `omp models` alone: the shipped
// probe (OMP_AGENTS_CAPABILITY_PROBE, boot-probes.ts:22-27), which prints whether the Oh My Pi
// that loaded it exposes `pi.agents`, the API every Legion phase worker's subagents run through.
//
//go:embed agents-probe.mjs
var agentsProbe []byte

const (
	loadedMarker     = "LEGION_PLUGIN_LOADED=yes"
	notLoadedMarker  = "LEGION_PLUGIN_LOADED=no"
	loadedFromMarker = "LEGION_PLUGIN_LOADED_FROM="
	agentsMarker     = "LEGION_OMP_AGENTS=available"
	noAgentsMarker   = "LEGION_OMP_AGENTS=missing"
	pluginPackage    = "@sjawhar/pi-legion-envoy"
	// maxProbeStderr bounds how much of a failed probe's stderr a refusal quotes, keeping the tail,
	// where the error usually is (boot-probes.ts:226-232).
	maxProbeStderr = 2048
	// sessionStorageVariable is the variable a `sql` session store sets on every Oh My Pi, and
	// sessionStorageProbeValue the value no build of any age accepts for it: a build that carries
	// the setting refuses it, naming the variable; one that predates the setting never reads it
	// (SESSION_STORAGE_VARIABLE, SESSION_STORAGE_PROBE_VALUE, boot-probes.ts:393-397).
	sessionStorageVariable   = "OMP_SESSION_STORAGE"
	sessionStorageProbeValue = "legion-launch-probe"
)

// pluginGate runs the Oh My Pi launch probes. As the daemon's boot gate on the plugin every pane
// loads, it runs before the daemon opens its store, so no pane launches until it passes, and it
// runs under the environment a pane will have (tmux.PaneEnvironment: the allow-listed variables,
// the XDG directories under `<state_dir>/home`), because the daemon's own HOME, profile, and XDG
// directories are not what a pane's Oh My Pi reads. Two probes, in the shipped gate's order
// (packages/daemon/src/daemon/index.ts:365-383): the contract probe reads the installed manifest
// and refuses a plugin that does not declare the gate's contract; the load probe runs Oh My Pi the
// way a pane does and refuses a plugin it did not load — installed but disabled, or not
// registered — or one it loaded from another root than the manifest the contract probe read.
//
// Inside the worker image the same gate is `legion probe-image` (ProbeImage), which adds the two
// probes only the image runs: pi.agents and the session-storage setting.
type pluginGate struct {
	// env is the environment the probes resolve and run under: a pane's, or the image's own.
	env map[string]string
	// workDir is the probes' working directory: the state directory, under which every pane's
	// workspace lives, rather than wherever the operator started the daemon.
	workDir string
	// invocation is the OMP invocation, a shell fragment (tmux.ResolveOmpInvocation's).
	invocation string
	// prefix is `omp_launch_prefix`.
	prefix []string
	// timeout is each probe attempt's budget, `slow_command_timeout_seconds`.
	timeout time.Duration
	retry   bootprobe.Retry
	// contract is the Go daemon API contract the plugin must declare: the daemon's own
	// GoDaemonAPIVersion, or the one `legion probe-image` is asked for.
	contract int
	log      *slog.Logger
}

// verify runs the two probes. A refusal names what the operator has to change; a gate the daemon's
// stop interrupted returns an error wrapping ctx's.
func (g pluginGate) verify(ctx context.Context) error {
	manifest, profile, err := pluginManifestPath(g.env)
	if err != nil {
		return err
	}
	version, err := verifyPluginContract(manifest, profile, g.contract)
	if err != nil {
		return err
	}
	if err := g.verifyLoaded(ctx, manifest, version, profile); err != nil {
		return err
	}
	g.log.Info("boot gate: pi-legion-envoy speaks this daemon's contract and loads in a pane",
		"manifest", manifest, "version", version, "goDaemonApiVersion", g.contract)
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
// on the Go daemon's own field): the manifest's `legion.goDaemonApiVersion` must be contract. A
// manifest that is missing, unreadable, or without the field is the same refusal, never a
// fallback — the load probe would call such a plugin merely "not loaded" and send the operator to
// `omp plugin list` when the fix is a reinstall. It answers the package version.
func verifyPluginContract(manifest, profile string, contract int) (string, error) {
	install := fmt.Sprintf("Install the @sjawhar/pi-legion-envoy release built from this daemon's commit into %s.", profileWords(profile))
	raw, err := os.ReadFile(manifest)
	var parsed any
	if err == nil {
		err = json.Unmarshal(raw, &parsed)
	}
	if err != nil {
		return "", fmt.Errorf("pi-legion-envoy manifest at %s could not be read (%v); this daemon requires a plugin speaking Go daemon API contract %d. %s",
			manifest, err, contract, install)
	}
	record, _ := parsed.(map[string]any)
	version, ok := record["version"].(string)
	if !ok {
		version = "unknown"
	}
	legion, _ := record["legion"].(map[string]any)
	declared, present := legion["goDaemonApiVersion"]
	if number, ok := declared.(float64); ok && number == float64(contract) {
		return version, nil
	}
	spoken := "none"
	if present {
		encoded, _ := json.Marshal(declared)
		spoken = string(encoded)
	}
	return "", fmt.Errorf("pi-legion-envoy at %s (package %s) speaks Go daemon API contract %s; this daemon requires %d. %s",
		manifest, version, spoken, contract, install)
}

// verifyLoaded is the load probe (verifyLegionPluginLoaded, boot-probes.ts:313-392): Oh My Pi,
// launched as a pane launches it — through the launch prefix, under the pane environment, with the
// pane's XDG directories created first as a spawn creates them — lists its models with the probe
// extension added, and passes only when the probe saw the plugin's load marker and the plugin
// loaded from the manifest's own package (verifyLoadedFrom). The classification is the shipped
// one (killedOutcome, :94-122, :348-360).
func (g pluginGate) verifyLoaded(ctx context.Context, manifest, version, profile string) error {
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		if dir := g.env[name]; dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("boot gate: create the pane's %s: %w", name, err)
			}
		}
	}
	dir, probe, err := writeProbe("legion-plugin-probe-", pluginLoadProbe)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	launch := tmux.WithOmpLaunchPrefix(g.prefix, g.invocation)
	loadedFrom := ""
	err = bootprobe.Run(ctx, "pi-legion-envoy load", g.retry, g.log, func(ctx context.Context) bootprobe.Outcome {
		outcome, from := g.probeLoad(ctx, launch, probe, version, profile)
		loadedFrom = from
		return outcome
	})
	if err != nil {
		return err
	}
	return verifyLoadedFrom(loadedFrom, manifest, profile, g.contract)
}

// verifyLoadedFrom holds the plugin a pane loads to the manifest the contract probe read: the
// load probe reports the URL its `legion.ts` loaded from, and the pi-legion-envoy manifest above
// that file must be the same file, links resolved, as the manifest at the plugin root the pane
// environment names. They part when something in the launch picks its own plugin root — a launch
// prefix that sets OMP_PROFILE (`env OMP_PROFILE=… --`), a dotenv file Oh My Pi reads —
// and the contract probe would otherwise have vouched for a plugin no pane runs.
func verifyLoadedFrom(location, manifest, profile string, contract int) error {
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
			owner, contract, read, profileWords(profile))
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

// probeLoad is one load-probe attempt, and, on a pass, where the plugin loaded from.
func (g pluginGate) probeLoad(ctx context.Context, launch, probe, version, profile string) (bootprobe.Outcome, string) {
	r, err := g.run(ctx, `exec `+launch+` models --extension "$1" --json >/dev/null`, probe)
	if err != nil {
		return bootprobe.Outcome{Refusal: fmt.Errorf("boot gate: run the pi-legion-envoy load probe: %w", err)}, ""
	}
	// A budget kill is transient when the probe never answered; a probe that said "not loaded" and
	// only then hung has answered, and is judged below like any other answer.
	if r.timedOut && !strings.Contains(r.output, notLoadedMarker) {
		return bootprobe.Outcome{Detail: r.killed(launch, g.timeout)}, ""
	}
	loaded := strings.Contains(r.output, loadedMarker)
	if r.exit == 0 && loaded {
		location := ""
		for _, line := range strings.Split(r.output, "\n") {
			if rest, ok := strings.CutPrefix(line, loadedFromMarker); ok {
				location = strings.TrimSpace(rest)
			}
		}
		return bootprobe.Outcome{Passed: true}, location
	}
	// Oh My Pi loaded the plugin and then died: under load, not for want of the plugin.
	if r.exit != 0 && loaded {
		return bootprobe.Outcome{Detail: r.tail}, ""
	}
	// The launch failed before Oh My Pi answered — the launch prefix refusing a key, an invocation
	// that is not Oh My Pi: its own refusal, since "not loaded" would send the operator to
	// `omp plugin list` when the fix is the prefix or the invocation.
	if r.exit != 0 && !strings.Contains(r.output, notLoadedMarker) {
		message := fmt.Sprintf("OMP launch probe failed (exit %d) for launch command %q", r.exit, launch)
		if r.tail != "" {
			message += ": " + r.tail
		}
		return bootprobe.Outcome{Refusal: errors.New(message)}, ""
	}
	list := "omp plugin list"
	if profile != "" {
		list = "OMP_PROFILE=" + profile + " " + list
	}
	return bootprobe.Outcome{Refusal: fmt.Errorf("pi-legion-envoy %s is installed but not loaded by omp (disabled or unregistered): run %s", version, list)}, ""
}

// verifyAgentsCapability is the pi.agents probe (verifyOmpAgentsCapability, boot-probes.ts:
// 168-220): Oh My Pi, with no extension but the probe's, lists its models, and passes only when
// the probe saw `pi.agents` and Oh My Pi exited cleanly. Oh My Pi dying after the probe answered
// yes, or an attempt the budget cut off before it answered, is transient; everything else — the
// `missing` answer, a clean exit without an answer, a launch that failed before Oh My Pi — is an
// answer no retry changes.
func (g pluginGate) verifyAgentsCapability(ctx context.Context) error {
	dir, probe, err := writeProbe("legion-omp-probe-", agentsProbe)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	launch := tmux.WithOmpLaunchPrefix(g.prefix, g.invocation)
	return bootprobe.Run(ctx, "OMP pi.agents", g.retry, g.log, func(ctx context.Context) bootprobe.Outcome {
		r, err := g.run(ctx, `exec `+launch+` models --no-extensions --extension "$1" --json >/dev/null`, probe)
		if err != nil {
			return bootprobe.Outcome{Refusal: fmt.Errorf("boot gate: run the OMP pi.agents probe: %w", err)}
		}
		answeredNo := strings.Contains(r.output, noAgentsMarker)
		if r.timedOut && !answeredNo {
			return bootprobe.Outcome{Detail: r.killed(launch, g.timeout)}
		}
		available := strings.Contains(r.output, agentsMarker)
		if r.exit == 0 && available {
			return bootprobe.Outcome{Passed: true}
		}
		if r.exit != 0 && available && !answeredNo {
			return bootprobe.Outcome{Detail: fmt.Sprintf("launch command %q exited %d after Oh My Pi answered: %s", launch, r.exit, r.tail)}
		}
		message := fmt.Sprintf("the OMP launch command %q does not expose pi.agents", launch)
		if r.tail != "" {
			message += ": " + r.tail
		}
		return bootprobe.Outcome{Refusal: errors.New(message)}
	})
}

// verifySessionStorage is the session-storage probe (verifySessionStorageSetting, boot-probes.ts:
// 405-468), which only the image runs: it proves the Oh My Pi build carries the `session.storage`
// setting, so a deployment that sets OMP_SESSION_STORAGE=sql gets its sessions in its database
// rather than a build that ignores the variable and silently keeps them on files. PI_TIMING=x
// makes an interactive start print its timings and exit just before the TUI would open, with
// stdin on /dev/null there is no piped prompt, and every `--no-*` flag keeps the profile's
// plugins, sessions, and tools out of a run that only needs the setting's resolver, which runs
// before that exit: a carrying build dies on the nonsense value first, naming the variable (a
// pass); a clean exit is a build that accepted it, so predates the setting (refused); any other
// failure is the launch dying before the resolver ran (transient).
func (g pluginGate) verifySessionStorage(ctx context.Context) error {
	launch := tmux.WithOmpLaunchPrefix(g.prefix, g.invocation)
	script := "export " + sessionStorageVariable + "=" + sessionStorageProbeValue + " PI_TIMING=x; exec " + launch +
		" --no-session --no-extensions --no-skills --no-rules --no-lsp --no-tools </dev/null >/dev/null"
	return bootprobe.Run(ctx, "OMP session storage setting", g.retry, g.log, func(ctx context.Context) bootprobe.Outcome {
		r, err := g.run(ctx, script)
		if err != nil {
			return bootprobe.Outcome{Refusal: fmt.Errorf("boot gate: run the OMP session storage probe: %w", err)}
		}
		switch {
		case r.timedOut:
			return bootprobe.Outcome{Detail: r.killed(launch, g.timeout)}
		case r.exit != 0 && strings.Contains(r.stderr, sessionStorageVariable):
			return bootprobe.Outcome{Passed: true}
		case r.exit == 0:
			return bootprobe.Outcome{Refusal: fmt.Errorf("OMP launch command %q started with %s=%s (exit 0): this build predates the session.storage setting and would silently keep sessions on files under a sql session store; pin a fork release that carries the setting",
				launch, sessionStorageVariable, sessionStorageProbeValue)}
		}
		detail := fmt.Sprintf("launch command %q exited %d without naming %s", launch, r.exit, sessionStorageVariable)
		if r.tail != "" {
			detail += ": " + r.tail
		}
		return bootprobe.Outcome{Detail: detail}
	})
}

// writeProbe writes an extension probe into a directory of its own, which the caller removes.
func writeProbe(pattern string, source []byte) (string, string, error) {
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", "", fmt.Errorf("boot gate: create a probe directory: %w", err)
	}
	probe := filepath.Join(dir, "probe.mjs")
	if err := os.WriteFile(probe, source, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", "", fmt.Errorf("boot gate: write the probe extension: %w", err)
	}
	return dir, probe, nil
}

// ran is one probe command's run: its exit code (-1 when killed), its stderr, stderr and stdout
// together, the tail of stderr a refusal quotes, and whether the budget killed it.
type ran struct {
	exit           int
	stderr, output string
	tail           string
	timedOut       bool
	elapsed        time.Duration
}

// killed is a budget kill's detail.
func (r ran) killed(launch string, budget time.Duration) string {
	detail := fmt.Sprintf("launch command %q timed out after %s (ran %.1f s)", launch, budget, r.elapsed.Seconds())
	if r.tail != "" {
		detail += "\n" + r.tail
	}
	return detail
}

// run runs `sh -c script sh args…` once within the gate's budget, under the gate's environment and
// working directory. `exec` in every probe's script replaces the shell with the launch prefix and
// Oh My Pi, and the attempt runs in its own process group, so a timeout or the daemon's stop kills
// what is actually running — a prompting `secrets`, Oh My Pi and its children — rather than a
// shell that leaves them holding the pipes (boot-probes.ts:163-167). It errs when the command could
// not be run at all, and when ctx ended, whose attempt is then not judged.
func (g pluginGate) run(ctx context.Context, script string, args ...string) (ran, error) {
	attempt, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	cmd := exec.CommandContext(attempt, "sh", append([]string{"-c", script, "sh"}, args...)...)
	environ := make([]string, 0, len(g.env))
	for _, name := range slices.Sorted(maps.Keys(g.env)) {
		environ = append(environ, name+"="+g.env[name])
	}
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
		return ran{}, ctx.Err()
	}
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) && !errors.Is(err, exec.ErrWaitDelay) {
		return ran{}, err
	}
	tail := strings.TrimSpace(stderr.String())
	if len(tail) > maxProbeStderr {
		tail = tail[len(tail)-maxProbeStderr:]
	}
	return ran{
		exit:     cmd.ProcessState.ExitCode(),
		stderr:   stderr.String(),
		output:   stderr.String() + "\n" + stdout.String(),
		tail:     tail,
		timedOut: errors.Is(attempt.Err(), context.DeadlineExceeded),
		elapsed:  time.Since(started),
	}, nil
}

// ImageProbe is `legion probe-image`, run inside the worker image: by its build's final step, and
// by the daemon's probe Sandbox (internal/runtime/sandbox/probe.go).
type ImageProbe struct {
	// Omp is the OMP invocation as the shell runs it: the image's LEGION_OMP_PATH, or --omp.
	Omp string
	// Contract is the Go daemon API contract the image's plugin must declare.
	Contract int
	// Env is the environment the probes resolve the plugin under and run Oh My Pi with: the
	// image's own, since the command runs where a pod's Oh My Pi runs.
	Env map[string]string
	// WorkDir is the probes' working directory.
	WorkDir string
	// Log receives each transient failure the retry waits out.
	Log *slog.Logger
}

// imageProbeTimeout is each image-probe attempt's budget: the default
// slow_command_timeout_seconds, so the image gate and a default-configured daemon agree
// (IMAGE_PROBE_TIMEOUT_MS, boot-probes.ts:56-58).
const imageProbeTimeout = 300 * time.Second

// ProbeImage runs the image's launch probes: pi.agents; then the daemon's own gate — the plugin
// held to Contract, then loaded, from the manifest it was held by; then the session-storage
// setting, which only the image runs (boot-probes.ts:11-21). The contract comes before the load,
// as in the daemon's gate, so a plugin of another contract is refused as a reinstall rather than
// sent to `omp plugin list`. Each attempt is bounded by imageProbeTimeout and retried under
// bootprobe.Image: an image build has no supervisor and must finish.
func ProbeImage(ctx context.Context, p ImageProbe) error {
	return imageGate(p).verifyImage(ctx)
}

func imageGate(p ImageProbe) pluginGate {
	return pluginGate{
		env: p.Env, workDir: p.WorkDir, invocation: p.Omp, timeout: imageProbeTimeout,
		retry: bootprobe.Image, contract: p.Contract, log: p.Log,
	}
}

// verifyImage runs the image's probes in ProbeImage's order.
func (g pluginGate) verifyImage(ctx context.Context) error {
	if err := g.verifyAgentsCapability(ctx); err != nil {
		return err
	}
	if err := g.verify(ctx); err != nil {
		return err
	}
	return g.verifySessionStorage(ctx)
}
