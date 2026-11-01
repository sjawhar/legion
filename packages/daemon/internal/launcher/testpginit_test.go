//go:build linux

package launcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// syncBuffer is the container log a test reads while the launcher's goroutines write it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// testPGInitEnv carries testpg-init's flags to the test binary re-entered as testpg-init
// (TestTestPGInitHelper), since the test binary is not `legion`. testPGTuningEnv carries the two
// package variables a test shortens — the tick interval and the bound on one run of the script —
// because the init is a process of its own and reads its own copy of them.
const (
	testPGInitEnv   = "LEGION_TESTPG_INIT_TEST_INIT"
	testPGTuningEnv = "LEGION_TESTPG_INIT_TEST_TUNING"
)

// testPGInitHelperLaunch is testPGInitLaunch for a test binary. A binary built with -race sleeps
// atexit_sleep_ms before it exits, which `legion launcher testpg-init` never does, so the helper
// turns that sleep off and keeps whatever else GORACE asks for.
func testPGInitHelperLaunch(self string) func(TestPGInitConfig) ([]string, []string, error) {
	return func(cfg TestPGInitConfig) ([]string, []string, error) {
		body, err := json.Marshal(cfg)
		if err != nil {
			return nil, nil, err
		}
		gorace := strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0")
		return []string{self, "-test.run=^TestTestPGInitHelper$"}, []string{
			testPGInitEnv + "=" + string(body),
			testPGTuningEnv + "=" + testPGInterval.String() + "," + testPGBound.String(),
			"GORACE=" + gorace,
		}, nil
	}
}

// TestTestPGInitHelper is testpg-init itself, as `legion launcher testpg-init` runs it: the
// launcher clones it into the test server's new namespaces, and it reads the flags it would have
// been given on the command line, and the two durations the test shortened, from its environment.
func TestTestPGInitHelper(t *testing.T) {
	encoded := os.Getenv(testPGInitEnv)
	if encoded == "" {
		return
	}
	if tuning := os.Getenv(testPGTuningEnv); tuning != "" {
		interval, bound, found := strings.Cut(tuning, ",")
		parsedInterval, intervalErr := time.ParseDuration(interval)
		parsedBound, boundErr := time.ParseDuration(bound)
		if !found || intervalErr != nil || boundErr != nil {
			fmt.Fprintf(os.Stderr, "testpg-init: %s=%q is no interval,bound pair\n", testPGTuningEnv, tuning)
			os.Exit(2)
		}
		testPGInterval, testPGBound = parsedInterval, parsedBound
	}
	var cfg TestPGInitConfig
	if err := json.Unmarshal([]byte(encoded), &cfg); err != nil {
		fmt.Fprintln(os.Stderr, "testpg-init:", err)
		os.Exit(2)
	}
	cfg.Stdout, cfg.Stderr = os.Stdout, os.Stderr
	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	if err := TestPGInit(stop, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "testpg-init:", err)
		os.Exit(TestPGInitSetupFailedExit)
	}
	os.Exit(0)
}

// useTestPGInitHelper runs the test server's init the way a test can: this test binary re-entered
// as testpg-init, with the home mounts directory the test's own, since a machine running the tests
// has neither the image's /var/run/legion mount points nor the right to make them.
func useTestPGInitHelper(t *testing.T, homeMounts string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previousMounts, previousLaunch := roleHomeMounts, testPGInitLaunch
	roleHomeMounts, testPGInitLaunch = homeMounts, testPGInitHelperLaunch(self)
	t.Cleanup(func() { roleHomeMounts, testPGInitLaunch = previousMounts, previousLaunch })
}

// reportingTestPG puts a `legion-testpg` on PATH that reads, from inside the server's own
// namespaces, everything the confinement must deny and the one thing it must allow, and writes what
// it found to report. It stands in for the image's script, whose postmaster would run in exactly
// this place.
func reportingTestPG(t *testing.T, report, token, rolesDir, dataDir string, launcherPID int) {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
{
	echo "uid=$(id -u)"
	echo "token-read=$(cat %[2]q 2>&1)"
	echo "token-stat=$(ls -l %[2]q 2>&1)"
	echo "roles-dir=$(ls -A %[3]q 2>&1 | tr '\n' ',')"
	echo "launcher-environ=$(cat /proc/%[5]d/environ 2>&1 | tr -d '\0' | cut -c1-60)"
	echo "launcher-root=$(ls /proc/%[5]d/root 2>&1 | head -1)"
	echo "proc-pids=$(ls -1 /proc | grep -c '^[0-9]')"
	touch %[4]q/writable && echo "data-dir=writable" || echo "data-dir=unwritable"
} >> %[1]q
exit 0
`, report, token, rolesDir, dataDir, launcherPID)
	if err := os.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The server runs in namespaces of its own, so a superuser's `copy … to program`, `copy … from`
// and pg_read_file reach no role's credentials and no process outside the server: every role's
// directories are behind an empty filesystem over the roles directory, the launcher's own /proc
// entry is not there to read a root or an environment through, and what is left is the server's
// own data directory, which it writes. Without these namespaces the postmaster is the launcher's
// child in the roles container's namespace, where a sibling role's token file is readable by the
// uid the postmaster runs as — which is the leak this covers, and which exists there whatever the
// role's privileges are.
func TestTheTestDatabaseRunsWhereNoRolesCredentialsAre(t *testing.T) {
	requireRoleNamespaces(t)
	rolesDir, scratch := t.TempDir(), t.TempDir()
	sibling := filepath.Join(rolesDir, "implementer", RolePrivateDir, "g7")
	if err := os.MkdirAll(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(sibling, "ENVOY_TOKEN")
	if err := os.WriteFile(token, []byte("a-sibling-roles-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The launcher's own view: the token is readable right here, as the same uid, which is what
	// the namespaces below take away.
	if _, err := os.ReadFile(token); err != nil {
		t.Fatalf("the launcher's own namespace cannot read the token this test is about: %v", err)
	}
	data := testPGDir(rolesDir)
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	homeMounts := filepath.Join(scratch, "home-mounts")
	if err := os.MkdirAll(homeMounts, 0o755); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(scratch, "report")
	reportingTestPG(t, report, token, rolesDir, data, os.Getpid())
	useTestPGInitHelper(t, homeMounts)
	testPGInterval = time.Hour
	t.Cleanup(func() { testPGInterval = 15 * time.Second })

	var log syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := serveTestPG(ctx, Config{RolesDir: rolesDir, TestPostgres: true}, &roleLog{stdout: &log, stderr: &log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })

	lines := waitForReport(t, report, &log)
	for _, want := range []struct{ prefix, holds string }{
		// Nothing of the sibling's: the cover answers both the read and the stat with the
		// path gone, which is what `copy … from '<path>'` and pg_read_file would see.
		{"token-read=", "No such file or directory"},
		{"token-stat=", "No such file or directory"},
		// The roles directory holds the server's own directory and nothing else.
		{"roles-dir=", "testpg,"},
		// No /proc entry of the launcher's, so no root and no environment of it either.
		{"launcher-environ=", "No such file or directory"},
		{"launcher-root=", "No such file or directory"},
		// The server's own data directory is its own to write.
		{"data-dir=", "writable"},
	} {
		got, ok := lines[want.prefix]
		if !ok {
			t.Errorf("the report has no %s line:\n%v", want.prefix, lines)
			continue
		}
		if !strings.Contains(got, want.holds) {
			t.Errorf("%s%s, want it to hold %q", want.prefix, got, want.holds)
		}
	}
	// The server's own /proc: its init, the script and the script's own children, and nothing
	// of the container's hundreds.
	if got := lines["proc-pids="]; got == "" || got > "9" && len(got) > 1 {
		t.Errorf("proc-pids=%s, want the server's own few processes alone", got)
	}
	if got := lines["uid="]; got != fmt.Sprint(roleUID) {
		t.Errorf("uid=%s, want %d: the postmaster refuses to run as root", got, roleUID)
	}
}

// waitForReport reads the stand-in script's report once it has been written, and fails the test
// with the container's log if it never is.
func waitForReport(t *testing.T, report string, log *syncBuffer) map[string]string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		body, err := os.ReadFile(report)
		if err == nil && strings.Contains(string(body), "data-dir=") {
			lines := map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
				key, value, found := strings.Cut(line, "=")
				if found {
					lines[key+"="] = value
				}
			}
			return lines
		}
		if time.Now().After(deadline) {
			t.Fatalf("the test database's init never ran the script: %v; the container log holds %q", err, log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recordingTestPG puts a `legion-testpg` on PATH that appends one line per call — the subcommand,
// the DSN it was given and the directory it was told — to calls, and exits 0.
func recordingTestPG(t *testing.T, calls string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s %s %s\\n' \"$1\" \"$" + TestPGVariable + "\" \"$" + testPGDirVariable + "\" >> " + calls + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// failingTestPG puts a `legion-testpg` on PATH that refuses, as a script whose server cannot start
// does.
func failingTestPG(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho 'legion-testpg: no server here' >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The launcher serves the pod's test database before it accepts a role start, and it does so
// through one `legion launcher testpg-init` in namespaces of its own, which is the server's PID 1
// and ticks from in there. serveTestPG returns only once that init's first `ensure` has run — the
// init closes the ready pipe then — so a role that starts at once finds the server serving its
// DSN, and every later tick asks again.
func TestTheLauncherServesTheTestDatabaseThroughItsOwnInit(t *testing.T) {
	requireRoleNamespaces(t)
	rolesDir, scratch := t.TempDir(), t.TempDir()
	calls := filepath.Join(scratch, "calls")
	recordingTestPG(t, calls)
	useTestPGInitHelper(t, filepath.Join(scratch, "home-mounts"))
	if err := os.MkdirAll(filepath.Join(scratch, "home-mounts"), 0o755); err != nil {
		t.Fatal(err)
	}
	testPGInterval = 20 * time.Millisecond
	t.Cleanup(func() { testPGInterval = 15 * time.Second })
	var log syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := serveTestPG(ctx, Config{RolesDir: rolesDir, TestPostgres: true}, &roleLog{stdout: &log, stderr: &log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	want := "ensure " + TestPGDSN + " " + filepath.Join(rolesDir, TestPGDirName) + "\n"
	// The ordering: the first call is already there when serveTestPG returns, with no wait of
	// this test's own.
	first, err := os.ReadFile(calls)
	if err != nil {
		t.Fatalf("serveTestPG returned before its first ensure ran: %v; the container log holds %q", err, log.String())
	}
	if string(first) != want {
		t.Errorf("the first call is %q, want %q", first, want)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		body, err := os.ReadFile(calls)
		if err == nil && strings.Count(string(body), "ensure ") > 1 {
			if got := strings.ReplaceAll(string(body), want, ""); got != "" {
				t.Errorf("a call is %q, want every call to be %q", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the init asked once and never again: %q, %v; the container log holds %q", body, err, log.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A script that fails is logged and nothing else: a pod whose test database cannot start still
// runs its roles, where a launcher that failed its container would cost the issue every role it
// serves. The init's own line reaches the container's log, since its output is the launcher's.
func TestATestDatabaseThatCannotStartOnlyLogs(t *testing.T) {
	requireRoleNamespaces(t)
	rolesDir, scratch := t.TempDir(), t.TempDir()
	failingTestPG(t)
	useTestPGInitHelper(t, filepath.Join(scratch, "home-mounts"))
	if err := os.MkdirAll(filepath.Join(scratch, "home-mounts"), 0o755); err != nil {
		t.Fatal(err)
	}
	testPGInterval = time.Hour
	t.Cleanup(func() { testPGInterval = 15 * time.Second })
	var log syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := serveTestPG(ctx, Config{RolesDir: rolesDir, TestPostgres: true}, &roleLog{stdout: &log, stderr: &log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(log.String(), "testpg-init: "+testPGProgram+" ensure") {
		if time.Now().After(deadline) {
			t.Fatalf("the container log holds %q, want the refusal named", log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The guard holds a running child out of the reaper's pass and forgets it once its own wait has
// read its exit, so nothing of the launcher's own is left in the skip list between calls.
func TestTheGuardForgetsAChildOnceItsWaitHasRead(t *testing.T) {
	guard := &pidGuard{}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	pid, err := guard.start(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if held := guard.held(); len(held) != 1 || held[0] != pid {
		t.Errorf("the guard holds %v while the child runs, want just %d", held, pid)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	guard.done(pid)
	if held := guard.held(); len(held) != 0 {
		t.Errorf("the guard still holds %v", held)
	}
}

// One run of the script is bounded, so an `ensure` that hangs — a lock a dead holder still owns —
// costs a pod one wait and a log line, not its whole boot: the launcher holds its roles for that
// first `ensure`, inside the daemon's boot timeout. It is driven through the init, as a pod runs
// it, and not by calling the run directly: the run nests a user namespace mapping its inside-1000
// to outside-0, which only a caller that is already root in a user namespace of its own may write
// — testpg-init always is, and a test process is not.
func TestATestDatabaseRunThatHangsIsBounded(t *testing.T) {
	requireRoleNamespaces(t)
	rolesDir, scratch := t.TempDir(), t.TempDir()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 30\n"
	if err := os.WriteFile(filepath.Join(dir, testPGProgram), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	useTestPGInitHelper(t, filepath.Join(scratch, "home-mounts"))
	if err := os.MkdirAll(filepath.Join(scratch, "home-mounts"), 0o755); err != nil {
		t.Fatal(err)
	}
	testPGBound, testPGInterval = 2*time.Second, time.Hour
	t.Cleanup(func() { testPGBound, testPGInterval = 60*time.Second, 15*time.Second })
	var log syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	done := serveTestPG(ctx, Config{RolesDir: rolesDir, TestPostgres: true}, &roleLog{stdout: &log, stderr: &log}, &pidGuard{})
	t.Cleanup(func() { cancel(); <-done })
	waited := time.Since(started)
	// The line travels a pipe from the init to the launcher's log writer, so it may land just
	// after serveTestPG returns; what the return itself proves is the wait, below.
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(log.String(), "did not finish within") {
		if time.Now().After(deadline) {
			t.Fatalf("the container log holds %q, want the bound named", log.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if waited > 20*time.Second {
		t.Errorf("the launcher waited %s on a script that hangs under a %s bound", waited, testPGBound)
	}
}
