package admission

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/accesslog"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The codes a request is answered with when the server could not serve it: a refusal by a Limiter
// is a 429, and the two failures after admission are 503s. Each carries Retry-After and
// Cache-Control: no-store (writeRetryable).
const (
	// CodeOverloaded is a request a Limiter refused, answered 429 Too Many Requests: nothing of it
	// ran, so any method may be retried.
	CodeOverloaded = "OVERLOADED"
	// CodeDatabaseBusy is a request whose handler waited its pool's acquire bound for a database
	// connection (store.ErrPoolBusy), answered 503. A write may have committed part of what it does
	// before the wait that failed it.
	CodeDatabaseBusy = "DATABASE_BUSY"
	// CodeRequestTimeout is a read that ran past its deadline (WithDeadline), answered 503. Only
	// reads have a deadline: a write runs to completion once admitted.
	CodeRequestTimeout = "REQUEST_TIMEOUT"
)

// RetryAfter is the Retry-After, in seconds, of every refusal and 503 this package writes, and of
// the document writer gate's 429: one second, after which the burst that refused the request may
// have passed.
const RetryAfter = "1"

// ErrRequestDeadline is the cause of a read's context whose deadline passed (WithDeadline). It
// tells the read's own deadline from every other deadline its handler meets - a pool's acquire
// bound, a client's timeout of its own - so only it is answered REQUEST_TIMEOUT.
var ErrRequestDeadline = errors.New("the read ran past its deadline")

// WithDeadline bounds a read's context at timeout, with ErrRequestDeadline as the cause.
func WithDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, timeout, ErrRequestDeadline)
}

// DeadlinePassed reports whether ctx is a read's context whose own deadline has passed.
func DeadlinePassed(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrRequestDeadline)
}

// WriteRefused answers a request a Limiter refused (Admit's *Refusal) 429 OVERLOADED. A request
// that carries a body has it drained first, by its caller: this writes the answer only.
func WriteRefused(w http.ResponseWriter, refusal *Refusal) {
	writeRetryable(w, http.StatusTooManyRequests, CodeOverloaded, "the server has no room for another "+
		refusal.Class.String()+" request ("+string(refusal.Reason)+"); nothing ran, retry after Retry-After")
}

// WriteUnserved answers a request whose handler failed with err because the server could not
// serve it, and reports whether it did; on false the caller maps err as it always has. Its
// order is the order the causes are told apart:
//
//   - a request cancelled by a refusal of the units it asked back (Ticket.Resume, whose refusal
//     the caller made ctx's cause): 429 OVERLOADED, as a request refused before it ran is
//     answered. It is told first because that cancellation reads as a client that went away,
//     which it is not: the client is still there, and what it is owed is the refusal;
//   - the read's deadline passed (DeadlinePassed): 503 REQUEST_TIMEOUT, whatever err says,
//     since every failure after the deadline is the deadline's;
//   - the client went away (ctx cancelled): nothing is written, since nobody reads it, and the
//     access log records the request as 499 rather than as the 200 a handler that wrote nothing
//     would otherwise be logged as (accesslog.ClientGone);
//   - no connection of a pool came free within its acquire bound (store.IsPoolBusy): 503
//     DATABASE_BUSY. Nothing ran on the connection that never came, so the wait is all that
//     failed.
//
// A failure of the server's own is none of these and is left to the caller, a commit cut off at
// its bound among them (store.ErrCommitTimeout), which was once answered DATABASE_BUSY by a guess
// from its deadline: its outcome is unknown, where a busy pool's is that nothing ran.
//
// None of these is a fault of the server's to alarm an operator over, and under load every
// request meets one, so each is logged at debug and never at error.
func WriteUnserved(w http.ResponseWriter, ctx context.Context, err error) bool {
	var refusal *Refusal
	switch {
	case errors.As(context.Cause(ctx), &refusal):
		slog.Debug("dispatch: request refused the units it asked back", "error", refusal)
		WriteRefused(w, refusal)
	case DeadlinePassed(ctx):
		slog.Debug("dispatch: read ran past its deadline", "error", err)
		writeRetryable(w, http.StatusServiceUnavailable, CodeRequestTimeout, "the read ran past its deadline; retry after Retry-After")
	case errors.Is(ctx.Err(), context.Canceled):
		slog.Debug("dispatch: request's client went away", "error", err)
		accesslog.ClientGone(ctx)
	case store.IsPoolBusy(err):
		slog.Debug("dispatch: request found the database busy", "error", err)
		writeRetryable(w, http.StatusServiceUnavailable, CodeDatabaseBusy, "the database had no connection free in time; retry after Retry-After")
	default:
		return false
	}
	return true
}

// writeRetryable writes status with code and message in Dispatch's {error, code} body, telling
// the client to retry after RetryAfter and every cache not to keep it.
func writeRetryable(w http.ResponseWriter, status int, code, message string) {
	header := w.Header()
	header.Set("Retry-After", RetryAfter)
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}
