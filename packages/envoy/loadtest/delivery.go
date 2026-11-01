package main

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// writeRec is one write the event generator sent. Times are wall-clock Unix nanoseconds.
type writeRec struct {
	Marker int64 // the write's counter in the run (markerTag)
	Kind   string
	Issue  string
	// Sent is when the client began the write's request (callResult.start; for an upload, the
	// POST's, not the GET's before it, unless that GET ended the write: a 429 or 503 answer to it
	// is the upload's refusal, and a failed read its transport error). Arrival is when the whole
	// request was in Dispatch's socket (httptrace WroteRequest), or Sent if it never got there
	// (callResult.arrival); Response is when the response was read.
	Sent, Arrival, Response int64
	// ConnWait is the client's own wait for a connection (GetConn to GotConn).
	ConnWait     time.Duration
	Status       int
	TransportErr string
	// Code is the refusal code of an answer outside 2xx (callResult.code); ConnFailure the class
	// of a write that got no complete response (connFailureOf), "" when it has none.
	Code, ConnFailure string
	// Scheduled is the arrival the open-loop schedule drew for the write, Unix ns; InWindow is set
	// when it is in the measured window.
	Scheduled int64
	InWindow  bool
}

// commitRow is one loadtest.event_commits row of this run: an event that carries one of the run's
// markers, with the marker's counter and the commit time of its transaction
// (pg_xact_commit_timestamp(xmin)), 0 when Postgres has none.
type commitRow struct {
	EventID  int64
	Type     string
	Marker   int64
	CommitNs int64
}

// commitTagRow is one loadtest.event_commits row as read: its marker as text.
type commitTagRow struct {
	EventID int64
	Type    string
	Tag     string
	At      *time.Time
}

// receipt is one envelope a simulated session received, matched by the event id in its payload.
type receipt struct {
	EventID int64
	Recv    int64 // Unix ns
	Agent   bool  // on a session's agent topic (a route copy), not an issue topic
	// Published is when the stream stored the message (a relay frame's Envoy-Stored-At), Unix ns;
	// 0 for a core receipt, which carries no such time.
	Published int64
}

type eventsSummary struct {
	Offered       int                    `json:"offered"`
	OfferedPerS   float64                `json:"offered_per_s"`
	Committed     int                    `json:"committed"`
	CommittedPerS float64                `json:"committed_per_s"`
	ByKind        map[string]kindSummary `json:"by_kind"`
	// DeliveryMsFromSent, DeliveryMsFromArrival and DeliveryMsFromCommit hold one value per
	// (primary event, subscribed session) issue-topic receipt. DeliveryMsFromPublish holds one
	// per such receipt that carries the stream's store time (a relay frame under consumer
	// delivery): the receipt time minus the time the event's NATS publish was stored.
	DeliveryMsFromSent         stat `json:"delivery_ms_from_sent"`
	DeliveryMsFromArrival      stat `json:"delivery_ms_from_arrival"`
	DeliveryMsFromCommit       stat `json:"delivery_ms_from_commit"`
	DeliveryMsFromPublish      stat `json:"delivery_ms_from_publish"`
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
	// Refusals splits the offered writes that failed: by refusal code, then by connection failure,
	// with the latency of the 429 and 503 answers.
	Refusals refusalSummary `json:"refusals"`
	// Answered counts the offered writes that got an HTTP status, whatever it was.
	Answered   int  `json:"answered"`
	ConnWaitMs stat `json:"conn_wait_ms"`
	// CommitsError is why loadtest.event_commits could not be read, when it could not. Then every
	// count and latency above that comes from the commit rows (committed, by_kind committed, the
	// receipts, missing, other events and receipts, commit_times_missing, clock_anomalies and the
	// delivery latencies) is 0 because nothing was read, not measured; the tables show it as "-"
	// with the mark "(commit rows not read)", and the step fails (stepFailure).
	CommitsError string `json:"commits_error,omitempty"`
}

// commitsUnread reports that the run's commit rows could not be read (CommitsError).
func (e eventsSummary) commitsUnread() bool { return e.CommitsError != "" }

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
// issue-topic receipt of a primary event gives one delivery latency from the write's sending, one
// from its arrival and one from the event's commit; expected receipts are committed primary
// events times the issue's subscriptions (watchers).
func joinDelivery(writes []writeRec, commits []commitRow, receipts []receipt, watchers map[string]int, durationS float64) eventsSummary {
	out := eventsSummary{ByKind: map[string]kindSummary{}}
	byMarker := commitsByMarker(commits)
	type timed struct {
		w      *writeRec
		commit int64
	}
	primary := map[int64]timed{}
	other := map[int64]bool{}
	writeMs := map[string][]float64{}
	var connWait []float64
	refusals := newRefusalTally()
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
		writeLatency := float64(w.Response-w.Arrival) / 1e6
		refusals.add(w.TransportErr != "", w.ConnFailure, w.Status, w.Code, writeLatency)
		if w.Status != 0 {
			out.Answered++
		}
		switch {
		case w.TransportErr != "":
			out.WriteErrors.Transport++
		case w.Status/100 != 2:
			out.WriteErrors.Non2xx++
		default:
			writeMs[w.Kind] = append(writeMs[w.Kind], writeLatency)
		}
		first := primaryCommit(byMarker[w.Marker], k.PrimaryType)
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

	var fromSent, fromArrival, fromCommit, fromPublish []float64
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
		fromSent = append(fromSent, float64(r.Recv-t.w.Sent)/1e6)
		if r.Published != 0 {
			fromPublish = append(fromPublish, float64(r.Recv-r.Published)/1e6)
		}
		arrival := t.w.Arrival
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
	out.DeliveryMsFromSent = mkStat(fromSent)
	out.DeliveryMsFromArrival = mkStat(fromArrival)
	out.DeliveryMsFromCommit = mkStat(fromCommit)
	out.DeliveryMsFromPublish = mkStat(fromPublish)
	out.ConnWaitMs = mkStat(connWait)
	out.Refusals = refusals.summary()
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

// commitsByMarker groups commit rows by their marker.
func commitsByMarker(commits []commitRow) map[int64][]commitRow {
	byMarker := map[int64][]commitRow{}
	for _, c := range commits {
		byMarker[c.Marker] = append(byMarker[c.Marker], c)
	}
	return byMarker
}

// primaryCommit is a write's primary event among the commit rows of its marker: the lowest event
// id of the kind's primary type, nil when no row has that type.
func primaryCommit(rows []commitRow, primaryType string) *commitRow {
	var first *commitRow
	for j, c := range rows {
		if c.Type == primaryType && (first == nil || c.EventID < first.EventID) {
			first = &rows[j]
		}
	}
	return first
}

// readCommits reads the commit rows of the given markers of the run with this nonce over a direct
// Postgres connection (commit-trigger.sql installs the table; track_commit_timestamp gives the
// times). Every error names loadtest.event_commits. With no markers there is no row to read, and
// it reads nothing: a step with no write in the window has nothing that depends on the rows.
func readCommits(ctx context.Context, pgURL string, nonce int64, markers []int64) ([]commitRow, error) {
	if len(markers) == 0 {
		return nil, nil
	}
	conn, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		return nil, fmt.Errorf("read loadtest.event_commits: %w", err)
	}
	defer conn.Close(context.Background())
	tags := make([]string, len(markers))
	for i, m := range markers {
		tags[i] = markerTag(nonce, m)
	}
	rows, err := conn.Query(ctx, `
		select event_id, type, marker, pg_xact_commit_timestamp(xmin)
		from loadtest.event_commits where marker = any($1)`, tags)
	if err != nil {
		return nil, fmt.Errorf("read loadtest.event_commits (is commit-trigger.sql installed?): %w", err)
	}
	defer rows.Close()
	var read []commitTagRow
	for rows.Next() {
		var c commitTagRow
		if err := rows.Scan(&c.EventID, &c.Type, &c.Tag, &c.At); err != nil {
			return nil, fmt.Errorf("read loadtest.event_commits: %w", err)
		}
		read = append(read, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read loadtest.event_commits: %w", err)
	}
	return runCommits(read, nonce), nil
}

// runCommits keeps the rows whose marker is the run's with this nonce, with the marker's counter;
// another run's rows, which can share every counter, are dropped.
func runCommits(read []commitTagRow, nonce int64) []commitRow {
	var out []commitRow
	for _, r := range read {
		counter, ok := parseMarker(r.Tag, nonce)
		if !ok {
			continue
		}
		c := commitRow{EventID: r.EventID, Type: r.Type, Marker: counter}
		if r.At != nil {
			c.CommitNs = r.At.UnixNano()
		}
		out = append(out, c)
	}
	return out
}
