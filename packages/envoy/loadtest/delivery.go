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
	// POST's, not the GET's before it). Arrival is when the whole request was in Dispatch's
	// socket (httptrace WroteRequest), or Sent if it never got there (callResult.arrival);
	// Response is when the response was read.
	Sent, Arrival, Response int64
	// ConnWait is the client's own wait for a connection (GetConn to GotConn).
	ConnWait     time.Duration
	Status       int
	TransportErr string
	// InWindow is set when the arrival the open-loop schedule drew is in the measured window.
	InWindow bool
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
	// Session is the loop session that took it (sessionWindow.ID): the join counts what each
	// session was owed and what it got by it, and a second receipt of one event id by one session
	// as a duplicate. It is empty for the flat population's sessions, which never stop and are
	// counted by their issue's watchers alone.
	Session string
}

// sessionWindow is one loop session's life as the join reads it: the class it belongs to, the
// issue keys it followed, and when the listener acknowledged each of its two calls. A session the
// step's end cut off has no unsubscribe, and its window stays open (UnsubAck 0), since it was
// still subscribed when the step stopped measuring.
type sessionWindow struct {
	Class  string   // deliveryClassUnit or deliveryClassDriver
	ID     string   // the session id its receipts carry
	Issues []string // the issue keys of the topics it followed
	// SubSent is when its subscribe went out and SubAck when the listener acknowledged it;
	// UnsubSent and UnsubAck are the same two of its unsubscribe, 0 for a session that never
	// unsubscribed. All four are wall-clock Unix nanoseconds.
	SubSent, SubAck, UnsubSent, UnsubAck int64
}

// windowPlace is where one write's commit stands in a session's life.
type windowPlace int

const (
	// windowOutside is a write no session of this window could be owed: it committed before the
	// session's subscribe went out, or at or after its unsubscribe was acknowledged.
	windowOutside windowPlace = iota
	// windowExpected is a write the session was owed: the listener acknowledged its subscribe
	// before the commit, and acknowledged its unsubscribe after the receipt deadline.
	windowExpected
	// windowBoundary is a write that committed inside one of the two edges, where neither
	// delivery nor loss means anything: while the subscribe was in flight, so the registry may
	// have held the interest or not when the event fanned out
	// (internal/dispatch/envoy/store.Registry.Match reads what the subscribe wrote), or within
	// the receipt deadline of the unsubscribe, where the session's topics went away while the
	// event was still on its way.
	windowBoundary
)

// classify places one write's commit in the session's life. A write whose commit time Postgres
// did not keep (CommitNs 0, track_commit_timestamp off) is placed nowhere: the rule is a
// comparison of times, and there is no time to compare. So is every write of a session whose
// subscribe the listener never acknowledged, which followed nothing.
func (w *sessionWindow) classify(commitNs int64, deadline time.Duration) windowPlace {
	switch {
	case commitNs == 0 || w.SubAck == 0 || commitNs < w.SubSent:
		return windowOutside
	case commitNs <= w.SubAck:
		return windowBoundary
	case w.UnsubAck == 0:
		return windowExpected
	case commitNs >= w.UnsubAck:
		return windowOutside
	case commitNs+int64(deadline) >= w.UnsubAck:
		return windowBoundary
	default:
		return windowExpected
	}
}

// classDelivery is what one session class was owed and what it got.
type classDelivery struct {
	// Sessions counts the class's sessions, whatever each was owed.
	Sessions int `json:"sessions"`
	// Expected counts (write, session) pairs the session was owed a receipt of, Received those a
	// receipt arrived for, and Lost those expected that none arrived for. Received counts a
	// receipt of a boundary write too, so it is no subtraction of Expected.
	Expected int `json:"expected"`
	Received int `json:"received"`
	Lost     int `json:"lost"`
	// Duplicate counts the second and later receipts of one event id by one session; each gives
	// no delivery latency, since the first receipt already gave one.
	Duplicate int `json:"duplicate"`
	// Boundary counts the (write, session) pairs that committed inside an edge of the session's
	// life (windowBoundary). A boundary write fails nothing: unit sessions start and stop every
	// -unit-life, and without this column a correct deployment would read as losing notifications
	// on timing alone.
	Boundary int `json:"boundary"`
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
	// ByClass is what each loop session class (unit, driver) was owed and what it got, under the
	// expectation rule of its sessions' subscribe and unsubscribe times (classDelivery). It is
	// absent for a step with no loop sessions (-unit-starts 0), whose sessions have no window.
	// Its expected receipts are counted in ExpectedIssueTopicReceipts above, so Missing stays the
	// step's whole shortfall; its Boundary column says how much of a difference is the timing of
	// a start or a stop rather than a loss.
	ByClass map[string]classDelivery `json:"by_class,omitempty"`
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

// joinDelivery joins the writes, commit rows and receipts of a step whose sessions are the flat
// population alone: the join with no session window, which is what a step without the spec loop's
// own sessions records (deliveryJoin.join holds the rule).
func joinDelivery(writes []writeRec, commits []commitRow, receipts []receipt, watchers map[string]int, durationS float64) eventsSummary {
	return deliveryJoin{writes: writes, commits: commits, receipts: receipts, watchers: watchers, durationS: durationS}.join()
}

// deliveryJoin is one step's delivery as the join reads it: the writes the event generator sent,
// the run's commit rows, the sessions' receipts, the flat population's subscriptions per issue
// (watchers), the loop sessions' lives (windows, empty without -unit-starts), the deadline a
// receipt is expected by (-receipt-deadline) and the measured window's length.
type deliveryJoin struct {
	writes    []writeRec
	commits   []commitRow
	receipts  []receipt
	watchers  map[string]int
	windows   []sessionWindow
	deadline  time.Duration
	durationS float64
}

// join joins the step's writes, the database's commit rows and the sessions' receipts.
// Offered counts writes scheduled in the window. A write is committed when a commit row of its
// marker has its kind's primary type, whatever its HTTP status (a write that hit the client
// timeout but committed still counts); its primary event is the lowest such event id. Each
// issue-topic receipt of a primary event gives one delivery latency from the write's sending, one
// from its arrival and one from the event's commit; expected receipts are committed primary
// events times the issue's subscriptions (watchers), plus what every loop session was owed under
// the window rule (classCounts). A second receipt of one event id by one loop session is that
// class's duplicate: it keeps the latency of the first alone and is no second receipt.
func (j deliveryJoin) join() eventsSummary {
	out := eventsSummary{ByKind: map[string]kindSummary{}}
	byMarker := commitsByMarker(j.commits)
	type timed struct {
		w      *writeRec
		commit int64
	}
	primary := map[int64]timed{}
	other := map[int64]bool{}
	writeMs := map[string][]float64{}
	classes := newClassCounts(j.windows)
	var connWait []float64
	for i := range j.writes {
		w := &j.writes[i]
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
			writeMs[w.Kind] = append(writeMs[w.Kind], float64(w.Response-w.Arrival)/1e6)
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
			out.ExpectedIssueTopicReceipts += j.watchers[w.Issue]
			classes.expect(w.Issue, first.EventID, first.CommitNs, j.deadline)
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
	for _, r := range j.receipts {
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
		if classes.receive(r) {
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
	out.ByClass = classes.summary()
	for _, c := range out.ByClass {
		out.ExpectedIssueTopicReceipts += c.Expected
	}
	out.Missing = out.ExpectedIssueTopicReceipts - out.IssueTopicReceipts
	out.DeliveryMsFromSent = mkStat(fromSent)
	out.DeliveryMsFromArrival = mkStat(fromArrival)
	out.DeliveryMsFromCommit = mkStat(fromCommit)
	out.DeliveryMsFromPublish = mkStat(fromPublish)
	out.ConnWaitMs = mkStat(connWait)
	for kind, k := range out.ByKind {
		k.WriteMs = mkStat(writeMs[kind])
		k.DeliveryMsFromArrival = mkStat(kindArrival[kind])
		k.DeliveryMsFromCommit = mkStat(kindCommit[kind])
		out.ByKind[kind] = k
	}
	if durationS := j.durationS; durationS > 0 {
		out.OfferedPerS = float64(out.Offered) / durationS
		out.CommittedPerS = float64(out.Committed) / durationS
	}
	return out
}

// owedKey is one event one session was owed, or took.
type owedKey struct {
	session string
	event   int64
}

// classCounts counts, per session class, what the loop sessions were owed and what they got. It
// holds the windows by session id, to place a receipt in its class, and by issue key, to place a
// write in every session that followed it.
type classCounts struct {
	byID    map[string]*sessionWindow
	byIssue map[string][]*sessionWindow
	counts  map[string]*classDelivery
	owed    map[owedKey]bool
	got     map[owedKey]bool
}

// newClassCounts indexes the windows; with none it counts nothing, and the join is the flat
// population's as it has always been.
func newClassCounts(windows []sessionWindow) *classCounts {
	c := &classCounts{byID: map[string]*sessionWindow{}, byIssue: map[string][]*sessionWindow{},
		counts: map[string]*classDelivery{}, owed: map[owedKey]bool{}, got: map[owedKey]bool{}}
	for i := range windows {
		w := &windows[i]
		c.byID[w.ID] = w
		for _, issue := range w.Issues {
			c.byIssue[issue] = append(c.byIssue[issue], w)
		}
		c.class(w.Class).Sessions++
	}
	return c
}

func (c *classCounts) class(name string) *classDelivery {
	cd := c.counts[name]
	if cd == nil {
		cd = &classDelivery{}
		c.counts[name] = cd
	}
	return cd
}

// expect places one committed write in every session that followed its issue: each session it was
// expected of is owed that event, and each one the commit fell inside an edge of counts a
// boundary instead.
func (c *classCounts) expect(issue string, event, commitNs int64, deadline time.Duration) {
	for _, w := range c.byIssue[issue] {
		switch w.classify(commitNs, deadline) {
		case windowExpected:
			c.class(w.Class).Expected++
			c.owed[owedKey{session: w.ID, event: event}] = true
		case windowBoundary:
			c.class(w.Class).Boundary++
		}
	}
}

// receive counts one issue-topic receipt of a primary event and reports whether it is a duplicate,
// which the join leaves out of its receipts and its latencies. A receipt of no loop session
// (Session "") is never a duplicate: the flat population's sessions share their subscriptions'
// receipts with no session id, so one event id arrives on as many of them as watch its issue.
func (c *classCounts) receive(r receipt) bool {
	w := c.byID[r.Session]
	if w == nil {
		return false
	}
	k := owedKey{session: r.Session, event: r.EventID}
	if c.got[k] {
		c.class(w.Class).Duplicate++
		return true
	}
	c.got[k] = true
	c.class(w.Class).Received++
	return false
}

// summary counts what was owed and never arrived as each class's loss, and answers the counts;
// nil for a step with no loop session, whose summary.json carries no by_class at all.
func (c *classCounts) summary() map[string]classDelivery {
	if len(c.byID) == 0 {
		return nil
	}
	for k := range c.owed {
		if !c.got[k] {
			c.class(c.byID[k.session].Class).Lost++
		}
	}
	out := make(map[string]classDelivery, len(c.counts))
	for name, cd := range c.counts {
		out[name] = *cd
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
