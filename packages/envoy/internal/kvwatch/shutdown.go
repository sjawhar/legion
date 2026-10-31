package kvwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
)

// Stop retires the watcher for a shutdown, before the NATS drain. Its end then belongs to no live
// generation, so it records nothing and logs nothing, and every later Rewatch is a no-op: a
// reconnect hook or a self-health rebuild still running at shutdown arms no new watcher and
// reports no error. It disarms the current generation's waits, its scan's idle wait among them. It
// makes no request of the server, so it returns at once against one that does not answer: the
// watcher it retires, and any a watch in flight arms after it, are kept for End, and their consume
// goes on reading them, dropping what they deliver, until they end.
func (w *Watcher) Stop() {
	w.mu.Lock()
	w.stopped = true
	if w.watcher != nil {
		w.retired = append(w.retired, w.watcher)
	}
	w.watcher = nil
	w.clearRepairArm()
	w.clearRereadArm()
	w.clearProbeArm()
	w.clearIdleArm()
	w.generation++
	w.mu.Unlock()
}

// End stops the watcher (Stop) and ends every watcher it retired while the connection still
// carries requests: the listener runs it from the bus's drain hook (bus.Client.AddDrainHook),
// before the connection's own drain. It waits, within ctx, for a watch still in flight to arm its
// watcher, then stops each one, which unsubscribes it, deletes the ordered consumer nats.go created
// for it and cancels the context its subscription lives in, and waits for those deletes within ctx.
// Left to the connection's drain, that delete races the drain and is refused as "connection
// draining" (bus.Client.AddDrainHook says how). A consumer already gone (a NATS restart loses an
// ordered consumer outright, and a disconnect past its inactive threshold lets the server delete
// it) is what the delete was for, so it is logged at WARN, and so is a delete the connection was
// lost before (lostConnection): the server deletes a consumer whose interest is gone on its own.
// Any other failure, and a delete still unanswered when ctx's deadline passes, is an ERROR. A watch
// that arms its watcher after End has taken them stops that watcher itself (watch). End does its
// work once: a later End, or one beside it, waits for the first to finish, within the first's ctx,
// and ends nothing more.
//
// End stops each watcher at once, whether or not its scan has ended: the consumers must be gone
// before the drain. That is safe with the jetstream watcher, which sends nothing once its
// subscription has ended, and its consume reads it until it closes.
func (w *Watcher) End(ctx context.Context) {
	w.endOnce.Do(func() { w.end(ctx) })
}

// end is End's work, run once.
func (w *Watcher) end(ctx context.Context) {
	w.Stop()
	w.mu.Lock()
	var idle chan struct{}
	if w.watching > 0 {
		idle = make(chan struct{})
		w.idle = idle
	}
	w.mu.Unlock()
	var gaveUp error
	if idle != nil {
		// A watch that has finished is read before ctx, which can already be done: a hook's
		// context ends the moment the connection is lost.
		select {
		case <-idle:
		default:
			select {
			case <-idle:
			case <-ctx.Done():
				gaveUp = context.Cause(ctx)
				w.logUnfinished(w.name+" watcher end found a watch still starting", gaveUp)
			}
		}
	}
	w.mu.Lock()
	retired := w.retired
	w.retired = nil
	w.ended = true
	w.endCause = gaveUp
	w.mu.Unlock()
	results := make(chan error, len(retired))
	for _, watcher := range retired {
		go func() { results <- watcher.stop() }()
	}
	for range retired {
		select {
		case err := <-results:
			w.logEnd(err)
			continue
		default:
		}
		select {
		case err := <-results:
			w.logEnd(err)
		case <-ctx.Done():
			w.logEnd(fmt.Errorf("delete its consumer: %w", context.Cause(ctx)))
		}
	}
}

// lostConnection reports whether err says the connection was lost before a request reached the
// server or its answer came back: the drain hook's own cause (bus.ErrConnectionLost), and what
// nats.go answers a request with when its connection is closed, reconnecting, or, with the
// reconnect buffer off, refusing a publish until it reconnects.
func lostConnection(err error) bool {
	return errors.Is(err, bus.ErrConnectionLost) || errors.Is(err, nats.ErrConnectionClosed) ||
		errors.Is(err, nats.ErrConnectionReconnecting) || errors.Is(err, nats.ErrReconnectBufExceeded)
}

// lateStop is how stopping a watcher that a watch armed after End had taken the retired ones
// ended. When End stopped waiting for that watch because the connection was lost, the stop runs
// on a connection the drain has gone on without (nats.go refuses it as "connection draining"
// once the connection's drain begins, nats.go's Unsubscribe), so a failure other than the consumer
// being gone is that loss, and the consumer is left to the server as End's own are.
func lateStop(err, endCause error) error {
	if err == nil || !lostConnection(endCause) || errors.Is(err, nats.ErrConsumerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", endCause, err)
}

// logEnd logs how stopping one retired watcher ended (End).
func (w *Watcher) logEnd(err error) {
	switch {
	case err == nil, errors.Is(err, nats.ErrBadSubscription):
		// Stopped, or its subscription had already ended.
	case errors.Is(err, nats.ErrConsumerNotFound):
		w.logger().Warn(w.name+" watcher's consumer was already gone at its end",
			slog.String("bucket", w.bucket), slog.String("error", err.Error()))
	case lostConnection(err):
		w.logger().Warn(w.name+" watcher's consumer was left to the server: the connection was lost before its delete",
			slog.String("bucket", w.bucket), slog.String("error", err.Error()))
	default:
		w.logger().Error(w.name+" watcher end failed",
			slog.String("bucket", w.bucket), slog.String("error", err.Error()))
	}
}

// logUnfinished logs what End could not wait for: at WARN when the connection was lost, since the
// server then ends what it would have, and at ERROR when End's deadline passed.
func (w *Watcher) logUnfinished(message string, cause error) {
	level := slog.LevelError
	if lostConnection(cause) {
		level = slog.LevelWarn
	}
	w.logger().Log(context.Background(), level, message, slog.String("bucket", w.bucket), slog.String("error", cause.Error()))
}

// watchDone counts a watch out (watching), and wakes End once none is left.
func (w *Watcher) watchDone() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watching--
	if w.watching == 0 && w.idle != nil {
		close(w.idle)
		w.idle = nil
	}
}

// subscription is one jetstream watcher and the cancel of the context its subscription lives in
// (watch). Whoever stops it, or reads it to its end, cancels that context, which releases the
// goroutine nats.go parks on it.
type subscription struct {
	watcher jetstream.KeyWatcher
	cancel  context.CancelFunc
}

// stop unsubscribes the watcher, which deletes the ordered consumer nats.go created for it, then
// cancels its context, and returns the stop's error. It never waits for the watcher's updates to
// close: they close once the subscription's goroutine has delivered what it holds, which needs a
// reader (consume, discard), since nats.go's delivery blocks holding the watcher's lock on a full
// 256-entry buffer. A stop is safe at any moment, mid-scan included: the jetstream watcher has no
// timer that could send after the close.
func (s *subscription) stop() error {
	err := s.watcher.Stop()
	s.cancel()
	return err
}

// discard reads and drops the watcher's entries until nats.go closes them, then cancels its
// context. A watcher nothing reads keeps its subscription's goroutine parked on a full buffer for
// the life of the process.
func (s *subscription) discard() {
	bus.DrainWatcher(s.watcher)
	s.cancel()
}
