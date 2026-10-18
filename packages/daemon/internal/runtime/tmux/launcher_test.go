package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// ompOnly is an omp-only daemon's launchers: Oh My Pi on the invocation given, no prefix.
func ompOnly(invocation string) map[runtime.Backend]Launcher {
	return map[runtime.Backend]Launcher{runtime.BackendOmp: NewOmpLauncher(nil, invocation)}
}

// The Oh My Pi launcher's command is innerCommand's, byte for byte (TestInnerCommand pins those
// bytes), and its pane carries no pairs of its own.
func TestOmpLauncherLaunchIsInnerCommand(t *testing.T) {
	spec := testSpec()
	prefix := []string{"env", "K=v", "--"}
	for _, resume := range []string{"", "/state/trees/x/.omp/s 1.jsonl"} {
		spec.ResumeSessionFile = resume
		inner, pairs, err := NewOmpLauncher(prefix, "/opt/oh-my-pi/18.0.3/omp").Launch(spec, Host{StateDir: "/state", Legion: "/opt/legion"})
		if err != nil || pairs != nil {
			t.Fatalf("Launch = %v, %q", err, pairs)
		}
		if want := innerCommand(prefix, "/opt/oh-my-pi/18.0.3/omp", resume, spec.Prompt); inner != want {
			t.Errorf("Launch(resume %q) = %q, want %q", resume, inner, want)
		}
	}
}

// The live pane's root process, as /proc/<pane pid>/cmdline held it on a real tmux server: `sh -c`
// on the pane's whole shell command, with the shim's inner command spliced in. The harness itself
// is a descendant, never the pane's pid. Every pane's command holds "omp", if only in
// `--append-system-prompt`; a Claude Code pane's alone holds ` claude-bridge -- ` (its Marker).
const (
	livePaneOmp = "/bin/sh\x00-c\x00tmux set-option -w -t \"$TMUX_PANE\" @legion_owner legion-t1 && export PATH=/state/bin:/usr/bin:/bin && " +
		"cd /state/workspaces/LEGION-1 && /opt/legion/legion worker-shim --connect unix:///state/stream.sock " +
		"--boot-token-file /state/secrets/legion-t1-legion-1-architect -- /opt/bin/omp --mode rpc --append-system-prompt \"$(cat /roles/architect.md)\"\x00"
	livePaneClaude = "/bin/sh\x00-c\x00tmux set-option -w -t \"$TMUX_PANE\" @legion_owner legion-t1 && export PATH=/state/bin:/usr/bin:/bin && " +
		"cd /state/workspaces/LEGION-2 && /opt/legion/legion worker-shim --connect unix:///state/stream.sock " +
		"--boot-token-file /state/secrets/legion-t1-legion-2-implementer -- /opt/legion/legion claude-bridge -- /opt/claude/bin/claude -p " +
		"--input-format stream-json --output-format stream-json --verbose --permission-mode bypassPermissions --strict-mcp-config " +
		"--mcp-config /state/claude/legion-t1-legion-2-implementer/mcp.json --plugin-dir /opt/claude-envoy --plugin-dir /opt/claude-envoy/legion " +
		"--session-id a1917bc9-2989-441e-9eef-c65e0d51bfbf --append-system-prompt \"$(cat /roles/implementer.md)\"\x00"
)

// verifyPane's command-line tier accepts a pane holding the marker of a harness the daemon launches,
// and nothing else. An omp-only daemon accepts exactly what `strings.Contains(cmdline, "omp")`
// accepts, as it always has; a daemon with Claude Code's launcher beside it accepts a Claude Code
// pane too, and a process that merely names claude-bridge is still no agent.
func TestRunsAgentAcceptsTheConfiguredHarnessesMarkers(t *testing.T) {
	runsAgent := func(launchers map[runtime.Backend]Launcher, cmdline string) bool {
		markers, _ := agentMarkers(launchers)
		r := &Runtime{markers: markers, readProc: func(string) ([]byte, error) { return []byte(cmdline), nil }}
		got, err := r.runsAgent(1)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	cmdlines := []string{
		livePaneOmp, livePaneClaude,
		"/usr/bin/unrelated-claude-bridge\x00300\x00", "/usr/bin/unrelated-omp\x00300\x00",
		"/bin/sh\x00-c\x00sleep 300\x00", "/bin/sh\x00-c\x00exec /opt/legion/legion worker-shim -- /opt/bin/agent\x00",
	}
	for _, cmdline := range cmdlines {
		if got, main := runsAgent(ompOnly("omp"), cmdline), strings.Contains(cmdline, "omp"); got != main {
			t.Errorf("omp-only runsAgent(%q) = %v, main's = %v", cmdline, got, main)
		}
	}
	both := map[runtime.Backend]Launcher{runtime.BackendOmp: NewOmpLauncher(nil, "omp"), runtime.BackendClaude: NewClaudeLauncher(testClaude)}
	claudeOnly := map[runtime.Backend]Launcher{runtime.BackendClaude: NewClaudeLauncher(testClaude)}
	for _, tc := range []struct {
		name      string
		launchers map[runtime.Backend]Launcher
		cmdline   string
		want      bool
	}{
		{"both, an Oh My Pi pane", both, livePaneOmp, true},
		{"both, a Claude Code pane", both, livePaneClaude, true},
		{"Claude Code's launcher alone, a Claude Code pane", claudeOnly, livePaneClaude, true},
		{"Claude Code's launcher alone, an Oh My Pi pane", claudeOnly, livePaneOmp, false},
		{"Claude Code's launcher alone, a process naming claude-bridge", claudeOnly, "/usr/bin/unrelated-claude-bridge\x00300\x00", false},
		{"both, a bystander", both, "/bin/sh\x00-c\x00sleep 300\x00", false},
	} {
		if got := runsAgent(tc.launchers, tc.cmdline); got != tc.want {
			t.Errorf("%s: runsAgent = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// What a pane that verifies as no agent is said not to run: every configured harness, by name.
func TestAgentMarkersNameEveryConfiguredHarness(t *testing.T) {
	markers, agents := agentMarkers(ompOnly("omp"))
	if strings.Join(markers, ",") != "omp" || agents != "OMP" {
		t.Errorf("omp-only = %q, %q", markers, agents)
	}
	markers, agents = agentMarkers(map[runtime.Backend]Launcher{runtime.BackendOmp: NewOmpLauncher(nil, "omp"), runtime.BackendClaude: NewClaudeLauncher(testClaude)})
	if strings.Join(markers, ",") != " claude-bridge -- ,omp" || agents != "Claude Code or OMP" {
		t.Errorf("both = %q, %q", markers, agents)
	}
}

// A launch whose backend the daemon resolved no launcher for is refused naming it, before any
// file is written or tmux is run, and a missing session names the harness it was recorded for.
func TestLaunchRefusesWhatItsLaunchersCannotRun(t *testing.T) {
	stateDir := t.TempDir()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	rt, err := New(Options{
		Project: "omp", StateDir: stateDir, StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", Launchers: ompOnly("omp"), StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=" + bin},
		Executable: func() (string, error) { return "/opt/legion", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	rt.run = func(context.Context, []string) (result, error) { return result{}, errors.New("tmux ran") }
	prompt := filepath.Join(t.TempDir(), "tester.md")
	if err := os.WriteFile(prompt, []byte("tester"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := testSpec()
	spec.Prompt.RolePromptPaths = []string{prompt}

	claude := spec
	claude.Backend = runtime.BackendClaude
	want := "spawn legion-omp-legion-43-tester: its role runs on claude, and the daemon resolved no Claude Code launch (worker_backends)"
	if _, err := rt.Spawn(context.Background(), claude); err == nil || err.Error() != want {
		t.Errorf("Spawn on claude = %v, want %q", err, want)
	}

	resumed := spec
	resumed.ResumeSessionFile = filepath.Join(stateDir, "gone.jsonl")
	want = "Refusing to start LEGION-43 fresh while respawning: recorded OMP session file is missing: " + resumed.ResumeSessionFile
	if _, err := rt.Resume(context.Background(), nil, resumed); err == nil || err.Error() != want {
		t.Errorf("Resume from a missing session = %v, want %q", err, want)
	}
	if entries, _ := os.ReadDir(filepath.Join(stateDir, "secrets")); len(entries) != 0 {
		t.Errorf("a refused launch wrote %d secret files", len(entries))
	}
}

// New refuses a runtime with no launcher, and one handed a launcher for a backend the tmux runtime
// does not launch.
func TestNewRefusesLaunchersItCannotRun(t *testing.T) {
	opts := Options{
		Project: "omp", StateDir: t.TempDir(), StreamAddress: "unix:///s", DaemonURL: "http://127.0.0.1:1",
		EnvoyURL: "http://127.0.0.1:2", StopGrace: time.Second, ProbeInterval: time.Second,
		AdoptTimeout: time.Second, Conns: fake.NewConns(), Environ: []string{"PATH=/usr/bin:/bin"},
		Executable: func() (string, error) { return "/opt/legion", nil },
	}
	if _, err := New(opts); err == nil || err.Error() != "tmux runtime: no launcher" {
		t.Errorf("New with no launcher = %v", err)
	}
	opts.Launchers = map[runtime.Backend]Launcher{"codex": NewOmpLauncher(nil, "omp")}
	if _, err := New(opts); err == nil || err.Error() != `tmux runtime: a launcher for "codex", which the tmux runtime does not launch` {
		t.Errorf("New with an unknown backend's launcher = %v", err)
	}
}
