package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
)

// manifestAt is where the tests' plugin manifest goes under a plugins root.
func manifestAt(root string) string {
	return filepath.Join(root, "plugins", "node_modules", "@sjawhar", "pi-legion-envoy", "package.json")
}

// writeManifest writes a pi-legion-envoy manifest at path; legion is the raw JSON of its `legion`
// member, or empty for a manifest without one.
func writeManifest(t *testing.T, path, legion string) {
	t.Helper()
	body := `{"name":"@sjawhar/pi-legion-envoy","version":"1.57.0"`
	if legion != "" {
		body += `,"legion":` + legion
	}
	body += "}\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the manifest directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}

// The gate reads the manifest where Oh My Pi, started under the pane's environment, looks for its
// plugins: the profile OMP_PROFILE (else PI_PROFILE) names, under HOME's .omp, unless an XDG data
// root already holds that profile's state.
func TestPluginManifestPathIsWhereOhMyPiResolvesItsPluginsRoot(t *testing.T) {
	passwdHome := ""
	if u, err := user.Current(); err == nil {
		passwdHome = u.HomeDir
	}
	for _, testCase := range []struct {
		name    string
		env     func(home, xdg string) map[string]string
		layout  func(t *testing.T, home, xdg string)
		want    func(home, xdg string) string
		profile string
	}{
		{
			name: "the default profile under HOME",
			env:  func(home, xdg string) map[string]string { return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg} },
			want: func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp")) },
		},
		{
			name:   "the default profile under an XDG data root that exists",
			env:    func(home, xdg string) map[string]string { return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg} },
			layout: func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:   func(_, xdg string) string { return manifestAt(filepath.Join(xdg, "omp")) },
		},
		{
			name: "a named profile stays under HOME while its own XDG directory does not exist",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": "work"}
			},
			layout:  func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp")) },
			want:    func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp", "profiles", "work")) },
			profile: "work",
		},
		{
			name: "a named profile whose XDG directory exists",
			env: func(home, xdg string) map[string]string {
				return map[string]string{"HOME": home, "XDG_DATA_HOME": xdg, "OMP_PROFILE": " work "}
			},
			layout:  func(t *testing.T, _, xdg string) { mkdir(t, filepath.Join(xdg, "omp", "profiles", "work")) },
			want:    func(_, xdg string) string { return manifestAt(filepath.Join(xdg, "omp", "profiles", "work")) },
			profile: "work",
		},
		{
			name: "PI_PROFILE when OMP_PROFILE is unset",
			env: func(home, _ string) map[string]string {
				return map[string]string{"HOME": home, "PI_PROFILE": "legacy"}
			},
			want:    func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp", "profiles", "legacy")) },
			profile: "legacy",
		},
		{
			name: "an empty OMP_PROFILE is the default profile, whatever PI_PROFILE says",
			env: func(home, _ string) map[string]string {
				return map[string]string{"HOME": home, "OMP_PROFILE": "", "PI_PROFILE": "legacy"}
			},
			want: func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp")) },
		},
		{
			name: "OMP_PROFILE=default is the default profile",
			env: func(home, _ string) map[string]string {
				return map[string]string{"HOME": home, "OMP_PROFILE": "default"}
			},
			want: func(home, _ string) string { return manifestAt(filepath.Join(home, ".omp")) },
		},
		{
			name: "no HOME is the account's home directory",
			env:  func(string, string) map[string]string { return map[string]string{} },
			want: func(string, string) string { return manifestAt(filepath.Join(passwdHome, ".omp")) },
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			home, xdg := t.TempDir(), t.TempDir()
			if testCase.layout != nil {
				testCase.layout(t, home, xdg)
			}
			got, profile, err := pluginManifestPath(testCase.env(home, xdg))
			if err != nil {
				t.Fatalf("pluginManifestPath: %v", err)
			}
			if want := testCase.want(home, xdg); got != want {
				t.Errorf("manifest = %s, want %s", got, want)
			}
			if profile != testCase.profile {
				t.Errorf("profile = %q, want %q", profile, testCase.profile)
			}
		})
	}
}

// Oh My Pi refuses a profile name it cannot use, and so does the gate, naming it.
func TestPluginManifestPathRefusesAProfileOhMyPiRefuses(t *testing.T) {
	for _, name := range []string{"Work", "..", "work.", "-work", "con", "nul.txt"} {
		_, _, err := pluginManifestPath(map[string]string{"HOME": t.TempDir(), "OMP_PROFILE": name})
		if err == nil || !strings.Contains(err.Error(), `Invalid OMP profile "`+name+`"`) {
			t.Errorf("OMP_PROFILE=%q: err = %v, want the invalid-profile refusal naming it", name, err)
		}
	}
}

// fakeOmp is an `omp` that answers the load probe by a plan, one step per attempt (the last step
// repeats), and records each attempt's argv, environment, and the probe file it was handed.
type fakeOmp struct {
	dir, path, prefix string
}

func newFakeOmp(t *testing.T, plan ...string) fakeOmp {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plan"), []byte(strings.Join(plan, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("write the plan: %v", err)
	}
	script := `#!/bin/sh
dir=` + dir + `
n=$(( $(cat "$dir/count" 2>/dev/null || echo 0) + 1 ))
echo "$n" >"$dir/count"
printf '%s\n' "$*" >"$dir/argv.$n"
env >"$dir/env.$n"
cp "$3" "$dir/probe.$n"
step=$(sed -n "${n}p" "$dir/plan")
[ -n "$step" ] || step=$(tail -n 1 "$dir/plan")
# Where a real Oh My Pi reports the plugin loaded from: the real path of the legion.js the profile
# links, as import.meta.url renders it (symlinks resolved, a cache-busting query appended).
root="$HOME/.omp"
[ -n "${OMP_PROFILE:-}" ] && root="$root/profiles/$OMP_PROFILE"
installed=$(cd "$root/plugins/node_modules/@sjawhar/pi-legion-envoy" 2>/dev/null && pwd -P)
case "$step" in
yes) printf 'LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://%s/dist/legion.js?mtime=1\n' "$installed" >&2; exit 0 ;;
yes-elsewhere) printf 'LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://%s/elsewhere/dist/legion.js?mtime=1\n' "$HOME" >&2; exit 0 ;;
yes-unowned) printf 'LEGION_PLUGIN_LOADED=yes\nLEGION_PLUGIN_LOADED_FROM=file://%s/unowned/legion.js\n' "$HOME" >&2; exit 0 ;;
yes-then-die) echo LEGION_PLUGIN_LOADED=yes >&2; echo "database is locked" >&2; exit 1 ;;
no) echo LEGION_PLUGIN_LOADED=no >&2; exit 0 ;;
silent) exit 0 ;;
denied) echo "secrets: ANTHROPIC_API_KEY was denied" >&2; exit 3 ;;
hang) exec sleep 30 ;;
no-then-hang) echo LEGION_PLUGIN_LOADED=no >&2; exec sleep 30 ;;
esac
`
	path := filepath.Join(dir, "omp")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake omp: %v", err)
	}
	prefix := filepath.Join(dir, "launch-prefix")
	if err := os.WriteFile(prefix, []byte("#!/bin/sh\necho prefixed >\""+dir+"/prefixed\"\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("write the launch prefix: %v", err)
	}
	return fakeOmp{dir: dir, path: path, prefix: prefix}
}

func (f fakeOmp) attempts(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "count"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("read the attempt count: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse the attempt count: %v", err)
	}
	return n
}

func (f fakeOmp) read(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// gateUnder is a gate over a pane environment whose HOME holds a manifest declaring legion (raw
// JSON), installed as `omp plugin install` installs it — the profile's package directory a link to
// the unpacked plugin — run through f with f's launch prefix, and a log that records what it said.
// Its attempt budget is long enough for the fake to answer on a loaded box — a budget that expires
// before the fake has counted its attempt would shift its plan — and short enough that a hang
// costs little.
func gateUnder(t *testing.T, f fakeOmp, legion string) (pluginGate, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	unpacked := filepath.Join(home, "unpacked-plugin")
	writeManifest(t, filepath.Join(unpacked, "package.json"), legion)
	installed := filepath.Dir(manifestAt(filepath.Join(home, ".omp", "profiles", "gate")))
	mkdir(t, filepath.Dir(installed))
	if err := os.Symlink(unpacked, installed); err != nil {
		t.Fatalf("link the plugin into the profile: %v", err)
	}
	var logged bytes.Buffer
	return pluginGate{
		env: map[string]string{
			"HOME": home, "OMP_PROFILE": "gate", "PATH": os.Getenv("PATH"),
			"XDG_DATA_HOME": filepath.Join(home, "state", "home", ".local", "share"),
		},
		workDir:    t.TempDir(),
		invocation: f.path,
		prefix:     []string{f.prefix},
		timeout:    1500 * time.Millisecond,
		retry:      bootprobe.Retry{Initial: 10 * time.Millisecond, Max: 40 * time.Millisecond},
		contract:   3,
		log:        slog.New(slog.NewTextHandler(&logged, nil)),
	}, &logged
}

const (
	contractPrevious = `{"daemonApiVersion":8,"goDaemonApiVersion":2}`
	contractCurrent  = `{"daemonApiVersion":8,"goDaemonApiVersion":3}`
)

// A manifest that declares another contract — or none, or one that is not a number — is refused
// naming the manifest, the package version, and both numbers, before any Oh My Pi is run.
func TestTheGateRefusesAPluginOfAnotherContractBeforeRunningOhMyPi(t *testing.T) {
	for _, testCase := range []struct {
		name, legion, declared string
	}{
		{"another number", contractPrevious, "speaks Go daemon API contract 2"},
		{"no Go contract", `{"daemonApiVersion":8}`, "speaks Go daemon API contract none"},
		{"no legion member", "", "speaks Go daemon API contract none"},
		{"a string", `{"goDaemonApiVersion":"1"}`, `speaks Go daemon API contract "1"`},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newFakeOmp(t, "yes")
			gate, _ := gateUnder(t, f, testCase.legion)

			err := gate.verify(context.Background())

			if err == nil {
				t.Fatal("the gate passed a plugin of another contract")
			}
			manifest := manifestAt(filepath.Join(gate.env["HOME"], ".omp", "profiles", "gate"))
			for _, want := range []string{manifest, "(package 1.57.0)", testCase.declared, "this daemon requires 3", "OMP profile gate"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if n := f.attempts(t); n != 0 {
				t.Errorf("Oh My Pi ran %d times for a plugin the manifest already refused", n)
			}
		})
	}
}

func TestTheGateRefusesThePreviousGoPluginContract(t *testing.T) {
	f := newFakeOmp(t, "yes")
	gate, _ := gateUnder(t, f, contractPrevious)

	err := gate.verify(context.Background())

	if err == nil {
		t.Fatal("the gate passed a plugin declaring the previous Go daemon contract")
	}
	for _, want := range []string{"speaks Go daemon API contract 2", "this daemon requires 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if n := f.attempts(t); n != 0 {
		t.Errorf("Oh My Pi ran %d times after the contract gate refused", n)
	}
}

func TestTheGateRefusesAManifestItCannotRead(t *testing.T) {
	f := newFakeOmp(t, "yes")
	gate, _ := gateUnder(t, f, contractCurrent)
	manifest := manifestAt(filepath.Join(gate.env["HOME"], ".omp", "profiles", "gate"))
	if err := os.WriteFile(manifest, []byte("{not json"), 0o644); err != nil {
		t.Fatalf("corrupt the manifest: %v", err)
	}
	missing := manifestAt(filepath.Join(gate.env["HOME"], ".omp", "profiles", "absent"))

	for name, env := range map[string]string{"absent": missing, "gate": manifest} {
		gate.env["OMP_PROFILE"] = name
		err := gate.verify(context.Background())
		if err == nil || !strings.Contains(err.Error(), "pi-legion-envoy manifest at "+env+" could not be read") {
			t.Errorf("profile %s: err = %v, want the unreadable-manifest refusal naming %s", name, err, env)
		}
	}
	if n := f.attempts(t); n != 0 {
		t.Errorf("Oh My Pi ran %d times with no manifest to hold it to", n)
	}
}

// The load probe is the embedded probe extension handed to `omp models`, through the launch
// prefix, under the pane's environment and nothing of the daemon's own.
func TestTheLoadProbeRunsWhatAPaneRunsAndPassesOnTheLoadedMarker(t *testing.T) {
	f := newFakeOmp(t, "yes")
	gate, _ := gateUnder(t, f, contractCurrent)
	t.Setenv("LEGION_DAEMON_ONLY_SECRET", "the daemon's own")

	if err := gate.verify(context.Background()); err != nil {
		t.Fatalf("the gate refused a loaded plugin of its contract: %v", err)
	}

	if n := f.attempts(t); n != 1 {
		t.Fatalf("Oh My Pi ran %d times, want once", n)
	}
	argv := strings.Fields(f.read(t, "argv.1"))
	if len(argv) != 4 || argv[0] != "models" || argv[1] != "--extension" || argv[3] != "--json" {
		t.Errorf("omp argv = %q, want models --extension <probe> --json", argv)
	}
	if got := f.read(t, "probe.1"); got != string(pluginLoadProbe) {
		t.Errorf("omp was handed %q, want the embedded load probe", got)
	}
	if _, err := os.Stat(argv[2]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the probe file %s outlived the probe: %v", argv[2], err)
	}
	if f.read(t, "prefixed") == "" {
		t.Error("the probe did not run through the launch prefix")
	}
	env := f.read(t, "env.1")
	for name, value := range gate.env {
		if name != "PATH" && !strings.Contains(env, name+"="+value+"\n") {
			t.Errorf("the probe's environment lacks the pane's %s=%s", name, value)
		}
	}
	if strings.Contains(env, "LEGION_DAEMON_ONLY_SECRET") {
		t.Error("the probe ran with a variable of the daemon's own environment")
	}
	if info, err := os.Stat(gate.env["XDG_DATA_HOME"]); err != nil || !info.IsDir() {
		t.Errorf("the pane's XDG_DATA_HOME was not created before the probe: %v", err)
	}
}

// The plugin a pane loads must be the one whose manifest the contract probe read. A launch prefix
// that sets its own OMP_PROFILE — or a dotenv file Oh My Pi reads — can put a pane's Oh My Pi on
// another plugin root than the pane environment names, and the contract probe would have vouched
// for a plugin no pane runs.
func TestTheLoadProbeRefusesAPluginLoadedFromAnotherRoot(t *testing.T) {
	for _, testCase := range []struct {
		step string
		want []string
	}{
		{"yes-elsewhere", []string{
			"pi-legion-envoy loads in a pane from ", filepath.Join("elsewhere", "package.json"),
			"but the manifest this gate held to Go daemon API contract 3 is ",
			"OMP profile gate", "launch prefix",
		}},
		{"yes-unowned", []string{"no @sjawhar/pi-legion-envoy package.json above", filepath.Join("unowned", "legion.js")}},
	} {
		t.Run(testCase.step, func(t *testing.T) {
			f := newFakeOmp(t, testCase.step)
			gate, _ := gateUnder(t, f, contractCurrent)
			writeManifest(t, filepath.Join(gate.env["HOME"], "elsewhere", "package.json"), contractCurrent)

			err := gate.verify(context.Background())

			if err == nil {
				t.Fatal("the gate passed a plugin loaded from a root it did not read")
			}
			for _, want := range testCase.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}
			if n := f.attempts(t); n != 1 {
				t.Errorf("Oh My Pi ran %d times for a definitive answer, want once", n)
			}
		})
	}
}

// A probe that ran and did not see the plugin load is an answer, not a failure to retry: the
// plugin is installed and disabled, or not registered.
func TestTheLoadProbeRefusesAPluginOhMyPiDidNotLoad(t *testing.T) {
	for _, step := range []string{"no", "silent", "no-then-hang"} {
		t.Run(step, func(t *testing.T) {
			f := newFakeOmp(t, step)
			gate, _ := gateUnder(t, f, contractCurrent)

			err := gate.verify(context.Background())

			want := "pi-legion-envoy 1.57.0 is installed but not loaded by omp (disabled or unregistered): run OMP_PROFILE=gate omp plugin list"
			if err == nil || err.Error() != want {
				t.Fatalf("err = %v, want %q", err, want)
			}
			if n := f.attempts(t); n != 1 {
				t.Errorf("Oh My Pi ran %d times for a definitive answer, want once", n)
			}
		})
	}
}

// A launch that fails before Oh My Pi says anything — the launch prefix refusing, a binary that is
// not Oh My Pi — is its own refusal, with the command and what it printed.
func TestTheLoadProbeRefusesALaunchThatFailsBeforeOhMyPi(t *testing.T) {
	f := newFakeOmp(t, "denied")
	gate, _ := gateUnder(t, f, contractCurrent)

	err := gate.verify(context.Background())

	if err == nil {
		t.Fatal("the gate passed a launch that failed")
	}
	for _, want := range []string{"OMP launch probe failed (exit 3) for launch command", f.prefix + " " + f.path, "secrets: ANTHROPIC_API_KEY was denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if n := f.attempts(t); n != 1 {
		t.Errorf("Oh My Pi ran %d times for a definitive failure, want once", n)
	}
}

// Oh My Pi dying after it loaded the plugin, or a probe the budget cut off before it answered, says
// nothing about the plugin: the gate waits it out and asks again.
func TestTheLoadProbeRetriesWhatSaysNothingAboutThePlugin(t *testing.T) {
	for _, step := range []string{"yes-then-die", "hang"} {
		t.Run(step, func(t *testing.T) {
			f := newFakeOmp(t, step, step, "yes")
			gate, logged := gateUnder(t, f, contractCurrent)

			if err := gate.verify(context.Background()); err != nil {
				t.Fatalf("the gate refused after transient failures: %v", err)
			}
			if n := f.attempts(t); n != 3 {
				t.Errorf("Oh My Pi ran %d times, want two transient failures and a pass", n)
			}
			if got := strings.Count(logged.String(), "failed transiently"); got != 2 {
				t.Errorf("the gate logged %d transient failures, want 2:\n%s", got, logged.String())
			}
		})
	}
}

// A daemon stopped while its gate waits kills the probe it is running and starts no other.
func TestTheLoadProbeStopsWithTheDaemon(t *testing.T) {
	f := newFakeOmp(t, "hang")
	gate, _ := gateUnder(t, f, contractCurrent)
	gate.timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)

	started := time.Now()
	err := gate.verify(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the daemon's cancellation", err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("the gate took %s to stop", elapsed)
	}
	if n := f.attempts(t); n != 1 {
		t.Errorf("Oh My Pi ran %d times, want the one the stop interrupted", n)
	}
}

// The gate runs under the environment a pane gets from the daemon's own (HOME and OMP_PROFILE here)
// before the daemon boots: a refusal records no boot, and a daemon stopped while the gate waits
// stops cleanly, having served nothing.
func TestRunGatesThePluginUnderThePaneEnvironmentBeforeItBoots(t *testing.T) {
	for _, testCase := range []struct {
		name, legion, step, want string
	}{
		{"another contract", `{"goDaemonApiVersion":1}`, "yes", "speaks Go daemon API contract 1"},
		{"not loaded", contractCurrent, "no", "is installed but not loaded by omp"},
		{"stopped while the probe runs", contractCurrent, "hang", ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := testConfig(t)
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("OMP_PROFILE", "gate")
			writeManifest(t, manifestAt(filepath.Join(home, ".omp", "profiles", "gate")), testCase.legion)
			f := newFakeOmp(t, testCase.step)
			o := fakeRuntime(fake.NewRuntime(), &built{})
			o.runtime = nil
			o.getenv = func(name string) string {
				if name == "LEGION_OMP_PATH" {
					return f.path
				}
				return ""
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if testCase.want == "" {
				time.AfterFunc(300*time.Millisecond, cancel)
			}

			err := run(ctx, cfg, quietLogger(), o)

			if testCase.want == "" && err != nil {
				t.Fatalf("run stopped during its gate = %v, want a clean stop", err)
			}
			if testCase.want != "" && (err == nil || !strings.Contains(err.Error(), testCase.want)) {
				t.Fatalf("run = %v, want the gate's refusal naming %q", err, testCase.want)
			}
			if count, _ := boots(t, cfg); count != 0 {
				t.Fatalf("boots after a start the gate did not pass = %d, want 0", count)
			}
		})
	}
}
