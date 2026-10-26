package kvwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
)

// ScanIdleTimeout is how long a scan of a bucket's existing keys waits for its next entry before it
// gives up as timed out: a cache's read (Watcher), and ScanExistingKeys for the callers that pass
// it. It is the JetStream MaxWait the listener's handles carry, the idle bound nats.go's legacy
// watcher held a scan to before kvwatch moved to the jetstream watcher, which has none.
const ScanIdleTimeout = 10 * time.Second

// KeyScan counts what one pass over a bucket's existing keys delivered, as disjoint fields: Puts is
// the live keys (entries whose operation is a PUT), Markers the delete markers the pass streamed
// past to find them. They must not be reported as one total of "keys": a few claims beside many
// subjects would read as claims left unrestored (LEGION-360).
type KeyScan struct {
	Puts    int
	Markers int
}

// count adds entry to the scan and reports whether it is a live key. A delete, a purge and a
// record the server removed for its TTL (a purge, Entry) are markers.
func (s *KeyScan) count(entry Entry) bool {
	if entry.Op == OpPut {
		s.Puts++
		return true
	}
	s.Markers++
	return false
}

// ScanExistingKeys hands visitLive each live key one watch of kv's existing keys matching keys
// (jetstream.AllKeys for every key) delivers, built with opts, and returns what the watch
// delivered. The callers that need no value pass jetstream.MetaOnly(), the watch nats.go's Keys
// runs, which discards each entry's revision; this keeps it, so a caller reads a revision without a
// round trip per key. A watch of every key names no key, so a key this build cannot read
// (bus.ErrRefused: too long, or outside nats.go's key alphabet) is delivered here like any other.
// The markers are kept rather than dropped by jetstream.IgnoreDeletes unless a caller asks for
// that, because they are on the wire either way and their count is what a restart's log needs;
// visitLive never sees one. An error visitLive returns ends the scan with that error.
//
// A snapshot short of the bucket is not a reading of it, so a scan that did not reach the end of
// the bucket fails the caller instead of handing it what happened to arrive. The jetstream watcher
// (nats.go v1.50.0 jetstream/kv.go) ends a scan two ways: it sends a nil entry once it has
// delivered every existing key, and it closes its updates with no nil at all when its subscription
// ends first, an ordered consumer it could not recreate or a closed connection. It has no idle
// timer, so the scan holds its own: a scan that delivers no entry for idle gives up as timed out.
// Only the first is a complete scan; what names the caller in the errors the other two produce.
//
// The watch lives in a context of its own, cancelled on return, so the goroutine nats.go parks on
// it ends with the scan. A scan that returns with its watcher still subscribed -- complete, timed
// out, or ended by visitLive -- stops it and has it read until it closes: nats.go's delivery
// callback blocks holding the watcher's lock on a full 256-entry buffer, so a watcher stopped and
// left unread would park its subscription's goroutine for good. A stop that fails is logged
// through log at WARN.
func ScanExistingKeys(kv bus.KeyValue, keys, what string, idle time.Duration, log *slog.Logger, visitLive func(Entry) error, opts ...jetstream.WatchOpt) (KeyScan, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watcher, err := kv.WatchKeys(ctx, keys, opts...)
	if err != nil {
		return KeyScan{}, err
	}
	done := func() {
		if err := watcher.Stop(); err != nil {
			log.Warn("could not stop the "+what+" watch of the bucket", slog.String("error", err.Error()))
		}
		go drain(watcher)
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()
	var scan KeyScan
	updates := watcher.Updates()
	for {
		select {
		case entry, open := <-updates:
			if !open {
				return KeyScan{}, fmt.Errorf("%s: the bucket's watch ended after %d entries, before it had delivered them all", what, scan.Puts+scan.Markers)
			}
			if entry == nil {
				done()
				return scan, nil
			}
			timer.Reset(idle)
			live := JetStreamEntry(entry)
			if !scan.count(live) {
				continue
			}
			if err := visitLive(live); err != nil {
				done()
				return KeyScan{}, err
			}
		case <-timer.C:
			done()
			return KeyScan{}, fmt.Errorf("%s: the bucket's watch delivered nothing for %s after %d entries: %w", what, idle, scan.Puts+scan.Markers, ErrScanTimedOut)
		}
	}
}

// ErrScanTimedOut is the error a scan that delivered no entry within its idle bound carries
// (ScanExistingKeys).
var ErrScanTimedOut = errors.New("the scan timed out")

// drain reads watcher's updates until nats.go closes them, which it does once the subscription a
// stop ended has delivered what it held.
func drain(watcher jetstream.KeyWatcher) {
	for range watcher.Updates() {
	}
}
