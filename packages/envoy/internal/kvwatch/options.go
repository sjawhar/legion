package kvwatch

import (
	"log/slog"
	"time"
)

// Option configures a Watcher.
type Option func(*Watcher)

// WithLogger sets the logger the watcher writes its lines through: each read's warm-up, a first
// start that failed, a recreated or deleted bucket, a re-read that failed and its own end. The
// listener passes its own, so they are JSON records with machine_id, as internal/store's are
// (store.WithLogger says why the listener never sets the default instead). Without it the watcher
// logs where slog.Default() points.
func WithLogger(log *slog.Logger) Option {
	return func(w *Watcher) { w.log = log }
}

// Timer is a pending wait an AfterFunc armed, which a repair that worked, or a read that made the
// wait moot, stops. *time.Timer is one.
type Timer interface{ Stop() bool }

// AfterFunc arms f to run once d has passed, as time.AfterFunc does.
type AfterFunc func(d time.Duration, f func()) Timer

// WithAfterFunc sets how the watcher arms its waits, time.AfterFunc by default: the unrepairedDelay
// wait for a repair, the wait before a bucket is read again (rereadDelay after a timed-out scan or
// a re-read that failed to start, none after a recreate a stream read found), the wait before the
// next stream probe, and a read's scan idle wait (the idle timeout, ScanIdleTimeout by default, and
// what remains of it when it is armed again: idleExpired). The production waits are a minute and
// seconds, so a test hands in its own, keeps each callback and fires it itself rather than sleeping
// for it.
func WithAfterFunc(after AfterFunc) Option {
	return func(w *Watcher) { w.after = after }
}

// WithNow sets the clock the stale episode (Servable), the age of the latest stream confirmation
// (streamConfirmMaxAge) and the time since a scan's last entry (idleExpired) are measured on,
// time.Now by default, so a test moves past the stale bound, or a scan's idle timeout, without
// waiting it out. Elapsed time is never read from record timestamps.
func WithNow(now func() time.Time) Option {
	return func(w *Watcher) { w.now = now }
}

// WithScanIdleTimeout sets how long a read's scan may deliver nothing before the read ends as timed
// out, in place of ScanIdleTimeout: for a test whose watcher runs on the real clock and the real
// AfterFunc, as a store's tests build theirs.
func WithScanIdleTimeout(idle time.Duration) Option {
	return func(w *Watcher) { w.scanIdle = idle }
}

// WithStreamProbeInterval sets how often the watcher reads its bucket's stream to find a recreate
// (probe), in place of the 2-second default. 0 arms no periodic probe, for a test that counts or
// fires the watcher's other waits (WithAfterFunc), or recreates the bucket and drives the reads
// itself. The stream is still read before a read completes and before a record is applied on an
// old confirmation, so a copy still never follows a stream other than the one it read; the gate then
// spaces its own reads by the 2-second default (confirmInterval).
func WithStreamProbeInterval(interval time.Duration) Option {
	return func(w *Watcher) { w.probeInterval = interval }
}

// WithStaleBound sets the longest stale episode served, in place of the 2-minute default, for
// tests. A stream with a MaxAge still caps it at half that MaxAge.
func WithStaleBound(bound time.Duration) Option {
	return func(w *Watcher) { w.staleBound = bound }
}
