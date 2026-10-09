// packages/envoy/cmd/agent-secrets/secret_prompt_linux.go
//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
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

// procStat is the part of /proc/<pid>/stat the orphan rule reads.
type procStat struct {
	pid, ppid, pgrp, session int
	state                    byte
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
	if len(fields) < 4 || len(fields[0]) != 1 {
		return procStat{}, fmt.Errorf("read /proc/%d/stat: %q", pid, data)
	}
	s := procStat{pid: pid, state: fields[0][0]}
	for i, field := range []*int{&s.ppid, &s.pgrp, &s.session} {
		if *field, err = strconv.Atoi(string(fields[i+1])); err != nil {
			return procStat{}, fmt.Errorf("read /proc/%d/stat: %w", pid, err)
		}
	}
	return s, nil
}
