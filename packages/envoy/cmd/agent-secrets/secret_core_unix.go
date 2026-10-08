//go:build unix && !linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func preventCoreDumps() error {
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		return fmt.Errorf("turn core dumps off: %w", err)
	}
	return nil
}
