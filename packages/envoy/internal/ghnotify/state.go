package ghnotify

import (
	"strings"
	"time"
)

const (
	// overlap is both the since overlap between two polls and the margin before a thread's high
	// water within which a comment GitHub shows late is still published.
	overlap = 5 * time.Minute
	// forgetAfter is how long a thread's record is kept after its last change. A later update of a
	// forgotten thread starts from the poll's since, so it loses no comment.
	forgetAfter = 7 * 24 * time.Hour
	// unreadableWindow is the span Health counts unreadable threads over.
	unreadableWindow = 24 * time.Hour
	// ssoRetry is how long the poller leaves an owner that requires single sign-on for the token.
	ssoRetry = time.Hour
	// maxFailures is how many polls in a row a thread's fetch may fail transiently before the
	// poller writes it off as unreadable, so one thread GitHub keeps failing cannot hold the
	// cursor, and with it every other thread's events and the poller's health, for ever.
	maxFailures = 5
)

// The poller keeps all its state in memory and relies on the stream's 72-hour duplicate window
// (message id DedupeKey:Topic) for a restart: a new poller reads the last Lookback again and the
// stream drops what it already holds.

// cursor is where the next list request starts: its If-Modified-Since and since.
type cursor struct {
	lastModified string
	since        time.Time
}

// threadState is everything the poller remembers about one notification thread.
type threadState struct {
	// highWater is the latest change the thread published, zero before its first; margin holds
	// the source event ids published within overlap of it, each with its time, so a change re-read
	// inside the margin is not published again. A stored margin map is never written again.
	highWater time.Time
	margin    map[string]time.Time
	// lastChange is the thread's latest change the poller has seen; the record is forgotten
	// forgetAfter after it.
	lastChange time.Time
	// failures counts the polls in a row whose fetch of the thread failed transiently.
	failures int
	// settled is the notification update the poller published nothing for and will not read
	// again: the thread was dropped (a skipped owner, an unmapped subject) or written off as
	// unreadable at it. An unchanged thread the next poll's overlap lists again is neither fetched
	// nor logged again. unreadableAt is when the thread was last written off, for Health.
	settled      string
	unreadableAt time.Time
	// deferred holds a thread whose owner requires single sign-on until its retry.
	deferred *deferral
}

// deferral is a thread kept for a retry after its owner's single sign-on wait. The cursor moves
// past it, so it is processed again at retryAt from the since it was first deferred with.
type deferral struct {
	n       notification
	since   time.Time
	retryAt time.Time
}

// settledAt reports whether the thread was dropped or written off at n's update.
func (s threadState) settledAt(n notification) bool {
	return s.settled != "" && s.settled == n.UpdatedAt
}

// record is a copy of one thread's record, the zero record when the poller has none.
func (p *Poller) record(threadID string) threadState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s, ok := p.threads[threadID]; ok {
		return *s
	}
	return threadState{}
}

// update applies change to one thread's record, creating it when the poller has none, and
// returns the record as change left it.
func (p *Poller) update(threadID string, change func(*threadState)) threadState {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.threads[threadID]
	if !ok {
		s = &threadState{}
		p.threads[threadID] = s
	}
	change(s)
	return *s
}

// startHighWater is where a thread's processing reads from: its own high water, or the poll's
// since for a thread that has published nothing.
func (s threadState) startHighWater(since time.Time) time.Time {
	if s.highWater.IsZero() {
		return since
	}
	return s.highWater
}

func (p *Poller) cursor() cursor {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

// ssoActive reports whether owner's single sign-on wait is still running, and when it ends.
func (p *Poller) ssoActive(owner string, now time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.ssoUntil[strings.ToLower(owner)]
	return until, ok && now.Before(until)
}

// prune forgets a thread forgetAfter after its last change, and a single sign-on wait once it
// ends. Health bounds the unreadable count by its own window, so a record kept longer changes no
// answer.
func (p *Poller) prune(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for threadID, s := range p.threads {
		if now.Sub(s.lastChange) > forgetAfter {
			delete(p.threads, threadID)
		}
	}
	for owner, until := range p.ssoUntil {
		if !now.Before(until) {
			delete(p.ssoUntil, owner)
		}
	}
}
