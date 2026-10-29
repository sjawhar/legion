package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadSSE(t *testing.T) {
	const nonce = 42
	stream := ": open\n\n" +
		"id: 41\nevent: comment.created\ndata: {\"id\":41,\"type\":\"comment.created\",\"created_at\":\"2026-10-27T05:00:00.123456789Z\",\"payload\":{\"body\":\"" + markerTag(nonce, 1234) + " hello\"}}\n\n" +
		": heartbeat\n\n" +
		// Another run's marker, with the same counter: not this run's.
		"id: 40\nevent: issue.updated\ndata: {\"id\":40,\"type\":\"issue.updated\",\"created_at\":\"2026-10-27T05:00:01Z\",\"payload\":{\"body\":\"" + markerTag(nonce+1, 1234) + "\"}}\n\n"
	clock := time.Unix(1000, 0)
	now := func() time.Time { clock = clock.Add(time.Millisecond); return clock }
	var got []liveFrame
	n, err := readSSE(strings.NewReader(stream), nonce, now, func(f liveFrame) { got = append(got, f) })
	if err != io.EOF {
		t.Fatalf("err = %v, want EOF", err)
	}
	if n != int64(len(stream)) {
		t.Fatalf("bytes = %d, want %d", n, len(stream))
	}
	if len(got) != 2 {
		t.Fatalf("frames = %d, want 2: %+v", len(got), got)
	}
	created0 := time.Date(2026, 10, 27, 5, 0, 0, 123456789, time.UTC).UnixNano()
	if f := got[0]; f.id != 41 || f.created != created0 || f.marker != 1234 || f.outOfOrder || f.at != time.Unix(1000, int64(time.Millisecond)).UnixNano() {
		t.Fatalf("frame 0 = %+v", f)
	}
	if f := got[1]; f.id != 40 || f.created != time.Date(2026, 10, 27, 5, 0, 1, 0, time.UTC).UnixNano() || f.marker != 0 || !f.outOfOrder {
		t.Fatalf("frame 1 = %+v", f)
	}
}

// TestFollowSSE serves two streams the way a Dispatch without the dashboard live socket does: the
// first ends, after which the client must ask again, and the second, which it holds open, starts
// the order check over. The frames are the ones readLive gives on the live socket.
func TestFollowSSE(t *testing.T) {
	const nonce = 42
	first := ": open\n\n" +
		"id: 41\nevent: comment.created\ndata: {\"id\":41,\"created_at\":\"2026-10-27T05:00:00.123456789Z\",\"payload\":{\"body\":\"" + markerTag(nonce, 1234) + " hello\"}}\n\n" +
		// Another run's marker, with the same counter: not this run's; and its id is below 41.
		"id: 40\nevent: issue.updated\ndata: {\"id\":40,\"created_at\":\"2026-10-27T05:00:01Z\",\"payload\":{\"body\":\"" + markerTag(nonce+1, 1234) + "\"}}\n\n"
	second := "id: 39\nevent: comment.created\ndata: {\"id\":39,\"created_at\":\"2026-10-27T05:00:02Z\"}\n\n"

	var (
		mu      sync.Mutex
		streams int
		users   []string
		accepts []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sseEventsPath {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		streams++
		n := streams
		users = append(users, r.Header.Get("X-Dispatch-User"))
		accepts = append(accepts, r.Header.Get("Accept"))
		mu.Unlock()
		w.Header().Set("Content-Type", sseMediaType)
		body := second
		if n == 1 {
			body = first
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write: %v", err)
			return
		}
		_ = http.NewResponseController(w).Flush()
		if n == 1 { // the stream ends; the client asks again a retry later
			return
		}
		<-r.Context().Done() // until the client goes away
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := make(chan liveFrame, 16)
	var bytes int64
	done := make(chan struct{})
	before := time.Now().UnixNano()
	go func() {
		defer close(done)
		followSSE(ctx, &http.Client{}, sseEventsURL(srv.URL), http.Header{"X-Dispatch-User": {"viewer-0"}}, nonce,
			10*time.Millisecond, func(f liveFrame) { frames <- f }, func(n int64) { bytes += n })
	}()
	var got []liveFrame
	for len(got) < 3 {
		select {
		case f := <-frames:
			got = append(got, f)
		case <-ctx.Done():
			t.Fatalf("frames %+v, want 3 before the deadline", got)
		}
	}
	cancel()
	<-done
	after := time.Now().UnixNano()

	created0 := time.Date(2026, 10, 27, 5, 0, 0, 123456789, time.UTC).UnixNano()
	for i, want := range []liveFrame{
		{id: 41, created: created0, marker: 1234},
		{id: 40, created: time.Date(2026, 10, 27, 5, 0, 1, 0, time.UTC).UnixNano(), outOfOrder: true},
		// A new stream: 39 is below 40, but the order check started over.
		{id: 39, created: time.Date(2026, 10, 27, 5, 0, 2, 0, time.UTC).UnixNano()},
	} {
		f := got[i]
		if f.at < before || f.at > after {
			t.Errorf("frame %d arrived at %d, outside the test's [%d, %d]", i, f.at, before, after)
		}
		f.at = 0
		if f != want {
			t.Errorf("frame %d = %+v, want %+v", i, f, want)
		}
	}
	if len(frames) != 0 {
		t.Errorf("%d frames beyond the 3 data frames", len(frames))
	}
	if want := int64(len(first) + len(second)); bytes != want {
		t.Errorf("bytes = %d, want %d, every byte of both streams", bytes, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if streams != 2 || len(users) != 2 || users[0] != "viewer-0" || users[1] != "viewer-0" {
		t.Errorf("streams %d as %q, want 2 as viewer-0: the client asks again when a stream ends", streams, users)
	}
	for _, accept := range accepts {
		if accept != sseMediaType {
			t.Errorf("Accept %q, want %s", accept, sseMediaType)
		}
	}
}
