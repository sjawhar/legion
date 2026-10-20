package sandbox

import (
	"errors"
	"fmt"
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

// An installing launch's agent argv (launcherCommand) becomes `/bin/sh -c '<script>' legion-start
// 0 <agent argv…>`: with no setup command the count is 0, the agent argv rides after it as
// legion-start's own positional parameters, never quoted into the script, and the script names
// both sources, the generation directory under the role's state home, and the contract, runs the
// install in a subshell on stderr, ends `exec "$@"`, and the agent argv it wraps carries the two
// installed --extension roots rather than the image's.
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
	if len(agent) < 5 || agent[0] != "/bin/sh" || agent[1] != "-c" || agent[3] != startName || agent[4] != "0" {
		t.Fatalf("an installing launch's agent command is %q, want /bin/sh -c <script> %s 0 ...", agent, startName)
	}
	script := agent[2]
	stateHome := roleStateHome(claim.RoleTester)
	genDir := pluginGenDir(stateHome, spec.Generation)
	for _, want := range []string{
		spec.Plugins.PiLegion, spec.Plugins.PiEnvoy, genDir, "/g*",
		"exec \"$@\"", ") 1>&2", "set -eu",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script does not name %q:\n%s", want, script)
		}
	}
	rest := agent[5:]
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
	script := startScript("/home/legion/.local/state/tester", 3, plugins)
	// The two sources carry a space and a single quote; verified below (sh -n) rather than as a
	// literal substring, since the quoting itself breaks one.
	if !strings.Contains(script, "pi-legion") || !strings.Contains(script, "another") {
		t.Fatalf("script does not carry the sources it quotes:\n%s", script)
	}
	cmd := exec.Command("sh", "-n", "-c", script, startName)
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
	script := startScript(filepath.Join(dir, "state"), 3, plugins)
	if !strings.Contains(script, "/usr/local/bin/bun") {
		t.Fatalf("the install script does not call bun by the image path:\n%s", script)
	}
	script = strings.Replace(script, "/usr/local/bin/bun", stub, 1)
	out, err := exec.Command("sh", "-c", script, startName, "0", "true").CombinedOutput()
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

// startArgv returns agent unchanged with neither plugins nor a setup command: a plain launch's
// argv is never wrapped.
func TestStartArgvIsAgentUnchangedWithoutPluginsOrSetup(t *testing.T) {
	agent := []string{"/opt/omp/bin/omp", "--mode", "rpc"}
	for name, setup := range map[string][]string{"nil": nil, "empty": {}} {
		if got := startArgv("/home/legion/.local/state/tester", 1, nil, setup, agent); !slices.Equal(got, agent) {
			t.Fatalf("startArgv(nil plugins, %s setup) = %q, want %q unchanged", name, got, agent)
		}
	}
}

// testSetup is a deployment's setup command as these tests configure it: one shell command, as the
// loader admits runtime.kubernetes.setup.
var testSetup = []string{"/bin/sh", "-c", "omp plugin marketplace add acme/agent-skills"}

// A deployment's setup command wraps every start of every role, a fresh one's, a resumed one's
// and the controller's: the agent command becomes `/bin/sh -c '<script>' legion-start <n>
// <setup…> <agent argv…>`, the setup words and the agent argv riding as positional parameters
// after their count, never quoted into the script, and the agent argv exactly the plain launch's,
// the image's two extension roots included, since setup installs nothing of Legion's.
func TestASetupCommandWrapsEveryStartOfEveryRole(t *testing.T) {
	opts := goldenOptions()
	opts.Setup = testSetup
	r, err := configure(opts)
	if err != nil {
		t.Fatal(err)
	}
	resume := workerSpec(t)
	resume.Generation, resume.BootToken, resume.ResumeSessionFile = 2, "boot-g2", resumeSession
	for name, spec := range map[string]runtime.SpawnSpec{"a fresh role": workerSpec(t), "a resumed role": resume, "the controller": controllerSpec(t)} {
		t.Run(name, func(t *testing.T) {
			l, err := r.prepare(spec)
			if err != nil {
				t.Fatal(err)
			}
			start := launcherCommand(l, r)
			agent := start.Argv[slices.Index(start.Argv, "--")+1:]
			if len(agent) < 5 || agent[0] != "/bin/sh" || agent[1] != "-c" || agent[3] != startName || agent[4] != "3" {
				t.Fatalf("the agent command is %q, want /bin/sh -c <script> %s 3 <setup…> <agent…>", agent, startName)
			}
			if got := agent[5:8]; !slices.Equal(got, testSetup) {
				t.Errorf("the setup words are %q, want %q", got, testSetup)
			}
			if got, want := agent[8:], l.agentArgv(r.agent); !slices.Equal(got, want) {
				t.Errorf("the wrapped agent argv is %q, want the plain launch's %q", got, want)
			}
			if script := agent[2]; strings.Contains(script, "acme/agent-skills") || strings.Contains(script, "bun add") {
				t.Errorf("the script quotes the setup command or installs plugins it was not asked to:\n%s", script)
			}
		})
	}
}

// startRun is one run of the start wrapper as a role's shim starts it: the argv startArgv renders,
// run under /bin/sh from start, its own directory, with stdin holding what the shim would write
// the agent, bin first on PATH, and bun's image path replaced by bun when bun is set.
type startRun struct {
	exit           int
	stdout, stderr string
}

func runStart(t *testing.T, start, bin, bun string, plugins *runtime.PluginInstall, setup, agent []string) startRun {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh in this environment")
	}
	argv := startArgv(filepath.Join(t.TempDir(), "state"), 3, plugins, setup, agent)
	if bun != "" {
		argv[2] = strings.Replace(argv[2], "/usr/local/bin/bun", bun, 1)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = start
	cmd.Stdin = strings.NewReader("frame\n")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return startRun{0, stdout.String(), stderr.String()}
	case errors.As(err, &exit):
		return startRun{exit.ExitCode(), stdout.String(), stderr.String()}
	}
	t.Fatalf("run the start wrapper: %v", err)
	return startRun{}
}

// recorder is a command that appends its working directory, each of its arguments, and what it
// read on stdin to the file at log, one per line, then prints out on stdout: the stand-in for the
// setup command and for Oh My Pi.
func recorder(log, out string, args ...string) []string {
	script := `log=$1; out=$2; shift 2; { pwd; printf 'arg %s\n' "$@"; printf 'stdin %s\n' "$(cat)"; } >>"$log"; echo "$out"`
	return append([]string{"/bin/sh", "-c", script, "recorder", log, out}, args...)
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(body), "\n"), "\n")
}

// A setup command that succeeds runs in the directory the wrapper started in, with every word as
// the daemon handed it — spaces, quotes, `$` and globs reach it unexpanded — with nothing on stdin
// and its stdout on stderr, so the shim's RPC frames reach the agent alone and nothing of the setup
// enters the frame stream the shim reads; then Oh My Pi starts there with its own argv, and stdin.
func TestASucceedingSetupCommandRunsBeforeTheAgentInTheStartDirectory(t *testing.T) {
	dir := t.TempDir()
	start := filepath.Join(dir, "workspace")
	if err := os.Mkdir(start, 0o700); err != nil {
		t.Fatal(err)
	}
	setupLog, agentLog := filepath.Join(dir, "setup.log"), filepath.Join(dir, "agent.log")
	words := []string{"a b", "it's", "$HOME", "*", "--"}
	got := runStart(t, start, dir, "", nil,
		recorder(setupLog, "setup-stdout", words...),
		recorder(agentLog, "agent-stdout", "--mode", "rpc", "$PATH"))
	if got.exit != 0 {
		t.Fatalf("the wrapper exited %d:\n%s", got.exit, got.stderr)
	}
	want := append([]string{start}, "arg a b", "arg it's", "arg $HOME", "arg *", "arg --", "stdin ")
	if lines := readLines(t, setupLog); !slices.Equal(lines, want) {
		t.Errorf("the setup command recorded %q, want %q", lines, want)
	}
	if lines, want := readLines(t, agentLog), []string{start, "arg --mode", "arg rpc", "arg $PATH", "stdin frame"}; !slices.Equal(lines, want) {
		t.Errorf("the agent recorded %q, want %q", lines, want)
	}
	if got.stdout != "agent-stdout\n" {
		t.Errorf("the wrapper's stdout is %q, want the agent's alone", got.stdout)
	}
	if !strings.Contains(got.stderr, "setup-stdout") {
		t.Errorf("the setup command's stdout is not on stderr: %q", got.stderr)
	}
}

// A setup command that fails, or cannot be run at all, ends the wrapper with its status before Oh
// My Pi starts: the agent never runs, the wrapper's exit is the command's, and stderr — the role's
// log — carries the command's own output and the line saying Oh My Pi was not started.
func TestAFailingSetupCommandNeverStartsTheAgent(t *testing.T) {
	for name, tc := range map[string]struct {
		setup  []string
		exit   int
		output string
	}{
		"a non-zero exit": {[]string{"/bin/sh", "-c", "echo boom >&2; exit 7"}, 7, "boom"},
		"no such command": {[]string{"/nonexistent/legion-setup"}, 127, "/nonexistent/legion-setup"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			agentLog := filepath.Join(dir, "agent.log")
			got := runStart(t, dir, dir, "", nil, tc.setup, recorder(agentLog, "agent-stdout"))
			if got.exit != tc.exit {
				t.Errorf("the wrapper exited %d, want %d:\n%s", got.exit, tc.exit, got.stderr)
			}
			if _, err := os.Stat(agentLog); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the agent ran (%v)", err)
			}
			if got.stdout != "" {
				t.Errorf("the wrapper wrote %q to stdout, the shim's frame stream", got.stdout)
			}
			line := fmt.Sprintf("legion setup: runtime.kubernetes.setup exited %d; not starting Oh My Pi", tc.exit)
			if !strings.Contains(got.stderr, tc.output) || !strings.Contains(got.stderr, line) {
				t.Errorf("stderr is %q, want the command's %q and %q", got.stderr, tc.output, line)
			}
		})
	}
}

// An installing launch with a setup command installs the pair first, in its generation
// directory, then runs the setup command and then the agent, both in the directory the wrapper
// started in: the install's cd into its generation directory ends with the install. bun and node
// are stand-ins that record their order and directory.
func TestAnInstallRunsBeforeTheSetupCommandAndLeavesTheStartDirectory(t *testing.T) {
	dir := t.TempDir()
	start := filepath.Join(dir, "workspace")
	if err := os.Mkdir(start, 0o700); err != nil {
		t.Fatal(err)
	}
	order := filepath.Join(dir, "order")
	stub := "#!/bin/sh\necho \"$(basename \"$0\") $(pwd)\" >>" + shellQuote(order) + "\n"
	for _, name := range []string{"bun", "node"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	record := func(what string) []string {
		return []string{"/bin/sh", "-c", `echo "$1 $(pwd)" >>"$0"`, order, what}
	}
	got := runStart(t, start, dir, filepath.Join(dir, "bun"), testPlugins(), record("setup"), record("agent"))
	if got.exit != 0 {
		t.Fatalf("the wrapper exited %d:\n%s", got.exit, got.stderr)
	}
	lines := readLines(t, order)
	if len(lines) != 4 {
		t.Fatalf("the run recorded %q, want bun, node, setup, agent", lines)
	}
	for i, want := range []string{"bun ", "node ", "setup " + start, "agent " + start} {
		if !strings.HasPrefix(lines[i], want) {
			t.Errorf("step %d is %q, want %q", i, lines[i], want)
		}
	}
	if strings.HasSuffix(lines[0], " "+start) || !strings.Contains(lines[0], "/legion-plugins/g3") {
		t.Errorf("bun ran in %q, want the generation directory", lines[0])
	}
}

// shellQuote is value as one single-quoted shell word.
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }
