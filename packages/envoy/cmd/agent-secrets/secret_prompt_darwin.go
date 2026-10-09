// packages/envoy/cmd/agent-secrets/secret_prompt_darwin.go
//go:build darwin && (amd64 || arm64)

package main

import (
	"errors"

	"golang.org/x/sys/unix"
)

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

// The process states sessionLeaderGone reads (bsd/sys/proc.h): SZOMB in p_stat, and P_WEXIT in
// p_flag, which sysctl sets from P_LEXIT (bsd/kern/kern_sysctl.c, fill_externproc).
const (
	procZombie  = 5      // SZOMB
	procExiting = 0x2000 // P_WEXIT
)

// sessionLeaderGone reports whether this process's session leader has exited or begun to. From
// then on the session has no controlling terminal and can never take one again: the leader's
// exit revokes it (bsd/kern/kern_exit.c, proc_exit), and only a session leader can take a
// terminal. A reaped leader has no record, which SysctlKinfoProc answers with EIO. It has not been
// exercised on a Darwin machine.
func sessionLeaderGone() (bool, error) {
	sid, err := unix.Getsid(0)
	if err != nil {
		return false, err
	}
	if sid == unix.Getpid() {
		return false, nil
	}
	leader, err := unix.SysctlKinfoProc("kern.proc.pid", sid)
	if errors.Is(err, unix.EIO) {
		return true, nil // reaped
	}
	if err != nil {
		return false, err
	}
	return leader.Proc.P_stat == procZombie || leader.Proc.P_flag&procExiting != 0, nil
}
