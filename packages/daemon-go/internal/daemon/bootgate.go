package daemon

import (
	"bytes"
	"cmp"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"github.com/sjawhar/legion/daemon/internal/omplaunch"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/runtime/tmux"
	workershim "github.com/sjawhar/legion/daemon/internal/shim"
)

// pluginLoadProbe is the Oh My Pi extension the load probe hands `omp models`: the shipped probe
// (packages/daemon/src/daemon/boot-probes.ts:233-238), which prints whether the plugin's
// `legion.ts` set its load marker (`Symbol.for("legion.pi-envoy.legion-loaded")`,
// packages/pi-envoy/extensions/legion.ts) — which only a plugin Oh My Pi actually loaded has done
// — and, beside it, the marker's value: the `import.meta.url` of that `legion.ts`, where the plugin
// loaded from. Given LEGION_PROMPT_AGENTS and LEGION_PROMPT_SKILLS (promptrefs), it also resolves
// those task agents and skills through Oh My Pi's own discovery over the launch's extension roots,
// as the task tool resolves an agent's name and a `skill://` read a skill's, and prints which it
// could not find; and, unless LEGION_SKIP_AGENT_MODELS is set, whether each of those agents runs
// on its own model as the task tool would select and resolve it.
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

// pluginGate runs the Oh My Pi launch probes, for three callers: the daemon's boot gate, `legion
// probe-image` in the worker image (ProbeImage), and `legion controller start` on the operator's
// machine (ProbeController), which runs the load probe alone, before any contract check, at the
// operator's terminal. As the daemon's boot gate on the plugin every pane loads, it runs before
// the daemon opens its store, so no pane launches until it passes, and it
// runs under the environment a pane will have (tmux.PaneEnvironment: the allow-listed variables,
// the XDG directories under `<state_dir>/home`), because the daemon's own HOME, profile, and XDG
// directories are not what a pane's Oh My Pi reads. Two probes, in the shipped gate's order
// (packages/daemon/src/daemon/index.ts:365-383): the contract probe reads the installed manifest
// and refuses a plugin that does not declare the gate's contract; the load probe runs Oh My Pi the
// way a pane does and refuses a plugin it did not load — installed but disabled, or not
// registered — or one it loaded from another root than the manifest the contract probe read, and
// refuses, by name, a task agent or a skill Legion's prompts name that the same Oh My Pi cannot
// find, and a task agent it would not run on the agent's own model.
//
// Inside the worker image the same gate is `legion probe-image` (ProbeImage), which adds the two
// probes only the image runs: pi.agents and the session-storage setting.
type pluginGate struct {
	// env is the environment the probes resolve and run under: a pane's, or the image's own.
	env map[string]string
	// workDir is the probes' working directory: the state directory, under which every pane's
	// workspace lives, rather than wherever the operator started the daemon.
	workDir string
	// invocation is the OMP invocation, a shell fragment (omplaunch.ResolveInvocation's).
	invocation string
	// prefix is `omp_launch_prefix`.
	prefix []string
	// timeout is each probe attempt's budget, `slow_command_timeout_seconds`.
	timeout time.Duration
	retry   bootprobe.Retry
	// contract is the Go daemon API contract the plugin must declare: the daemon's own
	// GoDaemonAPIVersion, or the one `legion probe-image` is asked for.
	contract int
	// pluginRoot is the plugin directory a pod passes Oh My Pi as its one explicit extension
	// (ImageProbe): the load probe then runs as a pod runs, with discovery off, and the contract
	// probe reads that root's manifest. Empty on tmux, where a pane loads the installed plugin
	// through discovery.
	pluginRoot string
	// roleReferences are the task agents and skills the role prompts the probed Oh My Pi is handed
	// name (promptrefs.Roles), resolved beside the plugin's own. The zero Names resolves the
	// plugin's alone.
	roleReferences promptrefs.Names
	// skipAgentModels leaves the prompt-named task agents' models unresolved (ImageProbe's
	// SkipAgentModels); every other gate holds each agent to its own model.
	skipAgentModels bool
	// stdin is each probe's standard input; nil is /dev/null. When it is a terminal this process
	// holds the foreground of, each attempt runs as the terminal's foreground job, and its stderr
	// is also copied to echo, so a launch prefix's prompt is seen and can be answered (terminalJob).
	stdin io.Reader
	echo  io.Writer
	// name begins the gate's own errors: "controller probe" for ProbeController; empty is the boot
	// gate's, which the image probe also is.
	name string
	log  *slog.Logger
}

// gateEnvironment is the environment a pane's Oh My Pi runs with, which the boot gate probes under:
// the pane environment, and each provider key the pane's shim exports from providerEnvDir as the
// shim exports it (shim.ReadProviderEnv), so a task agent whose model's key comes only through a
// provider key resolves as it will in a pane.
func gateEnvironment(environ []string, stateDir, providerEnvDir string) (map[string]string, error) {
	env := tmux.PaneEnvironment(environ, stateDir)
	if providerEnvDir == "" {
		return env, nil
	}
	pairs, err := workershim.ReadProviderEnv(providerEnvDir, func(name string) (string, bool) {
		value, ok := env[name]
		return value, ok
	})
	if err != nil {
		return nil, fmt.Errorf("boot gate: %w", err)
	}
	for _, pair := range pairs {
		name, value, _ := strings.Cut(pair, "=")
		env[name] = value
	}
	return env, nil
}

// label is the name the gate's errors begin with.
func (g pluginGate) label() string {
	if g.name == "" {
		return "boot gate"
	}
	return g.name
}

// verify runs the two probes. A refusal names what the operator has to change; a gate the daemon's
// stop interrupted returns an error wrapping ctx's.
func (g pluginGate) verify(ctx context.Context) error {
	lane, err := g.lane()
	if err != nil {
		return err
	}
	plugin, err := readPluginManifest(lane.manifest, lane.installInto, g.contract)
	if err != nil {
		return err
	}
	names, err := promptReferences(lane.manifest, plugin.skills)
	if err != nil {
		return err
	}
	names.Merge(g.roleReferences)
	check := promptCheck{names: names, skipAgentModels: g.skipAgentModels}
	loadedFrom, err := g.loadedFrom(ctx, lane, lane.notLoaded(plugin.version), check)
	if err != nil {
		return err
	}
	if err := lane.verifyLoadedFrom(loadedFrom, g.contract); err != nil {
		return err
	}
	g.log.Info("boot gate: pi-legion-envoy speaks this daemon's contract and loads",
		"lane", lane.described, "manifest", lane.manifest, "version", plugin.version, "goDaemonApiVersion", g.contract)
	return nil
}

// pluginLane is how the probed Oh My Pi loads pi-legion-envoy, resolved once for the gate (lane):
// a pane's installed plugin through discovery, or, given a plugin root, a pod's lane, that root as
// Oh My Pi's one explicit extension with discovery off. It carries the manifest the contract probe
// reads and the words every refusal uses, so none of them sends the operator to the other lane's
// remedy: in a pod's lane, discovery is off and the profile's plugin install is never loaded.
type pluginLane struct {
	// manifest is the pi-legion-envoy manifest the contract probe reads; profile the OMP profile a
	// pane resolves it under ("" for the default profile, and in a pod's lane).
	manifest, profile string
	// root is a pod's plugin root, absolute; "" for a pane's discovery.
	root string
	// installInto is where the contract refusal says to install the release built from this
	// daemon's commit.
	installInto string
	// described is how the probed Oh My Pi loaded the plugin, as a refusal of a task agent or skill
	// it could not find says it.
	described string
}

// lane resolves the gate's plugin lane. A relative plugin root is resolved against this process's
// working directory, so the directory the load probe hands Oh My Pi and the manifest the contract
// probe reads are the same one, and the loaded-from check compares absolute paths.
func (g pluginGate) lane() (pluginLane, error) {
	if g.pluginRoot != "" {
		root, err := filepath.Abs(g.pluginRoot)
		if err != nil {
			return pluginLane{}, fmt.Errorf("%s: resolve the plugin root %s: %w", g.label(), g.pluginRoot, err)
		}
		return pluginLane{
			manifest:    filepath.Join(root, "package.json"),
			root:        root,
			installInto: "the plugin root " + root + ", which a pod loads as its one explicit extension",
			described:   "loading the plugin from " + root + " with discovery off, as a pod does",
		}, nil
	}
	manifest, profile, err := pluginManifestPath(g.env, g.workDir)
	if err != nil {
		return pluginLane{}, err
	}
	return pluginLane{
		manifest:    manifest,
		profile:     profile,
		installInto: profileWords(profile),
		described:   "in a pane of " + profileWords(profile),
	}, nil
}

// notLoaded is the load probe's refusal for an Oh My Pi that answered without loading the plugin.
func (l pluginLane) notLoaded(version string) error {
	if l.root != "" {
		return fmt.Errorf("pi-legion-envoy %s at %s did not load with discovery off and %s as Oh My Pi's one explicit extension, as a pod loads it: the plugin root holds no plugin Oh My Pi can load; build the worker image from this daemon's commit",
			version, l.root, l.root)
	}
	list := "omp plugin list"
	if l.profile != "" {
		list = "OMP_PROFILE=" + l.profile + " " + list
	}
	return fmt.Errorf("pi-legion-envoy %s is installed but not loaded by omp (disabled or unregistered): run %s", version, list)
}

// loadArgs are the load probe's extension flags and their arguments: the probe beside what
// discovery loads, or, in a pod's lane, discovery off, the plugin root, then the probe.
func (l pluginLane) loadArgs(probe string) (string, []string) {
	if l.root != "" {
		return `--no-extensions --extension "$1" --extension "$2"`, []string{l.root, probe}
	}
	return `--extension "$1"`, []string{probe}
}

var (
	// profileName and windowsReservedProfile are the profile names Oh My Pi accepts and the device
	// names it refuses among them (@oh-my-pi/pi-utils src/dirs.ts:38, :49).
	profileName            = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	windowsReservedProfile = regexp.MustCompile(`(?i)^(?:CON|PRN|AUX|NUL|COM[0-9]|LPT[0-9])(?:\..*)?$`)
)

// pluginManifestPath is the installed pi-legion-envoy manifest where Oh My Pi, started under env in
// workDir, looks for its plugins, and the profile that decided it ("" for the default profile). It
// ports Oh My Pi's resolution (@oh-my-pi/pi-utils 18.1.21, src/dirs.ts) over env alone:
//
//   - the profile is OMP_PROFILE when it is set at all, even empty, else PI_PROFILE; trimmed, an
//     empty name or "default" is the default profile, and a name Oh My Pi would refuse is refused
//     here in its words (resolveProfileEnv, normalizeProfileName, :59-88);
//   - the config root is PI_CONFIG_DIR, else `.omp`, under the home directory — HOME, else the
//     account's, as `os.homedir()` answers — with `profiles/<name>` under it for a named profile
//     (getConfigDirName, getBaseConfigRoot, getProfileConfigRoot, :111-118, :282-284);
//   - the agent directory is PI_CODING_AGENT_DIR, resolved against workDir as `path.resolve`
//     resolves it against Oh My Pi's own, under the default profile only, and
//     only when it is not the agent directory of the profile PI_PROFILE names, which an Oh My Pi
//     running under that profile hands its children (resolveActiveAgentDirOverride,
//     resolvePreProfileAgentDir, isProfileDerivedAgentDir, :132-134, :411-432); else it is the
//     config root's own `agent`. That is the one way the variable reaches the plugins: an agent
//     directory other than the config root's own turns the XDG data root off (:320-323, :340);
//   - the data root is `$XDG_DATA_HOME/omp` for the default profile, or
//     `$XDG_DATA_HOME/omp/profiles/<name>` for a named one, when that directory already exists
//     and the XDG data root is on, else the config root (DirResolver's constructor, :316-375);
//   - the plugins are `plugins/node_modules` under the data root (getPluginsDir,
//     getPluginsNodeModules, :607-617), and the manifest is the package's own `package.json`
//     there (legionPluginManifestPath, packages/daemon/src/daemon/boot-probes.ts:240-244).
//
// env is all it reads. Before it resolves its directories, Oh My Pi fills XDG_DATA_HOME,
// PI_CONFIG_DIR and PI_CODING_AGENT_DIR from dotenv files it reads itself (~/.env, the config root's
// .env, the agent directory's .env, its working directory's .env; env.ts, then refreshDirsFromEnv),
// where parseEnvFile also mirrors OMP_CONFIG_DIR and OMP_CODING_AGENT_DIR onto the PI_ names, and a
// launch prefix can set any variable; neither reaches env. Where either moves the plugin root, this
// names another manifest than the one Oh My Pi loads. The daemon's gate then refuses, because its
// load probe holds what Oh My Pi loaded to this manifest (verifyLoadedFrom); `legion controller
// start` reads no manifest this names, and holds the one Oh My Pi loaded instead (ProbeController).
func pluginManifestPath(env map[string]string, workDir string) (string, string, error) {
	requested, set := env["OMP_PROFILE"]
	if !set {
		requested = env["PI_PROFILE"]
	}
	profile, valid := normalizeProfile(requested)
	if !valid {
		return "", "", fmt.Errorf(`Invalid OMP profile %q in the environment Oh My Pi starts under. Profile names must match %s, cannot be "." or "..", cannot end with ".", and cannot be a Windows reserved device name (CON, PRN, AUX, NUL, COM0-9, LPT0-9, or any of those with an extension).`,
			requested, profileName)
	}
	home, err := ompHome(env)
	if err != nil {
		return "", "", err
	}
	base := filepath.Join(home, cmp.Or(env["PI_CONFIG_DIR"], ".omp"))
	root := base
	if profile != "" {
		root = filepath.Join(root, "profiles", profile)
	}
	xdgOn := true
	if agent := env["PI_CODING_AGENT_DIR"]; agent != "" && profile == "" {
		handedDown, valid := normalizeProfile(env["PI_PROFILE"])
		if !valid || handedDown == "" || agent != filepath.Join(base, "profiles", handedDown, "agent") {
			if !filepath.IsAbs(agent) {
				agent = filepath.Join(workDir, agent)
			}
			xdgOn = filepath.Clean(agent) == filepath.Join(root, "agent")
		}
	}
	if xdg := env["XDG_DATA_HOME"]; xdgOn && xdg != "" && (goruntime.GOOS == "linux" || goruntime.GOOS == "darwin") {
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

// normalizeProfile is a profile name as Oh My Pi reads it (normalizeProfileName): trimmed, with an
// empty name or "default" the default profile, "", and a name Oh My Pi would refuse not valid.
func normalizeProfile(requested string) (string, bool) {
	profile := strings.TrimSpace(requested)
	if profile == "default" {
		profile = ""
	}
	if profile != "" && (profile == "." || profile == ".." || strings.HasSuffix(profile, ".") ||
		!profileName.MatchString(profile) || windowsReservedProfile.MatchString(profile)) {
		return "", false
	}
	return profile, true
}

// ompHome is the home directory Oh My Pi started under env reads its roots under: HOME, else the
// account's, as `os.homedir()` answers.
func ompHome(env map[string]string) (string, error) {
	if home := env["HOME"]; home != "" {
		return home, nil
	}
	account, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve the home directory Oh My Pi reads its plugins under: HOME is not set, and %w", err)
	}
	return account.HomeDir, nil
}

// profileWords names a profile the way a refusal tells the operator where to install.
func profileWords(profile string) string {
	if profile == "" {
		return "the default OMP profile"
	}
	return "OMP profile " + profile
}

// pluginManifest is what the gate reads from a pi-legion-envoy manifest: its package version and
// the skills directories it ships (`omp.skills`, relative to the plugin root).
type pluginManifest struct {
	version string
	skills  []string
}

// readPluginManifest reads the manifest once and is the contract probe
// (verifyLegionPluginContract, boot-probes.ts:267-298, on the Go daemon's own field): the
// manifest's `legion.goDaemonApiVersion` must be contract. A manifest that is missing,
// unreadable, or without the field is the same refusal, never a fallback — the load probe would
// call such a plugin merely "not loaded" and send the operator to `omp plugin list` when the fix
// is a reinstall. installInto names where the refusal says to install the release.
func readPluginManifest(manifest, installInto string, contract int) (pluginManifest, error) {
	install := fmt.Sprintf("Install the @sjawhar/pi-legion-envoy release built from this daemon's commit into %s.", installInto)
	raw, err := os.ReadFile(manifest)
	var parsed any
	if err == nil {
		err = json.Unmarshal(raw, &parsed)
	}
	if err != nil {
		return pluginManifest{}, fmt.Errorf("pi-legion-envoy manifest at %s could not be read (%v); this daemon requires a plugin speaking Go daemon API contract %d. %s",
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
		plugin := pluginManifest{version: version}
		// `omp` and `omp.skills` may be absent or null; any other shape is a manifest the gate
		// cannot read the shipped skills from, and every agent they dispatch would go unchecked.
		omp, isObject := record["omp"].(map[string]any)
		if value := record["omp"]; value != nil && !isObject {
			return pluginManifest{}, fmt.Errorf("pi-legion-envoy manifest at %s has an `omp` that is not an object (%v). %s", manifest, value, install)
		}
		listed, isList := omp["skills"].([]any)
		if value := omp["skills"]; value != nil && !isList {
			return pluginManifest{}, fmt.Errorf("pi-legion-envoy manifest at %s has an `omp.skills` that is not a list of directories (%v). %s", manifest, value, install)
		}
		for _, entry := range listed {
			dir, ok := entry.(string)
			if !ok {
				return pluginManifest{}, fmt.Errorf("pi-legion-envoy manifest at %s lists a skills entry %v that is not a directory name. %s", manifest, entry, install)
			}
			plugin.skills = append(plugin.skills, dir)
		}
		return plugin, nil
	}
	spoken := "none"
	if present {
		encoded, _ := json.Marshal(declared)
		spoken = string(encoded)
	}
	return pluginManifest{}, fmt.Errorf("pi-legion-envoy at %s (package %s) speaks Go daemon API contract %s; this daemon requires %d. %s",
		manifest, version, spoken, contract, install)
}

// loadedFrom is the load probe (verifyLegionPluginLoaded, boot-probes.ts:313-392), under the gate's
// retry, and answers where the plugin loaded from: Oh My Pi, launched as a pane or a pod launches
// it — through the launch prefix, under the gate's environment, with its XDG directories created
// first as a spawn creates them — lists its models with the probe extension added in lane's way,
// and passes only when the probe saw the plugin's load marker, found every task agent and skill
// check names (none for the controller probe), and answered that each of those agents runs on its
// own model (unless the gate skips that). notLoaded is the refusal for an Oh My Pi that answered
// without loading it. The classification is the shipped one (killedOutcome, :94-122, :348-360).
func (g pluginGate) loadedFrom(ctx context.Context, lane pluginLane, notLoaded error, check promptCheck) (string, error) {
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME"} {
		if dir := g.env[name]; dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return "", fmt.Errorf("%s: create the %s Oh My Pi runs under: %w", g.label(), name, err)
			}
		}
	}
	dir, probe, err := g.writeProbe("legion-plugin-probe-", pluginLoadProbe)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	launch := omplaunch.WithPrefix(g.prefix, g.invocation)
	location := ""
	err = bootprobe.Run(ctx, "pi-legion-envoy load", g.retry, g.log, func(ctx context.Context) bootprobe.Outcome {
		outcome, from := g.probeLoad(ctx, launch, probe, lane, notLoaded, check)
		location = from
		return outcome
	})
	return location, err
}

// verifyLoadedFrom holds the plugin Oh My Pi loaded to the manifest the contract probe read: the
// load probe reports the URL its `legion.ts` loaded from, and the pi-legion-envoy manifest above
// that file must be the same file, links resolved, as the lane's manifest. In a pane's lane they
// part when something in the launch picks its own plugin root — a launch prefix that sets
// OMP_PROFILE (`env OMP_PROFILE=… --`), a dotenv file Oh My Pi reads; in a pod's, when the
// invocation or its launch prefix loads another copy beside the explicit root. Either way the
// contract probe would otherwise have vouched for a plugin no pane or pod runs.
func (l pluginLane) verifyLoadedFrom(location string, contract int) error {
	owner, err := loadedManifest(location)
	if err != nil {
		return err
	}
	read, err := filepath.EvalSymlinks(l.manifest)
	if err != nil {
		return fmt.Errorf("boot gate: resolve the manifest %s the contract probe read: %w", l.manifest, err)
	}
	switch {
	case owner == read:
		return nil
	case l.root != "":
		return fmt.Errorf("pi-legion-envoy loads from %s, but the probe passed %s as Oh My Pi's one explicit extension, with discovery off, and held its manifest %s to Go daemon API contract %d: the OMP invocation, or its launch prefix, loads another copy of the plugin",
			owner, l.root, read, contract)
	}
	return fmt.Errorf("pi-legion-envoy loads in a pane from %s, but the manifest this gate held to Go daemon API contract %d is %s, at the plugin root of %s in the pane environment: the launch prefix, or a dotenv file Oh My Pi reads, selects another plugin root. Select the OMP profile in the daemon's own environment, which every pane inherits",
		owner, contract, read, profileWords(l.profile))
}

// loadedManifest is the pi-legion-envoy manifest of the file the load probe reported the plugin
// loaded from (a `file:` URL, as import.meta.url renders it), links resolved.
func loadedManifest(location string) (string, error) {
	loaded, err := url.Parse(location)
	if err != nil || loaded.Scheme != "file" || loaded.Path == "" {
		return "", fmt.Errorf("the pi-legion-envoy load probe reported the plugin loaded from %q, which is not a file the gate can hold to a manifest", location)
	}
	owner, err := owningManifest(loaded.Path)
	if err != nil {
		return "", fmt.Errorf("pi-legion-envoy loads from %s, and the gate cannot hold it to a manifest: %w", loaded.Path, err)
	}
	return owner, nil
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

// probeLoad is one load-probe attempt, and, on a pass, where the plugin loaded from. notLoaded is
// the refusal for an Oh My Pi that answered without loading the plugin.
func (g pluginGate) probeLoad(ctx context.Context, launch, probe string, lane pluginLane, notLoaded error, check promptCheck) (bootprobe.Outcome, string) {
	flags, args := lane.loadArgs(probe)
	script := `exec ` + launch + ` models ` + flags + ` --json >/dev/null`
	if export := check.export(lane.root != ""); export != "" {
		script = export + "; " + script
	}
	r, err := g.run(ctx, script, args...)
	if err != nil {
		return bootprobe.Outcome{Refusal: fmt.Errorf("%s: run the pi-legion-envoy load probe: %w", g.label(), err)}, ""
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
		if refusal := check.refusal(r.output, lane.described); refusal != nil {
			return bootprobe.Outcome{Refusal: refusal}, ""
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
		return bootprobe.Outcome{Refusal: errors.New(r.quoting(fmt.Sprintf("OMP launch probe failed (exit %d) for launch command %q", r.exit, launch)))}, ""
	}
	return bootprobe.Outcome{Refusal: notLoaded}, ""
}

// verifyAgentsCapability is the pi.agents probe (verifyOmpAgentsCapability, boot-probes.ts:
// 168-220): Oh My Pi, with no extension but the probe's, lists its models, and passes only when
// the probe saw `pi.agents` and Oh My Pi exited cleanly. Oh My Pi dying after the probe answered
// yes, or an attempt the budget cut off before it answered, is transient; everything else — the
// `missing` answer, a clean exit without an answer, a launch that failed before Oh My Pi — is an
// answer no retry changes.
func (g pluginGate) verifyAgentsCapability(ctx context.Context) error {
	dir, probe, err := g.writeProbe("legion-omp-probe-", agentsProbe)
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	launch := omplaunch.WithPrefix(g.prefix, g.invocation)
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
		return bootprobe.Outcome{Refusal: errors.New(r.quoting(fmt.Sprintf("the OMP launch command %q does not expose pi.agents", launch)))}
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
	launch := omplaunch.WithPrefix(g.prefix, g.invocation)
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
		return bootprobe.Outcome{Detail: r.quoting(fmt.Sprintf("launch command %q exited %d without naming %s", launch, r.exit, sessionStorageVariable))}
	})
}

// writeProbe writes an extension probe into a directory of its own, which the caller removes.
func (g pluginGate) writeProbe(pattern string, source []byte) (string, string, error) {
	dir, err := os.MkdirTemp("", pattern)
	if err != nil {
		return "", "", fmt.Errorf("%s: create a probe directory: %w", g.label(), err)
	}
	probe := filepath.Join(dir, "probe.mjs")
	if err := os.WriteFile(probe, source, 0o600); err != nil {
		os.RemoveAll(dir)
		return "", "", fmt.Errorf("%s: write the probe extension: %w", g.label(), err)
	}
	return dir, probe, nil
}

// ran is one probe command's run: its exit code (-1 when killed), its stderr, its stdout, stderr
// and stdout together, the tail of stderr a refusal quotes, and whether the budget killed it.
type ran struct {
	exit                   int
	stderr, stdout, output string
	tail                   string
	timedOut               bool
	elapsed                time.Duration
}

// quoting is message followed by the stderr tail, after ": ", when stderr said anything.
func (r ran) quoting(message string) string {
	if r.tail == "" {
		return message
	}
	return message + ": " + r.tail
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
// shell that leaves them holding the pipes (boot-probes.ts:163-167). That group is a background
// one, except at the operator's terminal, where it is the terminal's foreground job
// (terminalJob). It errs when the command could not be run at all, when ctx ended, whose attempt
// is then not judged, and when the terminal's Ctrl-C ended the attempt.
func (g pluginGate) run(ctx context.Context, script string, args ...string) (ran, error) {
	attempt, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()
	job := foregroundOf(g.stdin, g.echo)
	cmd := exec.CommandContext(attempt, "sh", append([]string{"-c", job.script(script), "sh"}, args...)...)
	environ := make([]string, 0, len(g.env))
	for _, name := range slices.Sorted(maps.Keys(g.env)) {
		environ = append(environ, name+"="+g.env[name])
	}
	cmd.Env = environ
	cmd.Dir = g.workDir
	cmd.Stdin = g.stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	job.attach(cmd)
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	started := time.Now()
	err := cmd.Run()
	job.release()
	if ctx.Err() != nil {
		return ran{}, ctx.Err()
	}
	if job.interrupted(cmd.ProcessState) {
		return ran{}, errors.New("the probe was interrupted at the terminal")
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
		stdout:   stdout.String(),
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
	// PluginRoot is the plugin directory the pod's Oh My Pi loads as its one explicit extension
	// (`--no-extensions --extension <root>`), and is required: the load probe runs the same way,
	// and the contract probe reads that root's manifest, so the probe certifies the lane a pod
	// uses. A relative root is resolved against this process's working directory.
	PluginRoot string
	// RoleReferences are the task agents and skills the role prompts a pod is handed name
	// (promptrefs.Roles), and are required: the daemon's own, which it inlines into every Sandbox pod,
	// or the image's when the command is given none.
	RoleReferences promptrefs.Names
	// SkipAgentModels leaves the task agents' models unresolved: the image build's probe, which runs
	// with none of the operator's model configuration.
	SkipAgentModels bool
}

// defaultProbeTimeout is each image-probe and controller-probe attempt's budget: the default
// slow_command_timeout_seconds, so both agree with a default-configured daemon's gate
// (IMAGE_PROBE_TIMEOUT_MS, boot-probes.ts:56-58).
const defaultProbeTimeout = 300 * time.Second

// ProbeImage runs the image's launch probes: pi.agents; then the daemon's own gate — the plugin
// held to Contract, then loaded, from the manifest it was held by; then the session-storage
// setting, which only the image runs (boot-probes.ts:11-21). The contract comes before
// the load, as in the daemon's gate, so a plugin of another contract is refused as a reinstall
// rather than sent to `omp plugin list`. Each attempt is bounded by defaultProbeTimeout and retried
// under bootprobe.Image: an image build has no supervisor and must finish.
func ProbeImage(ctx context.Context, p ImageProbe) error {
	if p.PluginRoot == "" {
		return errors.New("image probe: ImageProbe.PluginRoot is required: the plugin directory a pod loads as its one explicit extension")
	}
	if p.RoleReferences.Zero() {
		return errors.New("image probe: ImageProbe.RoleReferences is required: the references of the role prompts a pod is handed")
	}
	return pluginGate{
		env: p.Env, workDir: p.WorkDir, invocation: p.Omp, timeout: defaultProbeTimeout,
		retry: bootprobe.Image, contract: p.Contract,
		pluginRoot: p.PluginRoot, roleReferences: p.RoleReferences, skipAgentModels: p.SkipAgentModels,
		log: p.Log,
	}.verifyImage(ctx)
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

// ControllerProbe is `legion controller start`'s gate on the operator's Oh My Pi, run before the
// command's one daemon call: the mint that call asks for revokes the incumbent controller, and no
// boot gate runs on the operator's machine.
type ControllerProbe struct {
	// Omp is the resolved OMP invocation and Prefix the launch prefix: what the controller runs.
	Omp    string
	Prefix []string
	// Env is the environment the controller's Oh My Pi runs under, and WorkDir the directory it
	// runs in, `<state_dir>/controller`, which must exist.
	Env     map[string]string
	WorkDir string
	// Stdin is the operator's standard input, which the controller then reads: a launch prefix
	// like `secrets` scopes a human-tier grant by the terminal it runs on, and refuses a stdin that
	// is not one. When it is the terminal the command runs at, each attempt runs as its foreground
	// job, so a prefix may also read it (a PIN) or change its modes.
	Stdin io.Reader
	// Stderr also receives each foreground attempt's stderr, where a prefix's prompt goes.
	Stderr io.Writer
	// Contract is the Go daemon API contract the loaded plugin must declare.
	Contract int
	// Log receives each transient failure the retry waits out.
	Log *slog.Logger
}

// controllerProbeRetry is the controller probe's retry: short, since the operator waits at the
// terminal, and bounded, since nothing supervises the command.
var controllerProbeRetry = bootprobe.Retry{Initial: 2 * time.Second, Max: 10 * time.Second, Attempts: 3}

// ProbeController launches Oh My Pi as the controller launches it — the launch prefix and the
// invocation, under the controller's environment, in its directory — and holds the pi-legion-envoy
// it loaded to Contract: the manifest of the file Oh My Pi reports loading it from. Which copy loads
// is Oh My Pi's own resolution rather than a port of it, so a `--profile` in the invocation, a
// project plugin root above the directory, a dotenv file Oh My Pi reads itself, the launch prefix,
// or a symlinked state directory cannot make the check read another copy than the one the
// controller runs. Unlike the daemon's gate, which names every variable of the pane environment it
// probes, the load comes first: on the operator's machine only Oh My Pi can say which manifest to
// read. Each attempt has the default slow-command budget, defaultProbeTimeout, as the image probe's
// does.
func ProbeController(ctx context.Context, p ControllerProbe) error {
	g := pluginGate{
		env: p.Env, workDir: p.WorkDir, invocation: p.Omp, prefix: p.Prefix, timeout: defaultProbeTimeout,
		retry: controllerProbeRetry, contract: p.Contract, stdin: p.Stdin, echo: p.Stderr,
		name: "controller probe", log: p.Log,
	}
	// Neither refusal names a profile: which one Oh My Pi reads is its own resolution, which this
	// does not port, so the words say only what Oh My Pi did, where it ran, and how.
	launch := omplaunch.WithPrefix(p.Prefix, p.Omp)
	location, err := g.loadedFrom(ctx, pluginLane{}, fmt.Errorf("Oh My Pi, launched as the controller launches it (%q, in %s), did not load pi-legion-envoy (not installed, disabled, or unregistered). Install the @sjawhar/pi-legion-envoy release built from this daemon's commit into the Oh My Pi the controller runs, and check it with `cd %s && %s plugin list` under the controller's environment: a .env or a project plugin root there applies",
		launch, p.WorkDir, p.WorkDir, launch), promptCheck{})
	if err != nil {
		return err
	}
	manifest, err := loadedManifest(location)
	if err != nil {
		return err
	}
	_, err = readPluginManifest(manifest, "the Oh My Pi installation it loaded from", p.Contract)
	return err
}
