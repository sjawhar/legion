// Package kvwatchtest stands in for the one nats.go behaviour a test cannot provoke on demand: the
// idle timer that gives up on a KV watch's initial scan.
package kvwatchtest

import (
	"sync"
	"sync/atomic"

	"github.com/nats-io/nats.go"
)

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

// TimingOutFirst wraps kv so its first watch stalls the way that makes nats.go's idle timer fire:
// after `after` entries it puts ErrKeyWatcherTimeout on Error() and sends the nil marker, as
// TimingOut's watches do, and then forwards nothing more until that watcher is stopped, so the
// rest of the scan never reaches the cache through it. Every later watch is the real bucket's, so a
// test sees a cache recover from a timed-out first scan only by watching again.
func TimingOutFirst(kv nats.KeyValue, after int) nats.KeyValue {
	return &timingOutFirstKV{KeyValue: kv, after: after}
}

type timingOutFirstKV struct {
	nats.KeyValue

	after   int
	watched atomic.Bool
}

func (k *timingOutFirstKV) Watch(keys string, opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	watcher, err := k.KeyValue.Watch(keys, opts...)
	if err != nil || !k.watched.CompareAndSwap(false, true) {
		return watcher, err
	}
	stalled := &stalledWatcher{
		timingOutWatcher: timingOutWatcher{KeyWatcher: watcher, updates: make(chan nats.KeyValueEntry), faults: make(chan error, 1)},
		stopped:          make(chan struct{}),
	}
	go func() {
		defer close(stalled.updates)
		for range k.after {
			entry, ok := <-watcher.Updates()
			if !ok || entry == nil {
				return
			}
			stalled.updates <- entry
		}
		stalled.faults <- nats.ErrKeyWatcherTimeout
		stalled.updates <- nil
		<-stalled.stopped
	}()
	return stalled, nil
}

func (k *timingOutFirstKV) WatchAll(opts ...nats.WatchOpt) (nats.KeyWatcher, error) {
	return k.Watch(nats.AllKeys, opts...)
}

// stalledWatcher is TimingOutFirst's first watcher: its updates end only once it is stopped.
type stalledWatcher struct {
	timingOutWatcher

	stopOnce sync.Once
	stopped  chan struct{}
}

func (w *stalledWatcher) Stop() error {
	w.stopOnce.Do(func() { close(w.stopped) })
	return w.KeyWatcher.Stop()
}
