//go:build darwin

package main

import (
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// darwinSigaction is Darwin's sigaction layout: a handler union, trampoline, signal mask and flags.
type darwinSigaction struct {
	handler uintptr
	tramp   uintptr
	mask    uint32
	flags   int32
}

// resetJobControlSignalDefault installs SIG_DFL for a job-control signal. Go's runtime otherwise
// leaves SIGTSTP, SIGTTIN and SIGTTOU at its ignored default after signal.Reset, so re-sending one
// would not stop the process.
func resetJobControlSignalDefault(sig syscall.Signal) error {
	var action darwinSigaction // handler 0 is SIG_DFL.
	_, _, errno := unix.Syscall(
		unix.SYS_SIGACTION,
		uintptr(sig),
		uintptr(unsafe.Pointer(&action)),
		0,
	)
	if errno != 0 {
		return fmt.Errorf("set SIG_DFL for %s: %w", sig, errno)
	}
	return nil
}
