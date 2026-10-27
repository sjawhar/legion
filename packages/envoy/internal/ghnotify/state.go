package ghnotify

import (
	"strings"
	"time"
)

const (
	// overlap is both the since overlap between two polls and the margin before a thread's high
	// water within which a comment GitHub shows late is still published.
	overlap = 5 * time.Minute
	// forgetAfter is how long a thread's state is kept after its last change. A later update of a
	// forgotten thread starts from the poll's since, so it loses no comment.
	forgetAfter = 7 * 24 * time.Hour
	// unreadableWindow is the span Health counts unreadable threads over.
	unreadableWindow = 24 * time.Hour
	// ssoRetry is how long the poller leaves an owner that requires single sign-on for the token.
	ssoRetry = time.Hour
)

// The poller keeps all its state in memory and relies on the stream's 72-hour duplicate window
// (message id DedupeKey:Topic) for a restart: a new poller reads the last Lookback again and the
// stream drops what it already holds.

// cursor is where the next list request starts: its If-Modified-Since and since.
type cursor struct {
	lastModified string
	since        time.Time
}

// threadState is what a thread has published. highWater is the latest change the thread gave;
// margin holds the source event ids published within overlap of it, each with its time, so a
// change re-read inside the margin is not published again.
type threadState struct {
	highWater  time.Time
	margin     map[string]time.Time
	lastChange time.Time
}

// deferredThread is a thread whose owner requires single sign-on. The cursor moves past it, so the
// poller keeps the notification and processes it again at retryAt, from the since it first had.
type deferredThread struct {
	n       notification
	since   time.Time
	retryAt time.Time
	first   time.Time
}

// droppedThread records the update of a thread the poller dropped, so an unchanged thread the next
// poll's overlap lists again gives no second line.
type droppedThread struct {
	updatedAt string
	at        time.Time
}

func (p *Poller) cursor() cursor {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

// threadState is a copy of one thread's state.
func (p *Poller) threadState(threadID string) (threadState, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state, ok := p.threads[threadID]
	if !ok {
		return threadState{}, false
	}
	return *state, true
}

// startState is the state a thread's processing starts from: its own, or, for a thread with none,
// a high water at the poll's since. A stored margin map is never written again, so sharing it is
// safe.
func (p *Poller) startState(threadID string, since time.Time) threadState {
	if state, ok := p.threadState(threadID); ok {
		return state
	}
	return threadState{highWater: since}
}

func (p *Poller) writeThread(threadID string, state threadState) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.threads[threadID] = &state
}

// deferThread keeps n for a retry at retryAt, from the since it was first deferred with.
func (p *Poller) deferThread(n notification, since, retryAt time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	d, ok := p.deferred[n.ID]
	if !ok {
		d = deferredThread{since: since, first: p.cfg.Now()}
	}
	d.n = n
	d.retryAt = retryAt
	p.deferred[n.ID] = d
}

// ssoActive reports whether owner's single sign-on wait is still running, and when it ends.
func (p *Poller) ssoActive(owner string, now time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.ssoUntil[strings.ToLower(owner)]
	return until, ok && now.Before(until)
}

// prune forgets what has aged out: thread states 7 days after their last change, deferred threads
// and drop records 7 days after they were written, finished single sign-on waits and unreadable
// threads older than a day.
func (p *Poller) prune(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for threadID, state := range p.threads {
		if now.Sub(state.lastChange) > forgetAfter {
			delete(p.threads, threadID)
		}
	}
	for threadID, d := range p.deferred {
		if now.Sub(d.first) > forgetAfter {
			delete(p.deferred, threadID)
		}
	}
	for threadID, dropped := range p.dropped {
		if now.Sub(dropped.at) > forgetAfter {
			delete(p.dropped, threadID)
		}
	}
	for owner, until := range p.ssoUntil {
		if !now.Before(until) {
			delete(p.ssoUntil, owner)
		}
	}
	for threadID, at := range p.unreadable {
		if now.Sub(at) > unreadableWindow {
			delete(p.unreadable, threadID)
		}
	}
}
