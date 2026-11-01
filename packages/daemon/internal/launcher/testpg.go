package launcher

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// The pod's own PostgreSQL for tests, which an issue pod's launcher serves when the deployment
// sets `runtime.kubernetes.test_postgres` (Config.TestPostgres). TestPGVariable is the variable
// every role of such a pod is told, and TestPGDSN its value: a loopback server with no password,
// whose user is this cluster's superuser and owns the database the DSN names. The privileges the
// suites that run here need do not reduce to a set of grants — on 18 `create event trigger` needs
// SUPERUSER and there is no grantable role for it — so what bounds the DSN's role is the server
// rather than the role: the pod's own cluster, on an emptyDir the kubelet deletes with the pod,
// on loopback alone, with test data only, named by a DSN the runtime sets and refuses from a
// deployment's pod.env. The database it names is the role's own, not `postgres`: on 18 a
// non-owner may not create a table in a database's public schema, so a suite that migrates into
// the database the DSN names — rather than making one per test — failed there with "permission
// denied for schema public".
//
// TestPGDirName is the directory the server's data and admin socket live in, under the roles
// directory, which every role's namespace covers (role-init), so no role can edit the cluster's
// configuration and have the next start load it.
//
// The launcher owns the server and no role does: a role's generation runs as the init of its own
// PID namespace, and the kernel kills every process in a PID namespace when its init exits, so a
// server a role's setup step started would die with that generation and take every sibling role's
// database with it. The launcher does not run it in its own namespace either, since a superuser's
// `copy … to program` runs as the postmaster's process: it starts one `legion launcher
// testpg-init` in namespaces of its own (testpginit.go), and that process is the server's PID 1.
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

// serveTestPG starts the pod's test database and returns once its first `ensure` has run, before
// the launcher serves any role, so a role that starts at once finds the server serving its DSN.
// It answers a channel closed once the process it started has been waited for, so a caller that
// ends ctx can wait for it rather than leave it running.
//
// The launcher does not run the script itself and does not tick: it starts one `legion launcher
// testpg-init`, cloned into user, mount, PID and IPC namespaces of its own, which covers every
// role's directories, binds the server's own back, mounts its own /proc, and then runs `ensure`
// at once and on every tick for the pod's life. The server must be in those namespaces because
// its DSN's role is the cluster's superuser: `copy … to program` runs as the postmaster's own
// process, and the launcher's own namespace is the one where every role's credentials are
// readable by the uid the postmaster runs as. The first `ensure` is reported back over a pipe
// the init closes when it has run, whatever it answered, which is what this call waits for.
//
// A failure is logged and never thrown: a pod whose test database cannot start still runs its
// roles, whose agents then read the refusal in the container's log, where a launcher that failed
// its container would instead cost the issue every role it was serving.
func serveTestPG(ctx context.Context, cfg Config, log *roleLog, guard *pidGuard) <-chan struct{} {
	done := make(chan struct{})
	out, _ := log.writers(testPGProgram)
	attr, err := testPGNamespaceAttr()
	if err != nil {
		log.linef("launcher: test database: %v", err)
		close(done)
		return done
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		log.linef("launcher: test database: make the ready pipe: %v", err)
		close(done)
		return done
	}
	defer reader.Close()
	init := TestPGInitConfig{RolesDir: cfg.RolesDir, HomeMounts: roleHomeMounts, DSN: TestPGDSN, ReadyFD: testPGReadyFD}
	argv, extra, err := testPGInitLaunch(init)
	if err != nil {
		writer.Close()
		log.linef("launcher: test database: %v", err)
		close(done)
		return done
	}
	// No CommandContext: the kill on ctx's end is this function's own, after the stop below, so
	// the init is asked to stop first and killed only if it does not.
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), extra...)
	cmd.SysProcAttr = attr
	cmd.Stdout, cmd.Stderr = out, out
	cmd.ExtraFiles = []*os.File{writer}
	pid, err := guard.start(cmd)
	writer.Close()
	if err != nil {
		log.linef("launcher: test database: start %s: %v", TestPGInitSubcommand, err)
		close(done)
		return done
	}
	ended := make(chan error, 1)
	go func() { ended <- cmd.Wait() }()
	go func() {
		defer close(done)
		defer guard.done(pid)
		select {
		case err := <-ended:
			if err != nil && ctx.Err() == nil {
				log.linef("launcher: test database: %s ended: %v", TestPGInitSubcommand, err)
			}
			return
		case <-ctx.Done():
		}
		// The init forwards no stop to the postmaster: its own exit ends the PID namespace and
		// every process in it. SIGTERM first, so it returns from its loop, then the kill the
		// grace ends with.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-ended:
		case <-time.After(testPGStopGrace):
			_ = cmd.Process.Kill()
			<-ended
		}
	}()
	waitForFirstEnsure(ctx, reader, log)
	return done
}

// waitForFirstEnsure returns when the init has closed its end of the ready pipe, which it does
// when its first `ensure` has run or when its namespaces could not be made, and in no case later
// than testPGReadyBound, so a start that hangs costs the pod one log line and not its boot. The
// read runs in a goroutine of its own because a pipe read is not interruptible.
func waitForFirstEnsure(ctx context.Context, reader *os.File, log *roleLog) {
	read := make(chan struct{})
	go func() {
		defer close(read)
		_, _ = io.Copy(io.Discard, reader)
	}()
	select {
	case <-read:
	case <-ctx.Done():
	case <-time.After(testPGReadyBound):
		log.linef("launcher: test database: its first ensure had not finished within %s; the roles start without waiting for it", testPGReadyBound)
	}
}

// testPGReadyBound is how long the launcher waits for that first `ensure`. It is testPGBound, the
// bound on one run of the script, and a little more for the namespaces and the fork, so a server
// that cannot start costs the pod a log line inside the daemon's 120 s boot timeout rather than
// the boot itself.
var testPGReadyBound = testPGBound + 10*time.Second

// testPGStopGrace is how long the launcher waits for the test server's init to return from its
// own loop after the stop, before it kills the namespace. One `ensure` may be running, and the
// bound on that is testPGBound.
var testPGStopGrace = 5 * time.Second

// testPGBound is how long one run of the script may take. The first `ensure` runs before the
// launcher serves any role, and the daemon's boot timeout for a pod is 120 s by default, so an
// `ensure` that hangs — a lock a dead holder still owns, a disk that does not answer — must not
// take the pod's whole boot with it. `flock` inside the script waits up to 180 s of its own, which
// this bound cuts short deliberately: a role starting without its test database and a line in the
// log is better than a pod the daemon gives up on. A test shortens it.
var testPGBound = 60 * time.Second

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
	// leaves behind holds nothing of it; the server is stopped whatever the steps answered.
	steps := [][]string{
		{testPGProgram, "check"},
		{testPGProgram, "ensure"},
		{"psql", "-X", "-q", TestPGDSN, "-c", "create database legion_probe_testpg"},
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
