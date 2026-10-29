package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestFollowLive serves two sockets the way Dispatch's GET /ws/live does and follows them. The
// first sends the frames an events subscription gets beside a heartbeat, another subscription's
// event, an event of another run with an id below the last, and an ended frame, after which the
// client must subscribe again on the same socket; then it closes with 4001 (max age). The second,
// which the client must dial on its own, starts the order check over.
func TestFollowLive(t *testing.T) {
	const nonce = 42
	event := func(id int, extra string) string {
		return `{"type":"event","sub":"events","id":"` + strconv.Itoa(id) + `","event":{"id":` + strconv.Itoa(id) + `,"type":"comment.created"` + extra + `}}`
	}
	first := []string{
		`{"type":"events_start","sub":"events","since":"40","replay_complete":false}`,
		`{"type":"heartbeat"}`,
		event(41, `,"created_at":"2026-10-27T05:00:00.123456789Z","payload":{"body":"`+markerTag(nonce, 1234)+` hello"}`),
		// Another subscription's event frame: not the events view's.
		`{"type":"event","sub":"timeline","id":"99","event":{"id":99,"created_at":"2026-10-27T05:00:00Z"}}`,
		// Another run's marker, with the same counter: not this run's; and its id is below 41.
		event(40, `,"created_at":"2026-10-27T05:00:01Z","payload":{"body":"`+markerTag(nonce+1, 1234)+`"}`),
		`{"type":"ended","sub":"events","reason":"behind"}`,
	}
	afterResubscribe := []string{
		`{"type":"events_start","sub":"events","since":"41","replay_complete":false}`,
		event(42, ""),
	}
	second := []string{
		`{"type":"events_start","sub":"events","since":"38","replay_complete":false}`,
		event(39, `,"created_at":"2026-10-27T05:00:02Z"`),
	}
	var sent int64
	for _, m := range append(append(append([]string{}, first...), afterResubscribe...), second...) {
		sent += int64(len(m))
	}

	var (
		mu         sync.Mutex
		subscribes []string
		users      []string
		conns      int
	)
	upgrader := websocket.Upgrader{EnableCompression: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws/live" {
			http.NotFound(w, r)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		mu.Lock()
		conns++
		n := conns
		users = append(users, r.Header.Get("X-Dispatch-User"))
		mu.Unlock()
		readSubscribe := func() bool {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return false
			}
			mu.Lock()
			subscribes = append(subscribes, string(data))
			mu.Unlock()
			return true
		}
		send := func(frames []string) {
			for _, f := range frames {
				if err := conn.WriteMessage(websocket.TextMessage, []byte(f)); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		}
		switch n {
		case 1:
			if !readSubscribe() {
				return
			}
			send(first)
			if !readSubscribe() {
				return
			}
			send(afterResubscribe)
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(4001, "max age"), time.Now().Add(time.Second))
		case 2:
			if !readSubscribe() {
				return
			}
			send(second)
			for { // until the client closes it
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}
	}))
	defer srv.Close()

	url, err := liveSocketURL(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames := make(chan liveFrame, 16)
	var bytes int64
	done := make(chan struct{})
	before := time.Now().UnixNano()
	go func() {
		defer close(done)
		dialer := &websocket.Dialer{EnableCompression: true}
		followLive(ctx, dialer, url, http.Header{"X-Dispatch-User": {"viewer-0"}}, nonce, 10*time.Millisecond,
			func(f liveFrame) { frames <- f }, func(n int64) { bytes += n })
	}()
	var got []liveFrame
	for len(got) < 4 {
		select {
		case f := <-frames:
			got = append(got, f)
		case <-ctx.Done():
			t.Fatalf("frames %+v, want 4 before the deadline", got)
		}
	}
	cancel()
	<-done
	after := time.Now().UnixNano()

	created0 := time.Date(2026, 10, 27, 5, 0, 0, 123456789, time.UTC).UnixNano()
	for i, want := range []liveFrame{
		{id: 41, created: created0, marker: 1234},
		{id: 40, created: time.Date(2026, 10, 27, 5, 0, 1, 0, time.UTC).UnixNano(), outOfOrder: true},
		{id: 42},
		// A new connection: 39 is below 42, but the order check started over.
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
		t.Errorf("%d frames beyond the 4 event frames of the events view", len(frames))
	}
	if bytes != sent {
		t.Errorf("bytes = %d, want %d, every message Dispatch sent", bytes, sent)
	}
	mu.Lock()
	defer mu.Unlock()
	if conns != 2 || len(users) != 2 || users[0] != "viewer-0" || users[1] != "viewer-0" {
		t.Errorf("connections %d as %q, want 2 as viewer-0: the client dials again after the 4001 close", conns, users)
	}
	if len(subscribes) != 3 {
		t.Fatalf("subscribe frames %q, want 3: one per socket, and one after the ended frame", subscribes)
	}
	for _, s := range subscribes {
		var frame map[string]any
		if err := json.Unmarshal([]byte(s), &frame); err != nil {
			t.Fatalf("subscribe frame %q: %v", s, err)
		}
		view, _ := frame["view"].(map[string]any)
		if len(frame) != 3 || frame["type"] != "subscribe" || frame["sub"] != "events" || len(view) != 1 || view["kind"] != "events" {
			t.Errorf("subscribe frame %s, want {type: subscribe, sub: events, view: {kind: events}} and no resume", s)
		}
	}
}

func TestLiveSocketURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:18777":   "ws://127.0.0.1:18777/ws/live",
		"https://dispatch.example": "wss://dispatch.example/ws/live",
	} {
		if got, err := liveSocketURL(in); err != nil || got != want {
			t.Errorf("liveSocketURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := liveSocketURL("ftp://x"); err == nil || !strings.Contains(err.Error(), "neither") {
		t.Errorf("ftp:// gave %v, want a refusal", err)
	}
}

func TestSummarizeLive(t *testing.T) {
	writes := []writeRec{
		// A comment answered 201 at 1100 ms: its primary event is 10.
		{Marker: 1, Kind: kindComment, InWindow: true, Response: 1100 * msNs, Status: 201},
		// An edit that hit the client timeout but committed: no response to time from.
		{Marker: 2, Kind: kindEdit, InWindow: true, Response: 32000 * msNs, TransportErr: "context deadline exceeded"},
		// Before the window.
		{Marker: 3, Kind: kindComment, InWindow: false, Response: 20 * msNs, Status: 201},
	}
	commits := []commitRow{
		{EventID: 10, Type: "comment.created", Marker: 1},
		{EventID: 11, Type: "artifact.version", Marker: 1}, // another event with marker 1: not timed
		{EventID: 20, Type: "artifact.version", Marker: 2},
		{EventID: 30, Type: "comment.created", Marker: 3},
	}
	frames := []liveFrame{
		{at: 1150 * msNs, id: 10, marker: 1, created: 1040 * msNs},
		{at: 1160 * msNs, id: 11, marker: 1, created: 1040 * msNs},
		{at: 2600 * msNs, id: 20, marker: 2, created: 2500 * msNs},
		{at: 2610 * msNs, id: 30, marker: 3, created: 2500 * msNs, outOfOrder: true},
		{at: 2700 * msNs, id: 31}, // no marker, no created_at
	}

	got := summarizeLive(frames, writes, commits)

	if got.Frames != 5 || got.OrderViolations != 1 {
		t.Fatalf("frames %d order violations %d, want 5 and 1", got.Frames, got.OrderViolations)
	}
	if want := (stat{N: 4, P50: 110, P90: 110, P99: 110, Max: 120}); got.FrameMsFromCreatedAt != want { // 100 110 110 120
		t.Errorf("from created_at %+v, want %+v", got.FrameMsFromCreatedAt, want)
	}
	if want := (stat{N: 1, P50: 50, P90: 50, P99: 50, Max: 50}); got.FrameMsFromWriteResponse != want {
		t.Errorf("from write response %+v, want %+v: only event 10, the 2xx in-window write's primary event", got.FrameMsFromWriteResponse, want)
	}
}
