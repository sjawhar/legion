package cistore

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/logging"
)

// List returns a snapshot copy of the current cached states.
func (s *Store) List() []State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]State, 0, len(s.cache))
	for _, state := range s.cache {
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool {
		return Key(out[i].Owner, out[i].Repo, out[i].Number, out[i].SHA) <
			Key(out[j].Owner, out[j].Repo, out[j].Number, out[j].SHA)
	})
	return out
}

// testSummarizers holds the summarizer runSummaryTick runs on for each store, attached to the
// store at its first use and forgotten when the test that opened the store ends
// (forgetTestSummarizer).
var testSummarizers = struct {
	sync.Mutex
	byStore map[*Store]*summarizer
}{byStore: map[*Store]*summarizer{}}

// runSummaryTick runs one pass at time.Now() on store's summarizer and returns how many records it
// held back (verdictHeld). It panics when a call passes a debounce other than the one store's first
// call passed, so a test cannot silently run with another debounce.
func runSummaryTick(store *Store, pub Publisher, debounce time.Duration, logger *logging.Logger) int {
	testSummarizers.Lock()
	z, ok := testSummarizers.byStore[store]
	if !ok {
		store.attachSummary()
		z = newSummarizer(store, debounce)
		testSummarizers.byStore[store] = z
	}
	testSummarizers.Unlock()
	if z.debounce != debounce {
		panic(fmt.Sprintf("runSummaryTick: debounce %v on a store whose summarizer has %v", debounce, z.debounce))
	}
	held, _, _ := z.pass(pub, logger, time.Now().UnixMilli())
	return held
}

// forgetTestSummarizer drops store's summarizer from testSummarizers and detaches it from store,
// when runSummaryTick attached one. openStore runs it when the test that opened store ends.
func forgetTestSummarizer(store *Store) {
	testSummarizers.Lock()
	_, attached := testSummarizers.byStore[store]
	delete(testSummarizers.byStore, store)
	testSummarizers.Unlock()
	if attached {
		store.detachSummary()
	}
}
