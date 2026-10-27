package kvwatch_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// rewatchBenchValueBytes is the size of each value the rewatch benchmark stores, about a CI
// record's.
const rewatchBenchValueBytes = 2400

// rewatchBenchWait bounds one warm-up: 25,000 values replay in seconds even on two cores.
const rewatchBenchWait = 2 * time.Minute

// warmUp is one "<name> cache warm-up" line a watcher logged: the entries its scan delivered and
// how the scan ended.
type warmUp struct {
	entries int64
	outcome string
}

// warmUpHandler is a slog.Handler that hands every warm-up line the watcher logs to warmUps and
// drops the rest. The watcher logs that line once per generation, when the current watcher's scan
// reaches its nil marker and only for the generation still current (kvwatch.Watcher.consume), so
// the line after a Rewatch is the new generation's.
type warmUpHandler struct{ warmUps chan<- warmUp }

func (h warmUpHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h warmUpHandler) Handle(_ context.Context, record slog.Record) error {
	if !strings.HasSuffix(record.Message, " cache warm-up") {
		return nil
	}
	line := warmUp{entries: -1}
	record.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "entries":
			line.entries = attr.Value.Int64()
		case "outcome":
			line.outcome = attr.Value.String()
		}
		return true
	})
	h.warmUps <- line
	return nil
}

func (h warmUpHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h warmUpHandler) WithGroup(string) slog.Handler      { return h }

// awaitWarmUp returns the next warm-up line, failing b when none comes within rewatchBenchWait or
// the scan did not complete.
func awaitWarmUp(b *testing.B, warmUps <-chan warmUp) warmUp {
	b.Helper()
	select {
	case line := <-warmUps:
		if line.outcome != "completed" {
			b.Fatalf("warm-up ended %q after %d entries, want completed", line.outcome, line.entries)
		}
		return line
	case <-time.After(rewatchBenchWait):
		b.Fatalf("no warm-up line within %s", rewatchBenchWait)
		return warmUp{}
	}
}

// processCPU is the CPU time this process has used, user and system. The test NATS server runs in
// this process, so it counts the server's side of a replay as well as the watcher's.
func processCPU(b *testing.B) time.Duration {
	b.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatalf("getrusage: %v", err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// fillBucket stores keys values of rewatchBenchValueBytes in kv's bucket with asynchronous
// publishes, so setup does not wait out one round trip per key, and fails b unless the bucket then
// holds them all.
func fillBucket(b *testing.B, conn *natsgo.Conn, kv bus.KeyValue, keys int) {
	b.Helper()
	if keys == 0 {
		return
	}
	js, err := conn.JetStream(natsgo.PublishAsyncMaxPending(1024))
	if err != nil {
		b.Fatalf("jetstream: %v", err)
	}
	value := bytes.Repeat([]byte("x"), rewatchBenchValueBytes)
	for i := range keys {
		if _, err := js.PublishAsync(fmt.Sprintf("$KV.%s.key-%05d", kv.Bucket(), i), value); err != nil {
			b.Fatalf("publish key %d: %v", i, err)
		}
	}
	select {
	case <-js.PublishAsyncComplete():
	case <-time.After(rewatchBenchWait):
		b.Fatalf("%d publishes not acknowledged within %s", js.PublishAsyncPending(), rewatchBenchWait)
	}
	status, err := kv.Status()
	if err != nil {
		b.Fatalf("bucket status: %v", err)
	}
	if got := status.Values(); got != uint64(keys) {
		b.Fatalf("bucket holds %d values, want %d", got, keys)
	}
}

// BenchmarkRewatch times one Rewatch of a watcher on its own live connection, from the call until
// the new generation's scan has delivered every key and reached its nil marker, over a bucket of
// 0, 1,000 and 25,000 values of about 2.4 KB. It reports the entries the new generation applied per
// op (entries/op) and the process's CPU per op (cpu-ms/op), which includes the in-process NATS
// server's.
func BenchmarkRewatch(b *testing.B) {
	for _, keys := range []int{0, 1000, 25000} {
		b.Run(fmt.Sprintf("keys=%d", keys), func(b *testing.B) {
			benchmarkRewatch(b, keys)
		})
	}
}

func benchmarkRewatch(b *testing.B, keys int) {
	_, uri := testnats.Start(b)
	conn := testnats.Connect(b, uri)
	b.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		b.Fatalf("jetstream: %v", err)
	}
	kv, err := bus.EnsureKeyValue(js, &natsgo.KeyValueConfig{Bucket: "kvwatch-bench"})
	if err != nil {
		b.Fatalf("bucket: %v", err)
	}
	fillBucket(b, conn, kv, keys)

	var applied atomic.Int64
	warmUps := make(chan warmUp, 4)
	w := kvwatch.New("rewatch bench", kv,
		func(natsgo.KeyValueEntry) { applied.Add(1) },
		func() {},
		kvwatch.WithLogger(slog.New(warmUpHandler{warmUps: warmUps})))
	b.Cleanup(w.Stop)
	w.Start()
	ctx, cancel := context.WithTimeout(context.Background(), rewatchBenchWait)
	defer cancel()
	if err := w.WaitReady(ctx); err != nil {
		b.Fatalf("WaitReady: %v", err)
	}
	// The first generation logs its warm-up before it releases readiness.
	if line := awaitWarmUp(b, warmUps); line.entries != int64(keys) {
		b.Fatalf("first warm-up delivered %d entries, want %d", line.entries, keys)
	}

	b.ReportAllocs()
	appliedBefore := applied.Load()
	cpuBefore := processCPU(b)
	for b.Loop() {
		if err := w.Rewatch(conn); err != nil {
			b.Fatalf("Rewatch: %v", err)
		}
		// Readiness stays released after the first generation, so the new generation's own
		// warm-up line is what says its scan reached the nil marker.
		if line := awaitWarmUp(b, warmUps); line.entries != int64(keys) {
			b.Fatalf("warm-up delivered %d entries, want %d", line.entries, keys)
		}
	}
	cpu := processCPU(b) - cpuBefore
	b.ReportMetric(float64(applied.Load()-appliedBefore)/float64(b.N), "entries/op")
	b.ReportMetric(float64(cpu)/float64(time.Millisecond)/float64(b.N), "cpu-ms/op")
}
