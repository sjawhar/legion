// legion_phase_work.go holds the issues that carry Legion phase work, the input the board's tree
// walk needs beside the daemon's own reports (dispatch://INFRA-1077, the spec's "The top issue of
// a tree"): an issue is a Legion issue when the daemon reports it or when it carries Legion phase
// work — a handoff, or an event of a session titled `Legion <role> · <that issue's key>`.
//
// The one read of that fact is legion.PhaseWorkKeys, which the metrics make too
// (legion/metrics/population.go's readPhaseWork), so both readers answer the rule from the same
// rows and no second rule states it here. A session's title is its newest, so phase work is not
// only gained: a session renamed to another issue takes the work of the issue it wrote on with
// it, and an event that carried no title of its own is read through the session's newest title
// all the same. Only that one read can say which issues carry it.
//
// The board is recomputed on every event burst of every open page, so this keeps the read's
// answer rather than making it again each time. It is filled by one read — at the live hub's
// start, and at the first board read of a process whose hub never started — and the feed the
// live hub already reacts to marks it stale, never edits it (delivery_live_hub_intake.go's
// applyEventLocked). The next board read fills it again.
//
// Only an event that can move what the read answers marks it stale, since the read is a scan of
// every project's whole event log and sessions write events all the time: a recorded handoff,
// and an event of a session the rule's title half can reach (observe). That is why the read
// answers the titles of the sessions it found working an issue beside the keys
// (legion.PhaseWork): a session no read and no event since has found to be a Legion role cannot
// take phase work from the issue it writes on, and it is the commonest writer of all.
//
// The set holds the issues' bare keys, of every project: a board walks only the chain of its own
// issues, and nothing here is scoped to one.
package api

import (
	"context"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// legionPhaseWork is every issue key that carries Legion phase work. Its zero value is a usable
// empty set no read has filled, which is what a server a test builds directly holds.
type legionPhaseWork struct {
	// reading is held across the read alone, so two board reads at start-up make one read and
	// not two. mu guards the fields and is never held across a query; the live hub's intake
	// takes it while it holds the hub's own lock, so the order is the hub's lock and then this
	// one, and nothing here takes the hub's.
	reading sync.Mutex
	mu      sync.Mutex
	// filled is whether a read ever answered this set, and dirtied counts the events that have
	// marked it stale since: a read answers the rows as they stood before it began, so one
	// marked stale while it ran is stale still.
	filled  bool
	dirtied uint64
	// read is the value of dirtied the newest read started at.
	read uint64
	keys map[string]struct{}
	// roleSessions is the per-session index the narrow invalidation stands on: the newest title
	// of every session the newest read, or an event since, found to be a Legion role working an
	// issue (legion.PhaseWork.RoleSessions). A session it does not name had no such title then,
	// so no issue holds phase work by it and none can lose any when it writes again (observe).
	roleSessions map[string]string
}

// has is whether the issue carries Legion phase work. A nil set answers false: a population built
// without one is a population of the daemon's reports alone.
func (w *legionPhaseWork) has(key string) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, held := w.keys[key]
	return held
}

// invalidate marks the set stale: an event the one read may answer differently for was written,
// so the next board read makes that read again. A recorded handoff is one such event whatever
// wrote it; an event of a session is one only where observe says so.
func (w *legionPhaseWork) invalidate() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dirtied++
}

// observe takes one event a session wrote — the session's id, that session's newest title as
// the event carries it, and the issue the event was written on — and marks the set stale only
// where the one read can answer differently for it. The three cases it marks:
//
//   - the session's newest title is a Legion role's and the set's own read did not know it as
//     one, so every issue the session has written on may carry phase work now;
//   - the title moved from the one the read knew — to another issue's, or to a title of no
//     Legion role at all — so the issues it held under the old title may hold none now;
//   - the title is the one the read knew, it names this very issue, and the set does not hold
//     that issue yet, which is the session's first event on the issue its title names.
//
// Anything else cannot move the read's answer: an ordinary session's event, and a Legion role's
// event on an issue the set already holds or that its title does not name, which is every event
// of a running phase worker after its first. Which titles are a role's at all is the one
// statement of that rule (legion.WorkerTitle), never a second one stated here.
func (w *legionPhaseWork) observe(session, title, issue string) {
	if session == "" {
		return
	}
	parsed, isRole := legion.WorkerTitle(title)
	w.mu.Lock()
	defer w.mu.Unlock()
	known, wasRole := w.roleSessions[session]
	if isRole && wasRole && known == title {
		if _, held := w.keys[issue]; held || !parsed.Works(issue) {
			return
		}
		w.dirtied++
		return
	}
	if !isRole && !wasRole {
		return
	}
	if isRole {
		if w.roleSessions == nil {
			w.roleSessions = map[string]string{}
		}
		w.roleSessions[session] = title
	} else {
		delete(w.roleSessions, session)
	}
	w.dirtied++
}

// ensure fills the set where no read has, or where an event has marked it stale since the last
// one. A read that fails leaves it as it was, so the next board read reads again.
func (w *legionPhaseWork) ensure(ctx context.Context, pool *store.Pool) error {
	if w.isCurrent() {
		return nil
	}
	w.reading.Lock()
	defer w.reading.Unlock()
	if w.isCurrent() {
		return nil
	}
	return w.fill(ctx, pool)
}

// reseed fills the set again whatever it holds: the live hub's intake marks it stale from the
// event log above the cursor it starts on, so the fill belongs beside that cursor (seedEvents),
// at the hub's start and after a reset of the event log.
func (w *legionPhaseWork) reseed(ctx context.Context, pool *store.Pool) error {
	w.reading.Lock()
	defer w.reading.Unlock()
	return w.fill(ctx, pool)
}

// isCurrent is whether a read has filled the set and nothing has marked it stale since.
func (w *legionPhaseWork) isCurrent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.filled && w.read == w.dirtied
}

// fill reads the whole set and takes it as it stands, the index of the sessions the read found
// working an issue with it. It replaces what the set held: an issue whose only phase work was a
// session since renamed carries none now, and only this read says so. An event that marked the
// set stale while the read ran leaves it stale (read below dirtied), so the index this takes is
// read again with the set it belongs to; until then it costs that session one mark too many and
// never one too few.
func (w *legionPhaseWork) fill(ctx context.Context, pool *store.Pool) error {
	w.mu.Lock()
	at := w.dirtied
	w.mu.Unlock()
	work, err := legion.PhaseWorkKeys(ctx, pool)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keys, w.roleSessions, w.filled, w.read = work.Keys, work.RoleSessions, true, at
	return nil
}
