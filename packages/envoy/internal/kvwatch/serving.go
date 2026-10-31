package kvwatch

import (
	"errors"
	"time"
)

// ErrRefilling is what Servable answers while the copy cannot be served: no read has completed
// yet, or the copy has not followed the bucket for longer than the stale bound.
var ErrRefilling = errors.New("kvwatch: the cache has no complete read of its bucket recent enough to serve")

// defaultStaleBound is how long a copy that stopped following its bucket is still served. It is
// under half the 5-minute record lifetime of the session bucket, so a copy past it would mostly
// list sessions that already ended; within it the reconnect hook and the listener's 30 s
// self-health rebuild normally complete a read.
const defaultStaleBound = 2 * time.Minute

// boundFor is the stale bound of a stream whose MaxAge is maxAge: staleBound, or half the MaxAge
// when that is shorter, so a copy is never served once most of its records would have expired.
func (w *Watcher) boundFor(maxAge time.Duration) time.Duration {
	if maxAge > 0 {
		return min(w.staleBound, maxAge/2)
	}
	return w.staleBound
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
