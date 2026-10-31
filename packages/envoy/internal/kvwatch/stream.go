package kvwatch

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
)

// streamProbeInterval is how often a watcher reads its bucket's stream to find the bucket deleted,
// or deleted and created again, under it (probe). One STREAM.INFO per watcher every 2 s keeps the
// latest confirmation of a healthy bucket's stream younger than streamConfirmMaxAge, so the gate on
// delivered records rarely waits for a read of its own. It is also the gate's spacing: the gate
// sends no read of its own within it of the last read that ended (confirmation).
const streamProbeInterval = 2 * time.Second

// streamConfirmMaxAge is the oldest confirmation of the bucket's stream under which a record
// delivered while the copy follows its bucket is applied without waiting for a read of the stream
// (confirmation). It holds by nats.go's own constants (v1.50.0 js.go), not by timer luck. Deleting
// the stream ends the old ordered consumer's records and idle heartbeats, and nats.go moves the
// consumer onto a stream created again only from activityCheck, which runs every hbcThresh (2) ×
// the 5 s idle heartbeat (orderedHeartbeatsInterval) and resets the consumer only when nothing - no
// record, no heartbeat - arrived since the check before it, 10 s earlier. While the stream exists
// the server sends one or the other at least every 5 s, so the earliest reset comes 10 s after the
// last arrival before the delete: at least 5 s after the delete itself. A confirmation is the time
// its STREAM.INFO request was sent, and an answer naming the watched stream means the delete came
// after that send. So a record in hand at most 4 s after a confirmation's send arrived before any
// reset could have happened, at the earliest 5 s after that send, and is the watched stream's; a
// record past that waits for a read of the stream, which stops the copy following before the record
// is applied when the stream is no longer the watched one. 4 s leaves a second of margin under the
// 5 s. The jetstream watcher subscribes through the same legacy ordered push consumer (nats.go
// v1.50.0 jetstream/kv.go:1302-1330), so these constants are the ones it runs on. The wait is for
// the read in flight, or one the record sends when none is and none ended within the probe
// interval; past a read that did not answer, or answered too late to vouch for it, a record is
// applied unread (confirmation says why).
const streamConfirmMaxAge = 4 * time.Second

// Check reads the bucket's stream through KV's jetstream handle (bus.KeyValue.ReadStreamState), the
// round trip a store's health probe makes, returning its error, and records a terminal error (Err)
// when the bucket's stream is not the one the current watcher reads: the bucket was deleted and
// created again while the watcher ran. nats.go's ordered consumer can reset onto the new stream
// without ending the watcher, and its revisions number from 1 again, so the cache would stay frozen
// behind a live watcher. The copy stops following then, and the read under way, if any, completes
// nothing. The recorded error makes the listener's self-health rebuild the watcher, and that
// Rewatch reads the new bucket.
//
// The watcher normally finds a recreate first, within its probe interval, and reads the new bucket
// itself (confirmStream), so Check then finds the stream it reads unchanged. Check is the fallback
// for a recreate those reads of the stream missed: a probe that kept failing, or none armed.
//
// The stream is known by its creation time, and one path moves that time with no recreate.
// nats-server before v2.14.6 re-stamps a recovered stream's creation time in its file store, and
// an in-place config update persists the re-stamped time (nats-io/nats-server#8471, fixed in
// v2.14.6 and v2.15.0). So a stream config update after a server restart, followed by another
// restart, reads as a recreate. The reconnect hook's Rewatch after that restart, or the watcher's
// own probe, usually meets it first: either reads the same bucket again as a new epoch and adopts
// the new time, with no 503. When Check meets it first, /healthz answers 503 until the next
// self-health rebuild does the same. Either way the cost is one read, and Check is quiet from then
// on.
func (w *Watcher) Check() error {
	stream, err := w.KV().ReadStreamState()
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped && w.err == nil && !w.stream.IsZero() && !stream.Created.Equal(w.stream) {
		w.err = fmt.Errorf("%s bucket %s was recreated under its watcher", w.name, w.bucket)
		w.stopFollowing()
	}
	return nil
}

// streamCheck is what one read of the bucket's stream found for a generation (confirmStream).
type streamCheck int

const (
	// streamSame: the bucket's stream is the one the generation reads.
	streamSame streamCheck = iota
	// streamReplaced: the bucket was deleted, or deleted and created again; the copy stopped
	// following.
	streamReplaced
	// streamUnread: the read failed for another reason, which changes nothing.
	streamUnread
	// streamMoot: the generation is no longer current, or the watcher stopped.
	streamMoot
)

// confirmStream reads the bucket's stream for generation and acts on what it finds: the stream
// probe's read (probe), the gate's before a record is applied (confirmation), and the check before
// a read completes (completeRead). The request is sent outside the locks, and its answer compared
// under mu with the stream generation's watch installed, so an answer for a generation since
// replaced or stopped changes nothing. The read counts as in flight from before its send until its
// finding is recorded (confirming), so a record or a probe that waited for it acts on that finding.
//
// The same stream records when the request was sent as the latest confirmation (confirmedAt).
// Another creation time means the bucket was deleted and created again under the watcher, which
// nats.go's ordered consumer may follow without ending: the copy stops following at once, which
// starts its stale episode and takes from a read under way the right to complete, and unless a
// re-read is already due the watcher reads the new bucket itself as a re-read of generation
// (reread), whose install begins a new epoch (Read.Recreated). No stream at all means the bucket
// was deleted: the copy stops following, since it cannot follow a missing bucket, and the probe
// goes on, so the read that finds the bucket created again starts the re-read. Any other error
// changes nothing at once: a transport failure is Check's, Ping's and the self-health monitor's to
// report, and only a run of them longer than the stale bound stops the copy following (unread).
// A finding that starts a re-read logs one INFO line, as does one that stops a copy following a
// deleted bucket, so a probe that finds the same thing again logs nothing; a run of failures that
// ends the copy's following logs one WARN line.
func (w *Watcher) confirmStream(generation uint64) streamCheck {
	w.mu.Lock()
	if generation != w.generation || w.stopped {
		w.mu.Unlock()
		return streamMoot
	}
	kv := w.kv
	if w.confirming == 0 {
		w.confirmDone = make(chan struct{})
	}
	w.confirming++
	w.mu.Unlock()
	sent := w.now()
	state, err := kv.ReadStreamState()
	gone := errors.Is(err, nats.ErrStreamNotFound)
	w.mu.Lock()
	// The read ends under the lock its finding is recorded under: a record that waited for it reads
	// the watcher's state under mu before it is applied (current), so it sees that finding.
	w.confirming--
	w.attemptedAt = w.now()
	if w.confirming == 0 {
		close(w.confirmDone)
		w.confirmDone = nil
	}
	if generation != w.generation || w.stopped {
		w.mu.Unlock()
		return streamMoot
	}
	if err != nil && !gone {
		ended := w.unread(generation, sent)
		w.mu.Unlock()
		if ended {
			w.logger().Warn(w.name+" could not read its bucket's stream for longer than its stale bound; its cache stops following and is re-read once the stream answers",
				slog.String("bucket", w.bucket), slog.String("error", err.Error()))
		}
		return streamUnread
	}
	w.unreadSince = time.Time{}
	if !gone && state.Created.Equal(w.stream) {
		if sent.After(w.confirmedAt) {
			w.confirmedAt = sent
		}
		w.mu.Unlock()
		return streamSame
	}
	stopped := w.following || w.reading
	w.stopFollowing()
	rereads := !gone && w.rereadTimer == nil
	if rereads {
		w.armReread(generation, 0, recreatedCause)
	}
	w.mu.Unlock()
	switch {
	case rereads:
		w.logger().Info(w.name+" bucket was recreated under its watcher; its cache stops following and is re-read from the new bucket, kept until the read completes",
			slog.String("bucket", w.bucket))
	case gone && stopped:
		w.logger().Info(w.name+" bucket was deleted under its watcher; its cache stops following and is kept until the bucket is created again and read",
			slog.String("bucket", w.bucket))
	}
	return streamReplaced
}

// unread records, under mu, that a read of generation's stream sent at sent got no answer, and
// reports whether that ended the copy's following. A read that fails changes nothing by itself: the
// watcher may well deliver every record while STREAM.INFO alone is slow or refused. But while reads
// fail, nothing can find a recreate (Check fails too), so a copy that follows on them could follow a
// stream other than its bucket's for as long as they fail. Once an unbroken run of failures has
// outlasted the stale bound, the copy stops following, which starts its stale episode, and unless a
// re-read is already due the watcher reads the bucket again once its stream answers (reread, which
// is armed again until it does), so the copy follows again only on a read that completed.
func (w *Watcher) unread(generation uint64, sent time.Time) bool {
	if w.unreadSince.IsZero() {
		w.unreadSince = sent
	}
	if !w.following || w.now().Sub(w.unreadSince) <= w.bound {
		return false
	}
	w.stopFollowing()
	if w.rereadTimer == nil {
		w.armReread(generation, rereadDelay, unconfirmedCause)
	}
	return true
}

// armProbe arms generation's next stream probe (probe) after the probe interval, under mu, in place
// of any armed before. With no probe interval (WithStreamProbeInterval) it arms nothing.
func (w *Watcher) armProbe(generation uint64) {
	w.clearProbeArm()
	if w.probeInterval <= 0 {
		return
	}
	w.probeTimer = w.after(w.probeInterval, func() { w.probe(generation) })
}

// probe is one stream probe of generation: it reads the bucket's stream (confirmStream), which keeps
// the confirmation the gate on delivered records measures fresh and finds a bucket deleted, or
// deleted and created again, under a watcher that goes on running, and then arms the next probe
// while generation is still current. A read already in flight, the gate's or the completion
// check's, is the probe's read: it waits for that one rather than send a second beside it. A probe
// of a generation since replaced or stopped reads nothing and arms nothing: the watch that replaced
// it armed its own.
func (w *Watcher) probe(generation uint64) {
	w.mu.RLock()
	inflight := w.confirmDone
	w.mu.RUnlock()
	if inflight != nil {
		<-inflight
	} else if w.confirmStream(generation) == streamMoot {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if generation == w.generation && !w.stopped {
		w.armProbe(generation)
	}
}

// clearProbeArm drops the armed stream probe, under mu: the caller moves the generation under the
// same lock, so a callback of the wait already running reads nothing either (probe).
func (w *Watcher) clearProbeArm() {
	if w.probeTimer == nil {
		return
	}
	w.probeTimer.Stop()
	w.probeTimer = nil
}

// confirmation says what a record generation's watcher delivered must wait for before it is applied
// (applyCurrent). Only a record delivered while the copy follows its bucket, on a confirmation of
// its stream sent longer than streamConfirmMaxAge ago, waits at all: it may come from an ordered
// consumer nats.go moved onto a recreated bucket (streamConfirmMaxAge says why the bound is safe).
// Such a record waits for the read of the stream in flight when there is one (wait), whoever sent
// it: the probe, the completion check, or the gate for an earlier record. With none in flight it
// sends one of its own (send), unless a read ended within the probe interval (confirmInterval):
// that read did not answer, or answered too late to vouch for the record, and the gate never repeats
// it per record. So a STREAM.INFO that is slow or refused holds delivery back by the read under way
// at most, never by one read per record (each up to bus.RequestTimeout), and the copy keeps up with
// its bucket. A record past such a read is applied while the copy still follows: the probe, which
// goes on every interval, and Check stay the detectors of a recreate, and a run of failed reads
// longer than the stale bound stops the copy following (unread).
func (w *Watcher) confirmation(generation uint64) (wait <-chan struct{}, send bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	now := w.now()
	if generation != w.generation || !w.following || now.Sub(w.confirmedAt) <= streamConfirmMaxAge {
		return nil, false
	}
	if w.confirmDone != nil {
		return w.confirmDone, false
	}
	return nil, w.attemptedAt.IsZero() || now.Sub(w.attemptedAt) > w.confirmInterval()
}

// confirmInterval is the gate's spacing between reads of the stream (confirmation): the probe
// interval, or streamProbeInterval when WithStreamProbeInterval turned the periodic probe off.
func (w *Watcher) confirmInterval() time.Duration {
	if w.probeInterval > 0 {
		return w.probeInterval
	}
	return streamProbeInterval
}
