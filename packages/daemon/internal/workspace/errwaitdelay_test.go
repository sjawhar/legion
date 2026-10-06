package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// Go's os/exec returns ErrWaitDelay only once the process itself has already exited 0 and only
// the I/O copying goroutines outlived WaitDelay (Cmd.Run's doc): `sh` itself exits (`exit 0`)
// within milliseconds, nowhere near the command's own generous 5 s timeout, but `sleep 3 &`
// backgrounds a grandchild that keeps sh's inherited stdout pipe open after — so Wait does not
// return until execRunner's own procgroup.WaitDelay gives up on draining it. execRunner must
// still report the real exit (0): the command answered, and a lingering process it started
// earlier is not a reason to treat the answer as a failure.
func TestExecRunnerReportsErrWaitDelayAsTheCommandsRealExitStatus(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("test needs a real sh: %v", err)
	}
	run := NewRunner(5*time.Second, map[string]string{"sh": sh})
	argvLeavingAPID := func(pidFile string) []string {
		return []string{"sh", "-c", "echo done; sleep 3 & echo $! >" + pidFile + "; exit 0"}
	}

	pidFile1 := filepath.Join(t.TempDir(), "sleep.pid")
	started := time.Now()
	result, err := run.Run(context.Background(), Command{Argv: argvLeavingAPID(pidFile1), Timeout: run.Timeout()})
	elapsed := time.Since(started)
	t.Cleanup(func() { killPIDFile(t, pidFile1) })
	if err != nil {
		t.Fatalf("Run = %v, want no error: the command exited 0 on its own", err)
	}
	if elapsed < procgroup.WaitDelay || elapsed > procgroup.WaitDelay+2*time.Second {
		t.Fatalf("Run took %s, want close to its %s WaitDelay (sh itself exits in milliseconds; its 5 s command timeout never fires a kill)", elapsed, procgroup.WaitDelay)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0: sh itself really did exit 0", result.ExitCode)
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false: the command's own 5 s timeout never had reason to fire")
	}

	pidFile2 := filepath.Join(t.TempDir(), "sleep.pid")
	t.Cleanup(func() { killPIDFile(t, pidFile2) })
	if checked, err := RunChecked(context.Background(), run, argvLeavingAPID(pidFile2), nil, ""); err != nil {
		t.Errorf("RunChecked = %v, want no error: the command answered, a lingering process it started is not a failure", err)
	} else if checked.ExitCode != 0 {
		t.Errorf("RunChecked's ExitCode = %d, want 0", checked.ExitCode)
	}
}

// killPIDFile kills the process named by the PID pidFile holds, left running past this test's own
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
