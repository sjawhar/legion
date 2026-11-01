package docs

import (
	"context"
	"errors"
	"os"
	"runtime"
	"runtime/metrics"
	"testing"
	"time"

	"github.com/reearth/ygo/crdt"
	"github.com/reearth/ygo/provider/websocket"
)

// roomLoadMeasurement is the environment variable that runs the room load measurement
// (TestARoomsLoadCostsAtMostTwoDecodesOfItsStoredState).
const roomLoadMeasurement = "DISPATCH_MEASURE_ROOM_LOAD"

// agentRewrittenCacheEnv names a file the room load measurement keeps the agent-rewritten state in
// (agentRewrittenState), which takes about 200 s to build: read when the file exists, written once
// built when it does not.
const agentRewrittenCacheEnv = "DISPATCH_AGENT_REWRITTEN_CACHE"

// cachedAgentRewrittenState is agentRewrittenState, kept in the file agentRewrittenCacheEnv names.
func cachedAgentRewrittenState(t *testing.T) []byte {
	t.Helper()
	path := os.Getenv(agentRewrittenCacheEnv)
	if path != "" {
		if state, err := os.ReadFile(path); err == nil {
			return state
		}
	}
	state, refusal := agentRewrittenState(t)
	if !errors.Is(refusal, ErrDocumentTooLarge) {
		t.Fatalf("the agent's last write was refused with %v, want ErrDocumentTooLarge", refusal)
	}
	if path != "" {
		if err := os.WriteFile(path, state, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return state
}

// loadCost is what one run of a load cost at GOMAXPROCS=1: how long it took, what it allocated
// (runtime.MemStats.TotalAlloc across it), the most heap its objects held above what the heap held
// before it, sampled every millisecond the scheduler lets the sampler run
// (/memory/classes/heap/objects:bytes), and what it still holds once collected.
type loadCost struct {
	took      time.Duration
	allocated uint64
	peak      uint64
	held      int64
}

// measureLoad runs load once at GOMAXPROCS=1 and returns its cost (loadCost).
func measureLoad(t *testing.T, load func() any) loadCost {
	t.Helper()
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	heapObjects := func() uint64 {
		metrics.Read(sample)
		return sample[0].Value.Uint64()
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&before)
	base := heapObjects()
	stop, peaked := make(chan struct{}), make(chan uint64)
	go func() {
		peak := base
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				peaked <- max(peak, heapObjects())
				return
			case <-ticker.C:
				peak = max(peak, heapObjects())
			}
		}
	}()
	started := time.Now()
	kept := load()
	took := time.Since(started)
	close(stop)
	peak := <-peaked
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	runtime.GC()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(kept)
	return loadCost{took: took, allocated: allocated, peak: peak - base, held: int64(after.HeapAlloc) - int64(before.HeapAlloc)}
}

// A room's load decodes the stored state it loads at most twice, through the room's first
// transaction: the copy its load decodes - the socket admission's (loadTree) or the load's own
// (LoadDoc), whose scans start the room's scan total and which becomes the room's replica
// (keepReplica) - and ygo's decode into the room's own document. For the document an agent rewrote
// whole until the decode cap refused a write (agentRewrittenState) and the long-edited document
// typed on until the cap (longEditedAtCapState), each stored as one update, it measures at
// GOMAXPROCS=1 one decode of the stored state, a load the server's own work makes (Server.Apply on
// a room no one holds, and the room's first transaction, which its update observer renders against
// the replica), and a socket's (the admission's schema check as ServeHTTP makes it, websocket.go,
// then the same load). A load fails that allocates three and a half decodes: beside its two
// decodes it makes two encodes of the room's state, each lighter than a decode, the struct total's
// count (newStructTotal, countRoom) and the base ygo's update log records at the room's first
// commit (sjawhar/ygo v1.51.3-sami.7, crdt/update_log.go:50-56), which the replica, holding the
// load's copy, drops; and a socket's admission renders the tree it checks. On 2026-10-28, while the
// replica decoded the room again at its first transaction, the server's load allocated 4.00
// decodes of the agent-rewritten state and 3.89 of the long-edited one, and a socket's 4.10 and
// 3.89; once the replica took the load's copy, 2.89 and 2.89, and 2.99 and 2.89. Set
// DISPATCH_MEASURE_ROOM_LOAD=1 to run it, with the store, and DISPATCH_AGENT_REWRITTEN_CACHE to a
// file to keep the agent-rewritten state, about 200 s to build, between runs.
func TestARoomsLoadCostsAtMostTwoDecodesOfItsStoredState(t *testing.T) {
	if os.Getenv(roomLoadMeasurement) == "" {
		t.Skip("set " + roomLoadMeasurement + "=1 to run the room load measurement")
	}
	for _, row := range []struct {
		name  string
		state func(t *testing.T) []byte
	}{
		{"an agent's whole rewrites until the cap refused one", cachedAgentRewrittenState},
		{"a long-edited document typed on until the cap", longEditedAtCapState},
	} {
		t.Run(row.name, func(t *testing.T) {
			state := row.state(t)
			ctx := context.Background()
			service, artifactID := newTestService(t)
			service.settle = time.Hour // no settlement runs in the room while it is measured
			storeHistory(t, service, artifactID, state)
			closed := func() { closeResidentRoom(t, service, artifactID) }
			// load loads the room as the server's own work does, then commits the room's first
			// transaction, which its update observer renders against the room's replica.
			load := func() any {
				if err := service.srv.Apply(ctx, artifactID, func(*crdt.Doc, func(func(*crdt.Transaction))) {}); err != nil && !errors.Is(err, websocket.ErrNoChanges) {
					t.Fatalf("load the room: %v", err)
				}
				live := roomDocument(t, service, artifactID)
				live.Transact(func(*crdt.Transaction) {})
				return live
			}
			paths := []struct {
				name    string
				prepare func()
				load    func() any
			}{
				{"a decode of the stored state", func() {}, func() any {
					doc := newDocumentCopy()
					if err := crdt.ApplyUpdateV1(doc, state, nil); err != nil {
						t.Fatal(err)
					}
					return doc
				}},
				{"the server's load and first transaction", closed, load},
				{"a socket's admission, load and first transaction", closed, func() any {
					// The admission's schema check, as ServeHTTP makes it before ygo loads the room.
					tree, preload, err := service.loadTree(ctx, artifactID)
					if err != nil {
						t.Fatalf("the admission's schema check: %v", err)
					}
					if _, err := documentMarkdown(tree); err != nil {
						t.Fatalf("the admission's rendering: %v", err)
					}
					defer service.leavePreload(artifactID, preload)()
					return load()
				}},
			}
			var decode loadCost
			for _, path := range paths {
				path.prepare()
				cost := measureLoad(t, path.load)
				t.Logf("%d-byte state, %s: %s, %d bytes allocated, %d bytes peak heap, %d held",
					len(state), path.name, cost.took.Round(time.Millisecond), cost.allocated, cost.peak, cost.held)
				if decode.allocated == 0 {
					decode = cost
					continue
				}
				decodes := float64(cost.allocated) / float64(decode.allocated)
				t.Logf("%s: %.2f decodes allocated, %.2f decodes' time", path.name, decodes, cost.took.Seconds()/decode.took.Seconds())
				if decodes >= 3.5 {
					t.Errorf("%s allocated %d bytes, %.2f decodes of the stored state (%d each), want under three and a half", path.name, cost.allocated, decodes, decode.allocated)
				}
			}
		})
	}
}
