package session

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// registrySizes are the cached-session counts the registry benchmarks run at: today's largest
// listener, and two steps past it.
var registrySizes = []int{400, 1000, 3000}

// benchRegistry is a registry with n cached sessions and n/10 last-seen records, built from a
// struct literal as cache_test.go's are, so no NATS is involved. live lists every cached id, all
// is the same list, and gone is one id that has only a last-seen record.
//
// The rows go in through the copy's apply path (applyWatched), which is the only way into it, so
// each one's deadline is scheduled as a delivered record's is (storedLocked). They are all live: a
// copy that follows its bucket removes a row at its deadline, so a cache holding rows past theirs
// is not a state the merged registry can be left in, and a benchmark cannot pin one. Each
// sub-benchmark builds its own registry, so each starts from this same state.
func benchRegistry(n int) (r *SessionRegistry, live, all []string, gone string) {
	now := time.Now()
	r = newTestRegistry(5 * time.Minute)
	for i := range n {
		id := fmt.Sprintf("ses_bench_%05d", i)
		value, err := json.Marshal(SessionEntry{
			Port:           4000 + i,
			MachineID:      "bench-machine",
			Dir:            "/bench/" + id,
			Title:          "bench session " + id,
			Capabilities:   []string{"aside", "btw"},
			UpdatedAt:      now.UnixMilli(),
			SelfSubscribed: true,
		})
		if err != nil {
			panic("session: encode a benchmark entry: " + err.Error())
		}
		r.applyWatched(benchEntry{key: id, value: value, revision: uint64(i + 1), created: now})
		live = append(live, id)
		all = append(all, id)
	}
	r.mu.Lock()
	for i := range n / 10 {
		id := fmt.Sprintf("ses_gone_%05d", i)
		r.lastSeen[id] = lastSeenSession{
			lastSeen:  now.Add(-time.Minute).UnixMilli(),
			expiresAt: now.Add(5 * time.Minute),
		}
		if gone == "" {
			gone = id
		}
	}
	r.mu.Unlock()
	return r, live, all, gone
}

// benchEntry is a nats.KeyValueEntry the watcher could have delivered: a put of value under key.
type benchEntry struct {
	key      string
	value    []byte
	revision uint64
	created  time.Time
}

func (e benchEntry) Bucket() string               { return "envoy_sessions" }
func (e benchEntry) Key() string                  { return e.key }
func (e benchEntry) Value() []byte                { return e.value }
func (e benchEntry) Revision() uint64             { return e.revision }
func (e benchEntry) Created() time.Time           { return e.created }
func (e benchEntry) Delta() uint64                { return 0 }
func (e benchEntry) Operation() natsgo.KeyValueOp { return natsgo.KeyValuePut }

// BenchmarkRegistry times the session cache's reads at each of registrySizes: one Get, one List,
// one LastSeen, a Get of every cached id in turn (the interest reaper's access pattern: Reap asks
// isSessionLive, which is Get, once per interest), and Gets from every P beside one writer applying
// watcher entries the whole time.
func BenchmarkRegistry(b *testing.B) {
	for _, n := range registrySizes {
		b.Run(fmt.Sprintf("Get/n=%d", n), func(b *testing.B) {
			r, live, _, _ := benchRegistry(n)
			id := live[0]
			b.ReportAllocs()
			for b.Loop() {
				if _, err := r.Get(id); err != nil {
					b.Fatalf("Get(%s): %v", id, err)
				}
			}
		})
	}
	for _, n := range registrySizes {
		b.Run(fmt.Sprintf("List/n=%d", n), func(b *testing.B) {
			r, live, _, _ := benchRegistry(n)
			b.ReportAllocs()
			for b.Loop() {
				entries, err := r.List()
				if err != nil || len(entries) != len(live) {
					b.Fatalf("List: %d entries, err %v; want %d", len(entries), err, len(live))
				}
			}
		})
	}
	for _, n := range registrySizes {
		b.Run(fmt.Sprintf("LastSeen/n=%d", n), func(b *testing.B) {
			r, _, _, gone := benchRegistry(n)
			b.ReportAllocs()
			for b.Loop() {
				if r.LastSeen(gone) == 0 {
					b.Fatalf("LastSeen(%s) = 0, want its last heartbeat", gone)
				}
			}
		})
	}
	for _, n := range registrySizes {
		b.Run(fmt.Sprintf("GetEach/n=%d", n), func(b *testing.B) {
			r, _, all, _ := benchRegistry(n)
			b.ReportAllocs()
			for b.Loop() {
				for _, id := range all {
					_, _ = r.Get(id)
				}
			}
		})
	}
	for _, n := range registrySizes {
		b.Run(fmt.Sprintf("ParallelGetWithWriter/n=%d", n), func(b *testing.B) {
			benchmarkParallelGetWithWriter(b, n)
		})
	}
}

// benchmarkParallelGetWithWriter times Gets of live ids from every P while one goroutine applies
// a fresh put of a live session through applyWatched in a loop, as the session bucket's watcher
// does on every heartbeat. The writer keeps each session live (Created is now), so the registry's
// shape holds for the whole run. The writer stops before the benchmark returns.
func benchmarkParallelGetWithWriter(b *testing.B, n int) {
	r, live, _, _ := benchRegistry(n)
	values := make([][]byte, len(live))
	for i, id := range live {
		entry, err := r.Get(id)
		if err != nil {
			b.Fatalf("Get(%s): %v", id, err)
		}
		value, err := json.Marshal(entry)
		if err != nil {
			b.Fatalf("marshal %s: %v", id, err)
		}
		values[i] = value
	}
	stop := make(chan struct{})
	var writer sync.WaitGroup
	writer.Go(func() {
		revision := uint64(n)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			revision++
			j := i % len(live)
			r.applyWatched(benchEntry{key: live[j], value: values[j], revision: revision, created: time.Now()})
		}
	})
	b.Cleanup(func() {
		close(stop)
		writer.Wait()
	})
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			id := live[i%len(live)]
			if _, err := r.Get(id); err != nil {
				b.Errorf("Get(%s): %v", id, err)
				return
			}
			i++
		}
	})
	b.StopTimer()
}
