// Package wait names the one outcome of a side effect that is neither success nor failure: the
// effect cannot act yet, and will once something else happens — a turn ends, a stop lands, a
// tree's cleanup confirms. An error that is a wait (errors.Is(err, ErrWaiting)) is retried on the
// outbox's backoff and logged as a wait, never as a failure; each wait keeps its own sentinel or
// message, so its callers still tell one wait from another.
package wait

import (
	"errors"
	"fmt"
)

// ErrWaiting is what every wait matches.
var ErrWaiting = errors.New("waiting")

// New is a wait sentinel with text as its message: errors.Is matches it by identity, as any
// sentinel, and ErrWaiting too.
func New(text string) error {
	return &waiting{err: errors.New(text)}
}

// Errorf is a wait whose message, and whatever its %w wraps, are fmt.Errorf's.
func Errorf(format string, args ...any) error {
	return &waiting{err: fmt.Errorf(format, args...)}
}

type waiting struct{ err error }

func (w *waiting) Error() string { return w.err.Error() }

func (w *waiting) Unwrap() error { return w.err }

func (w *waiting) Is(target error) bool { return target == ErrWaiting }
