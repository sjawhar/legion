package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/promptrefs"
)

// imageOmp is an `omp` that answers each of the image's probes by its own plan, one step per
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
"models --no-extensions --extension "*" --extension "*" --extension "*" --json") kind=load ;;
"models --no-extensions --extension "*" --json") kind=agents ;;
"models --extension "*" --json") kind=load ;;
"--mode rpc --no-session --no-extensions --no-skills --no-rules --no-lsp --no-tools") kind=session ;;
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
installed=$(cd "$root/plugins/node_modules/@sjawhar/pi-legion" 2>/dev/null && pwd -P)
envoy=$(cd "$root/plugins/node_modules/@sjawhar/pi-envoy" 2>/dev/null && pwd -P)
# A pod's lane hands Oh My Pi the Envoy plugin root, then the Legion plugin root, as its explicit
# extensions: each loads from its root.
if [ "$kind:$2" = "load:--no-extensions" ]; then envoy=$(cd "$4" && pwd -P); installed=$(cd "$6" && pwd -P); fi
legion="LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://$installed/dist/legion.js?mtime=1\nLEGION_PLUGIN_ENVOY_INTERFACE=1\n"
with_envoy="LEGION_ENVOY_INTERFACE=1\nLEGION_ENVOY_LOADED_FROM=file://$envoy/dist/envoy.js?mtime=1\n"
case "$kind:$step" in
agents:available) echo LEGION_OMP_AGENTS=available >&2; exit 0 ;;
agents:missing) echo LEGION_OMP_AGENTS=missing >&2; exit 0 ;;
agents:silent) exit 0 ;;
agents:available-then-die) echo LEGION_OMP_AGENTS=available >&2; echo "database is locked" >&2; exit 1 ;;
agents:missing-then-hang) echo LEGION_OMP_AGENTS=missing >&2; exec sleep 30 ;;
load:yes) printf "$legion$with_envoy" >&2; exit 0 ;;
load:no) echo LEGION_PLUGIN_LOADED=no >&2; exit 0 ;;
load:elsewhere) printf "LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://$dir/elsewhere/pi-legion/dist/legion.js\nLEGION_PLUGIN_ENVOY_INTERFACE=1\n$with_envoy" >&2; exit 0 ;;
load:no-envoy) printf "${legion}LEGION_ENVOY_INTERFACE=none\n" >&2; exit 0 ;;
load:envoy-mismatch) printf "${legion}LEGION_ENVOY_INTERFACE=2\nLEGION_ENVOY_LOADED_FROM=file://$envoy/dist/envoy.js?mtime=1\n" >&2; exit 0 ;;
load:legacy) printf "$legion${with_envoy}LEGION_LEGACY_PLUGIN_LOADED_FROM=file://$dir/legacy/dist/legion.js\n" >&2; exit 0 ;;
load:envoy-elsewhere) printf "${legion}LEGION_ENVOY_INTERFACE=1\nLEGION_ENVOY_LOADED_FROM=file://$dir/elsewhere/pi-envoy/dist/envoy.js\n" >&2; exit 0 ;;
session:refuses) echo "Invalid OMP_SESSION_STORAGE: legion-launch-probe (expected file or sql)" >&2; exit 1 ;;
session:accepts) exit 0 ;;
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

// imageHome is a HOME laid out as the worker image lays it out: the Legion plugin's manifest
// declaring legion (raw JSON) under opt-legion/pi-legion and the Envoy plugin's under
// opt-legion/pi-envoy, each linked into the `legion` profile as `omp plugin install` links it.
func imageHome(t *testing.T, legion string) string {
	t.Helper()
	home := t.TempDir()
	profile := filepath.Join(home, ".omp", "profiles", "legion")
	writeManifest(t, filepath.Join(legionRootOf(home), "package.json"), legion)
	link(t, legionRootOf(home), filepath.Dir(manifestAt(profile)))
	writeEnvoyManifest(t, filepath.Join(envoyRootOf(home), "package.json"))
	link(t, envoyRootOf(home), filepath.Dir(envoyManifestAt(profile)))
	return home
}

// legionRootOf and envoyRootOf are the image's two plugin roots under home, which a pod passes
// as its explicit extensions.
func legionRootOf(home string) string { return filepath.Join(home, "opt-legion", "pi-legion") }
func envoyRootOf(home string) string  { return filepath.Join(home, "opt-legion", "pi-envoy") }

func imageEnv(home string) map[string]string {
	return map[string]string{"HOME": home, "OMP_PROFILE": "legion", "PATH": os.Getenv("PATH"), "LEGION_OMP_PATH": "/opt/omp/bin/omp"}
}

// imageGateUnder is the image probe's gate over the image's HOME, loading the image's plugin roots
// as a pod does, run through f, with budgets that cost the suite little: attempts bounds the retry
// (0 for none).
func imageGateUnder(t *testing.T, f imageOmp, legion string, attempts int) (pluginGate, *bytes.Buffer) {
	t.Helper()
	var logged bytes.Buffer
	home := imageHome(t, legion)
	return pluginGate{
		env:             imageEnv(home),
		pluginRoot:      legionRootOf(home),
		envoyPluginRoot: envoyRootOf(home),
		workDir:         t.TempDir(),
		invocation:      f.path,
		timeout:         1500 * time.Millisecond,
		retry:           bootprobe.Retry{Initial: 10 * time.Millisecond, Max: 40 * time.Millisecond, Attempts: attempts},
		contract:        3,
		log:             slog.New(slog.NewTextHandler(&logged, nil)),
	}, &logged
}

// A pod loads the Envoy and Legion plugins as its two explicit extension roots, in that order,
// with discovery off, so the probe that certifies the image loads them the same way: a packaging
// or pin change that broke only that lane would otherwise pass the probe whose job is to refuse
// such an image, and every pod would start without the Legion tool.
func TestProbeImageLoadsThePluginTheWayAPodDoes(t *testing.T) {
	f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	home := imageHome(t, contractCurrent)
	root, envoyRoot := legionRootOf(home), envoyRootOf(home)

	err := ProbeImage(context.Background(), ImageProbe{
		Omp: f.path, Contract: 3, Env: imageEnv(home), WorkDir: t.TempDir(), PluginRoot: root, EnvoyPluginRoot: envoyRoot,
		RoleReferences: promptrefs.New(), Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	if err != nil {
		t.Fatalf("ProbeImage = %v, want every probe to pass", err)
	}
	argv := f.read(t, "load.argv.1")
	if !strings.Contains(argv, "--no-extensions --extension "+envoyRoot+" --extension "+root+" --extension ") {
		t.Errorf("the load probe ran `omp %s`, want it to run as a pod does: --no-extensions with %s then %s as the explicit extensions", strings.TrimSpace(argv), envoyRoot, root)
	}
	// The two plugin roots are also the extension roots the probe's own discovery of task agents
	// and skills reads: exported as the roots, never as the probe's own path.
	if env := f.read(t, "load.env.1"); !slices.Contains(strings.Split(env, "\n"), "LEGION_PROMPT_ROOTS="+envoyRoot+":"+root) {
		t.Errorf("the load probe's environment has no LEGION_PROMPT_ROOTS=%s:%s, the plugin roots:\n%s", envoyRoot, root, env)
	}
	// Without either plugin root the image probe has no lane a pod uses, and is refused before any
	// probe runs.
	for _, testCase := range []struct {
		name  string
		probe ImageProbe
		want  string
	}{
		{"no plugin root", ImageProbe{EnvoyPluginRoot: envoyRoot}, "ImageProbe.PluginRoot is required"},
		{"no Envoy plugin root", ImageProbe{PluginRoot: root}, "ImageProbe.EnvoyPluginRoot is required"},
	} {
		f = newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
		probe := testCase.probe
		probe.Omp, probe.Contract, probe.Env, probe.WorkDir = f.path, 3, imageEnv(home), t.TempDir()
		probe.RoleReferences, probe.Log = promptrefs.New(), slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
		err = ProbeImage(context.Background(), probe)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Fatalf("ProbeImage with %s = %v, want it refused saying %q", testCase.name, err, testCase.want)
		}
		if calls := f.calls(t); len(calls) != 0 {
			t.Errorf("ProbeImage with %s ran %v, want no probe", testCase.name, calls)
		}
	}
}

// Without the references of the role prompts a pod is handed, the image probe would resolve the
// plugin's names alone, so it is refused before any probe runs.
func TestTheImageProbeRequiresTheRolePromptsReferences(t *testing.T) {
	f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	home := imageHome(t, contractCurrent)

	err := ProbeImage(context.Background(), ImageProbe{
		Omp: f.path, Contract: 3, Env: imageEnv(home), WorkDir: t.TempDir(), PluginRoot: legionRootOf(home), EnvoyPluginRoot: envoyRootOf(home),
		Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	if err == nil || !strings.Contains(err.Error(), "ImageProbe.RoleReferences is required") {
		t.Fatalf("ProbeImage without role references = %v, want it refused naming RoleReferences", err)
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Errorf("ProbeImage without role references ran %v, want no probe", calls)
	}
}

// A relative plugin root is the one the caller's working directory names: the probe hands Oh My Pi
// each directory, absolute, and holds what it loaded to those directories' manifests.
func TestTheImageProbeResolvesARelativePluginRoot(t *testing.T) {
	f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	home := imageHome(t, contractCurrent)
	t.Chdir(home)

	err := ProbeImage(context.Background(), ImageProbe{
		Omp: f.path, Contract: 3, Env: imageEnv(home), WorkDir: home,
		PluginRoot: filepath.Join("opt-legion", "pi-legion"), EnvoyPluginRoot: filepath.Join("opt-legion", "pi-envoy"),
		RoleReferences: promptrefs.New(), Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})

	if err != nil {
		t.Fatalf("ProbeImage with relative plugin roots = %v, want every probe to pass", err)
	}
	if argv := f.read(t, "load.argv.1"); !strings.Contains(argv, "--extension "+envoyRootOf(home)+" --extension "+legionRootOf(home)+" ") {
		t.Errorf("the load probe ran `omp %s`, want the plugin roots as the absolute %s and %s", strings.TrimSpace(argv), envoyRootOf(home), legionRootOf(home))
	}
}

// In a pod's lane the plugins come from the image's plugin roots, and discovery is off, so no
// refusal there sends the operator to the profile's plugin install: each names the roots a pod
// loads, and sends the operator to rebuild the worker image.
func TestTheImageProbesRefusalsNameThePluginRootAPodLoads(t *testing.T) {
	for _, testCase := range []struct {
		name, legion string
		load         []string
		want         []string
		notWant      string
	}{
		{"another contract", contractPrevious, []string{"yes"},
			[]string{"speaks daemon API contract 2; this daemon requires 3", "into the plugin root @ROOT, which a pod loads as an explicit extension beside the Envoy plugin root @ENVOY"}, "OMP profile"},
		{"not loaded", contractCurrent, []string{"no"},
			[]string{"pi-legion 1.57.0 at @ROOT did not load with discovery off and @ROOT as one of Oh My Pi's two explicit extensions, as a pod loads it"}, "omp plugin list"},
		{"loaded from another copy", contractCurrent, []string{"elsewhere"},
			[]string{"pi-legion loads from @DIR/elsewhere/pi-legion/package.json, but the probe passed @ROOT as one of Oh My Pi's two explicit extensions"}, "dotenv"},
		{"no Envoy plugin", contractCurrent, []string{"no-envoy"},
			[]string{"pi-legion 1.57.0 loaded from @ROOT, but no pi-envoy loaded from @ENVOY, the other explicit extension a pod passes: the worker image is incomplete; build it from this daemon's commit"}, "install the @sjawhar/pi-envoy"},
		{"an Envoy plugin at another interface", contractCurrent, []string{"envoy-mismatch"},
			[]string{"pi-envoy at file://@REALENVOY/dist/envoy.js?mtime=1 publishes plugin interface 2; the pi-legion at @ROOT speaks 1: build the worker image from this daemon's commit"}, "install both"},
		{"the old package beside it", contractCurrent, []string{"legacy"},
			[]string{"the worker image loads @sjawhar/pi-legion-envoy from file://@RAWDIR/legacy/dist/legion.js beside pi-legion: build the worker image from this daemon's commit"}, "plugin uninstall"},
		{"an Envoy plugin from another copy", contractCurrent, []string{"envoy-elsewhere"},
			[]string{"pi-envoy loads from @DIR/elsewhere/pi-envoy/package.json, but the probe passed @ENVOY as one of Oh My Pi's two explicit extensions"}, "dotenv"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newImageOmp(t, []string{"available"}, testCase.load, []string{"refuses"})
			writeManifest(t, filepath.Join(f.dir, "elsewhere", "pi-legion", "package.json"), contractCurrent)
			writeEnvoyManifest(t, filepath.Join(f.dir, "elsewhere", "pi-envoy", "package.json"))
			gate, _ := imageGateUnder(t, f, testCase.legion, 0)

			err := gate.verifyImage(context.Background())

			if err == nil {
				t.Fatal("verifyImage passed, want a refusal")
			}
			dir, _ := filepath.EvalSymlinks(f.dir)
			realEnvoy, _ := filepath.EvalSymlinks(gate.envoyPluginRoot)
			for _, want := range testCase.want {
				want = strings.NewReplacer("@ROOT", gate.pluginRoot, "@ENVOY", gate.envoyPluginRoot, "@REALENVOY", realEnvoy, "@DIR", dir, "@RAWDIR", f.dir).Replace(want)
				if !strings.Contains(err.Error(), want) {
					t.Errorf("verifyImage = %v, want it to say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), testCase.notWant) {
				t.Errorf("verifyImage = %v, which names %q, a remedy of the lane a pod does not load", err, testCase.notWant)
			}
		})
	}
}

// `legion probe-image` runs the image's three launch probes — pi.agents, the plugin load held to
// the contract, the session-storage setting — each as a pod runs Oh My Pi, under the image's own
// environment, and leaves no probe file behind.
func TestProbeImageRunsTheThreeProbesUnderTheImagesEnvironment(t *testing.T) {
	f := newImageOmp(t, []string{"available"}, []string{"yes"}, []string{"refuses"})
	home := imageHome(t, contractCurrent)
	env := imageEnv(home)

	err := ProbeImage(context.Background(), ImageProbe{
		Omp: f.path, Contract: 3, Env: env, WorkDir: t.TempDir(), PluginRoot: legionRootOf(home), EnvoyPluginRoot: envoyRootOf(home),
		RoleReferences: promptrefs.New(), Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
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
	if sessionEnv := f.read(t, "session.env.1"); !strings.Contains(sessionEnv, "OMP_SESSION_STORAGE=legion-launch-probe\n") {
		t.Error("the session-storage probe ran without OMP_SESSION_STORAGE=legion-launch-probe")
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

	if err == nil || !strings.Contains(err.Error(), "speaks daemon API contract 2; this daemon requires 3") {
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
