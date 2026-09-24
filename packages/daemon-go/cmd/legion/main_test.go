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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/registry"
)

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
