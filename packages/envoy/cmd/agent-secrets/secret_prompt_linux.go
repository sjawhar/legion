// packages/envoy/cmd/agent-secrets/secret_prompt_linux.go

package main

import "golang.org/x/sys/unix"

// The terminal calls the value prompt makes on Linux (secret_prompt_unix.go).
const (
	ioctlGetTermios      = unix.TCGETS  // read the terminal's settings
	ioctlSetTermios      = unix.TCSETS  // set them, keeping its unread input
	ioctlSetTermiosFlush = unix.TCSETSF // set them and discard its unread input
	disabledControlByte  = 0            // Linux's _POSIX_VDISABLE
)
