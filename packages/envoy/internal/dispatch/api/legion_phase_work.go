// legion_phase_work.go holds the issues that carry Legion phase work, the input the board's tree
// walk needs beside the daemon's own reports (dispatch://INFRA-1077, the spec's "The top issue of
// a tree"): an issue is a Legion issue when the daemon reports it or when it carries Legion phase
// work — a handoff, or an event of a session titled `Legion <role> · <that issue's key>`.
//
// The one read of that fact is legion.PhaseWorkKeys, which the metrics make too
// (legion/metrics/population.go's readPhaseWork), so both readers answer the rule from the same
// rows. The board is recomputed on every event burst of every open page, so making that read
// again each time is what this set exists to avoid: an issue gains phase work and never loses
// it, so a set of keys that grows is exact. It is seeded by one read — at the live hub's start,
// and at the first board read of a process whose hub never started — and grows by a key per
// event after it, from the feed the live hub already reacts to
// (delivery_live_hub_intake.go's applyEventLocked).
//
// The set holds the issues' bare keys, of every project: a board walks only the chain of its own
// issues, and nothing here is scoped to one.
package api

import (
	"context"
	"maps"
	"sync"

	"github.com/sjawhar/envoy/internal/dispatch/legion"
	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// legionPhaseWork is every issue key that carries Legion phase work. Its zero value is a usable
// empty set, which is what a server a test builds directly holds.
type legionPhaseWork struct {
	// reading is held across the seed read alone, so two board reads at start-up make one read
	// and not two. mu guards the set and is never held across a query; the live hub's intake
	// takes it while it holds the hub's own lock, so the order is the hub's lock and then this
	// one, and nothing here takes the hub's.
	reading sync.Mutex
	mu      sync.Mutex
	seeded  bool
	keys    map[string]struct{}
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

// add takes the key of an issue an event just gave phase work. An event that names no issue adds
// nothing.
func (w *legionPhaseWork) add(key string) {
	if key == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.keys == nil {
		w.keys = map[string]struct{}{}
	}
	w.keys[key] = struct{}{}
}

// ensure seeds the set once, which is what a board read in a process whose live hub never started
// stands on. A read that fails leaves the set unseeded, so the next board read reads again.
func (w *legionPhaseWork) ensure(ctx context.Context, pool *store.Pool) error {
	if w.isSeeded() {
		return nil
	}
	w.reading.Lock()
	defer w.reading.Unlock()
	if w.isSeeded() {
		return nil
	}
	return w.seed(ctx, pool)
}

// reseed reads the set again whatever it holds: the live hub's intake grows it from the event log
// above the cursor it starts on, so the seed belongs beside that cursor (seedEvents), at the
// hub's start and after a reset of the event log.
func (w *legionPhaseWork) reseed(ctx context.Context, pool *store.Pool) error {
	w.reading.Lock()
	defer w.reading.Unlock()
	return w.seed(ctx, pool)
}

func (w *legionPhaseWork) isSeeded() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.seeded
}

// seed reads the whole set and merges it into what the set already holds: phase work is never
// lost, so a key an event added while the read ran stays.
func (w *legionPhaseWork) seed(ctx context.Context, pool *store.Pool) error {
	keys, err := legion.PhaseWorkKeys(ctx, pool, metrics.RoleSessionWorks)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.keys == nil {
		w.keys = make(map[string]struct{}, len(keys))
	}
	maps.Copy(w.keys, keys)
	w.seeded = true
	return nil
}
