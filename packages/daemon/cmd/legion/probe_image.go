package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/capabilities"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/launcher"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/shim"
)

// imageClaudeRoot and imageClaudeCodePin are where the worker image carries the claude-envoy root
// and the Claude Code pin (packages/daemon/docker/worker.Dockerfile; claudePluginDir and
// claudeCodePin, internal/runtime/sandbox/names.go, which a Claude role's pod names): the defaults
// of --claude-root and --claude-code-pin.
const (
	imageClaudeRoot    = "/opt/legion/claude-envoy"
	imageClaudeCodePin = "/opt/legion/claude-code"
)

// digits is what --daemon-api-version accepts before it is read as a number.
var digits = regexp.MustCompile(`^[0-9]+$`)

// runProbeImage is `legion probe-image`, run inside the worker image: by its build's final step
// (packages/daemon/docker/worker.Dockerfile), and with the daemon's own contract by the daemon's
// probe Sandbox, which reads the OK line back from the pod's log (internal/runtime/sandbox,
// ProbeImage). Both pass --plugin-root and --envoy-plugin-root, the Legion and Envoy plugin
// directories a pod loads as its two explicit extensions, and the command refuses a run without
// either (exit 2): the probe certifies the lane a pod uses, so a build line that lost a flag fails
// the build instead of probing a lane no pod loads. Every run also checks the Claude lane:
// --claude-root, the claude-envoy package root the image carries, and --claude-code-pin, the
// directory of the Claude Code pin a pod installs Claude Code from at launch, both defaulting to
// where the worker image carries them, so the daemon's probe Sandbox, which names neither, checks
// them as the build does. The image carries no Claude Code — its licence grants no
// redistribution and the image is public — so the lane holds the root's built channel server and
// generated Legion plugin, and its manifest to the same contract, and holds the pin to one a pod
// can install and verify, exact and checksummed (daemon.ProbeImage, whose verifyClaude says why
// it reads the pin rather than installing from it); the claude-code capability row checks the
// same two facts.
// It runs the image's launch probes under the image's own environment (daemon.ProbeImage), then
// the capability check (capabilities.CheckImage) under the same environment and launch: every
// image-site capability of the list every worker is held to (LEGION-578), with the whole table
// printed, one line per capability, before anything else — so the probe pod's log reads as the
// declared list — and a missing one named on stderr, exit 1, after the table. Then it reads
// whether the image's Oh My Pi falls back to another model (capabilities.ReadModelFallback): a
// failure to read it is not a refusal — the probe writes the failure to stderr and carries on
// with the mark bootprobe.ModelFallbackUnread, so the deployment's capability report shows that
// row open rather than the probe's own line going missing. When every check passes, it prints
// bootprobe.OKLine with the capabilities and model-fallback marks; a failed capability is the
// probe's message, exit 1, so a broken image never publishes. Unlike the
// TypeScript command, the contract is always checked:
// with none named, against this binary's own DaemonAPIVersion, which the plugin packed from the
// same commit must declare. The load probe also holds every task agent the
// prompts dispatch to its own model; the OK line says so (agent-models=resolved), or that the
// build's probe, which has no operator model configuration, skipped it (--skip-agent-models).
// With --skip-browser, the build of an image for a deployment that turns Oh My Pi's browser off
// (`browser.enabled: false`), which carries no Chromium, the capability check leaves the browser
// row unchecked, and the OK line's capabilities mark says so (capabilities=browser-skipped, which
// the daemon's probe Sandbox refuses: it never passes the flag, and passes only an image whose
// whole capability list was checked). The
// probe Sandbox starts it on the pod baseline's variables, with no state home of its
// own: with --pod-safety, as a worker's shim starts Oh My Pi (podsafety.Apply: the two
// session-placing variables where the pod leaves them unset, and no overlay of Legion's own,
// since it writes none, so the probe reads the role's own PI_CONFIG_FILES as its pod carries
// it), but not the per-role state home the shim makes (podsafety.EnsureStateHome): the probe
// pod runs one container, so no broker lock collides; with --provider-env-dir, each
// provider key exported after it as the shim exports them (shim.ReadProviderEnv); and with
// --role-references, the references of the role prompts the daemon inlines into its pods
// (promptrefs.Encode's encoding, decoded as the flags are read). Without it, the probe resolves the
// references of the role prompts this binary embeds (prompts.RoleReferences). When its environment
// names a NATS nkey seed (a probe pod's NATS_NKEY_SEED_FILE, the providers Secret's key), that seed
// is read as the daemon reads its own (natsauth.Seed) and must be a user's, and the line before the
// OK line names that user's public key (bootprobe.NATSUserLine), never the seed, for the daemon to
// compare with its own. With --role-namespaces, which the probe Sandbox passes and the image build
// does not (a build's container may not make the namespaces), it first runs one role generation as
// an issue pod's launcher runs every role, its own `legion version` as the shim, over a layout of
// its own in that directory (launcher.CheckRoleNamespaces), and refuses, exit 1, when the
// namespaces, a mount role-init makes or the shim fails there. That layout's home is an empty
// directory on the probe's volume carrying no mount, so the check does not bind a home's volume
// back or copy a config home.
func runProbeImage(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("probe-image", "usage: legion probe-image --plugin-root <dir> --envoy-plugin-root <dir> [flags]", stderr)
	omp := flags.String("omp", "", "the OMP executable to probe (default: $LEGION_OMP_PATH)")
	contract := flags.String("daemon-api-version", strconv.Itoa(api.DaemonAPIVersion),
		"the daemon API contract the image's pi-legion must declare (the daemon's probe Sandbox passes its own)")
	pluginRoot := flags.String("plugin-root", "", "the Legion plugin directory a pod loads as an explicit extension, which the load probe loads the same way (required)")
	envoyPluginRoot := flags.String("envoy-plugin-root", "", "the Envoy plugin directory a pod loads as its other explicit extension, which the load probe loads the same way (required)")
	claudeRoot := flags.String("claude-root", imageClaudeRoot, "the claude-envoy package root a worker on the `claude` backend loads, which the Claude lane checks")
	claudeCodePin := flags.String("claude-code-pin", imageClaudeCodePin, "the directory of the package.json and package-lock.json a pod installs Claude Code from at launch, which the Claude lane checks")
	podSafety := flags.Bool("pod-safety", false, "run the probes on a pod's baseline (internal/podsafety), as a pod's shim starts Oh My Pi")
	providerEnvDir := flags.String("provider-env-dir", "", "a directory whose files NAME=contents the probes' Oh My Pi gets, as a worker's shim exports them")
	probeBudget := flags.Duration("probe-budget", 0, "the duration the daemon's probe pod reserves for this command")
	skipAgentModels := flags.Bool("skip-agent-models", false, "leave the prompt-named task agents' models unresolved (the image build's probe)")
	skipBrowser := flags.Bool("skip-browser", false, "leave the browser row unchecked: the build of an image for a deployment that turns Oh My Pi's browser off (browser.enabled: false), which carries no Chromium")
	roleReferences := flags.String("role-references", "", "the task agents and skills the daemon's own role prompts name, resolved in place of the role prompts this binary embeds")
	roleNamespaces := flags.String("role-namespaces", "", "run one role generation's namespaces (legion launcher role-init) over a layout of its own in this directory, on a volume of its own, and refuse when it fails; its home is an empty directory there, not the image's (the probe Sandbox passes it)")
	var roleOverlays roleConfigFilesFlag
	flags.Var(&roleOverlays, strings.TrimPrefix(bootprobe.RoleConfigFilesFlag, "--"), "<role>=<file>[:<file>…]: a role's own Oh My Pi overlays (role_config_files), read after the environment's PI_CONFIG_FILES as that role's Oh My Pi starts on them; repeatable, one per role (the probe Sandbox passes it)")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "legion probe-image: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *pluginRoot == "" {
		fmt.Fprintln(stderr, "legion probe-image: --plugin-root is required: the Legion plugin directory a pod loads as an explicit extension, which the load probe loads the same way")
		return 2
	}
	if *envoyPluginRoot == "" {
		fmt.Fprintln(stderr, "legion probe-image: --envoy-plugin-root is required: the Envoy plugin directory a pod loads as its other explicit extension, which the load probe loads the same way")
		return 2
	}
	if *claudeRoot == "" || *claudeCodePin == "" {
		fmt.Fprintln(stderr, "legion probe-image: --claude-root and --claude-code-pin each name a directory: the claude-envoy plugin and the Claude Code pin a worker on the `claude` backend needs, which the Claude lane checks")
		return 2
	}

	if *probeBudget < 0 {
		fmt.Fprintln(stderr, "legion probe-image: --probe-budget must not be negative")
		return 1
	}
	if *probeBudget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *probeBudget)
		defer cancel()
	}
	if *roleNamespaces != "" {
		self, err := os.Executable()
		if err == nil {
			err = launcher.CheckRoleNamespaces(*roleNamespaces, self, "version")
		}
		if err != nil {
			fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
			return 1
		}
	}
	// The role prompts a pod is handed: the daemon's, when it passes their references, else the
	// ones this binary embeds.
	var references promptrefs.Names
	if *roleReferences == "" {
		references = prompts.RoleReferences()
	} else {
		decoded, err := promptrefs.Decode(*roleReferences)
		if err != nil {
			fmt.Fprintf(stderr, "legion probe-image: --role-references: %v\n", err)
			return 1
		}
		references = decoded
	}
	invocation := *omp
	if invocation == "" {
		invocation = os.Getenv("LEGION_OMP_PATH")
	}
	if invocation == "" {
		fmt.Fprintln(stderr, "legion probe-image: set LEGION_OMP_PATH (or pass --omp) to the OMP executable to probe")
		return 1
	}
	expected, err := strconv.Atoi(*contract)
	if !digits.MatchString(*contract) || err != nil || expected < 1 {
		fmt.Fprintf(stderr, "legion probe-image: --daemon-api-version must be a positive integer (got %q)\n", *contract)
		return 1
	}
	workDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	// The seed a pod's Envoy connections authenticate with: a probe pod's NATS_NKEY_SEED_FILE names
	// the providers Secret's NATS_NKEY_SEED, which must be a user's seed, and whose user the daemon
	// compares with its own seed's. The image build's probe names none.
	natsUser := ""
	if seed, err := natsauth.Seed("", os.LookupEnv); err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	} else if seed != "" {
		if natsUser, err = natsauth.PublicKey(seed); err != nil {
			fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
			return 1
		}
	}
	environ := os.Environ()
	if *podSafety {
		environ = podsafety.Apply(environ)
	}
	if *providerEnvDir != "" {
		providerEnv, err := shim.ReadProviderEnv(*providerEnvDir, os.LookupEnv)
		if err != nil {
			fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
			return 1
		}
		environ = append(environ, providerEnv...)
	}
	env := map[string]string{}
	for _, pair := range environ {
		if name, value, ok := strings.Cut(pair, "="); ok {
			env[name] = value
		}
	}
	err = daemon.ProbeImage(ctx, daemon.ImageProbe{
		Omp: invocation, Contract: expected, Env: env, WorkDir: workDir, PluginRoot: *pluginRoot, EnvoyPluginRoot: *envoyPluginRoot,
		ClaudeRoot: *claudeRoot, ClaudeCodePin: *claudeCodePin,
		RoleReferences: references, SkipAgentModels: *skipAgentModels,
		Log: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	// The capability check reads the environment the launch probes ran under — the pod baseline's
	// and the provider keys' included — so a PATH or a PUPPETEER_EXECUTABLE_PATH the operator's pod
	// env sets is checked as a worker's Oh My Pi would honour it. The table is printed whole even
	// when a row is missing: the operator reads what was checked, then why it failed.
	img := capabilities.Image{
		Launch:        invocation,
		Env:           environ,
		WorkDir:       workDir,
		ClaudeRoot:    *claudeRoot,
		ClaudeCodePin: *claudeCodePin,
		SkipBrowser:   *skipBrowser,
	}
	lines, err := capabilities.CheckImage(ctx, img)
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	if err := newTLSPositiveControls(ctx, environ, workDir); err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, tlsPositiveControlsLine(compromisedTLSSites()))
	observations, err := probeTLSClients(ctx, newImageTLSClients(invocation, environ, workDir))
	for _, observation := range observations {
		fmt.Fprintf(stdout, "probe-image: TLS client %s %s: %s (%s)\n",
			observation.client, observation.site.name, observation.status, observation.detail)
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	modelFallback, err := capabilities.ReadModelFallback(ctx, img)
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: model-fallback not read; the deployment's capability report shows the row open: %v\n", err)
		modelFallback = bootprobe.ModelFallbackUnread
	}
	// Each role's own overlays, read as that role's Oh My Pi will start on them, after the table so
	// the log still reads as the declared list first.
	roleImage := capabilities.Image{Launch: invocation, Env: environ, WorkDir: workDir}
	if code := checkRoleOverlays(roleOverlays, func(files []string) (string, error) {
		overlays := append(capabilities.OverlayEntries(envValue(environ, "PI_CONFIG_FILES")), files...)
		return capabilities.CheckOverlays(ctx, roleImage, overlays, files)
	}, stderr); code != 0 {
		return code
	}
	agentModels := bootprobe.AgentModelsResolved
	if *skipAgentModels {
		agentModels = bootprobe.AgentModelsSkipped
	}
	checked := bootprobe.CapabilitiesAll
	if *skipBrowser {
		checked = bootprobe.CapabilitiesBrowserSkipped
	}
	if natsUser != "" {
		fmt.Fprintln(stdout, bootprobe.NATSUserLine(natsUser))
	}
	fmt.Fprintln(stdout, bootprobe.OKLine(invocation, expected, agentModels, checked, modelFallback))
	return 0
}

// roleOverlays is one --role-config-files value: a role and its own overlays, in order.
type roleOverlays struct {
	role  string
	files []string
}

// roleConfigFilesFlag is every --role-config-files value, in the order given; a role given twice,
// a name that is no Legion role, and a file that is not an absolute path are refused as the flag
// is read.
type roleConfigFilesFlag []roleOverlays

func (f *roleConfigFilesFlag) String() string {
	values := make([]string, len(*f))
	for i, entry := range *f {
		values[i] = bootprobe.RoleConfigFilesValue(entry.role, entry.files)
	}
	return strings.Join(values, " ")
}

func (f *roleConfigFilesFlag) Set(value string) error {
	role, files, err := bootprobe.ParseRoleConfigFiles(value)
	if err != nil {
		return err
	}
	known := false
	for _, legionRole := range config.RoleConfigRoles {
		known = known || string(legionRole) == role
	}
	if !known {
		return fmt.Errorf("%q is not a Legion role", role)
	}
	if slices.ContainsFunc(*f, func(entry roleOverlays) bool { return entry.role == role }) {
		return fmt.Errorf("%s is given twice", role)
	}
	for _, file := range files {
		if !filepath.IsAbs(file) {
			return fmt.Errorf("%s's overlay %q is not an absolute path", role, file)
		}
	}
	*f = append(*f, roleOverlays{role: role, files: files})
	return nil
}

// envValue is the value environ gives name, "" when it gives none.
func envValue(environ []string, name string) string {
	value := ""
	for _, pair := range environ {
		if found, v, _ := strings.Cut(pair, "="); found == name {
			value = v
		}
	}
	return value
}

// checkRoleOverlays runs check on each role's own overlays in order and answers the exit code: 1,
// after printing the role's RoleOverlaysRefusedLine, for the first role check refuses, else 0. A
// run check reports as unanswered (a timeout) is printed and does not stop the role's own verdict:
// a refusal found after it still refuses.
func checkRoleOverlays(entries []roleOverlays, check func(files []string) (string, error), stderr io.Writer) int {
	for _, entry := range entries {
		why, err := check(entry.files)
		if err != nil {
			fmt.Fprintf(stderr, "legion probe-image: role %s's overlays were read but Oh My Pi did not answer on some of them: %v\n", entry.role, err)
		}
		if why != "" {
			fmt.Fprintln(stderr, bootprobe.RoleOverlaysRefusedLine(entry.role, why))
			return 1
		}
	}
	return 0
}
