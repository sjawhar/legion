package kvwatch

import (
	"log/slog"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
)

// rereadDelay is how long a watcher waits before it reads its bucket again after the current
// read's scan timed out, or a re-read failed to start.
const rereadDelay = 5 * time.Second

// The causes a re-read names in the line it logs when it fails to start (reread). A timed-out
// scan's wording is the refill rig's key (internal/testnats/cmd/refillrig).
const (
	timedOutCause    = "a timed-out scan"
	recreatedCause   = "a recreated bucket"
	unconfirmedCause = "a read whose stream could not be confirmed"
)

// unrepairedDelay is how long a stop nobody asked for waits for a repair before it is reported as
// an ERROR. The bus's reconnect hook rewatches within seconds of a reconnect, so a minute passes
// only when nothing is repairing the cache at all. It is bus.RepairGrace itself, so both halves of
// the same outage report at the same minute: past the grace the bus reports its own rebuild that
// has not succeeded on every attempt it makes (internal/bus/recovery.go, recoverUntilHealthy,
// whose backoff is capped at 30 s), and this wait adds the stopped watcher's line to that.
const unrepairedDelay = bus.RepairGrace

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
// read or the watcher's create; a recreated bucket deleted again, say) logs a WARN naming cause and
// is armed again for the same generation after rereadDelay.
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

// clearRereadArm drops the armed re-read, under mu: the caller moves the generation under the same
// lock, so a callback of the wait already running reads nothing either (reread).
func (w *Watcher) clearRereadArm() {
	if w.rereadTimer == nil {
		return
	}
	w.rereadTimer.Stop()
	w.rereadTimer = nil
}

// reportUnrepaired logs the one ERROR for the stop nobody asked for that the watcher of generation
// made: unrepairedDelay passed with no repair, which is the stop's one report. Its only caller is
// the wait's own callback, which runs once the wait has fired, so there is no wait left to stop
// here. It returns silently unless that stop is still outstanding: repairTimer is set and
// generation is still current. The generation tells a wait's own stop from a later one.
// time.AfterFunc's callback can already be running when Stop returns false, so a wait a repair
// cleared can still call here after the replacement has ended on its own and armed a wait of its
// own; reporting then would log that stop moments after it happened and leave its own wait
// nothing to report. Reading both under the mu that clearRepairArm and the generation move under
// is what keeps such a wait, or a later generation's, from reporting.
func (w *Watcher) reportUnrepaired(generation uint64) {
	w.mu.Lock()
	if w.repairTimer == nil || generation != w.generation {
		w.mu.Unlock()
		return
	}
	w.repairTimer = nil
	err := w.err
	w.mu.Unlock()
	recorded := ""
	if err != nil {
		recorded = err.Error()
	}
	w.logger().Error(w.name+" watcher was not repaired",
		slog.String("bucket", w.bucket),
		slog.String("reason", "no repair within "+unrepairedDelay.String()),
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
