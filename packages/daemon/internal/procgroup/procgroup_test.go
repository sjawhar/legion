package procgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Configure's Cancel runs in the race window between ctx firing and the process finishing on its
// own: by the time the signal reaches it, there may be nothing left to kill. exec's watchCtx
// goroutine treats any Cancel error other than one equivalent to os.ErrProcessDone as a real
// cancellation failure — wrapping it "exec: canceling Cmd: <err>" and returning that from Wait
// even when the command's own exit was a clean success (exec.go's watchCtx) — so a raw ESRCH from
// the kill syscall must be translated, not passed through.
func TestConfigureCancelMapsESRCHToErrProcessDone(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "true")
	Configure(cmd, time.Second)
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

func TestHeldOpenIsTrueOnlyForErrWaitDelay(t *testing.T) {
	if !HeldOpen(exec.ErrWaitDelay) {
		t.Error("HeldOpen(exec.ErrWaitDelay) = false, want true")
	}
	if !HeldOpen(fmt.Errorf("run widget: %w", exec.ErrWaitDelay)) {
		t.Error("HeldOpen on a wrapped ErrWaitDelay = false, want true (errors.Is sees through %w)")
	}
	if HeldOpen(nil) {
		t.Error("HeldOpen(nil) = true, want false")
	}
	if HeldOpen(&exec.ExitError{}) {
		t.Error("HeldOpen(*exec.ExitError{}) = true, want false: an ordinary exit is not this case")
	}
}

func TestHeldOpenMessageNamesTheCommand(t *testing.T) {
	got := HeldOpenMessage("git clone --bare https://example.invalid/widgets.git")
	want := "a process git clone --bare https://example.invalid/widgets.git started held its output open past its WaitDelay"
	if got != want {
		t.Errorf("HeldOpenMessage = %q, want %q", got, want)
	}
}
