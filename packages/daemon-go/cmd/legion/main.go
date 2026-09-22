package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
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
	// pidFileName sits under the configured state_dir: `legion stop` reads it to find the daemon
	// of the configuration it was handed.
	pidFileName = "legion.pid"
	// stopTimeout bounds the wait for a stopping daemon, whose own exit is two bounded steps —
	// draining the API and stamping the boot.
	stopTimeout = 30 * time.Second
	// requestTimeout bounds a CLI read of a daemon's HTTP API.
	requestTimeout = 5 * time.Second
	// aliveInterval is how often a wait re-asks whether a process is gone.
	aliveInterval = 20 * time.Millisecond
)

type command func(ctx context.Context, args []string, stdout, stderr io.Writer) int

var commands = map[string]command{
	"version": runVersion,
	"start":   runStart,
	"stop":    runStop,
	"state":   runState,
	"legions": runLegions,
	"status":  runStatus,
	"restart": runRestart,
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

// start runs a legion in this process until its context is done. The pid file and the registry
// entry are this process's claim on the team: both are written before the daemon serves and
// removed however it stops, so `legion stop`, `legion status` and `legion legions` read one
// answer and not the daemon's own opinion of itself.
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
	legions, err := registryPath()
	if err != nil {
		fmt.Fprintf(stderr, "legion start: %v\n", err)
		return 1
	}

	pidPath, err := writePIDFile(cfg.StateDir)
	if err != nil {
		fmt.Fprintf(stderr, "legion start: %v\n", err)
		return 1
	}
	defer func() {
		if err := os.Remove(pidPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "legion start: remove the pid file %s: %v\n", pidPath, err)
		}
	}()

	err = registry.Put(legions, registry.Entry{
		Team:       cfg.Project,
		ConfigPath: absoluteConfig,
		PID:        os.Getpid(),
		Port:       cfg.Port,
		StartedAt:  time.Now().UTC(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "legion start: %v\n", err)
		return 1
	}
	defer func() {
		if err := registry.Remove(legions, cfg.Project); err != nil {
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
	pidPath := filepath.Join(cfg.StateDir, pidFileName)
	pid, err := readPIDFile(pidPath)
	if err != nil {
		fmt.Fprintf(stderr, "legion stop: %v\n", err)
		return 1
	}

	if !registry.Alive(pid) {
		if err := os.Remove(pidPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "legion stop: remove the pid file %s: %v\n", pidPath, err)
			return 1
		}
		fmt.Fprintf(stdout, "legion %s: not running (removed the stale pid file %s)\n", cfg.Project, pidPath)
		return 0
	}
	if err := stopProcess(pid); err != nil {
		fmt.Fprintf(stderr, "legion stop: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "legion %s: stopped (pid %d)\n", cfg.Project, pid)
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

	address, err := stateAddress(*configPath, *port)
	if err != nil {
		fmt.Fprintf(stderr, "legion state: %v\n", err)
		return 1
	}
	body, err := get(ctx, "http://"+address+"/legion/v1/state")
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
	return 0
}

// stateAddress is where a daemon answers: the configured bind and port, with --port overriding
// the port. --port on its own reads a daemon on this box without a configuration to load, which
// is how a proof script reads a legion whose file it did not write.
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
	return net.JoinHostPort(cfg.Bind, strconv.Itoa(port)), nil
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
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(entry.Port))
	if _, err := get(ctx, "http://"+address+"/healthz"); err != nil {
		fmt.Fprintf(stderr, "legion %s: pid %d is alive but %s did not answer: %v\n",
			team, entry.PID, address, err)
		return 1
	}
	fmt.Fprintf(stdout, "legion %s: running — pid %d, port %d, healthy, started %s\n",
		team, entry.PID, entry.Port, entry.StartedAt.Format(time.RFC3339))
	return 0
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

// writePIDFile is how `legion stop` finds this daemon. 0600, explicitly: a pid file another user
// can rewrite is a SIGTERM another user picks the target of.
func writePIDFile(stateDir string) (string, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("make the state directory %s: %w", stateDir, err)
	}
	path := filepath.Join(stateDir, pidFileName)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("write the pid file %s: %w", path, err)
	}
	defer file.Close()
	// O_CREATE leaves an existing file's mode alone, and a crashed daemon's file is what this
	// one is overwriting.
	if err := file.Chmod(0o600); err != nil {
		return "", fmt.Errorf("write the pid file %s: %w", path, err)
	}
	if _, err := fmt.Fprintf(file, "%d\n", os.Getpid()); err != nil {
		return "", fmt.Errorf("write the pid file %s: %w", path, err)
	}
	return path, nil
}

func readPIDFile(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("no daemon pid file at %s", path)
	}
	if err != nil {
		return 0, fmt.Errorf("read the pid file %s: %w", path, err)
	}
	text := strings.TrimSpace(string(raw))
	pid, err := strconv.Atoi(text)
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("the pid file %s does not hold a pid: %q", path, text)
	}
	return pid, nil
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
