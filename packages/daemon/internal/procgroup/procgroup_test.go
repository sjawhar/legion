package procgroup

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
)

// Configure's Cancel runs in the race window between ctx firing and the process finishing on its
// own: by the time the signal reaches it, there may be nothing left to kill. exec's watchCtx
// goroutine treats any Cancel error other than one equivalent to os.ErrProcessDone as a real
// cancellation failure — wrapping it "exec: canceling Cmd: <err>" and returning that from Wait
// even when the command's own exit was a clean success (exec.go's watchCtx) — so a raw ESRCH from
// the kill syscall must be translated, not passed through.
func TestConfigureCancelMapsESRCHToErrProcessDone(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "true")
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start a trivial command: %v", err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		t.Fatalf("wait for it to exit on its own: %v", err)
	}
	// The process, and its whole group (it is its own, and forked nothing), is already reaped:
	// Cancel's kill(-pid, SIGKILL) now finds no such process or group.
	if err := cmd.Cancel(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("Cancel() on an already-finished command = %v, want an error equivalent to os.ErrProcessDone", err)
	}
}

// Configure takes no waitDelay argument: both callers always want the same bound, so Configure
// sets it from the package's own WaitDelay constant rather than asking each caller to pass it.
func TestConfigureSetsThePackagesOwnWaitDelay(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "true")
	Configure(cmd)
	if cmd.WaitDelay != WaitDelay {
		t.Errorf("cmd.WaitDelay = %s, want the package's own WaitDelay (%s)", cmd.WaitDelay, WaitDelay)
	}
}
