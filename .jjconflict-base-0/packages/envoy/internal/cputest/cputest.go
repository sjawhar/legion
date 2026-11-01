// Package cputest reads this process's own CPU time, the measure every growth test in this module
// times a call by. A call's wall time counts every moment the scheduler leaves the process off a
// core, so a loaded machine stretches it on either side of a comparison without the call doing more
// work; CPU time leaves that out. It is not immune to load: a busy machine still slows the
// process's own CPU time through shared caches and clock frequency, so a comparison still samples
// each side more than once. pmdoc's linear-time tests and the api's save ladders both time calls
// this way, so it lives here once; it is an ordinary package rather than a _test.go file because
// Go cannot share test-only code across packages (see internal/stacktest's identical reasoning).
package cputest

import (
	"fmt"
	"sync"
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

// resolution is the smallest step Process moves in on this machine, measured once: the fewest
// nanoseconds between two readings that differ, over resolutionSamples spins. A Linux kernel moves
// it in steps far under a millisecond; gVisor moves it in 10 ms steps, so there a call shorter than
// 10 ms reads as 0 or 10 ms.
var resolution = sync.OnceValue(func() time.Duration {
	var step time.Duration
	for range resolutionSamples {
		start := Process()
		next := Process()
		for next == start {
			next = Process()
		}
		if step == 0 || next-start < step {
			step = next - start
		}
	}
	return step
})

// resolutionSamples is how many changes of Process resolution waits for. A sample that also
// counts another goroutine's CPU time reads a larger step, never a smaller one, so the smallest
// of a few is the clock's own step.
const resolutionSamples = 5

// windowSteps is how many steps of resolution a timed window must last before Measure divides it
// by its runs. A reading is off by up to one step at each end, so this keeps that error near 4% of
// the reading, where a single call shorter than a step reads as 0 or as one whole step.
const windowSteps = 50

// Measure returns the CPU time one run of an operation takes. batch runs the operation runs times
// in one timed window and returns the window's CPU time (Process after less Process before); it
// builds every run's input before the window opens, a fresh one for each run of an operation
// that changes its input, and returns an error that makes the window's time meaningless. Measure
// calls batch with more runs until a window lasts windowSteps steps of resolution, then divides
// that window's time by its runs. A window shorter than that is discarded, never averaged in.
func Measure(batch func(runs int) (time.Duration, error)) (time.Duration, error) {
	want := windowSteps * resolution()
	for runs := 1; ; {
		took, err := batch(runs)
		if err != nil {
			return took / time.Duration(runs), err
		}
		if took >= want {
			return took / time.Duration(runs), nil
		}
		if took <= 0 {
			runs *= 10
			continue
		}
		// Aim a quarter past the window, so a run that takes a little longer than this one's
		// average still lands the next window past it.
		runs = max(runs*2, int(int64(runs)*int64(want)*5/(int64(took)*4))+1)
	}
}
