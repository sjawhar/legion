package kvwatch_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/goroutinetest"
	"github.com/sjawhar/envoy/internal/kvwatch"
	"github.com/sjawhar/envoy/internal/kvwatch/kvwatchtest"
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
// it waits on: a wait that has read the state and found it incomplete.
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
		t.Fatalf("WaitComplete did not return within %s of %s; a caller gating its start on a complete view waits its whole bound out", waitCompleteWake, cause)
		return nil
	}
}

// timingOutWatcher is a watcher of the test bucket whose first scan ends the way nats.go's idle
// timer ends one, and which stays live afterwards (kvwatchtest.TimingOut): the cache is ready and
// never complete, which is the state a WaitComplete blocks in. Without WithRescan nothing scans
// the bucket again, so only the cause each test turns on can end that wait.
func timingOutWatcher(t *testing.T, kv bus.KeyValue) *kvwatch.Watcher {
	t.Helper()
	into := newSeen()
	w := kvwatch.New("test cache", bus.KeyValue{KeyValue: kvwatchtest.TimingOut(kv, 0)}, into.apply, into.reset)
	t.Cleanup(w.Stop)
	w.Start()
	eventually(t, "the first scan the idle timer ended", w.Ready)
	if _, complete := w.Complete(); complete {
		t.Fatal("a first scan the idle timer ended reads as complete, so nothing would block in WaitComplete")
	}
	return w
}

// A first start that fails with no watcher current completes no scan until a Rewatch, so a caller
// waiting for a complete view is answered its error at once rather than waiting its own bound out.
func TestWaitCompleteReturnsOnAFailedFirstStart(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	conn.Close()
	w := kvwatch.New("test cache", kv, newSeen().apply, func() {})
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

// recreatedStatusKV answers Status from the bucket it watches until the test moves it to another
// stream's, which is what a bucket deleted and created again under a live watcher looks like to
// Check: a stream created at a moment that is not the one the watch recorded. Deleting the bucket
// for real would also end the watcher, and that end records a terminal error of its own, so the
// wake this test is about would no longer be the only one that can answer its wait.
type recreatedStatusKV struct {
	natsgo.KeyValue

	other     natsgo.KeyValueStatus
	recreated *atomic.Bool
}

func (k recreatedStatusKV) Status() (natsgo.KeyValueStatus, error) {
	if k.recreated.Load() {
		return k.other, nil
	}
	return k.KeyValue.Status()
}

// anotherStreamsStatus is the status of a second bucket, created and deleted here: its stream was
// created at its own moment, which is the whole of what Check compares.
func anotherStreamsStatus(t *testing.T, conn *natsgo.Conn) natsgo.KeyValueStatus {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	const bucket = "kvwatch-test-other-stream"
	kv, err := bus.EnsureKeyValue(js, &natsgo.KeyValueConfig{Bucket: bucket})
	if err != nil {
		t.Fatalf("create %s: %v", bucket, err)
	}
	status, err := kv.Status()
	if err != nil {
		t.Fatalf("status of %s: %v", bucket, err)
	}
	if err := js.DeleteKeyValue(bucket); err != nil {
		t.Fatalf("delete %s: %v", bucket, err)
	}
	return status
}

// A bucket recreated under a live watcher, which Check records as that watcher's terminal error,
// completes no scan until a Rewatch resets the cache, so a caller waiting for a complete view is
// answered that error rather than waiting its own bound out. The view refuses every read until
// then (store.Registry.RolesHeldBy reads the error first too), so answering such a wait nil would
// release a caller onto a view that answers nothing.
func TestWaitCompleteReturnsWhenCheckFindsTheBucketRecreated(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	recreated := &atomic.Bool{}
	handle := bus.KeyValue{KeyValue: recreatedStatusKV{
		KeyValue:  kv,
		other:     anotherStreamsStatus(t, conn),
		recreated: recreated,
	}}
	w := timingOutWatcher(t, handle)

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
// caller waiting for a complete view is answered the end's terminal error rather than waiting its
// own bound out.
func TestWaitCompleteReturnsWhenTheWatcherEndsOnItsOwn(t *testing.T) {
	uri := testnats.URL(t)
	conn, kv := bucket(t, uri)
	w := timingOutWatcher(t, kv)

	ctx, cancel := context.WithTimeout(context.Background(), waitCompleteCtx)
	defer cancel()
	answers := waitCompleteBlocked(t, ctx, w)
	conn.Close()

	err := answerWithin(t, "the watcher ending on its own", answers)
	if err == nil || !errors.Is(err, w.Err()) {
		t.Fatalf("WaitComplete after the watcher ended = %v, want the terminal error Err records (%v)", err, w.Err())
	}
}

// A stopped watcher arms nothing and completes no scan, so a caller waiting for a complete view at
// a shutdown is answered at once rather than holding the shutdown for its own bound.
func TestWaitCompleteReturnsWhenStopRetiresTheWatcher(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	w := timingOutWatcher(t, kv)

	ctx, cancel := context.WithTimeout(context.Background(), waitCompleteCtx)
	defer cancel()
	answers := waitCompleteBlocked(t, ctx, w)
	w.Stop()

	err := answerWithin(t, "Stop retiring the watcher", answers)
	if err == nil || !strings.Contains(err.Error(), "stopped before its first scan completed") {
		t.Fatalf("WaitComplete after Stop = %v, want the stop named", err)
	}
}

// A view that is merely incomplete is not an early return: the watcher runs, nothing is recorded
// against it, and the wait ends with its caller's own deadline, which is what bounds a listener's
// startup gate.
func TestWaitCompleteReturnsItsContextsErrorWhileTheViewIsIncomplete(t *testing.T) {
	uri := testnats.URL(t)
	_, kv := bucket(t, uri)
	w := timingOutWatcher(t, kv)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	answers := make(chan error, 1)
	go func() { answers <- w.WaitComplete(ctx) }()

	err := answerWithin(t, "its context ending", answers)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitComplete on an incomplete view = %v, want its context's deadline", err)
	}
	if w.Err() != nil {
		t.Fatalf("the watcher recorded %v; the wait is meant to have ended on its context alone", w.Err())
	}
}
