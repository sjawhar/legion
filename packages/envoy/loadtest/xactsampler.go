package main

import (
	"cmp"
	"context"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// xactRow is one backend of Dispatch's database that is in a transaction, in one look at
// pg_stat_activity: whether it holds a transaction id (backend_xid is set), its state and the
// first 80 characters of its current or last statement.
type xactRow struct {
	pid    int32
	xact   time.Time // xact_start
	hasXID bool
	state  string
	query  string
}

// xactSample is one look at pg_stat_activity: every backend of the database in a transaction.
type xactSample struct {
	at   time.Time
	rows []xactRow
}

// xactSamplerQuery reads pg_stat_activity once per sample, for database $1: the client backends in
// a transaction, the sampler's own backend excluded. It reads no pg_locks, whose cost the 20 ms
// ungranted-lock sampler had (ungrantedLockInterval).
const xactSamplerQuery = `
select pid, xact_start, backend_xid is not null, coalesce(state, ''), coalesce(left(query, 80), '')
from pg_stat_activity
where datname = $1 and backend_type = 'client backend' and xact_start is not null and pid <> pg_backend_pid()`

// defaultXactInterval is how often the transaction-id hold sampler reads (-xact-interval).
const defaultXactInterval = 10 * time.Millisecond

// xactLongest is how many of the longest transaction-id holders the summary names.
const xactLongest = 5

// xactSampler samples how long Dispatch's transactions hold a transaction id. It keeps no samples:
// its tracker reduces each one as it arrives.
type xactSampler struct {
	tr     *xactTracker
	errors int // failed reads (pollPG)
}

// loop samples every tracker interval from from to to over a direct Postgres connection, reading
// the backends of database db.
func (p *xactSampler) loop(ctx context.Context, url, db string, from, to time.Time, lg *log.Logger) {
	var rows []xactRow // reused: the tracker keeps no row
	p.errors = pollPG(ctx, url, from, to, p.tr.interval, "xactsampler", lg, func(ctx context.Context, conn *pgx.Conn) error {
		at := time.Now()
		res, err := conn.Query(ctx, xactSamplerQuery, db)
		if err != nil {
			return err
		}
		defer res.Close()
		rows = rows[:0]
		for res.Next() {
			var r xactRow
			if err := res.Scan(&r.pid, &r.xact, &r.hasXID, &r.state, &r.query); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		if err := res.Err(); err != nil {
			return err
		}
		p.tr.add(xactSample{at: at, rows: rows})
		return nil
	})
}

// xactKey is one transaction: its backend and its start.
type xactKey struct {
	pid   int32
	start int64 // xact_start, Unix ns
}

// xactTrack is what the samples showed of one transaction.
type xactTrack struct {
	start              time.Time // xact_start
	last               time.Time // its last sample
	held               bool      // a sample showed it with a transaction id
	firstXID, lastXID  time.Time // its first and last samples with a transaction id
	idleXID, idleNoXID int       // its samples idle in transaction, with and without one
	query              string    // its statement at its last sample
	seen               int       // the sample that last showed it
}

// xactTracker reduces the samples as they arrive. A transaction ends at the first sample that does
// not show it; the tracker then keeps it only when it held a transaction id.
type xactTracker struct {
	interval    time.Duration
	samples     int
	first, last time.Time
	live        map[xactKey]*xactTrack
	ended       []*xactTrack // the ended transactions that held a transaction id
}

func newXactTracker(interval time.Duration) *xactTracker {
	return &xactTracker{interval: interval, live: map[xactKey]*xactTrack{}}
}

func (t *xactTracker) add(s xactSample) {
	t.samples++
	if t.samples == 1 {
		t.first = s.at
	}
	t.last = s.at
	for _, r := range s.rows {
		k := xactKey{r.pid, r.xact.UnixNano()}
		x := t.live[k]
		if x == nil {
			x = &xactTrack{start: r.xact}
			t.live[k] = x
		}
		x.last, x.query, x.seen = s.at, r.query, t.samples
		if r.hasXID {
			if !x.held {
				x.held, x.firstXID = true, s.at
			}
			x.lastXID = s.at
		}
		if r.state == "idle in transaction" {
			if r.hasXID {
				x.idleXID++
			} else {
				x.idleNoXID++
			}
		}
	}
	for k, x := range t.live {
		if x.seen == t.samples {
			continue
		}
		delete(t.live, k)
		if x.held {
			t.ended = append(t.ended, x)
		}
	}
}

type xactSummary struct {
	// IntervalMs is the sampler's interval; 0 when -xact-interval 0 turned it off.
	IntervalMs float64 `json:"interval_ms"`
	Samples    int     `json:"samples"`
	// Errors counts the sampler's failed reads, a failed connect included; samples miss them.
	Errors      int     `json:"errors"`
	SamplesPerS float64 `json:"samples_per_s"`
	// XIDHolders counts the transactions that ended in the window and that a sample showed holding
	// a transaction id; CommittedEvents is the run's committed in-window writes, for comparison.
	XIDHolders      int `json:"xid_holders"`
	CommittedEvents int `json:"committed_events"`
	// Over XIDHolders, one value each: the transaction-id hold, the transaction's age at its last
	// sample, and its time idle in transaction with and without a transaction id.
	XIDHoldMs           stat        `json:"xid_hold_ms"`
	XactMs              stat        `json:"xact_ms"`
	IdleInXactWithXIDMs stat        `json:"idle_in_xact_with_xid_ms"`
	IdleInXactNoXIDMs   stat        `json:"idle_in_xact_without_xid_ms"`
	LongestXIDHolders   []xidHolder `json:"longest_xid_holders"`
}

// xidHolder is one transaction that held a transaction id, and its statement at its last sample.
type xidHolder struct {
	HoldMs float64 `json:"xid_hold_ms"`
	XactMs float64 `json:"xact_ms"`
	Query  string  `json:"last_query"`
}

// summary reduces what the tracker saw; failedReads is the sampler's failed reads. A transaction's
// hold runs from its first to its last sample with a transaction id, plus one interval; its
// duration from xact_start to its last sample. A transaction still open at the last sample has no
// known end and is not counted.
func (t *xactTracker) summary(failedReads, committedEvents int) xactSummary {
	out := xactSummary{IntervalMs: durMs(t.interval), Samples: t.samples, Errors: failedReads, CommittedEvents: committedEvents,
		LongestXIDHolders: []xidHolder{}}
	if span := t.last.Sub(t.first); t.samples > 1 && span > 0 {
		out.SamplesPerS = float64(t.samples-1) / span.Seconds()
	}
	holders := make([]xidHolder, len(t.ended))
	var hold, xact, idleXID, idleNoXID []float64
	for i, x := range t.ended {
		holders[i] = xidHolder{HoldMs: durMs(x.lastXID.Sub(x.firstXID) + t.interval), XactMs: durMs(x.last.Sub(x.start)),
			Query: strings.Join(strings.Fields(x.query), " ")}
		hold = append(hold, holders[i].HoldMs)
		xact = append(xact, holders[i].XactMs)
		idleXID = append(idleXID, durMs(time.Duration(x.idleXID)*t.interval))
		idleNoXID = append(idleNoXID, durMs(time.Duration(x.idleNoXID)*t.interval))
	}
	out.XIDHolders = len(holders)
	out.XIDHoldMs, out.XactMs = mkStat(hold), mkStat(xact)
	out.IdleInXactWithXIDMs, out.IdleInXactNoXIDMs = mkStat(idleXID), mkStat(idleNoXID)
	// Longest hold first; transactions that end at one sample come in map order, so ties are broken
	// by the longer transaction, then the statement.
	slices.SortFunc(holders, func(a, b xidHolder) int {
		return cmp.Or(cmp.Compare(b.HoldMs, a.HoldMs), cmp.Compare(b.XactMs, a.XactMs), strings.Compare(a.Query, b.Query))
	})
	out.LongestXIDHolders = append(out.LongestXIDHolders, holders[:min(xactLongest, len(holders))]...)
	return out
}

// durMs is d in milliseconds.
func durMs(d time.Duration) float64 { return float64(d) / 1e6 }
