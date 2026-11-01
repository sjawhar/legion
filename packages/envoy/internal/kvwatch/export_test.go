package kvwatch

import (
	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// WithResumeOpener replaces how a resuming watch arms its watcher (openResumed,
// bus.KeyValue.WatchAllFrom outside a test), so a test drives a resume whose watcher cannot be
// created or one whose watcher it feeds itself. It exists in the test build alone: a watcher the
// listener makes always resumes through the bucket's own handle.
func WithResumeOpener(open func(kv bus.KeyValue, conn *nats.Conn, from uint64) (nats.KeyWatcher, error)) Option {
	return func(w *Watcher) { w.openResumed = open }
}

// WithBucketOpener replaces how a Rewatch takes the bucket handle it watches (openBucket,
// bus.OpenKeyValue outside a test), so a test gives the watch a bucket whose stream state it
// chooses -- a state no server this release links still produces, such as a sequence space that
// went backwards under an unchanged creation time (kvwatchtest.StreamBehind). It exists in the
// test build alone: a watcher the listener makes always opens the bucket from its connection.
func WithBucketOpener(open func(js nats.JetStreamContext, bucket string) (bus.KeyValue, error)) Option {
	return func(w *Watcher) { w.openBucket = open }
}
