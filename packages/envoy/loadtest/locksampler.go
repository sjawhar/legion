package main

import (
	"cmp"
	"context"
	"log"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// lockWait is one ungranted heavyweight lock in pg_locks: what the waiter runs and what its
// blocker runs (a granted holder of the same lock, ungrantedLocksQuery). Waits group by lock type, target and mode. The
// target is a label for the two locks this harness tracks (commit_order_lock: Dispatch's
// commit-order advisory lock; notify_commit_lock: Postgres's NOTIFY commit lock, the database-0
// object lock PreCommit_Notify takes), a relation name, row_wait for a transaction-id wait (a row
// lock held by another transaction), and the raw ids otherwise.
type lockWait struct {
	locktype, mode, target string
	relDB                  string // a relation or tuple outside the connection's database: its database,
	relOID                 uint32 // and relation oid, which resolveRelations names
	query                  string
	blockerQuery           string
}

// ungrantedSample is one look at every ungranted lock.
type ungrantedSample struct {
	waits []lockWait
}

type lockSampler struct {
	samples []ungrantedSample
	errors  int // failed reads (pollPG)
}

// ungrantedLockInterval is how often the sampler reads. Each read costs Postgres CPU on the box it
// measures: read every 20 ms, with ~45 waiters at 1x, the query took 75-180 ms of one backend per
// read, and 1x committed 0.29-0.82 events/s against 1.63-1.81 without the sampler (2026-10-27,
// same build and seed). Once a second gives 160 reads in a 180 s window.
const ungrantedLockInterval = time.Second

// ungrantedLocksQuery reads pg_locks once per sample, in one materialized CTE, and names each
// waiter's blocker from that same scan: a granted holder of the same lock (the lowest pid). It
// never calls pg_blocking_pids, which takes exclusive access to the lock manager's shared state.
// $1 and $2 are the commit-order lock's classid and objid.
const ungrantedLocksQuery = `
with l as materialized (
  select locktype, mode, pid, granted, database, relation, page, tuple, virtualxid, transactionid,
         classid, objid, objsubid
  from pg_locks
),
w as (
  select l.*,
    (select h.pid from l h
     where h.granted and h.pid is distinct from l.pid and h.locktype = l.locktype
       and h.database is not distinct from l.database and h.relation is not distinct from l.relation
       and h.page is not distinct from l.page and h.tuple is not distinct from l.tuple
       and h.virtualxid is not distinct from l.virtualxid
       and h.transactionid is not distinct from l.transactionid
       and h.classid is not distinct from l.classid and h.objid is not distinct from l.objid
       and h.objsubid is not distinct from l.objsubid
     order by h.pid limit 1) as blocker
  from l
  where not l.granted
)
select w.locktype, w.mode,
  case
    when w.locktype = 'advisory' and w.classid = $1 and w.objid = $2 then 'commit_order_lock'
    when w.locktype = 'advisory' then w.classid::text || ':' || w.objid::text
    when w.locktype = 'object' and w.classid = 'pg_database'::regclass and w.objid = 0 then 'notify_commit_lock'
    when w.locktype in ('relation', 'tuple') and w.database in (0, (select oid from pg_database where datname = current_database())) then w.relation::regclass::text
    when w.locktype in ('relation', 'tuple') then ''
    when w.locktype = 'transactionid' then 'row_wait'
    else concat_ws(':', w.database, w.relation, w.page, w.tuple, w.virtualxid, w.transactionid, w.classid, w.objid, w.objsubid)
  end,
  coalesce(d.datname, ''), coalesce(w.relation, 0),
  coalesce(left(regexp_replace(a.query, '\s+', ' ', 'g'), 80), ''),
  coalesce(left(regexp_replace(b.query, '\s+', ' ', 'g'), 80), '')
from w
left join pg_database d on d.oid = w.database
left join pg_stat_activity a on a.pid = w.pid
left join pg_stat_activity b on b.pid = w.blocker`

// loop samples every ungrantedLockInterval from from to to over a direct Postgres connection.
func (p *lockSampler) loop(ctx context.Context, url string, from, to time.Time, lg *log.Logger) {
	p.errors = pollPG(ctx, url, from, to, ungrantedLockInterval, "locksampler", lg, func(ctx context.Context, conn *pgx.Conn) error {
		rows, err := conn.Query(ctx, ungrantedLocksQuery, commitOrderLockClassID, commitOrderLockObjID)
		if err != nil {
			return err
		}
		defer rows.Close()
		var s ungrantedSample
		for rows.Next() {
			var w lockWait
			if err := rows.Scan(&w.locktype, &w.mode, &w.target, &w.relDB, &w.relOID, &w.query, &w.blockerQuery); err != nil {
				return err
			}
			s.waits = append(s.waits, w)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		p.samples = append(p.samples, s)
		return nil
	})
}

// resolveRelations names the relations the sampling connection could not, because they live in
// another database: one short connection per database of the Postgres server at url, after the
// load has ended. A relation it cannot name gets the target <database>:<oid>.
func (p *lockSampler) resolveRelations(url string) {
	byDB := map[string]map[uint32]string{}
	for _, s := range p.samples {
		for _, w := range s.waits {
			if w.target == "" && w.relDB != "" && w.relOID != 0 {
				if byDB[w.relDB] == nil {
					byDB[w.relDB] = map[uint32]string{}
				}
				byDB[w.relDB][w.relOID] = ""
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for db, names := range byDB {
		cfg, err := pgx.ParseConfig(url)
		if err != nil {
			continue
		}
		cfg.Database = db
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			continue
		}
		oids := make([]uint32, 0, len(names))
		for o := range names {
			oids = append(oids, o)
		}
		rows, err := conn.Query(ctx, `select r, r::regclass::text from unnest($1::oid[]) r`, oids)
		if err == nil {
			for rows.Next() {
				var o uint32
				var name string
				if rows.Scan(&o, &name) == nil {
					names[o] = name
				}
			}
			rows.Close()
		}
		conn.Close(ctx)
	}
	for i := range p.samples {
		for j := range p.samples[i].waits {
			w := &p.samples[i].waits[j]
			if w.target == "" && w.relDB != "" {
				if name := byDB[w.relDB][w.relOID]; name != "" {
					w.target = name
				} else {
					w.target = w.relDB + ":" + strconv.FormatUint(uint64(w.relOID), 10)
				}
			}
		}
	}
}

type ungrantedSummary struct {
	Samples int `json:"samples"`
	// Errors counts the sampler's failed reads, a failed connect included; samples miss them.
	Errors int `json:"errors"`
	// ShareWithAny is the share of samples that saw at least one ungranted lock.
	ShareWithAny float64          `json:"share_of_samples_with_any_ungranted"`
	Groups       []ungrantedGroup `json:"groups"`
}

// ungrantedGroup is the waits on one (lock type, target, mode).
type ungrantedGroup struct {
	LockType string `json:"locktype"`
	Target   string `json:"target"`
	Mode     string `json:"mode"`
	// TopPairs are the three most frequent (waiter query, first blocker query) pairs over the
	// group's waits.
	TopPairs []waitPair `json:"top_pairs"`
	// WaitersP50 and WaitersMax are over every sample, 0 where the group had no waiter.
	WaitersP50      float64 `json:"waiters_p50"`
	WaitersMax      float64 `json:"waiters_max"`
	ShareWithWaiter float64 `json:"share_of_samples_with_waiter"`
}

type waitPair struct {
	WaiterQuery  string `json:"waiter_query"`
	BlockerQuery string `json:"blocker_query"`
	Count        int    `json:"count"`
}

// summarizeUngranted groups the samples' waits by (lock type, target, mode), the group that has
// waiters in the most samples first; failedReads is the sampler's failed reads.
func summarizeUngranted(samples []ungrantedSample, failedReads int) ungrantedSummary {
	out := ungrantedSummary{Samples: len(samples), Errors: failedReads, Groups: []ungrantedGroup{}}
	if len(samples) == 0 {
		return out
	}
	type groupKey struct{ locktype, target, mode string }
	type pairKey struct{ waiter, blocker string }
	type group struct {
		key        groupKey
		withWaiter int
		perSample  []float64 // waiters in each sample, 0 where the group had none
		pairs      map[pairKey]int
	}
	groups := map[groupKey]*group{}
	anyWaiter := 0
	for i, s := range samples {
		if len(s.waits) > 0 {
			anyWaiter++
		}
		for _, w := range s.waits {
			k := groupKey{w.locktype, w.target, w.mode}
			g := groups[k]
			if g == nil {
				g = &group{key: k, perSample: make([]float64, len(samples)), pairs: map[pairKey]int{}}
				groups[k] = g
			}
			if g.perSample[i] == 0 {
				g.withWaiter++
			}
			g.perSample[i]++
			g.pairs[pairKey{w.query, w.blockerQuery}]++
		}
	}
	list := make([]*group, 0, len(groups))
	for _, g := range groups {
		list = append(list, g)
	}
	slices.SortFunc(list, func(a, b *group) int {
		return cmp.Or(cmp.Compare(b.withWaiter, a.withWaiter),
			cmp.Compare(a.key.locktype, b.key.locktype), cmp.Compare(a.key.target, b.key.target), cmp.Compare(a.key.mode, b.key.mode))
	})
	for _, g := range list {
		pairs := make([]waitPair, 0, len(g.pairs))
		for k, n := range g.pairs {
			pairs = append(pairs, waitPair{WaiterQuery: k.waiter, BlockerQuery: k.blocker, Count: n})
		}
		slices.SortFunc(pairs, func(a, b waitPair) int {
			return cmp.Or(cmp.Compare(b.Count, a.Count), cmp.Compare(a.WaiterQuery, b.WaiterQuery), cmp.Compare(a.BlockerQuery, b.BlockerQuery))
		})
		ws := mkStat(g.perSample)
		out.Groups = append(out.Groups, ungrantedGroup{
			LockType: g.key.locktype, Target: g.key.target, Mode: g.key.mode, TopPairs: pairs[:min(3, len(pairs))],
			WaitersP50: ws.P50, WaitersMax: ws.Max,
			ShareWithWaiter: float64(g.withWaiter) / float64(len(samples)),
		})
	}
	out.ShareWithAny = float64(anyWaiter) / float64(len(samples))
	return out
}
