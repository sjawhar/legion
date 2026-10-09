// packages/envoy/cmd/agent-secrets/secret_prompt_darwin.go
//go:build darwin && (amd64 || arm64)

package main

import "golang.org/x/sys/unix"

// The terminal calls the value prompt makes on macOS (secret_prompt_unix.go).
const (
	ioctlGetTermios      = unix.TIOCGETA  // read the terminal's settings
	ioctlSetTermios      = unix.TIOCSETA  // set them, keeping its unread input
	ioctlSetTermiosFlush = unix.TIOCSETAF // set them and discard its unread input
)

// discardInput discards what the terminal holds unread (TIOCFLUSH with FREAD, 1 in XNU's
// sys/fcntl.h; bsd/kern/tty.c), as the kernel's own signal handling would under ISIG. It has not
// been exercised on a Darwin machine.
func discardInput(fd int) error {
	return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, 1)
}

// processGroupOrphaned reports whether this process's group is orphaned: its job-control count,
// which XNU keeps per group and reads as orphaned at zero (bsd/kern/tty.c), is 0. No shell can
// then hand the group the terminal. It has not been exercised on a Darwin machine.
func processGroupOrphaned() (bool, error) {
	proc, err := unix.SysctlKinfoProc("kern.proc.pid", unix.Getpid())
	if err != nil {
		return false, err
	}
	return proc.Eproc.Jobc == 0, nil
}
