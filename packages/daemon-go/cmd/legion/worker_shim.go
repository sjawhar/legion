package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/sjawhar/legion/daemon/internal/modelroute"
	"github.com/sjawhar/legion/daemon/internal/shim"
)

const workerShimUsage = "legion worker-shim --connect <unix:///path|tcp://host:port> --boot-token-file <path> [--provider-env-dir <dir>] -- <omp argv…>"

// runWorkerShim is `legion worker-shim`, the command a runtime puts in front of every phase
// worker's OMP: it dials the daemon's worker stream, and bridges OMP to it once acked. Its lines
// go to stdout, which is what the pane shows; its exit status is OMP's. In a pod
// (LEGION_MODEL_GATEWAY_URL set) it first writes the model route into OMP's profile
// (modelroute.Install), so the agent reaches its models through the gateway alone.
func runWorkerShim(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("worker-shim", stderr)
	connect := flags.String("connect", "", "the daemon's worker stream: unix:///<path> or tcp://<host>:<port>")
	bootTokenFile := flags.String("boot-token-file", "", "the file holding the pane's boot token")
	providerEnvDir := flags.String("provider-env-dir", "", "a directory whose files become NAME=contents in OMP's environment only")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	set := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { set[f.Name] = true })

	cfg, err := workerShimConfig(set, *connect, *bootTokenFile, *providerEnvDir, flags.Args())
	if err == nil {
		_, err = modelroute.Install(os.LookupEnv)
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

// workerShimConfig is every refusal the command makes, each before anything is dialled or
// spawned and each naming the flag or path at fault (worker-shim.ts:630-676). `set` is which
// flags were given: a flag given an empty value is refused by its reader, not taken as absent.
func workerShimConfig(set map[string]bool, connect, bootTokenFile, providerEnvDir string, argv []string) (shim.Config, error) {
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
	return shim.Config{
		Network:     network,
		Address:     address,
		BootToken:   token,
		Argv:        argv,
		Env:         os.Environ(),
		ProviderEnv: providerEnv,
		Grace:       shim.DefaultGrace,
	}, nil
}
