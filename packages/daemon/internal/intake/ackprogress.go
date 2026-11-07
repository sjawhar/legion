package intake

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// What the progress signal makes silent, for whoever reads the daemon's log.
//
// It removes a false positive: a slow fact's repeated preparation work. A fact that outlived its
// AckWait was delivered again and prepared again — a Dispatch read, a GitHub read, another wait on
// the fact lock — before ApplyFact found it already processed, and that repeated work read in the
// log as trouble when the fact was only slow.
//
// It also quiets a true failure. A wedged handler no longer shows as that repeated preparation,
// and no longer raises the delivered count in the consumer-lag warning (lagWatch.observe in
// consume.go), because the message it is stuck on is not delivered again while it holds.
//
// What still shows a wedged handler is the stall: ConsumerLag.Stalled, true once messages wait and
// none has been handled for consumerStallAfter, and the state route's consumers[].stalled that
// serves it (packages/daemon/internal/api/state.go).

// keepInProgress reports message in progress to JetStream every `every` for as long as a handler
// works on it, so JetStream does not deliver it again and have it prepared a second time. It
// returns stop, which ends the keeper. stop is safe to call many times and returns only once the
// keeper's goroutine has ended, so a caller that calls it before it acknowledges or naks the
// message knows no tick can still be in flight: a tick that reached the server after a nak would
// reset the server's redelivery timer and stretch the retry the nak asked for. nats.go would stop
// that tick today anyway, since a nak marks the message acknowledged and every later signal for
// it is refused with ErrMsgAlreadyAckd and published nowhere, but that guard is the client
// library's and this order does not rest on it.
//
// The one caller ticks at a third of AckWait. A fraction well below half leaves room for one tick
// to be lost or to arrive late and still have another inside the window before it expires.
//
// jetstream.ErrMsgAlreadyAckd ends the keeper with no line at all. nats.go never marks a message
// acknowledged for a progress signal (nats.go v1.53.1's (*jetStreamMsg).ackReply, which sets ackd
// for every acknowledgement type but ackProgress), so that error can only mean the handler
// already ended the message, which is the moment the keeper has nothing left to do.
//
// Every other error is tolerated and logged once per message, at debug. The signal is a
// fire-and-forget publish (the same ackReply, which publishes rather than requests for a caller
// that does not ask for the server's reply, and InProgress is such a caller), so the only
// consequence of losing one is a redelivery the system already handles, and a line per tick would
// flood the log for the length of a NATS outage.
//
// An interval that is not positive starts nothing and returns a stop that does nothing, because
// time.NewTicker panics on such a duration. The guard is this function's own contract rather than
// a live caller's need: every caller today reaches it through OpenConsumers, whose normalizedSpec
// makes AckWait positive.
func keepInProgress(ctx context.Context, message jetstream.Msg, every time.Duration, logger *slog.Logger) (stop func()) {
	if every <= 0 {
		return func() {}
	}
	stopped, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		logged := false
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopped:
				return
			case <-ticker.C:
				err := message.InProgress()
				switch {
				case err == nil:
				case errors.Is(err, jetstream.ErrMsgAlreadyAckd):
					return
				case !logged:
					logged = true
					logMessage(logger, slog.LevelDebug, "report an intake message in progress", message, "error", err)
				}
			}
		}
	}()
	stopTicking := sync.OnceFunc(func() { close(stopped) })
	return func() {
		stopTicking()
		<-ended
	}
}
