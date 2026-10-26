package kvwatch

import "github.com/sjawhar/envoy/internal/bus"

// WatchOn installs a new read on kv as Rewatch does once it has opened its handle. Rewatch opens
// that handle itself (bus.OpenKeyValue), so a test that holds, times out or ends a re-read's scan
// installs that read through here, on a handle that does so.
func WatchOn(w *Watcher, kv bus.KeyValue) error {
	return w.watch(kv, 0)
}
