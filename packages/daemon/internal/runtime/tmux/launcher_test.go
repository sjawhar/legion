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

// An Oh My Pi launch refuses a recorded session in Claude Code's directory, one recorded before
// the role moved back from claude, which `omp --resume` cannot continue; a session anywhere else is
// left to Oh My Pi, and a Claude Code launch is not refused its own.
func TestRefuseAnotherBackendsSession(t *testing.T) {
	state := "/state"
	transcript := "/state/claude/legion-omp-legion-43-tester/projects/-state-workspaces-LEGION-43/11111111-2222-4333-8444-555555555555.jsonl"
	err := refuseAnotherBackendsSession(state, runtime.BackendOmp, transcript)
	want := "the recorded session " + transcript + " is a Claude Code session under /state/claude: the role's backend changed since it was recorded, and OMP cannot continue it"
	if err == nil || err.Error() != want {
		t.Errorf("omp resuming a Claude Code transcript = %v, want %q", err, want)
	}
	for _, file := range []string{"/state/trees/x/.omp/s.jsonl", "/state/claudette/s.jsonl", "/elsewhere/claude/s.jsonl"} {
		if err := refuseAnotherBackendsSession(state, runtime.BackendOmp, file); err != nil {
			t.Errorf("omp resuming %s = %v", file, err)
		}
	}
	if err := refuseAnotherBackendsSession(state, runtime.BackendClaude, transcript); err != nil {
		t.Errorf("claude resuming its own transcript = %v", err)
	}
}

// A launch whose backend the daemon resolved no launcher for is refused naming it, before any
// file is written or tmux is run; an Oh My Pi launch resuming a Claude Code transcript is refused
// the same way, and a missing session names the harness it was recorded for.
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

	transcript := filepath.Join(ClaudeConfigDir(stateDir, spec.Claim), "projects", "p", "11111111-2222-4333-8444-555555555555.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	resumed := spec
	resumed.ResumeSessionFile = transcript
	if _, err := rt.Resume(context.Background(), nil, resumed); err == nil || !strings.Contains(err.Error(), "is a Claude Code session under "+filepath.Join(stateDir, "claude")) {
		t.Errorf("Resume from a Claude Code transcript on omp = %v", err)
	}

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
