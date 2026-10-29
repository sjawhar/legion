package bus

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// resumeOpenWait bounds the js.KeyValue lookup WatchAllFrom makes before it creates its watcher,
// the one call of the resume that takes a deadline of its own (the watch's context carries none,
// WatchAllFrom says why). It is the wait kvwatch's Rewatch gives every JetStream request it makes
// (nats.MaxWait(10 * time.Second)).
const resumeOpenWait = 10 * time.Second

// WatchAllFrom watches every key of kv's bucket on conn from revision on, rather than from each
// key's current value as WatchAll does. A KV revision is a stream sequence, so the watcher delivers
// every message the bucket's stream still holds at revision or after, delete and purge markers
// included, then the nil marker once it has delivered what was pending when it started (at once
// when nothing was), then each later write. Each bucket the listener keeps holds one revision per
// key (nats.go's default History of 1), so that is at most one message per key that still has one.
// A message the stream no longer holds is not delivered: a delete marker purged meanwhile, or a
// value MaxAge expired, leaves nothing to see. revision must be at least 1; revision 0 would be
// nats.go's "no resume", a full replay the caller did not ask for.
//
// The legacy nats.KeyValue API has no resume option (its watchOpts is unexported), so this opens the
// bucket through nats.go's jetstream package on conn and hands back its watcher behind the legacy
// nats.KeyWatcher (resumedWatcher), which every cache watcher reads. The search key is every key, so
// the subject check WatchAll would need is the one Watch makes, held to the same bound.
func (kv KeyValue) WatchAllFrom(conn *nats.Conn, revision uint64) (nats.KeyWatcher, error) {
	bucket := kv.Bucket()
	if revision == 0 {
		return nil, fmt.Errorf("resume a watch of KV bucket %s: revision 0 is no revision", bucket)
	}
	if err := kv.check(nats.AllKeys, watchOverhead(bucket)); err != nil {
		return nil, err
	}
	js, err := jetstream.New(conn)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), resumeOpenWait)
	defer cancel()
	handle, err := js.KeyValue(ctx, bucket)
	if err != nil {
		return nil, err
	}
	// The watch's context carries no deadline. nats.go keeps it for the subscription's whole life
	// and unsubscribes the subscription once it is done (js.go:2053-2057), so a deadline would end
	// the watch at it rather than bound its create. The create itself waits the legacy JetStream
	// context's default 5 s (jsm.go:1791-1796, js.go:299), under the 10 s the full WatchAll's own
	// JetStream context gives it; a create that does not answer within that fails the resume, and
	// kvwatch's watch then reads the bucket in full instead.
	//
	// It is cancellable so that nothing of it outlives the watch: nats.go parks a goroutine on
	// this context for the subscription (js.go:2053), and cancels it only in Unsubscribe
	// (nats.go:5210-5220), which a closed connection refuses (nats.go:5063-5065). The watcher
	// cancels the context itself once the inner updates close (forward).
	watchCtx, cancelWatch := context.WithCancel(context.Background())
	watcher, err := handle.WatchAll(watchCtx, jetstream.ResumeFromRevision(revision))
	if err != nil {
		cancelWatch()
		return nil, err
	}
	return newResumedWatcher(watchCtx, cancelWatch, conn, bucket, watcher), nil
}

// resumedWatcher is a jetstream.KeyWatcher behind the legacy nats.KeyWatcher interface, so a cache
// watcher reads a resumed watch exactly as it reads WatchAll's. Updates hands on each entry with its
// operation mapped onto the legacy one (resumedEntry), and the nil marker as it is. The jetstream
// watcher has no Error channel: nats.go closes its updates when the subscription ends (its closed
// handler, which runs when the connection closes, when the ordered consumer cannot be recreated, and
// after Stop), and nothing else. So Error delivers one error when that happens without Stop having
// been called, before Updates closes, and is closed with it; a reader that finds Updates closed
// reads the cause there, as it reads the legacy watcher's (kvwatch's terminalError). A watcher
// stopped on purpose ends with Error closed and empty.
type resumedWatcher struct {
	ctx     context.Context
	cancel  context.CancelFunc
	conn    *nats.Conn
	bucket  string
	inner   jetstream.KeyWatcher
	updates chan nats.KeyValueEntry
	errs    chan error
	stopped atomic.Bool
}

// newResumedWatcher wraps inner, the watcher of bucket on conn that ctx created, and starts handing
// on its entries. cancel is ctx's, which forward calls once the watch has ended.
func newResumedWatcher(ctx context.Context, cancel context.CancelFunc, conn *nats.Conn, bucket string, inner jetstream.KeyWatcher) *resumedWatcher {
	w := &resumedWatcher{
		ctx:     ctx,
		cancel:  cancel,
		conn:    conn,
		bucket:  bucket,
		inner:   inner,
		updates: make(chan nats.KeyValueEntry),
		errs:    make(chan error, 1),
	}
	go w.forward()
	return w
}

// forward hands on inner's entries until nats.go closes its updates, then cancels the watch's
// context, leaves the end's cause on Error unless Stop ended it, and closes Error and then
// Updates, so the cause is there to read by the time Updates reads as closed. Updates is
// unbuffered: inner's own 256-entry buffer is the one bound, and nats.go blocks its delivery on it
// as it does the legacy watcher's.
//
// The cancel is what ends the goroutine nats.go parks on that context for the subscription
// (js.go:2053): nats.go cancels it only in Unsubscribe, which a closed connection refuses, so
// without this one goroutine would stay behind for every resumed watch whose connection closed.
// The subscription is over by then, so the Unsubscribe the cancel wakes has nothing left to end,
// and a cancel after Stop (which unsubscribed already) is harmless too.
func (w *resumedWatcher) forward() {
	defer close(w.updates)
	for entry := range w.inner.Updates() {
		if entry == nil {
			w.updates <- nil
			continue
		}
		w.updates <- resumedEntry{entry}
	}
	w.cancel()
	if !w.stopped.Load() {
		w.errs <- w.endCause()
	}
	close(w.errs)
}

// endCause names why the subscription ended with no Stop: its connection closed, which nats.go
// marks before it ends the connection's subscriptions, or anything else that ended it.
func (w *resumedWatcher) endCause() error {
	if w.conn.IsClosed() {
		return fmt.Errorf("watch of KV bucket %s ended: %w", w.bucket, nats.ErrConnectionClosed)
	}
	return fmt.Errorf("watch of KV bucket %s ended: %w", w.bucket, errResumedWatchEnded)
}

// errResumedWatchEnded is the cause of a resumed watch that ended with its connection still open:
// nats.go could not recreate its ordered consumer, or its subscription was removed some other way.
var errResumedWatchEnded = errors.New("its subscription closed")

// Context is the context the watch was created with, which forward cancels once the watch ends.
func (w *resumedWatcher) Context() context.Context { return w.ctx }

// Updates delivers each entry and the nil marker, and closes when the watch ends.
func (w *resumedWatcher) Updates() <-chan nats.KeyValueEntry { return w.updates }

// Error delivers the cause of an end nobody asked for, and is closed when the watch ends.
func (w *resumedWatcher) Error() <-chan error { return w.errs }

// Stop unsubscribes the watch, which nats.go answers by closing its updates; the end is then the
// caller's, so Error delivers nothing.
func (w *resumedWatcher) Stop() error {
	w.stopped.Store(true)
	return w.inner.Stop()
}

// resumedEntry is a jetstream.KeyValueEntry read as a legacy nats.KeyValueEntry. Every method but
// Operation has the same signature in both packages; the two operation types number the same three
// operations, but are distinct types, so Operation maps one onto the other by name.
type resumedEntry struct {
	jetstream.KeyValueEntry
}

// Operation is the entry's operation as the legacy API names it.
func (e resumedEntry) Operation() nats.KeyValueOp {
	switch e.KeyValueEntry.Operation() {
	case jetstream.KeyValueDelete:
		return nats.KeyValueDelete
	case jetstream.KeyValuePurge:
		return nats.KeyValuePurge
	default:
		return nats.KeyValuePut
	}
}
