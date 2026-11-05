package cistore

import (
	"fmt"
	"io"
	"strconv"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/logging"
)

// benchDebounce keeps every waiting record of the benchmarks waiting: their last event is now, an
// hour short of due, so no tick claims one and no timed tick touches KV.
const benchDebounce = time.Hour

// benchSizes are the cache sizes each summary benchmark runs at.
var benchSizes = [...]int{12000, 30000}

// benchCheckNames are the five checks every benchmark record holds.
var benchCheckNames = [...]string{"build", "test", "lint", "typecheck", "e2e"}

// benchRepos spreads the benchmark records over repositories, two of them named with a dot so
// their keys go through keyCleaner.
var benchRepos = [...]string{"widgets", "gadgets", ".github", "sprockets", "widgets.v2", "cogs", "levers", "pulleys"}

// benchState is record i of a cache of production shape: five completed, successful checks with
// run ids and a schema, settled, or, when waiting is set, unsettled with its last event at
// lastEventAt.
func benchState(i int, waiting bool, lastEventAt int64) State {
	st := State{
		Owner:       "example-org",
		Repo:        benchRepos[(i/7)%len(benchRepos)],
		Number:      strconv.Itoa(1000 + i/3),
		SHA:         fmt.Sprintf("%040x", i+1),
		Checks:      make(map[string]Check, len(benchCheckNames)),
		Suites:      map[string]Suite{},
		LastEventAt: lastEventAt,
		Generation:  1,
		Schema:      recordSchema,
	}
	for c, name := range benchCheckNames {
		id := uint64(i)*uint64(len(benchCheckNames)) + uint64(c) + 1
		st.Checks[name] = Check{
			Name:       name,
			CheckRunID: checkRunID(id),
			URL:        "https://github.com/example-org/" + st.Repo + "/runs/" + strconv.FormatUint(id, 10),
			Status:     "completed",
			Conclusion: "success",
			ObservedAt: "2026-10-27T00:00:00Z",
		}
	}
	if !waiting {
		st.SettledEmitted = true
		st.EmittedCount = 1
	}
	return st
}

// benchWaiting reports whether record i is one of the 1% that wait out their debounce.
func benchWaiting(i int) bool {
	return i%100 == 0
}

// benchCache is a store whose cache holds a benchmark's records, and the next revision a change
// to it takes.
type benchCache struct {
	store    *Store
	revision uint64
	changes  int
}

// openBenchCaches opens one store for each of benchSizes on conn and puts that many records
// straight into its cache, at revisions 1 to n, with no KV write. The stores are opened here, before
// any sub-benchmark starts, so their log lines do not split a sub-benchmark's result line.
func openBenchCaches(b *testing.B, conn *natsgo.Conn) []*benchCache {
	b.Helper()
	now := time.Now().UnixMilli()
	caches := make([]*benchCache, len(benchSizes))
	for c, n := range benchSizes {
		s := openStore(b, conn)
		s.mu.Lock()
		for i := range n {
			st := benchState(i, benchWaiting(i), now)
			s.cacheStateLocked(Key(st.Owner, st.Repo, st.Number, st.SHA), st, uint64(i+1))
		}
		s.mu.Unlock()
		caches[c] = &benchCache{store: s, revision: uint64(n + 1)}
	}
	return caches
}

// BenchmarkSummaryIdle times one summary tick over a cache in which nothing changed and nothing is
// due.
func BenchmarkSummaryIdle(b *testing.B) {
	conn, cleanup := connectNATS(b)
	defer cleanup()
	caches := openBenchCaches(b, conn)
	logger := logging.NewWithWriter("bench", io.Discard)
	for c, n := range benchSizes {
		cache := caches[c]
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			pub := &recPub{}
			runSummaryTick(cache.store, pub, benchDebounce, logger)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				runSummaryTick(cache.store, pub, benchDebounce, logger)
			}
			b.StopTimer()
			if got := pub.count(); got != 0 {
				b.Fatalf("the ticks published %d settlements, want none", got)
			}
		})
	}
}

// BenchmarkSummaryChange times one record's change followed by one summary tick: the record turns
// from settled to waiting, or back, at a higher revision each time, and nothing is due.
func BenchmarkSummaryChange(b *testing.B) {
	conn, cleanup := connectNATS(b)
	defer cleanup()
	caches := openBenchCaches(b, conn)
	logger := logging.NewWithWriter("bench", io.Discard)
	// Record 1 starts settled (benchWaiting), so its first change makes it wait.
	const changed = 1
	now := time.Now().UnixMilli()
	variants := [2]State{benchState(changed, true, now), benchState(changed, false, now)}
	key := Key(variants[0].Owner, variants[0].Repo, variants[0].Number, variants[0].SHA)
	for c, n := range benchSizes {
		cache := caches[c]
		b.Run(fmt.Sprintf("records=%d", n), func(b *testing.B) {
			pub := &recPub{}
			runSummaryTick(cache.store, pub, benchDebounce, logger)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				cache.store.mu.Lock()
				cache.store.cacheStateLocked(key, variants[cache.changes%2], cache.revision)
				cache.store.mu.Unlock()
				cache.revision++
				cache.changes++
				runSummaryTick(cache.store, pub, benchDebounce, logger)
			}
			b.StopTimer()
			if got := pub.count(); got != 0 {
				b.Fatalf("the ticks published %d settlements, want none", got)
			}
		})
	}
}
