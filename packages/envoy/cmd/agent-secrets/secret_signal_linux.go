//go:build linux

package main

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// linuxSigaction is Linux's kernel rt_sigaction layout on the agent-secrets release architectures.
type linuxSigaction struct {
	handler  uintptr
	flags    uint64
	restorer uintptr
	mask     uint64
}

// sigaction sets sig's action to act and, when old is not nil, stores the action it replaced. A
// zero linuxSigaction is SIG_DFL with an empty mask.
func sigaction(sig syscall.Signal, act, old *linuxSigaction) error {
	_, _, errno := unix.RawSyscall6(
		unix.SYS_RT_SIGACTION,
		uintptr(sig),
		uintptr(unsafe.Pointer(act)),
		uintptr(unsafe.Pointer(old)),
		unsafe.Sizeof(act.mask),
		0,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// stopBy stops the process by the job-control signal sig at its default action, and returns once
// SIGCONT resumes it, with sig still delivered to signals. It sends sig to the calling thread
// alone, so the stop takes effect as that thread returns from the call, before any more of the
// prompt runs. Go's runtime leaves these signals at its own ignoring default even after
// signal.Reset, so stopBy installs SIG_DFL itself, and puts back the handler it replaced
// directly: making sig ignored on the way back, as signal.Ignore does, would discard a stop still
// pending on another thread. In an orphaned process group the kernel discards the stop, and the
// prompt goes on reading.
func stopBy(sig syscall.Signal, _ chan<- os.Signal) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var dfl, old linuxSigaction
	if err := sigaction(sig, &dfl, &old); err != nil {
		return fmt.Errorf("set SIG_DFL for %s: %w", sig, err)
	}
	stopErr := unix.Tgkill(unix.Getpid(), unix.Gettid(), sig)
	if err := sigaction(sig, &old, nil); err != nil {
		return fmt.Errorf("put back the handler of %s: %w", sig, err)
	}
	if stopErr != nil {
		return fmt.Errorf("stop by %s: %w", sig, stopErr)
	}
	return nil
}

// quitWithoutCore makes the process one the kernel writes no core of, whatever its core limit or
// core pattern says, and sets SIGQUIT to its default action, so a SIGQUIT sent next ends the
// process by that signal rather than by Go's own SIGQUIT handler, which writes every goroutine's
// stack and exits 2.
func quitWithoutCore() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("turn core dumps off: %w", err)
	}
	var dfl linuxSigaction
	if err := sigaction(syscall.SIGQUIT, &dfl, nil); err != nil {
		return fmt.Errorf("set SIG_DFL for %s: %w", syscall.SIGQUIT, err)
	}
	return nil
}
