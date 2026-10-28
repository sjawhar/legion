package main

import (
	"io"
	"strings"
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
	var got []sseFrame
	n, err := readSSE(strings.NewReader(stream), nonce, now, func(f sseFrame) { got = append(got, f) })
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

func TestSummarizeSSE(t *testing.T) {
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
	frames := []sseFrame{
		{at: 1150 * msNs, id: 10, marker: 1, created: 1040 * msNs},
		{at: 1160 * msNs, id: 11, marker: 1, created: 1040 * msNs},
		{at: 2600 * msNs, id: 20, marker: 2, created: 2500 * msNs},
		{at: 2610 * msNs, id: 30, marker: 3, created: 2500 * msNs, outOfOrder: true},
		{at: 2700 * msNs, id: 31}, // no marker, no created_at
	}

	got := summarizeSSE(frames, writes, commits)

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
