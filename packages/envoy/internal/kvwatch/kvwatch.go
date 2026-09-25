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
	// by the watcher it replaces.
	applyMu sync.Mutex

	// first is the handle Start watches.
	first nats.KeyValue

	mu         sync.RWMutex
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
	}
}

// Start arms the first watcher in the background, so a store's Open never waits on WatchAll. A
// first start that fails logs its error, records it only when no watcher is current (a reconnect
// hook's Rewatch may have armed one meanwhile), and releases readiness, so a caller waiting for
// the cache sees the empty cache rather than hanging.
func (w *Watcher) Start() {
	go func() {
		err := w.watch(w.first)
		if err == nil {
			return
		}
		slog.Error(w.name+" watch failed", slog.String("error", err.Error()))
		w.mu.Lock()
		if w.watcher == nil && !w.stopped {
			w.err = err
		}
		w.mu.Unlock()
		w.signalReady()
	}()
}

// Rewatch opens the bucket on conn, replaces the current watcher with one there, clearing a
// recorded terminal error once the replacement has started, and returns the handle so the store
// writes through the same connection. After a server restart conn is the connection the store
// opened on, reconnected in place; when the bus replaces a closed connection, conn is the new one
// and the cache moves to it. A failure leaves the current watcher as it was. After Stop it arms
// nothing and still returns the handle.
func (w *Watcher) Rewatch(conn *nats.Conn) (nats.KeyValue, error) {
	if conn == nil {
		return nil, errors.New(w.name + ": no connection")
	}
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		return nil, fmt.Errorf("open %s JetStream: %w", w.name, err)
	}
	kv, err := js.KeyValue(w.bucket)
	if err != nil {
		return nil, fmt.Errorf("open %s KV bucket: %w", w.name, err)
	}
	if err := w.watch(kv); err != nil {
		return nil, fmt.Errorf("watch %s KV bucket: %w", w.name, err)
	}
	return kv, nil
}

// Check reads kv's status, the round trip a store's health probe makes, returning its error, and
// records a terminal error (Err) when the bucket's stream is not the one the current watcher reads:
// the bucket was deleted and created again while the watcher ran. nats.go's ordered consumer can reset onto the new stream
// without ending the watcher, and its revisions number from 1 again, so the cache would stay frozen
// behind a live watcher. The recorded error makes the listener's self-health rebuild the watcher, and
// that Rewatch resets the cache.
func (w *Watcher) Check(kv nats.KeyValue) error {
	stream, err := streamCreated(kv)
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

// watch arms a watcher on kv and replaces the current one. The replaced watcher is stopped, and an
// entry it still delivers is dropped.
func (w *Watcher) watch(kv nats.KeyValue) error {
	if kv == nil {
		return errors.New(w.name + ": KV unavailable")
	}
	w.mu.RLock()
	stopped := w.stopped
	w.mu.RUnlock()
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
	w.mu.Lock()
	if w.stopped {
		// Stop ran while WatchAll was starting. Stopping this watcher would be a server request
		// outside the drain's deadline, so the drain ends its subscription; until then its
		// updates are read and dropped, since nats.go blocks a watcher whose 256-entry buffer is
		// full and a drain waits for every pending message to be delivered.
		w.mu.Unlock()
		go func() {
			for range watcher.Updates() {
			}
		}()
		return nil
	}
	previous := w.watcher
	recreated := !w.stream.IsZero() && !stream.Equal(w.stream)
	w.generation++
	generation := w.generation
	w.watcher = watcher
	w.stream = stream
	w.err = nil
	w.mu.Unlock()
	if previous != nil {
		// Stop reports a consumer the server has lost to its caller, never at ERROR.
		_ = previous.Stop()
	}
	if recreated {
		// The generation has moved, so no entry from the replaced watcher is applied after this.
		w.applyMu.Lock()
		w.reset()
		w.applyMu.Unlock()
		slog.Info(w.name+" bucket was recreated; its cache is refilled from the new bucket",
			slog.String("bucket", w.bucket))
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
