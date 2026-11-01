package pmdoc

import (
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
)

// ErrPanic is a panic in this package's reader or renderer, recovered at the entry point that
// caught it: a bug here, never a shape of the caller's markdown, so callers answer it as an internal
// error. Its text begins with "panic: ".
var ErrPanic = errors.New("panic")

// recoverPanic, deferred around an entry point that reads or writes markdown, turns a panic in it
// into an ErrPanic, so the bug fails that one call rather than the process, and logs the panic with
// its stack.
func recoverPanic[T any](result *T, err *error, reading string) {
	if recovered := recover(); recovered != nil {
		var zero T
		*result = zero
		*err = fmt.Errorf("%w: %s: %v", ErrPanic, reading, recovered)
		slog.Error("pmdoc: "+reading+" panicked", "panic", recovered, "stack", string(debug.Stack()))
	}
}
