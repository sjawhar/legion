package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// testPlugins is the pair an installing launch's spec carries in these tests.
func testPlugins() *runtime.PluginInstall {
	return &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
}

// An installing launch's agent argv (launcherCommand) becomes `/bin/sh -c '<script>' legion-
// plugins <agent argv…>`: the agent argv rides after the script as legion-plugins's own positional
// parameters, never quoted into the script, and the script names both sources, the generation
// directory under the role's state home, and the contract, ends `exec "$@"`, and the agent argv it
// wraps carries the two installed --extension roots rather than the image's.
func TestAnInstallingLaunchWrapsTheAgentArgvInTheInstallScript(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	spec.Plugins = testPlugins()
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	start := launcherCommand(l, r)
	i := slices.Index(start.Argv, "--")
	if i < 0 {
		t.Fatalf("argv %q carries no -- separator", start.Argv)
	}
	agent := start.Argv[i+1:]
	if len(agent) < 4 || agent[0] != "/bin/sh" || agent[1] != "-c" || agent[3] != "legion-plugins" {
		t.Fatalf("an installing launch's agent command is %q, want /bin/sh -c <script> legion-plugins ...", agent)
	}
	script := agent[2]
	stateHome := roleStateHome(claim.RoleTester)
	genDir := pluginGenDir(stateHome, spec.Generation)
	for _, want := range []string{
		spec.Plugins.PiLegion, spec.Plugins.PiEnvoy, genDir, "/g*",
		"exec \"$@\"", "} 1>&2", "set -eu",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script does not name %q:\n%s", want, script)
		}
	}
	rest := agent[4:]
	plain := l.agentArgv(r.agent)
	// The agent argv the script wraps is the same shape a plain launch's is, less its roots,
	// which name the installed pair instead of the image's.
	envoy, legion := pluginRoots(genDir)
	if !slices.Contains(rest, envoy) || !slices.Contains(rest, legion) {
		t.Fatalf("the wrapped agent argv %q names no installed root (%s, %s)", rest, envoy, legion)
	}
	if slices.Contains(rest, envoyPlugin) || slices.Contains(rest, legionPlugin) {
		t.Fatalf("the wrapped agent argv %q still names an image root", rest)
	}
	if !slices.Equal(rest, plain) {
		t.Fatalf("the wrapped agent argv %q, want the plain launch's own %q with the installed roots", rest, plain)
	}
	if !strings.Contains(script, "\"$@\"") {
		t.Fatal("the script never execs its positional parameters")
	}
}

// A plain launch (spec.Plugins nil) carries no install wrapper: the agent command starts with the
// agent's own path, naming the image's two extension roots, byte for byte what a fresh planner's
// argv names today (stage4b-sandbox-tree.sh).
func TestAPlainLaunchCarriesNoInstallWrapperAndTheImagesRoots(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := workerSpec(t)
	l, err := r.prepare(spec)
	if err != nil {
		t.Fatal(err)
	}
	start := launcherCommand(l, r)
	i := slices.Index(start.Argv, "--")
	agent := start.Argv[i+1:]
	if agent[0] == "/bin/sh" {
		t.Fatalf("a plain launch's argv %q carries an install wrapper", agent)
	}
	if !slices.Contains(agent, envoyPlugin) || !slices.Contains(agent, legionPlugin) {
		t.Fatalf("a plain launch's argv %q names no image root", agent)
	}
}

// The install script's directory, sources and contract are shell-quoted so a source with a
// single quote or a space is one word to `sh -c`: run the rendered script through `sh -n` (syntax
// check only) in this pod, which has /bin/sh.
func TestInstallScriptQuotesShellUnsafeSources(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh in this environment")
	}
	plugins := &runtime.PluginInstall{PiLegion: "/tmp/odd dir/pi-legion's.tgz", PiEnvoy: "/tmp/another 'one'.tgz", Contract: 24}
	script := installScript("/home/legion/.local/state/tester", 3, plugins)
	// The two sources carry a space and a single quote; verified below (sh -n) rather than as a
	// literal substring, since the quoting itself breaks one.
	if !strings.Contains(script, "pi-legion") || !strings.Contains(script, "another") {
		t.Fatalf("script does not carry the sources it quotes:\n%s", script)
	}
	cmd := exec.Command("sh", "-n", "-c", script, "legion-plugins")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sh -n the rendered script: %v\n%s\nscript:\n%s", err, out, script)
	}
}

// bunInstallArgs runs the rendered install script under sh with a stand-in bun at the script's
// own bun path and reports the arguments bun was asked to install with: the words the shell
// hands it after quoting, the one surface that decides whether the install can resolve. The
// stand-in exits non-zero, so the script stops at its install-failed line and nothing else runs.
func bunInstallArgs(t *testing.T, plugins *runtime.PluginInstall) []string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh in this environment")
	}
	dir := t.TempDir()
	recorded := filepath.Join(dir, "bun-args")
	stub := filepath.Join(dir, "bun")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+recorded+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	script := installScript(filepath.Join(dir, "state"), 3, plugins)
	if !strings.Contains(script, "/usr/local/bin/bun") {
		t.Fatalf("the install script does not call bun by the image path:\n%s", script)
	}
	script = strings.Replace(script, "/usr/local/bin/bun", stub, 1)
	out, err := exec.Command("sh", "-c", script, "legion-plugins", "true").CombinedOutput()
	if err == nil {
		t.Fatalf("the script ran on past a failed install:\n%s", out)
	}
	args, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatalf("the script never ran bun (%v):\n%s", err, out)
	}
	return strings.Split(strings.TrimRight(string(args), "\n"), "\n")
}

// A source that is an exact version — `runtime.kubernetes.plugins: {pi_legion: 9.7.0, …}` as
// the loader accepts it, and the pair the release links into the binary — reaches bun as the
// package at that version, `@sjawhar/pi-legion@9.7.0`: a bare `9.7.0` is a package name to
// `bun add`, which refuses it (`Package "9.7.0" with tag "latest" not found`), and the agent
// never starts (LEGION-164, the production default source).
func TestAVersionSourceIsInstalledAsThePackageAtThatVersion(t *testing.T) {
	args := bunInstallArgs(t, &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24})
	if !slices.Contains(args, "@sjawhar/pi-legion@9.7.0") || !slices.Contains(args, "@sjawhar/pi-envoy@9.6.1") {
		t.Fatalf("bun add was asked for %q, want @sjawhar/pi-legion@9.7.0 and @sjawhar/pi-envoy@9.6.1", args)
	}
	if slices.Contains(args, "9.7.0") || slices.Contains(args, "9.6.1") {
		t.Fatalf("bun add was asked for a bare version in %q, which it reads as a package name", args)
	}
}

// A tarball path or an http(s) URL source reaches bun as it is: bun resolves those by their own
// shape, and prefixing a package name would turn them into a version it cannot find.
func TestATarballOrURLSourceReachesBunVerbatim(t *testing.T) {
	legion, envoy := "/tmp/plugins/sjawhar-pi-legion-9.7.0.tgz", "https://registry.example/pi-envoy-9.6.1.tgz"
	args := bunInstallArgs(t, &runtime.PluginInstall{PiLegion: legion, PiEnvoy: envoy, Contract: 24})
	if !slices.Contains(args, legion) || !slices.Contains(args, envoy) {
		t.Fatalf("bun add was asked for %q, want %q and %q verbatim", args, legion, envoy)
	}
}

// An upper-case scheme reaches bun verbatim too: the loader already canonicalises it to lower
// case, but bunSpec's own guard is case-insensitive, so the two never disagree over what counts as
// a URL.
func TestAnUppercaseSchemeReachesBunVerbatim(t *testing.T) {
	legion := "HTTPS://cache/x.tgz"
	args := bunInstallArgs(t, &runtime.PluginInstall{PiLegion: legion, PiEnvoy: "9.6.1", Contract: 24})
	if !slices.Contains(args, legion) {
		t.Fatalf("bun add was asked for %q, want %q verbatim", args, legion)
	}
}

// installArgv returns agent unchanged when plugins is nil: a plain launch's argv is never wrapped.
func TestInstallArgvIsAgentUnchangedWithoutPlugins(t *testing.T) {
	agent := []string{"/opt/omp/bin/omp", "--mode", "rpc"}
	got := installArgv("/home/legion/.local/state/tester", 1, nil, agent)
	if !slices.Equal(got, agent) {
		t.Fatalf("installArgv(nil) = %q, want %q unchanged", got, agent)
	}
}
