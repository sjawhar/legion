// Package kvwatch keeps an in-memory cache fed by one JetStream KV bucket's watcher, and owns that
// cache's whole watch lifecycle: the first start, readiness, moving to a replacement connection,
// telling the current watcher from one it replaced, recording the terminal error when the current
// watcher ends on its own, emptying the cache when the bucket was recreated, and arming nothing
// once stopped. The interest registry, the session registry and the CI store each keep one, so
// the listener rewatches, health-checks and stops all three the same way.
package kvwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// Watcher is one cache's KV watcher, from New.
type Watcher struct {
	name   string
	bucket string
	apply  func(nats.KeyValueEntry)
	reset  func()

	ready     chan struct{}
	readyOnce sync.Once

	// applyMu serializes apply and reset, and lets watch wait out an entry already being applied
	// by the watcher it replaces. It is taken before mu.
	applyMu sync.Mutex

	// first is the handle Start watches.
	first nats.KeyValue

	mu sync.RWMutex
	// kv is the handle the store reads and writes through: first, until a watch moves it together
	// with the watcher, so the store writes through the connection its cache reads.
	kv         nats.KeyValue
	watcher    nats.KeyWatcher
	generation uint64
	stopped    bool
	err        error
	// stream is when the bucket's stream the current watcher reads was created. A different time
	// on the next watch means the bucket was deleted and created again.
	stream time.Time
}

// New returns the watcher for the bucket kv is a handle on. name labels its log lines and its
// terminal error ("<name> watcher stopped"). apply takes each entry the current watcher delivers,
// one at a time; an entry from a watcher already replaced is dropped rather than applied. reset
// empties the cache and its revision fence: a recreated bucket numbers its revisions from 1
// again, so the old fence would drop every entry the new bucket delivers. New watches nothing
// until Start.
func New(name string, kv nats.KeyValue, apply func(nats.KeyValueEntry), reset func()) *Watcher {
	return &Watcher{
		name:   name,
		bucket: kv.Bucket(),
		apply:  apply,
		reset:  reset,
		ready:  make(chan struct{}),
		first:  kv,
		kv:     kv,
	}
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
		slog.Error(w.name+" watch failed", slog.String("error", err.Error()))
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
	if conn == nil {
		return errors.New(w.name + ": no connection")
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return fmt.Errorf("open %s JetStream: %w", w.name, err)
	}
	kv, err := js.KeyValue(w.bucket)
	if err != nil {
		return fmt.Errorf("open %s KV bucket: %w", w.name, err)
	}
	if err := w.watch(kv); err != nil {
		return fmt.Errorf("watch %s KV bucket: %w", w.name, err)
	}
	return nil
}

// KV is the bucket handle the store reads and writes through.
func (w *Watcher) KV() nats.KeyValue {
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
// restart, reads here as a recreate. Its cost is bounded: /healthz answers 503 until the next
// self-health rebuild, that rebuild refills the cache from the same bucket and adopts the new
// time, and Check is quiet from then on.
func (w *Watcher) Check() error {
	stream, err := streamCreated(w.KV())
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped && w.err == nil && !w.stream.IsZero() && !stream.Equal(w.stream) {
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

// Ready reports whether the cache is ready: the first watcher delivered every key's current value,
// or it ended or failed to start.
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
// newer watch has switched to a recreated bucket: a watch whose stream is older than the current
// one's is discarded, and the current watcher stays. After Stop it arms nothing and only moves
// the handle.
func (w *Watcher) watch(kv nats.KeyValue) error {
	if kv == nil {
		return errors.New(w.name + ": KV unavailable")
	}
	w.mu.Lock()
	stopped := w.stopped
	if stopped {
		w.kv = kv
	}
	w.mu.Unlock()
	if stopped {
		return nil
	}
	stream, err := streamCreated(kv)
	if err != nil {
		return err
	}
	watcher, err := kv.WatchAll()
	if err != nil {
		return err
	}
	w.applyMu.Lock()
	w.mu.Lock()
	if w.stopped {
		// Stop ran while WatchAll was starting. Stopping this watcher would be a server request
		// outside the drain's deadline, so the drain ends its subscription; until then its
		// updates are read and dropped, since nats.go blocks a watcher whose 256-entry buffer is
		// full and a drain waits for every pending message to be delivered.
		w.kv = kv
		w.mu.Unlock()
		w.applyMu.Unlock()
		go func() {
			for range watcher.Updates() {
			}
		}()
		return nil
	}
	if !w.stream.IsZero() && stream.Before(w.stream) {
		// A newer watch already switched to a recreated bucket. This watcher may be on the old
		// stream, so installing it would reset the cache the current watcher filled and feed it
		// the old bucket's keys. It is read and dropped until it ends: nats.go blocks a watcher
		// whose 256-entry buffer is full, and Stop only unsubscribes, so an unread one would keep
		// its delivery goroutine parked for the life of the process.
		w.mu.Unlock()
		w.applyMu.Unlock()
		go func() {
			for range watcher.Updates() {
			}
		}()
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
		slog.Info(w.name+" bucket was recreated; its cache is refilled from the new bucket",
			slog.String("bucket", w.bucket))
	}
	if previous != nil {
		// Stop reports a consumer the server has lost to its caller, never at ERROR.
		_ = previous.Stop()
	}
	go w.consume(watcher, generation)
	return nil
}

func (w *Watcher) consume(watcher nats.KeyWatcher, generation uint64) {
	for entry := range watcher.Updates() {
		if entry == nil {
			// WatchAll emits a nil sentinel once it has delivered the current value of every key.
			w.signalReady()
			continue
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
	slog.Error(w.name+" watcher stopped", slog.String("error", err.Error()))
	w.signalReady()
}

// applyCurrent applies entry only while generation is still the current watcher's.
func (w *Watcher) applyCurrent(entry nats.KeyValueEntry, generation uint64) {
	w.applyMu.Lock()
	defer w.applyMu.Unlock()
	w.mu.RLock()
	current := generation == w.generation
	w.mu.RUnlock()
	if current {
		w.apply(entry)
	}
}

func (w *Watcher) signalReady() {
	w.readyOnce.Do(func() { close(w.ready) })
}

// streamCreated is when the stream behind kv was created.
func streamCreated(kv nats.KeyValue) (time.Time, error) {
	status, err := kv.Status()
	if err != nil {
		return time.Time{}, err
	}
	bucket, ok := status.(*nats.KeyValueBucketStatus)
	if !ok {
		return time.Time{}, fmt.Errorf("KV status of %s is a %T, not a JetStream bucket's", kv.Bucket(), status)
	}
	return bucket.StreamInfo().Created, nil
}

// terminalError is the error the watcher reported on its Error channel, if any (for example
// nats.ErrKeyWatcherTimeout), and otherwise "<name> watcher stopped".
func terminalError(watcher nats.KeyWatcher, name string) error {
	select {
	case err, ok := <-watcher.Error():
		if ok && err != nil {
			return err
		}
	default:
	}
	return errors.New(name + " watcher stopped")
}
