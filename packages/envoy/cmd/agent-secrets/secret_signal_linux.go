//go:build linux

package main

import (
	"fmt"
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

// resetJobControlSignalDefault installs SIG_DFL for a job-control signal. Go's runtime otherwise
// leaves SIGTSTP, SIGTTIN and SIGTTOU at its ignored default after signal.Reset, so re-sending one
// would not stop the process.
func resetJobControlSignalDefault(sig syscall.Signal) error {
	var action linuxSigaction // handler 0 is SIG_DFL; the empty mask leaves every signal unblocked.
	_, _, errno := unix.RawSyscall6(
		unix.SYS_RT_SIGACTION,
		uintptr(sig),
		uintptr(unsafe.Pointer(&action)),
		0,
		unsafe.Sizeof(action.mask),
		0,
		0,
	)
	if errno != 0 {
		return fmt.Errorf("set SIG_DFL for %s: %w", sig, errno)
	}
	return nil
}
