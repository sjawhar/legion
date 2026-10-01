package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sjawhar/legion/daemon/internal/podsafety"
	"github.com/sjawhar/legion/daemon/internal/shim"
)

const workerShimUsage = "legion worker-shim --connect <unix:///path|tcp://host:port> --boot-token-file <path> [--provider-env-dir <dir>] [--pod-safety] [--agent-secrets-key-dir <dir> --pod-token-file <path> --agent-secrets-bin <path>] -- <omp argv…>"

// runWorkerShim is `legion worker-shim`, the command a runtime puts in front of every phase
// worker's OMP: it dials the daemon's worker stream, and bridges OMP to it once acked. Its lines
// go to stdout, which is what the pane shows; its exit status is OMP's. With --pod-safety, which
// the Sandbox runtime passes and a tmux pane never does, OMP starts on the pod's baseline
// (podsafety.Apply, its overlay written to LEGION_STATE_DIR), and once OMP has actually started
// the shim begins the CodeGraph warm-up in the background for the issue's workspace
// (shim.Config.WarmCodegraph, internal/shim's spawnOnce): the pod init container builds no
// index (#1647), and a tmux pane never sets this, since host provisioning already warms that
// workspace (internal/daemon/outbox.go).
func runWorkerShim(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("worker-shim", stderr)
	connect := flags.String("connect", "", "the daemon's worker stream: unix:///<path> or tcp://<host>:<port>")
	bootTokenFile := flags.String("boot-token-file", "", "the file holding the pane's boot token")
	providerEnvDir := flags.String("provider-env-dir", "", "a directory whose files become NAME=contents in OMP's environment only")
	podSafety := flags.Bool("pod-safety", false, "start OMP on a pod's baseline (internal/podsafety), writing its overlay to LEGION_STATE_DIR")
	keyDir := flags.String("agent-secrets-key-dir", "", "the pod's tmpfs directory for its agent-secrets key and enrollment id")
	tokenFile := flags.String("pod-token-file", "", "the projected service-account token for the secrets broker's audience")
	agentSecretsBin := flags.String("agent-secrets-bin", "", "the agent-secrets binary that generates the key and renews the lease")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, err := workerShimConfig(set, *connect, *bootTokenFile, *providerEnvDir, *keyDir, *tokenFile, *agentSecretsBin, flags.Args())
	if err == nil && *podSafety {
		cfg.WarmCodegraph = true
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

// podSafeEnvironment is environ on the pod's baseline, its overlay written to the pod's state
// directory, which the runtime names as LEGION_STATE_DIR.
func podSafeEnvironment(environ []string) ([]string, error) {
	state := os.Getenv("LEGION_STATE_DIR")
	if state == "" {
		return nil, errors.New("--pod-safety needs LEGION_STATE_DIR, the pod's state directory, to write the baseline overlay to")
	}
	return podsafety.Apply(environ, state)
}

// workerShimConfig is every refusal the command makes, each before anything is dialled or
// spawned and each naming the flag or path at fault (worker-shim.ts:630-676). `set` is which
// flags were given: a flag given an empty value is refused by its reader, not taken as absent.
func workerShimConfig(set map[string]bool, connect, bootTokenFile, providerEnvDir, keyDir, tokenFile, agentSecretsBin string, argv []string) (shim.Config, error) {
	if !set["connect"] {
		return shim.Config{}, fmt.Errorf("--connect is required: %s", workerShimUsage)
	}
	if !set["boot-token-file"] {
		return shim.Config{}, errors.New("--connect requires --boot-token-file <path>")
	}
	network, address, err := shim.ParseAddress(connect)
	if err != nil {
		return shim.Config{}, err
	}
	if len(argv) == 0 {
		return shim.Config{}, fmt.Errorf("no wrapped command: %s", workerShimUsage)
	}
	token, err := shim.ReadBootToken(bootTokenFile)
	if err != nil {
		return shim.Config{}, err
	}
	var providerEnv []string
	if set["provider-env-dir"] {
		if providerEnv, err = shim.ReadProviderEnv(providerEnvDir, os.LookupEnv); err != nil {
			return shim.Config{}, err
		}
	}
	agentSecrets, err := shim.ReadAgentSecretsFlags(set, keyDir, tokenFile, agentSecretsBin)
	if err != nil {
		return shim.Config{}, err
	}
	return shim.Config{
		Network:      network,
		Address:      address,
		BootToken:    token,
		Argv:         argv,
		Env:          os.Environ(),
		ProviderEnv:  providerEnv,
		AgentSecrets: agentSecrets,
		Grace:        shim.DefaultGrace,
	}, nil
}
