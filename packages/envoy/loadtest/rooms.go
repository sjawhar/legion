package main

// The document rooms' measurements (README "Document rooms"): the round trips Dispatch's
// connections made and how many were room work (the proxy's wire counts, proxy.go), the
// connections the rooms held (a 100 ms sampler of pg_locks and pg_stat_activity), the append
// batches Dispatch reports on /debug/vars (dispatch_rooms), and what whole-markdown versions cost
// (artifact_versions' growth and the proxy's timing of its inserts).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// roomConnectionsQuery counts, in one read, the backends of database dispatch that hold rooms'
// connections: those that hold or wait for a single-key advisory lock other than the events
// commit-order lock ($1, $2: its classid and objid), and those not idle whose statement names
// doc_updates; then how many of them are not on Dispatch's room pools (dispatch-room-appends,
// dispatch-rooms), the shared pool's share. loadgen's own backends (application_name loadgen-*)
// are left out.
const roomConnectionsQuery = `
with held as (
  select l.pid from pg_locks l
  where l.locktype = 'advisory' and l.objsubid = 1
    and l.database = (select oid from pg_database where datname = current_database())
    and not (l.classid = $1 and l.objid = $2)
  union
  select a.pid from pg_stat_activity a
  where a.datname = current_database() and a.state is distinct from 'idle' and a.query ilike '%doc\_updates%')
select count(*)::int,
       (count(*) filter (where coalesce(a.application_name, '') not in ('dispatch-room-appends', 'dispatch-rooms')))::int
from held h left join pg_stat_activity a on a.pid = h.pid
where coalesce(a.application_name, '') not like 'loadgen-%'`

// roomSample is one look at the rooms' connections: the backends counted, and those of them on
// the shared pool.
type roomSample struct {
	held, shared int
}

type roomSampler struct {
	samples []roomSample
	errors  int
}

// loop samples every 100 ms from from to to over its own direct connection.
func (s *roomSampler) loop(ctx context.Context, url string, from, to time.Time, lg *log.Logger) {
	conn, err := connectAs(ctx, url, "loadgen-room-sampler")
	if err != nil {
		lg.Printf("room sampler: %v", err)
		return
	}
	defer conn.Close(context.Background())
	if !sleepUntil(ctx, from) {
		return
	}
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for time.Now().Before(to) {
		var r roomSample
		if err := conn.QueryRow(ctx, roomConnectionsQuery, commitOrderLockClassID, commitOrderLockObjID).Scan(&r.held, &r.shared); err != nil {
			if ctx.Err() != nil {
				return
			}
			if s.errors++; s.errors <= 3 {
				lg.Printf("room sampler: %v", err)
			}
		} else {
			s.samples = append(s.samples, r)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// connectAs opens a direct connection over url under application name name. loadgen's own
// connections take a name starting loadgen-, which roomConnectionsQuery leaves out.
func connectAs(ctx context.Context, url, name string) (*pgx.Conn, error) {
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	config.RuntimeParams["application_name"] = name
	return pgx.ConnectConfig(ctx, config)
}

// dispatchRooms is the part of Dispatch's dispatch_rooms variable (/debug/vars) the tables use:
// what the rooms' browser append transactions committed.
type dispatchRooms struct {
	AppendBatches    uint64            `json:"append_batches"`
	AppendedUpdates  uint64            `json:"appended_updates"`
	AppendRoundTrips uint64            `json:"append_round_trips"`
	BatchSizes       map[string]uint64 `json:"batch_sizes"`
}

// windowReading is what the rooms' counters read at one end of the window. Each source's error
// says why it has no value.
type windowReading struct {
	proxy    *proxyStats
	proxyErr string
	rooms    *dispatchRooms
	roomsErr string
	// versionBytes and versionRows are pg_total_relation_size('artifact_versions') and its rows.
	versionBytes, versionRows int64
	versionsAt                time.Time
	versionsErr               string
}

// notServed is the reason given for a build whose profiler serves no dispatch_rooms.
const notServed = "not served by this build"

// readWindow reads the proxy's wire counts, Dispatch's dispatch_rooms and artifact_versions'
// size, in that order.
func readWindow(ctx context.Context, ep endpoints) windowReading {
	var r windowReading
	switch {
	case ep.pgDirect:
		r.proxyErr = "Dispatch reaches Postgres directly (pg_direct), not through the proxy"
	case ep.proxyStats == "":
		r.proxyErr = "no proxy wire counts to read: -allow-foreign, or a stack.json from before proxy_stats"
	default:
		var s proxyStats
		if err := getJSON(ctx, ep.proxyStats+"/stats", &s); err != nil {
			r.proxyErr = "the proxy serves no wire counts: " + err.Error()
		} else {
			r.proxy = &s
		}
	}
	var vars map[string]json.RawMessage
	if err := getJSON(ctx, ep.pprof+"/debug/vars", &vars); err != nil {
		r.roomsErr = notServed + " (" + err.Error() + ")"
	} else if raw, ok := vars["dispatch_rooms"]; !ok {
		r.roomsErr = notServed
	} else {
		var rooms dispatchRooms
		if err := json.Unmarshal(raw, &rooms); err != nil {
			r.roomsErr = "dispatch_rooms: " + err.Error()
		} else {
			r.rooms = &rooms
		}
	}
	pgCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(pgCtx, ep.pg)
	if err != nil {
		r.versionsErr = err.Error()
		return r
	}
	defer conn.Close(context.Background())
	if err := conn.QueryRow(pgCtx, `select pg_total_relation_size('artifact_versions'), (select count(*) from artifact_versions)`).Scan(&r.versionBytes, &r.versionRows); err != nil {
		r.versionsErr = err.Error()
	}
	r.versionsAt = time.Now()
	return r
}

func getJSON(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := shortClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %d %s", url, resp.StatusCode, trunc(b))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type roomsSummary struct {
	RoundTrips      *roundTripsSummary `json:"round_trips"`
	RoundTripsNote  string             `json:"round_trips_note,omitempty"`
	Connections     *roomConnections   `json:"connections"`
	ConnectionsNote string             `json:"connections_note,omitempty"`
	// Batches is dispatch_rooms' append figures, the window's end minus its start.
	Batches     *dispatchRooms  `json:"batches"`
	BatchesNote string          `json:"batches_note,omitempty"`
	Versions    versionsSummary `json:"versions"`
}

// roundTripsSummary is the proxy's counts between its readings at the window's start and end
// (IntervalS apart).
type roundTripsSummary struct {
	IntervalS           float64 `json:"interval_s"`
	All                 int64   `json:"all"`
	Room                int64   `json:"room"`
	Flush               int64   `json:"flush"`
	UnparsedConnections int64   `json:"unparsed_connections"`
	// RoomPerUpdate is Room divided by the editor updates sent in the window; null without any.
	RoomPerUpdate *float64 `json:"room_per_update"`
}

// roomConnections is the 100 ms sampler's counts: Held per sample, and SharedPoolShare, the share
// of all counted backends, over every sample, that were on the shared pool.
type roomConnections struct {
	Samples         int      `json:"samples"`
	Errors          int      `json:"errors"`
	Held            stat     `json:"held"`
	SharedPoolShare *float64 `json:"shared_pool_share"`
}

// versionsSummary is the measurement of whole-markdown versions in the window.
type versionsSummary struct {
	// Bytes is artifact_versions' growth (pg_total_relation_size: heap, TOAST and indexes) and
	// BytesPerS that over the seconds between the two reads; Written its rows' growth.
	Bytes     *int64   `json:"bytes"`
	BytesPerS *float64 `json:"bytes_per_s"`
	Written   *int64   `json:"written"`
	Note      string   `json:"note,omitempty"`
	// InsertCount and InsertTotalMs are the proxy's version-insert round trips and their time in
	// all; InsertMs their distribution from the proxy's ring, which held InsertCount minus
	// InsertSamplesMissing of them.
	InsertCount          *int64   `json:"insert_count"`
	InsertTotalMs        *float64 `json:"insert_total_ms"`
	InsertMs             stat     `json:"insert_ms"`
	InsertSamplesMissing int64    `json:"insert_samples_missing"`
}

// summarizeRooms reduces the two window readings and the room sampler's samples (sampler nil when
// it did not run). sent is the editor updates sent in the window.
func summarizeRooms(start, end windowReading, sampler *roomSampler, sent int) *roomsSummary {
	out := &roomsSummary{}
	switch {
	case start.proxy == nil:
		out.RoundTripsNote = start.proxyErr
	case end.proxy == nil:
		out.RoundTripsNote = end.proxyErr
	default:
		a, b := start.proxy, end.proxy
		rt := &roundTripsSummary{
			IntervalS: b.At.Sub(a.At).Seconds(), All: b.RoundTrips - a.RoundTrips, Room: b.RoomRoundTrips - a.RoomRoundTrips,
			Flush: b.FlushRoundTrips - a.FlushRoundTrips, UnparsedConnections: b.UnparsedConnections - a.UnparsedConnections,
		}
		if sent > 0 {
			rt.RoomPerUpdate = new(float64(rt.Room) / float64(sent))
		}
		out.RoundTrips = rt
		count := b.VersionInserts - a.VersionInserts
		out.Versions.InsertCount = &count
		out.Versions.InsertTotalMs = new(float64(b.VersionInsertNs-a.VersionInsertNs) / 1e6)
		var ms []float64
		for _, v := range b.RecentVersionInserts {
			if v.At.After(a.At) {
				ms = append(ms, float64(v.Ns)/1e6)
			}
		}
		out.Versions.InsertMs = mkStat(ms)
		out.Versions.InsertSamplesMissing = count - int64(len(ms))
	}
	if sampler == nil {
		out.ConnectionsNote = "the room sampler runs only in a step with editors"
	} else {
		rc := &roomConnections{Samples: len(sampler.samples), Errors: sampler.errors}
		var held []float64
		var all, shared int
		for _, s := range sampler.samples {
			held = append(held, float64(s.held))
			all += s.held
			shared += s.shared
		}
		rc.Held = mkStat(held)
		if all > 0 {
			rc.SharedPoolShare = new(float64(shared) / float64(all))
		}
		out.Connections = rc
	}
	switch {
	case start.rooms == nil:
		out.BatchesNote = start.roomsErr
	case end.rooms == nil:
		out.BatchesNote = end.roomsErr
	default:
		a, b := start.rooms, end.rooms
		rb := &dispatchRooms{AppendBatches: b.AppendBatches - a.AppendBatches, AppendedUpdates: b.AppendedUpdates - a.AppendedUpdates,
			AppendRoundTrips: b.AppendRoundTrips - a.AppendRoundTrips, BatchSizes: map[string]uint64{}}
		for k, v := range b.BatchSizes {
			rb.BatchSizes[k] = v - a.BatchSizes[k]
		}
		out.Batches = rb
	}
	switch {
	case start.versionsErr != "":
		out.Versions.Note = start.versionsErr
	case end.versionsErr != "":
		out.Versions.Note = end.versionsErr
	default:
		bytes, rows := end.versionBytes-start.versionBytes, end.versionRows-start.versionRows
		out.Versions.Bytes, out.Versions.Written = &bytes, &rows
		if s := end.versionsAt.Sub(start.versionsAt).Seconds(); s > 0 {
			out.Versions.BytesPerS = new(float64(bytes) / s)
		}
	}
	return out
}
