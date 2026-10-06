package daemon

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/bootprobe"
	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// run shares its process-group handling with workspace.execRunner (both call procgroup.Configure):
// `sh` itself exits (`exit 0`) within milliseconds here, well short of the gate's own generous 5 s
// timeout, but `sleep 3 &` backgrounds a grandchild that keeps sh's inherited stdout pipe open
// after, so Wait does not return until procgroup.WaitDelay gives up draining it. run must still
// report the real exit (0): the command answered, and the lingering child is not its concern.
func TestPluginGateRunReportsErrWaitDelayAsTheCommandsRealExitStatus(t *testing.T) {
	pidFile := t.TempDir() + "/sleep.pid"
	g := pluginGate{
		env:     map[string]string{"PATH": os.Getenv("PATH")},
		workDir: t.TempDir(),
		timeout: 5 * time.Second,
		retry:   bootprobe.Retry{Initial: 10 * time.Millisecond, Max: 40 * time.Millisecond},
	}
	t.Cleanup(func() { killPIDFile(t, pidFile) })

	started := time.Now()
	r, err := g.run(context.Background(), "echo done; sleep 3 & echo $! >"+pidFile+"; exit 0")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("run = %v, want no error: the command exited 0 on its own", err)
	}
	if elapsed < procgroup.WaitDelay || elapsed > procgroup.WaitDelay+2*time.Second {
		t.Fatalf("run took %s, want close to its %s WaitDelay (sh itself exits in milliseconds; its 5 s timeout never fires a kill)", elapsed, procgroup.WaitDelay)
	}
	if r.exit != 0 {
		t.Errorf("exit = %d, want 0: sh itself really did exit 0", r.exit)
	}
	if r.timedOut {
		t.Error("timedOut = true, want false: the gate's own 5 s timeout never had reason to fire")
	}
}

// killPIDFile kills the process named by the PID pidFile holds, left running past a test's own
// assertions by design (a backgrounded sleep standing in for a command's own lingering child).
func killPIDFile(t *testing.T, pidFile string) {
	t.Helper()
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
