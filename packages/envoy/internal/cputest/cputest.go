// Package cputest reads this process's own CPU time, the measure every growth test in this module
// times a call by: a loaded machine's scheduler stretches a call's wall time on either side of a
// comparison without the call doing more work, and this process's CPU time grows only with the work
// it and its garbage collection do. pmdoc's linear-time tests and the api's save ladders both time
// calls this way, so it lives here once; it is an ordinary package rather than a _test.go file
// because Go cannot share test-only code across packages (see internal/stacktest's identical
// reasoning).
package cputest

import (
	"fmt"
	"syscall"
	"time"
)

// Process is this process's total CPU time, user and system, since it started
// (getrusage(RUSAGE_SELF)). It counts every goroutine, so a test that times one call by it runs
// no other measured work beside that call: no test in the packages that use it calls t.Parallel.
func Process() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		panic(fmt.Sprintf("getrusage(RUSAGE_SELF): %v", err))
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
