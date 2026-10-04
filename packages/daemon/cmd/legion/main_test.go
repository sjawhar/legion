package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/registry"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// testMainEnv makes this package's test binary the real `legion`: with it set, TestMain is main()
// — its argv, its signal handling, its exit status — so a test can run a command as its own
// process; workspace-init's tests run several contending for one tree volume, the way two pods'
// init containers do.
const testMainEnv = "LEGION_CMD_TEST_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(testMainEnv) == "1" {
		main()
	}
	// Every test below starts isolated from the operator's own jj configuration — in particular
	// fsmonitor.backend = "watchman" on this devbox: JJ_CONFIG names a file that does not exist,
	// so jj falls back to its built-in defaults instead of reading ~/.jjconfig.toml, and no
	// test-created repository ever registers a root with the operator's long-running watchman.
	// watchman drops a root once its directory is deleted, so without this, the roots that pile
	// up are the ones from a run this devbox's load killed before t.TempDir's cleanup ran. A test
	// that sets its own JJ_CONFIG afterward (push_test.go's commit-trailer overlay, treeVolume's
	// isolated one) still wins: os.Environ() is read fresh by every exec.Command.
	configDir, err := os.MkdirTemp("", "legion-test-jj-config")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(1)
	}
	if err := os.Setenv("JJ_CONFIG", filepath.Join(configDir, "no-user-config.toml")); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(configDir)
	os.Exit(code)
}

// legionState points the registry at a directory of this test's own, so nothing here reads or
// writes the box's real record of running legions.
func legionState(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", home)
	return registry.Path(nil, home)
}

// legionConfig writes a configuration whose Postgres is deliberately unreachable: every test
// here is about the claim on the team, which `start` settles before it opens a store.
func legionConfig(t *testing.T, project string, port int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "legion.yaml")
	body := fmt.Sprintf("project: %s\nport: %d\npostgres_dsn: postgres://legion:legion@127.0.0.1:1/legion\nstate_dir: %s\n",
		project, port, filepath.Join(dir, "state"))
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}
	return path
}

// runningLegion is a live process named `legion`, which is what the registry's pids point at and
// what registry.Alive answers true for. A copy of sleep: Alive reads argv[0], and a shell
// script's argv[0] is the shell.
func runningLegion(t *testing.T) int {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("find sleep to copy as the probe: %v", err)
	}
	body, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatalf("read %s: %v", sleep, err)
	}
	path := filepath.Join(t.TempDir(), "legion")
	if err := os.WriteFile(path, body, 0o700); err != nil {
		t.Fatalf("write the probe: %v", err)
	}

	probe := exec.Command(path, "300")
	if err := probe.Start(); err != nil {
		t.Fatalf("start the probe: %v", err)
	}
	t.Cleanup(func() {
		_ = probe.Process.Kill()
		_, _ = probe.Process.Wait()
	})
	deadline := time.Now().Add(5 * time.Second)
	for !registry.Alive(probe.Process.Pid) {
		if time.Now().After(deadline) {
			t.Fatalf("the probe at %s never exec'd", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return probe.Process.Pid
}

// claim seeds the registry the way `legion start` takes a team.
func claim(t *testing.T, legions string, e registry.Entry) {
	t.Helper()
	held, ok, err := registry.Claim(legions, e)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !ok {
		t.Fatalf("Claim refused %s: pid %d holds the team", e.Team, held.PID)
	}
}

func reapedPID(t *testing.T) int {
	t.Helper()
	done := exec.Command("/bin/true")
	if err := done.Run(); err != nil {
		t.Fatalf("run /bin/true: %v", err)
	}
	return done.Process.Pid
}

// healthzOn serves /healthz on one address and returns the port, so a status can be told to dial
// somewhere other than 127.0.0.1 and be wrong about it.
func healthzOn(t *testing.T, host string) int {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Skipf("this box does not serve on %s: %v", host, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	return listener.Addr().(*net.TCPAddr).Port
}

// The registry entry is the team's one claim, and a second start must not take it: the first
// daemon is still serving, and a start that overwrote the entry and then failed to bind would
// delete it on the way out — leaving the running daemon invisible to stop, status and legions.
func TestStartRefusesWhileTheTeamsLegionIsRunning(t *testing.T) {
	legions := legionState(t)
	pid := runningLegion(t)
	claim(t, legions, registry.Entry{
		Team: "LEGION", ConfigPath: "/srv/legion.yaml", PID: pid, Port: 13370,
		Bind: "127.0.0.1", StartedAt: time.Now().UTC(),
	})

	var errb bytes.Buffer
	code := start(context.Background(), legionConfig(t, "LEGION", 13370), &errb)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr = %q", code, errb.String())
	}
	if !strings.Contains(errb.String(), "already running") || !strings.Contains(errb.String(), strconv.Itoa(pid)) {
		t.Fatalf("stderr = %q, want it to name the running legion and its pid", errb.String())
	}

	entry, ok, err := registry.Find(legions, "LEGION")
	if err != nil || !ok {
		t.Fatalf("Find = %v, %v, want the running legion's entry still there", ok, err)
	}
	if entry.PID != pid {
		t.Fatalf("entry pid = %d, want the running daemon's %d", entry.PID, pid)
	}
}

// Two teams on one state directory would share the worker stream socket and the panes' secret
// files, so `legion start` refuses the second while the first is live — naming both teams and the
// directory, the three things the operator changes one of — and records nothing for it.
func TestStartRefusesTheStateDirectoryAnotherTeamIsRunningOn(t *testing.T) {
	legions := legionState(t)
	config := legionConfig(t, "LEGION", 13370)
	stateDir := filepath.Join(filepath.Dir(config), "state")
	claim(t, legions, registry.Entry{
		Team: "WIDGETS", ConfigPath: "/srv/widgets.yaml", PID: runningLegion(t), Port: 14370,
		Bind: "127.0.0.1", StateDir: stateDir, StartedAt: time.Now().UTC(),
	})

	var errb bytes.Buffer
	code := start(context.Background(), config, &errb)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr = %q", code, errb.String())
	}
	for _, name := range []string{"LEGION", "WIDGETS", stateDir} {
		if !strings.Contains(errb.String(), name) {
			t.Errorf("stderr = %q, want it to name %s", errb.String(), name)
		}
	}
	if _, ok, err := registry.Find(legions, "LEGION"); err != nil || ok {
		t.Fatalf("Find(LEGION) = %v, %v; want the refused start unrecorded", ok, err)
	}
}

// A daemon that died without cleaning up leaves an entry naming a pid nothing holds. A stop is
// then the record's own repair, not an error.
func TestStopRemovesTheEntryOfADaemonThatIsGone(t *testing.T) {
	legions := legionState(t)
	claim(t, legions, registry.Entry{
		Team: "LEGION", ConfigPath: "/srv/legion.yaml", PID: reapedPID(t), Port: 13370,
		Bind: "127.0.0.1", StartedAt: time.Now().UTC(),
	})

	var out, errb bytes.Buffer
	code := runStop(context.Background(), []string{"--config", legionConfig(t, "LEGION", 13370)}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr = %q", code, errb.String())
	}
	if !strings.Contains(out.String(), "not running") {
		t.Fatalf("stdout = %q, want it to say the legion is not running", out.String())
	}
	if _, ok, err := registry.Find(legions, "LEGION"); err != nil || ok {
		t.Fatalf("Find after the stop = %v, %v, want the stale entry gone", ok, err)
	}
}

// `bind` is a shipped key the overlays set, so the address a legion answers on is the one it
// recorded — not loopback by assumption.
func TestStatusDialsTheAddressTheLegionRecorded(t *testing.T) {
	for _, probe := range []struct {
		name   string
		serve  string
		record string
	}{
		{name: "a bind that is not 127.0.0.1", serve: "127.0.0.2", record: "127.0.0.2"},
		{name: "every interface", serve: "127.0.0.1", record: "0.0.0.0"},
	} {
		t.Run(probe.name, func(t *testing.T) {
			legions := legionState(t)
			port := healthzOn(t, probe.serve)
			claim(t, legions, registry.Entry{
				Team: "LEGION", ConfigPath: "/srv/legion.yaml", PID: runningLegion(t), Port: port,
				Bind: probe.record, StartedAt: time.Now().UTC(),
			})

			var out, errb bytes.Buffer
			code := runStatus(context.Background(), []string{"LEGION"}, &out, &errb)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr = %q", code, errb.String())
			}
			if !strings.Contains(out.String(), "running") || !strings.Contains(out.String(), "healthy") {
				t.Fatalf("stdout = %q, want the legion reported running and healthy", out.String())
			}
		})
	}
}

func TestUnknownSubcommandIsUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "frobnicate"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), `unknown command "frobnicate"`) {
		t.Fatalf("stderr = %q", errb.String())
	}
}

// `legion --help` is the one place the commands are listed, which the docs site's CLI reference is
// generated from: every command, each on a line of its own, on stdout with exit 0; a bare `legion`
// prints the same list as a usage error.
func TestHelpListsEveryCommand(t *testing.T) {
	for _, tc := range []struct {
		argv   []string
		code   int
		stdout bool
	}{
		{[]string{"legion", "--help"}, 0, true},
		{[]string{"legion", "-h"}, 0, true},
		{[]string{"legion", "help"}, 0, true},
		{[]string{"legion"}, 2, false},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), tc.argv, &out, &errb)
		listed := errb.String()
		if tc.stdout {
			listed = out.String()
		}
		if code != tc.code {
			t.Fatalf("%v = %d, want %d; stderr %q", tc.argv, code, tc.code, errb.String())
		}
		for name := range commands {
			if !strings.Contains(listed, "\n  "+name+" ") {
				t.Errorf("%v does not list %s: %q", tc.argv, name, listed)
			}
		}
	}
}

// Every command answers -h, -help and --help with its usage, `usage: legion <command>…`, and exit
// 0: the docs site's CLI generator fails the build on any other answer.
func TestEveryCommandAnswersHelp(t *testing.T) {
	for name := range commands {
		for _, help := range []string{"-h", "-help", "--help"} {
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", name, help}, &out, &errb)
			if code != 0 || !usageOf(errb.String(), name) {
				t.Errorf("legion %s %s = %d, stderr %q; want exit 0 and its usage", name, help, code, errb.String())
			}
		}
	}
}

// usageOf says whether help begins with the usage line of `legion <words…>`.
func usageOf(help string, words ...string) bool {
	line, _, _ := strings.Cut(help, "\n")
	fields := strings.Fields(line)
	return len(fields) >= 2+len(words) && slices.Equal(fields[:2+len(words)], append([]string{"usage:", "legion"}, words...))
}

// A command with subcommands answers --help, exit 0, with a usage line naming every one of them —
// every key of its table — as `legion <command> a|b|…`, the line the docs site's CLI generator
// reads each subcommand from; and each subcommand answers --help with its own usage.
func TestDispatchersAnswerHelpWithTheirSubcommands(t *testing.T) {
	for _, tc := range []struct {
		command string
		table   map[string]command
	}{
		{"claims", claimsCommands},
		{"handoff", handoffCommands},
		{"workspace-init", workspaceInitCommands},
		{"controller", controllerCommands},
		{"threads", threadsCommands},
	} {
		var out, errb bytes.Buffer
		code := run(context.Background(), []string{"legion", tc.command, "--help"}, &out, &errb)
		prefix := "usage: legion " + tc.command + " "
		line, _, _ := strings.Cut(errb.String(), "\n")
		if code != 0 || !strings.HasPrefix(line, prefix) {
			t.Errorf("legion %s --help = %d, stderr %q; want exit 0 and stderr starting %q", tc.command, code, errb.String(), prefix)
			continue
		}
		named := strings.Split(strings.Fields(strings.TrimPrefix(line, prefix))[0], "|")
		for sub := range tc.table {
			if !slices.Contains(named, sub) {
				t.Errorf("legion %s --help names %v, not %s: %q", tc.command, named, sub, line)
			}
			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", tc.command, sub, "--help"}, &out, &errb)
			if code != 0 || !usageOf(errb.String(), tc.command, sub) {
				t.Errorf("legion %s %s --help = %d, stderr %q; want exit 0 and its usage", tc.command, sub, code, errb.String())
			}
		}
	}
}

func TestVersionPrintsBuildInfo(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !strings.HasPrefix(out.String(), "legion ") {
		t.Fatalf("stdout = %q", out.String())
	}
}

// The worker image links the commit it builds (`-ldflags "-X main.revision=<commit>"`), and
// `legion version` names it after the module version.
func TestVersionNamesTheLinkedRevision(t *testing.T) {
	linked := revision
	t.Cleanup(func() { revision = linked })
	revision = "0123456789abcdef0123456789abcdef01234567"

	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "version"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	got := out.String()
	if !strings.HasPrefix(got, "legion ") || !strings.HasSuffix(got, " commit "+revision+"\n") {
		t.Fatalf("stdout = %q, want \"legion <version> commit %s\\n\"", got, revision)
	}
}

// The legion release links its version and commit as release.yaml does
// (`-ldflags "-X main.revision=<commit> -X main.release=v<version>"`), and the built binary's
// `legion version` prints both: a variable renamed without the workflow would leave every
// release binary printing the module version instead.
func TestVersionPrintsTheReleaseTheLinkerSets(t *testing.T) {
	const commit = "0123456789abcdef0123456789abcdef01234567"
	binary := filepath.Join(t.TempDir(), "legion")
	build := exec.Command("go", "build", "-ldflags", "-X main.revision="+commit+" -X main.release=v9.9.9", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	out, err := exec.Command(binary, "version").Output()
	if err != nil {
		t.Fatalf("%s version: %v", binary, err)
	}
	if got, want := string(out), "legion v9.9.9 commit "+commit+"\n"; got != want {
		t.Fatalf("legion version = %q, want %q", got, want)
	}
}

// Both commands that read a configured bind dial it the same way: `legion state --config` on a
// daemon bound to every interface reads loopback, as `legion status` does.
func TestStateAddressDialsLoopbackForAWildcardBind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legion.yaml")
	body := "project: demo\nbind: 0.0.0.0\nport: 13370\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\nstate_dir: " + dir + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	address, err := stateAddress(path, 0)
	if err != nil {
		t.Fatalf("stateAddress: %v", err)
	}
	if address != "127.0.0.1:13370" {
		t.Fatalf("address = %q, want 127.0.0.1:13370", address)
	}
}

// Every Go role prompt tells a worker to re-read its issue record with `legion state` on relaunch.
// A pane has no legion.yaml; it has the daemon's URL (LEGION_DAEMON_URL) and its issue
// (LEGION_ISSUE), which the daemon names on every pane. From there `legion state` reads the state
// the daemon serves and prints the pane's issue record.
func TestStateInAPaneReadsTheDaemonItNamesAndPrintsTheIssueRecord(t *testing.T) {
	served := `{"daemon":{"project":"LEGION","schemaVersion":6,"boots":1,"firstBootAt":"2026-09-23T00:00:00Z","startedAt":"2026-09-23T00:00:00Z"},` +
		`"admission":{"cap":2,"active":[],"waiting":[]},"issues":{"LEGION-208":{"key":"LEGION-208","generation":2,"phase":"testing","status":"testing","workers":{}}},"pendingStatusWrites":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/legion/v1/state" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(served))
	}))
	defer server.Close()
	t.Chdir(t.TempDir())
	t.Setenv("LEGION_DAEMON_URL", server.URL)
	t.Setenv("LEGION_ISSUE", "LEGION-208")

	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "state"}, &out, &errb); code != 0 {
		t.Fatalf("legion state in a pane = %d, stderr %q", code, errb.String())
	}
	for _, want := range []string{"LEGION-208", `"phase": "testing"`, `"generation": 2`} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("legion state printed %q, want the pane's issue record naming %s", out.String(), want)
		}
	}
}

// workflowConfig writes a Stage 3 configuration whose two GitHub Apps read their keys through a
// private_key_command that leaves marker behind and fails: a command that ran it is seen twice
// over, in the marker and in the refusal. The operator and Dispatch bearer files it names exist, and
// LEGION_OMP_PATH names an executable, so the OMP invocation resolves without mise.
func workflowConfig(t *testing.T, port int, extra string) (path, marker string) {
	t.Helper()
	omp, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LEGION_OMP_PATH", omp)
	dir := t.TempDir()
	marker = filepath.Join(dir, "private-key-command-ran")
	command := "touch " + marker + "; exit 1"
	body := fmt.Sprintf(`project: DEMO
port: %d
postgres_dsn: postgres://legion:legion@127.0.0.1:1/legion
state_dir: %s
operator_token_file: ./operator-token
dispatch_url: https://dispatch.test
dispatch_token_file: ./dispatch-token
nats_urls: [nats://127.0.0.1:4222]
projects:
  DEMO: { repo: acme/widgets }
github_apps:
  implement: { app_id: "1", private_key_command: %q }
  review: { app_id: "2", private_key_command: %q }
%s`, port, filepath.Join(dir, "state"), command, command, extra)
	path = filepath.Join(dir, "legion.yaml")
	for name, contents := range map[string]string{path: body, filepath.Join(dir, "operator-token"): "operator-token\n", filepath.Join(dir, "dispatch-token"): "dispatch-token\n"} {
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return path, marker
}

// `legion start --check-config` validates the file the way boot does, says so, and exits: no App
// key command runs, no store is opened (the configured Postgres is unreachable), and no team is
// taken.
func TestStartCheckConfigValidatesAndStartsNothing(t *testing.T) {
	legions := legionState(t)
	config, marker := workflowConfig(t, 13370, "")

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--config", config, "--check-config"}, &out, &errb)
	if code != 0 {
		t.Fatalf("legion start --check-config = %d, stderr %q", code, errb.String())
	}
	if out.String() != "Config OK: project=DEMO\n" {
		t.Fatalf("stdout = %q, want \"Config OK: project=DEMO\\n\"", out.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the private_key_command ran (marker stat: %v)", err)
	}
	if _, found, err := registry.Find(legions, "DEMO"); err != nil || found {
		t.Fatalf("registry.Find = %v, %v, want no entry: a check takes no team", found, err)
	}
}

// A file given as an argument is refused rather than ignored: before, `legion start --check-config
// broken.yaml` checked ./legion.yaml instead and answered Config OK for the file it never read.
func TestStartRefusesAConfigurationGivenAsAnArgument(t *testing.T) {
	legionState(t)
	config, _ := workflowConfig(t, 13370, "")
	dir := filepath.Dir(config)
	if err := os.Rename(config, filepath.Join(dir, "legion.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.yaml"), []byte("worker_cap: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "start", "--check-config", "broken.yaml"}, &out, &errb)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stdout %q stderr %q", code, out.String(), errb.String())
	}
	if out.Len() != 0 {
		t.Fatalf("stdout = %q, want nothing: the file checked was not the one named", out.String())
	}
	if !strings.Contains(errb.String(), `unexpected argument "broken.yaml"`) || !strings.Contains(errb.String(), "--config") {
		t.Fatalf("stderr = %q, want it to name the argument and --config", errb.String())
	}
}

// stop and state refuse a file given as an argument as start does: they too read only --config, so
// `legion stop other.yaml` read ./legion.yaml instead and acted on the team that file names.
func TestStopAndStateRefuseAConfigurationGivenAsAnArgument(t *testing.T) {
	for _, command := range []string{"stop", "state"} {
		t.Run(command, func(t *testing.T) {
			legionState(t)
			config, _ := workflowConfig(t, 13370, "")
			dir := filepath.Dir(config)
			if err := os.Rename(config, filepath.Join(dir, "legion.yaml")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "other.yaml"), []byte("worker_cap: [\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Chdir(dir)

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", command, "other.yaml"}, &out, &errb)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2; stdout %q stderr %q", code, out.String(), errb.String())
			}
			if out.Len() != 0 {
				t.Fatalf("stdout = %q, want nothing: ./legion.yaml was not the file named", out.String())
			}
			want := "legion " + command + `: unexpected argument "other.yaml"`
			if !strings.Contains(errb.String(), want) || !strings.Contains(errb.String(), "--config") {
				t.Fatalf("stderr = %q, want %q and --config", errb.String(), want)
			}
		})
	}
}

// Every broken variant is refused naming its key, and the command exits non-zero — including a
// key only boot read before (the Dispatch bearer's file, internal/daemon/workflow.go bind).
func TestStartCheckConfigNamesTheBrokenKey(t *testing.T) {
	legionState(t)
	for _, variant := range []struct{ extra, drop, says string }{
		{extra: "worker_cap: 3\n", says: "worker_cap"},
		{extra: "admission_cap: 0\n", says: "admission_cap"},
		{extra: "gates: { design: sometimes }\n", says: "gates.design"},
		{extra: "envoy_url: not a url\n", says: "envoy_url"},
		{drop: "dispatch_token_file: ./dispatch-token\n", says: "dispatch_token_file is required when dispatch_url is configured"},
		{extra: "nats_nkey_seed_file: ./nats-seed\nprovider_keys: {NATS_NKEY_SEED: NATS_NKEY_SEED_TESTS}\n", says: "provider_keys names NATS_NKEY_SEED, the launch secret every launch carries"},
	} {
		config, marker := workflowConfig(t, 13370, variant.extra)
		if variant.drop != "" {
			body, err := os.ReadFile(config)
			if err != nil || !strings.Contains(string(body), variant.drop) {
				t.Fatalf("the fixture does not hold %q (%v)", variant.drop, err)
			}
			if err := os.WriteFile(config, []byte(strings.Replace(string(body), variant.drop, "", 1)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var out, errb bytes.Buffer
		code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)
		if code != 1 {
			t.Fatalf("%+v: exit code = %d, want 1; stdout %q stderr %q", variant, code, out.String(), errb.String())
		}
		if !strings.Contains(errb.String(), variant.says) {
			t.Fatalf("%+v: stderr = %q, want it to name %s", variant, errb.String(), variant.says)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("%+v: the private_key_command ran (marker stat: %v)", variant, err)
		}
	}
}

// --check-config makes every refusal boot makes from the configuration, the environment and the
// files they name before boot writes anything (daemon.CheckStart, which boot's prepare shares): the
// operator bearer's file, the Dispatch bearer's file, the instructions file, the OMP invocation,
// and the host's gh, git and jj. Each is refused in boot's words, no App key command runs, and no
// state directory is made.
func TestStartCheckConfigRefusesWhatBootRefuses(t *testing.T) {
	legionState(t)
	for _, tc := range []struct {
		name   string
		extra  string
		change func(t *testing.T, dir string)
		says   func(dir string) string
	}{
		{"a missing operator token file", "", func(t *testing.T, dir string) { remove(t, filepath.Join(dir, "operator-token")) },
			func(dir string) string {
				return "legion start: operator_token_file names " + filepath.Join(dir, "operator-token") + ", which could not be read: "
			}},
		{"no operator token file", "", func(t *testing.T, dir string) {
			rewrite(t, filepath.Join(dir, "legion.yaml"), "operator_token_file: ./operator-token\n", "")
		},
			func(string) string {
				return "legion start: operator_token_file is required: the operator routes that spawn and drive claims authenticate against the bearer it names\n"
			}},
		{"a missing Dispatch token file", "", func(t *testing.T, dir string) { remove(t, filepath.Join(dir, "dispatch-token")) },
			func(dir string) string {
				return "legion start: dispatch_token_file names " + filepath.Join(dir, "dispatch-token") + ", which could not be read: "
			}},
		{"a missing instructions file", "instructions: ./instructions.md\n", func(*testing.T, string) {},
			func(dir string) string {
				return "legion start: instructions file " + filepath.Join(dir, "instructions.md") + " could not be read: "
			}},
		{"no OMP invocation", "", func(t *testing.T, _ string) { t.Setenv("LEGION_OMP_PATH", "") },
			func(string) string {
				return "legion start: omp_invocation is not set: set it to 'mise x <tool> -- omp', or set LEGION_OMP_PATH to an absolute executable path\n"
			}},
		{"a relative LEGION_GIT_PATH", "", func(t *testing.T, _ string) { t.Setenv("LEGION_GIT_PATH", "bin/git") },
			func(string) string {
				return `legion start: LEGION_GIT_PATH is not an absolute executable path: "bin/git"` + "\n"
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, marker := workflowConfig(t, 13370, tc.extra)
			dir := filepath.Dir(config)
			tc.change(t, dir)

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)

			if want := tc.says(dir); code != 1 || out.Len() != 0 || !strings.HasPrefix(errb.String(), want) {
				t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1 and stderr starting %q", code, out.String(), errb.String(), want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the private_key_command ran (marker stat: %v)", err)
			}
			if _, err := os.Stat(filepath.Join(dir, "state")); !os.IsNotExist(err) {
				t.Fatalf("the check created the state directory (stat: %v)", err)
			}
		})
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func rewrite(t *testing.T, path, old, replacement string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), old) {
		t.Fatalf("%s does not hold %q (%v)", path, old, err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(body), old, replacement, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// --check-config reads every launch secret the way boot does (daemon.CheckStart): the Envoy
// bearer's file, and the NATS nkey seed (natsauth.Seed: the key's file over both variables, a file
// the daemon owns held to 0600, a user's nkey seed or nothing). A file boot
// would refuse fails the check with boot's refusal, and one it would take passes, the OK line
// naming the seed's public key and never the seed.
func TestStartCheckConfigReadsTheLaunchSecretsAsBootDoes(t *testing.T) {
	legionState(t)
	seed, public := testnats.User(t)
	const seedKey, envoyKey = "nats_nkey_seed_file", "envoy_token_file"
	for _, tc := range []struct {
		name     string
		key      string
		contents string
		mode     os.FileMode
		refusal  func(path string) string
	}{
		{"a missing Envoy token file", envoyKey, "", 0, func(path string) string { return "envoy_token_file names " + path + ", which could not be read: " }},
		{"a blank Envoy token file", envoyKey, " \n", 0o600, func(path string) string { return "envoy_token_file names " + path + ", which is empty\n" }},
		{"an Envoy token file", envoyKey, "envoy-token\n", 0o600, nil},
		{"a missing seed file", seedKey, "", 0, func(path string) string { return "nats_nkey_seed_file names " + path + ", which could not be read: " }},
		{"a seed file others can read", seedKey, seed + "\n", 0o644, func(path string) string {
			return "nats_nkey_seed_file " + path + " is readable by its group or others (mode 0644); chmod 0600 it\n"
		}},
		{"a group-readable seed file the daemon owns", seedKey, seed + "\n", 0o640, func(path string) string {
			return "nats_nkey_seed_file " + path + " is readable by its group or others (mode 0640); chmod 0600 it\n"
		}},
		{"a seed file holding no seed", seedKey, "SUNOTASEED\n", 0o600, func(path string) string {
			return "nats_nkey_seed_file (" + path + ") does not hold a valid nkey seed"
		}},
		{"a user seed", seedKey, seed + "\n", 0o600, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, marker := workflowConfig(t, 13370, tc.key+": ./secret\n")
			path := filepath.Join(filepath.Dir(config), "secret")
			if tc.mode != 0 {
				if err := os.WriteFile(path, []byte(tc.contents), tc.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			if tc.key == seedKey {
				t.Setenv("NATS_NKEY_SEED", "hunter2") // the key's file outranks it, as at boot
			}

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)

			if strings.Contains(out.String()+errb.String(), seed) {
				t.Fatalf("the check printed the seed: stdout %q stderr %q", out.String(), errb.String())
			}
			want := "Config OK: project=DEMO\n"
			if tc.key == seedKey {
				want = "Config OK: project=DEMO nats-nkey-user=" + public + "\n"
			}
			if tc.refusal == nil {
				if code != 0 || out.String() != want || errb.Len() != 0 {
					t.Fatalf("exit code = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), want)
				}
			} else if want := "legion start: " + tc.refusal(path); code != 1 || out.Len() != 0 || !strings.HasPrefix(errb.String(), want) {
				t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1 and stderr starting %q", code, out.String(), errb.String(), want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the private_key_command ran (marker stat: %v)", err)
			}
		})
	}
}

// --check-config reads the daemon's own seed as boot does, beside the pane seed: a refusal in boot's
// words, or an OK line naming both users by public key, never either seed.
func TestStartCheckConfigReadsTheDaemonNATSSeedAsBootDoes(t *testing.T) {
	legionState(t)
	paneSeed, paneUser := testnats.User(t)
	daemonSeed, daemonUser := testnats.User(t)
	for _, tc := range []struct {
		name     string
		contents string
		mode     os.FileMode
		refusal  func(path string) string
	}{
		{"a missing file", "", 0, func(path string) string {
			return "nats_daemon_nkey_seed_file names " + path + ", which could not be read: "
		}},
		{"a file others can read", daemonSeed + "\n", 0o644, func(path string) string {
			return "nats_daemon_nkey_seed_file " + path + " is readable by its group or others (mode 0644); chmod 0600 it\n"
		}},
		{"a file holding no seed", "SUNOTASEED\n", 0o600, func(path string) string {
			return "nats_daemon_nkey_seed_file (" + path + ") does not hold a valid nkey seed"
		}},
		{"a user seed only its owner can read", daemonSeed + "\n", 0o600, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, marker := workflowConfig(t, 13370, "nats_daemon_nkey_seed_file: ./nats-daemon-seed\n")
			path := filepath.Join(filepath.Dir(config), "nats-daemon-seed")
			if tc.mode != 0 {
				if err := os.WriteFile(path, []byte(tc.contents), tc.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, tc.mode); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("NATS_NKEY_SEED", paneSeed)
			t.Setenv("NATS_DAEMON_NKEY_SEED", "hunter2") // the key's file outranks it, as at boot

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)

			if printed := out.String() + errb.String(); strings.Contains(printed, daemonSeed) || strings.Contains(printed, paneSeed) {
				t.Fatalf("the check printed a seed: stdout %q stderr %q", out.String(), errb.String())
			}
			if tc.refusal == nil {
				if want := "Config OK: project=DEMO nats-nkey-user=" + paneUser + " nats-daemon-nkey-user=" + daemonUser + "\n"; code != 0 || out.String() != want || errb.Len() != 0 {
					t.Fatalf("exit code = %d, stdout %q, stderr %q; want 0 and %q", code, out.String(), errb.String(), want)
				}
			} else if want := "legion start: " + tc.refusal(path); code != 1 || out.Len() != 0 || !strings.HasPrefix(errb.String(), want) {
				t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1 and stderr starting %q", code, out.String(), errb.String(), want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the private_key_command ran (marker stat: %v)", err)
			}
		})
	}
}

// Under runtime: kubernetes, --check-config also makes boot's refusals of the runtime: an operator
// pod that collides with Legion's own (a mount at Legion's boot projection, refused naming both
// paths), and a kubeconfig that cannot be read — with no App key command run.
func TestStartCheckConfigRefusesWhatTheSandboxRuntimeRefuses(t *testing.T) {
	legionState(t)
	for _, tc := range []struct {
		name, runtime string
		says          func(dir string) string
	}{
		{"an operator pod colliding with Legion's", `    pod:
      volumes: [{name: creds, secret: {name: legion-creds}}]
      volume_mounts: [{volume: creds, mount_path: /var/run/legion/boot}]
`, func(string) string {
			return "legion start: runtime.kubernetes.pod.volume_mounts[0].mount_path /var/run/legion/boot overlaps /var/run/legion/boot, which Legion mounts in every pod: a mount may be neither at, under, nor above one of Legion's\n"
		}},
		{"a kubeconfig that cannot be read", "    kubeconfig: ./kubeconfig\n", func(dir string) string {
			return "legion start: read runtime.kubernetes.kubeconfig " + filepath.Join(dir, "kubeconfig") + ": "
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "private-key-command-ran")
			command := "touch " + marker + "; exit 1"
			body := fmt.Sprintf(`project: DEMO
postgres_dsn: postgres://legion:legion@127.0.0.1:1/legion
state_dir: %s
bind: 10.0.0.5
daemon_url: http://10.0.0.5:13370
envoy_url: http://envoy-listener.internal.example:9020
envoy_token_file: ./envoy-token
operator_token_file: ./operator-token
dispatch_url: https://dispatch.internal.example
dispatch_token_file: ./dispatch-token
nats_urls: [nats://nats.internal.example:4222]
projects:
  DEMO: { repo: acme/widgets }
github_apps:
  implement: { app_id: "1", private_key_command: %q }
  review: { app_id: "2", private_key_command: %q }
runtime:
  kubernetes:
    namespace: legion
    image: ghcr.io/sjawhar/legion-worker@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    storage_class: gp2
%s`, filepath.Join(dir, "state"), command, command, tc.runtime)
			config := filepath.Join(dir, "legion.yaml")
			for name, contents := range map[string]string{config: body, "envoy-token": "envoy\n", "operator-token": "operator\n", "dispatch-token": "dispatch\n"} {
				if !filepath.IsAbs(name) {
					name = filepath.Join(dir, name)
				}
				if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			var out, errb bytes.Buffer
			code := run(context.Background(), []string{"legion", "start", "--check-config", "--config", config}, &out, &errb)

			if want := tc.says(dir); code != 1 || out.Len() != 0 || !strings.HasPrefix(errb.String(), want) {
				t.Fatalf("exit code = %d, stdout %q, stderr %q; want 1 and stderr starting %q", code, out.String(), errb.String(), want)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the private_key_command ran (marker stat: %v)", err)
			}
		})
	}
}

// `legion state --config` reads where the file says the daemon answers, and nothing else of it:
// both GitHub Apps' private_key_command would fail, and the state is read all the same, with
// neither command run.
func TestStateConfigRunsNoPrivateKeyCommand(t *testing.T) {
	served := `{"daemon":{"project":"DEMO","schemaVersion":7,"boots":1,"firstBootAt":"2026-09-23T00:00:00Z","startedAt":"2026-09-23T00:00:00Z"},` +
		`"admission":{"cap":2,"active":[],"waiting":[]},"issues":{},"pendingStatusWrites":[]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(served))
	}))
	defer server.Close()
	port, err := strconv.Atoi(server.URL[strings.LastIndex(server.URL, ":")+1:])
	if err != nil {
		t.Fatalf("read the test server's port: %v", err)
	}
	config, marker := workflowConfig(t, port, "")

	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "state", "--config", config, "--json"}, &out, &errb); code != 0 {
		t.Fatalf("legion state --config = %d, stderr %q", code, errb.String())
	}
	if out.String() != served {
		t.Fatalf("stdout = %q, want the state served", out.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the private_key_command ran (marker stat: %v)", err)
	}
}

// `legion stop` needs the file only for the team it names, so it runs neither App's key command
// either: the answer is about the registry, never the key command's failure.
func TestStopRunsNoPrivateKeyCommand(t *testing.T) {
	legionState(t)
	config, marker := workflowConfig(t, 13370, "")

	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"legion", "stop", "--config", config}, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), "no legion is registered for DEMO") {
		t.Fatalf("legion stop = %d, stderr %q, want 1 naming the unregistered team", code, errb.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the private_key_command ran (marker stat: %v)", err)
	}
}

// An operator reading plain `legion state` during a Dispatch outage is told how many status writes
// wait and why the oldest has not landed: the count, the oldest's issue, its attempts, its next
// attempt and its last error, on one line. With none waiting the line says so.
func TestPlainStateSummarizesThePendingStatusWrites(t *testing.T) {
	daemon := `"daemon":{"project":"LEGION","schemaVersion":6,"boots":1,"firstBootAt":"2026-09-23T00:00:00Z","startedAt":"2026-09-23T00:00:00Z"},` +
		`"admission":{"cap":2,"active":[],"waiting":[]},"issues":{}`
	for _, tc := range []struct {
		name, pending, want string
	}{
		{
			name: "two waiting",
			pending: `[{"issue":"LEGION-208","payload":{"status":"testing"},"attempts":3,"nextAt":"2026-09-23T00:01:04Z","lastError":"Dispatch unavailable"},` +
				`{"issue":"LEGION-209","payload":{"status":"retro"},"attempts":0,"nextAt":"2026-09-23T00:00:30Z"}]`,
			want: "pending Dispatch status writes: 2; the oldest, for LEGION-208, has failed 3 attempts, next at 2026-09-23T00:01:04Z: Dispatch unavailable\n",
		},
		{
			name:    "oldest not yet attempted",
			pending: `[{"issue":"LEGION-209","payload":{"status":"retro"},"attempts":0,"nextAt":"2026-09-23T00:00:30Z"}]`,
			want:    "pending Dispatch status writes: 1; the oldest, for LEGION-209, has not run yet, next at 2026-09-23T00:00:30Z\n",
		},
		{name: "none", pending: `[]`, want: "pending Dispatch status writes: none\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served := "{" + daemon + `,"pendingStatusWrites":` + tc.pending + "}"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(served))
			}))
			defer server.Close()
			t.Chdir(t.TempDir())
			t.Setenv("LEGION_DAEMON_URL", server.URL)
			t.Setenv("LEGION_ISSUE", "")

			var out, errb bytes.Buffer
			if code := run(context.Background(), []string{"legion", "state"}, &out, &errb); code != 0 {
				t.Fatalf("legion state = %d, stderr %q", code, errb.String())
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("legion state printed %q, want the line %q", out.String(), tc.want)
			}
		})
	}
}
