package workspace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// fakeWindows is the countingReader's window in a test: the watch asks for one window after
// another, and the test closes each when the stream has delivered what the case means it to, so
// a stream's whole run takes no real time. A window the test never closes never passes, which is
// how a test that drives no stall (every other fetch test) leaves the watch waiting.
type fakeWindows struct {
	asked chan chan time.Time
	// pending is the window the watch is waiting on, held by the test's own goroutine alone.
	pending chan time.Time
}

func newFakeWindows() *fakeWindows { return &fakeWindows{asked: make(chan chan time.Time, 1)} }

func (w *fakeWindows) window(time.Duration) <-chan time.Time {
	opened := make(chan time.Time, 1)
	w.asked <- opened
	return opened
}

// waiting is the window the watch waits on now, failing the test when it asked for none.
func (w *fakeWindows) waiting(t *testing.T) chan time.Time {
	t.Helper()
	if w.pending == nil {
		select {
		case opened := <-w.asked:
			w.pending = opened
		case <-time.After(testTimeout):
			t.Fatal("the low-speed watch waited on no window")
		}
	}
	opened := w.pending
	w.pending = nil
	return opened
}

// pass closes one whole CacheStreamLowSpeedTime over what the stream delivered since the last,
// and returns once the watch has judged it: it has either opened the next window or ended the
// stream, so what the test reads next sees the judgement.
func (w *fakeWindows) pass(t *testing.T, ctx context.Context) {
	t.Helper()
	w.waiting(t) <- time.Time{}
	select {
	case opened := <-w.asked:
		w.pending = opened
	case <-ctx.Done():
	case <-time.After(testTimeout):
		t.Fatal("the low-speed watch neither opened another window nor ended the stream")
	}
}

// A stream that keeps delivering more than CacheStreamLowSpeedBytes in every
// CacheStreamLowSpeedTime runs for as long as its repository needs: seven such windows are 35
// minutes of a pod's time, past the FetchTimeout a clock once gave the stream and its
// verification together, and the stream still reaches its last byte whole and unstopped. The
// clock in its place threw away a download that was still arriving (INFRA-495).
func TestTheCachesStreamRunsPastFetchTimeoutWhileItKeepsReceiving(t *testing.T) {
	const windows = 7
	if ran := windows * CacheStreamLowSpeedTime; ran <= FetchTimeout {
		t.Fatalf("%d windows of %s are %s, not past the %s a clock gave the stream", windows, CacheStreamLowSpeedTime, ran, FetchTimeout)
	}
	const chunk = CacheStreamLowSpeedBytes + 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	windowsOf := newFakeWindows()
	stream := &countingReader{reader: bytes.NewReader(bytes.Repeat([]byte{'p'}, windows*chunk)), window: windowsOf.window}
	go stream.watchStall(ctx, cancel)

	buffer := make([]byte, chunk)
	for window := range windows {
		if _, err := io.ReadFull(stream, buffer); err != nil {
			t.Fatalf("the stream ended %s in, after %d bytes: %v", time.Duration(window)*CacheStreamLowSpeedTime, stream.read.Load(), err)
		}
		windowsOf.pass(t, ctx)
	}
	if n, err := stream.Read(buffer); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("the stream's last read = %d, %v; want its end", n, err)
	}
	if got, want := stream.read.Load(), int64(windows*chunk); got != want {
		t.Errorf("the stream delivered %d bytes, want the whole %d", got, want)
	}
	if ctx.Err() != nil || stream.stalled.Load() {
		t.Errorf("a stream delivering %d bytes per %s was ended: %v", chunk, CacheStreamLowSpeedTime, ctx.Err())
	}
}

// A stream that falls below the bound is ended, and names it: half of CacheStreamLowSpeedBytes in
// a whole CacheStreamLowSpeedTime is under git's own http.lowSpeedLimit over http.lowSpeedTime
// rule, 1 MiB in 5 minutes, so the watch cancels the context the cache's body is read under — the
// one thing that wakes a Read blocked on a sender that has stopped — and the failure says the
// bound rather than a bare cancelled context.
func TestTheCachesStreamEndsWhenItFallsBelowTheLowSpeedBound(t *testing.T) {
	if CacheStreamLowSpeedBytes != 1<<20 || CacheStreamLowSpeedTime != 5*time.Minute {
		t.Fatalf("the low-speed bound is %d bytes in %s, want 1 MiB in 5 minutes", CacheStreamLowSpeedBytes, CacheStreamLowSpeedTime)
	}
	const sent = CacheStreamLowSpeedBytes / 2
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	windowsOf := newFakeWindows()
	stream := &countingReader{reader: &stoppedSender{ctx: ctx, left: sent}, window: windowsOf.window}
	go stream.watchStall(ctx, cancel)

	if _, err := io.ReadFull(stream, make([]byte, sent)); err != nil {
		t.Fatalf("the stream's first %d bytes: %v", sent, err)
	}
	windowsOf.pass(t, ctx)
	_, err := stream.Read(make([]byte, 1<<16))
	if !errors.Is(err, errStreamStalled) {
		t.Fatalf("a stream delivering %d bytes per %s ended with %v, want the low-speed bound's refusal", sent, CacheStreamLowSpeedTime, err)
	}
	if want := "the repository cache delivered less than 1048576 bytes in 5m0s"; !strings.Contains(err.Error(), want) {
		t.Errorf("the stream's failure is %q, want it naming the bound: %q", err, want)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the stream's failure is %q, want the cancelled read it came from too", err)
	}
	if ctx.Err() == nil {
		t.Error("the watch left the cache's body being read after it ended the stream")
	}
}

// stoppedSender is a cache that sends left bytes and then stops, as one whose pod is wedged or
// whose connection is black-holed does: its Read blocks until the watch cancels the context the
// body is read under, which is what an http body's own Read answers then.
type stoppedSender struct {
	ctx  context.Context
	left int
}

func (s *stoppedSender) Read(p []byte) (int, error) {
	if s.left == 0 {
		<-s.ctx.Done()
		return 0, s.ctx.Err()
	}
	n := min(len(p), s.left)
	s.left -= n
	return n, nil
}
