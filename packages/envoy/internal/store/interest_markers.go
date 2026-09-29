package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
)

// interestMarkerCollector removes the delete markers the interest bucket accumulates, one pass at
// a time.
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
// A pass reads the stream (read 1), scans the bucket, reads the stream again (read 2), and purges
// only when the scan reached the end of the bucket, it saw at least one live key, read 2 is the
// stream read 1 read, that stream's sequence space only moved forward between the two reads, and the
// floor sits inside it (FirstSeq < floor <= LastSeq). The creation time bounds the stream's
// identity and the sequences bound its space, and neither alone bounds both. A bucket deleted and
// created again is a new stream with a new creation time, and it does not always lower a sequence:
// when the original's first sequence was still 1 (a young bucket), a replacement written as far as
// the original moves neither. A JetStream restore keeps the snapshot's creation time, so only a
// sequence space that moved backward shows it. A creation time can also move with no replacement --
// nats-server 2.10 re-stamps it after an in-place config update and a restart
// (nats-io/nats-server#8471) -- which refuses that one pass; a refusal costs only the next five
// minutes, so the gate errs that way. A floor above LastSeq is the one value that makes the server's
// unfiltered purge compact the whole stream.
//
// One window cannot be guarded: STREAM.PURGE at 2.10 takes no expected-stream precondition (its
// request carries a subject, a sequence and a keep count, and nothing else), so a bucket deleted and
// created again between read 2 and the purge cannot be refused. That window is one round trip,
// which is why read 2 is taken immediately before the purge and every value the decision used is
// logged.
//
// The purge is idempotent -- a repeat purges 0 -- so every listener runs this on its own cadence
// with no lock and no leader.
type interestMarkerCollector struct {
	registry *Registry
	interval time.Duration
	log      *slog.Logger
	// purgeRefused is set once a purge the server refused has been logged, so a NATS user without
	// STREAM.PURGE on the interest bucket says so once per process rather than every pass. Passes
	// run one at a time on the collector's goroutine, so it needs no lock.
	purgeRefused bool
}

// interestPass is what one pass has read so far: read 1 and read 2 (bus.ReadStreamState, the
// STREAM.INFO the listener already sends on this bucket before every watcher and on every health
// check, so a pass adds no new capability by reading it), the floor its scan computed, and what that
// scan saw once it completed. A read or scan the pass has not taken is nil. Only the package's own
// tests reach it, through pass's hook, to stand in for what a concurrent writer or a replaced stream
// does at exactly that moment.
type interestPass struct {
	read1 *bus.StreamState
	floor uint64
	scan  *kvwatch.KeyScan
	read2 *bus.StreamState
}

// StartInterestMarkerCollector runs a collection pass now and then every interval, on the JetStream
// context js answers at each pass: a replaced NATS connection reassigns it, so a cached one would
// send the purge on a connection the scan did not read. The listener starts it after the interest
// cache's first warm-up, so the first pass -- the one that streams every marker in the bucket -- is
// off the readiness path.
func (r *Registry) StartInterestMarkerCollector(js func() nats.JetStreamContext, interval time.Duration) {
	go (&interestMarkerCollector{registry: r, interval: interval, log: r.logger()}).run(js)
}

func (c *interestMarkerCollector) run(js func() nats.JetStreamContext) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		// Every pass logs how it ended, with every stream value it read, except a purge the server
		// keeps refusing, which says so once per process. A failed pass changes nothing, and the
		// next tick reads the bucket again.
		_, _ = c.pass(js(), nil)
		<-ticker.C
	}
}

// pass runs one collection pass and returns how many messages its purge removed. afterScan is a
// hook the package's own tests set, which runs between the scan and read 2.
func (c *interestMarkerCollector) pass(js nats.JetStreamContext, afterScan func(*interestPass)) (uint64, error) {
	kv := c.registry.interests()
	var pass interestPass
	read1, err := bus.ReadStreamState(kv)
	if err != nil {
		c.refuse(slog.LevelWarn, "its first read of the bucket's stream failed", &pass, slog.String("error", err.Error()))
		return 0, fmt.Errorf("interest markers: read the bucket's stream: %w", err)
	}
	pass.read1 = &read1
	scan, err := kvwatch.ScanExistingKeys(kv, "interest markers", c.log, func(entry nats.KeyValueEntry) {
		if pass.floor == 0 || entry.Revision() < pass.floor {
			pass.floor = entry.Revision()
		}
	})
	if err != nil {
		// A scan short of the bucket is not a reading of it: the keys it never delivered are still
		// there, and the lowest revision among those it did is no floor.
		c.refuse(slog.LevelWarn, "its scan of the bucket did not complete", &pass, slog.String("error", err.Error()))
		return 0, nil
	}
	pass.scan = &scan
	if afterScan != nil {
		afterScan(&pass)
	}
	read2, err := bus.ReadStreamState(kv)
	if err != nil {
		c.refuse(slog.LevelWarn, "its second read of the bucket's stream failed", &pass, slog.String("error", err.Error()))
		return 0, fmt.Errorf("interest markers: read the bucket's stream again: %w", err)
	}
	pass.read2 = &read2
	switch {
	case pass.scan.Puts == 0:
		// A bucket with no live key is never collected, on purpose: a fleet-wide outage leaves
		// every marker where it is, which is safe, and a later reader must not "fix" it.
		c.refuse(slog.LevelDebug, "the bucket holds no live key", &pass)
		return 0, nil
	case !pass.read2.Created.Equal(pass.read1.Created):
		c.refuse(slog.LevelWarn, "the bucket's stream was replaced between the two reads", &pass)
		return 0, nil
	case pass.read2.LastSeq < pass.read1.LastSeq || pass.read2.FirstSeq < pass.read1.FirstSeq:
		c.refuse(slog.LevelWarn, "the bucket's sequence space moved backward between the two reads", &pass)
		return 0, nil
	case pass.floor > pass.read2.LastSeq:
		c.refuse(slog.LevelWarn, "the floor is above the stream's last sequence", &pass)
		return 0, nil
	case pass.floor <= pass.read2.FirstSeq:
		c.refuse(slog.LevelDebug, "the bucket holds nothing below the floor", &pass)
		return 0, nil
	}
	if err := js.PurgeStream(pass.read2.Name, &nats.StreamPurgeRequest{Sequence: pass.floor}); err != nil {
		// A purge the server refuses -- a NATS user without STREAM.PURGE on this stream -- leaves
		// every marker where it was and the listener serving. It says so once rather than every
		// five minutes, since the grant does not change on its own.
		if !c.purgeRefused {
			c.purgeRefused = true
			c.logPass(slog.LevelWarn, "interest marker collection could not purge the bucket", &pass,
				slog.String("error", err.Error()))
		}
		return 0, fmt.Errorf("interest markers: purge %s below %d: %w", pass.read2.Name, pass.floor, err)
	}
	c.purgeRefused = false
	// nats.go's PurgeStream discards the count the server answers with, so purged is the drop in
	// the stream's message count between read 2 and a read after the purge. It is approximate both
	// ways: a put landing in between under-reports it, and a peer listener's purge landing in
	// between is counted as this pass's too, so a sum of purged across the fleet overstates what
	// was removed.
	purgedStream, err := bus.ReadStreamState(kv)
	if err != nil {
		// The purge is done; only its count is unknown.
		c.logPass(slog.LevelWarn, "interest markers collected", &pass,
			slog.String("error", fmt.Sprintf("read the purged stream: %v", err)))
		return 0, fmt.Errorf("interest markers: read the purged stream: %w", err)
	}
	var purged uint64
	if pass.read2.Msgs > purgedStream.Msgs {
		purged = pass.read2.Msgs - purgedStream.Msgs
	}
	c.logPass(slog.LevelInfo, "interest markers collected", &pass,
		slog.Uint64("purged", purged),
		slog.Uint64("purged_first_seq", purgedStream.FirstSeq),
		slog.Uint64("purged_msgs", purgedStream.Msgs),
	)
	return purged, nil
}

// refuse logs one pass that purged nothing, and why. A normal refusal (no live key, nothing below
// the floor) is DEBUG. Everything else is WARN: a read of the stream that failed, a scan that did
// not complete, a replaced stream, a sequence space that moved backward, and a floor above the
// stream's last sequence.
func (c *interestMarkerCollector) refuse(level slog.Level, reason string, pass *interestPass, extra ...slog.Attr) {
	c.logPass(level, "interest marker collection refused", pass, append([]slog.Attr{slog.String("reason", reason)}, extra...)...)
}

// logPass writes one line about a pass with every stream value it had read by then, so an
// unexpected purge or refusal is diagnosable from the line alone. A read or scan the pass never
// took is absent from the line rather than zero.
func (c *interestMarkerCollector) logPass(level slog.Level, msg string, pass *interestPass, extra ...slog.Attr) {
	attrs := []slog.Attr{slog.String("bucket", c.registry.interests().Bucket())}
	if pass.read1 != nil {
		attrs = append(attrs,
			slog.Uint64("read1_first_seq", pass.read1.FirstSeq),
			slog.Uint64("read1_last_seq", pass.read1.LastSeq),
			slog.Time("read1_created", pass.read1.Created),
		)
	}
	if pass.scan != nil {
		attrs = append(attrs,
			slog.Uint64("floor", pass.floor),
			slog.Int("live", pass.scan.Puts),
			slog.Int("delete_markers", pass.scan.Markers),
		)
	}
	if pass.read2 != nil {
		attrs = append(attrs,
			slog.Uint64("read2_first_seq", pass.read2.FirstSeq),
			slog.Uint64("read2_last_seq", pass.read2.LastSeq),
			slog.Time("read2_created", pass.read2.Created),
		)
	}
	c.log.LogAttrs(context.Background(), level, msg, append(attrs, extra...)...)
}
