package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
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
// (packages/daemon/docker/worker.Dockerfile) and the legion release (.github/workflows/release.yaml)
// with `-ldflags "-X main.revision=<commit>"`; empty otherwise.
var revision string

// release is the legion-v* version a release binary was built for, linked in by the release
// workflow with `-ldflags "-X main.release=v<version>"`; empty in every other build.
var release string

type command func(ctx context.Context, args []string, stdout, stderr io.Writer) int

// commandEntry is one `legion` command: what runs it, and its line in `legion --help`.
type commandEntry struct {
	run     command
	summary string
}

var commands = map[string]commandEntry{
	"version":        {runVersion, "print the version and the commit the binary was built from"},
	"start":          {runStart, "run the Legion daemon from legion.yaml in the foreground; --check-config validates the file and exits"},
	"stop":           {runStop, "stop the daemon registered for legion.yaml's project"},
	"state":          {runState, "print the daemon's state: admission, issues, pending Dispatch status writes (--json for all of it)"},
	"legions":        {runLegions, "list the daemons registered on this machine"},
	"status":         {runStatus, "say whether a team's daemon is running, or set an issue's Dispatch status"},
	"restart":        {runRestart, "stop a registered daemon and start it again from the configuration it recorded"},
	"worker-shim":    {runWorkerShim, "bridge an agent's Oh My Pi to the daemon's worker stream (the daemon starts it in every pod)"},
	"model-token":    {runModelToken, "sign a pod in to Cognito with its service-account token and print the access token (a model apiKey command)"},
	"claims":         {runClaims, "the operator's hand on the daemon's claims"},
	"gh":             {runGh, "run gh with a GitHub token from this session's grant; merges and GitHub-issue writes are refused"},
	"credential":     {runCredential, "git credential helper answering with a token from this session's grant"},
	"handoff":        {runHandoff, "write or read a phase's .legion/ handoff, or report the phase complete"},
	"threads":        {runThreads, "resolve a pull request's review threads whose opener accepted the reply"},
	"push":           {runPush, "push the issue branch (@-) to legion/<issue>, the one push every phase worker uses"},
	"probe-image":    {runProbeImage, "run the worker image's launch probes (the image build and the daemon's probe Sandbox run it)"},
	"workspace-init": {runWorkspaceInit, "a Sandbox pod's two init containers: fetch the repository, provision the issue's workspace"},
	"controller":     {runController, "start the controller, the interactive Oh My Pi session an operator talks to"},
}

// helpRequested says whether a command's first argument asks for its usage.
func helpRequested(arg string) bool {
	return arg == "-h" || arg == "-help" || arg == "--help"
}

// usage is `legion --help`: every command, alphabetically, with its one line.
func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: legion <command> [flags]")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Commands:")
	for _, name := range slices.Sorted(maps.Keys(commands)) {
		fmt.Fprintf(w, "  %-16s %s\n", name, commands[name].summary)
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Run `legion <command> --help` for a command's flags.")
}

// runSubcommand runs the subcommand of `legion <name>` that args[0] names, from table, the one
// list of them: -h, -help or --help prints subcommandUsage and exits 0, and no subcommand or an
// unknown one is a usage error.
func runSubcommand(ctx context.Context, name string, table map[string]command, args []string, stdout, stderr io.Writer) int {
	line := subcommandUsage(name, table)
	if len(args) > 0 && helpRequested(args[0]) {
		fmt.Fprintln(stderr, line)
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, line)
		return 2
	}
	sub, ok := table[args[0]]
	if !ok {
		fmt.Fprintf(stderr, "legion %s: unknown subcommand %q\n%s\n", name, args[0], line)
		return 2
	}
	return sub(ctx, args[1:], stdout, stderr)
}

// subcommandUsage is `legion <name>`'s usage line, naming every subcommand of table
// alphabetically: `usage: legion <name> a|b|… [flags]`.
func subcommandUsage(name string, table map[string]command) string {
	return "usage: legion " + name + " " + strings.Join(slices.Sorted(maps.Keys(table)), "|") + " [flags]"
}

func run(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if len(argv) < 2 {
		usage(stderr)
		return 2
	}
	if argv[1] == "help" || helpRequested(argv[1]) {
		usage(stdout)
		return 0
	}
	cmd, ok := commands[argv[1]]
	if !ok {
		fmt.Fprintf(stderr, "legion: unknown command %q; run legion --help for the commands\n", argv[1])
		return 2
	}
	return cmd.run(ctx, argv[2:], stdout, stderr)
}

func runVersion(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("version", "usage: legion version", stderr)
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() > 0 {
		flags.Usage()
		return 2
	}
	version := "(devel)"
	if release != "" {
		version = release
	} else if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		version = info.Main.Version
	}
	if revision != "" {
		fmt.Fprintf(stdout, "legion %s commit %s\n", version, revision)
		return 0
	}
	fmt.Fprintf(stdout, "legion %s\n", version)
	return 0
}

func runStart(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("start", "usage: legion start [flags]", stderr)
	configPath := flags.String("config", defaultConfigPath, "path to legion.yaml")
	checkConfig := flags.Bool("check-config", false, "validate the configuration, run none of its key commands, and exit without starting")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	// The configuration is named only by --config: a file given as an argument would otherwise be
	// ignored while ./legion.yaml is read, and --check-config would vouch for the wrong file.
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "legion start: unexpected argument %q; name the configuration with --config <file>\n", flags.Arg(0))
		return 2
	}
	if *checkConfig {
		return checkStartConfig(*configPath, stdout, stderr)
	}
	return start(ctx, *configPath, stderr)
}

// checkStartConfig is `legion start --check-config`: every refusal the configuration's read at
// boot makes, through the loader that runs neither GitHub App's private_key_command nor any
// secretsd read, then every refusal boot makes from the configuration, the environment and the
// files they name before it writes anything (daemon.CheckStart, the reads boot's own prepare
// starts with, the daemon's own NATS nkey seed among them), and nothing else: no store, no team, no
// file written, and no process but `mise where <tool>` when a tmux configuration's omp_invocation
// names a mise tool. The OK line names each NATS nkey seed's public key when there is one, never
// the seed.
func checkStartConfig(configPath string, stdout, stderr io.Writer) int {
	cfg, err := config.LoadForValidation(configPath, nil)
	paneNatsUser, daemonNatsUser := "", ""
	if err == nil {
		paneNatsUser, daemonNatsUser, err = daemon.CheckStart(cfg, os.LookupEnv)
	}
	if err != nil {
		fmt.Fprintf(stderr, "legion start: %v\n", err)
		return 1
	}
	ok := "Config OK: project=" + cfg.Project
	if paneNatsUser != "" {
		ok += " nats-nkey-user=" + paneNatsUser
	}
	if daemonNatsUser != "" {
		ok += " nats-daemon-nkey-user=" + daemonNatsUser
	}
	fmt.Fprintln(stdout, ok)
	return 0
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
	flags := newFlags("stop", "usage: legion stop [flags]", stderr)
	configPath := flags.String("config", defaultConfigPath, "path to legion.yaml")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "legion stop: unexpected argument %q; name the configuration with --config <file>\n", flags.Arg(0))
		return 2
	}

	// The file names the team and nothing else here, so its App key commands do not run.
	cfg, err := config.LoadForValidation(*configPath, nil)
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
	flags := newFlags("state", "usage: legion state [flags]", stderr)
	asJSON := flags.Bool("json", false, "print the state as the daemon serves it")
	configPath := flags.String("config", "", "path to legion.yaml (default "+defaultConfigPath+")")
	port := flags.Int("port", 0, "port to read, overriding the configured one")
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "legion state: unexpected argument %q; name the configuration with --config <file>\n", flags.Arg(0))
		return 2
	}

	address, err := daemonAddress(*configPath, *port)
	if err != nil {
		fmt.Fprintf(stderr, "legion state: %v\n", err)
		return 1
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
	fmt.Fprintln(stdout, pendingSummary(state.PendingStatusWrites))
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

// pendingSummary is the one line an operator reads for the Dispatch status writes the daemon has
// not finished: how many wait, and why the oldest — the one its issue's later writes wait behind —
// has not landed.
func pendingSummary(pending []api.PendingStatusWrite) string {
	if len(pending) == 0 {
		return "pending Dispatch status writes: none"
	}
	oldest := pending[0]
	next := oldest.NextAt.UTC().Format(time.RFC3339)
	if oldest.Attempts == 0 {
		return fmt.Sprintf("pending Dispatch status writes: %d; the oldest, for %s, has not run yet, next at %s", len(pending), oldest.Issue, next)
	}
	return fmt.Sprintf("pending Dispatch status writes: %d; the oldest, for %s, has failed %d attempts, next at %s: %s",
		len(pending), oldest.Issue, oldest.Attempts, next, oldest.LastError)
}

// stateAddress is where a daemon answers: the configured bind and port, with --port overriding
// the port. --port on its own reads a daemon on this box without a configuration to load, which
// is how a proof script reads a legion whose file it did not write. A wildcard bind is dialled
// the same way `legion status` dials it — one rule for both commands that read a configured
// bind. The file is read by the validating loader: finding an address is no reason to run both
// GitHub Apps' private_key_command, which config.Load does on every read.
func stateAddress(configPath string, port int) (string, error) {
	if configPath == "" && port != 0 {
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), nil
	}
	if configPath == "" {
		configPath = defaultConfigPath
	}
	cfg, err := config.LoadForValidation(configPath, nil)
	if err != nil {
		return "", err
	}
	if port == 0 {
		port = cfg.Port
	}
	return net.JoinHostPort(dialHost(cfg.Bind), strconv.Itoa(port)), nil
}

func runLegions(_ context.Context, args []string, stdout, stderr io.Writer) int {
	flags := newFlags("legions", "usage: legion legions [flags]", stderr)
	asJSON := flags.Bool("json", false, "print the registry as JSON")
	if code, ok := parseFlags(flags, args); !ok {
		return code
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
	if len(args) >= 2 && !strings.HasPrefix(args[0], "-") && !strings.HasPrefix(args[1], "-") {
		return runIssueStatus(ctx, args[0], args[1], args[2:], stdout, stderr)
	}
	flags := newFlags("status", "usage: legion status <team>\n       "+issueStatusSynopsis, stderr)
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 1 {
		flags.Usage()
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
	flags := newFlags("restart", "usage: legion restart <team>", stderr)
	if code, ok := parseFlags(flags, args); !ok {
		return code
	}
	if flags.NArg() != 1 {
		flags.Usage()
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

// newFlags is `legion <name>`'s flag set, reporting on stderr. Its Usage, which Parse prints for
// -h, -help and --help and after a flag it refuses, is usage and then every flag.
func newFlags(name, usage string, stderr io.Writer) *flag.FlagSet {
	flags := flag.NewFlagSet("legion "+name, flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprintln(stderr, usage)
		flags.PrintDefaults()
	}
	return flags
}

// parseFlags parses args into flags. When ok is false Parse has printed flags' Usage, and code
// is the command's exit: 0 for a help request, 2 for a flag it refused.
func parseFlags(flags *flag.FlagSet, args []string) (code int, ok bool) {
	switch err := flags.Parse(args); {
	case err == nil:
		return 0, true
	case errors.Is(err, flag.ErrHelp):
		return 0, false
	default:
		return 2, false
	}
}

// processEnvironment is this process's environment by name, the last entry for a name winning as
// it does for a child the process starts.
func processEnvironment() map[string]string {
	env := map[string]string{}
	for _, pair := range os.Environ() {
		if name, value, ok := strings.Cut(pair, "="); ok {
			env[name] = value
		}
	}
	return env
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args, os.Stdout, os.Stderr))
}
