package procgroup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
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
	killed := Configure(cmd)
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
	if killed() {
		t.Error("killed() = true after a Cancel that found nothing left to signal")
	}
}

// killed is true when the end of the command's context made Cancel signal its group, and false
// for a command whose own process exited first, though WaitDelay then held Run open past the
// context's deadline for a child still holding the command's output pipe. The second case's
// deadline is 100 ms short of WaitDelay, so its Run always returns past it, while the leader still
// has 900 ms to exit under load: a caller that read a timeout from the deadline having passed would
// call that command's own exit a timeout.
func TestKilledIsWhetherTheContextsEndSignaledTheGroup(t *testing.T) {
	t.Run("cut off by its deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sleep", "30")
		killed := Configure(cmd)

		err := cmd.Run()

		if cmd.Process == nil {
			t.Fatalf("Run = %v before the command started: the deadline passed before Start, so this run proves nothing", err)
		}
		if !killed() {
			t.Errorf("killed() = false for a command its deadline cut off (Run = %v)", err)
		}
	})
	t.Run("exited before its deadline, a child holding its pipe past it", func(t *testing.T) {
		budget := WaitDelay - 100*time.Millisecond
		ctx, cancel := context.WithTimeout(context.Background(), budget)
		defer cancel()
		cmd := exec.CommandContext(ctx, "sh", "-c", "sleep 30 & exit 0")
		var out bytes.Buffer
		cmd.Stdout = &out
		killed := Configure(cmd)

		started := time.Now()
		err := cmd.Run()
		elapsed := time.Since(started)
		if cmd.Process == nil {
			t.Fatalf("Run = %v before the command started: the deadline passed before Start, so this run proves nothing", err)
		}
		// The backgrounded sleep is still in the command's own process group, which outlives the
		// reaped leader while it does.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

		// The wall clock, not ctx.Err(): under load the context's own timer can fire after Run has
		// returned, and what this run needs is that the drain outlasted the deadline.
		if elapsed < budget {
			t.Fatalf("Run = %v returned after %s, inside its %s deadline: nothing held the pipe past it, so this run proves nothing", err, elapsed, budget)
		}
		if killed() {
			t.Errorf("killed() = true for a command that exited on its own before its deadline (Run = %v)", err)
		}
		if !errors.Is(err, exec.ErrWaitDelay) {
			t.Errorf("Run = %v, want exec.ErrWaitDelay: the leader exited 0 and the sleep held the pipe past WaitDelay", err)
		}
	})
}

// A Cancel whose signal fails with anything but "already gone" still counts as the context's end
// killing the command: exec then kills the process itself once WaitDelay has passed (watchCtx).
// A killed that counted only a Cancel that succeeded would call a command its deadline ended an
// exit of its own, exit -1, where the old reading of the deadline called it a timeout.
func TestKilledCountsACancelWhoseSignalFailed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sleep", "30")
	cmd.Cancel = func() error { return syscall.EPERM }
	cmd.WaitDelay = WaitDelay
	killed := KilledByContext(cmd)

	err := cmd.Run()

	if cmd.Process == nil {
		t.Fatalf("Run = %v before the command started: the deadline passed before Start, so this run proves nothing", err)
	}
	if !killed() {
		t.Errorf("killed() = false for a command its deadline ended though Cancel's signal failed (Run = %v)", err)
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
