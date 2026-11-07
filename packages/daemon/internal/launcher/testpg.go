package launcher

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The pod's own PostgreSQL for tests, which an issue pod's launcher serves when the deployment
// sets `runtime.kubernetes.test_postgres` (Config.TestPostgres). TestPGVariable is the variable
// this launcher puts into every generation it starts (testPGEnv), and TestPGDSN its value: a
// loopback server with no password, whose user is a `createdb createrole nosuperuser` role of the
// cluster rather than its superuser, so a test that reaches it can make the roles its own suite
// needs and still cannot run programs as the server's process. The database the DSN names is that
// login's own, which `legion-testpg ensure` creates with the login as its owner, so a test may
// create a schema or a trusted extension in it; an untrusted one still needs the superuser, which
// is why `ensure` puts `vector` in template1, where every database made from it carries it.
// TestPGDirName is the directory the server's data and admin socket live in, under the roles
// directory, which every role's namespace covers (role-init), so no role can edit the cluster's
// configuration and have the launcher load it at the next start.
//
// The launcher owns the server and no role does: a role's generation runs as the init of its own
// PID namespace, and the kernel kills every process in a PID namespace when its init exits, so a
// server a role's setup step started would die with that generation and take every sibling role's
// database with it.
const (
	TestPGVariable = "LEGION_TEST_PG_DSN"
	TestPGDSN      = "postgres://legion_test@127.0.0.1:5433/legion_test?sslmode=disable"
	TestPGDirName  = "testpg"
	// testPGDirVariable is how the worker image's script is told where the cluster lives.
	testPGDirVariable = "LEGION_TESTPG_DIR"
	// testPGProgram is that script, on the image's PATH.
	testPGProgram = "legion-testpg"
)

// testPGInterval is how often the launcher asks the script again. `ensure` returns after one
// connection through the DSN while the server serves it, so the poll costs milliseconds, and a
// postmaster that died — killed by the node under memory pressure, or crashed — is replaced
// within it, as is a start that left the server up and its role or database unmade. The
// launcher polls rather than waiting on a child because `pg_ctl` daemonises the postmaster: it is
// reparented to this process, and PID 1's reaper collects it. A test shortens it.
var testPGInterval = 15 * time.Second

// testPGDir is where the server's data and admin socket live for this launcher's roles directory.
func testPGDir(rolesDir string) string { return filepath.Join(rolesDir, TestPGDirName) }

// testPGEnv is what a generation of this launcher is told about the pod's test database: when
// this launcher serves one (serves, from Config.TestPostgres), TestPGVariable is TestPGDSN of
// this binary, over whatever value the daemon's start command carried; when it serves none the
// variable is taken out of that environment, so a role is never told of a server its pod does not
// run. env is the launcher's own environment with the start command's values over it (start's
// mergeEnv), so the removal drops the variable from either source. The daemon puts it in a role's
// start command for an issue pod alone, and only while that deployment sets
// `runtime.kubernetes.test_postgres` (the sandbox package's manifest), so from the start command
// the removal serves a pod started before that key was turned on and kept since: its launcher was
// given no such flag, while the daemon that starts its roles now sends the DSN. From the
// launcher's own environment it serves a launcher that inherited the variable, as the idle rig of
// the daemon package's tests runs one inside a test process that holds LEGION_TEST_PG_DSN.
//
// It overrides the start command because the daemon and the worker image are deployed from two
// separately pinned builds, and this launcher, which comes from the image, is the process that
// runs `legion-testpg ensure` and so decides which database the server holds. The process that
// serves the database is therefore the one that names it: a role in a pod whose image predates a
// change of the DSN is told that image's own DSN, rather than a database nothing in its pod
// creates.
//
// It reads whether this launcher was told to serve a database, never whether the server answers:
// the first `ensure` runs before any role starts and a failure of it is logged rather than thrown
// (serveTestPG), so a role whose server is late or dead is still told where that server is.
func testPGEnv(env []string, serves bool) []string {
	if !serves {
		return withoutEnv(env, TestPGVariable)
	}
	return mergeEnv(env, []string{TestPGVariable + "=" + TestPGDSN})
}

// serveTestPG starts the pod's test database and keeps it running until ctx ends, and answers a
// channel closed once its ticker goroutine has returned, so a caller that ends ctx can wait for
// it rather than leave it running. The first attempt is this call's, before the launcher serves
// any role, so a role that starts at once finds the server serving its DSN; afterwards each tick
// asks again, which starts a postmaster that has died and finishes a start that left one up.
// A failure is logged and never thrown: a pod whose test database cannot start still runs its
// roles, whose agents then read the refusal in the container's log, where a launcher that failed
// its container would instead cost the issue every role it was serving.
func serveTestPG(ctx context.Context, cfg Config, log *roleLog, guard *pidGuard) <-chan struct{} {
	// The interval is read here, in the caller's own goroutine, and not inside the one below: it
	// is a package variable a test shortens, and a goroutine still reading it after the test
	// restored it is a data race the race detector fails the test for.
	interval := testPGInterval
	ensure := func() {
		if err := runTestPG(ctx, cfg, log, guard, "ensure"); err != nil {
			log.linef("launcher: test database: %v", err)
		}
	}
	ensure()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				ensure()
			}
		}
	}()
	return done
}

// testPGBound is how long one run of the script may take. The first `ensure` runs before the
// launcher serves any role, and the daemon's boot timeout for a pod is 120 s by default, so an
// `ensure` that hangs — a lock a dead holder still owns, a disk that does not answer — must not
// take the pod's whole boot with it. `flock` inside the script waits up to 180 s of its own, which
// this bound cuts short deliberately: a role starting without its test database and a line in the
// log is better than a pod the daemon gives up on. A test shortens it.
var testPGBound = 60 * time.Second

// runTestPG runs one command of the image's script with the DSN it must serve and the directory it
// must serve it from, writing its output to the container's log behind a prefix of its own, so a
// refusal reads apart from a role's lines. Its process is held out of the reaper's pass (pidGuard)
// until this wait has read its exit. WaitDelay makes the kill at testPGBound final: without it a
// child that keeps the output pipe open — the postmaster `pg_ctl` leaves running — would hold this
// wait past the bound.
func runTestPG(ctx context.Context, cfg Config, log *roleLog, guard *pidGuard, command string) error {
	bounded, cancel := context.WithTimeout(ctx, testPGBound)
	defer cancel()
	out, _ := log.writers(testPGProgram)
	cmd := exec.CommandContext(bounded, testPGProgram, command)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		TestPGVariable+"="+TestPGDSN,
		testPGDirVariable+"="+testPGDir(cfg.RolesDir),
	)
	cmd.Stdout, cmd.Stderr = out, out
	pid, err := guard.start(cmd)
	if err != nil {
		return fmt.Errorf("start %s %s: %w", testPGProgram, command, err)
	}
	defer guard.done(pid)
	if err := cmd.Wait(); err != nil {
		if bounded.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("%s %s did not finish within %s", testPGProgram, command, testPGBound)
		}
		return fmt.Errorf("%s %s: %w", testPGProgram, command, err)
	}
	return nil
}

// CheckTestPostgres proves the pod's own test database where a pod runs it, before any issue pod
// does: `legion probe-image --test-postgres <dir>`, which the daemon's probe Sandbox passes when
// the deployment sets runtime.kubernetes.test_postgres and the image build does not. The probe pod
// runs under the deployment's own runtime (gVisor on the Autopilot cluster), so a runtime the
// server cannot `initdb`, start or connect under refuses the daemon's boot probe rather than every
// pod's first Postgres test. It runs what a pod's launcher runs — check, ensure — then makes and
// drops a database through the DSN every role is told, and stops the server it started, so the
// probe pod leaves nothing running. dir is a directory of the probe's own volume.
func CheckTestPostgres(dir string) error {
	env := append(os.Environ(), TestPGVariable+"="+TestPGDSN, testPGDirVariable+"="+dir)
	run := func(name string, argv ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), testPGBound)
		defer cancel()
		cmd := exec.CommandContext(ctx, name, argv...)
		cmd.WaitDelay = time.Second
		cmd.Env = env
		output, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("the pod's own test database: %s %s: %w: %s",
				name, strings.Join(argv, " "), err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	// The database is named for the probe alone, and dropped again, so the cluster the probe
	// leaves behind holds nothing of it; the server is stopped whatever the steps answered. It is a
	// file copy of template1, as every database the module's tests make is (docs/testing.md).
	steps := [][]string{
		{testPGProgram, "check"},
		{testPGProgram, "ensure"},
		{"psql", "-X", "-q", TestPGDSN, "-c", "create database legion_probe_testpg strategy file_copy"},
		{"psql", "-X", "-q", TestPGDSN, "-c", "drop database legion_probe_testpg with (force)"},
	}
	for _, step := range steps {
		if err := run(step[0], step[1:]...); err != nil {
			_ = run(testPGProgram, "stop")
			return err
		}
	}
	return run(testPGProgram, "stop")
}

// pidGuard holds the pids of the launcher's own children that are no generation's, so PID 1's
// reaper leaves each exit to the wait that started it (reapOrphans skips them as it skips a
// manager's generation). A pid is recorded before its process exists, under the lock the reaper
// holds for its whole pass, so no pass can read /proc between a child's start and its record.
type pidGuard struct {
	mu   sync.Mutex
	pids map[int]bool
}

// start runs cmd and answers its pid, recorded as the launcher's own.
func (g *pidGuard) start(cmd *exec.Cmd) (int, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	if g.pids == nil {
		g.pids = map[int]bool{}
	}
	g.pids[pid] = true
	return pid, nil
}

// done forgets pid once its own wait has read its exit.
func (g *pidGuard) done(pid int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.pids, pid)
}

// held are the pids the reaper must skip. The caller holds the guard's lock for the whole pass.
func (g *pidGuard) held() []int {
	pids := make([]int, 0, len(g.pids))
	for pid := range g.pids {
		pids = append(pids, pid)
	}
	return pids
}
