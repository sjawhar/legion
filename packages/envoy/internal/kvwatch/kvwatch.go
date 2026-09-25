// Package kvwatch keeps an in-memory cache fed by one JetStream KV bucket's watcher: it replaces
// the watcher when asked, tells the current watcher from one it replaced, records the terminal
// error when the current watcher ends on its own, and arms nothing once stopped. The interest
// registry, the session registry and the CI store each keep one, so the listener rewatches,
// health-checks and stops all three the same way.
package kvwatch

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/nats-io/nats.go"
)

// Watcher is one cache's KV watcher. The zero value is ready to use once Name, Apply and Ready are
// set; a store embeds it by value and must not copy it after the first Watch.
type Watcher struct {
	// Name labels the watcher in its log lines and its terminal error, as "<Name> watcher stopped".
	Name string
	// Apply takes every entry the watcher delivers, one at a time, from the watcher's goroutine.
	Apply func(nats.KeyValueEntry)
	// Ready runs at each end of an initial scan and when a watcher ends, so a caller waiting for the
	// cache is released either way. It must be idempotent.
	Ready func()

	mu         sync.RWMutex
	watcher    nats.KeyWatcher
	generation uint64
	stopped    bool
	err        error
}

// Watch arms a watcher on kv and replaces the current one, clearing a recorded terminal error once
// the replacement has started. It is a no-op after Stop. The replaced watcher is stopped; its last
// updates can still reach Apply after the new watcher's, so Apply must keep a revision fence.
func (w *Watcher) Watch(kv nats.KeyValue) error {
	if kv == nil {
		return errors.New(w.Name + ": KV unavailable")
	}
	w.mu.RLock()
	stopped := w.stopped
	w.mu.RUnlock()
	if stopped {
		return nil
	}
	watcher, err := kv.WatchAll()
	if err != nil {
		return err
	}
	w.mu.Lock()
	if w.stopped {
		// Stop ran while WatchAll was starting. Leave this watcher to the connection's drain, which
		// ends its subscription; stopping it here would be a server request outside the drain's
		// deadline.
		w.mu.Unlock()
		return nil
	}
	previous := w.watcher
	w.generation++
	generation := w.generation
	w.watcher = watcher
	w.err = nil
	w.mu.Unlock()
	if previous != nil {
		// Stop reports a consumer the server has lost to its caller, never at ERROR.
		_ = previous.Stop()
	}
	go w.consume(watcher, generation)
	return nil
}

// Fail records err as the current watcher's terminal error. A store calls it when its first Watch
// fails, so the failure shows in Err rather than as an empty cache.
func (w *Watcher) Fail(err error) {
	w.mu.Lock()
	w.err = err
	w.mu.Unlock()
}

// Err is the current watcher's terminal error, or nil while it runs.
func (w *Watcher) Err() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.err
}

// Stop retires the watcher for a shutdown, before the NATS drain ends it. Its end then belongs to
// no live generation, so it records nothing and logs nothing, and every later Watch is a no-op: a
// reconnect hook or a self-health rebuild still running at shutdown arms no new watcher and
// reports no error. It makes no request of the server; the drain ends the watcher's subscription.
func (w *Watcher) Stop() {
	w.mu.Lock()
	w.stopped = true
	w.watcher = nil
	w.generation++
	w.mu.Unlock()
}

func (w *Watcher) consume(watcher nats.KeyWatcher, generation uint64) {
	for entry := range watcher.Updates() {
		if entry == nil {
			// WatchAll emits a nil sentinel once it has delivered the current value of every key.
			w.Ready()
			continue
		}
		w.Apply(entry)
	}
	w.mu.Lock()
	if generation != w.generation {
		// Replaced or stopped: the end belongs to no live watcher.
		w.mu.Unlock()
		return
	}
	w.watcher = nil
	err := terminalError(watcher, w.Name)
	w.err = err
	w.mu.Unlock()
	// The watcher ended on its own: the connection it was on closed, or the bucket went away. The
	// cache is frozen until a Watch replaces it, and Err says so.
	slog.Error(w.Name+" watcher stopped", slog.String("error", err.Error()))
	w.Ready()
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
