package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
)

// imageOmp is an `omp` that answers each of the image's three probes by its own plan, one step per
// attempt of that probe (the last step repeats), telling them apart by argv as a real Oh My Pi
// would run them, and recording the order they ran in and each attempt's argv and environment.
type imageOmp struct{ dir, path string }

func newImageOmp(t *testing.T, agents, load, session []string) imageOmp {
	t.Helper()
	dir := t.TempDir()
	for kind, plan := range map[string][]string{"agents": agents, "load": load, "session": session} {
		if err := os.WriteFile(filepath.Join(dir, kind+".plan"), []byte(strings.Join(plan, "\n")+"\n"), 0o644); err != nil {
			t.Fatalf("write the %s plan: %v", kind, err)
		}
	}
	script := `#!/bin/sh
dir=` + dir + `
case "$*" in
"models --no-extensions --extension "*" --json") kind=agents ;;
"models --extension "*" --json") kind=load ;;
"--no-session --no-extensions --no-skills --no-rules --no-lsp --no-tools") kind=session ;;
*) echo "fake omp: unexpected argv: $*" >&2; exit 64 ;;
esac
n=$(( $(cat "$dir/$kind.count" 2>/dev/null || echo 0) + 1 ))
echo "$n" >"$dir/$kind.count"
echo "$kind" >>"$dir/calls"
printf '%s\n' "$*" >"$dir/$kind.argv.$n"
env >"$dir/$kind.env.$n"
step=$(sed -n "${n}p" "$dir/$kind.plan")
[ -n "$step" ] || step=$(tail -n 1 "$dir/$kind.plan")
root="$HOME/.omp"
[ -n "${OMP_PROFILE:-}" ] && root="$root/profiles/$OMP_PROFILE"
installed=$(cd "$root/plugins/node_modules/@sjawhar/pi-legion-envoy" 2>/dev/null && pwd -P)
case "$kind:$step" in
agents:available) echo LEGION_OMP_AGENTS=available >&2; exit 0 ;;
agents:missing) echo LEGION_OMP_AGENTS=missing >&2; exit 0 ;;
agents:silent) exit 0 ;;
agents:available-then-die) echo LEGION_OMP_AGENTS=available >&2; echo "database is locked" >&2; exit 1 ;;
agents:missing-then-hang) echo LEGION_OMP_AGENTS=missing >&2; exec sleep 30 ;;
load:yes) printf 'LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://%s/dist/legion.js?mtime=1\n' "$installed" >&2; exit 0 ;;
load:no) echo LEGION_PLUGIN_LOADED=no >&2; exit 0 ;;
session:refuses) echo "Invalid OMP_SESSION_STORAGE: legion-launch-probe (expected file or sql)" >&2; exit 1 ;;
session:accepts) echo "startup timings: settings 3ms" >&2; exit 0 ;;
session:dies) echo "database is locked" >&2; exit 1 ;;
*:denied) echo "secrets: ANTHROPIC_API_KEY was denied" >&2; exit 3 ;;
*:hang) exec sleep 30 ;;
esac
echo "fake omp: no step $step for $kind" >&2; exit 65
`
	path := filepath.Join(dir, "omp")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake omp: %v", err)
	}
	return imageOmp{dir: dir, path: path}
}

// calls are the probes the fake ran, in order.
func (f imageOmp) calls(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "calls"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read the calls: %v", err)
	}
	return strings.Fields(string(raw))
}

func (f imageOmp) attempts(t *testing.T, kind string) int {
	t.Helper()
	n := 0
	for _, call := range f.calls(t) {
		if call == kind {
			n++
		}
	}
	return n
}

func (f imageOmp) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// imageHome is a HOME laid out as the worker image lays it out: the plugin's manifest declaring
// legion (raw JSON), linked into the `legion` profile as `omp plugin install` links it.
func imageHome(t *testing.T, legion string) string {
	t.Helper()
	home := t.TempDir()
	unpacked := filepath.Join(home, "opt-legion", "pi-legion-envoy")
	writeManifest(t, filepath.Join(unpacked, "package.json"), legion)
	installed := filepath.Dir(manifestAt(filepath.Join(home, ".omp", "profiles", "legion")))
	mkdir(t, filepath.Dir(installed))
	if err := os.Symlink(unpacked, installed); err != nil {
		t.Fatalf("link the plugin into the profile: %v", err)
	}
	return home
}

func imageEnv(home string) map[string]string {
	return map[string]string{"HOME": home, "OMP_PROFILE": "legion", "PATH": os.Getenv("PATH"), "LEGION_OMP_PATH": "/opt/omp/bin/omp"}
}

// imageGateUnder is the image probe's gate over the image's HOME, run through f, with budgets that
// cost the suite little: attempts bounds the retry (0 for none).
func imageGateUnder(t *testing.T, f imageOmp, legion string, attempts int) (pluginGate, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	return pluginGate{
		env:        imageEnv(imageHome(t, legion)),
		workDir:    t.TempDir(),
		invocation: f.path,
		timeout:    1500 * time.Millisecond,
		retry:      bootprobe.Retry{Initial: 10 * time.Millisecond, Max: 40 * time.Millisecond, Attempts: attempts},
		contract:   3,
		log:        slog.New(slog.NewTextHandler(&logged, nil)),
	}, &logged
}

// `legion probe-image` runs the image's three launch probes — pi.agents, the plugin load held to
// the contract, the session-storage setting — each as a pane would run Oh My Pi, under the image's
// own environment, and leaves no probe file behind.
func TestProbeImageRunsTheThreeProbesUnderTheImagesEnvironment(t *testing.T) {
	f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	env := imageEnv(imageHome(t, contractCurrent))

	err := ProbeImage(context.Background(), ImageProbe{
		Omp: f.path, Contract: 3, Env: env, WorkDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	if err != nil {
		t.Fatalf("ProbeImage = %v, want every probe to pass", err)
	}
	if got := strings.Join(f.calls(t), " "); got != "agents load session" {
		t.Fatalf("probes ran as %q, want pi.agents, the plugin load, then the session-storage setting", got)
	}
	for _, kind := range []string{"agents", "load"} {
		argv := strings.Fields(f.read(t, kind+".argv.1"))
		probe := argv[len(argv)-2]
		if _, err := os.Stat(probe); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the %s probe file %s outlived the probe: %v", kind, probe, err)
		}
	}
	for _, kind := range []string{"agents", "load", "session"} {
		probeEnv := f.read(t, kind+".env.1")
		for name, value := range env {
			if !strings.Contains(probeEnv, name+"="+value+"\n") {
				t.Errorf("the %s probe ran without the image's %s=%s", kind, name, value)
			}
		}
	}
	sessionEnv := f.read(t, "session.env.1")
	for _, want := range []string{"OMP_SESSION_STORAGE=legion-launch-probe\n", "PI_TIMING=x\n"} {
		if !strings.Contains(sessionEnv, want) {
			t.Errorf("the session-storage probe ran without %q", strings.TrimSpace(want))
		}
	}
	if strings.Contains(f.read(t, "agents.env.1"), "OMP_SESSION_STORAGE=") {
		t.Error("the pi.agents probe ran with the session-storage probe's variable")
	}
}

// The image's plugin is held to the contract the caller names — the daemon's, passed to the probe
// Sandbox's pod — and refused naming both numbers before the plugin load is probed.
func TestTheImageProbeHoldsThePluginToTheContractItIsGiven(t *testing.T) {
	f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	gate, _ := imageGateUnder(t, f, contractPrevious, 0)
	gate.contract = 2
	if err := gate.verifyImage(context.Background()); err != nil {
		t.Fatalf("a plugin declaring contract 2 against contract 2: %v", err)
	}

	f = newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	gate, _ = imageGateUnder(t, f, contractPrevious, 0)
	err := gate.verifyImage(context.Background())

	if err == nil || !strings.Contains(err.Error(), "speaks Go daemon API contract 2; this daemon requires 3") {
		t.Fatalf("verifyImage = %v, want the contract refusal naming 2 and 3", err)
	}
	if got := strings.Join(f.calls(t), " "); got != "agents" {
		t.Errorf("probes ran as %q, want only pi.agents before the contract refused", got)
	}
}

// The pi.agents probe: a pass needs the capability marker and a clean exit; an answer — the
// `missing` marker, a clean exit without a marker, a launch that fails before Oh My Pi — is
// refused at once; Oh My Pi dying after it answered, or a probe the budget cut off before it
// answered, is waited out.
func TestTheAgentsProbeClassifiesWhatOhMyPiSaid(t *testing.T) {
	for _, testCase := range []struct {
		plan     []string
		attempts int
		refusal  []string
	}{
		{[]string{"available"}, 1, nil},
		{[]string{"missing"}, 1, []string{"does not expose pi.agents", "LEGION_OMP_AGENTS=missing"}},
		{[]string{"silent"}, 1, []string{"does not expose pi.agents"}},
		{[]string{"denied"}, 1, []string{"does not expose pi.agents", "secrets: ANTHROPIC_API_KEY was denied"}},
		{[]string{"missing-then-hang"}, 1, []string{"does not expose pi.agents", "LEGION_OMP_AGENTS=missing"}},
		{[]string{"available-then-die", "available-then-die", "available"}, 3, nil},
		{[]string{"hang", "hang", "available"}, 3, nil},
	} {
		t.Run(strings.Join(testCase.plan, ","), func(t *testing.T) {
			f := newImageOmp(t, testCase.plan, []string{"yes"}, []string{"refuses"})
			gate, logged := imageGateUnder(t, f, contractCurrent, 0)

			err := gate.verifyImage(context.Background())

			if testCase.refusal == nil && err != nil {
				t.Fatalf("verifyImage = %v, want a pass", err)
			}
			for _, want := range testCase.refusal {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("verifyImage = %v, want a refusal saying %q", err, want)
				}
			}
			if testCase.refusal != nil && !strings.Contains(err.Error(), f.path) {
				t.Errorf("the refusal does not name the launch command %s: %v", f.path, err)
			}
			if n := f.attempts(t, "agents"); n != testCase.attempts {
				t.Errorf("the pi.agents probe ran %d times, want %d", n, testCase.attempts)
			}
			if got, want := strings.Count(logged.String(), "failed transiently"), testCase.attempts-1; got != want {
				t.Errorf("logged %d transient failures, want %d:\n%s", got, want, logged.String())
			}
		})
	}
}

// The session-storage probe: a build that carries the setting refuses the probe's nonsense value,
// naming the variable; a clean start is a build that predates the setting, refused; any other
// failure is the launch dying before the setting's resolver ran, waited out.
func TestTheSessionStorageProbeClassifiesWhatOhMyPiSaid(t *testing.T) {
	for _, testCase := range []struct {
		plan     []string
		attempts int
		refusal  []string
	}{
		{[]string{"refuses"}, 1, nil},
		{[]string{"accepts"}, 1, []string{"started with OMP_SESSION_STORAGE=legion-launch-probe (exit 0)", "predates the session.storage setting"}},
		{[]string{"dies", "dies", "refuses"}, 3, nil},
		{[]string{"hang", "hang", "refuses"}, 3, nil},
	} {
		t.Run(strings.Join(testCase.plan, ","), func(t *testing.T) {
			f := newImageOmp(t, []string{"available"}, []string{"yes"}, testCase.plan)
			gate, _ := imageGateUnder(t, f, contractCurrent, 0)

			err := gate.verifyImage(context.Background())

			if testCase.refusal == nil && err != nil {
				t.Fatalf("verifyImage = %v, want a pass", err)
			}
			for _, want := range testCase.refusal {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("verifyImage = %v, want a refusal saying %q", err, want)
				}
			}
			if n := f.attempts(t, "session"); n != testCase.attempts {
				t.Errorf("the session-storage probe ran %d times, want %d", n, testCase.attempts)
			}
		})
	}
}

// The image probe's retry is bounded: an image build has no supervisor, so a probe that never
// answers ends the build naming the budget and what the last attempt said.
func TestTheImageProbeGivesUpAfterItsAttempts(t *testing.T) {
	for _, testCase := range []struct {
		name                  string
		agents, session, want []string
		kind                  string
	}{
		{"pi.agents cut off every time", []string{"hang"}, []string{"refuses"},
			[]string{"the OMP pi.agents probe never completed within its retry budget (3 attempts)", "timed out after 1.5s"}, "agents"},
		{"Oh My Pi dying before the resolver every time", []string{"available"}, []string{"dies"},
			[]string{"the OMP session storage setting probe never completed within its retry budget (3 attempts)", "exited 1 without naming OMP_SESSION_STORAGE", "database is locked"}, "session"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newImageOmp(t, testCase.agents, []string{"yes"}, testCase.session)
			gate, _ := imageGateUnder(t, f, contractCurrent, 3)

			err := gate.verifyImage(context.Background())

			for _, want := range append(testCase.want, f.path) {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("verifyImage = %v, want an error saying %q", err, want)
				}
			}
			if n := f.attempts(t, testCase.kind); n != 3 {
				t.Errorf("the %s probe ran %d times, want the bound, 3", testCase.kind, n)
			}
		})
	}
}
