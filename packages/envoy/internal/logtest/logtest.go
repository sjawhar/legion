// Package logtest makes a test's handler the default logger's for the test's lifetime, and holds
// Recorder, the handler a test captures records with.
// slog.SetDefault also points the log package's output at that handler and clears its flags, and
// setting the previous default logger back undoes neither, so the restore puts the log package's
// output and flags back as well: without that, every log.Print after the test writes into its
// finished buffer instead of stderr. Tests across this module capture the default logger this way,
// so it lives here once; it is an ordinary package rather than a _test.go file because Go cannot
// share test-only code across packages (see internal/cputest's identical reasoning). The locked
// buffer those handlers write into lives here too, because a test that swaps the default logger
// needs a sink the code under test and the test itself may hold at the same time.
package logtest

import (
	"context"
	"log"
	"log/slog"
	"slices"
	"sync"
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

// Recorder is the slog.Handler a test reads its code's own log records through, rather than a
// rendering of them: every record written while it is the default logger's handler is kept, with
// its attributes as the code logged them. It is a handler for SetDefault and Swap, and each
// package reads Records back in its own vocabulary.
//
// Its zero value is ready, and the goroutines a package logs from may share it: Handle takes a
// lock and clones the record, which slog requires of a handler that keeps one past the call.
type Recorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *Recorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *Recorder) Handle(_ context.Context, record slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record.Clone())
	return nil
}

func (r *Recorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *Recorder) WithGroup(string) slog.Handler      { return r }

// Records are the records kept so far, in the order they were written, as a slice of the
// caller's own, so one written while the caller reads it changes nothing it holds.
func (r *Recorder) Records() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.records)
}
