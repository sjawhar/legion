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
