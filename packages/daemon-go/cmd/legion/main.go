package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/daemon"
	"github.com/sjawhar/legion/daemon/internal/registry"
)

const (
	defaultConfigPath = "./legion.yaml"
	// stopTimeout bounds the wait for a stopping daemon, whose own exit is two bounded steps —
	// draining the API and stamping the boot.
	stopTimeout = 30 * time.Second
	// requestTimeout bounds a CLI read of a daemon's HTTP API.
	requestTimeout = 5 * time.Second
	// aliveInterval is how often a wait re-asks whether a process is gone.
	aliveInterval = 20 * time.Millisecond
)

// revision is the commit the binary was built from, linked in by the worker image's build
// (`-ldflags "-X main.revision=<commit>"`, packages/daemon/docker/worker.Dockerfile); empty otherwise.
var revision string

type command func(ctx context.Context, args []string, stdout, stderr io.Writer) int

var commands = map[string]command{
	"version":        runVersion,
	"start":          runStart,
	"stop":           runStop,
	"state":          runState,
	"legions":        runLegions,
	"status":         runStatus,
	"restart":        runRestart,
	"worker-shim":    runWorkerShim,
	"claims":         runClaims,
	"gh":             runGh,
	"credential":     runCredential,
	"handoff":        runHandoff,
	"threads":        runThreads,
	"probe-image":    runProbeImage,
	"workspace-init": runWorkspaceInit,
}

func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) < 2 {
		fmt.Fprintln(stderr, "usage: legion <command> [flags]")
		return 2
	}
	cmd, ok := commands[argv[1]]
	if !ok {
		fmt.Fprintf(stderr, "legion: unknown command %q\n", argv[1])
		return 2
	}
	return cmd(ctx, argv[2:], stdout, stderr)
}

func runVersion(_ context.Context, _ []string, stdout, _ io.Writer) int {
	version := "(devel)"
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	if revision != "" {
		fmt.Fprintf(stdout, "legion %s commit %s\n", version, revision)
		return 0
	}
	fmt.Fprintf(stdout, "legion %s\n", version)
	return 0
}

func runStart(ctx context.Context, args []string, _, stderr io.Writer) int {
	flags := newFlags("start", stderr)
	configPath := flags.String("config", defaultConfigPath, "path to legion.yaml")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	return start(ctx, *configPath, stderr)
}

// start runs a legion in this process until its context is done. The registry entry is this
// process's one claim on the team — there is no second record to disagree with it — so `legion
// stop`, `legion status` and `legion legions` read one answer and not the daemon's own opinion
// of itself.
func start(ctx context.Context, configPath string, stderr io.Writer) int {
	log := slog.New(slog.NewJSONHandler(stderr, nil))
	slog.SetDefault(log)

	cfg, err := config.Load(configPath, nil)
	if err != nil {
		fmt.Fprintf(stderr, "legion start: %v\n", err)
		return 1
	}
	absoluteConfig, err := filepath.Abs(configPath)
	if err != nil {
		fmt.Fprintf(stderr, "legion start: resolve %s: %v\n", configPath, err)
		return 1
	}
	// The state directory is where the worker stream socket, the panes' secret files, and their
	// home live; the registry refuses a second live legion on it, which only holds if every
	// start names it the same way.
	if cfg.StateDir, err = filepath.Abs(cfg.StateDir); err != nil {
		fmt.Fprintf(stderr, "legion start: resolve state_dir: %v\n", err)
		return 1
	}
	legions, err := registryPath()
	if err != nil {
		fmt.Fprintf(stderr, "legion start: %v\n", err)
		return 1
	}

	// Taking the team is one step, not a read and then a write: two starts released together
	// would both pass a separate check, and the loser's release would delete the winner's entry,
	// leaving that daemon serving with nothing to stop or read it by.
	held, claimed, err := registry.Claim(legions, registry.Entry{
		Team:       cfg.Project,
		ConfigPath: absoluteConfig,
		PID:        os.Getpid(),
		Port:       cfg.Port,
		Bind:       cfg.Bind,
		StateDir:   cfg.StateDir,
		StartedAt:  time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "legion start: %v\n", err)
		return 1
	}
	if !claimed {
		fmt.Fprintf(stderr, "legion start: %s is already running (pid %d)\n", cfg.Project, held.PID)
		return 1
	}
	// Only this process's own entry: a daemon that replaced it owns the team now.
	defer func() {
		if err := registry.RemoveIf(legions, cfg.Project, os.Getpid()); err != nil {
			fmt.Fprintf(stderr, "legion start: %v\n", err)
		}
	}()

	if err := daemon.Run(ctx, cfg, log); err != nil {
		fmt.Fprintf(stderr, "legion start: %v\n", err)
		return 1
	}
	return 0
}

func runStop(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("stop", stderr)
	configPath := flags.String("config", defaultConfigPath, "path to legion.yaml")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*configPath, nil)
	if err != nil {
		fmt.Fprintf(stderr, "legion stop: %v\n", err)
		return 1
	}
	entry, found, err := findLegion(cfg.Project)
	if err != nil {
		fmt.Fprintf(stderr, "legion stop: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "legion stop: no legion is registered for %s\n", cfg.Project)
		return 1
	}

	// A daemon that died without cleaning up leaves an entry naming a pid nothing holds: the
	// stop is that record's repair, not an error.
	if !registry.Alive(entry.PID) {
		legions, err := registryPath()
		if err != nil {
			fmt.Fprintf(stderr, "legion stop: %v\n", err)
			return 1
		}
		if err := registry.RemoveIf(legions, cfg.Project, entry.PID); err != nil {
			fmt.Fprintf(stderr, "legion stop: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "legion %s: not running (removed the entry of pid %d, which is gone)\n",
			cfg.Project, entry.PID)
		return 0
	}
	if err := stopProcess(entry.PID); err != nil {
		fmt.Fprintf(stderr, "legion stop: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "legion %s: stopped (pid %d)\n", cfg.Project, entry.PID)
	return 0
}

func runState(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("state", stderr)
	asJSON := flags.Bool("json", false, "print the state as the daemon serves it")
	configPath := flags.String("config", "", "path to legion.yaml (default "+defaultConfigPath+")")
	port := flags.Int("port", 0, "port to read, overriding the configured one")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	// A pane has no legion.yaml; it has the daemon's URL, which the daemon names on every pane.
	address := daemonURL()
	if _, pane := os.LookupEnv("LEGION_DAEMON_URL"); !pane || *configPath != "" || *port != 0 {
		configured, err := stateAddress(*configPath, *port)
		if err != nil {
			fmt.Fprintf(stderr, "legion state: %v\n", err)
			return 1
		}
		address = "http://" + configured
	}
	body, err := get(ctx, address+"/legion/v1/state")
	if err != nil {
		fmt.Fprintf(stderr, "legion state: %v\n", err)
		return 1
	}

	if *asJSON {
		if _, err := stdout.Write(body); err != nil {
			fmt.Fprintf(stderr, "legion state: %v\n", err)
			return 1
		}
		return 0
	}

	var state api.State
	if err := json.Unmarshal(body, &state); err != nil {
		fmt.Fprintf(stderr, "legion state: read the state %s served: %v\n", address, err)
		return 1
	}
	fmt.Fprintf(stdout, "legion %s on %s — schema %d, boot %d started %s (first boot %s)\n",
		state.Daemon.Project, address, state.Daemon.SchemaVersion, state.Daemon.Boots,
		state.Daemon.StartedAt.Format(time.RFC3339), state.Daemon.FirstBootAt.Format(time.RFC3339))
	fmt.Fprintf(stdout, "admission: %d active, %d waiting, cap %d; %d issues\n",
		len(state.Admission.Active), len(state.Admission.Waiting), state.Admission.Cap, len(state.Issues))
	// In a pane, the issue record is what a relaunched worker re-reads.
	if key := os.Getenv("LEGION_ISSUE"); key != "" {
		issue, ok := state.Issues[key]
		if !ok {
			fmt.Fprintf(stdout, "issue %s is not recorded\n", key)
			return 0
		}
		record, err := json.MarshalIndent(issue, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "legion state: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "issue %s:\n%s\n", key, record)
	}
	return 0
}

// stateAddress is where a daemon answers: the configured bind and port, with --port overriding
// the port. --port on its own reads a daemon on this box without a configuration to load, which
// is how a proof script reads a legion whose file it did not write. A wildcard bind is dialled
// the same way `legion status` dials it — one rule for both commands that read a configured
// bind.
func stateAddress(configPath string, port int) (string, error) {
	if configPath == "" && port != 0 {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}
	if configPath == "" {
		configPath = defaultConfigPath
	}
	cfg, err := config.Load(configPath, nil)
	if err != nil {
		return "", err
	}
	if port == 0 {
		port = cfg.Port
	}
	return net.JoinHostPort(dialHost(cfg.Bind), strconv.Itoa(port)), nil
}

func runLegions(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("legions", stderr)
	asJSON := flags.Bool("json", false, "print the registry as JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	legions, err := registryPath()
	if err != nil {
		fmt.Fprintf(stderr, "legion legions: %v\n", err)
		return 1
	}
	entries, err := registry.Read(legions)
	if err != nil {
		fmt.Fprintf(stderr, "legion legions: %v\n", err)
		return 1
	}

	if *asJSON {
		if entries == nil {
			entries = []registry.Entry{}
		}
		encoded, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "legion legions: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "%s\n", encoded)
		return 0
	}
	for _, entry := range entries {
		fmt.Fprintf(stdout, "%s %d %d %s\n",
			entry.Team, entry.PID, entry.Port, entry.StartedAt.Format(time.RFC3339))
	}
	return 0
}

func runStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 2 && !strings.HasPrefix(args[0], "-") && !strings.HasPrefix(args[1], "-") {
		return runIssueStatus(ctx, args[0], args[1], stdout, stderr)
	}
	flags := newFlags("status", stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: legion status <team>")
		return 2
	}
	team := flags.Arg(0)

	entry, found, err := findLegion(team)
	if err != nil {
		fmt.Fprintf(stderr, "legion status: %v\n", err)
		return 1
	}
	// The registry says which process; the process says whether it is still there; /healthz says
	// whether it is still serving. A legion is all three.
	if !found || !registry.Alive(entry.PID) {
		fmt.Fprintf(stdout, "legion %s: not running\n", team)
		return 0
	}
	address := net.JoinHostPort(dialHost(entry.Bind), strconv.Itoa(entry.Port))
	if _, err := get(ctx, "http://"+address+"/healthz"); err != nil {
		fmt.Fprintf(stderr, "legion %s: pid %d is alive but %s did not answer: %v\n",
			team, entry.PID, address, err)
		return 1
	}
	fmt.Fprintf(stdout, "legion %s: running — pid %d, port %d, healthy, started %s\n",
		team, entry.PID, entry.Port, entry.StartedAt.Format(time.RFC3339))
	return 0
}

// dialHost is where a CLI reaches a legion that recorded its bind: a daemon bound to every
// interface answers on loopback, and `bind` is a shipped key the deployment overlays set, so the
// address is the one the daemon recorded rather than loopback by assumption.
func dialHost(bind string) string {
	switch bind {
	case "", "0.0.0.0", "::", "[::]":
		return "127.0.0.1"
	}
	return bind
}

func runRestart(ctx context.Context, args []string, _, stderr io.Writer) int {
	flags := newFlags("restart", stderr)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: legion restart <team>")
		return 2
	}
	team := flags.Arg(0)

	entry, found, err := findLegion(team)
	if err != nil {
		fmt.Fprintf(stderr, "legion restart: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintf(stderr, "legion restart: no legion is registered for %s\n", team)
		return 1
	}
	if registry.Alive(entry.PID) {
		if err := stopProcess(entry.PID); err != nil {
			fmt.Fprintf(stderr, "legion restart: %v\n", err)
			return 1
		}
	}
	// The restarted legion is this process, started from the configuration the stopped one
	// recorded: a restart changes the process, never the deployment.
	return start(ctx, entry.ConfigPath, stderr)
}

// stopProcess asks a daemon to stop and waits until it is gone, so what follows a stop — a
// status, a start of the same team — sees the box as the stop left it.
func stopProcess(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(stopTimeout)
	for registry.Alive(pid) {
		if time.Now().After(deadline) {
			return fmt.Errorf("pid %d is still running %s after SIGTERM", pid, stopTimeout)
		}
		time.Sleep(aliveInterval)
	}
	return nil
}

func registryPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory the legions registry lives under: %w", err)
	}
	return registry.Path(nil, home), nil
}

func findLegion(team string) (registry.Entry, bool, error) {
	legions, err := registryPath()
	if err != nil {
		return registry.Entry{}, false, err
	}
	return registry.Find(legions, team)
}

func get(ctx context.Context, url string) ([]byte, error) {
	timed, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(timed, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s answered %d: %s", url, response.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet("legion "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	return flags
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args, os.Stdout, os.Stderr))
}
