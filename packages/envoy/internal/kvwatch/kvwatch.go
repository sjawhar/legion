// Package kvwatch keeps an in-memory cache fed by one JetStream KV bucket's watcher, and owns that
// cache's whole watch lifecycle: the first start, readiness, moving to a replacement connection,
// telling the current watcher from one it replaced, recording the terminal error when the current
// watcher ends on its own, and arming nothing once stopped. The interest registry, the session
// registry and the CI store each keep one, so the listener rewatches, health-checks and stops all
// three the same way. It also owns the one-shot scan of a bucket's existing keys
// (ScanExistingKeys), and the one reading of how nats.go ended a scan that both share
// (idleTimeout).
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
// the consumer (streamConfirmMaxAge). A different stream stops the copy following at once and the
// watcher reads the new bucket whole; a missing one stops it following until the bucket is back.
// Check, the stores' health probe, stays the fallback for a recreate these reads missed.
package kvwatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"

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
	Apply(entry nats.KeyValueEntry, read Read)
	// BeginRead says a new read of the whole bucket starts. The cache keeps every entry through it.
	// A read whose Recreated is set reads a bucket that was deleted and created again, whose
	// revisions number from 1 again, so the cache drops its revision fence (and keeps its entries).
	BeginRead(read Read)
	// CompleteRead says the read delivered every record of its bucket: the cache removes the entries
	// the read found no evidence of (Marks, Sweep). A read that times out or ends before its end
	// marker never completes, and removes nothing.
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

// ErrRefilling is what Servable answers while the copy cannot be served: no read has completed
// yet, or the copy has not followed the bucket for longer than the stale bound.
var ErrRefilling = errors.New("kvwatch: the cache has no complete read of its bucket recent enough to serve")

// defaultStaleBound is how long a copy that stopped following its bucket is still served. It is
// under half the 5-minute record lifetime of the session bucket, so a copy past it would mostly
// list sessions that already ended; within it the reconnect hook and the listener's 30 s
// self-health rebuild normally complete a read.
const defaultStaleBound = 2 * time.Minute

// rereadDelay is how long a watcher waits before it reads its bucket again after the current
// read's scan timed out, or a re-read failed to start.
const rereadDelay = 5 * time.Second

// streamProbeInterval is how often a watcher reads its bucket's stream to find the bucket deleted,
// or deleted and created again, under it (probe). One STREAM.INFO per watcher every 2 s keeps the
// latest confirmation of a healthy bucket's stream younger than streamConfirmMaxAge, so the gate on
// delivered records rarely waits for a read of its own.
const streamProbeInterval = 2 * time.Second

// streamConfirmMaxAge is the oldest confirmation of the bucket's stream under which a record
// delivered while the copy follows its bucket is applied without reading the stream first
// (unconfirmed). It holds by nats.go's own constants (v1.50.0 js.go), not by timer luck. Deleting
// the stream ends the old ordered consumer's records and idle heartbeats, and nats.go moves the
// consumer onto a stream created again only from activityCheck, which runs every hbcThresh (2) ×
// the 5 s idle heartbeat (orderedHeartbeatsInterval) and resets the consumer only when nothing - no
// record, no heartbeat - arrived since the check before it, 10 s earlier. While the stream exists
// the server sends one or the other at least every 5 s, so the earliest reset comes 10 s after the
// last arrival before the delete: at least 5 s after the delete itself. A confirmation is the time
// its STREAM.INFO request was sent, and an answer naming the watched stream means the delete came
// after that send. So a record in hand at most 4 s after a confirmation's send arrived before any
// reset could have happened, at the earliest 5 s after that send, and is the watched stream's; a
// record past that waits for one synchronous read of the stream, which stops the copy following
// before the record is applied when the stream is no longer the watched one. 4 s leaves a second of
// margin under the 5 s.
const streamConfirmMaxAge = 4 * time.Second

// The causes a re-read names in the line it logs when it fails to start (reread). A timed-out
// scan's wording is the refill rig's key (internal/testnats/cmd/refillrig).
const (
	timedOutCause    = "a timed-out scan"
	recreatedCause   = "a recreated bucket"
	unconfirmedCause = "a read whose stream could not be confirmed"
)

// Watcher is one cache's KV watcher, from New.
type Watcher struct {
	name   string
	bucket string
	cache  Cache
	log    *slog.Logger
	// now is the clock the stale episode and the age of a stream confirmation are measured on,
	// time.Now unless WithNow replaced it.
	now func() time.Time
	// staleBound is the longest stale episode served (defaultStaleBound unless WithStaleBound
	// replaced it); a stream's MaxAge can shorten it (boundFor).
	staleBound time.Duration
	// probeInterval is how often the watcher reads its bucket's stream (probe): streamProbeInterval
	// unless WithStreamProbeInterval replaced it, 0 for no periodic probe.
	probeInterval time.Duration

	ready     chan struct{}
	readyOnce sync.Once

	// applyMu serializes the cache's hooks, and lets watch wait out an entry already being applied
	// by the watcher it replaces. It is taken before mu.
	applyMu sync.Mutex

	// first is the handle Start watches.
	first bus.KeyValue

	// after arms the wait between a stop nobody asked for and the ERROR that says no repair came,
	// the wait before a read is read again, and the wait before the next stream probe:
	// time.AfterFunc unless WithAfterFunc replaced it, so a test fires those waits itself.
	after AfterFunc

	mu sync.RWMutex
	// kv is the handle the store reads and writes through: first, until a watch moves it together
	// with the watcher, so the store writes through the connection its cache reads.
	kv      bus.KeyValue
	watcher nats.KeyWatcher
	// scanned is closed by the current watcher's consume once that watcher's scan has ended (at
	// its first nil, or with its updates channel closed first), so a watch that replaces it knows
	// whether it may stop it at once or must wait for that end (stopAfterScan).
	scanned    chan struct{}
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
	// stream is when the bucket's stream the current watcher reads was created. A different time
	// on a later read of the stream means the bucket was deleted and created again.
	stream time.Time
	// confirmedAt is when the latest read that found the bucket's stream to be stream was sent (the
	// installing watch's own, and confirmStream's), on now: the gate on records delivered while
	// following measures streamConfirmMaxAge from it (unconfirmed).
	confirmedAt time.Time
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
	retired []nats.KeyWatcher
	// deferred are the watchers a watch replaced or discarded before their scan ended, each waiting
	// for that end to be stopped (stopAfterScan). Whoever takes one from it stops it: its own wait,
	// or End, which takes those still waiting with the retired ones.
	deferred map[*deferredStop]struct{}
	ended    bool
	endCause error
	// endOnce runs End's work once: a later End waits for it to finish.
	endOnce sync.Once
}

// Option configures a Watcher.
type Option func(*Watcher)

// WithLogger sets the logger the watcher writes its lines through: each read's warm-up, a first
// start that failed, a recreated or deleted bucket, a re-read that failed and its own end. The
// listener passes its own, so they are JSON records with machine_id, as internal/store's are
// (store.WithLogger says why the listener never sets the default instead). Without it the watcher
// logs where slog.Default() points.
func WithLogger(log *slog.Logger) Option {
	return func(w *Watcher) { w.log = log }
}

// Timer is a pending wait an AfterFunc armed, which a repair that worked, or a read that made the
// wait moot, stops. *time.Timer is one.
type Timer interface{ Stop() bool }

// AfterFunc arms f to run once d has passed, as time.AfterFunc does.
type AfterFunc func(d time.Duration, f func()) Timer

// unrepairedDelay is how long a stop nobody asked for waits for a repair before it is reported as
// an ERROR. The bus's reconnect hook rewatches within seconds of a reconnect, so a minute passes
// only when nothing is repairing the cache at all.
const unrepairedDelay = time.Minute

// repairFailedReason names the report a failed Rewatch makes: the repair the stop was waiting for
// ran and did not work, so the ERROR need not wait out unrepairedDelay.
const repairFailedReason = "repair failed"

// WithAfterFunc sets how the watcher arms its waits, time.AfterFunc by default: the unrepairedDelay
// wait for a repair, the wait before a bucket is read again (rereadDelay after a timed-out scan or
// a re-read that failed to start, none after a recreate a stream read found), and the wait before
// the next stream probe. The production waits are a minute and seconds, so a test hands in its
// own, keeps each callback and fires it itself rather than sleeping for it.
func WithAfterFunc(after AfterFunc) Option {
	return func(w *Watcher) { w.after = after }
}

// WithNow sets the clock the stale episode (Servable) and the age of the latest stream confirmation
// (streamConfirmMaxAge) are measured on, time.Now by default, so a test moves past the stale bound
// without waiting it out. Elapsed time is never read from record timestamps.
func WithNow(now func() time.Time) Option {
	return func(w *Watcher) { w.now = now }
}

// WithStreamProbeInterval sets how often the watcher reads its bucket's stream to find a recreate
// (probe), in place of the 2-second default. 0 arms no periodic probe, for a test that counts or
// fires the watcher's other waits (WithAfterFunc), or recreates the bucket and drives the reads
// itself. The stream is still read before a read completes and before a record is applied on an
// old confirmation, so a copy still never follows a stream other than the one it read.
func WithStreamProbeInterval(interval time.Duration) Option {
	return func(w *Watcher) { w.probeInterval = interval }
}

// WithStaleBound sets the longest stale episode served, in place of the 2-minute default, for
// tests. A stream with a MaxAge still caps it at half that MaxAge.
func WithStaleBound(bound time.Duration) Option {
	return func(w *Watcher) { w.staleBound = bound }
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

// boundFor is the stale bound of a stream whose MaxAge is maxAge: staleBound, or half the MaxAge
// when that is shorter, so a copy is never served once most of its records would have expired.
func (w *Watcher) boundFor(maxAge time.Duration) time.Duration {
	if maxAge > 0 {
		return min(w.staleBound, maxAge/2)
	}
	return w.staleBound
}

// logger is the logger WithLogger set, or the default one.
func (w *Watcher) logger() *slog.Logger {
	if w.log == nil {
		return slog.Default()
	}
	return w.log
}

// Start arms the first watcher in the background, so a store's Open never waits on WatchAll. A
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
	kv, err := bus.OpenKeyValue(js, w.bucket)
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

// Servable reports whether the copy can be served: nil once a read has completed and the copy
// either follows its bucket or stopped following it no longer ago than the stale bound (2 minutes,
// or half the stream's MaxAge when that is shorter), and ErrRefilling otherwise. It reads the
// watcher's state only, never the cache, so a caller may ask it under the cache's mutex.
func (w *Watcher) Servable() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if w.completedOnce && (w.following || w.now().Sub(w.staleSince) <= w.bound) {
		return nil
	}
	return ErrRefilling
}

// Following reports whether the copy follows its bucket: a read has completed since the copy last
// stopped following (a watch installing a new read, the current watcher's own end, a deleted or
// recreated bucket a read of its stream found, a recreate Check found, or a disconnect).
func (w *Watcher) Following() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.following
}

// Disconnected says the connection the watcher reads through was lost. nats.go reconnects forever
// and keeps the watcher's subscription open through the outage, so the watcher does not end and
// nothing else would say its copy stopped following the bucket: the listener calls this from the
// bus's disconnect hook (bus.Client.AddDisconnectHook). The copy stops following, its stale episode
// begins unless one is under way, and a read under way completes nothing. The reconnect hook's
// Rewatch then reads the bucket again, and that read's completion follows it once more.
func (w *Watcher) Disconnected() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.disconnects++
	w.stopFollowing()
}

// NeedsRead reports whether the copy does not follow its bucket and no read is under way to make
// it: the current generation's scan has ended or never began, and no re-read (of a timed-out scan,
// or of a recreated bucket a read of its stream found) is armed. Such a copy is repaired by nothing
// but a Rewatch, for instance when the reconnect hook's Rewatch failed while the old watcher
// survived, so the listener's self-health check reads it as a watcher to rebuild. A stopped watcher
// needs nothing.
func (w *Watcher) NeedsRead() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return !w.stopped && !w.following && !w.reading && w.rereadTimer == nil
}

// stopFollowing records that the copy no longer follows its bucket, under mu: the read under way,
// if any, can no longer restore following, and the stale episode begins unless one already has, so
// one episode runs from the first stop to the next completed read across any number of failed
// reads.
func (w *Watcher) stopFollowing() {
	w.following = false
	w.reading = false
	w.markStale()
}

// markStale begins the stale episode unless one is under way, under mu.
func (w *Watcher) markStale() {
	if w.staleSince.IsZero() {
		w.staleSince = w.now()
	}
}

// Check reads the bucket's status through KV, the round trip a store's health probe makes,
// returning its error, and records a terminal error (Err) when the bucket's stream is not the one
// the current watcher reads: the bucket was deleted and created again while the watcher ran.
// nats.go's ordered consumer can reset onto the new stream without ending the watcher, and its
// revisions number from 1 again, so the cache would stay frozen behind a live watcher. The copy
// stops following then, and the read under way, if any, completes nothing. The recorded error
// makes the listener's self-health rebuild the watcher, and that Rewatch reads the new bucket.
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
	stream, err := bus.ReadStreamState(w.KV())
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

// WaitReady blocks until the cache is ready or ctx is done. Callers bound ctx: WatchAll on a
// healthy cluster completes in milliseconds, but a bucket can be unreachable.
func (w *Watcher) WaitReady(ctx context.Context) error {
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Stop retires the watcher for a shutdown, before the NATS drain. Its end then belongs to no live
// generation, so it records nothing and logs nothing, and every later Rewatch is a no-op: a
// reconnect hook or a self-health rebuild still running at shutdown arms no new watcher and
// reports no error. It makes no request of the server, so it returns at once against one that does
// not answer: the watcher it retires, and any a watch in flight arms after it, are kept for End, as
// are the watchers a watch replaced whose stop still waits for their scan's end (stopAfterScan).
func (w *Watcher) Stop() {
	w.mu.Lock()
	w.stopped = true
	if w.watcher != nil {
		w.retired = append(w.retired, w.watcher)
	}
	w.watcher = nil
	w.scanned = nil
	w.clearRepairArm()
	w.clearRereadArm()
	w.clearProbeArm()
	w.generation++
	w.mu.Unlock()
}

// End stops the watcher (Stop) and ends every watcher it retired, and every watcher a watch
// replaced or discarded whose stop still waits for its scan's end (stopAfterScan), while the
// connection still carries requests: the listener runs it from the bus's drain hook
// (bus.Client.AddDrainHook), before the connection's own drain. It waits, within ctx, for a watch
// still in flight to arm its watcher, then stops each one, which unsubscribes it and deletes the
// ordered consumer nats.go created for it, and waits for those deletes within ctx. Left to the
// connection's drain, that delete races the drain and is refused as "connection draining"
// (bus.Client.AddDrainHook says how). A consumer already gone (a NATS restart loses an ordered
// consumer outright, and a disconnect past its inactive threshold lets the server delete it) is
// what the delete was for, so it is logged at WARN, and so is a delete the connection was lost
// before (lostConnection): the server deletes a consumer whose interest is gone on its own. Any
// other failure, and a delete still unanswered when ctx's deadline passes, is an ERROR. A watch
// that arms its watcher after End has taken them stops that watcher itself (watch). End does its
// work once: a later End, or one beside it, waits for the first to finish, within the first's ctx,
// and ends nothing more.
//
// End stops each watcher at once, whether or not its scan has ended, and so does a watch that arms
// its watcher after End took them: the consumers must be gone before the drain, and a stop that
// waited for a scan could wait out nats.go's idle timer. So the shutdown is the one path on which
// kvwatch itself can still stop a watcher whose idle timer is about to send, and meet the nats.go
// race stopAfterScan describes.
func (w *Watcher) End(ctx context.Context) {
	w.endOnce.Do(func() { w.end(ctx) })
}

// end is End's work, run once.
func (w *Watcher) end(ctx context.Context) {
	w.Stop()
	w.mu.Lock()
	var idle chan struct{}
	if w.watching > 0 {
		idle = make(chan struct{})
		w.idle = idle
	}
	w.mu.Unlock()
	var gaveUp error
	if idle != nil {
		// A watch that has finished is read before ctx, which can already be done: a hook's
		// context ends the moment the connection is lost.
		select {
		case <-idle:
		default:
			select {
			case <-idle:
			case <-ctx.Done():
				gaveUp = context.Cause(ctx)
				w.logUnfinished(w.name+" watcher end found a watch still starting", gaveUp)
			}
		}
	}
	w.mu.Lock()
	retired := w.retired
	w.retired = nil
	// A deferred stop End takes is its to make: the wait that registered it finds it gone and
	// stops nothing, so no watcher is stopped twice.
	for stop := range w.deferred {
		retired = append(retired, stop.watcher)
	}
	w.deferred = nil
	w.ended = true
	w.endCause = gaveUp
	w.mu.Unlock()
	results := make(chan error, len(retired))
	for _, watcher := range retired {
		go func() { results <- watcher.Stop() }()
	}
	for range retired {
		select {
		case err := <-results:
			w.logEnd(err)
			continue
		default:
		}
		select {
		case err := <-results:
			w.logEnd(err)
		case <-ctx.Done():
			w.logEnd(fmt.Errorf("delete its consumer: %w", context.Cause(ctx)))
		}
	}
}

// lostConnection reports whether err says the connection was lost before a request reached the
// server or its answer came back: the drain hook's own cause (bus.ErrConnectionLost), and what
// nats.go answers a request with when its connection is closed, reconnecting, or, with the
// reconnect buffer off, refusing a publish until it reconnects.
func lostConnection(err error) bool {
	return errors.Is(err, bus.ErrConnectionLost) || errors.Is(err, nats.ErrConnectionClosed) ||
		errors.Is(err, nats.ErrConnectionReconnecting) || errors.Is(err, nats.ErrReconnectBufExceeded)
}

// lateStop is how stopping a watcher that a watch armed after End had taken the retired ones
// ended. When End stopped waiting for that watch because the connection was lost, the stop runs
// on a connection the drain has gone on without (nats.go refuses it as "connection draining"
// once the connection's drain begins, nats.go's Unsubscribe), so a failure other than the consumer
// being gone is that loss, and the consumer is left to the server as End's own are.
func lateStop(err, endCause error) error {
	if err == nil || !lostConnection(endCause) || errors.Is(err, nats.ErrConsumerNotFound) {
		return err
	}
	return fmt.Errorf("%w: %w", endCause, err)
}

// logEnd logs how stopping one retired watcher ended (End).
func (w *Watcher) logEnd(err error) {
	switch {
	case err == nil, errors.Is(err, nats.ErrBadSubscription):
		// Stopped, or its subscription had already ended.
	case errors.Is(err, nats.ErrConsumerNotFound):
		w.logger().Warn(w.name+" watcher's consumer was already gone at its end",
			slog.String("bucket", w.bucket), slog.String("error", err.Error()))
	case lostConnection(err):
		w.logger().Warn(w.name+" watcher's consumer was left to the server: the connection was lost before its delete",
			slog.String("bucket", w.bucket), slog.String("error", err.Error()))
	default:
		w.logger().Error(w.name+" watcher end failed",
			slog.String("bucket", w.bucket), slog.String("error", err.Error()))
	}
}

// logUnfinished logs what End could not wait for: at WARN when the connection was lost, since the
// server then ends what it would have, and at ERROR when End's deadline passed.
func (w *Watcher) logUnfinished(message string, cause error) {
	level := slog.LevelError
	if lostConnection(cause) {
		level = slog.LevelWarn
	}
	w.logger().Log(context.Background(), level, message, slog.String("bucket", w.bucket), slog.String("error", cause.Error()))
}

// watchDone counts a watch out (watching), and wakes End once none is left.
func (w *Watcher) watchDone() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watching--
	if w.watching == 0 && w.idle != nil {
		close(w.idle)
		w.idle = nil
	}
}

// watch arms a watcher on kv and replaces the current one, moving the handle to kv; the new watcher
// is a new read of the whole bucket. The replaced watcher is stopped, at once when its scan has
// ended and otherwise once it does (stopAfterScan), and an entry it still delivers is dropped. The
// switch to the new watcher and the read's BeginRead happen under applyMu, so no cache hook runs
// between them: an apply the replaced watcher already started finishes first, and a watch that
// switches after this one computes recreated against the new stream and applies only after this
// read began. Two Rewatches do overlap, the bus's reconnect hook and the listener's self-health
// rebuild. A watch reads its stream before it arms its watcher and takes the locks, so it can
// arrive after a newer watch has switched to a recreated bucket: a watch whose stream is older
// than a running, healthy watcher's is discarded, and that watcher stays. A watch with a nonzero
// expect, a re-read of generation expect (reread), installs only while that generation is still
// current, and is discarded the same way otherwise: whatever moved the generation began a read of
// its own. A discarded watcher is drained, and stopped once its scan ends (stopAfterScan). A watch
// that installs arms its generation's stream probe and counts the read of the stream it made as
// the latest confirmation of it (confirmedAt). After Stop it arms nothing and only moves the
// handle; a watcher it armed while Stop ran is kept for End.
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
	state, err := bus.ReadStreamState(kv)
	if err != nil {
		return err
	}
	stream := state.Created
	watcher, err := kv.WatchAll()
	if err != nil {
		return err
	}
	w.applyMu.Lock()
	w.mu.Lock()
	if w.stopped {
		// Stop ran while WatchAll was starting. Stopping this watcher here would be a server
		// request outside the drain's deadline, so it is kept for End, which runs within it; until
		// then it is read and dropped, since a drain waits for every pending message to be
		// delivered. Once End has taken the retired watchers, which it does only after this watch
		// returns or its deadline passes, nothing else would end this one, so the watch ends it.
		w.kv = kv
		ended, endCause := w.ended, w.endCause
		if !ended {
			w.retired = append(w.retired, watcher)
		}
		w.mu.Unlock()
		w.applyMu.Unlock()
		discard(watcher)
		if ended {
			w.logEnd(lateStop(watcher.Stop(), endCause))
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
		stop := w.deferStop(watcher)
		w.mu.Unlock()
		w.applyMu.Unlock()
		go w.stopAfterScan(stop, discard(watcher))
		return nil
	}
	previous, previousScanned := w.watcher, w.scanned
	recreated := !w.stream.IsZero() && !stream.Equal(w.stream)
	if recreated {
		w.epoch++
	}
	w.generation++
	generation := w.generation
	scanned := make(chan struct{})
	w.watcher = watcher
	w.scanned = scanned
	w.kv = kv
	w.stream = stream
	w.confirmedAt = sent
	w.err = nil
	w.bound = w.boundFor(state.MaxAge)
	// A new read begins: the copy stops following until it completes.
	w.stopFollowing()
	w.reading = true
	w.readDisconnects = disconnects
	// The replacement runs, so the stop it repaired needs no ERROR, and the read a timed-out scan
	// or a recreate was waiting to begin is this one. The stream probe follows the new generation.
	w.clearRepairArm()
	w.clearRereadArm()
	w.armProbe(generation)
	// A replaced watcher whose scan has not ended is stopped only once it has (stopAfterScan); its
	// consume goes on draining it until then.
	var deferred *deferredStop
	if previous != nil && !scanEnded(previousScanned) {
		deferred = w.deferStop(previous)
	}
	read := Read{Epoch: w.epoch, Recreated: recreated, MaxAge: state.MaxAge}
	w.mu.Unlock()
	w.cache.BeginRead(read)
	w.applyMu.Unlock()
	if recreated {
		w.logger().Info(w.name+" bucket was recreated; its cache is re-read from the new bucket and kept until the read completes",
			slog.String("bucket", w.bucket))
	}
	switch {
	case deferred != nil:
		go w.stopAfterScan(deferred, previousScanned)
	case previous != nil:
		// Its scan has ended, so nats.go's idle timer can no longer send (stopAfterScan). Stop
		// reports a consumer the server has lost to its caller, never at ERROR.
		_ = previous.Stop()
	}
	go w.consume(watcher, generation, read, started, scanned)
	return nil
}

// consume feeds the cache what the watcher of generation delivers, as the read read. The first nil
// ends the read's scan: a complete scan completes the read (completeRead), and one nats.go's idle
// timer gave up on arms a re-read (readAgainLater). A watcher that ends before its scan's nil
// completes nothing either: the cache removes nothing for a read it cannot vouch for. A record
// delivered after the read completed waits for a read of the bucket's stream when the latest
// confirmation of it is too old to vouch for the record (applyCurrent). It closes scanned when the
// scan ends, at the first nil or at the watcher's end before one, so a watch that replaced this
// watcher mid-scan stops it then (stopAfterScan); it drains a replaced watcher until it ends.
func (w *Watcher) consume(watcher nats.KeyWatcher, generation uint64, read Read, started time.Time, scanned chan<- struct{}) {
	var scan KeyScan
	scanning := true
	for entry := range watcher.Updates() {
		if entry == nil {
			// WatchAll emits a nil sentinel once it has delivered the current value of every key,
			// and nats.go's idle timer emits the same sentinel when it gives up on that scan. Only
			// the current watcher's completes a read, releases readiness, or logs: a replaced
			// one's scan says nothing about what the current watcher has delivered.
			if scanning {
				scanning = false
				close(scanned)
				// A scan the idle timer ended is logged and never recorded as this watcher's
				// error: the watcher is still running, so Err(), Ping(), /healthz and the
				// self-health rebuild must not read it as dead; the watcher reads the bucket again
				// itself. Reading the timeout consumes nats.go's one buffered error (idleTimeout),
				// so a watcher that later ends after a timed-out scan records "<name> watcher
				// stopped" instead of the timeout -- a non-nil error either way.
				timedOut := idleTimeout(watcher) != nil
				if w.current(generation) {
					w.logWarmUp(scan, time.Since(started), timedOut)
				}
				if timedOut {
					w.readAgainLater(generation, timedOutCause)
				} else {
					w.completeRead(generation, read)
				}
			}
			if w.current(generation) {
				w.signalReady()
			}
			continue
		}
		if scanning {
			scan.count(entry)
		}
		w.applyCurrent(entry, generation, read)
	}
	if scanning {
		close(scanned)
	}
	w.mu.Lock()
	if generation != w.generation {
		// Replaced or stopped: the end belongs to no live watcher.
		w.mu.Unlock()
		return
	}
	w.watcher = nil
	w.scanned = nil
	err := terminalError(watcher, w.name)
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

// readAgainLater ends generation's read without completing it -- its scan timed out, or the stream
// it read could not be confirmed (completeRead) -- so the cache removes nothing, and the watcher
// reads the bucket again after rereadDelay (reread) rather than wait for a Rewatch that nothing
// would start: the watcher still runs, so Err stays nil and the listener's self-health rebuild has
// nothing to rebuild. cause names the re-read in the line a failed one logs.
func (w *Watcher) readAgainLater(generation uint64, cause string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if generation != w.generation || w.stopped {
		return
	}
	w.reading = false
	w.armReread(generation, rereadDelay, cause)
}

// armReread arms the re-read of generation's bucket (reread) after delay, under mu, in place of
// any armed before. cause names it in the line a failed re-read logs.
func (w *Watcher) armReread(generation uint64, delay time.Duration, cause string) {
	w.clearRereadArm()
	w.rereadTimer = w.after(delay, func() { w.reread(generation, cause) })
}

// reread reads the bucket again for generation, whose read timed out, could not be confirmed, or
// was found to be of a bucket since recreated: only while generation is still current and the
// watcher not stopped, since whatever moved the generation began a read of its own, and through
// watch's expect, so a Rewatch that installs first wins. A re-read whose setup fails (the stream
// read or WatchAll; a recreated bucket deleted again, say) logs a WARN naming cause and is armed
// again for the same generation after rereadDelay.
func (w *Watcher) reread(generation uint64, cause string) {
	w.mu.RLock()
	due := generation == w.generation && !w.stopped
	kv := w.kv
	w.mu.RUnlock()
	if !due {
		return
	}
	err := w.watch(kv, generation)
	if err == nil {
		return
	}
	w.logger().Warn(w.name+" re-read of "+cause+" failed; it is tried again",
		slog.String("bucket", w.bucket), slog.String("error", err.Error()))
	w.mu.Lock()
	defer w.mu.Unlock()
	if generation == w.generation && !w.stopped {
		w.armReread(generation, rereadDelay, cause)
	}
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
// probe's read (probe), the gate's before a record is applied (applyCurrent), and the check before
// a read completes (completeRead). The request is sent outside the locks, and its answer compared
// under mu with the stream generation's watch installed, so an answer for a generation since
// replaced or stopped changes nothing.
//
// The same stream records when the request was sent as the latest confirmation (confirmedAt).
// Another creation time means the bucket was deleted and created again under the watcher, which
// nats.go's ordered consumer may follow without ending: the copy stops following at once, which
// starts its stale episode and takes from a read under way the right to complete, and unless a
// re-read is already due the watcher reads the new bucket itself as a re-read of generation
// (reread), whose install begins a new epoch (Read.Recreated). No stream at all means the bucket
// was deleted: the copy stops following, since it cannot follow a missing bucket, and the probe
// goes on, so the read that finds the bucket created again starts the re-read. Any other error
// changes nothing: a transport failure is Check's, Ping's and the self-health monitor's to report.
// A finding that starts a re-read logs one INFO line, as does one that stops a copy following a
// deleted bucket, so a probe that finds the same thing again logs nothing.
func (w *Watcher) confirmStream(generation uint64) streamCheck {
	w.mu.RLock()
	due := generation == w.generation && !w.stopped
	kv := w.kv
	w.mu.RUnlock()
	if !due {
		return streamMoot
	}
	sent := w.now()
	state, err := bus.ReadStreamState(kv)
	gone := errors.Is(err, nats.ErrStreamNotFound)
	if err != nil && !gone {
		return streamUnread
	}
	w.mu.Lock()
	if generation != w.generation || w.stopped {
		w.mu.Unlock()
		return streamMoot
	}
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
// while generation is still current. A probe of a generation since replaced or stopped reads
// nothing and arms nothing: the watch that replaced it armed its own.
func (w *Watcher) probe(generation uint64) {
	if w.confirmStream(generation) == streamMoot {
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

// unconfirmed reports whether a record generation's watcher delivered must wait for a read of the
// bucket's stream before it is applied: the copy follows its bucket, and the latest confirmation of
// its stream was sent longer than streamConfirmMaxAge ago, so the record may come from an ordered
// consumer nats.go moved onto a recreated bucket (streamConfirmMaxAge says why the bound is safe).
func (w *Watcher) unconfirmed(generation uint64) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return generation == w.generation && w.following && w.now().Sub(w.confirmedAt) > streamConfirmMaxAge
}

// clearRereadArm drops the armed re-read, under mu: the caller moves the generation under the same
// lock, so a callback of the wait already running reads nothing either (reread).
func (w *Watcher) clearRereadArm() {
	if w.rereadTimer == nil {
		return
	}
	w.rereadTimer.Stop()
	w.rereadTimer = nil
}

// reportFailedRepair reports the stop nobody asked for that is outstanding when a Rewatch fails, if
// one is, as repairFailedReason: the outstanding stop is the current generation's (repairTimer).
// When the generation moves between reading it here and reportUnrepaired, a repair or a Stop
// cleared that stop, and a later stop has its own wait to report it.
func (w *Watcher) reportFailedRepair() {
	w.mu.RLock()
	generation := w.generation
	w.mu.RUnlock()
	w.reportUnrepaired(repairFailedReason, generation)
}

// reportUnrepaired logs the one ERROR for the stop nobody asked for that the watcher of generation
// made, naming reason: a repair that failed, or unrepairedDelay passing with none. It returns
// silently unless that stop is still outstanding: repairTimer is set and generation is still
// current. The generation tells a wait's own stop from a later one. time.AfterFunc's callback can
// already be running when Stop returns false, so a wait a repair cleared can still call here after
// the replacement has ended on its own and armed a wait of its own; reporting then would log that
// stop moments after it happened and leave its own wait nothing to report. The same check under mu
// makes the ERROR exactly one, since a failed repair racing the wait would otherwise log twice.
func (w *Watcher) reportUnrepaired(reason string, generation uint64) {
	w.mu.Lock()
	timer := w.repairTimer
	if timer == nil || generation != w.generation {
		w.mu.Unlock()
		return
	}
	timer.Stop()
	w.repairTimer = nil
	err := w.err
	w.mu.Unlock()
	recorded := ""
	if err != nil {
		recorded = err.Error()
	}
	w.logger().Error(w.name+" watcher was not repaired",
		slog.String("bucket", w.bucket),
		slog.String("reason", reason),
		slog.String("error", recorded))
}

// clearRepairArm drops the wait a stop nobody asked for armed, so nothing reports it: the caller
// holds mu, has either repaired the watcher or retired it for a shutdown, and moves the generation
// under the same lock, so a callback of the wait already running reports nothing either
// (reportUnrepaired). The caller's shutdown adds no ERROR of its own this way
// (cmd/listener/shutdown_test.go greps for one).
func (w *Watcher) clearRepairArm() {
	if w.repairTimer == nil {
		return
	}
	w.repairTimer.Stop()
	w.repairTimer = nil
}

// logWarmUp logs the line that says what one read's scan cost: the cache, the bucket, how long it
// took, the live keys the scan delivered, the delete markers it streamed past to find them, and how
// it ended, so a restart's own log names what a slow readiness spent (LEGION-374). Every read logs
// one, so a re-read after a timed-out scan or a recreate is in the log too. The two counts are
// disjoint, as the role restore line's are: a marker is not a key the cache keeps, and one total of
// both reads as keys the warm-up failed to apply.
// entries counts what the scan delivered, which is what sizes the replay; a cache can hold fewer,
// since each one evicts a value it cannot decode. A scan the idle timer gave up on logs the same
// line at WARN, since the read behind it is incomplete and is read again.
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
// than streamConfirmMaxAge (unconfirmed) waits for a read of the stream first (confirmStream), so
// a record from a consumer nats.go moved onto a recreated bucket is applied only once the copy has
// stopped following. The probe keeps the confirmation of a healthy bucket fresh, so that read is
// rare; one that fails changes nothing, and the record is applied.
func (w *Watcher) applyCurrent(entry nats.KeyValueEntry, generation uint64, read Read) {
	if w.unconfirmed(generation) {
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

// discard reads and drops a watcher's entries until it ends. nats.go blocks a watcher whose
// 256-entry buffer is full, and Stop only unsubscribes, so a watcher nothing reads keeps its
// delivery goroutine parked for the life of the process. The channel it returns is closed once
// the watcher's scan has ended, at its first nil or at its end before one, for stopAfterScan.
func discard(watcher nats.KeyWatcher) <-chan struct{} {
	scanned := make(chan struct{})
	go func() {
		scanning := true
		for entry := range watcher.Updates() {
			if entry == nil && scanning {
				scanning = false
				close(scanned)
			}
		}
		if scanning {
			close(scanned)
		}
	}()
	return scanned
}

// deferredStop is a watcher a watch replaced or discarded before its scan ended, whose stop waits
// for that end (stopAfterScan). Its identity is the key it is registered under (deferred).
type deferredStop struct{ watcher nats.KeyWatcher }

// deferStop registers watcher's stop as deferred, under mu, so End takes it while it waits. A
// watch registers one only while the watcher is not stopped, which is before End takes them.
func (w *Watcher) deferStop(watcher nats.KeyWatcher) *deferredStop {
	stop := &deferredStop{watcher: watcher}
	if w.deferred == nil {
		w.deferred = map[*deferredStop]struct{}{}
	}
	w.deferred[stop] = struct{}{}
	return stop
}

// stopAfterScan stops the watcher of stop once scanned is closed: its scan has ended, at its first
// nil or with its updates channel closed before one. End may take the stop first, and then this
// stops nothing, so each watcher is stopped once. Its error is dropped, as a replaced watcher's
// always was: Stop reports a consumer the server has lost, or a subscription already ended, to its
// caller, and neither is worth a line.
//
// Stopping a watcher before its scan has ended can crash the process, in nats.go v1.50.0 and on
// its main branch alike. A watch created with records pending arms an idle timer (kv.go:1145-1156)
// that each scan record resets (:1097-1098) to the JetStream context's MaxWait, 10 s for a
// Rewatch's handle. When it fires it takes the watcher's lock and, unless the scan is done
// (initDone, :1148), sends ErrKeyWatcherTimeout on Error() (:1151) and the nil marker on Updates()
// (:1154). The subscription's end, which Stop's Unsubscribe brings about (:990-995), runs the
// watcher's pDone (:1164-1172): it takes the same lock, stops the timer (:1168) and closes both
// channels (:1170-1171). time.Timer's Stop does not wait for a callback already running, so a
// timer that fired just before the stop is parked on that lock, and once pDone releases it, it
// sends on a closed channel: a "send on closed channel" panic in a goroutine nats.go owns, which
// ends the listener. The refill rig met it when a reconnect's Rewatch replaced a watcher still
// scanning: the process died about 10 s after that scan's last record. nats.go marks the scan done
// under that lock before it sends the nil, both on the scan's last record (:1095-1096) and in the
// timer (:1149), so once the nil has been received the timer can no longer send; an updates
// channel closed first means pDone has already run. The wait is bounded by nats.go itself: a scan
// that stalls is ended by that same timer.
//
// This runs beside the goroutine that drains the watcher (consume, discard), never in it. nats.go's
// delivery callback holds the watcher's lock while it blocks on a full 256-entry Updates buffer
// (:1070-1082), so a stop made by the only reader would leave that buffer unread while it waits.
//
// What is left exposed is what kvwatch cannot order: nats.go ending a subscription on its own (an
// ordered consumer it could not recreate, a closed connection) runs the same pDone at any moment;
// and the shutdown stops every watcher at once, scanning or not (End, and a watch that arms its
// watcher after End took them), since their consumers must be deleted before the connection's
// drain.
func (w *Watcher) stopAfterScan(stop *deferredStop, scanned <-chan struct{}) {
	<-scanned
	w.mu.Lock()
	_, mine := w.deferred[stop]
	delete(w.deferred, stop)
	w.mu.Unlock()
	if mine {
		_ = stop.watcher.Stop()
	}
}

// scanEnded reports whether scanned, a watcher's scan signal (consume), is closed.
func scanEnded(scanned <-chan struct{}) bool {
	select {
	case <-scanned:
		return true
	default:
		return false
	}
}

func (w *Watcher) signalReady() {
	w.readyOnce.Do(func() { close(w.ready) })
}

// terminalError is the error the watcher left on its Error channel, if any (idleTimeout), and
// otherwise "<name> watcher stopped".
func terminalError(watcher nats.KeyWatcher, name string) error {
	if err := idleTimeout(watcher); err != nil {
		return err
	}
	return errors.New(name + " watcher stopped")
}
