package cistore

import (
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/logging"
)

// List returns a snapshot copy of the current cached states. Only tests list the cache: the
// summary loop reads a record at a time, through the change feed (attachSummary).
func (s *Store) List() []State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]State, 0, s.cache.Len())
	for _, state := range s.cache.All() {
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool {
		return Key(out[i].Owner, out[i].Repo, out[i].Number, out[i].SHA) <
			Key(out[j].Owner, out[j].Repo, out[j].Number, out[j].SHA)
	})
	return out
}

// cacheStateLocked puts state in the copy at revision as a delivered record does (ciCache.Apply),
// and marks the key changed for the summary loop when it moves the copy on: the benchmarks seed
// and change a cache with no KV write. Callers hold s.mu.
func (s *Store) cacheStateLocked(key string, state State, revision uint64) {
	value, err := json.Marshal(state)
	if err != nil {
		panic("cistore: encode a benchmark state: " + err.Error())
	}
	moved := s.movedLocked(0, key, revision)
	if err := s.cache.Delivered(kvwatch.Entry{Key: key, Value: value, Revision: revision}); err != nil {
		panic("cistore: deliver a benchmark state: " + err.Error())
	}
	if moved {
		s.markChangedLocked(key)
	}
}

// testSummarizers holds the summarizer testSummarizer returns for each store, attached to the
// store at its first use and forgotten when the test that opened the store ends
// (forgetTestSummarizer).
var testSummarizers = struct {
	sync.Mutex
	byStore map[*Store]*summarizer
}{byStore: map[*Store]*summarizer{}}

// runSummaryTick runs one pass at time.Now() on store's summarizer (testSummarizer) and returns how
// many records it held back (verdictHeld).
func runSummaryTick(store *Store, pub Publisher, debounce time.Duration, logger *logging.Logger) int {
	held, _, _ := testSummarizer(store, debounce).pass(pub, logger, time.Now().UnixMilli())
	return held
}

// testSummarizer returns store's summarizer, creating it and attaching it to store at the first
// call. It panics when a call passes a debounce other than the one store's first call passed, so a
// test cannot silently run with another debounce.
func testSummarizer(store *Store, debounce time.Duration) *summarizer {
	testSummarizers.Lock()
	z, ok := testSummarizers.byStore[store]
	if !ok {
		store.attachSummary()
		z = newSummarizer(store, debounce)
		testSummarizers.byStore[store] = z
	}
	testSummarizers.Unlock()
	if z.debounce != debounce {
		panic(fmt.Sprintf("testSummarizer: debounce %v on a store whose summarizer has %v", debounce, z.debounce))
	}
	return z
}

// forgetTestSummarizer drops store's summarizer from testSummarizers and detaches it from store,
// when testSummarizer attached one. openStore runs it when the test that opened store ends.
func forgetTestSummarizer(store *Store) {
	testSummarizers.Lock()
	_, attached := testSummarizers.byStore[store]
	delete(testSummarizers.byStore, store)
	testSummarizers.Unlock()
	if attached {
		store.detachSummary()
	}
}
