// packages/envoy/cmd/agent-secrets/secret_prompt_linux.go
//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// The terminal calls the value prompt makes on Linux (secret_prompt_unix.go).
const (
	ioctlGetTermios      = unix.TCGETS  // read the terminal's settings
	ioctlSetTermios      = unix.TCSETS  // set them, keeping its unread input
	ioctlSetTermiosFlush = unix.TCSETSF // set them and discard its unread input
)

// discardInput discards what the terminal holds unread (TCFLSH TCIFLUSH, the kernel's
// tty_ioctl.c), as the kernel's own signal handling would under ISIG.
func discardInput(fd int) error {
	return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}

// processGroupOrphaned reports whether this process's group is orphaned, by the kernel's own rule
// (kernel/exit.c, will_become_orphaned_pgrp): no member but a zombie has a parent in another group
// of the same session. No shell can then hand the group the terminal. The shell that started the
// job is usually the parent of this process or of a wrapper in its group, so the walk up its
// ancestors settles it in a few reads; only a group no ancestor holds is scanned for in /proc.
func processGroupOrphaned() (bool, error) {
	self, err := readProcStat(unix.Getpid())
	if err != nil {
		return false, err
	}
	for member := self; ; {
		parent, err := readProcStat(member.ppid)
		if err != nil || parent.pgrp != self.pgrp {
			if err == nil && parent.session == self.session {
				return false, nil
			}
			break
		}
		member = parent
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self.pid {
			continue
		}
		member, err := readProcStat(pid)
		if err != nil || member.pgrp != self.pgrp || member.state == 'Z' {
			continue // gone since the listing, another group's, or a zombie
		}
		if parent, err := readProcStat(member.ppid); err == nil && parent.pgrp != self.pgrp && parent.session == self.session {
			return false, nil
		}
	}
	return true, nil
}

// pfExiting is PF_EXITING (include/linux/sched.h), which /proc/<pid>/stat reports in its flags
// field (fs/proc/array.c) from the start of a task's exit (kernel/exit.c, do_exit's exit_signals).
const pfExiting = 0x4

// sessionLeaderGone reports whether this process's session leader has exited or begun to. From
// then on the session has no controlling terminal and can never take one again: do_exit sets
// PF_EXITING before disassociate_ctty clears the session's terminal (kernel/exit.c), and only a
// session leader can take a terminal. While this process stays in the session its leader's
// PID stays the session's (kernel/pid.c frees a PID only once no task uses it), so it names
// no other process. A leader outside this process's PID namespace has no number here (getsid
// answers 0: kernel/sys.c, pid_vnr), so nothing can be read of it and it counts as present.
func sessionLeaderGone() (bool, error) {
	sid, err := unix.Getsid(0)
	if err != nil {
		return false, err
	}
	if sid == 0 || sid == unix.Getpid() {
		return false, nil
	}
	leader, err := readProcStat(sid)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return true, nil // reaped
	}
	if err != nil {
		return false, err
	}
	return leader.state == 'Z' || leader.state == 'X' || leader.flags&pfExiting != 0, nil
}

// procStat is the part of /proc/<pid>/stat the orphan and session rules read.
type procStat struct {
	pid, ppid, pgrp, session int
	state                    byte
	flags                    uint64
}

func readProcStat(pid int) (procStat, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return procStat{}, err
	}
	// The command name, in parentheses, can hold spaces and parentheses of its own.
	end := bytes.LastIndexByte(data, ')')
	if end < 0 || end+2 > len(data) {
		return procStat{}, fmt.Errorf("read /proc/%d/stat: no command name", pid)
	}
	fields := bytes.Fields(data[end+2:])
	if len(fields) < 7 || len(fields[0]) != 1 {
		return procStat{}, fmt.Errorf("read /proc/%d/stat: %q", pid, data)
	}
	s := procStat{pid: pid, state: fields[0][0]}
	for i, field := range []*int{&s.ppid, &s.pgrp, &s.session} {
		if *field, err = strconv.Atoi(string(fields[i+1])); err != nil {
			return procStat{}, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
		}
	}
	if s.flags, err = strconv.ParseUint(string(fields[6]), 10, 64); err != nil {
		return procStat{}, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
	}
	return s, nil
}
