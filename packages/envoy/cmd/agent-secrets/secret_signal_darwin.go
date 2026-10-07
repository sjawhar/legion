//go:build darwin

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// darwinSigaction is the action Darwin's sigaction system call sets (its __sigaction): a handler
// union, trampoline, signal mask and flags. The action it answers back has no trampoline, so this
// layout is only ever written, never read back.
type darwinSigaction struct {
	handler uintptr
	tramp   uintptr
	mask    uint32
	flags   int32
}

// setDefaultAction sets sig to its default action.
func setDefaultAction(sig syscall.Signal) error {
	var dfl darwinSigaction // handler 0 is SIG_DFL.
	if _, _, errno := unix.Syscall(unix.SYS_SIGACTION, uintptr(sig), uintptr(unsafe.Pointer(&dfl)), 0); errno != 0 {
		return fmt.Errorf("set SIG_DFL for %s: %w", sig, errno)
	}
	return nil
}

// stopBy stops the process by the job-control signal sig at its default action, and returns once
// SIGCONT resumes it, with sig delivered to signals again. Go's runtime leaves these signals at its
// own ignoring default even after signal.Reset, so stopBy installs SIG_DFL itself. Darwin has no
// call that sends a signal to one thread of a process here, so stopBy sends sig to the process,
// then puts Go's handler back through os/signal: signal.Ignore clears the runtime's record that
// its handler is installed, so the signal.Notify after it installs the handler again. That
// signal.Ignore discards a stop still pending, so the order holds only if Darwin stops the
// process before kill returns to its caller. Unverified: this has run on no Darwin machine.
func stopBy(sig syscall.Signal, signals chan<- os.Signal) error {
	if err := setDefaultAction(sig); err != nil {
		return err
	}
	stopErr := unix.Kill(unix.Getpid(), sig)
	signal.Ignore(sig)
	signal.Notify(signals, sig)
	if stopErr != nil {
		return fmt.Errorf("stop by %s: %w", sig, stopErr)
	}
	return nil
}

// quitWithoutCore sets the process's core limit to 0, so the kernel writes no core of it, and sets
// SIGQUIT to its default action, so a SIGQUIT sent next ends the process by that signal rather than
// by Go's own SIGQUIT handler, which writes every goroutine's stack and exits 2.
func quitWithoutCore() error {
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		return fmt.Errorf("turn core dumps off: %w", err)
	}
	return setDefaultAction(syscall.SIGQUIT)
}
