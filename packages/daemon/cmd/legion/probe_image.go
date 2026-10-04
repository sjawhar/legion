package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/natsauth"
	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
	"github.com/sjawhar/legion/daemon/internal/prompts"
	"github.com/sjawhar/legion/daemon/internal/shim"
)

// digits is what --daemon-api-version accepts before it is read as a number.
var digits = regexp.MustCompile(`^[0-9]+$`)

// runProbeImage is `legion probe-image`, run inside the worker image: by its build's final step
// (packages/daemon/docker/worker.Dockerfile), and with the daemon's own contract by the daemon's
// probe Sandbox, which reads the OK line back from the pod's log (internal/runtime/sandbox,
// ProbeImage). Both pass --plugin-root, the plugin directory a pod loads as its one explicit
// extension, and the command refuses a run without it (exit 2): the probe certifies the lane a pod
// uses, so a build line that lost the flag fails the build instead of probing a lane no pod loads.
// It runs the image's launch probes under the image's own environment (daemon.ProbeImage) and,
// when every one passes, prints bootprobe.OKLine; a failure is the probe's message, exit 1, so a
// broken image never publishes. Unlike the TypeScript command, the contract is always checked:
// with none named, against this binary's own DaemonAPIVersion, which the plugin packed from the
// same commit must declare. The load probe also holds every task agent the
// prompts dispatch to its own model; the OK line says so (agent-models=resolved), or that the
// build's probe, which has no operator model configuration, skipped it (--skip-agent-models). The
// probe Sandbox runs it as a worker runs: with --pod-safety, on the pod's baseline as a worker's
// shim starts Oh My Pi (podsafety.Apply), its overlay written to a fresh temporary directory since
// the probe pod mounts no state volume; with --provider-env-dir, each provider key exported after
// it as the shim exports them (shim.ReadProviderEnv); and with --role-references, the references
// of the role prompts the daemon inlines into its pods (promptrefs.Encode's encoding, decoded as
// the flags are read). Without it, the probe resolves the references of the role prompts this
// binary embeds (prompts.RoleReferences). When its environment names a NATS nkey seed (a probe
// pod's NATS_NKEY_SEED_FILE, the providers Secret's key), that seed is read as the daemon reads its
// own (natsauth.Seed) and must be a user's, and the line before the OK line names that user's
// public key (bootprobe.NATSUserLine), never the seed, for the daemon to compare with its own.
func runProbeImage(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("probe-image", "usage: legion probe-image --plugin-root <dir> [flags]", stderr)
	omp := flags.String("omp", "", "the OMP executable to probe (default: $LEGION_OMP_PATH)")
	contract := flags.String("daemon-api-version", strconv.Itoa(api.DaemonAPIVersion),
		"the daemon API contract the image's pi-legion-envoy must declare (the daemon's probe Sandbox passes its own)")
	pluginRoot := flags.String("plugin-root", "", "the plugin directory a pod loads as its one explicit extension, which the load probe loads the same way (required)")
	podSafety := flags.Bool("pod-safety", false, "run the probes on a pod's baseline (internal/podsafety), as a pod's shim starts Oh My Pi")
	providerEnvDir := flags.String("provider-env-dir", "", "a directory whose files NAME=contents the probes' Oh My Pi gets, as a worker's shim exports them")
	skipAgentModels := flags.Bool("skip-agent-models", false, "leave the prompt-named task agents' models unresolved (the image build's probe)")
	roleReferences := flags.String("role-references", "", "the task agents and skills the daemon's own role prompts name, resolved in place of the role prompts this binary embeds")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "legion probe-image: unexpected argument %q\n", flags.Arg(0))
		return 2
	}
	if *pluginRoot == "" {
		fmt.Fprintln(stderr, "legion probe-image: --plugin-root is required: the plugin directory a pod loads as its one explicit extension, which the load probe loads the same way")
		return 2
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
		state, err := os.MkdirTemp("", "legion-probe-image-")
		if err == nil {
			defer os.RemoveAll(state)
			environ, err = podsafety.Apply(environ, state)
		}
		if err != nil {
			fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
			return 1
		}
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
		Omp: invocation, Contract: expected, Env: env, WorkDir: workDir, PluginRoot: *pluginRoot,
		RoleReferences: references, SkipAgentModels: *skipAgentModels,
		Log: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	agentModels := bootprobe.AgentModelsResolved
	if *skipAgentModels {
		agentModels = bootprobe.AgentModelsSkipped
	}
	if natsUser != "" {
		fmt.Fprintln(stdout, bootprobe.NATSUserLine(natsUser))
	}
	fmt.Fprintln(stdout, bootprobe.OKLine(invocation, expected, agentModels))
	return 0
}
