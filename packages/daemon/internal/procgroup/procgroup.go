// Package procgroup makes an exec.Cmd die as a unit when its context ends, instead of leaving a
// child's own child (a helper it spawned, reparented once the direct child is gone) holding its
// pipes open forever.
package procgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// WaitDelay is the one value Configure sets: how long Wait keeps waiting, once the command's own
// leader process has exited, for whatever still holds its output pipes open to close them on its
// own, before giving up and forcing the pipes closed itself. After the group kill (Cancel's
// SIGKILL), that is every process in the group, already dying together, so the wait is usually
// brief — except for a descendant that escaped the group itself by calling setsid (never git's or
// jj's own background maintenance, which daemonizes with its descriptors pointed elsewhere, but
// nothing stops another command from spawning one): the kill reaches only the group's own
// members, so that descendant survives it too, and Run then ends only at the command's own
// deadline plus WaitDelay. After the leader's own clean exit, Cancel never ran at all: a child the
// leader started earlier is untouched and keeps running on its own schedule, holding the pipe
// open for as long as it likes, the same as an escaped descendant. WaitDelay does not stop any of
// these processes — only Go's own wait for them — so it needs no margin for slow work, only
// enough to not block on one indefinitely; one second serves a probe and a git clone alike.
const WaitDelay = time.Second

// Configure sets cmd's own process group (SysProcAttr.Setpgid), a Cancel that signals the whole
// group instead of the single process exec.CommandContext would kill alone, and WaitDelay as the
// backstop that stops Wait from blocking forever on a pipe something still holds open once cmd's
// own process has exited, cleanly or by the group kill. Without this, a helper a command's own
// child spawns (git's git-remote-https, under an https remote) can keep a pipe open forever,
// reparented, after exec.CommandContext's own kill ends only the direct child — leaving Wait, and
// so Run, never returning.
//
// Call Configure before setting any of the caller's own SysProcAttr fields (a terminal foreground
// job's Ctty, for one): it replaces cmd.SysProcAttr whole, and the caller's own fields need to
// land on the same struct afterward.
func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			// The group was already gone by the time the signal reached it — cmd's own process
			// exited in the race window between ctx firing and this call. exec's watchCtx goroutine
			// treats any other error here as a real cancellation failure, wrapping it as "exec:
			// canceling Cmd: <err>" and returning that from Wait even when the command's own exit
			// was a clean success; it special-cases exactly one answer to mean "nothing to cancel,
			// the process already finished on its own" — an error equivalent to os.ErrProcessDone.
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = WaitDelay
}

// Err turns cmd.Run's error into what a caller should propagate: nil whenever cmd has its own
// real exit to report through cmd.ProcessState.ExitCode() instead — whether that is an ordinary
// *exec.ExitError or exec.ErrWaitDelay (a clean exit whose I/O draining outlived WaitDelay; Go
// returns this only once the process itself already exited, Cmd.Run's doc). Any other error (the
// command never started, or Run failed some other way) is returned unchanged, for the caller to
// propagate as its own.
func Err(err error) error {
	if err == nil {
		return nil
	}
	var exited *exec.ExitError
	if errors.As(err, &exited) || errors.Is(err, exec.ErrWaitDelay) {
		return nil
	}
	return err
}
