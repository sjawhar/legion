package docs

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync/atomic"
	"time"
)

// headTrustWindow is how long after its LISTEN's connection last delivered anything the tracker
// still trusts itself. While some process holds something the trust serves, the longest gap
// between deliveries on a healthy connection is one holder's quiet window - headPingAfter plus a
// jitter under headPingAfter, so under 2 s - plus its ping's delivery (pingWhileQuiet), which is
// inside this window; and it is short enough that a half-open connection stops being trusted well
// inside the notification delay a reader is promised.
const headTrustWindow = 3 * time.Second

// headPingAfter is the least a process's LISTEN may deliver nothing before its liveness pings, and
// the most a ping may take (pingWhileQuiet): each process's quiet window is headPingAfter plus a
// jitter drawn uniformly from [0, headPingAfter) afresh after each delivery and each ping
// (drawQuietWindow), so the window runs from one to two seconds and a ping a process sends
// arrives well inside headTrustWindow.
const headPingAfter = time.Second

// documentPingChannel carries the liveness ping, whose payload is the id of the process that sent
// it (Service.ProcessID) and whose only use is to arrive: every stale-room evictor listens on it on
// its LISTEN connection, and a delivery of it, like any other, keeps that process's head tracker
// trusting itself (headTracker.delivered, headLiveness.pingWhileQuiet).
const documentPingChannel = "dispatch_doc_ping"

// notifyDocumentPingStatement sends one ping on documentPingChannel; the process id is the
// parameter.
const notifyDocumentPingStatement = `select pg_notify('` + documentPingChannel + `', $1)`

// headLiveness is what keeps the head tracker's trust in the stale-room evictor's LISTEN
// connection honest. The evictor forwards what its listener reports - the LISTEN standing
// (connected), the connection lost (disconnected), and each notification on any channel
// (delivered) - and runs pingWhileQuiet beside its listener; headLiveness tells the tracker the
// same three things, and keeps an idle healthy connection delivering with a ping of its own.
type headLiveness struct {
	service *Service
	// quietWindow is how long, in nanoseconds, the LISTEN may deliver nothing before this
	// process pings: headPingAfter plus a jitter drawn afresh after each delivery, each LISTEN
	// and each ping (drawQuietWindow). The listener's goroutine draws it, and so does the ping's,
	// which reads it.
	quietWindow atomic.Int64
}

func newHeadLiveness(service *Service) *headLiveness {
	l := &headLiveness{service: service}
	l.drawQuietWindow()
	return l
}

// connected is the LISTEN standing: the head tracker's new epoch (headTracker.connected) and a
// fresh quiet window, the LISTEN being the connection's first delivery.
func (l *headLiveness) connected() {
	l.service.heads.connected()
	l.drawQuietWindow()
}

// disconnected is the LISTEN lost, or about to be dialled again, which the listener does only once
// the connection it had has failed: the head tracker trusts nothing until the next connected
// (headTracker.disconnected).
func (l *headLiveness) disconnected() {
	l.service.heads.disconnected()
}

// delivered is a notification the LISTEN's connection carried, on any channel, the ping
// included: a delivery the head tracker counts (headTracker.delivered) and a fresh quiet window.
func (l *headLiveness) delivered() {
	l.service.heads.delivered()
	l.drawQuietWindow()
}

// pingWhileQuiet sends a ping on documentPingChannel (notifyDocumentPingStatement), until ctx
// ends, whenever the LISTEN stands, it has delivered nothing for this process's quiet window
// (quietWindow), and this process holds something the head tracker's trust serves
// (Service.holdsWhatTheTrackerProtects). The ping arrives on every process's evictor, this one's
// included, so a healthy idle connection goes on delivering and its tracker trusting itself
// (headTracker.delivered), while a half-open one delivers nothing and its tracker stops within
// headTrustWindow. A process that holds nothing the trust serves sends none, and still counts
// what it hears as delivery.
//
// Every delivery draws the window afresh, on every process that hears it, and so does each ping
// its sender, so processes that heard the same ping or notice do not ping together: the first
// holder whose window ends pings, and its ping resets every other window. A deployment therefore
// sends about one ping a second - one per headPingAfter plus the least of its holders' jitters -
// while some process holds a room, a cached read or a preload and no document write is announced,
// and none otherwise. While any process holds something, the longest gap between deliveries is a
// holder's window, under two seconds, plus the ping's own delivery: inside headTrustWindow.
//
// The ping goes through a connection of the shared pool, never the LISTEN connection, which would
// prove only itself, and waits for it at most headPingAfter: a pool too busy to lend a connection
// that soon skips the ping, and if it stays that busy the tracker's trust lapses and cold reads
// and preloads read the store, which is the answer a saturated pool costs and nothing worse.
// After a ping, and while the LISTEN is down or the process holds nothing, it looks again
// headPingAfter later.
func (l *headLiveness) pingWhileQuiet(ctx context.Context) {
	for {
		wait := headPingAfter
		window := time.Duration(l.quietWindow.Load())
		if quiet, listening := l.service.heads.quiet(); listening && quiet < window {
			wait = window - quiet
		} else if listening && l.service.holdsWhatTheTrackerProtects() {
			l.ping(ctx)
			l.drawQuietWindow()
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// drawQuietWindow draws this process's quiet window afresh (quietWindow): headPingAfter plus a
// jitter uniform in [0, headPingAfter), from math/rand/v2's source, which each process seeds on
// its own.
func (l *headLiveness) drawQuietWindow() {
	l.quietWindow.Store(int64(headPingAfter + rand.N(headPingAfter)))
}

// ping sends one documentPingChannel notification, bounded at headPingAfter. A ping that fails
// is only a ping skipped (pingWhileQuiet), so it is logged at debug level: a saturated pool fails
// one a window.
func (l *headLiveness) ping(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, headPingAfter)
	defer cancel()
	if _, err := l.service.store.Pool.Exec(pingCtx, notifyDocumentPingStatement, l.service.processID); err != nil && ctx.Err() == nil {
		slog.Debug("dispatch: ping the document listener", "error", err)
	}
}

// holdsWhatTheTrackerProtects is whether this process holds anything the head tracker's trust
// answers without the database: a resident room, a cold read's cached rendering, or a cold
// socket's preload. Tracker entries are not counted: beside those holders the tracker keeps one
// only for a cold read in flight, which ends as a cached rendering or as nothing, and for a write
// of this process, during which nothing about the document is trusted (headTracker). Each is a
// counter or a map's length under its lock, and the preloads a sync.Map whose first entry ends the
// look, so the ping asks it every window without walking the rooms or reading the store.
func (s *Service) holdsWhatTheTrackerProtects() bool {
	if s.residentRooms.Load() > 0 || s.reads.figures().Entries > 0 {
		return true
	}
	held := false
	s.preloads.Range(func(_, _ any) bool {
		held = true
		return false
	})
	return held
}
