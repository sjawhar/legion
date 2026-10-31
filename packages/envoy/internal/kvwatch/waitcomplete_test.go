package kvwatch_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/testnats"
)

// waitCompleteCtx bounds every WaitComplete these tests make, and waitCompleteWake, well inside
// it, bounds the answer each one waits for. A cause whose wakeComplete is missing therefore fails
// its test at waitCompleteWake, naming the cause, rather than letting the call sit until ctx ends
// or hanging the suite.
const (
	waitCompleteCtx  = 5 * time.Second
	waitCompleteWake = 2 * time.Second
)

// blockedInWaitComplete reports whether a goroutine is parked in WaitComplete, which is the select
// it waits on: a wait that has read the state and found the copy not servable.
func blockedInWaitComplete() bool {
	for _, goroutine := range goroutinetest.Traces() {
		if strings.Contains(goroutine, "[select") && strings.Contains(goroutine, "kvwatch.(*Watcher).WaitComplete(") {
			return true
		}
	}
	return false
}

// waitCompleteBlocked starts WaitComplete under ctx in the background and returns the channel its
// answer arrives on, once that call is parked on its wake-up. Each test turns on its cause only
// after that, so the answer it reads is the wake the cause makes, never a state the call found
// already changed before it waited.
func waitCompleteBlocked(t *testing.T, ctx context.Context, w *kvwatch.Watcher) <-chan error {
	t.Helper()
	answers := make(chan error, 1)
	go func() { answers <- w.WaitComplete(ctx) }()
	eventually(t, "WaitComplete to block on its wake-up", blockedInWaitComplete)
	return answers
}

// answerWithin reads the answer of a WaitComplete waiting on cause, failing the test when none
// comes within waitCompleteWake: that is the wake-up cause does not make.
func answerWithin(t *testing.T, cause string, answers <-chan error) error {
	t.Helper()
	select {
	case err := <-answers:
		return err
	case <-time.After(waitCompleteWake):
		t.Fatalf("WaitComplete did not return within %s of %s; a caller gating its start on a servable copy waits its whole bound out", waitCompleteWake, cause)
		return nil
	}
}

// refillingWatcher is a watcher of the test bucket whose first read stalls and ends at its scan's
// idle wait, with the re-read that read armed left unfired: the cache is ready and its copy is not
// servable (kvwatch.ErrRefilling), which is the state a WaitComplete blocks in. Nothing fires that
// re-read, and no periodic probe runs, so only what each test turns on can end such a wait. It
// returns the watcher and the waits it armed, the re-read among them.
func refillingWatcher(t *testing.T, kv bus.KeyValue) (*kvwatch.Watcher, *armed) {
	t.Helper()
	into := newSeen()
	arm := &armed{}
	now := newClock()
	w := kvwatch.New("test cache", scripted(kv, stallsAfter(0)).handle(), into,
		kvwatch.WithAfterFunc(arm.afterFunc()), kvwatch.WithNow(now.Now), kvwatch.WithStreamProbeInterval(0))
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first read to begin", func() bool { return len(into.reads()) == 1 })
	now.advance(kvwatch.ScanIdleTimeout)
	arm.fireIdle(t)
	eventually(t, "the first read to end as timed out", w.Ready)
	if err := w.Servable(); !errors.Is(err, kvwatch.ErrRefilling) {
		t.Fatalf("a copy no read of the bucket completed answers Servable %v; nothing would block in WaitComplete", err)
	}
	return w, arm
}

// A first start that fails with no watcher current completes no read until a Rewatch, so a caller
// waiting for a servable copy is answered its error at once rather than waiting its own bound out.
func TestWaitCompleteReturnsOnAFailedFirstStart(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	conn.Close()
	w := kvwatch.New("test cache", kv, newSeen())
	t.Cleanup(w.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), waitCompleteCtx)
	defer cancel()
	answers := waitCompleteBlocked(t, ctx, w)
	w.Start()

	err := answerWithin(t, "a first start that failed with no watcher current", answers)
	if err == nil || !errors.Is(err, w.Err()) {
		t.Fatalf("WaitComplete after a failed first start = %v, want the terminal error Err records (%v)", err, w.Err())
	}
}

// recreatedStreams reads the bucket's own stream until the test moves it to another bucket's,
// which is what a bucket deleted and created again under a live watcher looks like to Check: a
// stream created at a moment that is not the one the watch recorded. Deleting the bucket for real
// would also end the watcher, and that end records a terminal error of its own, so the wake this
// test is about would no longer be the only one that can answer its wait.
type recreatedStreams struct {
	jetstream.JetStream

	other     string
	recreated *atomic.Bool
}

func (s recreatedStreams) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	if s.recreated.Load() {
		name = s.other
	}
	return s.JetStream.Stream(ctx, name)
}

// anotherBucketsStream is the stream of a second bucket, created here: it was created at its own
// moment, which is the whole of what Check compares.
func anotherBucketsStream(t *testing.T, conn *natsgo.Conn) string {
	t.Helper()
	const bucket = "kvwatch-test-other-stream"
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := bus.EnsureKeyValue(conn, js, &natsgo.KeyValueConfig{Bucket: bucket}); err != nil {
		t.Fatalf("create %s: %v", bucket, err)
	}
	return "KV_" + bucket
}

// A bucket recreated under a live watcher, which Check records as that watcher's terminal error,
// completes no read until a Rewatch reads the new bucket, so a caller waiting for a servable copy
// is answered that error rather than waiting its own bound out. The view refuses every read until
// then (store.Registry.RolesHeldBy reads the error first too), so answering such a wait nil would
// release a caller onto a view that answers nothing.
func TestWaitCompleteReturnsWhenCheckFindsTheBucketRecreated(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	recreated := &atomic.Bool{}
	streams := recreatedStreams{JetStream: kv.Streams(), other: anotherBucketsStream(t, conn), recreated: recreated}
	w, _ := refillingWatcher(t, bus.NewKeyValue(kv.Legacy(), kv.WatchHandle(), streams))

	ctx, cancel := context.WithTimeout(context.Background(), waitCompleteCtx)
	defer cancel()
	answers := waitCompleteBlocked(t, ctx, w)
	recreated.Store(true)
	if err := w.Check(); err != nil {
		t.Fatalf("Check on the recreated bucket: %v", err)
	}

	err := answerWithin(t, "Check recording a bucket recreated under the watcher", answers)
	if err == nil || !strings.Contains(err.Error(), "was recreated under its watcher") {
		t.Fatalf("WaitComplete after Check found the bucket recreated = %v, want the recreate Err records (%v)", err, w.Err())
	}
}

// A watcher that ends on its own - its connection closed - freezes the cache until a Rewatch, so a
// caller waiting for a servable copy is answered the end's terminal error rather than waiting its
// own bound out.
func TestWaitCompleteReturnsWhenTheWatcherEndsOnItsOwn(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	w, _ := refillingWatcher(t, kv)

	ctx, cancel := context.WithTimeout(context.Background(), waitCompleteCtx)
	defer cancel()
	answers := waitCompleteBlocked(t, ctx, w)
	conn.Close()

	err := answerWithin(t, "the watcher ending on its own", answers)
	if err == nil || !errors.Is(err, w.Err()) {
		t.Fatalf("WaitComplete after the watcher ended = %v, want the terminal error Err records (%v)", err, w.Err())
	}
}

// A stopped watcher arms nothing and completes no read, so a caller waiting for a servable copy at
// a shutdown is answered at once rather than holding the shutdown for its own bound.
func TestWaitCompleteReturnsWhenStopRetiresTheWatcher(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	w, _ := refillingWatcher(t, kv)

	ctx, cancel := context.WithTimeout(context.Background(), waitCompleteCtx)
	defer cancel()
	answers := waitCompleteBlocked(t, ctx, w)
	w.Stop()

	err := answerWithin(t, "Stop retiring the watcher", answers)
	if err == nil || !strings.Contains(err.Error(), "stopped before a read of its bucket completed") {
		t.Fatalf("WaitComplete after Stop = %v, want the stop named", err)
	}
}

// The read the watcher arms for itself after a scan that timed out is what ends the wait in the
// ordinary case: its completion makes the copy servable, and the waiter is released then, with no
// Rewatch and no poll of its own.
func TestWaitCompleteReturnsWhenTheWatchersOwnReReadCompletes(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	if _, err := kv.Put("key", []byte("1")); err != nil {
		t.Fatalf("put: %v", err)
	}
	w, arm := refillingWatcher(t, kv)

	ctx, cancel := context.WithTimeout(context.Background(), waitCompleteCtx)
	defer cancel()
	answers := waitCompleteBlocked(t, ctx, w)
	arm.fireIt(t)

	if err := answerWithin(t, "the watcher's own re-read completing", answers); err != nil {
		t.Fatalf("WaitComplete after the re-read completed = %v, want nil", err)
	}
	if err := w.Servable(); err != nil {
		t.Fatalf("the wait returned nil over a copy Servable refuses (%v)", err)
	}
}

// A copy that is merely refilling is not an early return: the watcher runs, nothing is recorded
// against it, and the wait ends with its caller's own deadline, which is what bounds a listener's
// startup gate.
func TestWaitCompleteReturnsItsContextsErrorWhileTheCopyIsRefilling(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	w, _ := refillingWatcher(t, kv)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	answers := make(chan error, 1)
	go func() { answers <- w.WaitComplete(ctx) }()

	err := answerWithin(t, "its context ending", answers)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitComplete on a refilling copy = %v, want its context's deadline", err)
	}
	if w.Err() != nil {
		t.Fatalf("the watcher recorded %v; the wait is meant to have ended on its context alone", w.Err())
	}
}
