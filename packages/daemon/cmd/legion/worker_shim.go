package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/shim"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

const workerShimUsage = "legion worker-shim --connect <unix:///path|tcp://host:port> --boot-token-file <path> [--provider-env-dir <dir>] [--pod-safety] [--stop-grace <duration>] [--warm-codegraph] [--agent-secrets-key-dir <dir> --pod-token-file <path> --agent-secrets-bin <path>] -- <omp argv…>"

// runWorkerShim is `legion worker-shim`, the command a runtime puts in front of every phase
// worker's OMP: it dials the daemon's worker stream, and bridges OMP to it once acked. Its lines
// go to stdout, which is what the pane shows; its exit status is OMP's. With --pod-safety, which
// the Sandbox runtime passes and a tmux pane never does, OMP starts on the pod's baseline
// (podsafety.Apply: the turn-scoping overlay written to LEGION_STATE_DIR and named first in
// PI_CONFIG_FILES, under the operator's, and the two variables that place a pod's sessions where
// the pod leaves them unset; nothing of a repository's settings held off). With --warm-codegraph,
// which the Sandbox runtime passes for a role in an issue pod — never for a tmux pane, whose
// workspace the daemon warms itself (internal/daemon/outbox.go), nor for the controller, which has
// no workspace — the shim builds the CodeGraph index of the workspace LEGION_WORKSPACE names in the
// background once Oh My Pi has started (shim.Config.WarmCodegraph), and a stop before the build
// ends — the launcher's SIGTERM to the role's process group, which ends Oh My Pi and the codegraph
// child together — ends the warm-up with Oh My Pi: the shim exits only once the build's lease is
// released, so the role's relaunch is never told a live build holds it. On a relaunch the warm-up
// runs again: `codegraph status` on the volume's existing index answers complete and nothing runs,
// or an index an earlier build left partial is repaired as workspace.nextCodegraphStep decides.
func runWorkerShim(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("worker-shim", "usage: legion worker-shim [flags] -- <omp argv…>", stderr)
	var given workerShimFlags
	flags.StringVar(&given.connect, "connect", "", "the daemon's worker stream: unix:///<path> or tcp://<host>:<port>")
	flags.StringVar(&given.bootTokenFile, "boot-token-file", "", "the file holding the pane's boot token")
	flags.StringVar(&given.providerEnvDir, "provider-env-dir", "", "a directory whose files become NAME=contents in OMP's environment only")
	podSafety := flags.Bool("pod-safety", false, "start OMP on a pod's baseline (internal/podsafety), writing its overlay to LEGION_STATE_DIR")
	flags.DurationVar(&given.stopGrace, "stop-grace", 0, "how long the runtime gives this shim to exit after its stop signal before killing it (a pod's terminationGracePeriodSeconds, which the launcher passes); the wait for an in-flight CodeGraph warm-up stays inside it; unset is the daemon's default worker_stop_timeout_seconds")
	flags.BoolVar(&given.warmCodegraph, "warm-codegraph", false, "once Oh My Pi has started, build the CodeGraph index of the workspace LEGION_WORKSPACE names, in the background (a role's shim in an issue pod; never a tmux pane's or the controller's)")
	flags.StringVar(&given.keyDir, "agent-secrets-key-dir", "", "the pod's tmpfs directory for its agent-secrets key and enrollment id")
	flags.StringVar(&given.tokenFile, "pod-token-file", "", "the projected service-account token for the secrets broker's audience")
	flags.StringVar(&given.agentSecretsBin, "agent-secrets-bin", "", "the agent-secrets binary that generates the key and renews the lease")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, err := workerShimConfig(set, given, flags.Args())
	if err == nil && *podSafety {
		cfg.Env, err = podSafeEnvironment(cfg.Env)
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion worker-shim: %v\n", err)
		return 1
	}
	cfg.Log = stdout
	code, err := shim.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "legion worker-shim: %v\n", err)
		return 1
	}
	return code
}

// podSafeEnvironment is environ on the pod's baseline: the turn-scoping overlay written to the
// pod's state directory, which the runtime names as LEGION_STATE_DIR, and named first in
// PI_CONFIG_FILES, and PI_CONFIG_DIR and OMP_SESSION_STORAGE set where environ leaves them unset.
func podSafeEnvironment(environ []string) ([]string, error) {
	state := os.Getenv("LEGION_STATE_DIR")
	if state == "" {
		return nil, errors.New("--pod-safety needs LEGION_STATE_DIR, the pod's state directory, to write the baseline overlay to")
	}
	return podsafety.Apply(environ, state)
}

// workerShimFlags are the command's flag values, as parsed, apart from --pod-safety, which
// runWorkerShim applies to the environment once the configuration holds.
type workerShimFlags struct {
	connect, bootTokenFile, providerEnvDir, keyDir, tokenFile, agentSecretsBin string
	warmCodegraph                                                              bool
	stopGrace                                                                  time.Duration
}

// workerShimConfig is every refusal the command makes, each before anything is dialled or
// spawned and each naming the flag or path at fault (worker-shim.ts:630-676). `set` is which
// flags were given: a flag given an empty value is refused by its reader, not taken as absent.
// --warm-codegraph is refused without LEGION_WORKSPACE in the environment: a shim told to warm a
// workspace it was not told of has nothing to build, and the runtime that passes the flag
// (internal/runtime/sandbox's launcherCommand) is the one that sets the variable. --stop-grace is
// refused when it is not positive: a grace the shim cannot fit anything inside is a runtime's
// mistake, not a shim that should wait for nothing.
func workerShimConfig(set map[string]bool, given workerShimFlags, argv []string) (shim.Config, error) {
	if !set["connect"] {
		return shim.Config{}, fmt.Errorf("--connect is required: %s", workerShimUsage)
	}
	if !set["boot-token-file"] {
		return shim.Config{}, errors.New("--connect requires --boot-token-file <path>")
	}
	if given.warmCodegraph && os.Getenv("LEGION_WORKSPACE") == "" {
		return shim.Config{}, errors.New("--warm-codegraph needs LEGION_WORKSPACE, the workspace whose CodeGraph index it builds")
	}
	if set["stop-grace"] && given.stopGrace <= 0 {
		return shim.Config{}, fmt.Errorf("--stop-grace %s is not a positive duration", given.stopGrace)
	}
	network, address, err := shim.ParseAddress(given.connect)
	if err != nil {
		return shim.Config{}, err
	}
	if len(argv) == 0 {
		return shim.Config{}, fmt.Errorf("no wrapped command: %s", workerShimUsage)
	}
	token, err := shim.ReadBootToken(given.bootTokenFile)
	if err != nil {
		return shim.Config{}, err
	}
	var providerEnv []string
	if set["provider-env-dir"] {
		if providerEnv, err = shim.ReadProviderEnv(given.providerEnvDir, os.LookupEnv); err != nil {
			return shim.Config{}, err
		}
	}
	agentSecrets, err := shim.ReadAgentSecretsFlags(set, given.keyDir, given.tokenFile, given.agentSecretsBin)
	if err != nil {
		return shim.Config{}, err
	}
	cfg := shim.Config{
		Network:      network,
		Address:      address,
		BootToken:    token,
		Argv:         argv,
		Env:          os.Environ(),
		ProviderEnv:  providerEnv,
		AgentSecrets: agentSecrets,
		Grace:        shim.DefaultGrace,
		StopGrace:    given.stopGrace,
	}
	if given.warmCodegraph {
		cfg.WarmCodegraph = workspace.WarmCodegraphIndex
	}
	return cfg, nil
}
