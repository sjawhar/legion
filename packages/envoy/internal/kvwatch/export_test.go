package kvwatch

import "github.com/sjawhar/envoy/internal/bus"

// WatchOn installs a new read on kv as Rewatch does once it has opened its handle. Rewatch opens
// that handle itself (bus.OpenKeyValue), so a test that holds, times out or ends a re-read's scan
// installs that read through here, on a handle that does so.
func WatchOn(w *Watcher, kv bus.KeyValue) error {
	return w.watch(kv, 0)
}

// Generation is the watcher's current generation: the one a re-read armed now would read again.
func Generation(w *Watcher) uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.generation
}

// RereadOn installs a re-read of generation on kv as reread does once its wait has passed: it is
// discarded unless generation is still current.
func RereadOn(w *Watcher, kv bus.KeyValue, generation uint64) error {
	return w.watch(kv, generation)
}
