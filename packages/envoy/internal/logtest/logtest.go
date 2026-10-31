// Package logtest makes a test's handler the default logger's for the test's lifetime.
// slog.SetDefault also points the log package's output at that handler and clears its flags, and
// setting the previous default logger back undoes neither, so the restore puts the log package's
// output and flags back as well: without that, every log.Print after the test writes into its
// finished buffer instead of stderr. Tests across this module capture the default logger this way,
// so it lives here once; it is an ordinary package rather than a _test.go file because Go cannot
// share test-only code across packages (see internal/cputest's identical reasoning).
package logtest

import (
	"log"
	"log/slog"
	"testing"
)

// SetDefault makes handler the default logger's until t ends, then puts back the previous default
// logger and the log package's output and flags.
func SetDefault(t testing.TB, handler slog.Handler) {
	t.Helper()
	Swap(t, handler)
}

// Swap is SetDefault for a test that puts the defaults back before it ends: it returns the
// function that does, which t's cleanup also calls. Calling it again sets back the same defaults.
func Swap(t testing.TB, handler slog.Handler) (restore func()) {
	t.Helper()
	previous, output, flags := slog.Default(), log.Writer(), log.Flags()
	restore = func() {
		slog.SetDefault(previous)
		log.SetOutput(output)
		log.SetFlags(flags)
	}
	slog.SetDefault(slog.New(handler))
	t.Cleanup(restore)
	return restore
}
