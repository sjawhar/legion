package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// BenchmarkReap times one interest reaper pass over n live sessions, each with an interest and a
// session entry, with the liveness check the listener's reaper passes (main.go's StartReaper call).
// Every session is live, so a pass deletes nothing and each op is the same pass.
func BenchmarkReap(b *testing.B) {
	for _, n := range []int{400, 1000, 3000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			benchmarkReap(b, n)
		})
	}
}

func benchmarkReap(b *testing.B, n int) {
	client, err := bus.ConnectOwningStream([]string{testnats.URL(b)}, bus.WithReplicas(1))
	if err != nil {
		b.Fatalf("connect bus: %v", err)
	}
	b.Cleanup(client.Close)
	quiet := slog.New(slog.DiscardHandler)
	registry, err := store.Open(client.Conn, store.WithReplicas(1), store.WithLogger(quiet))
	if err != nil {
		b.Fatalf("open registry: %v", err)
	}
	b.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(client.Conn, session.WithSessionReplicas(1), session.WithSessionLogger(quiet))
	if err != nil {
		b.Fatalf("open session registry: %v", err)
	}
	b.Cleanup(sessions.StopWatch)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := registry.WaitForCacheReady(ctx); err != nil {
		b.Fatalf("interest cache not ready: %v", err)
	}
	if err := sessions.WaitForCacheReady(ctx); err != nil {
		b.Fatalf("session cache not ready: %v", err)
	}

	// Register the sessions from a few goroutines, so setup does not wait out 2n round trips one
	// after another. A worker that fails keeps draining ids, so the feed below never blocks.
	const workers = 16
	ids := make(chan string)
	errs := make(chan error, workers)
	var registering sync.WaitGroup
	for range workers {
		registering.Go(func() {
			var failed error
			for id := range ids {
				if failed != nil {
					continue
				}
				if _, err := registry.Upsert(store.Interest{
					SessionID: id,
					MachineID: "bench-machine",
					Dir:       "/bench/" + id,
				}, []string{contracts.AgentSubject(id), "notifications.team.bench"}); err != nil {
					failed = fmt.Errorf("upsert interest %s: %w", id, err)
					continue
				}
				if err := sessions.Put(id, session.SessionEntry{
					MachineID:      "bench-machine",
					Dir:            "/bench/" + id,
					SelfSubscribed: true,
				}); err != nil {
					failed = fmt.Errorf("put session %s: %w", id, err)
				}
			}
			errs <- failed
		})
	}
	for i := range n {
		ids <- fmt.Sprintf("ses_reap_bench_%05d", i)
	}
	close(ids)
	registering.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			b.Fatal(err)
		}
	}
	if got := sessions.CacheSize(); got != n {
		b.Fatalf("session cache holds %d sessions, want %d", got, n)
	}

	isAlive := func(id string) bool { return isSessionLive(sessions, id) }
	b.ReportAllocs()
	for b.Loop() {
		reaped, err := registry.Reap(isAlive, 10*time.Minute)
		if err != nil {
			b.Fatalf("Reap: %v", err)
		}
		if reaped != 0 {
			b.Fatalf("Reap removed %d interests of live sessions", reaped)
		}
	}
}
