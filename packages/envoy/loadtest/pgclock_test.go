package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"
	"time"
)

// fakePGClock is a local clock and a Postgres whose clock is ahead of it by `ahead` (behind when
// negative). Read i takes rtts[i] of local time, and Postgres reads its clock at the share at[i]
// of that round trip; a read at index failAt fails.
type fakePGClock struct {
	local  time.Time
	ahead  time.Duration
	rtts   []time.Duration
	at     []float64
	failAt int
	i      int
}

func (c *fakePGClock) now() time.Time { return c.local }

func (c *fakePGClock) read(context.Context) (time.Time, error) {
	i := c.i
	c.i++
	if i == c.failAt {
		c.local = c.local.Add(c.rtts[i])
		return time.Time{}, errors.New("connection reset by peer")
	}
	pg := c.local.Add(time.Duration(float64(c.rtts[i]) * c.at[i])).Add(c.ahead)
	c.local = c.local.Add(c.rtts[i])
	return pg, nil
}

func newFakePGClock(ahead time.Duration) *fakePGClock {
	ms := time.Millisecond
	return &fakePGClock{
		local: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC), ahead: ahead, failAt: -1,
		// The fastest read (2 ms) is read at its midpoint; the slower ones far from theirs, so an
		// offset taken from any other read is off by a millisecond or more.
		rtts: []time.Duration{8 * ms, 2 * ms, 6 * ms, 4 * ms, 9 * ms, 5 * ms, 7 * ms, 3 * ms, 10 * ms, 11 * ms},
		at:   []float64{0.95, 0.5, 0.05, 0.9, 0.1, 0.95, 0.05, 0.9, 0.1, 0.95},
	}
}

// TestPGClockOffsetIsPostgresMinusTheMidpointOfTheFastestRead: the offset is positive when
// Postgres's clock is ahead and negative when it is behind, and comes from the read with the
// smallest round trip.
func TestPGClockOffsetIsPostgresMinusTheMidpointOfTheFastestRead(t *testing.T) {
	for _, c := range []struct {
		ahead time.Duration
		want  float64
	}{{250 * time.Millisecond, 250}, {-1500 * time.Millisecond, -1500}, {0, 0}} {
		clk := newFakePGClock(c.ahead)
		r := measurePGClock(context.Background(), clk.now, clk.read)
		if r.Error != "" || r.OffsetMs == nil || r.RTTMsMin == nil || r.RTTMsP50 == nil {
			t.Fatalf("Postgres %s ahead: reading %+v, want every number and no error", c.ahead, r)
		}
		if *r.OffsetMs != c.want {
			t.Errorf("Postgres %s ahead: offset_ms %v, want %v", c.ahead, *r.OffsetMs, c.want)
		}
		// Round trips 2..11 ms: the smallest 2, the lower-nearest-rank median 6.
		if *r.RTTMsMin != 2 || *r.RTTMsP50 != 6 || r.Reads != 10 {
			t.Errorf("rtt_ms_min %v, rtt_ms_p50 %v over %d reads; want 2, 6 over 10", *r.RTTMsMin, *r.RTTMsP50, r.Reads)
		}
	}
}

// TestPGClockFailedReadGivesAnErrorAndNoNumber: one failed read leaves the reading with its error
// and no offset or round trip, in meta.json too.
func TestPGClockFailedReadGivesAnErrorAndNoNumber(t *testing.T) {
	clk := newFakePGClock(time.Second)
	clk.failAt = 3
	r := measurePGClock(context.Background(), clk.now, clk.read)
	if !strings.Contains(r.Error, "read 4 of 10") || !strings.Contains(r.Error, "connection reset by peer") {
		t.Fatalf("error %q does not name the failed read", r.Error)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"offset_ms", "rtt_ms_min", "rtt_ms_p50"} {
		if strings.Contains(string(b), key) {
			t.Errorf("a failed reading holds %s: %s", key, b)
		}
	}
}

// TestMetaRecordsAFailedPGClockRead: collectMeta takes pg_clock.start on -pg; a Postgres that does
// not answer gives start.error and no number.
func TestMetaRecordsAFailedPGClockRead(t *testing.T) {
	t.Setenv("LOADTEST_ROOT", t.TempDir())
	var logged bytes.Buffer
	cfg := runConfig{boxBusyBefore: 0, ep: endpoints{pg: "postgres://postgres@127.0.0.1:1/dispatch?sslmode=disable", pids: map[string]int{}}}
	meta := collectMeta(context.Background(), cfg, log.New(&logged, "", 0))
	if meta.PGClock == nil || meta.PGClock.Start == nil || meta.PGClock.Start.Error == "" || meta.PGClock.Start.OffsetMs != nil {
		t.Fatalf("pg_clock %+v: want start with an error and no offset", meta.PGClock)
	}
	if meta.PGClock.End != nil {
		t.Errorf("collectMeta took pg_clock.end, which is read after the window")
	}
	if !strings.Contains(logged.String(), "meta: pg clock at the start:") {
		t.Errorf("the failed reading is not logged:\n%s", logged.String())
	}
}
