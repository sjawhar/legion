//go:build linux

package launcher

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// testPGNamespaceAttr clones the test server into user, mount, PID and IPC namespaces of its own.
// It is roleNamespaceAttr itself: the launcher's user maps to 0 inside, because a process whose
// euid is not 0 loses its capabilities on the next execve and could then mount nothing, and the
// script runs in a nested user namespace as the image's unprivileged user (runTestPGHere), as a
// role's shim does, since the postmaster refuses to run as root. gVisor supports exactly these
// flags (pkg/sentry/kernel/task_clone.go, SupportedCloneFlags).
func testPGNamespaceAttr() (*syscall.SysProcAttr, error) { return roleNamespaceAttr() }

// testPGStage is where prepareTestPGNamespace holds the data directory while it covers the roles
// directory: a mount of its own, outside the roles directory, removed again once the data
// directory is bound back. It is in this namespace alone.
const testPGStage = "/tmp/.legion-testpg-stage"

// testPGInit is `legion launcher testpg-init`'s body: the namespaces' setup, then the script on
// every tick until stop, reaping the server's orphans as they exit.
func testPGInit(stop context.Context, cfg TestPGInitConfig) error {
	if os.Getpid() != 1 {
		return errors.New("testpg-init: not the init of a PID namespace of its own")
	}
	// The kernel delivers a signal to a PID namespace's init only while it handles it, and
	// `legion`'s main registers for the launcher's stop before it runs any subcommand, so a
	// stop that arrives during the setup below ends this process at the first tick rather than
	// waiting for the launcher's SIGKILL.
	exits := make(chan os.Signal, 1)
	signal.Notify(exits, syscall.SIGCHLD)
	defer signal.Stop(exits)
	// The script never inherits the launcher's ready pipe: this process alone reports the first
	// `ensure`, and a postmaster holding the pipe open would leave the launcher waiting for a
	// server that is already there.
	syscall.CloseOnExec(cfg.ReadyFD)
	ready := os.NewFile(uintptr(cfg.ReadyFD), "testpg-ready")
	if err := prepareTestPGNamespace(cfg); err != nil {
		// The launcher waits on this pipe; closing it here is what frees the pod's roles to
		// start without their test database rather than after the launcher's whole bound.
		if ready != nil {
			_ = ready.Close()
		}
		return fmt.Errorf("testpg-init: %w", err)
	}
	serveTestPGHere(stop, cfg, exits, ready)
	return nil
}

// prepareTestPGNamespace is everything the server's mount namespace holds that the container's
// does not. Its order matters: the data directory is held aside before the cover goes over the
// roles directory and bound back after it, and /proc is mounted last, so the server sees its own
// processes alone from the moment it starts.
func prepareTestPGNamespace(cfg TestPGInitConfig) error {
	data := testPGDir(cfg.RolesDir)
	if err := privateMounts(); err != nil {
		return fmt.Errorf("hold this server's mounts to itself: %w", err)
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return fmt.Errorf("make the server's own directory: %w", err)
	}
	if err := os.MkdirAll(testPGStage, 0o700); err != nil {
		return fmt.Errorf("make the server's stage: %w", err)
	}
	if err := bindMount(data, testPGStage); err != nil {
		return fmt.Errorf("hold the server's directory aside: %w", err)
	}
	// The covers: every role's gh, private, state and agent-secrets directories, every role's
	// home layers, and this launcher's own token, all behind an empty filesystem, and the
	// container's view of the home's volumes with them. What is left of the roles directory is
	// the server's own directory, bound back on top.
	for _, dir := range []string{cfg.RolesDir, cfg.HomeMounts} {
		if err := mountTmpfs(dir, "size=1m,mode=0755"); err != nil {
			return fmt.Errorf("cover %s: %w", dir, err)
		}
	}
	if err := os.MkdirAll(data, 0o700); err != nil {
		return fmt.Errorf("make the server's own directory on the cover: %w", err)
	}
	if err := bindMount(testPGStage, data); err != nil {
		return fmt.Errorf("bind the server's directory back: %w", err)
	}
	if err := unix.Unmount(testPGStage, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("drop the server's stage: %w", err)
	}
	if err := os.Remove(testPGStage); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove the server's stage: %w", err)
	}
	if err := unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount the server's own /proc: %w", err)
	}
	return nil
}

// serveTestPGHere runs `ensure` at once and again on every tick until stop, and reaps whatever the
// server leaves behind: `pg_ctl` daemonises the postmaster, which this process is the PID 1 of.
// The first `ensure` is reported by closing ready, whatever it answered, so the launcher holds a
// role start until the server a role is told about is there, and does not hold it any longer than
// that when the server cannot start.
// A failure is logged and never thrown, as the launcher's own tick did: a pod whose test database
// cannot start still runs its roles, and its agents read the refusal in the container's log.
func serveTestPGHere(stop context.Context, cfg TestPGInitConfig, exits <-chan os.Signal, ready *os.File) {
	interval := testPGInterval
	ensure := func() {
		if err := runTestPGHere(stop, cfg, "ensure"); err != nil {
			fmt.Fprintf(cfg.Stderr, "testpg-init: %v\n", err)
		}
	}
	ensure()
	if ready != nil {
		_ = ready.Close()
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop.Done():
			return
		case <-exits:
			reapTestPGOrphans()
		case <-ticker.C:
			ensure()
		}
	}
}

// reapTestPGOrphans collects every child this init has: the script's own process is waited for by
// the run that started it, and what is left is the postmaster `pg_ctl` daemonised and whatever it
// leaves behind, reparented here.
func reapTestPGOrphans() {
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}

// runTestPGHere runs one command of the image's script with the DSN it must serve and the
// directory it must serve it from, in a nested user namespace as the image's unprivileged user:
// the postmaster refuses to run as root, and this init is root inside its own user namespace so
// that it keeps the capabilities its mounts need. The files the script writes are that user's
// outside the namespaces, as they were when the launcher ran the script itself. It is runTestPG
// without the launcher's reaper guard: this process reaps its own namespace, and the wait below
// is the only one that reads this child's exit. WaitDelay makes the kill at testPGBound final:
// without it the postmaster `pg_ctl` leaves running would hold the wait past the bound on the
// output pipe it inherited.
func runTestPGHere(ctx context.Context, cfg TestPGInitConfig, command string) error {
	bounded, cancel := context.WithTimeout(ctx, testPGBound)
	defer cancel()
	cmd := exec.CommandContext(bounded, testPGProgram, command)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(),
		TestPGVariable+"="+cfg.DSN,
		testPGDirVariable+"="+testPGDir(cfg.RolesDir),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: roleUID, HostID: 0, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: roleUID, HostID: 0, Size: 1}},
		GidMappingsEnableSetgroups: false,
	}
	cmd.Stdout, cmd.Stderr = cfg.Stdout, cfg.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s %s: %w", testPGProgram, command, err)
	}
	if err := cmd.Wait(); err != nil {
		if bounded.Err() != nil && ctx.Err() == nil {
			return fmt.Errorf("%s %s did not finish within %s", testPGProgram, command, testPGBound)
		}
		return fmt.Errorf("%s %s: %w", testPGProgram, command, err)
	}
	return nil
}
