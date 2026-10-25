package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/shellprefix"
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
// installed --extension roots rather than the image's. The install carries `--backend=copyfile`,
// which keeps bun from hardlinking the installed plugin files out of the role's bun install
// cache: the cache and the generation directory share the issue's volume, and a hardlinked
// install is one inode with the cache's copy, so an in-place edit of either corrupts the other
// for the life of that volume. The rest of what it pins is the script's answer to the one race
// that was measured: six concurrent installs of the pair into six directories against one empty
// shared cache leave several of the six failing with `ENOENT: failed copying files from cache to
// destination`, where the same six against the cache they just filled all succeed. So the script
// reads BUN_INSTALL_CACHE_DIR with no default, which leaves `set -u` to abort if the runtime ever
// stops setting it, makes that directory (mkdir -p) so the lock's inode exists before anything
// fills the cache, takes /usr/bin/flock on it with a bounded wait (installLockWait, a third of
// the registration deadline the runtime's own settings give, since an unbounded one would charge
// a stalled holder to every role of the pod), and runs the install twice before the failure
// branch. The expected wait is built from that derivation over the options this spec configured,
// not written here as a number.
func TestAnInstallingLaunchWrapsTheAgentArgvInTheInstallScript(t *testing.T) {
	opts := goldenOptions()
	r, err := configure(opts)
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
		"--backend=copyfile", "exec \"$@\"", ") 1>&2", "set -eu",
		"cache=\"$BUN_INSTALL_CACHE_DIR\"", "mkdir -p -- \"$cache\"",
		fmt.Sprintf("/usr/bin/flock -w %d -- \"$cache\" /usr/local/bin/bun add", installLockWait(opts.BootTimeout, opts.RegistrationIntervals)),
		"legion_install || legion_install",
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
	script := startScript("/home/legion/.local/state/tester", 3, plugins, "", installLockWait(2*time.Second, 3))
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

// requireFlock skips when /usr/bin/flock is not on this machine. The rendered script names flock
// by that absolute path, so where it is missing (a developer's Mac) the install line exits 127:
// every test that runs the script would fail on the install's absence rather than on what it is
// about, and the failure would name the wrong thing. The launcher package's namespace_test.go
// takes its prerequisite skip the same way.
func requireFlock(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/usr/bin/flock"); err != nil {
		t.Skipf("the install script locks with /usr/bin/flock, which this environment lacks: %v", err)
	}
}

// bunInstallArgs runs the rendered start wrapper under sh with a stand-in bun at the script's
// own bun path and reports the arguments bun was asked to install with: the words the shell
// hands it after quoting, the one surface that decides whether the install can resolve. The
// stand-in exits non-zero, so the script stops at its install-failed line and nothing else runs.
// BUN_INSTALL_CACHE_DIR is pinned under t.TempDir() rather than inherited: the script mkdirs that
// directory and takes a real exclusive flock on it, so an inherited value — unset in CI, set in
// every role container — would have this test lock the cache the rest of the pod is installing
// out of, and would make the run's behaviour depend on where it was started.
func bunInstallArgs(t *testing.T, plugins *runtime.PluginInstall) []string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh in this environment")
	}
	requireFlock(t)
	dir := t.TempDir()
	recorded := filepath.Join(dir, "bun-args")
	stub := filepath.Join(dir, "bun")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+recorded+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	script := startScript(filepath.Join(dir, "state"), 3, plugins, "", installLockWait(2*time.Second, 3))
	if !strings.Contains(script, "/usr/local/bin/bun") {
		t.Fatalf("the install script does not call bun by the image path:\n%s", script)
	}
	script = strings.Replace(script, "/usr/local/bin/bun", stub, 1)
	cmd := exec.Command("sh", "-c", script, startName, "0", "true")
	cmd.Env = append(os.Environ(), "BUN_INSTALL_CACHE_DIR="+filepath.Join(dir, "cache"))
	out, err := cmd.CombinedOutput()
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

// startArgv returns agent unchanged with no plugins, no Claude Code directory and no setup
// command: a plain Oh My Pi launch's argv is never wrapped.
func TestStartArgvIsAgentUnchangedWithoutPluginsOrSetup(t *testing.T) {
	agent := []string{"/opt/omp/bin/omp", "--mode", "rpc"}
	for name, setup := range map[string][]string{"nil": nil, "empty": {}} {
		got := startArgv("/home/legion/.local/state/tester", 1, nil, "", setup, agent, installLockWait(2*time.Second, 3))
		if !slices.Equal(got, agent) {
			t.Fatalf("startArgv(nil plugins, %s setup) = %q, want %q unchanged", name, got, agent)
		}
	}
}

// A Claude Code launch is wrapped in the start script for one thing: its configuration directory
// is made before the agent execs. Nothing else makes it — it is on the issue's volume, which the
// image cannot prepare — and Claude Code is told it as CLAUDE_CONFIG_DIR rather than asked to
// create it. The script is run here, so the directory it makes is the one the pod would get, and
// an Oh My Pi launch of the same deployment is left unwrapped.
func TestAClaudeLaunchsStartScriptMakesItsConfigurationDirectory(t *testing.T) {
	r, err := configure(goldenOptions())
	if err != nil {
		t.Fatal(err)
	}
	spec := claudeWorkerSpec(t)
	l := launchOf(t, r, spec)
	start := launcherCommand(l, r)
	agent := start.Argv[slices.Index(start.Argv, "--")+1:]
	if len(agent) < 5 || agent[0] != "/bin/sh" || agent[1] != "-c" || agent[3] != startName || agent[4] != "0" {
		t.Fatalf("the agent command is %q, want /bin/sh -c <script> %s 0 <agent argv…>", agent, startName)
	}
	if got, want := agent[5:], l.agentArgv(r.agent); !slices.Equal(got, want) {
		t.Errorf("the wrapped agent argv is %q, want the Claude command %q", got, want)
	}
	made := "mkdir -p -- " + shellprefix.Literal(claudeConfigDir(spec.Role))
	at, execs := strings.Index(agent[2], made), strings.Index(agent[2], `exec "$@"`)
	if at < 0 || execs < at {
		t.Errorf("the start script does not make %s before its exec:\n%s", claudeConfigDir(spec.Role), agent[2])
	}
	omp := launcherCommand(launchOf(t, r, workerSpec(t)), r)
	if wrapped := omp.Argv[slices.Index(omp.Argv, "--")+1:]; wrapped[0] == "/bin/sh" {
		t.Errorf("an Oh My Pi launch of the same deployment is wrapped: %q", wrapped)
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh in this environment")
	}
	dir := filepath.Join(t.TempDir(), "state", "tester", "claude")
	script := startScript("/legion/scratch/state/tester", 1, nil, dir, installLockWait(2*time.Second, 3))
	if out, err := exec.Command("/bin/sh", "-c", script, startName, "0", "true").CombinedOutput(); err != nil {
		t.Fatalf("the start script: %v\n%s\n%s", err, out, script)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Errorf("the start script left %s as %v (%v), want the directory made", dir, info, err)
	}
}

// installLockProbe runs the rendered start wrapper twice at once, each for its own role state
// home and both against one shared BUN_INSTALL_CACHE_DIR, with a stand-in bun at the script's own
// bun path that logs `start <id>`, holds, then logs `end <id>` to one shared file — the same
// stand-in substitution bunInstallArgs makes, so what runs is the script the daemon renders and
// no production signature widens for the test. The hold is 300ms: long enough that two unlocked
// installs, which reach bun within a few milliseconds of each other, overlap every time, and
// short enough that the whole test stays near a second. Each line is one printf to a file opened
// in append mode, so the two writers cannot garble each other. unlock strips the rendered flock
// prefix, which is how the negative control runs the identical harness with no lock. The scripts
// may well fail after the install — the contract check's `node -e` has no package.json to read —
// so the exit codes are ignored and the proof is the log alone.
func installLockProbe(t *testing.T, unlock bool) []string {
	t.Helper()
	dir := t.TempDir()
	logPath, cache, stub := filepath.Join(dir, "log"), filepath.Join(dir, "cache"), filepath.Join(dir, "bun")
	body := "#!/bin/sh\n" +
		"printf 'start %s\\n' \"$LEGION_TEST_ID\" >> \"$LEGION_TEST_LOG\"\n" +
		"sleep 0.3\n" +
		"printf 'end %s\\n' \"$LEGION_TEST_ID\" >> \"$LEGION_TEST_LOG\"\n" +
		"exit 0\n"
	if err := os.WriteFile(stub, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		script := startScript(filepath.Join(dir, "state-"+id), 3, testPlugins(), "", installLockWait(2*time.Second, 3))
		script = strings.Replace(script, "/usr/local/bin/bun", stub, 1)
		if unlock {
			// Everything from flock's own path through the locked directory goes, so the control
			// strips the lock however its wait is spelled rather than only the wait rendered today.
			start := strings.Index(script, "/usr/bin/flock ")
			end := strings.Index(script, "-- \"$cache\" ")
			if start < 0 || end < start {
				t.Fatalf("the rendered script does not take /usr/bin/flock on the cache, so the control cannot strip it:\n%s", script)
			}
			script = script[:start] + script[end+len("-- \"$cache\" "):]
		}
		cmd := exec.Command("sh", "-c", script, startName, "0", "true")
		cmd.Env = append(os.Environ(),
			"BUN_INSTALL_CACHE_DIR="+cache,
			"LEGION_TEST_LOG="+logPath,
			"LEGION_TEST_ID="+id,
		)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = cmd.Wait()
		}()
	}
	wg.Wait()
	recorded, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("neither script ran the stand-in bun: %v", err)
	}
	return strings.Split(strings.TrimRight(string(recorded), "\n"), "\n")
}

// overlapped reports whether one install began while another was still running, and whether the
// log is a well-formed pair of start/end per process at all.
func overlapped(lines []string) (overlap bool, err error) {
	running := map[string]bool{}
	done := map[string]bool{}
	for _, line := range lines {
		event, id, ok := strings.Cut(line, " ")
		if !ok {
			return false, fmt.Errorf("unreadable log line %q", line)
		}
		switch event {
		case "start":
			if running[id] || done[id] {
				return false, fmt.Errorf("%q started twice", id)
			}
			if len(running) > 0 {
				overlap = true
			}
			running[id] = true
		case "end":
			if !running[id] {
				return false, fmt.Errorf("%q ended without starting", id)
			}
			delete(running, id)
			done[id] = true
		default:
			return false, fmt.Errorf("unreadable log line %q", line)
		}
	}
	if len(running) > 0 || len(done) != 2 {
		return false, fmt.Errorf("the log is not two completed installs: %q", lines)
	}
	return overlap, nil
}

// The rendered script's flock serialises the installs it renders: two of them, run at once
// against one shared bun install cache, never overlap — no process's install begins between
// another's start and its end, which is the property the cold-entry race needs and which a
// substring assertion over the script's text cannot tell from `-n` or `-w 0`. The same harness
// with the lock stripped is the negative control, in this same run: without it a pair that merely
// happened not to overlap would pass vacuously. The control is given a few attempts, since it is
// a race it must win rather than a certainty, and a control that never interleaves is a broken
// harness, failed here rather than reported green.
func TestTwoRenderedInstallsSerialiseOnTheSharedBunCache(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh in this environment")
	}
	requireFlock(t)

	locked := installLockProbe(t, false)
	overlap, err := overlapped(locked)
	if err != nil {
		t.Fatalf("the locked run's log: %v", err)
	}
	if overlap {
		t.Fatalf("two locked installs overlapped, so the lock serialises nothing: %q", locked)
	}

	const controlAttempts = 5
	for attempt := 1; attempt <= controlAttempts; attempt++ {
		unlocked := installLockProbe(t, true)
		overlap, err := overlapped(unlocked)
		if err != nil {
			t.Fatalf("the control run's log: %v", err)
		}
		if overlap {
			return
		}
		t.Logf("control attempt %d did not interleave: %q", attempt, unlocked)
	}
	t.Fatalf("the lockless control never interleaved in %d attempts: the harness proves nothing, so the locked run above passed vacuously", controlAttempts)
}

// queuedInstalls and installBudget are what the wait must be big enough for, from the feature
// rather than from the derivation: the last of a pod's six roles queues behind the five installs
// before it, and a cold install of the plugin pair measured 505 milliseconds in an issue pod of
// this cluster, so a second each is a generous budget. They are the lower bound the derivation
// exists to leave room for, and nothing in installLockWait computes them.
const (
	queuedInstalls = 5
	installBudget  = time.Second
)

// The install lock's wait is a bound in proportion to the registration deadline, at every
// configuration an operator can set rather than only at the defaults. Each case carries the wait
// computed by hand from that configuration — the arithmetic is in the case's own comment — so the
// expectation comes from somewhere other than the code under test: a derivation that returned a
// constant, or divided by something else, fails here, where a clause reading installLockWait's
// own output against installLockWait's own deadline could not see either (the pin built from the
// value cannot see the value change, which is what this test inherited and no longer does).
//
// Three further clauses say what the number has to be rather than what it is, so a future
// derivation that renders different values is still held: the wait is positive, since `-w 0` is
// byte for byte `-n` and would restore the race the lock is for; it is never more than its third
// of that configuration's deadline, so two waits and the install that follows them still fit; and
// — where the deadline leaves room for it — it covers the queue the lock itself creates, five
// serialised installs at installBudget each, which is the half of the bound the derivation is
// for and the half that was missing while the wait could shrink to a second unseen.
//
// The tiny configuration is the honest case, not an excluded one: below a deadline of about three
// seconds the one-second floor dominates, two waits cannot fit and the queue cannot be covered.
// That is deliberate — being serialised and then retired by the deadline is no worse than racing,
// since an unserialised cold fill fails the launch anyway — so the property drops those clauses
// there, conditional on the deadline rather than on the configuration's name.
func TestTheInstallLockWaitIsABoundInProportionToTheRegistrationDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legion.yaml")
	body := "project: demo\nstate_dir: /var/lib/legion\npostgres_dsn: postgres://legion@127.0.0.1:5432/legion\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadForValidation(path, func(string) string { return "" })
	if err != nil {
		t.Fatalf("LoadForValidation: %v", err)
	}
	for _, tc := range []struct {
		name      string
		boot      time.Duration
		intervals int
		want      int
	}{
		// 120s x 3 = a 360s deadline, a third of which is 120s. The daemon's own defaults, read
		// rather than written, so a change to either of them fails this case against the hand
		// arithmetic above rather than moving with it.
		{"the daemon's defaults", cfg.WorkerBootTimeout, cfg.WorkerBootRegistrationDeadlineIntervals, 120},
		// 60s x 3 = 180s, a third of which is 60s: the tuned deployment a constant 120 broke.
		{"a halved boot timeout", cfg.WorkerBootTimeout / 2, cfg.WorkerBootRegistrationDeadlineIntervals, 60},
		// 120s x 2 = 240s, a third of which is 80s: the other setting moving on its own.
		{"fewer intervals", cfg.WorkerBootTimeout, 2, 80},
		// 800ms x 3 = 2.4s, a third of which is 800ms, which is no whole second at all: the floor
		// renders 1 rather than the `-w 0` that would be `-n`.
		{"a deadline small enough that the floor bites", 800 * time.Millisecond, 3, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deadline := runtime.RegistrationDeadline(tc.boot, tc.intervals, 0)
			wait := installLockWait(tc.boot, tc.intervals)
			if wait != tc.want {
				t.Errorf("the wait at boot %s x %d intervals is %ds, want %ds: the %s deadline's third, floored at a second",
					tc.boot, tc.intervals, wait, tc.want, deadline)
			}
			if wait <= 0 {
				t.Fatalf("the wait at boot %s x %d intervals is %ds, want a positive bound: a non-positive wait renders -w 0, which is -n, and restores the cold-entry race", tc.boot, tc.intervals, wait)
			}
			if wait > 1 && time.Duration(wait)*time.Second*3 > deadline {
				t.Errorf("the wait at boot %s x %d intervals is %ds, more than its third of the %s deadline", tc.boot, tc.intervals, wait, deadline)
			}
			if deadline < 3*time.Second {
				return
			}
			if spent := time.Duration(2*wait)*time.Second + installBudget; spent > deadline {
				t.Errorf("two waits of %ds and a second of install take %s, past the %s registration deadline, so a stalled lock is retired by the deadline rather than failing the launch by its own branch", wait, spent, deadline)
			}
			if queue := queuedInstalls * installBudget; deadline >= 3*queue && time.Duration(wait)*time.Second < queue {
				t.Errorf("the wait at boot %s x %d intervals is %ds, less than the %s the queue this lock creates needs: the last of a pod's six roles waits for the %d installs before it, so a wait this short fails the launch the lock was added to save",
					tc.boot, tc.intervals, wait, queue, queuedInstalls)
			}
		})
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

// runStart renders and runs that argv. An installing run needs what the install itself needs:
// /usr/bin/flock, which the rendered script names by that path, and a BUN_INSTALL_CACHE_DIR, which
// the script reads with no default behind it. cache is that directory, for a caller that has
// something to say about what is at it; the empty string pins it under t.TempDir() here rather
// than inheriting it, so the run never locks a cache anything else is installing out of.
func runStart(t *testing.T, start, bin, bun, cache string, plugins *runtime.PluginInstall, setup, agent []string) startRun {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no /bin/sh in this environment")
	}
	if plugins != nil {
		requireFlock(t)
	}
	dir := t.TempDir()
	if cache == "" {
		cache = filepath.Join(dir, "cache")
	}
	argv := startArgv(filepath.Join(dir, "state"), 3, plugins, "", setup, agent, installLockWait(2*time.Second, 3))
	if bun != "" {
		argv[2] = strings.Replace(argv[2], "/usr/local/bin/bun", bun, 1)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = start
	cmd.Stdin = strings.NewReader("frame\n")
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BUN_INSTALL_CACHE_DIR="+cache,
	)
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
	got := runStart(t, start, dir, "", "", nil,
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
			got := runStart(t, dir, dir, "", "", nil, tc.setup, recorder(agentLog, "agent-stdout"))
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
	got := runStart(t, start, dir, filepath.Join(dir, "bun"), "", testPlugins(), record("setup"), record("agent"))
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

// An install that fails ends the wrapper where it stands: nothing of the deployment's setup
// command runs and Oh My Pi never starts on a pair that was not installed. That failure is the
// one in this wrapper that has to cross a subshell boundary — installBlock renders `( … ) 1>&2`,
// so the install's non-zero exit is the subshell's, and only `set -e` in the wrapper around it
// makes it the wrapper's own. A change that let it escape would start an agent on the wrong
// plugin pair with every other test in this file still green, which is why the behaviour is
// pinned here rather than left to the live run that first showed it.
//
// The succeeding case is the control and belongs in the same test: without it the absent setup
// and agent markers would prove only that the harness ran nothing at all.
func TestAFailingInstallNeverRunsTheSetupCommandOrTheAgent(t *testing.T) {
	plugins := testPlugins()
	failMsg := fmt.Sprintf("legion plugins: installing %s and %s failed",
		bunSpec("@sjawhar/pi-legion", plugins.PiLegion), bunSpec("@sjawhar/pi-envoy", plugins.PiEnvoy))
	for name, tc := range map[string]struct {
		bun  string
		exit int
		ran  bool
	}{
		"an install that fails":    {"#!/bin/sh\necho 'bun: boom' >&2\nexit 1\n", 1, false},
		"an install that succeeds": {"#!/bin/sh\nexit 0\n", 0, true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for file, body := range map[string]string{"bun": tc.bun, "node": "#!/bin/sh\nexit 0\n"} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			setupLog, agentLog := filepath.Join(dir, "setup.log"), filepath.Join(dir, "agent.log")
			got := runStart(t, dir, dir, filepath.Join(dir, "bun"), "", plugins,
				recorder(setupLog, "setup-stdout"), recorder(agentLog, "agent-stdout"))
			if got.exit != tc.exit {
				t.Errorf("the wrapper exited %d, want %d:\n%s", got.exit, tc.exit, got.stderr)
			}
			for what, log := range map[string]string{"setup command": setupLog, "agent": agentLog} {
				_, err := os.Stat(log)
				switch {
				case tc.ran && err != nil:
					t.Errorf("the %s did not run (%v), so the absence the failing case asserts would prove nothing", what, err)
				case !tc.ran && !errors.Is(err, os.ErrNotExist):
					t.Errorf("the %s ran after a failed install (%v): the install's subshell exit did not end the wrapper", what, err)
				}
			}
			switch {
			case tc.ran:
				return
			case !strings.Contains(got.stderr, failMsg):
				t.Errorf("stderr is %q, want the install's own failure %q", got.stderr, failMsg)
			case got.stdout != "":
				t.Errorf("the wrapper wrote %q to stdout, the shim's frame stream", got.stdout)
			}
		})
	}
}

// An install makes its own cache directory on every attempt, and makes it only when what is at
// the path is not one already. `/usr/bin/flock -w N -- <missing path> <cmd>` creates that path as
// a 0-byte *regular file* and exits 0, so an operator reclaiming the issue's volume under a
// running install leaves a file where the shared cache belongs; with the mkdir made once outside
// the retried function, every later installing launch of that issue then dies at `mkdir: …
// Already exists` under `set -eu`, before Oh My Pi starts, until a person removes the file by
// hand — on a volume, where no CI run ever looks. The guard is `[ -d "$cache" ] || rm -f --
// "$cache"` ahead of the mkdir, inside legion_install, and it has to stay exactly that narrow:
// a cache that is a directory, or a symlink to one, is left as it stands, since emptying a filled
// cache charges every role of the pod a cold install, which is the race the lock above it exists
// to survive.
//
// Every case asserts observable state — the wrapper's exit, the file type at the cache path, the
// bytes still inside it — rather than a line of the rendered script. The substring assertions in
// TestAnInstallingLaunchWrapsTheAgentArgvInTheInstallScript match the mkdir wherever in the block
// it sits, which is how the one-way failure shipped; what runs here is the script the daemon
// renders, with stand-ins for bun and node, never a retyped copy of it.
func TestAnInstallRemakesAPoisonedCacheAndSparesARealOne(t *testing.T) {
	// cached is what a filled cache holds; every byte of it has to outlive the install.
	cached := map[string]string{"entry.txt": "cached bytes", "nested.json": "{\"v\":1}"}
	fill := func(t *testing.T, dir string) string {
		t.Helper()
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for name, body := range cached {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	for name, tc := range map[string]struct {
		// prepare leaves the cache path as this case finds it and returns the directory whose
		// contents must survive, or "" when the case starts with no cache worth keeping.
		prepare func(t *testing.T, cache string) string
		// symlink is whether the cache path itself must still be a symlink afterwards; every
		// other case wants a directory there.
		symlink bool
	}{
		"a regular file an interrupted flock left behind": {prepare: func(t *testing.T, cache string) string {
			if err := os.WriteFile(cache, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			return ""
		}},
		"a cache already filled": {prepare: func(t *testing.T, cache string) string {
			return fill(t, cache)
		}},
		"a symlink to a filled cache": {prepare: func(t *testing.T, cache string) string {
			target := fill(t, cache+"-target")
			if err := os.Symlink(target, cache); err != nil {
				t.Fatal(err)
			}
			return target
		}, symlink: true},
		"no cache at all": {prepare: func(t *testing.T, cache string) string { return "" }},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, file := range []string{"bun", "node"} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			cache := filepath.Join(dir, "cache")
			kept := tc.prepare(t, cache)
			agentLog := filepath.Join(dir, "agent.log")
			got := runStart(t, dir, dir, filepath.Join(dir, "bun"), cache, testPlugins(),
				nil, recorder(agentLog, "agent-stdout"))
			if got.exit != 0 {
				t.Fatalf("the wrapper exited %d with %s at the cache path, so every installing launch of this issue stays dead until a person clears it:\n%s", got.exit, name, got.stderr)
			}
			if _, err := os.Stat(agentLog); err != nil {
				t.Errorf("Oh My Pi never started (%v), so this case says nothing about the install", err)
			}
			info, err := os.Lstat(cache)
			if err != nil {
				t.Fatalf("the cache path is gone after the install: %v", err)
			}
			switch {
			case tc.symlink && info.Mode()&os.ModeSymlink == 0:
				t.Errorf("the cache path is %s, want the symlink it was: the guard reaches past a cache it should leave alone", info.Mode().Type())
			case !tc.symlink && !info.IsDir():
				t.Errorf("the cache path is %s, want a directory: flock's lock inode is not a cache, and the next install dies on it", info.Mode().Type())
			}
			if kept == "" {
				return
			}
			for file, want := range cached {
				body, err := os.ReadFile(filepath.Join(kept, file))
				if err != nil {
					t.Errorf("the install took %s out of the cache (%v): every role of the pod pays a cold install for that", file, err)
					continue
				}
				if string(body) != want {
					t.Errorf("cached %s is %q, want the %q it held", file, body, want)
				}
			}
		})
	}
}

// shellQuote is value as one single-quoted shell word.
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'" }
