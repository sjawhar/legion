package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
)

// pgClockReads is how many timed reads of Postgres's clock one pg_clock reading takes.
const pgClockReads = 10

// pgClocks is meta.json's pg_clock: Postgres's clock against loadgen's at the step's start
// (collectMeta) and after its window. The delivery latency from commit subtracts a Postgres commit
// time from a loadgen receipt time, so it spans the two clocks.
type pgClocks struct {
	Start *pgClockReading `json:"start,omitempty"`
	End   *pgClockReading `json:"end,omitempty"`
}

// pgClockReading is one reading of Postgres's clock: the round trips of its timed reads, and
// OffsetMs, Postgres's time minus loadgen's local midpoint of the read with the smallest round
// trip (positive when Postgres's clock is ahead). When a read failed it holds Error and no number.
type pgClockReading struct {
	Reads    int      `json:"reads,omitempty"`
	RTTMsMin *float64 `json:"rtt_ms_min,omitempty"`
	RTTMsP50 *float64 `json:"rtt_ms_p50,omitempty"`
	OffsetMs *float64 `json:"offset_ms,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// readPGClock takes one pg_clock reading on the Postgres at url: one connection, then
// pgClockReads reads of select clock_timestamp(), each one round trip (the simple protocol). The
// whole reading has 10 s.
func readPGClock(ctx context.Context, url string) pgClockReading {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return pgClockReading{Error: fmt.Sprintf("connect: %v", err)}
	}
	defer conn.Close(context.Background())
	return measurePGClock(ctx, time.Now, func(ctx context.Context) (time.Time, error) {
		var t time.Time
		err := conn.QueryRow(ctx, "select clock_timestamp()", pgx.QueryExecModeSimpleProtocol).Scan(&t)
		return t, err
	})
}

// measurePGClock times pgClockReads reads of Postgres's clock (read) on the local clock now. The
// read with the smallest round trip bounds the offset best: Postgres read its clock between the
// read's send and its answer, so the offset is Postgres's time minus the midpoint, within half that
// round trip. A failed read ends the reading with Error and no number.
func measurePGClock(ctx context.Context, now func() time.Time, read func(context.Context) (time.Time, error)) pgClockReading {
	const n = pgClockReads
	rtts := make([]float64, 0, n)
	var best, bestOffset time.Duration
	for i := range n {
		sent := now()
		pg, err := read(ctx)
		got := now()
		if err != nil {
			return pgClockReading{Error: fmt.Sprintf("read %d of %d of select clock_timestamp(): %v", i+1, n, err)}
		}
		rtt := got.Sub(sent)
		if i == 0 || rtt < best {
			best, bestOffset = rtt, pg.Sub(sent.Add(rtt/2))
		}
		rtts = append(rtts, durationMs(rtt))
	}
	minMs, p50Ms, offsetMs := durationMs(best), mkStat(rtts).P50, durationMs(bestOffset)
	return pgClockReading{Reads: n, RTTMsMin: &minMs, RTTMsP50: &p50Ms, OffsetMs: &offsetMs}
}

// durationMs is d in milliseconds.
func durationMs(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// logPGClock logs a pg_clock reading, taken at when ("start" or "end").
func logPGClock(lg *log.Logger, when string, r pgClockReading) {
	if r.Error != "" {
		lg.Printf("meta: pg clock at the %s: %s", when, r.Error)
		return
	}
	lg.Printf("meta: pg clock at the %s: Postgres is %+.2f ms from loadgen, round trip %.2f ms min, %.2f ms p50 over %d reads",
		when, *r.OffsetMs, *r.RTTMsMin, *r.RTTMsP50, r.Reads)
}
