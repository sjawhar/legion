package main

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// lockSample is one 20 ms look at Dispatch's database-wide commit-order advisory lock
// (internal/dispatch/events/broker.go:18, taken at :85; key 0x4449535041544348 = classid
// 1145656144, objid 1096041288): who holds it, what that backend is doing, and how many backends
// wait for it. pid is 0 when nobody holds it.
type lockSample struct {
	at      time.Time
	pid     int32
	xact    time.Time
	state   string
	waitEv  string
	query   string
	waiters int
}

type lockProbe struct {
	samples []lockSample
}

const lockProbeQuery = `
select coalesce(h.pid, 0), coalesce(h.xact_start, 'epoch'::timestamptz), coalesce(h.state, ''),
       coalesce(h.wait_event_type || ':' || h.wait_event, ''), coalesce(left(regexp_replace(h.query, '\s+', ' ', 'g'), 90), ''),
       (select count(*) from pg_locks w where w.locktype = 'advisory' and w.classid = 1145656144 and w.objid = 1096041288 and not w.granted)::int
from (select 1) one
left join (
  select a.pid, a.xact_start, a.state, a.wait_event_type, a.wait_event, a.query
  from pg_locks l join pg_stat_activity a on a.pid = l.pid
  where l.locktype = 'advisory' and l.classid = 1145656144 and l.objid = 1096041288 and l.granted
  limit 1) h on true`

// loop samples every 20 ms from from to to over a direct Postgres connection.
func (p *lockProbe) loop(ctx context.Context, url string, from, to time.Time, lg *log.Logger) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		lg.Printf("lockprobe: %v", err)
		return
	}
	defer conn.Close(context.Background())
	select {
	case <-time.After(time.Until(from)):
	case <-ctx.Done():
		return
	}
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	for time.Now().Before(to) {
		var s lockSample
		s.at = time.Now()
		if err := conn.QueryRow(ctx, lockProbeQuery).Scan(&s.pid, &s.xact, &s.state, &s.waitEv, &s.query, &s.waiters); err != nil {
			if ctx.Err() != nil {
				return
			}
		} else {
			p.samples = append(p.samples, s)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// holdDurations segments samples into holds: consecutive samples with one (pid, transaction
// start) are one hold, which a change of holder, or a sample with no holder, ends. A hold's
// duration runs from its first sample to the sample that ended it, in ms; a hold still open at
// the last sample has no known end and is not counted.
func holdDurations(samples []lockSample) []float64 {
	var holds []float64
	var cur *lockSample
	for i := range samples {
		s := &samples[i]
		if cur != nil && (s.pid != cur.pid || !s.xact.Equal(cur.xact)) {
			holds = append(holds, float64(s.at.Sub(cur.at))/1e6)
			cur = nil
		}
		if cur == nil && s.pid != 0 {
			cur = s
		}
	}
	return holds
}

type lockSummary struct {
	Samples       int           `json:"samples"`
	HeldFraction  float64       `json:"held_fraction"`
	Waiters       stat          `json:"waiters"`
	HoldMs        stat          `json:"hold_ms"`
	HoldsObserved int           `json:"holds_observed"`
	HolderDoing   []holderDoing `json:"holder_doing"`
}

type holderDoing struct {
	What  string  `json:"holder_doing"`
	Share float64 `json:"share_of_held_samples"`
}

func summarizeLock(samples []lockSample) lockSummary {
	out := lockSummary{Samples: len(samples), HolderDoing: []holderDoing{}}
	if len(samples) == 0 {
		return out
	}
	held := 0
	var waiters []float64
	what := map[string]int{}
	for _, s := range samples {
		waiters = append(waiters, float64(s.waiters))
		if s.pid == 0 {
			continue
		}
		held++
		key := s.state
		if s.state == "active" {
			key = "active: " + s.query
		} else if s.waitEv != "" {
			key = s.state + " (" + s.waitEv + ")"
		}
		what[strings.TrimSpace(key)]++
	}
	holds := holdDurations(samples)
	out.HeldFraction = float64(held) / float64(len(samples))
	out.Waiters = mkStat(waiters)
	out.HoldMs = mkStat(holds)
	out.HoldsObserved = len(holds)
	for k, v := range what {
		out.HolderDoing = append(out.HolderDoing, holderDoing{What: k, Share: float64(v) / float64(held)})
	}
	sort.Slice(out.HolderDoing, func(i, j int) bool {
		a, b := out.HolderDoing[i], out.HolderDoing[j]
		return a.Share > b.Share || (a.Share == b.Share && a.What < b.What)
	})
	if len(out.HolderDoing) > 12 {
		out.HolderDoing = out.HolderDoing[:12]
	}
	return out
}
