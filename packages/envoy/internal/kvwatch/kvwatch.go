// Package kvwatch keeps an in-memory cache fed by one JetStream KV bucket's watcher, and owns that
// cache's whole watch lifecycle: the first start, readiness, moving to a replacement connection,
// telling the current watcher from one it replaced, recording the terminal error when the current
// watcher ends on its own, emptying the cache when the bucket was recreated, and arming nothing
// once stopped. The interest registry, the session registry and the CI store each keep one, so
// the listener rewatches, health-checks and stops all three the same way. It also owns the
// one-shot scan of a bucket's existing keys (ScanExistingKeys), and the one reading of how
// nats.go ended a scan that both share (idleTimeout).
package kvwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// Watcher is one cache's KV watcher, from New.
type Watcher struct {
	name   string
	bucket string
	apply  func(nats.KeyValueEntry)
	reset  func()
	log    *slog.Logger

	ready     chan struct{}
	readyOnce sync.Once

	// applyMu serializes apply and reset, and lets watch wait out an entry already being applied
	// by the watcher it replaces. It is taken before mu.
	applyMu sync.Mutex

	// first is the handle Start watches.
	first bus.KeyValue

	mu sync.RWMutex
	// kv is the handle the store reads and writes through: first, until a watch moves it together
	// with the watcher, so the store writes through the connection its cache reads.
	kv         bus.KeyValue
	watcher    nats.KeyWatcher
	generation uint64
	stopped    bool
	err        error
	// stream is when the bucket's stream the current watcher reads was created. A different time
	// on the next watch means the bucket was deleted and created again.
	stream time.Time
}

// Option configures a Watcher.
type Option func(*Watcher)

// WithLogger sets the logger the watcher writes its lines through: its warm-up, a first start that
// failed, a recreated bucket and its own end. The listener passes its own, so they are JSON records
// with machine_id, as internal/store's are (store.WithLogger says why the listener never sets the
// default instead). Without it the watcher logs where slog.Default() points.
func WithLogger(log *slog.Logger) Option {
	return func(w *Watcher) { w.log = log }
}

// New returns the watcher for the bucket kv is a handle on. name labels its log lines and its
// terminal error ("<name> watcher stopped"). apply takes each entry the current watcher delivers,
// one at a time; an entry from a watcher already replaced is dropped rather than applied. reset
// empties the cache and its revision fence: a recreated bucket numbers its revisions from 1
// again, so the old fence would drop every entry the new bucket delivers. New watches nothing
// until Start. kv is a bus.KeyValue, the handle bus.EnsureKeyValue opens, and each Rewatch opens its
// replacement with bus.OpenKeyValue, so every handle the store reads and writes through checks its
// keys.
func New(name string, kv bus.KeyValue, apply func(nats.KeyValueEntry), reset func(), options ...Option) *Watcher {
	w := &Watcher{
		name:   name,
		bucket: kv.Bucket(),
		apply:  apply,
		reset:  reset,
		ready:  make(chan struct{}),
		first:  kv,
		kv:     kv,
	}
	for _, option := range options {
		option(w)
	}
	return w
}

// logger is the logger WithLogger set, or the default one.
func (w *Watcher) logger() *slog.Logger {
	if w.log == nil {
		return slog.Default()
	}
	return w.log
}

// Start arms the first watcher in the background, so a store's Open never waits on WatchAll. A
// first start that fails logs its error. When no watcher is current it records the error and
// releases readiness, so a caller waiting for the cache sees the empty cache rather than hanging.
// When a reconnect hook's Rewatch has armed one meanwhile, it does neither: the error is not that
// watcher's, and that watcher releases readiness once it has delivered every existing key.
func (w *Watcher) Start() {
	go func() {
		err := w.watch(w.first)
		if err == nil {
			return
		}
		w.logger().Error(w.name+" watch failed", slog.String("error", err.Error()))
		w.mu.Lock()
		current := w.watcher != nil
		if !current && !w.stopped {
			w.err = err
		}
		w.mu.Unlock()
		if !current {
			w.signalReady()
		}
	}()
}

// Rewatch opens the bucket on conn and replaces the current watcher with one there, clearing a
// recorded terminal error once the replacement has started. The handle (KV) moves with it, so the
// store writes through the connection its cache reads. After a server restart conn is the
// connection the store opened on, reconnected in place; when the bus replaces a closed connection,
// conn is the new one and the cache moves to it. A failure leaves the watcher and the handle as
// they were. After Stop it arms nothing and still moves the handle.
func (w *Watcher) Rewatch(conn *nats.Conn) error {
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return fmt.Errorf("open %s JetStream: %w", w.name, err)
	}
	kv, err := bus.OpenKeyValue(js, w.bucket)
	if err != nil {
		return fmt.Errorf("open %s KV bucket: %w", w.name, err)
	}
	if err := w.watch(kv); err != nil {
		return fmt.Errorf("watch %s KV bucket: %w", w.name, err)
	}
	return nil
}

// KV is the bucket handle the store reads and writes through.
func (w *Watcher) KV() bus.KeyValue {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.kv
}

// Check reads the bucket's status through KV, the round trip a store's health probe makes,
// returning its error, and records a terminal error (Err) when the bucket's stream is not the one
// the current watcher reads: the bucket was deleted and created again while the watcher ran.
// nats.go's ordered consumer can reset onto the new stream without ending the watcher, and its
// revisions number from 1 again, so the cache would stay frozen behind a live watcher. The
// recorded error makes the listener's self-health rebuild the watcher, and that Rewatch resets the
// cache.
//
// The stream is known by its creation time, and one path moves that time with no recreate.
// nats-server before v2.14.6 re-stamps a recovered stream's creation time in its file store, and
// an in-place config update persists the re-stamped time (nats-io/nats-server#8471, fixed in
// v2.14.6 and v2.15.0). So a stream config update after a server restart, followed by another
// restart, reads as a recreate. The reconnect hook's Rewatch after that restart usually meets it
// first: it resets and refills the cache from the same bucket and adopts the new time, with no
// 503. When Check meets it first, /healthz answers 503 until the next self-health rebuild does
// the same. Either way the cost is one refill, and Check is quiet from then on.
func (w *Watcher) Check() error {
	stream, err := bus.ReadStreamState(w.KV())
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped && w.err == nil && !w.stream.IsZero() && !stream.Created.Equal(w.stream) {
		w.err = fmt.Errorf("%s bucket %s was recreated under its watcher", w.name, w.bucket)
	}
	return nil
}

// Err is the current watcher's terminal error, or nil while it runs.
func (w *Watcher) Err() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.err
}

// Ready reports whether the cache is ready: the current watcher delivered every key's current
// value or ended on its own, or the first start failed with no watcher current.
func (w *Watcher) Ready() bool {
	select {
	case <-w.ready:
		return true
	default:
		return false
	}
}

// WaitReady blocks until the cache is ready or ctx is done. Callers bound ctx: WatchAll on a
// healthy cluster completes in milliseconds, but a bucket can be unreachable.
func (w *Watcher) WaitReady(ctx context.Context) error {
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop retires the watcher for a shutdown, before the NATS drain ends it. Its end then belongs to
// no live generation, so it records nothing and logs nothing, and every later Rewatch is a no-op:
// a reconnect hook or a self-health rebuild still running at shutdown arms no new watcher and
// reports no error. It makes no request of the server; the drain ends the watcher's subscription.
func (w *Watcher) Stop() {
	w.mu.Lock()
	w.stopped = true
	w.watcher = nil
	w.generation++
	w.mu.Unlock()
}

// watch arms a watcher on kv and replaces the current one, moving the handle to kv. The replaced
// watcher is stopped, and an entry it still delivers is dropped. The switch to the new watcher
// and, for a recreated bucket, the cache reset happen under applyMu, so no apply runs between
// them: an apply the replaced watcher already started finishes first, and a watch that switches
// after this one computes recreated against the new stream and applies only after the reset. Two
// Rewatches do overlap, the bus's reconnect hook and the listener's self-health rebuild. A watch
// reads its stream before it arms its watcher and takes the locks, so it can arrive after a
// newer watch has switched to a recreated bucket: a watch whose stream is older than a running,
// healthy watcher's is discarded, and that watcher stays. After Stop it arms nothing and only
// moves the handle.
func (w *Watcher) watch(kv bus.KeyValue) error {
	w.mu.Lock()
	stopped := w.stopped
	if stopped {
		w.kv = kv
	}
	w.mu.Unlock()
	if stopped {
		return nil
	}
	// The warm-up's cost as the caller waiting on readiness sees it: the stream read and the
	// watcher's create request are part of it, not just the entries that follow.
	started := time.Now()
	state, err := bus.ReadStreamState(kv)
	if err != nil {
		return err
	}
	stream := state.Created
	watcher, err := kv.WatchAll()
	if err != nil {
		return err
	}
	w.applyMu.Lock()
	w.mu.Lock()
	if w.stopped {
		// Stop ran while WatchAll was starting. Stopping this watcher would be a server request
		// outside the drain's deadline, so the drain ends its subscription; until then it is read
		// and dropped, since a drain waits for every pending message to be delivered.
		w.kv = kv
		w.mu.Unlock()
		w.applyMu.Unlock()
		discard(watcher)
		return nil
	}
	if w.err == nil && stream.Before(w.stream) {
		// A newer watch already switched to a recreated bucket and its watcher is running. This
		// watcher may be on the old stream, so installing it would reset the cache the current
		// watcher filled and feed it the old bucket's keys. The rule holds only against a live,
		// healthy watcher (an installed stream with no recorded error has one running): creation
		// times do not always grow (a JetStream restore keeps the snapshot's, and a recreate can
		// follow a clock step back), so when the current watcher has ended or Check has flagged
		// its bucket, the watch installs and resets as usual, and if it did read the old stream
		// the next Check flags that too.
		w.mu.Unlock()
		w.applyMu.Unlock()
		discard(watcher)
		_ = watcher.Stop()
		return nil
	}
	previous := w.watcher
	recreated := !w.stream.IsZero() && !stream.Equal(w.stream)
	w.generation++
	generation := w.generation
	w.watcher = watcher
	w.kv = kv
	w.stream = stream
	w.err = nil
	w.mu.Unlock()
	if recreated {
		w.reset()
	}
	w.applyMu.Unlock()
	if recreated {
		w.logger().Info(w.name+" bucket was recreated; its cache is refilled from the new bucket",
			slog.String("bucket", w.bucket))
	}
	if previous != nil {
		// Stop reports a consumer the server has lost to its caller, never at ERROR.
		_ = previous.Stop()
	}
	go w.consume(watcher, generation, started)
	return nil
}

func (w *Watcher) consume(watcher nats.KeyWatcher, generation uint64, started time.Time) {
	var scan KeyScan
	scanning := true
	for entry := range watcher.Updates() {
		if entry == nil {
			// WatchAll emits a nil sentinel once it has delivered the current value of every key,
			// and nats.go's idle timer emits the same sentinel when it gives up on that scan. Only
			// the current watcher's releases readiness, or logs: a replaced one's scan says nothing
			// about what the current watcher has delivered.
			current := w.current(generation)
			if scanning {
				scanning = false
				// A scan the idle timer ended is logged and never recorded as this watcher's
				// error: the watcher is still running and its cache still follows the bucket, so
				// Err(), Ping(), /healthz and the self-health rebuild must not read it as dead.
				// Reading the timeout consumes nats.go's one buffered error (idleTimeout), so a
				// watcher that later ends after a timed-out scan records "<name> watcher stopped"
				// instead of the timeout -- a non-nil error either way.
				timedOut := idleTimeout(watcher) != nil
				if current {
					w.logWarmUp(scan, time.Since(started), timedOut)
				}
			}
			if current {
				w.signalReady()
			}
			continue
		}
		if scanning {
			scan.count(entry)
		}
		w.applyCurrent(entry, generation)
	}
	w.mu.Lock()
	if generation != w.generation {
		// Replaced or stopped: the end belongs to no live watcher.
		w.mu.Unlock()
		return
	}
	w.watcher = nil
	err := terminalError(watcher, w.name)
	w.err = err
	w.mu.Unlock()
	// The watcher ended on its own: the connection it was on closed, or the bucket went away. The
	// cache is frozen until a Rewatch replaces it, and Err says so.
	w.logger().Error(w.name+" watcher stopped", slog.String("error", err.Error()))
	w.signalReady()
}

// logWarmUp logs the line that says what one initial scan cost: the cache, the bucket, how long it
// took, the live keys the scan delivered, the delete markers it streamed past to find them, and how
// it ended. A restart's own log then names its cost, which is what LEGION-374 could not read off a
// 17-25 s readiness gap. The two counts are disjoint, as the role restore line's are: a marker is
// not a key the cache keeps, and one total of both reads as keys the warm-up failed to apply.
// entries counts what the scan delivered, which is what sizes the replay; a cache can hold fewer,
// since each one evicts a value it cannot decode. A scan the idle timer gave up on logs the same
// line at WARN, since the cache behind it is short of the bucket.
func (w *Watcher) logWarmUp(scan KeyScan, elapsed time.Duration, timedOut bool) {
	level, outcome := slog.LevelInfo, "completed"
	if timedOut {
		level, outcome = slog.LevelWarn, "timed out"
	}
	w.logger().Log(context.Background(), level, w.name+" cache warm-up",
		slog.String("bucket", w.bucket),
		slog.Int64("elapsed_ms", elapsed.Milliseconds()),
		slog.Int("entries", scan.Puts),
		slog.Int("delete_markers", scan.Markers),
		slog.String("outcome", outcome),
	)
}

// applyCurrent applies entry only while generation is still the current watcher's.
func (w *Watcher) applyCurrent(entry nats.KeyValueEntry, generation uint64) {
	w.applyMu.Lock()
	defer w.applyMu.Unlock()
	if w.current(generation) {
		w.apply(entry)
	}
}

// current reports whether generation is still the current watcher's.
func (w *Watcher) current(generation uint64) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return generation == w.generation
}

// discard reads and drops a watcher's entries until it ends. nats.go blocks a watcher whose
// 256-entry buffer is full, and Stop only unsubscribes, so a watcher nothing reads keeps its
// delivery goroutine parked for the life of the process.
func discard(watcher nats.KeyWatcher) {
	go func() {
		for range watcher.Updates() {
		}
	}()
}

func (w *Watcher) signalReady() {
	w.readyOnce.Do(func() { close(w.ready) })
}

// terminalError is the error the watcher left on its Error channel, if any (idleTimeout), and
// otherwise "<name> watcher stopped".
func terminalError(watcher nats.KeyWatcher, name string) error {
	if err := idleTimeout(watcher); err != nil {
		return err
	}
	return errors.New(name + " watcher stopped")
}
