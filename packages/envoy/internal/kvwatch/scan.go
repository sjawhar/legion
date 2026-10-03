package kvwatch

import (
	"fmt"
	"log/slog"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// KeyScan counts what one pass over a bucket's existing keys delivered, as disjoint fields: Puts is
// the live keys (entries whose operation is a PUT), Markers the delete markers the pass streamed
// past to find them. They must not be reported as one total of "keys": a few claims beside many
// subjects would read as claims left unrestored (LEGION-360).
type KeyScan struct {
	Puts    int
	Markers int
}

// count adds entry to the scan and reports whether it is a live key.
func (s *KeyScan) count(entry nats.KeyValueEntry) bool {
	if entry.Operation() == nats.KeyValuePut {
		s.Puts++
		return true
	}
	s.Markers++
	return false
}

// ScanExistingKeys hands visitLive each live key one MetaOnly watch over kv's existing keys
// delivers, and returns what the watch delivered. That is the watch nats.go's kv.Keys() runs --
// MetaOnly over every key -- which discards each entry's revision; this keeps it, so a caller reads
// a revision without a round trip per key. The watch names no key, so a key this build cannot read
// (bus.ErrRefused: too long, or outside nats.go's key alphabet) is delivered here like any other.
// The markers are kept rather than dropped by nats.go's IgnoreDeletes, because they are on the wire
// either way and their count is what a restart's log needs; visitLive never sees one.
//
// A snapshot short of the bucket is not a reading of it, so a scan that did not reach the end of
// the bucket fails the caller instead of handing it what happened to arrive. nats.go ends a scan
// three ways (kv.go in nats.go v1.50.0): it sends a nil entry once it has delivered every existing
// key (:1096-1099, :1140-1142); its idle timer sends the same nil after putting
// ErrKeyWatcherTimeout on Error() (idleTimeout); and it closes the updates channel with no nil at
// all when the subscription ends first (:1170, which closes Error() at :1171 too), an ordered
// consumer it could not recreate or a closed connection. Only the first is a complete scan. what
// names the caller in the errors the other two produce, and log takes the one line a failed stop
// of the watch writes.
func ScanExistingKeys(kv bus.KeyValue, what string, log *slog.Logger, visitLive func(nats.KeyValueEntry)) (KeyScan, error) {
	watcher, err := kv.Watch(nats.AllKeys, nats.MetaOnly())
	if err != nil {
		return KeyScan{}, err
	}
	defer func() {
		if err := watcher.Stop(); err != nil {
			log.Warn("could not stop the "+what+" watch of the bucket", slog.String("error", err.Error()))
		}
	}()
	var scan KeyScan
	for entry := range watcher.Updates() {
		if entry == nil {
			if err := idleTimeout(watcher); err != nil {
				return KeyScan{}, fmt.Errorf("%s: the bucket's watch stopped after %d entries: %w", what, scan.Puts+scan.Markers, err)
			}
			return scan, nil
		}
		if scan.count(entry) {
			visitLive(entry)
		}
	}
	return KeyScan{}, fmt.Errorf("%s: the bucket's watch ended after %d entries, before it had delivered them all", what, scan.Puts+scan.Markers)
}

// idleTimeout is the ErrKeyWatcherTimeout nats.go's idle timer left on watcher's Error() channel
// when it gave up on the initial scan, or nil when there is none. The timer puts it there under the
// watcher's lock before it sends the nil marker a complete scan also ends with (nats.go v1.50.0
// kv.go:1145-1156), so the error is there to read by the time that marker arrives. The timer is the
// channel's only writer, the channel buffers that one error, and nats.go closes it when the
// subscription ends (:1171), which reads as nil here. So this read consumes the error: whoever
// reads it first is the only one who sees it.
//
// Each caller keeps its own policy on what it reads. A one-shot scan refuses a scan the timer ended
// (ScanExistingKeys); a cache's warm-up logs it and keeps its watcher, which is still live (consume);
// and a watcher that ended on its own reports it as its terminal error (terminalError).
func idleTimeout(watcher nats.KeyWatcher) error {
	select {
	case err := <-watcher.Error():
		return err
	default:
		return nil
	}
}
