package sandbox

import (
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
)

// testPlugins is the pair an installing launch's spec carries in these tests.
func testPlugins() *runtime.PluginInstall {
	return &runtime.PluginInstall{PiLegion: "9.7.0", PiEnvoy: "9.6.1", Contract: 24}
}

// An installing launch's agent argv (launcherCommand) becomes `/bin/sh -c '<script>' legion-
// plugins <agent argv…>`: the agent argv rides after the script as legion-plugins's own positional
// parameters, never quoted into the script, and the script names both sources, the generation
// directory under the role's state home, and the contract, ends `exec "$@"`, and the agent argv it
// wraps carries the two installed --extension roots rather than the image's. The install carries
// `--backend=copyfile`, which keeps bun from hardlinking the installed plugin files out of the
// role's bun install cache: the cache and the generation directory share the issue's volume, and a
// hardlinked install is one inode with the cache's copy, so an in-place edit of either corrupts
// the other for the life of that volume. The rest of what it pins is the script's answer to the
// one race that was measured: six concurrent installs of the pair into six directories against one
// empty shared cache leave several of the six failing with `ENOENT: failed copying files from
// cache to destination`, where the same six against the cache they just filled all succeed. So the
// script reads BUN_INSTALL_CACHE_DIR with no default, which leaves `set -u` to abort if the
// runtime ever stops setting it, makes that directory (mkdir -p) so the lock's inode exists before
// anything fills the cache, takes /usr/bin/flock on it with a bounded wait (installLockWait, a
// third of the registration deadline the runtime's own settings give, since an unbounded one
// would charge a stalled holder to every role of the pod), and runs the install twice before the
// failure branch. The expected wait is built from that derivation over the options this spec
// configured, not written here as a number.
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
	if len(agent) < 4 || agent[0] != "/bin/sh" || agent[1] != "-c" || agent[3] != "legion-plugins" {
		t.Fatalf("an installing launch's agent command is %q, want /bin/sh -c <script> legion-plugins ...", agent)
	}
	script := agent[2]
	stateHome := roleStateHome(claim.RoleTester)
	genDir := pluginGenDir(stateHome, spec.Generation)
	for _, want := range []string{
		spec.Plugins.PiLegion, spec.Plugins.PiEnvoy, genDir, "/g*",
		"--backend=copyfile", "exec \"$@\"", "} 1>&2", "set -eu",
		"cache=\"$BUN_INSTALL_CACHE_DIR\"", "mkdir -p -- \"$cache\"",
		fmt.Sprintf("/usr/bin/flock -w %d -- \"$cache\" /usr/local/bin/bun add", installLockWait(opts.BootTimeout, opts.RegistrationIntervals)),
		"legion_install || legion_install",
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
	script := installScript("/home/legion/.local/state/tester", 3, plugins, installLockWait(2*time.Second, 3))
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

// bunInstallArgs runs the rendered install script under sh with a stand-in bun at the script's
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
	script := installScript(filepath.Join(dir, "state"), 3, plugins, installLockWait(2*time.Second, 3))
	if !strings.Contains(script, "/usr/local/bin/bun") {
		t.Fatalf("the install script does not call bun by the image path:\n%s", script)
	}
	script = strings.Replace(script, "/usr/local/bin/bun", stub, 1)
	cmd := exec.Command("sh", "-c", script, "legion-plugins", "true")
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

// installArgv returns agent unchanged when plugins is nil: a plain launch's argv is never wrapped.
func TestInstallArgvIsAgentUnchangedWithoutPlugins(t *testing.T) {
	agent := []string{"/opt/omp/bin/omp", "--mode", "rpc"}
	got := installArgv("/home/legion/.local/state/tester", 1, nil, agent, installLockWait(2*time.Second, 3))
	if !slices.Equal(got, agent) {
		t.Fatalf("installArgv(nil) = %q, want %q unchanged", got, agent)
	}
}

// installLockProbe runs the rendered install script twice at once, each for its own role state
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
		script := installScript(filepath.Join(dir, "state-"+id), 3, testPlugins(), installLockWait(2*time.Second, 3))
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
		cmd := exec.Command("sh", "-c", script, "legion-plugins", "true")
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
