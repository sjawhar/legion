//go:build darwin && (amd64 || arm64)

package main

import (
	"fmt"
	"os/signal"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// darwinSigaction is the action Darwin's sigaction system call sets (its __sigaction): a handler
// union, trampoline, signal mask and flags. The action it answers back has no trampoline, so this
// layout is only ever written, never read back.
type darwinSigaction struct {
	handler uintptr
	tramp   uintptr
	mask    uint32
	flags   int32
}

// Darwin answers sigaction queries with struct sigaction, without the input
// structure's trampoline field.
type darwinOldSigaction struct {
	handler uintptr
	mask    uint32
	flags   int32
}

func promptSignalIgnored(sig syscall.Signal) (bool, error) {
	var old darwinOldSigaction
	if _, _, errno := unix.Syscall(unix.SYS_SIGACTION, uintptr(sig), 0, uintptr(unsafe.Pointer(&old))); errno != 0 {
		return false, fmt.Errorf("read the handler of %s: %w", sig, errno)
	}
	return old.handler == 1, nil // SIG_IGN
}

// setDefaultAction sets sig to its default action.
func setDefaultAction(sig syscall.Signal) error {
	var dfl darwinSigaction // handler 0 is SIG_DFL.
	if _, _, errno := unix.Syscall(unix.SYS_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(&dfl)), 0); errno != 0 {
		return fmt.Errorf("set SIG_DFL for %s: %w", sig, errno)
	}
	return nil
}

// stopBy stops the process by the job-control signal sig at its default action, and returns once
// SIGCONT resumes it. Go's runtime leaves this signal at its own ignoring default
// even after signal.Reset, so stopBy installs SIG_DFL itself. Darwin sends the
// signal to the process; signal.Ignore clears the runtime's handler bookkeeping
// so the watcher's subsequent signal.Notify reinstalls Go's handler. That Ignore
// discards a pending stop, so this relies on Darwin stopping before kill returns.
// The Darwin stop/resume path has not been exercised on a Darwin machine.
func stopBy(sig syscall.Signal) error {
	if err := setDefaultAction(sig); err != nil {
		return err
	}
	stopErr := unix.Kill(unix.Getpid(), sig)
	signal.Ignore(sig)
	if stopErr != nil {
		return fmt.Errorf("stop by %s: %w", sig, stopErr)
	}
	return nil
}

// quitWithoutCore selects the kernel's SIGQUIT action instead of Go's stack dump.
// Core dumps have already been disabled before reading the value.
func quitWithoutCore() error {
	return setDefaultAction(syscall.SIGQUIT)
}
