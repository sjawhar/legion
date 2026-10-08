// packages/envoy/cmd/agent-secrets/secret_prompt_darwin.go
//go:build darwin && (amd64 || arm64)

package main

import "golang.org/x/sys/unix"

// The terminal calls the value prompt makes on macOS (secret_prompt_unix.go).
const (
	ioctlGetTermios      = unix.TIOCGETA  // read the terminal's settings
	ioctlSetTermios      = unix.TIOCSETA  // set them, keeping its unread input
	ioctlSetTermiosFlush = unix.TIOCSETAF // set them and discard its unread input
	disabledControlByte  = 0xff           // Darwin's _POSIX_VDISABLE
)
