package main

import (
	"context"
	"errors"
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
	"github.com/sjawhar/legion/daemon/internal/modelroute"
)

// digits is what --go-daemon-api-version accepts before it is read as a number.
var digits = regexp.MustCompile(`^[0-9]+$`)

// runProbeImage is `legion probe-image` (packages/daemon/src/cli/index.ts:946-976, cmdProbeImage
// :355-397), run inside the worker image: bare by its build's final step
// (packages/daemon/docker/worker.Dockerfile), and with the daemon's own contract by the daemon's
// probe Sandbox, which reads the OK line back from the pod's log (internal/runtime/sandbox,
// ProbeImage). It runs the image's launch probes under the image's own environment
// (daemon.ProbeImage) and, when every one passes, prints bootprobe.OKLine; a failure is the
// probe's message, exit 1, so a broken image never publishes. Unlike the TypeScript command, the
// contract is always checked: bare, against this binary's own GoDaemonAPIVersion, which the
// plugin packed from the same commit must declare. In the probe Sandbox, which the daemon routes
// through the model gateway as it does every worker (LEGION_MODEL_GATEWAY_URL), it first writes
// the route into the image's profile, as the worker shim does (modelroute.Install), then makes one
// model round trip through it, and names the model that answered on the OK line, exiting
// bootprobe.TransientExit when the gateway could not answer; the build has no gateway, makes no
// round trip, and prints no model.
func runProbeImage(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("probe-image", stderr)
	omp := flags.String("omp", "", "the OMP executable to probe (default: $LEGION_OMP_PATH)")
	contract := flags.String("go-daemon-api-version", strconv.Itoa(api.GoDaemonAPIVersion),
		"the Go daemon API contract the image's pi-legion-envoy must declare (the daemon's probe Sandbox passes its own)")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "legion probe-image: unexpected argument %q\n", flags.Arg(0))
		return 2
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
		fmt.Fprintf(stderr, "legion probe-image: --go-daemon-api-version must be a positive integer (got %q)\n", *contract)
		return 1
	}
	workDir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	installed, err := modelroute.Install(os.LookupEnv)
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	env := map[string]string{}
	for _, pair := range installed.Environ(os.Environ()) {
		if name, value, ok := strings.Cut(pair, "="); ok {
			env[name] = value
		}
	}
	route, model := installed.Route, ""
	if route != "" {
		model = modelroute.DefaultModel
	}
	err = daemon.ProbeImage(ctx, daemon.ImageProbe{
		Omp: invocation, Contract: expected, Env: env, WorkDir: workDir, Model: model, Route: route,
		Log: slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if unavailable := (*daemon.ModelRouteUnavailable)(nil); errors.As(err, &unavailable) {
		fmt.Fprintf(stderr, "legion probe-image: %v (transient: the daemon's probe runs again)\n", err)
		return bootprobe.TransientExit
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion probe-image: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, bootprobe.OKLine(invocation, model, expected))
	return 0
}
