// Package kvwatch keeps an in-memory cache fed by one JetStream KV bucket's watcher, and owns that
// cache's whole watch lifecycle: the first start, readiness, whether the current watcher's first
// scan delivered every key (Complete), moving to a replacement connection, telling the current
// watcher from one it replaced, recording the terminal error when the current watcher ends on its
// own, emptying the cache when the bucket was recreated, and arming nothing once stopped. The
// interest registry (its interest and role claim views), the session registry and the CI store
// keep one per cache, so the listener rewatches, health-checks and stops them all the same way. It
// also owns the one-shot scan of a bucket's existing keys (ScanExistingKeys), and the one reading
// of how nats.go ended a scan that both share (idleTimeout).
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

// Watcher is one cache's KV watcher, from New.
type Watcher struct {
	name   string
	bucket string
	apply  func(nats.KeyValueEntry)
	reset  func()
	log    *slog.Logger
	// rescan is set by WithRescan.
	rescan bool

	ready     chan struct{}
	readyOnce sync.Once

	// applyMu serializes apply and reset, and lets watch wait out an entry already being applied
	// by the watcher it replaces. It is taken before mu.
	applyMu sync.Mutex

	// first is the handle Start watches.
	first bus.KeyValue

	// after arms the wait between a stop nobody asked for and the ERROR that says no repair came,
	// time.AfterFunc unless WithAfterFunc replaced it, so a test fires that wait itself.
	after AfterFunc

	mu sync.RWMutex
	// kv is the handle the store reads and writes through: first, until a watch moves it together
	// with the watcher, so the store writes through the connection its cache reads.
	kv         bus.KeyValue
	watcher    nats.KeyWatcher
	generation uint64
	stopped    bool
	err        error
	// complete is set once the current generation's first scan delivered every key's current
	// value, and cleared whenever the generation moves or the current watcher ends (Complete).
	complete bool
	// repairTimer is the armed wait left by a stop nobody asked for, and is the marker that such a
	// stop is outstanding and not yet reported: a repair that worked, and an ordered Stop, clear
	// it, and the one report clears it too, so exactly one ERROR follows one stop. It always
	// belongs to the current generation: a stop arms it only while its generation is current, and
	// the two places the generation moves (watch, Stop) clear it. Its callback carries the
	// generation it was armed for, so one still running after a repair cleared it reports nothing
	// (reportUnrepaired).
	repairTimer Timer
	// stream is when the bucket's stream the current watcher reads was created. A different time
	// on the next watch means the bucket was deleted and created again.
	stream time.Time

	// watching counts the watches in flight, so End can wait for one that may still arm a watcher.
	// A watch joins it under mu, and only while the watcher is not stopped, so once Stop has run it
	// only falls. idle, set by End while it waits, is closed when it falls to 0.
	watching int
	idle     chan struct{}
	// retired are the watchers Stop took out of service and those a watch armed after it, which
	// End ends. ended is set once End has taken them, and endCause, with it, to why End stopped
	// waiting for a watch still starting, when it did.
	retired  []nats.KeyWatcher
	ended    bool
	endCause error
	// endOnce runs End's work once: a later End waits for it to finish.
	endOnce sync.Once
}

// Option configures a Watcher.
type Option func(*Watcher)

// WithLogger sets the logger the watcher writes its lines through: its warm-up, a first start that
// failed, a recreated bucket and its own end. The listener passes its own, so they are JSON records
// with machine_id, as internal/store's are (store.WithLogger says why the listener never sets the
// default instead). Without it the watcher logs where slog.Default() points.
func WithLogger(log *slog.Logger) Option {
	return func(w *Watcher) { w.log = log }
}

// Timer is a pending wait an AfterFunc armed, which a repair that worked stops. *time.Timer is one.
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

// WithAfterFunc sets how the watcher arms its unrepairedDelay wait, time.AfterFunc by default. The
// production wait is a minute, so a test hands in its own, keeps the callback and fires it itself
// rather than sleeping for it.
func WithAfterFunc(after AfterFunc) Option {
	return func(w *Watcher) { w.after = after }
}

// rescanDelay is how long a watcher WithRescan made waits, after its first scan timed out or a
// rescan failed to start, before it scans the bucket again.
const rescanDelay = time.Second

// WithRescan makes the watcher scan the bucket again whenever its first scan does not complete:
// when nats.go's idle timer ends that scan, the watcher replaces itself, after rescanDelay, with
// one on the same handle (KV), whose own first scan delivers every key again. It goes on doing so
// until a scan completes, or a Rewatch, a Stop or the watcher's own end takes the generation out of
// its hands. Without it a timed-out scan leaves Complete false until the next Rewatch. A cache
// that answers only from a complete view (Complete) needs it, or nothing would end that wait.
func WithRescan() Option {
	return func(w *Watcher) { w.rescan = true }
}

// New returns the watcher for the bucket kv is a handle on. name labels its log lines and its
// terminal error ("<name> watcher stopped"). apply takes each entry the current watcher delivers,
// one at a time; an entry from a watcher already replaced is dropped rather than applied. reset
// empties the cache and its revision fence: a recreated bucket numbers its revisions from 1
// again, so the old fence would drop every entry the new bucket delivers. New watches nothing
// until Start. kv is a bus.KeyValue, the handle bus.EnsureKeyValue opens, and each Rewatch opens its
// replacement with bus.OpenKeyValue, so every handle the store reads and writes through checks its
// keys.
func New(name string, kv bus.KeyValue, apply func(nats.KeyValueEntry), reset func(), options ...Option) *Watcher {
	w := &Watcher{
		name:   name,
		bucket: kv.Bucket(),
		apply:  apply,
		reset:  reset,
		ready:  make(chan struct{}),
		first:  kv,
		kv:     kv,
		after:  func(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) },
	}
	for _, option := range options {
		option(w)
	}
	return w
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
// releases readiness, so a caller waiting for the cache sees the empty cache rather than hanging.
// When a reconnect hook's Rewatch has armed one meanwhile, it does neither: the error is not that
// watcher's, and that watcher releases readiness once it has delivered every existing key.
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
// recorded terminal error once the replacement has started. The handle (KV) moves with it, so the
// store writes through the connection its cache reads. After a server restart conn is the
// connection the store opened on, reconnected in place; when the bus replaces a closed connection,
// conn is the new one and the cache moves to it. A failure leaves the watcher and the handle as
// they were. After Stop it arms nothing and still moves the handle. A failure repairs nothing, so
// when a stop nobody asked for is still outstanding it reports that stop as unrepaired
// (reportFailedRepair) before returning the error unchanged, which its caller already logs.
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

// Check reads the bucket's status through KV, the round trip a store's health probe makes,
// returning its error, and records a terminal error (Err) when the bucket's stream is not the one
// the current watcher reads: the bucket was deleted and created again while the watcher ran.
// nats.go's ordered consumer can reset onto the new stream without ending the watcher, and its
// revisions number from 1 again, so the cache would stay frozen behind a live watcher. The
// recorded error makes the listener's self-health rebuild the watcher, and that Rewatch resets the
// cache.
//
// The stream is known by its creation time, and one path moves that time with no recreate.
// nats-server before v2.14.6 re-stamps a recovered stream's creation time in its file store, and
// an in-place config update persists the re-stamped time (nats-io/nats-server#8471, fixed in
// v2.14.6 and v2.15.0). So a stream config update after a server restart, followed by another
// restart, reads as a recreate. The reconnect hook's Rewatch after that restart usually meets it
// first: it resets and refills the cache from the same bucket and adopts the new time, with no
// 503. When Check meets it first, /healthz answers 503 until the next self-health rebuild does
// the same. Either way the cost is one refill, and Check is quiet from then on.
func (w *Watcher) Check() error {
	stream, err := bus.ReadStreamState(w.KV())
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.stopped && w.err == nil && !w.stream.IsZero() && !stream.Created.Equal(w.stream) {
		w.err = fmt.Errorf("%s bucket %s was recreated under its watcher", w.name, w.bucket)
	}
	return nil
}

// Err is the current watcher's terminal error, or nil while it runs.
func (w *Watcher) Err() error {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.err
}

// Ready reports whether the cache is ready: the current watcher delivered every key's current
// value or ended on its own, or the first start failed with no watcher current.
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

// Complete reports whether the current watcher is running and its first scan delivered every
// key's current value: its nil marker arrived, and was not nats.go's idle timer's. Unlike Ready it
// is not one-way: a new generation (a Rewatch, a WithRescan rescan, a recreated bucket's reset)
// clears it until that watcher's own scan completes, and so do the watcher's end and Stop. It says
// nothing about Err, which a caller reads too. generation names the watcher the answer is for: a
// reset happens only under a new generation, so a caller that reads the cache between two calls
// read a complete cache when the first answered complete and both name the same generation.
func (w *Watcher) Complete() (generation uint64, complete bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.generation, w.complete
}

// Stop retires the watcher for a shutdown, before the NATS drain. Its end then belongs to no live
// generation, so it records nothing and logs nothing, and every later Rewatch is a no-op: a
// reconnect hook or a self-health rebuild still running at shutdown arms no new watcher and
// reports no error. It makes no request of the server, so it returns at once against one that does
// not answer: the watcher it retires, and any a watch in flight arms after it, are kept for End.
func (w *Watcher) Stop() {
	w.mu.Lock()
	w.stopped = true
	if w.watcher != nil {
		w.retired = append(w.retired, w.watcher)
	}
	w.watcher = nil
	w.clearRepairArm()
	w.generation++
	w.complete = false
	w.mu.Unlock()
}

// End stops the watcher (Stop) and ends every watcher it retired, while the connection still
// carries requests: the listener runs it from the bus's drain hook (bus.Client.AddDrainHook),
// before the connection's own drain. It waits, within ctx, for a watch still in flight to arm its
// watcher, then stops each one, which unsubscribes it and deletes the ordered consumer nats.go
// created for it, and waits for those deletes within ctx. Left to the connection's drain, that
// delete races the drain and is refused as "connection draining" (bus.Client.AddDrainHook says
// how). A consumer already gone (a NATS restart loses an ordered consumer outright, and a
// disconnect past its inactive threshold lets the server delete it) is what the delete was for,
// so it is logged at WARN, and so is a delete the connection was lost before (lostConnection):
// the server deletes a consumer whose interest is gone on its own. Any other failure, and a
// delete still unanswered when ctx's deadline passes, is an ERROR. A watch that arms its watcher
// after End has taken them stops that watcher itself (watch). End does its work once: a later
// End, or one beside it, waits for the first to finish, within the first's ctx, and ends nothing
// more.
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

// watch arms a watcher on kv and replaces the current one, moving the handle to kv. The replaced
// watcher is stopped, and an entry it still delivers is dropped. The switch to the new watcher
// and, for a recreated bucket, the cache reset happen under applyMu, so no apply runs between
// them: an apply the replaced watcher already started finishes first, and a watch that switches
// after this one computes recreated against the new stream and applies only after the reset. Two
// Rewatches do overlap, the bus's reconnect hook and the listener's self-health rebuild. A watch
// reads its stream before it arms its watcher and takes the locks, so it can arrive after a
// newer watch has switched to a recreated bucket: a watch whose stream is older than a running,
// healthy watcher's is discarded, and that watcher stays. A rescan (WithRescan) passes replacing,
// the generation it replaces, and installs nothing once another watch or a Stop has moved the
// generation past it: the current watcher is then newer than the scan it repeats, and so is its
// handle. Every other watch passes 0. After Stop it arms nothing and only moves the handle; a
// watcher it armed while Stop ran is kept for End.
func (w *Watcher) watch(kv bus.KeyValue, replacing uint64) error {
	w.mu.Lock()
	stopped := w.stopped
	if stopped {
		w.kv = kv
	} else {
		w.watching++
	}
	w.mu.Unlock()
	if stopped {
		return nil
	}
	defer w.watchDone()
	// The warm-up's cost as the caller waiting on readiness sees it: the stream read and the
	// watcher's create request are part of it, not just the entries that follow.
	started := time.Now()
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
	if replacing != 0 && w.generation != replacing {
		w.mu.Unlock()
		w.applyMu.Unlock()
		discard(watcher)
		_ = watcher.Stop()
		return nil
	}
	if w.err == nil && stream.Before(w.stream) {
		// A newer watch already switched to a recreated bucket and its watcher is running. This
		// watcher may be on the old stream, so installing it would reset the cache the current
		// watcher filled and feed it the old bucket's keys. The rule holds only against a live,
		// healthy watcher (an installed stream with no recorded error has one running): creation
		// times do not always grow (a JetStream restore keeps the snapshot's, and a recreate can
		// follow a clock step back), so when the current watcher has ended or Check has flagged
		// its bucket, the watch installs and resets as usual, and if it did read the old stream
		// the next Check flags that too.
		w.mu.Unlock()
		w.applyMu.Unlock()
		discard(watcher)
		_ = watcher.Stop()
		return nil
	}
	previous := w.watcher
	recreated := !w.stream.IsZero() && !stream.Equal(w.stream)
	w.generation++
	generation := w.generation
	w.watcher = watcher
	w.kv = kv
	w.stream = stream
	w.err = nil
	w.complete = false
	// The replacement runs, so the stop it repaired needs no ERROR.
	w.clearRepairArm()
	w.mu.Unlock()
	if recreated {
		w.reset()
	}
	w.applyMu.Unlock()
	if recreated {
		w.logger().Info(w.name+" bucket was recreated; its cache is refilled from the new bucket",
			slog.String("bucket", w.bucket))
	}
	if previous != nil {
		// Stop reports a consumer the server has lost to its caller, never at ERROR.
		_ = previous.Stop()
	}
	go w.consume(watcher, generation, started)
	return nil
}

func (w *Watcher) consume(watcher nats.KeyWatcher, generation uint64, started time.Time) {
	var scan KeyScan
	scanning := true
	for entry := range watcher.Updates() {
		if entry == nil {
			// WatchAll emits a nil sentinel once it has delivered the current value of every key,
			// and nats.go's idle timer emits the same sentinel when it gives up on that scan. Only
			// the current watcher's releases readiness, or logs: a replaced one's scan says nothing
			// about what the current watcher has delivered.
			current := w.current(generation)
			if scanning {
				scanning = false
				// A scan the idle timer ended is logged and never recorded as this watcher's
				// error: the watcher is still running and its cache still follows the bucket, so
				// Err(), Ping(), /healthz and the self-health rebuild must not read it as dead.
				// Reading the timeout consumes nats.go's one buffered error (idleTimeout), so a
				// watcher that later ends after a timed-out scan records "<name> watcher stopped"
				// instead of the timeout -- a non-nil error either way.
				timedOut := idleTimeout(watcher) != nil
				if current {
					w.logWarmUp(scan, time.Since(started), timedOut)
					w.scanEnded(generation, timedOut)
				}
			}
			if current {
				w.signalReady()
			}
			continue
		}
		if scanning {
			scan.count(entry)
		}
		w.applyCurrent(entry, generation)
	}
	w.mu.Lock()
	if generation != w.generation {
		// Replaced or stopped: the end belongs to no live watcher.
		w.mu.Unlock()
		return
	}
	w.watcher = nil
	w.complete = false
	err := terminalError(watcher, w.name)
	w.err = err
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

// scanEnded records how the first scan of generation's watcher ended. One that delivered every key
// makes the cache complete while that watcher stays current (Complete): consume applied every entry
// the scan delivered before it read the marker, and the generation is checked under the lock that
// moves it. One the idle timer ended leaves it incomplete and, WithRescan, arms the rescan.
func (w *Watcher) scanEnded(generation uint64, timedOut bool) {
	w.mu.Lock()
	current := generation == w.generation
	if current && !timedOut {
		w.complete = true
	}
	w.mu.Unlock()
	if current && timedOut && w.rescan {
		w.armRescan(generation)
	}
}

// armRescan scans the bucket again after rescanDelay, replacing generation's watcher (rescanFrom).
func (w *Watcher) armRescan(generation uint64) {
	time.AfterFunc(rescanDelay, func() { w.rescanFrom(generation) })
}

// rescanFrom replaces generation's watcher, whose first scan timed out, with a new one on the same
// handle, so the new watcher's first scan delivers every key again. It does nothing once that
// watcher is no longer current and running: a Rewatch or a Stop replaced it, or it ended on its own
// and its repair is the listener's (Err). A rescan that fails to start is logged at WARN and tried
// again after rescanDelay, under the same conditions.
func (w *Watcher) rescanFrom(generation uint64) {
	w.mu.RLock()
	due := !w.stopped && w.generation == generation && w.watcher != nil
	kv := w.kv
	w.mu.RUnlock()
	if !due {
		return
	}
	if err := w.watch(kv, generation); err != nil {
		w.logger().Warn(w.name+" rescan failed; it is tried again",
			slog.String("bucket", w.bucket),
			slog.String("error", err.Error()))
		w.armRescan(generation)
	}
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

// logWarmUp logs the line that says what one initial scan cost: the cache, the bucket, how long it
// took, the live keys the scan delivered, the delete markers it streamed past to find them, and how
// it ended, so a restart's own log names what a slow readiness spent (LEGION-374). The two counts
// are disjoint, as the role restore line's are: a marker is not a key the cache keeps, and one
// total of both reads as keys the warm-up failed to apply.
// entries counts what the scan delivered, which is what sizes the replay; a cache can hold fewer,
// since each one evicts a value it cannot decode. A scan the idle timer gave up on logs the same
// line at WARN, since the cache behind it is short of the bucket.
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

// applyCurrent applies entry only while generation is still the current watcher's.
func (w *Watcher) applyCurrent(entry nats.KeyValueEntry, generation uint64) {
	w.applyMu.Lock()
	defer w.applyMu.Unlock()
	if w.current(generation) {
		w.apply(entry)
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
// delivery goroutine parked for the life of the process.
func discard(watcher nats.KeyWatcher) {
	go func() {
		for range watcher.Updates() {
		}
	}()
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
