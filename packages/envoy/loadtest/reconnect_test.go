package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func frameText(id int64) string {
	return fmt.Sprintf("id: %d\nevent: comment.created\ndata: {\"id\":%d,\"type\":\"comment.created\"}\n\n", id, id)
}

func TestReplayTallyCountsTheFramesAtOrBelowTheHeadWithTheirBytes(t *testing.T) {
	stream := ": open\n\n" + frameText(5) + ": heartbeat\n\n" + frameText(6) + frameText(7)
	tally := replayTally{head: 6}
	var replayed []int64
	if _, err := readSSE(strings.NewReader(stream), 0, time.Now, func(f sseFrame) {
		if tally.add(f) {
			replayed = append(replayed, f.id)
		}
	}); err == nil {
		t.Fatal("readSSE ended without the body's end")
	}
	if !slices.Equal(replayed, []int64{5, 6}) {
		t.Fatalf("replayed %v, want 5 and 6 (at or below head 6)", replayed)
	}
	if want := int64(len(frameText(5)) + len(frameText(6))); tally.frames != 2 || tally.bytes != want {
		t.Fatalf("tally %d frames %d bytes, want 2 frames %d bytes (comment lines are not frame bytes)", tally.frames, tally.bytes, want)
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
	var got []time.Duration
	for attempt := range 7 {
		got = append(got, reconnectDelay(attempt))
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	if !slices.Equal(got, want) {
		t.Fatalf("delays %v, want %v", got, want)
	}
}

func TestSummarizeReconnectsGroupsTheWindowsReconnectsByKind(t *testing.T) {
	winStart := time.Unix(1000, 0)
	inWindow := func(at time.Time) bool { return !at.Before(winStart) }
	recs := []reconnectRec{
		{Kind: reconnectLastID, At: winStart.Add(time.Second), Complete: true, ReplayFrames: 3, ReplayBytes: 300, ReplayMs: 20, TTFBMs: 4, GapMs: 1500},
		{Kind: reconnectLastID, At: winStart.Add(2 * time.Second), AfterRestart: true, FailedAttempts: 2, ReplayFrames: 1, ReplayBytes: 100, ReplayMs: 10, TTFBMs: 6, GapMs: 7000,
			Refetch: &refetchRec{Requests: 6, Failures: 1, Bytes: 5000, LatencyMs: []float64{10, 20, 30, 40, 50, 60}}},
		{Kind: reconnectStale, At: winStart.Add(3 * time.Second), AfterRestart: true, TTFBMs: 9, GapMs: 7000,
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

// sseServer serves /api/v1/events: each request gets the frames its handler picks, then the
// connection ends.
type sseServer struct {
	mu       sync.Mutex
	requests []string // the raw query of each request
	respond  func(n int, query string) (complete bool, ids []int64)
}

func (s *sseServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	n := len(s.requests)
	s.requests = append(s.requests, r.URL.RawQuery)
	s.mu.Unlock()
	complete, ids := s.respond(n, r.URL.RawQuery)
	if complete {
		w.Header().Set(replayHeader, replayComplete)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, ": open\n\n")
	for _, id := range ids {
		fmt.Fprint(w, frameText(id))
	}
}

func TestResumingStreamsResumeFromTheLastIDAndGoStaleAtTheRestart(t *testing.T) {
	srv := &sseServer{respond: func(n int, query string) (bool, []int64) {
		switch n {
		case 0:
			return false, []int64{41, 42} // first connection: live frames, then the server ends it
		case 1:
			return true, []int64{42, 43} // resumes since 42; the head was 43; a complete replay
		default:
			return false, nil // stale reconnect: nothing replayed, no complete header
		}
	}}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	var restarted atomic.Bool
	var heads atomic.Int64
	heads.Store(43)
	var refetched atomic.Int64
	var frames []sseFrame
	var framesMu sync.Mutex
	streams := &sseStreams{
		base: ts.URL, client: ts.Client(), resume: true, stale: 1,
		head:         func(context.Context) (int64, error) { return heads.Load(), nil },
		restartBegun: restarted.Load,
		refetch: func(context.Context, int) *refetchRec {
			refetched.Add(1)
			return &refetchRec{Requests: 6, Bytes: 600}
		},
		frame: func(f sseFrame) {
			framesMu.Lock()
			frames = append(frames, f)
			framesMu.Unlock()
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		streams.run(ctx, 0)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for len(streams.reconnects()) < 1 {
		if time.Now().After(deadline) {
			t.Fatal("no reconnect recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	heads.Store(20000)
	restarted.Store(true)
	for len(streams.reconnects()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("no stale reconnect recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	srv.mu.Lock()
	queries := slices.Clone(srv.requests[:3])
	srv.mu.Unlock()
	if want := []string{"", "since=42", fmt.Sprintf("since=%d", 20000-staleBacklog)}; !slices.Equal(queries, want) {
		t.Fatalf("queries %q, want %q", queries, want)
	}
	recs := streams.reconnects()
	resumed, stale := recs[0], recs[1]
	if resumed.Kind != reconnectLastID || resumed.Since != 42 || resumed.Head != 43 || !resumed.Complete || resumed.Refetch != nil ||
		resumed.ReplayFrames != 2 || resumed.ReplayBytes != int64(len(frameText(42))+len(frameText(43))) || resumed.AfterRestart {
		t.Fatalf("resumed reconnect %+v", resumed)
	}
	if stale.Kind != reconnectStale || stale.Since != 20000-staleBacklog || stale.Complete || stale.Refetch == nil || stale.Refetch.Requests != 6 || !stale.AfterRestart {
		t.Fatalf("stale reconnect %+v", stale)
	}
	if refetched.Load() != 1 {
		t.Fatalf("refetches %d, want 1: only the reconnect without a complete replay refetches", refetched.Load())
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
		t.Fatalf("replayed frames %v, want the resume's 42 and 43, not the first connection's", replayed)
	}
}
