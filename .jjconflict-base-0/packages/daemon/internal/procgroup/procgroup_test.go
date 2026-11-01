package procgroup

import (
	"context"
	"errors"
	"fmt"
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

// Err is the one place both execRunner and the boot gate's probe runner read a command's real
// exit: nil for an ordinary exit (whatever its own code) and for exec.ErrWaitDelay (a clean exit
// whose I/O draining outlived WaitDelay), seen through a wrapped error too — a caller then reads
// cmd.ProcessState.ExitCode() for both. Had Err instead let exec.ErrWaitDelay through unchanged, a
// caller would propagate it as a real error, reporting a command that actually exited 0 as
// "run git: exec: WaitDelay expired before I/O complete" rather than reading its own exit code.
// Anything else — the command never started, or Run failed some other way — is unchanged.
func TestErrRecognizesAnOrdinaryExitAndErrWaitDelayButNothingElse(t *testing.T) {
	exited := exec.Command("false").Run()
	if exited == nil {
		t.Fatal("exec.Command(\"false\").Run() = nil, want an *exec.ExitError to test against")
	}
	if err := Err(nil); err != nil {
		t.Errorf("Err(nil) = %v, want nil", err)
	}
	if err := Err(exited); err != nil {
		t.Errorf("Err(%v) = %v, want nil: an ordinary exit is not an error to propagate", exited, err)
	}
	if err := Err(exec.ErrWaitDelay); err != nil {
		t.Errorf("Err(exec.ErrWaitDelay) = %v, want nil", err)
	}
	if err := Err(fmt.Errorf("run widget: %w", exec.ErrWaitDelay)); err != nil {
		t.Errorf("Err on a wrapped ErrWaitDelay = %v, want nil (errors.Is sees through %%w)", err)
	}
	other := errors.New("workspace command is not a tool the daemon resolved at boot")
	if err := Err(other); err != other {
		t.Errorf("Err(%v) = %v, want it unchanged", other, err)
	}
}
