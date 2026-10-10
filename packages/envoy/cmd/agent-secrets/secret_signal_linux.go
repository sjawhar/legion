//go:build linux && (amd64 || arm64)

package main

import (
	"fmt"
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

// promptSignalIgnored reads the kernel disposition, including SIG_IGN inherited
// for job-control signals that os/signal's runtime bookkeeping does not record.
func promptSignalIgnored(sig syscall.Signal) (bool, error) {
	var old linuxSigaction
	if err := sigaction(sig, nil, &old); err != nil {
		return false, fmt.Errorf("read the handler of %s: %w", sig, err)
	}
	return old.handler == 1, nil // SIG_IGN
}

// stopBy stops the process by the job-control signal sig at its default action, and returns once
// SIGCONT resumes it, with its handler restored. It sends sig to the calling thread
// alone, so the stop takes effect as that thread returns from the call, before any more of the
// prompt runs. Go's runtime leaves these signals at its own ignoring default even after
// signal.Reset, so stopBy installs SIG_DFL itself, and puts back the handler it replaced
// directly: making sig ignored on the way back, as signal.Ignore does, would discard a stop still
// pending on another thread. In an orphaned process group the kernel discards the stop, and the
// prompt goes on reading.
func stopBy(sig syscall.Signal) error {
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

// quitWithoutCore selects the kernel's SIGQUIT action instead of Go's stack dump.
// Core dumps have already been disabled before reading the value.
func quitWithoutCore() error {
	var dfl linuxSigaction
	if err := sigaction(syscall.SIGQUIT, &dfl, nil); err != nil {
		return fmt.Errorf("set SIG_DFL for %s: %w", syscall.SIGQUIT, err)
	}
	return nil
}
