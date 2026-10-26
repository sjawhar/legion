// Package kvwatch keeps an in-memory cache fed by one JetStream KV bucket's watcher, and owns that
// cache's whole watch lifecycle: the first start, readiness, moving to a replacement connection,
// telling the current watcher from one it replaced, recording the terminal error when the current
// watcher ends on its own, and arming nothing once stopped. The interest registry, the session
// registry and the CI store each keep one, so the listener rewatches, health-checks and stops all
// three the same way. It also owns the one-shot scan of a bucket's existing keys
// (ScanExistingKeys).
//
// Every watcher is the jetstream package's KV watcher (bus.KeyValue.WatchKeys), never nats.go's
// legacy one: the legacy watcher arms an init timer while its scan is pending, and that timer's
// callback can send on the channels the subscription's end has just closed, a "send on closed
// channel" panic in a goroutine nats.go owns that ends the process whenever the subscription ends
// mid-scan -- a Stop, nats.go's own unsubscribe, a connection closed or drained (nats.go v1.50.0
// kv.go:1145-1172, unfixed through v1.55.0). The jetstream watcher has no timer and closes its
// updates only on its subscription goroutine's exit, so no send can follow the close, and a watcher
// may be stopped at any moment provided it is read until it closes. It has no idle bound either, so
// the watcher owns its scan's: a read whose scan delivers nothing for ScanIdleTimeout ends as timed
// out (consume).
//
// Every watcher a watch installs is a read of the whole bucket (Cache). The cache keeps every entry
// through the read, takes each delivered record as an update, and only once the read has delivered
// every record removes the entries it never touched: a read is never preceded by emptying the
// cache, so a list served from it during the read is the copy it had, not a part of the bucket. A
// read that times out or ends early removes nothing, and a timed-out one is read again by the
// watcher itself. The watcher records whether its copy can be served (Servable): never before its
// first complete read, and not once the copy has stopped following the bucket for longer than a
// bound (the stale episode).
//
// The watcher also finds by itself a bucket deleted, or deleted and created again, under it.
// nats.go's ordered consumer does not end on a recreate: once its idle heartbeats stop it moves
// onto the new stream at the old stream's last sequence + 1, says nothing, and so skips the new
// bucket's first records, every one of them when the old stream was the longer. So the watcher reads
// its bucket's stream every few seconds (probe), again before a read completes, and again before it
// applies a record while following when its last confirmation is older than nats.go could have moved
// the consumer (streamConfirmMaxAge), sharing a read already in flight and sending none of its own
// within a probe interval of the last one (confirmation). A different stream stops the copy
// following at once and the watcher reads the new bucket whole; a missing one stops it following
// until the bucket is back, and reads that keep failing stop it following once they outlast the
// stale bound (unread). Check, the stores' health probe, stays the fallback for a recreate these
// reads missed.
package kvwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/sjawhar/envoy/internal/bus"
)

// Cache is what a watcher feeds: one in-memory copy of a bucket, read again whole by every watcher
// a watch installs. Each hook is called with the watcher's own locks taken as its ordering needs
// and its state lock released, so a hook may take the cache's own mutex and, under it, call the
// watcher's read accessors (Handle, Epoch, Following): the order is the cache's mutex, then the
// watcher's, never the reverse. No two hooks of one watcher run at once.
type Cache interface {
	// Apply takes one record the current read delivered. A record from a watcher already replaced
	// is dropped before it reaches the cache.
	Apply(entry Entry, read Read)
	// BeginRead says a new read of the whole bucket starts. The cache keeps every entry through it.
	// A read whose Recreated is set reads a bucket that was deleted and created again, whose
	// revisions number from 1 again, so the cache drops its revision fence (and keeps its entries).
	BeginRead(read Read)
	// CompleteRead says the read delivered every record of its bucket: the cache removes the entries
	// the read found no evidence of (Copy.CompleteRead). A read that times out or ends before its
	// end marker never completes, and removes nothing.
	CompleteRead(read Read)
}

// Read names one read of the bucket: the watcher generation that reads it, as the cache sees it.
type Read struct {
	// Epoch counts the bucket's incarnations the watcher has read: it moves only when a watch
	// installs onto a stream other than the one the watcher read before (the bucket was recreated),
	// and a Rewatch onto the same stream keeps it. A write a store made through a handle of an
	// earlier epoch (Handle) is not applied to the cache.
	Epoch uint64
	// Recreated says this read is the first of a new epoch.
	Recreated bool
	// MaxAge is the stream's MaxAge, the bucket's TTL, as the watch read it before it installed
	// this read: a bucket recreated with another TTL moves it. 0 keeps records for good.
	MaxAge time.Duration
}

// Watcher is one cache's KV watcher, from New.
type Watcher struct {
	name   string
	bucket string
	cache  Cache
	log    *slog.Logger
	// now is the clock the stale episode, the age of a stream confirmation and the time since a
	// scan's last entry (idleExpired) are measured on, time.Now unless WithNow replaced it.
	now func() time.Time
	// staleBound is the longest stale episode served (defaultStaleBound unless WithStaleBound
	// replaced it); a stream's MaxAge can shorten it (boundFor).
	staleBound time.Duration
	// probeInterval is how often the watcher reads its bucket's stream (probe): streamProbeInterval
	// unless WithStreamProbeInterval replaced it, 0 for no periodic probe.
	probeInterval time.Duration
	// scanIdle is how long a read's scan may deliver nothing before the read ends as timed out
	// (idleExpired): ScanIdleTimeout unless WithScanIdleTimeout replaced it.
	scanIdle time.Duration

	ready     chan struct{}
	readyOnce sync.Once

	// applyMu serializes the cache's hooks, and lets watch wait out an entry already being applied
	// by the watcher it replaces. It is taken before mu.
	applyMu sync.Mutex

	// first is the handle Start watches.
	first bus.KeyValue

	// after arms the wait between a stop nobody asked for and the ERROR that says no repair came,
	// the wait before a read is read again, the wait before the next stream probe and the scan's
	// idle wait: time.AfterFunc unless WithAfterFunc replaced it, so a test fires those waits
	// itself.
	after AfterFunc

	mu sync.RWMutex
	// kv is the handle the store reads and writes through: first, until a watch moves it together
	// with the watcher, so the store writes through the connection its cache reads.
	kv bus.KeyValue
	// watcher is the current generation's subscription, nil when none runs.
	watcher    *subscription
	generation uint64
	stopped    bool
	err        error
	// repairTimer is the armed wait left by a stop nobody asked for, and is the marker that such a
	// stop is outstanding and not yet reported: a repair that worked, and an ordered Stop, clear
	// it, and the one report clears it too, so exactly one ERROR follows one stop. It always
	// belongs to the current generation: a stop arms it only while its generation is current, and
	// the two places the generation moves (watch, Stop) clear it. Its callback carries the
	// generation it was armed for, so one still running after a repair cleared it reports nothing
	// (reportUnrepaired).
	repairTimer Timer
	// rereadTimer is the armed wait before the current generation's bucket is read again (reread):
	// after its scan timed out, after a read of its stream found the bucket recreated, or after such a
	// re-read failed to start. It is the marker that such a read is due, so a probe that finds the
	// same recreate again starts no second one. The two places the generation moves (watch, Stop)
	// clear it, and its callback carries the generation it was armed for, so one still running after
	// that reads nothing.
	rereadTimer Timer
	// probeTimer is the armed wait before the current generation's next stream probe (probe). Each
	// watch that installs arms it for its own generation and Stop clears it; its callback carries
	// that generation, so one still running after either reads nothing.
	probeTimer Timer
	// idleTimer is the armed wait of the current generation's scan, from its install until its
	// nil, its time out or its watcher's end, replaced by the next watch and cleared by Stop. Its
	// callback only signals that generation's consume, which reads the time since the scan's last
	// entry before it ends anything (idleExpired), so a callback that runs late, after a reset,
	// the scan's end or a newer generation, ends nothing.
	idleTimer Timer
	// stream is when the bucket's stream the current watcher reads was created. A different time
	// on a later read of the stream means the bucket was deleted and created again.
	stream time.Time
	// confirmedAt is when the latest read that found the bucket's stream to be stream was sent (the
	// installing watch's own, and confirmStream's), on now: the gate on records delivered while
	// following measures streamConfirmMaxAge from it (confirmation).
	confirmedAt time.Time
	// confirming counts the reads of the bucket's stream in flight (confirmStream), and confirmDone,
	// made when the count rises from 0, is closed when it falls back to 0: a record the gate holds,
	// and a probe that finds a read in flight, wait on it rather than send a read of their own
	// (confirmation, probe). attemptedAt is when the latest of those reads ended, whatever it found,
	// on now: the gate sends no read of its own within the probe interval after it. unreadSince is
	// when the first of an unbroken run of reads that got no answer was sent, zero once one answers
	// or a watch installs (unread).
	confirming  int
	confirmDone chan struct{}
	attemptedAt time.Time
	unreadSince time.Time
	// epoch is the current read's Read.Epoch, and bound its stale bound (boundFor).
	epoch uint64
	bound time.Duration

	// completedOnce is set by the first read that completed, and following while the copy follows
	// the bucket: from a read's completion until the next stop (stopFollowing, watch). staleSince is
	// when the current stale episode began, zero while following and before any stop: an episode
	// lasts from the first stop until a read completes, across any number of failed reads.
	completedOnce bool
	following     bool
	staleSince    time.Time
	// reading is set while the current generation's read is under way: installed, its scan not yet
	// ended, and nothing has taken from it the right to complete (Check, Disconnected).
	reading bool
	// disconnects counts the disconnects Disconnected has reported, and readDisconnects is the
	// count when the current generation's watch began. A read a disconnect overlapped completes
	// nothing (completes): what the watcher delivered across the outage is no proof it delivered
	// every record, and a copy whose connection is down does not follow its bucket.
	disconnects     uint64
	readDisconnects uint64

	// watching counts the watches in flight, so End can wait for one that may still arm a watcher.
	// A watch joins it under mu, and only while the watcher is not stopped, so once Stop has run it
	// only falls. idle, set by End while it waits, is closed when it falls to 0.
	watching int
	idle     chan struct{}
	// retired are the watchers Stop took out of service and those a watch armed after it, which
	// End ends. ended is set once End has taken them, and endCause, with it, to why End stopped
	// waiting for a watch still starting, when it did.
	retired  []*subscription
	ended    bool
	endCause error
	// endOnce runs End's work once: a later End waits for it to finish.
	endOnce sync.Once
}

// New returns the watcher that feeds cache from the bucket kv is a handle on. name labels its log
// lines and its terminal error ("<name> watcher stopped"). Every watcher it installs is a read of
// the whole bucket (Cache): the cache keeps its entries through the read and removes what the read
// did not find only once it completes. New watches nothing until Start. kv is a bus.KeyValue, the
// handle bus.EnsureKeyValue opens, and each Rewatch opens its replacement with bus.OpenKeyValue,
// so every handle the store reads and writes through checks its keys.
func New(name string, kv bus.KeyValue, cache Cache, options ...Option) *Watcher {
	w := &Watcher{
		name:          name,
		bucket:        kv.Bucket(),
		cache:         cache,
		now:           time.Now,
		staleBound:    defaultStaleBound,
		probeInterval: streamProbeInterval,
		scanIdle:      ScanIdleTimeout,
		ready:         make(chan struct{}),
		first:         kv,
		kv:            kv,
		after:         func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) },
	}
	for _, option := range options {
		option(w)
	}
	w.bound = w.staleBound
	return w
}

// logger is the logger WithLogger set, or the default one.
func (w *Watcher) logger() *slog.Logger {
	if w.log == nil {
		return slog.Default()
	}
	return w.log
}

// Start arms the first watcher in the background, so a store's Open never waits on its watch. A
// first start that fails logs its error. When no watcher is current it records the error and
// releases readiness, so a caller waiting for the cache sees a cache with no complete read (which
// Servable refuses) rather than hanging. When a reconnect hook's Rewatch has armed one meanwhile,
// it does neither: the error is not that watcher's, and that watcher releases readiness once it has
// delivered every existing key.
func (w *Watcher) Start() {
	go func() {
		err := w.watch(w.first, 0)
		if err == nil {
			return
		}
		w.logger().Error(w.name+" watch failed", slog.String("error", err.Error()))
		w.mu.Lock()
		current := w.watcher != nil
		if !current && !w.stopped {
			w.err = err
		}
		w.mu.Unlock()
		if !current {
			w.signalReady()
		}
	}()
}

// Rewatch opens the bucket on conn and replaces the current watcher with one there, clearing a
// recorded terminal error once the replacement has started; the replacement reads the whole bucket
// again (Cache). The handle (KV) moves with it, so the store writes through the connection its
// cache reads. After a server restart conn is the connection the store opened on, reconnected in
// place; when the bus replaces a closed connection, conn is the new one and the cache moves to it.
// A failure leaves the watcher, the handle and whether the copy follows its bucket as they were: a
// healthy watcher stays following, and one that had stopped already began its stale episode.
// After Stop it arms nothing and still moves the handle. A failure repairs nothing, so when a stop
// nobody asked for is still outstanding it reports that stop as unrepaired (reportFailedRepair)
// before returning the error unchanged, which its caller already logs.
func (w *Watcher) Rewatch(conn *nats.Conn) error {
	js, err := conn.JetStream(nats.MaxWait(10 * time.Second))
	if err != nil {
		w.reportFailedRepair()
		return fmt.Errorf("open %s JetStream: %w", w.name, err)
	}
	kv, err := bus.OpenKeyValue(conn, js, w.bucket)
	if err != nil {
		w.reportFailedRepair()
		return fmt.Errorf("open %s KV bucket: %w", w.name, err)
	}
	if err := w.watch(kv, 0); err != nil {
		w.reportFailedRepair()
		return fmt.Errorf("watch %s KV bucket: %w", w.name, err)
	}
	return nil
}

// KV is the bucket handle the store reads and writes through.
func (w *Watcher) KV() bus.KeyValue {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.kv
}

// Handle is the bucket handle the store reads and writes through and the epoch of the bucket it is
// a handle on, read together: a store captures both before a KV call and applies the call's result
// to its cache only while Epoch is still that epoch, so a result from a bucket since recreated
// never fences out the new bucket's revisions.
func (w *Watcher) Handle() (bus.KeyValue, uint64) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.kv, w.epoch
}

// Epoch is the current read's Read.Epoch: it moves only when a watch installs onto a recreated
// bucket.
func (w *Watcher) Epoch() uint64 {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.epoch
}

// Err is the current watcher's terminal error, or nil while it runs.
func (w *Watcher) Err() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.err
}

// Ready reports whether the cache is ready: the current watcher's scan ended (complete or timed
// out) or the watcher ended on its own, or the first start failed with no watcher current. It is
// the one-shot release of a startup's warm-up, not whether the copy can be served (Servable).
func (w *Watcher) Ready() bool {
	select {
	case <-w.ready:
		return true
	default:
		return false
	}
}

// WaitReady blocks until the cache is ready or ctx is done. Callers bound ctx: a watch on a
// healthy cluster completes in milliseconds, but a bucket can be unreachable.
func (w *Watcher) WaitReady(ctx context.Context) error {
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// watch arms a watcher on kv and replaces the current one, moving the handle to kv; the new watcher
// is a new read of the whole bucket. The replaced watcher is stopped at once, whether or not its
// scan has ended, and its consume goes on reading it until it closes, dropping what it delivers.
// The switch to the new watcher and the read's BeginRead happen under applyMu, so no cache hook
// runs between them: an apply the replaced watcher already started finishes first, and a watch
// that switches after this one computes recreated against the new stream and applies only after
// this read began. Two Rewatches do overlap, the bus's reconnect hook and the listener's
// self-health rebuild. A watch reads its stream before it arms its watcher and takes the locks, so
// it can arrive after a newer watch has switched to a recreated bucket: a watch whose stream is
// older than a running, healthy watcher's is discarded, and that watcher stays. A watch with a
// nonzero expect, a re-read of generation expect (reread), installs only while that generation is
// still current, and is discarded the same way otherwise: whatever moved the generation began a
// read of its own. A discarded watcher is stopped at once and read until it closes. A watch that
// installs arms its generation's stream probe and its scan's idle wait, and counts the read of the
// stream it made as the latest confirmation of it (confirmedAt). After Stop it arms nothing and
// only moves the handle; a watcher it armed while Stop ran is kept for End.
//
// Each watcher lives in a context of its own (subscription): nats.go ends the subscription when
// that context ends and parks a goroutine on it until then (bus.KeyValue.WatchKeys), so it is
// cancelled once the watcher is stopped or has ended, never before. Its create request is bounded
// by nats.go itself, since the context carries no deadline (WatchKeys): a create that fails is the
// watch's error, as a failed stream read is, and leaves nothing to stop.
func (w *Watcher) watch(kv bus.KeyValue, expect uint64) error {
	w.mu.Lock()
	stopped := w.stopped
	if stopped {
		w.kv = kv
	} else {
		w.watching++
	}
	disconnects := w.disconnects
	w.mu.Unlock()
	if stopped {
		return nil
	}
	defer w.watchDone()
	// The warm-up's cost as the caller waiting on readiness sees it: the stream read and the
	// watcher's create request are part of it, not just the entries that follow.
	started := time.Now()
	sent := w.now()
	state, err := kv.ReadStreamState()
	if err != nil {
		return err
	}
	stream := state.Created
	ctx, cancel := context.WithCancel(context.Background())
	watcher, err := kv.WatchKeys(ctx, jetstream.AllKeys)
	if err != nil {
		cancel()
		return err
	}
	sub := &subscription{watcher: watcher, cancel: cancel}
	w.applyMu.Lock()
	w.mu.Lock()
	if w.stopped {
		// Stop ran while the watch was starting. Stopping this watcher here would be a server
		// request outside the drain's deadline, so it is kept for End, which runs within it; until
		// then it is read and dropped, since a drain waits for every pending message to be
		// delivered. Once End has taken the retired watchers, which it does only after this watch
		// returns or its deadline passes, nothing else would end this one, so the watch ends it.
		w.kv = kv
		ended, endCause := w.ended, w.endCause
		if !ended {
			w.retired = append(w.retired, sub)
		}
		w.mu.Unlock()
		w.applyMu.Unlock()
		go sub.discard()
		if ended {
			w.logEnd(lateStop(sub.stop(), endCause))
		}
		return nil
	}
	// A newer watch already switched to a recreated bucket and its watcher is running. This
	// watcher may be on the old stream, so installing it would begin a read of the old bucket over
	// the copy the current watcher reads. The rule holds only against a live, healthy watcher (an
	// installed stream with no recorded error has one running): creation times do not always grow
	// (a JetStream restore keeps the snapshot's, and a recreate can follow a clock step back), so
	// when the current watcher has ended or Check has flagged its bucket, the watch installs as
	// usual, and if it did read the old stream the next Check flags that too. A re-read needs no
	// such rule: its generation is still current, so no watch switched since, and the stream it
	// read is the bucket's, however old its creation time (a probe that found a restored bucket).
	older := expect == 0 && w.err == nil && stream.Before(w.stream)
	if older || (expect != 0 && w.generation != expect) {
		w.mu.Unlock()
		w.applyMu.Unlock()
		go sub.discard()
		// Stop reports a consumer the server has lost, or a subscription already ended, to its
		// caller, and neither is worth a line.
		_ = sub.stop()
		return nil
	}
	previous := w.watcher
	recreated := !w.stream.IsZero() && !stream.Equal(w.stream)
	if recreated {
		w.epoch++
	}
	w.generation++
	generation := w.generation
	w.watcher = sub
	w.kv = kv
	w.stream = stream
	w.confirmedAt = sent
	w.unreadSince = time.Time{}
	w.err = nil
	w.bound = w.boundFor(state.MaxAge)
	// A new read begins: the copy stops following until it completes.
	w.stopFollowing()
	w.reading = true
	w.readDisconnects = disconnects
	// The replacement runs, so the stop it repaired needs no ERROR, and the read a timed-out scan
	// or a recreate was waiting to begin is this one. The stream probe and the scan's idle wait
	// follow the new generation.
	w.clearRepairArm()
	w.clearRereadArm()
	w.armProbe(generation)
	idle := make(chan struct{}, 1)
	idleSince := w.now()
	w.armIdle(w.scanIdle, idle)
	read := Read{Epoch: w.epoch, Recreated: recreated, MaxAge: state.MaxAge}
	w.mu.Unlock()
	w.cache.BeginRead(read)
	w.applyMu.Unlock()
	if recreated {
		w.logger().Info(w.name+" bucket was recreated; its cache is re-read from the new bucket and kept until the read completes",
			slog.String("bucket", w.bucket))
	}
	if previous != nil {
		// Its consume goes on reading it until it closes. Stop reports a consumer the server has
		// lost to its caller, never at ERROR.
		_ = previous.stop()
	}
	go w.consume(sub, generation, read, started, idle, idleSince)
	return nil
}

// consume feeds the cache what the watcher of generation delivers, as the read read, and reads it
// until nats.go closes its updates, whether or not it is still current. The first nil ends the
// read's scan: a complete scan completes the read (completeRead). A scan that delivers nothing for
// the idle timeout, counted from since (when its idle wait was armed) and then from its last entry,
// ends as timed out instead (idleExpired, signalled on idle by the scan's idle wait): the read
// completes nothing and is read again (readAgainLater), and the watcher goes on applying what it
// delivers, its scan's nil included as nothing more than a late marker. A watcher that ends before
// its scan's nil completes nothing either: the cache removes nothing for a read it cannot vouch
// for. A record delivered after the read completed waits for a read of the bucket's stream when
// the latest confirmation of it is too old to vouch for the record (applyCurrent).
func (w *Watcher) consume(sub *subscription, generation uint64, read Read, started time.Time, idle chan struct{}, since time.Time) {
	var scan KeyScan
	scanning := true
	last := since
	updates := sub.watcher.Updates()
	for {
		var delivered jetstream.KeyValueEntry
		select {
		case entry, open := <-updates:
			if !open {
				w.closed(sub, generation)
				return
			}
			delivered = entry
		case <-idle:
			if scanning && w.idleExpired(generation, last, idle) {
				scanning = false
				w.endScan(generation, read, scan, started, true)
			}
			continue
		}
		if delivered == nil {
			// The watcher sends a nil once it has delivered the current value of every key. Only
			// the current watcher's completes a read, releases readiness, or logs: a replaced
			// one's scan says nothing about what the current watcher has delivered. A nil past
			// the scan's end -- one that came after its idle wait ended it -- says nothing more.
			if scanning {
				scanning = false
				w.endScan(generation, read, scan, started, false)
			}
			continue
		}
		entry := JetStreamEntry(delivered)
		if scanning {
			scan.count(entry)
			last = w.now()
		}
		w.applyCurrent(entry, generation, read)
	}
}

// endScan ends generation's scan, which delivered scan, at its nil or, when timedOut, at its idle
// wait. The current generation's scan disarms that wait, logs its warm-up line and releases
// readiness. A complete scan completes the read (completeRead). A timed-out one is logged and
// never recorded as the watcher's error: the watcher is still running, so Err(), Ping(), /healthz
// and the self-health rebuild must not read it as dead, and the watcher reads the bucket again
// itself (readAgainLater).
func (w *Watcher) endScan(generation uint64, read Read, scan KeyScan, started time.Time, timedOut bool) {
	w.mu.Lock()
	current := generation == w.generation
	if current {
		w.clearIdleArm()
	}
	w.mu.Unlock()
	if current {
		w.logWarmUp(scan, time.Since(started), timedOut)
	}
	if timedOut {
		w.readAgainLater(generation, timedOutCause)
	} else {
		w.completeRead(generation, read)
	}
	if w.current(generation) {
		w.signalReady()
	}
}

// closed handles the end of sub, generation's watcher, whose updates nats.go has closed: it cancels
// sub's context, which releases the goroutine nats.go parks on it, and, when generation is still
// current, records the end as the watcher's terminal error.
func (w *Watcher) closed(sub *subscription, generation uint64) {
	sub.cancel()
	w.mu.Lock()
	if generation != w.generation {
		// Replaced or stopped: the end belongs to no live watcher.
		w.mu.Unlock()
		return
	}
	w.watcher = nil
	w.clearIdleArm()
	err := errors.New(w.name + " watcher stopped")
	w.err = err
	w.stopFollowing()
	// The watcher ended on its own: the connection it was on closed, or the bucket went away. The
	// cache is frozen until a Rewatch replaces it, and Err says so. The stop is logged at WARN and
	// not at ERROR, because nothing repairs a watcher while the listener's connection to NATS is
	// down: a line held back until a repair result is known would leave such an outage ending in a
	// restart with no line at all, while an ERROR at the stop itself is noise whenever the rebuild
	// a moment later repairs it. The ERROR is the repair's to make - a failed Rewatch reports at
	// once, and this wait reports a stop nothing has repaired within unrepairedDelay.
	w.repairTimer = w.after(unrepairedDelay, func() {
		w.reportUnrepaired("no repair within "+unrepairedDelay.String(), generation)
	})
	w.mu.Unlock()
	w.logger().Warn(w.name+" watcher stopped", slog.String("error", err.Error()))
	w.signalReady()
}

// armIdle arms the current generation's scan idle wait after d, under mu, in place of any armed
// before. Its callback signals idle, the channel that generation's consume reads, and does nothing
// else; a signal already pending is enough.
func (w *Watcher) armIdle(d time.Duration, idle chan<- struct{}) {
	w.clearIdleArm()
	w.idleTimer = w.after(d, func() {
		select {
		case idle <- struct{}{}:
		default:
		}
	})
}

// idleExpired reports whether generation's scan, whose last entry consume took at last, has gone
// the idle timeout without one, when its idle wait signalled. A signal for a generation no longer
// current, after Stop, or once the wait was disarmed (the scan's nil, or the watcher's end) ends
// nothing. Within the timeout -- an entry came since the wait was armed -- it arms the wait again
// for what remains, so a scan times out the idle timeout after its last entry, with one wait armed
// per timeout rather than one per entry.
func (w *Watcher) idleExpired(generation uint64, last time.Time, idle chan<- struct{}) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if generation != w.generation || w.stopped || w.idleTimer == nil {
		return false
	}
	if remaining := w.scanIdle - w.now().Sub(last); remaining > 0 {
		w.armIdle(remaining, idle)
		return false
	}
	w.clearIdleArm()
	return true
}

// clearIdleArm drops the armed scan idle wait, under mu.
func (w *Watcher) clearIdleArm() {
	if w.idleTimer == nil {
		return
	}
	w.idleTimer.Stop()
	w.idleTimer = nil
}

// completeRead completes generation's read, whose scan delivered every record. While the read may
// complete, it first reads the bucket's stream, outside the locks (confirmStream): nats.go's ordered
// consumer can move onto a bucket recreated during the scan and end the scan with records of two
// streams, so a read is complete only of the stream it was installed on. Another stream, or none,
// stops the copy following and completes nothing, and the new bucket is read again; a stream read
// that fails completes nothing and is read again after rereadDelay, as a timed-out scan is. Then,
// under applyMu, so no other hook runs between the check and the state write, it checks the read
// may still complete (completes), has the cache sweep what the read did not find (CompleteRead),
// and only then records that the copy follows its bucket, so expiry that waits on following
// resumes only after the sweep. A recreate found, or a disconnect, between the sweep and the state
// write skips the write: the read did deliver every record of the stream it read, so its sweep
// stands, and the copy stays not following until the next read.
func (w *Watcher) completeRead(generation uint64, read Read) {
	w.mu.RLock()
	completes := w.completes(generation)
	w.mu.RUnlock()
	if completes {
		switch w.confirmStream(generation) {
		case streamReplaced:
			return
		case streamUnread:
			w.readAgainLater(generation, unconfirmedCause)
			return
		}
	}
	w.applyMu.Lock()
	defer w.applyMu.Unlock()
	w.mu.Lock()
	if generation == w.generation {
		w.reading = false
	}
	completes = w.completes(generation)
	w.mu.Unlock()
	if !completes {
		return
	}
	w.cache.CompleteRead(read)
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.completes(generation) {
		w.following = true
		w.completedOnce = true
		w.staleSince = time.Time{}
	}
}

// completes reports, under mu, whether generation's read may complete: it is still the current
// one, the watcher is not stopped, no terminal error is recorded (its own end, or a recreate Check
// found), and no disconnect came since its watch began.
func (w *Watcher) completes(generation uint64) bool {
	return generation == w.generation && !w.stopped && w.err == nil && w.disconnects == w.readDisconnects
}

// logWarmUp logs the line that says what one read's scan cost: the cache, the bucket, how long it
// took, the live keys the scan delivered, the delete markers it streamed past to find them, and how
// it ended, so a restart's own log names what a slow readiness spent (LEGION-374). Every read logs
// one, so a re-read after a timed-out scan or a recreate is in the log too. The two counts are
// disjoint, as the role restore line's are: a marker is not a key the cache keeps, and one total of
// both reads as keys the warm-up failed to apply.
// entries counts what the scan delivered, which is what sizes the replay; a cache can hold fewer,
// since each one evicts a value it cannot decode. A scan the idle wait ended logs the same line at
// WARN, since the read behind it is incomplete and is read again.
func (w *Watcher) logWarmUp(scan KeyScan, elapsed time.Duration, timedOut bool) {
	level, outcome := slog.LevelInfo, "completed"
	if timedOut {
		level, outcome = slog.LevelWarn, "timed out"
	}
	w.logger().Log(context.Background(), level, w.name+" cache warm-up",
		slog.String("bucket", w.bucket),
		slog.Int64("elapsed_ms", elapsed.Milliseconds()),
		slog.Int("entries", scan.Puts),
		slog.Int("delete_markers", scan.Markers),
		slog.String("outcome", outcome),
	)
}

// applyCurrent applies entry, a record of read, only while generation is still the current
// watcher's. A record delivered while the copy follows its bucket on a stream confirmation older
// than streamConfirmMaxAge first waits for the read of the stream in flight, or one it sends itself
// when none is and none ended within the probe interval (confirmation), so a record from a consumer
// nats.go moved onto a recreated bucket is applied only once the copy has stopped following
// whenever such a read can tell. The probe keeps the confirmation of a healthy bucket fresh, so the
// wait is rare; a read that fails changes nothing, and the record is applied.
func (w *Watcher) applyCurrent(entry Entry, generation uint64, read Read) {
	switch wait, send := w.confirmation(generation); {
	case wait != nil:
		<-wait
	case send:
		w.confirmStream(generation)
	}
	w.applyMu.Lock()
	defer w.applyMu.Unlock()
	if w.current(generation) {
		w.cache.Apply(entry, read)
	}
}

// current reports whether generation is still the current watcher's.
func (w *Watcher) current(generation uint64) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return generation == w.generation
}

func (w *Watcher) signalReady() {
	w.readyOnce.Do(func() { close(w.ready) })
}
