package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func preventCoreDumps() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("turn core dumps off: %w", err)
	}
	return nil
}
