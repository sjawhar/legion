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

// WaitDelay is the one value every Configure caller passes: how long the backstop waits, once a
// command's own process has already died (by its own exit or the group kill), for whatever still
// holds its pipes open to close them; past this, Wait forces the pipes closed itself rather than
// block forever. It needs no margin for the command's own slow work — that's what its context
// deadline and the group kill are for — only enough for an already-dead process tree's file
// descriptors to actually close, so one second serves a probe and a git clone alike.
const WaitDelay = time.Second

// Configure sets cmd's own process group (SysProcAttr.Setpgid), a Cancel that signals the whole
// group instead of the single process exec.CommandContext would kill alone, and waitDelay as the
// backstop that force-closes cmd's pipes if something still holds them open once Cancel has
// fired. Without this, a helper a command's own child spawns (git's git-remote-https, under an
// https remote) can keep a pipe open forever, reparented, after exec.CommandContext's own kill
// ends only the direct child — leaving Wait, and so Run, never returning.
//
// Call Configure before setting any of the caller's own SysProcAttr fields (a terminal foreground
// job's Ctty, for one): it replaces cmd.SysProcAttr whole, and the caller's own fields need to
// land on the same struct afterward.
func Configure(cmd *exec.Cmd, waitDelay time.Duration) {
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
	cmd.WaitDelay = waitDelay
}

// HeldOpen reports whether err is exec.ErrWaitDelay: cmd exited on its own, but a process it
// started (directly or not) kept an output pipe open past its WaitDelay, so Wait gave up draining
// it and forced the pipes closed instead. Go reports this only once the command's own exit status
// was already a clean 0 (Cmd.Run's doc on WaitDelay): read in isolation, that exit code looks like
// an ordinary successful run, which is why every caller that inspects one of Configure's commands
// should check HeldOpen before trusting it as one.
func HeldOpen(err error) bool {
	return errors.Is(err, exec.ErrWaitDelay)
}

// HeldOpenMessage is HeldOpen's own accurate description of what happened to command (the argv or
// launch string a caller names it by): not a failure code the process itself never reported, and
// not silence that lets the exit look like nothing unusual happened.
func HeldOpenMessage(command string) string {
	return "a process " + command + " started held its output open past its WaitDelay"
}
