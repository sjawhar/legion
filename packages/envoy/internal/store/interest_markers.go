package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// interestPass is one collection pass between its scan and its purge: the stream as the pass first
// read it (bus.ReadStreamState, the STREAM.INFO the listener already sends on this bucket before
// every watcher and on every health check, so a pass adds no new capability by reading it), the
// floor its scan computed, and what that scan saw. Only the package's own tests reach it, through
// collectInterestMarkers' hook, to stand in for what a concurrent writer or a replaced stream does
// at exactly that moment.
type interestPass struct {
	before bus.StreamState
	floor  uint64
	scan   bucketScan
}

// CollectInterestMarkers removes the delete markers the interest bucket accumulates, and returns
// how many messages the purge removed.
//
// Every delete path leaves one: the reaper for each dead session, an unsubscribe-all, the admin
// delete. The bucket has no MaxAge and keeps one message per subject, so a marker stays until
// something removes it, and every listener restart streams all of them before its cache is ready --
// 41,833 markers behind 40 live keys in production, 17-25 s of a restart on the on-prem machines
// with no deliveries (LEGION-374).
//
// The floor is the lowest revision the pass's own MetaOnly scan of the bucket delivered as a PUT,
// and the purge removes every message below it. That floor is safe because it comes from the stream
// rather than from the cache:
//
//   - it is a sequence that scan saw in that stream, and every live key's latest message is either
//     that PUT or a later write with a higher sequence, so nothing below the floor belongs to a live
//     key (the bucket keeps one message per subject);
//   - no value is decoded and no key is parsed on the way, so a live key this build cannot decode --
//     which applyWatched evicts from the cache while its message stays in the stream -- is protected
//     exactly like any other, and a cache short of the bucket cannot raise the floor;
//   - a put or a delete after the scan is given a higher sequence than anything the scan saw, so it
//     lands above the floor and survives.
//
// It refuses to purge unless the scan reached the end of the bucket, it saw at least one live key,
// the floor sits inside the stream's sequence space (FirstSeq < floor <= LastSeq), and that
// sequence space only moved forward between the pass's two reads of it. Every stream identity
// change that could hurt -- a bucket deleted and created again, a JetStream restore -- lowers the
// sequence space, and a floor above LastSeq is the one value that makes the server's unfiltered
// purge compact the whole stream. The stream's creation time is logged as a secondary signal and
// gates nothing: nats-server 2.10 re-stamps it after an in-place config update and a restart
// (nats-io/nats-server#8471) and a restore keeps the snapshot's, so it is wrong in both directions.
//
// One window cannot be guarded: STREAM.PURGE at 2.10 takes no expected-stream precondition (its
// request carries a subject, a sequence and a keep count, and nothing else), so a bucket deleted and
// created again between the second read and the purge cannot be refused. That window is one round
// trip, which is why the second read is taken immediately before the purge and every value the
// decision used is logged.
//
// The purge is idempotent -- a repeat purges 0 -- so every listener runs this on its own cadence
// with no lock and no leader.
func (r *Registry) CollectInterestMarkers(js nats.JetStreamContext) (uint64, error) {
	return r.collectInterestMarkers(js, nil)
}

// collectInterestMarkers is CollectInterestMarkers with a hook the package's own tests set, which
// runs between the scan and the second read of the stream.
func (r *Registry) collectInterestMarkers(js nats.JetStreamContext, afterScan func(*interestPass)) (uint64, error) {
	kv := r.interests()
	var pass interestPass
	before, err := bus.ReadStreamState(kv)
	if err != nil {
		return 0, fmt.Errorf("interest markers: read the bucket's stream: %w", err)
	}
	pass.before = before
	scan, err := scanExistingKeys(kv, "interest markers", r.logger(), func(entry nats.KeyValueEntry) {
		if entry.Operation() != nats.KeyValuePut {
			return
		}
		if pass.floor == 0 || entry.Revision() < pass.floor {
			pass.floor = entry.Revision()
		}
	})
	if err != nil {
		// A scan short of the bucket is not a reading of it: the keys it never delivered are still
		// there, and the lowest revision among those it did is no floor.
		r.refuseCollection(slog.LevelWarn, "its scan of the bucket did not complete", &pass, nil,
			slog.String("error", err.Error()))
		return 0, nil
	}
	pass.scan = scan
	if afterScan != nil {
		afterScan(&pass)
	}
	if pass.scan.puts == 0 {
		// A bucket with no live key is never collected, on purpose: a fleet-wide outage leaves
		// every marker where it is, which is safe, and a later reader must not "fix" it.
		r.refuseCollection(slog.LevelDebug, "the bucket holds no live key", &pass, nil)
		return 0, nil
	}
	after, err := bus.ReadStreamState(kv)
	if err != nil {
		return 0, fmt.Errorf("interest markers: read the bucket's stream again: %w", err)
	}
	switch {
	case after.LastSeq < pass.before.LastSeq || after.FirstSeq < pass.before.FirstSeq:
		r.refuseCollection(slog.LevelWarn, "the bucket's sequence space moved backward between the two reads", &pass, &after)
		return 0, nil
	case pass.floor > after.LastSeq:
		r.refuseCollection(slog.LevelWarn, "the floor is above the stream's last sequence", &pass, &after)
		return 0, nil
	case pass.floor <= after.FirstSeq:
		r.refuseCollection(slog.LevelDebug, "the bucket holds nothing below the floor", &pass, &after)
		return 0, nil
	}
	if err := js.PurgeStream(after.Name, &nats.StreamPurgeRequest{Sequence: pass.floor}); err != nil {
		// A purge the server refuses -- a NATS user without STREAM.PURGE on this stream -- leaves
		// every marker where it was and the listener serving. It says so once rather than every
		// five minutes, since the grant does not change on its own.
		if r.purgeRefused.CompareAndSwap(false, true) {
			r.logCollection(slog.LevelWarn, "interest marker collection could not purge the bucket", &pass, &after,
				slog.String("error", err.Error()))
		}
		return 0, fmt.Errorf("interest markers: purge %s below %d: %w", after.Name, pass.floor, err)
	}
	r.purgeRefused.Store(false)
	// nats.go's PurgeStream discards the count the server answers with, so the drop in messages is
	// what a pass can report.
	final, err := bus.ReadStreamState(kv)
	if err != nil {
		return 0, fmt.Errorf("interest markers: read the purged stream: %w", err)
	}
	var purged uint64
	if after.Msgs > final.Msgs {
		purged = after.Msgs - final.Msgs
	}
	r.logCollection(slog.LevelInfo, "interest markers collected", &pass, &after,
		slog.Uint64("purged", purged),
		slog.Uint64("first_seq", final.FirstSeq),
		slog.Uint64("msgs", final.Msgs),
	)
	return purged, nil
}

// refuseCollection logs one refused pass: why, and every value the decision was made on, so an
// unexpected refusal is diagnosable from the line alone. A normal refusal (nothing below the floor,
// no live key) is DEBUG; a scan that did not complete or a sequence space that moved backward is
// WARN.
func (r *Registry) refuseCollection(level slog.Level, reason string, pass *interestPass, after *bus.StreamState, extra ...slog.Attr) {
	r.logCollection(level, "interest marker collection refused", pass, after, append([]slog.Attr{slog.String("reason", reason)}, extra...)...)
}

func (r *Registry) logCollection(level slog.Level, msg string, pass *interestPass, after *bus.StreamState, extra ...slog.Attr) {
	attrs := []slog.Attr{
		slog.String("stream", pass.before.Name),
		slog.Uint64("floor", pass.floor),
		slog.Int("live", pass.scan.puts),
		slog.Int("delete_markers", pass.scan.markers),
		slog.Uint64("before_first_seq", pass.before.FirstSeq),
		slog.Uint64("before_last_seq", pass.before.LastSeq),
		slog.Time("before_created", pass.before.Created),
	}
	if after != nil {
		attrs = append(attrs,
			slog.Uint64("after_first_seq", after.FirstSeq),
			slog.Uint64("after_last_seq", after.LastSeq),
			slog.Time("after_created", after.Created),
		)
	}
	r.logger().LogAttrs(context.Background(), level, msg, append(attrs, extra...)...)
}

// StartInterestMarkerCollector runs CollectInterestMarkers now and then every interval, on the
// JetStream context js answers at each pass: a replaced NATS connection reassigns it, so a cached
// one would send the purge on a connection the scan did not read. The listener starts it after the
// interest cache's first warm-up, so the first pass -- the one that streams every marker in the
// bucket -- is off the readiness path.
func (r *Registry) StartInterestMarkerCollector(js func() nats.JetStreamContext, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			// The pass logs every outcome itself, with the floor and both reads; a failed pass
			// changes nothing and the next tick reads the bucket again.
			_, _ = r.CollectInterestMarkers(js())
			<-ticker.C
		}
	}()
}
