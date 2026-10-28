package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"syscall"
	"time"
)

// A deployed launcher is PID 1 in its role's private PID namespace. kill(-1) there includes
// setsid/daemonized descendants but cannot reach another role. Standalone host invocations keep
// their existing process-group scope; they must never signal the host's whole process population.
func signalGeneration(pid int, sig syscall.Signal) error {
	target := -pid
	if os.Getpid() == 1 {
		target = -1
	}
	if err := syscall.Kill(target, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal role processes: %w", err)
	}
	return nil
}

// reapNamespace leaves each process of skip, a direct child exec.Cmd.Wait reads, to that wait.
// Every other adopted child belongs to PID 1; Wait4 with WNOHANG reaps exited orphans without
// delaying a resident worker's next turn, and never takes the exit status a generation's own wait
// reports.
func reapNamespace(skip ...int) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, fmt.Errorf("list role namespace processes: %w", err)
	}
	remaining := false
	for _, entry := range entries {
		name := entry.Name()
		if name == "" || name[0] < '0' || name[0] > '9' {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 1 || slices.Contains(skip, pid) {
			continue
		}
		reaped, err := syscall.Wait4(pid, nil, syscall.WNOHANG, nil)
		if err != nil && !errors.Is(err, syscall.ECHILD) && !errors.Is(err, syscall.ESRCH) && !errors.Is(err, syscall.EINTR) {
			return false, fmt.Errorf("reap role process %d: %w", pid, err)
		}
		remaining = remaining || reaped != pid
	}
	return remaining, nil
}

// reapOrphans is PID 1's reaper: on every SIGCHLD it reaps each exited process of the container's
// PID namespace but the generations its managers run, whose exits their own waits read. Every
// manager's lock is held across the pass, and a manager records its generation's pid under its
// lock as it starts it, so the reaper never waits on a generation it has not seen yet. In the
// controller's pod a pass that fails leaves the role's namespace unobservable, so the launcher
// fails its container loudly. In an issue pod's container the container's /proc still lists every
// role's processes, since a role's PID namespace is nested in it, but none is the launcher's child
// except each role's role-init, which the pass skips: the rest are their role-init's to reap, and
// Wait4 on them answers ECHILD. So a failed pass costs no role anything, and it is logged and the
// next pass tries again.
func reapOrphans(ctx context.Context, signals <-chan os.Signal, managers []*manager) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			skip := make([]int, 0, len(managers))
			for _, m := range managers {
				m.mu.Lock()
				if m.child != nil {
					skip = append(skip, m.child.pid)
				}
			}
			_, err := reapNamespace(skip...)
			for _, m := range managers {
				m.mu.Unlock()
			}
			switch {
			case err == nil:
			case !managers[0].namespaced:
				panic(err)
			default:
				managers[0].log.linef("launcher: %v", err)
			}
		}
	}
}

// Cleanup finishes before the exit is reported, credentials are removed or a new generation is
// admitted. A namespace whose processes cannot be observed is not safe to reuse: fail the launcher
// loudly so Kubernetes replaces this container rather than reporting a false completed stop. Only
// the single-role form runs it; an issue pod's roles end with their own namespace's init.
func (m *manager) cleanExitedGeneration() {
	if os.Getpid() != 1 {
		return
	}
	if err := signalGeneration(0, syscall.SIGTERM); err != nil {
		panic(err)
	}
	deadline := time.Now().Add(m.cfg.StopGrace)
	killed := false
	for {
		remaining, err := reapNamespace(0)
		if err != nil {
			panic(err)
		}
		if !remaining {
			return
		}
		if !killed && !time.Now().Before(deadline) {
			if err := signalGeneration(0, syscall.SIGKILL); err != nil {
				panic(err)
			}
			killed = true
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// signalChild reaches one generation. In the controller's pod the generation is a process group
// of the container's own namespace, so the signal goes to the group. In an issue pod the
// generation is the init of the role's own namespaces: the signal goes to it alone, through its
// own process handle, which forwards a stop to every process of the role and, killed, takes the
// whole namespace with it. A generation its wait has already reaped is done: the handle reaches
// nothing then, where its pid could name another role's role-init by now.
func (m *manager) signalChild(active *child, sig syscall.Signal) error {
	if !m.namespaced {
		return signalGeneration(active.pid, sig)
	}
	if err := active.process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal role %s: %w", m.role, err)
	}
	return nil
}

func (m *manager) endGeneration(active *child, grace time.Duration) error {
	if err := m.signalChild(active, syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case <-active.done:
	case <-time.After(grace):
		if err := m.signalChild(active, syscall.SIGKILL); err != nil {
			return err
		}
		<-active.done
	}
	return nil
}
