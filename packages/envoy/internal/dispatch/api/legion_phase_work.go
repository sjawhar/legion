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
// live hub already reacts to marks it stale, never edits it: a handoff, or any event of a
// session, can move what the read answers (delivery_live_hub_intake.go's applyEventLocked). The
// next board read fills it again.
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
// so the next board read makes that read again.
func (w *legionPhaseWork) invalidate() {
	w.mu.Lock()
	defer w.mu.Unlock()
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

// fill reads the whole set and takes it as it stands. It replaces what the set held: an issue
// whose only phase work was a session since renamed carries none now, and only this read says so.
func (w *legionPhaseWork) fill(ctx context.Context, pool *store.Pool) error {
	w.mu.Lock()
	at := w.dirtied
	w.mu.Unlock()
	keys, err := legion.PhaseWorkKeys(ctx, pool)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.keys, w.filled, w.read = keys, true, at
	return nil
}
