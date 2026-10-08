package launcher

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// reapNamespace leaves the direct child to exec.Cmd.Wait. Every other adopted child belongs to
// PID 1; Wait4 with WNOHANG reaps exited orphans without delaying a resident worker's next turn.
func reapNamespace(directPID int) (bool, error) {
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
		if err != nil || pid <= 1 || pid == directPID {
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

func (m *manager) reapOrphans(ctx context.Context, signals <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			m.mu.Lock()
			pid := 0
			if m.child != nil {
				pid = m.child.pid
			}
			_, err := reapNamespace(pid)
			m.mu.Unlock()
			if err != nil {
				panic(err)
			}
		}
	}
}

// Cleanup finishes before the exit is reported, credentials are removed or a new generation is
// admitted. A namespace whose processes cannot be observed is not safe to reuse: fail the launcher
// loudly so Kubernetes replaces this role container rather than reporting a false completed stop.
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

func (m *manager) endGeneration(active *child, grace time.Duration) error {
	if err := signalGeneration(active.pid, syscall.SIGTERM); err != nil {
		return err
	}
	select {
	case <-active.done:
	case <-time.After(grace):
		if err := signalGeneration(active.pid, syscall.SIGKILL); err != nil {
			return err
		}
		<-active.done
	}
	return nil
}
