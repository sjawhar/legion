package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// liveOnlyServer serves GET /ws/live as a Dispatch from #2449 on does, and 404 everywhere else: it
// answers the subscribe frame with events_start and one event frame, then holds the socket open.
func liveOnlyServer(t *testing.T, event string) *httptest.Server {
	t.Helper()
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
		for {
			if _, _, err := conn.ReadMessage(); err != nil { // the subscribe frame, or the probe's close
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"events_start","sub":"events","since":"40","replay_complete":false}`)); err != nil {
				return
			}
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"event","sub":"events","id":"41","event":`+event+`}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// sseOnlyServer serves the SSE feed of a Dispatch before #2449 and answers 404 at /ws/live: it
// sends one data frame and holds the stream open.
func sseOnlyServer(t *testing.T, event string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ssePath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		flusher.Flush()
		fmt.Fprintf(w, "id: 41\nevent: comment.created\ndata: %s\n\n", event)
		flusher.Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDashboardChoice probes two builds and opens the dashboard's streams on each: one that serves
// /ws/live gets live sockets, and one that answers 404 there gets SSE streams on GET
// /api/v1/events. Each build's frame arrives and carries the run's marker.
func TestDashboardChoice(t *testing.T) {
	const nonce = 42
	event := `{"id":41,"type":"comment.created","created_at":"2026-10-27T05:00:00Z","payload":{"body":"` + markerTag(nonce, 1234) + `"}}`
	live := liveOnlyServer(t, event)
	sse := sseOnlyServer(t, event)
	for _, tc := range []struct {
		name, want, wantURL string
		srv                 *httptest.Server
	}{
		{"a build that serves /ws/live", dashboardLive, "ws://" + strings.TrimPrefix(live.URL, "http://") + "/ws/live", live},
		{"a build that answers 404 at /ws/live", dashboardSSE, sse.URL + ssePath, sse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			kind, url, err := probeDashboard(ctx, tc.srv.URL)
			if err != nil {
				t.Fatalf("probeDashboard: %v", err)
			}
			if kind != tc.want || url != tc.wantURL {
				t.Fatalf("probe chose %s on %s, want %s on %s", kind, url, tc.want, tc.wantURL)
			}

			st := &step{
				cfg:      runConfig{live: 2, dashboard: kind},
				nonce:    nonce,
				winStart: time.Now().Add(-time.Minute),
				winEnd:   time.Now().Add(time.Minute),
			}
			ds := st.startDashboard(ctx, url)
			var frames []liveFrame
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
				if frames = ds.frames.snapshot(); len(frames) >= 2 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if len(frames) < 2 {
				t.Fatalf("%d frames from 2 %s streams, want 2", len(frames), kind)
			}
			for _, f := range frames {
				if f.id != 41 || f.marker != 1234 || f.at == 0 {
					t.Fatalf("frame %+v: want event 41 with the run's marker, timed on arrival", f)
				}
			}
		})
	}

	t.Run("a build that serves neither", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(http.NotFound))
		defer srv.Close()
		_, _, err := probeDashboard(context.Background(), srv.URL)
		if err == nil || !strings.Contains(err.Error(), "serves no dashboard stream") {
			t.Fatalf("err = %v, want the refusal naming both answers", err)
		}
	})
}
