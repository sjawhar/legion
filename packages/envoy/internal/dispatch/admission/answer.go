package admission

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The codes of the three 503s a request is answered with when the server could not serve it. Each
// carries Retry-After and Cache-Control: no-store (WriteUnavailable).
const (
	// CodeOverloaded is a request a Limiter refused: nothing of it ran, so any method may be
	// retried.
	CodeOverloaded = "OVERLOADED"
	// CodeDatabaseBusy is a request whose handler waited its pool's acquire bound for a database
	// connection (store.ErrPoolBusy). A write may have committed part of what it does before the
	// wait that failed it.
	CodeDatabaseBusy = "DATABASE_BUSY"
	// CodeRequestTimeout is a request that ran past its deadline (WithDeadline). A write's commit
	// is never cut short by the deadline, so a write answered this may have committed.
	CodeRequestTimeout = "REQUEST_TIMEOUT"
)

// RetryAfter is the Retry-After, in seconds, of every 503 this package writes: one second, the
// order of a request's own wait for room (DefaultWait), after which the burst that refused it may
// have passed.
const RetryAfter = "1"

// ErrRequestDeadline is the cause of a request context whose deadline passed (WithDeadline). It
// tells the request's own deadline from every other deadline its handler meets - a pool's acquire
// bound, a client's timeout of its own - so only it is answered REQUEST_TIMEOUT.
var ErrRequestDeadline = errors.New("the request ran past its deadline")

// WithDeadline bounds a request's context at timeout, with ErrRequestDeadline as the cause.
func WithDeadline(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, timeout, ErrRequestDeadline)
}

// DeadlinePassed reports whether ctx is a request context whose own deadline has passed.
func DeadlinePassed(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrRequestDeadline)
}

// WriteRefused answers a request a Limiter refused (Admit's *Refusal) 503 OVERLOADED.
func WriteRefused(w http.ResponseWriter, refusal *Refusal) {
	WriteUnavailable(w, CodeOverloaded, "the server has no room for another "+refusal.Class.String()+
		" request ("+string(refusal.Reason)+"); nothing ran, retry after Retry-After")
}

// WriteUnserved answers a request whose handler failed with err because the server could not
// serve it, and reports whether it did; on false the caller maps err as it always has. Its
// order is the order the causes are told apart:
//
//   - the request's deadline passed (DeadlinePassed): 503 REQUEST_TIMEOUT, whatever err says,
//     since every failure after the deadline is the deadline's;
//   - the client went away (ctx cancelled): nothing is written, since nobody reads it;
//   - a database connection did not come free within its pool's bound (store.IsPoolBusy), or a
//     deadline other than the request's ran out while the request was still live: 503
//     DATABASE_BUSY.
//
// None of these is a fault of the server's to alarm an operator over, and under load every
// request meets one, so each is logged at debug and never at error.
func WriteUnserved(w http.ResponseWriter, ctx context.Context, err error) bool {
	switch {
	case DeadlinePassed(ctx):
		slog.Debug("dispatch: request ran past its deadline", "error", err)
		WriteUnavailable(w, CodeRequestTimeout, "the request ran past its deadline; retry after Retry-After (a write may have committed)")
	case errors.Is(ctx.Err(), context.Canceled):
		slog.Debug("dispatch: request's client went away", "error", err)
	case store.IsPoolBusy(err) || errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil:
		slog.Debug("dispatch: request found the database busy", "error", err)
		WriteUnavailable(w, CodeDatabaseBusy, "the database had no connection free in time; retry after Retry-After")
	default:
		return false
	}
	return true
}

// WriteUnavailable writes a 503 with code and message in Dispatch's {error, code} body, telling
// the client to retry after RetryAfter and every cache not to keep it.
func WriteUnavailable(w http.ResponseWriter, code, message string) {
	header := w.Header()
	header.Set("Retry-After", RetryAfter)
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}
