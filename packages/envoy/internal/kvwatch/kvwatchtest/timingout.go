// Package kvwatchtest stands in for the one nats.go behaviour a test cannot provoke on demand: the
// idle timer that gives up on a KV watch's initial scan.
package kvwatchtest

import "github.com/nats-io/nats.go"

// TimingOut wraps kv so every watch it hands out ends its initial scan the way nats.go's idle timer
// does (nats.go v1.50.0 kv.go:1145-1156): after `after` entries of the real bucket it puts
// ErrKeyWatcherTimeout on Error() and sends the nil marker a complete scan also sends. The watcher
// stays open afterwards and forwards whatever the bucket delivers next, as nats.go's does: the timer
// gives up on the initial values, it does not end the subscription.
func TimingOut(kv nats.KeyValue, after int) nats.KeyValue {
	return timingOutKV{KeyValue: kv, after: after}
}

type timingOutKV struct {
	nats.KeyValue

	after int
}

func (k timingOutKV) Watch(keys string, opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(keys, opts...)
	if err != nil {
		return nil, err
	}
	timed := &timingOutWatcher{KeyWatcher: watcher, updates: make(chan nats.KeyValueEntry), faults: make(chan error, 1)}
	go func() {
		defer close(timed.updates)
		for range k.after {
			entry, ok := <-watcher.Updates()
			if !ok || entry == nil {
				return
			}
			timed.updates <- entry
		}
		timed.faults <- nats.ErrKeyWatcherTimeout
		timed.updates <- nil
		for entry := range watcher.Updates() {
			timed.updates <- entry
		}
	}()
	return timed, nil
}

func (k timingOutKV) WatchAll(opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	return k.Watch(nats.AllKeys, opts...)
}

type timingOutWatcher struct {
	nats.KeyWatcher

	updates chan nats.KeyValueEntry
	faults  chan error
}

func (w *timingOutWatcher) Updates() <-chan nats.KeyValueEntry { return w.updates }

func (w *timingOutWatcher) Error() <-chan error { return w.faults }
