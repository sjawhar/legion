package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// liveFrameText is one event frame of the events view, as Dispatch sends it.
func liveFrameText(id int64) string {
	return fmt.Sprintf(`{"type":"event","sub":"events","id":"%d","event":{"id":%d,"type":"comment.created"}}`, id, id)
}

func TestReplayTallyCountsTheFramesAtOrBelowTheHeadWithTheirBytes(t *testing.T) {
	tally := replayTally{head: 6}
	var replayed []int64
	for _, f := range []liveFrame{{id: 5, bytes: 120, at: 10}, {id: 6, bytes: 80, at: 20}, {id: 7, bytes: 90, at: 30}} {
		if tally.add(f) {
			replayed = append(replayed, f.id)
		}
	}
	if !slices.Equal(replayed, []int64{5, 6}) {
		t.Fatalf("replayed %v, want 5 and 6 (at or below head 6)", replayed)
	}
	if tally.frames != 2 || tally.bytes != 200 || tally.last != 20 {
		t.Fatalf("tally %d frames %d bytes last %d, want 2 frames, 200 bytes and the last replayed frame's arrival 20", tally.frames, tally.bytes, tally.last)
	}
}

func TestRefetchRoutesAreTheDefaultMixsDashboardRoutes(t *testing.T) {
	m, err := loadRouteMix("")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range refetchRoutes(m) {
		names = append(names, r.Name)
	}
	want := []string{
		"browser GET /api/v1/issues/{key}",
		"browser GET /api/v1/issues?project={project}",
		"browser GET /api/v1/inbox",
		"browser GET /api/v1/me/replies",
		"browser GET /api/v1/me/state",
		"browser GET /api/v1/issues/{key}/events?after=0&limit=200",
	}
	if !slices.Equal(names, want) {
		t.Fatalf("refetch routes %v, want %v", names, want)
	}
}

func TestReconnectDelayIsTheDashboardsBackoff(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 11))
	for range 200 {
		if d := reconnectDelay(0, r); d < 0 || d >= reconnectSpread {
			t.Fatalf("the first attempt waited %s, want the spread [0, %s)", d, reconnectSpread)
		}
	}
	ceilings := map[int]time.Duration{1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second,
		4: 16 * time.Second, 5: 30 * time.Second, 6: 30 * time.Second, 40: 30 * time.Second}
	for attempt, ceiling := range ceilings {
		for range 200 {
			d := reconnectDelay(attempt, r)
			if d < ceiling/2 || d >= ceiling {
				t.Fatalf("attempt %d waited %s, want half of %s to the whole of it", attempt, d, ceiling)
			}
		}
	}
}

func TestSummarizeReconnectsGroupsTheWindowsReconnectsByKind(t *testing.T) {
	winStart := time.Unix(1000, 0)
	inWindow := func(at time.Time) bool { return !at.Before(winStart) }
	recs := []reconnectRec{
		{Kind: reconnectLastID, At: winStart.Add(time.Second), Complete: true, ReplayFrames: 3, ReplayBytes: 300, ReplayMs: 20, StartMs: 4, GapMs: 1500},
		{Kind: reconnectLastID, At: winStart.Add(2 * time.Second), AfterRestart: true, FailedAttempts: 2, ReplayFrames: 1, ReplayBytes: 100, ReplayMs: 10, StartMs: 6, GapMs: 7000,
			Refetch: &refetchRec{Requests: 6, Failures: 1, Bytes: 5000, LatencyMs: []float64{10, 20, 30, 40, 50, 60}}},
		{Kind: reconnectStale, At: winStart.Add(3 * time.Second), AfterRestart: true, StartMs: 9, GapMs: 7000,
			Refetch: &refetchRec{Requests: 6, Bytes: 6000, LatencyMs: []float64{15, 25, 35, 45, 55, 65}}},
		{Kind: reconnectLastID, At: winStart.Add(-time.Second), ReplayFrames: 99}, // before the window
	}
	got := summarizeReconnects(recs, 1, []route{{Name: "browser GET /api/v1/inbox"}}, inWindow)

	all := got.Groups["all"]
	if all.Reconnects != 3 || all.Complete != 1 || all.AfterRestart != 2 || all.FailedAttempts != 2 || all.ReplayFramesTotal != 4 || all.ReplayBytesTotal != 400 {
		t.Fatalf("all %+v", all)
	}
	if all.Refetches != 2 || all.RefetchRequests != 12 || all.RefetchFailures != 1 || all.RefetchBytes != 11000 || all.RefetchMs.N != 12 {
		t.Fatalf("all refetches %+v", all)
	}
	if want := (stat{N: 2, P50: 10, P90: 10, P99: 10, Max: 20}); all.ReplayMs != want {
		t.Fatalf("replay ms %+v, want %+v: only the reconnects that replayed a frame", all.ReplayMs, want)
	}
	if all.StartMs.Max != 9 {
		t.Fatalf("events_start ms %+v, want one value per reconnect", all.StartMs)
	}
	stale := got.Groups[reconnectStale]
	if stale.Reconnects != 1 || stale.ReplayFrames.Max != 0 || stale.Refetches != 1 || stale.RefetchBytes != 6000 {
		t.Fatalf("stale %+v", stale)
	}
	if _, ok := got.Groups[reconnectNoID]; ok {
		t.Fatal("a kind with no reconnect has a group")
	}
	if got.Stale != 1 || !slices.Equal(got.RefetchRoutes, []string{"browser GET /api/v1/inbox"}) {
		t.Fatalf("summary %+v", got)
	}
}

// liveServer serves /ws/live: each socket's events subscription gets the events_start its handler
// picks and the frames after it, then the socket ends.
type liveServer struct {
	mu      sync.Mutex
	resumes []string // the resume each subscribe frame sent, "" for none
	respond func(n int, since string) (replayComplete bool, ids []int64)
}

func (s *liveServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	_, data, err := conn.ReadMessage()
	if err != nil {
		return
	}
	var sub struct {
		Resume struct {
			Since string `json:"since"`
		} `json:"resume"`
	}
	if err := json.Unmarshal(data, &sub); err != nil {
		return
	}
	s.mu.Lock()
	n := len(s.resumes)
	s.resumes = append(s.resumes, sub.Resume.Since)
	s.mu.Unlock()
	complete, ids := s.respond(n, sub.Resume.Since)
	start := fmt.Sprintf(`{"type":"events_start","sub":"events","since":"%s","replay_complete":%t}`, sub.Resume.Since, complete)
	if err := conn.WriteMessage(websocket.TextMessage, []byte(start)); err != nil {
		return
	}
	for _, id := range ids {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(liveFrameText(id))); err != nil {
			return
		}
	}
}

func TestResumingSocketsResumeFromTheLastIDAndGoStaleAtTheRestart(t *testing.T) {
	srv := &liveServer{respond: func(n int, since string) (bool, []int64) {
		switch n {
		case 0:
			return false, []int64{41, 42} // the first socket: live frames, then Dispatch ends it
		case 1:
			return true, []int64{42, 43} // resumes since 42; the head was 43; a complete replay
		default:
			return false, nil // the stale reconnect: nothing replayed, no complete replay
		}
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	url, err := liveSocketURL(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	var restarted atomic.Bool
	var heads atomic.Int64
	heads.Store(43)
	var refetched atomic.Int64
	var frames []liveFrame
	var framesMu sync.Mutex
	sockets := &liveSockets{
		url: url, dialer: &websocket.Dialer{}, retry: 10 * time.Millisecond, resume: true, stale: 1,
		head:         func(context.Context) (int64, error) { return heads.Load(), nil },
		restartBegun: restarted.Load,
		refetch: func(context.Context, int) *refetchRec {
			refetched.Add(1)
			return &refetchRec{Requests: 6, Bytes: 600}
		},
		frame: func(f liveFrame) {
			framesMu.Lock()
			frames = append(frames, f)
			framesMu.Unlock()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sockets.run(ctx, 0)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for len(sockets.reconnects()) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no reconnect recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	heads.Store(20000)
	restarted.Store(true)
	for len(sockets.reconnects()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no stale reconnect recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	srv.mu.Lock()
	resumes := slices.Clone(srv.resumes[:3])
	srv.mu.Unlock()
	if want := []string{"", "42", fmt.Sprint(20000 - staleBacklog)}; !slices.Equal(resumes, want) {
		t.Fatalf("resumes %q, want %q", resumes, want)
	}
	recs := sockets.reconnects()
	resumed, stale := recs[0], recs[1]
	if resumed.Kind != reconnectLastID || resumed.Since != 42 || resumed.Head != 43 || !resumed.Complete || resumed.Refetch != nil ||
		resumed.ReplayFrames != 2 || resumed.ReplayBytes != int64(len(liveFrameText(42))+len(liveFrameText(43))) || resumed.AfterRestart {
		t.Fatalf("resumed reconnect %+v", resumed)
	}
	if stale.Kind != reconnectStale || stale.Since != 20000-staleBacklog || stale.Complete || stale.Refetch == nil || stale.Refetch.Requests != 6 || !stale.AfterRestart {
		t.Fatalf("stale reconnect %+v", stale)
	}
	if refetched.Load() != 1 {
		t.Fatalf("refetches %d, want 1: only the reconnect whose subscription did not replay in full refetches", refetched.Load())
	}
	framesMu.Lock()
	defer framesMu.Unlock()
	var replayed []int64
	for _, f := range frames {
		if f.replay {
			replayed = append(replayed, f.id)
		}
	}
	if !slices.Equal(replayed, []int64{42, 43}) {
		t.Fatalf("replayed frames %v, want the resume's 42 and 43, not the first socket's", replayed)
	}
}
