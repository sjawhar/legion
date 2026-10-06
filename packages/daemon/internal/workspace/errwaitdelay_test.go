package workspace

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/procgroup"
)

// Go's os/exec returns ErrWaitDelay only once the process itself has already exited 0 and only
// the I/O copying goroutines outlived WaitDelay (Cmd.Run's doc): `sh` itself exits (`exit 0`)
// within milliseconds, nowhere near the command's own generous 5 s timeout, but `sleep 8 &`
// backgrounds a grandchild that keeps sh's inherited stdout pipe open long after — so Wait does
// not return until execRunner's own procgroup.WaitDelay gives up on draining it. execRunner must
// report the exit truthfully as ExitCode 0 (matching the process's real exit, and
// bootgate.pluginGate.run's own choice for the same case) while still surfacing HeldOpen, since
// checkedResult treats that as a failure unconditionally — never silently exit 0, and never the
// confusing "command failed (exit -1)" an earlier, different choice reported for this same case.
func TestExecRunnerReportsErrWaitDelayAsItsRealExitZeroButStillHeldOpen(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("test needs a real sh: %v", err)
	}
	run := NewRunner(5*time.Second, map[string]string{"sh": sh})
	argv := []string{"sh", "-c", "echo done; sleep 8 & exit 0"}

	started := time.Now()
	result, err := run.Run(context.Background(), Command{Argv: argv, Timeout: run.Timeout()})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Run = %v, want no error (HeldOpen is reported through Result, not err)", err)
	}
	if elapsed < procgroup.WaitDelay || elapsed > procgroup.WaitDelay+2*time.Second {
		t.Fatalf("Run took %s, want close to its %s WaitDelay (sh itself exits in milliseconds; its 5 s command timeout never fires a kill)", elapsed, procgroup.WaitDelay)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0: sh itself really did exit 0", result.ExitCode)
	}
	if !result.HeldOpen {
		t.Error("HeldOpen = false, want true: the backgrounded sleep kept sh's stdout pipe open past ProcessGroupWaitDelay")
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false: the command's own 5 s timeout never had reason to fire")
	}

	if _, err := RunChecked(context.Background(), run, argv, nil, ""); err == nil {
		t.Fatal("RunChecked = nil error, want a failure despite the real exit 0")
	} else if !strings.Contains(err.Error(), "held its output open") {
		t.Errorf("RunChecked error = %v, want it to say a process held its output open, not just report exit 0", err)
	}
}
