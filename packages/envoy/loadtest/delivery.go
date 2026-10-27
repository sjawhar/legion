package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// writeRec is one write the event generator sent. Times are wall-clock Unix nanoseconds.
type writeRec struct {
	Marker int64
	Kind   string
	Issue  string
	Human  bool
	// Scheduled is the arrival the open-loop schedule drew; it decides whether the write is in
	// the measured window. Start is when the client began the request; Arrival is when the
	// whole request was in Dispatch's socket (httptrace WroteRequest), 0 when it never got
	// there; Response is when the response was read.
	Scheduled, Start, Arrival, Response int64
	// ConnWait is the client's own wait for a connection (GetConn to GotConn).
	ConnWait     time.Duration
	Status       int
	TransportErr string
	InWindow     bool
}

func (w writeRec) arrival() int64 {
	if w.Arrival != 0 {
		return w.Arrival
	}
	return w.Start
}

// commitRow is one loadtest.event_commits row: an event that carries a marker, with the commit
// time of its transaction (pg_xact_commit_timestamp(xmin)), 0 when Postgres has none.
type commitRow struct {
	EventID  int64
	Type     string
	Marker   int64
	CommitNs int64
}

// receipt is one envelope a simulated session received, matched by the event id in its payload.
type receipt struct {
	EventID int64
	Recv    int64 // Unix ns
	Agent   bool  // on a session's agent topic (a route copy), not an issue topic
}

type eventsSummary struct {
	Offered       int                    `json:"offered"`
	OfferedPerS   float64                `json:"offered_per_s"`
	Committed     int                    `json:"committed"`
	CommittedPerS float64                `json:"committed_per_s"`
	ByKind        map[string]kindSummary `json:"by_kind"`
	// DeliveryMsFromArrival and DeliveryMsFromCommit hold one value per (primary event,
	// subscribed session) issue-topic receipt.
	DeliveryMsFromArrival      stat `json:"delivery_ms_from_arrival"`
	DeliveryMsFromCommit       stat `json:"delivery_ms_from_commit"`
	IssueTopicReceipts         int  `json:"issue_topic_receipts"`
	ExpectedIssueTopicReceipts int  `json:"expected_issue_topic_receipts"`
	Missing                    int  `json:"missing"`
	AgentTopicReceipts         int  `json:"agent_topic_receipts"`
	// OtherEvents counts events carrying an in-window write's marker that are not its primary
	// event (a document snapshot, a reopened thread); OtherReceipts their receipts. Neither is
	// timed.
	OtherEvents   int `json:"other_events"`
	OtherReceipts int `json:"other_receipts"`
	// CommitTimesMissing counts primary events with no commit timestamp
	// (track_commit_timestamp off); they get no delivery_ms_from_commit.
	CommitTimesMissing int `json:"commit_times_missing"`
	// ClockAnomalies counts receipts whose event committed before its write arrived.
	ClockAnomalies int         `json:"clock_anomalies"`
	WriteErrors    writeErrors `json:"write_errors"`
	ConnWaitMs     stat        `json:"conn_wait_ms"`
	// CommitsError is why loadtest.event_commits could not be read, when it could not.
	CommitsError string `json:"commits_error,omitempty"`
}

type kindSummary struct {
	PrimaryType           string `json:"primary_type"`
	Offered               int    `json:"offered"`
	Committed             int    `json:"committed"`
	WriteMs               stat   `json:"write_ms"`
	DeliveryMsFromArrival stat   `json:"delivery_ms_from_arrival"`
	DeliveryMsFromCommit  stat   `json:"delivery_ms_from_commit"`
}

type writeErrors struct {
	Non2xx    int `json:"non_2xx"`
	Transport int `json:"transport"`
}

// joinDelivery joins the step's writes, the database's commit rows and the sessions' receipts.
// Offered counts writes scheduled in the window. A write is committed when a commit row of its
// marker has its kind's primary type, whatever its HTTP status (a write that hit the client
// timeout but committed still counts); its primary event is the lowest such event id. Each
// issue-topic receipt of a primary event gives one delivery latency from the write's arrival and
// one from the event's commit; expected receipts are committed primary events times the issue's
// subscriptions (watchers).
func joinDelivery(writes []writeRec, commits []commitRow, receipts []receipt, watchers map[string]int, durationS float64) eventsSummary {
	out := eventsSummary{ByKind: map[string]kindSummary{}}
	byMarker := map[int64][]commitRow{}
	for _, c := range commits {
		byMarker[c.Marker] = append(byMarker[c.Marker], c)
	}
	type timed struct {
		w      *writeRec
		commit int64
	}
	primary := map[int64]timed{}
	other := map[int64]bool{}
	writeMs := map[string][]float64{}
	var connWait []float64
	for i := range writes {
		w := &writes[i]
		if !w.InWindow {
			continue
		}
		k := out.ByKind[w.Kind]
		k.PrimaryType = primaryEventType[w.Kind]
		k.Offered++
		out.Offered++
		connWait = append(connWait, float64(w.ConnWait)/1e6)
		switch {
		case w.TransportErr != "":
			out.WriteErrors.Transport++
		case w.Status/100 != 2:
			out.WriteErrors.Non2xx++
		default:
			writeMs[w.Kind] = append(writeMs[w.Kind], float64(w.Response-w.arrival())/1e6)
		}
		var first *commitRow
		for j, c := range byMarker[w.Marker] {
			if c.Type == k.PrimaryType && (first == nil || c.EventID < first.EventID) {
				first = &byMarker[w.Marker][j]
			}
		}
		for _, c := range byMarker[w.Marker] {
			if first == nil || c.EventID != first.EventID {
				other[c.EventID] = true
			}
		}
		if first != nil {
			k.Committed++
			out.Committed++
			out.ExpectedIssueTopicReceipts += watchers[w.Issue]
			primary[first.EventID] = timed{w: w, commit: first.CommitNs}
			if first.CommitNs == 0 {
				out.CommitTimesMissing++
			}
		}
		out.ByKind[w.Kind] = k
	}
	out.OtherEvents = len(other)

	var fromArrival, fromCommit []float64
	kindArrival := map[string][]float64{}
	kindCommit := map[string][]float64{}
	for _, r := range receipts {
		t, ok := primary[r.EventID]
		if !ok {
			if other[r.EventID] {
				out.OtherReceipts++
			}
			continue
		}
		if r.Agent {
			out.AgentTopicReceipts++
			continue
		}
		out.IssueTopicReceipts++
		arrival := t.w.arrival()
		ms := float64(r.Recv-arrival) / 1e6
		fromArrival = append(fromArrival, ms)
		kindArrival[t.w.Kind] = append(kindArrival[t.w.Kind], ms)
		if t.commit != 0 {
			ms := float64(r.Recv-t.commit) / 1e6
			fromCommit = append(fromCommit, ms)
			kindCommit[t.w.Kind] = append(kindCommit[t.w.Kind], ms)
			if t.commit < arrival {
				out.ClockAnomalies++
			}
		}
	}
	out.Missing = out.ExpectedIssueTopicReceipts - out.IssueTopicReceipts
	out.DeliveryMsFromArrival = mkStat(fromArrival)
	out.DeliveryMsFromCommit = mkStat(fromCommit)
	out.ConnWaitMs = mkStat(connWait)
	for kind, k := range out.ByKind {
		k.WriteMs = mkStat(writeMs[kind])
		k.DeliveryMsFromArrival = mkStat(kindArrival[kind])
		k.DeliveryMsFromCommit = mkStat(kindCommit[kind])
		out.ByKind[kind] = k
	}
	if durationS > 0 {
		out.OfferedPerS = float64(out.Offered) / durationS
		out.CommittedPerS = float64(out.Committed) / durationS
	}
	return out
}

// parseMarker reads the counter of an "lgm<N>x" marker.
func parseMarker(s string) (int64, bool) {
	digits, ok := strings.CutPrefix(s, "lgm")
	if !ok {
		return 0, false
	}
	digits, ok = strings.CutSuffix(digits, "x")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	return n, err == nil
}

// readCommits reads the commit rows of the given markers over a direct Postgres connection
// (commit-trigger.sql installs the table; track_commit_timestamp gives the times).
func readCommits(ctx context.Context, pgURL string, markers []int64) ([]commitRow, error) {
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	tags := make([]string, len(markers))
	for i, m := range markers {
		tags[i] = markerTag(m)
	}
	rows, err := conn.Query(ctx, `
		select event_id, type, marker, pg_xact_commit_timestamp(xmin)
		from loadtest.event_commits where marker = any($1)`, tags)
	if err != nil {
		return nil, fmt.Errorf("read loadtest.event_commits (is commit-trigger.sql installed?): %w", err)
	}
	defer rows.Close()
	var out []commitRow
	for rows.Next() {
		var c commitRow
		var tag string
		var at *time.Time
		if err := rows.Scan(&c.EventID, &c.Type, &tag, &at); err != nil {
			return nil, err
		}
		c.Marker, _ = parseMarker(tag)
		if at != nil {
			c.CommitNs = at.UnixNano()
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
