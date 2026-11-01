package storetest

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// The xid window rig (INFRA-636) measures how long a write transaction holds its transaction id
// (xid) and what its backend does meanwhile. Every xid holds back the snapshot horizon of every
// reader until it ends, so the time a transaction spends with an xid and no statement running - Go
// doing document work or advice between two statements - is time the whole database waits on that
// Go code.
//
// Two sources are joined, both stamped with this process's monotonic clock:
//   - XidTracer, a pgx tracer on the pool under test, records each statement every backend runs:
//     its start, its end, its SQL and whether its own result says it wrote (xidAssigned, which
//     reads the command tag Postgres answered it with). pgx sends BEGIN and COMMIT as ordinary
//     statements, so the trace splits each backend's statements into its transactions.
//   - A poller on a connection of its own reads pg_stat_activity every xidPollInterval and records
//     which backends hold an xid. A statement's command tag does not say which statement assigned
//     the xid when the write was a trigger's or a function's; Postgres does.
//
// A transaction's xid window starts at the earlier of the two sources' answers: the last statement
// that began before the first poll that saw the backend hold an xid, and the first statement whose
// own result says it wrote (xidAssigned). It runs to the end of its COMMIT. A transaction the
// poller never saw - a write phase that is one batch can hold its xid for less than a poll
// interval - is judged from its statements alone; one that wrote nothing by either source holds
// no xid to judge (MayHoldNoXid). Inside the window the rig checks D6 of the INFRA-636 plan:
// every statement-free gap is at most XidMaxGap, and the gaps together take no longer than the
// statements do.
//
// Which transactions are judged is decided by context, not guessed from timing: the caller marks
// the context its operation runs under (MarkXid) and the tracer marks every statement whose
// context carries it. A transaction is the operation's when any statement of it is marked, and
// then every statement its backend ran between BEGIN and COMMIT is counted, marked or not, since a
// connection serves only its transaction's holder until it commits. Background work of the server
// (settlement timers, the outbox, listeners) runs under contexts of its own, so it shares the pool
// but never the verdict.

// XidMaxGap is the longest statement-free gap D6 allows inside an xid window.
const XidMaxGap = 5 * time.Millisecond

// xidPollInterval is how often the poller reads pg_stat_activity: the plan's 0.5 ms. A poll's own
// round trip is of the same order, so in practice a backend is looked at about every millisecond.
const xidPollInterval = 500 * time.Microsecond

// xidSQLWidth is how much of a statement's SQL a report quotes.
const xidSQLWidth = 100

type xidMarkKey struct{}

type xidSpanKey struct{}

// MarkXid marks ctx as the operation an XidTracer.Measure judges: every statement run under it,
// or under a context derived from it, is the operation's.
func MarkXid(ctx context.Context) context.Context {
	return context.WithValue(ctx, xidMarkKey{}, true)
}

func markedXid(ctx context.Context) bool {
	marked, _ := ctx.Value(xidMarkKey{}).(bool)
	return marked
}

// XidStatement is one statement a backend ran, as the tracer saw it from the client: Start is
// when pgx began sending it and End when its result was read (for a query, when its rows were
// closed). A batch is one statement whose SQL joins the batch's queries.
type XidStatement struct {
	PID    uint32
	Start  time.Time
	End    time.Time
	SQL    string
	Marked bool
	// Wrote is true when the statement's own result says it took a transaction id (xidAssigned):
	// for a batch, when any query of it did.
	Wrote bool
}

// XidTracer is a pgx QueryTracer, BatchTracer and CopyFromTracer that records the statements each
// backend runs while Measure runs. Install it on a pool with Traced.
type XidTracer struct {
	mu         sync.Mutex
	recording  bool
	statements []XidStatement
	// blind is how long Measure's poller waits before its first read of pg_stat_activity; zero
	// polls from the start (BlindPoller).
	blind time.Duration
}

// NewXidTracer returns a tracer that records nothing until Measure starts it.
func NewXidTracer() *XidTracer { return &XidTracer{} }

// BlindPoller makes the next Measure's poller wait d before its first read of pg_stat_activity, so
// a transaction that begins and commits inside d is never seen holding an xid. Only the rig's own
// self-tests use it, to prove that such a transaction is still judged from the window its own
// statements derive; a measurement of the code under test leaves it unset.
func (x *XidTracer) BlindPoller(d time.Duration) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.blind = d
}

type xidSpan struct {
	statement XidStatement
	sql       []string
}

func (x *XidTracer) begin(ctx context.Context, conn *pgx.Conn, sql string) context.Context {
	x.mu.Lock()
	recording := x.recording
	x.mu.Unlock()
	if !recording {
		return ctx
	}
	span := &xidSpan{statement: XidStatement{
		PID: conn.PgConn().PID(), Start: time.Now(), SQL: sql, Marked: markedXid(ctx),
	}}
	return context.WithValue(ctx, xidSpanKey{}, span)
}

// end closes the span ctx carries with tag, the command tag the statement answered - the zero tag
// for a batch, whose queries report their own (TraceBatchQuery).
func (x *XidTracer) end(ctx context.Context, tag pgconn.CommandTag) {
	span, ok := ctx.Value(xidSpanKey{}).(*xidSpan)
	if !ok {
		return
	}
	span.statement.End = time.Now()
	if len(span.sql) > 0 {
		span.statement.SQL = strings.Join(span.sql, "; ")
	}
	span.statement.Wrote = span.statement.Wrote || xidAssigned(span.statement.SQL, tag)
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.recording {
		x.statements = append(x.statements, span.statement)
	}
}

// TraceQueryStart implements pgx.QueryTracer.
func (x *XidTracer) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return x.begin(ctx, conn, data.SQL)
}

// TraceQueryEnd implements pgx.QueryTracer.
func (x *XidTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	x.end(ctx, data.CommandTag)
}

// TraceBatchStart implements pgx.BatchTracer.
func (x *XidTracer) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	ctx = x.begin(ctx, conn, "batch")
	if span, ok := ctx.Value(xidSpanKey{}).(*xidSpan); ok && data.Batch != nil {
		for _, queued := range data.Batch.QueuedQueries {
			span.sql = append(span.sql, queued.SQL)
		}
	}
	return ctx
}

// TraceBatchQuery implements pgx.BatchTracer. A batch is one statement of the trace, and it wrote
// when any query of it did: the first of them to write is what gave the transaction its xid, and
// the batch's own start is when that query was sent.
func (x *XidTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if span, ok := ctx.Value(xidSpanKey{}).(*xidSpan); ok && xidAssigned(data.SQL, data.CommandTag) {
		span.statement.Wrote = true
	}
}

// TraceBatchEnd implements pgx.BatchTracer.
func (x *XidTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchEndData) {
	x.end(ctx, pgconn.CommandTag{})
}

// TraceCopyFromStart implements pgx.CopyFromTracer.
func (x *XidTracer) TraceCopyFromStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceCopyFromStartData) context.Context {
	return x.begin(ctx, conn, "copy "+data.TableName.Sanitize())
}

// TraceCopyFromEnd implements pgx.CopyFromTracer.
func (x *XidTracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	x.end(ctx, data.CommandTag)
}

// xidPoll is one read of pg_stat_activity: sent when the query left, received when its answer
// came back, and the backends that held an xid in the snapshot taken between the two.
type xidPoll struct {
	sent     time.Time
	received time.Time
	holders  []uint32
}

// Measure runs operation while it records every statement and polls which backends hold an xid,
// and answers the xid windows of the transactions operation's marked context ran (MarkXid). pool
// is the pool under test; the poller dials a connection of its own from its configuration,
// untraced, so it never waits for one of the pool's.
func (x *XidTracer) Measure(t testing.TB, pool *store.Pool, operation func()) *XidReport {
	t.Helper()
	ctx := context.Background()
	config := pool.Config().ConnConfig.Copy()
	config.Tracer = nil
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("xid rig: open the poll connection: %v", err)
	}
	defer conn.Close(ctx)
	const holdersQuery = `
		select pid from pg_stat_activity
		where datname = current_database() and backend_xid is not null and pid <> pg_backend_pid()`
	poll := func() (xidPoll, error) {
		sample := xidPoll{sent: time.Now()}
		rows, err := conn.Query(ctx, holdersQuery)
		if err != nil {
			return sample, err
		}
		holders, err := pgx.CollectRows(rows, pgx.RowTo[uint32])
		sample.received = time.Now()
		sample.holders = holders
		return sample, err
	}
	// The first poll prepares the statement, so no later one pays for it.
	if _, err := poll(); err != nil {
		t.Fatalf("xid rig: poll pg_stat_activity: %v", err)
	}

	x.mu.Lock()
	x.statements = nil
	x.recording = true
	blind := x.blind
	x.mu.Unlock()

	stop := make(chan struct{})
	var polls []xidPoll
	var pollErr error
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		if blind > 0 {
			select {
			case <-stop:
				return
			case <-time.After(blind):
			}
		}
		for {
			sample, err := poll()
			if err != nil {
				pollErr = err
				return
			}
			polls = append(polls, sample)
			select {
			case <-stop:
				return
			default:
			}
			if wait := xidPollInterval - time.Since(sample.sent); wait > 0 {
				time.Sleep(wait)
			}
		}
	}()

	started := time.Now()
	operation()
	finished := time.Now()
	close(stop)
	<-polled

	x.mu.Lock()
	x.recording = false
	statements := x.statements
	x.statements = nil
	x.mu.Unlock()
	if pollErr != nil {
		t.Fatalf("xid rig: poll pg_stat_activity: %v", pollErr)
	}
	return analyzeXidWindows(statements, polls, finished.Sub(started))
}

// XidTransaction is one transaction of the measured operation: every statement its backend ran
// from BEGIN through COMMIT.
type XidTransaction struct {
	PID        uint32
	Statements []XidStatement
	// Committed is false for a transaction that rolled back or had not ended when the operation
	// returned; neither is judged.
	Committed bool
	// Sighted is when the first poll that saw the backend hold an xid answered; zero when no poll
	// did.
	Sighted time.Time
	// Unseen is when the last poll before that one, which saw no xid, was sent; zero when no poll
	// sent after BEGIN preceded the sighting. The xid was assigned between the two.
	Unseen time.Time
}

// XidWindow is the part of a transaction during which it held its xid, as D6 judges it.
type XidWindow struct {
	Transaction XidTransaction
	// Statements are the transaction's statements from the window's first through COMMIT.
	Statements    []XidStatement
	Start, End    time.Time
	StatementTime time.Duration
	GapTime       time.Duration
	MaxGap        time.Duration
	// MaxGapAfter indexes Statements: the biggest gap follows Statements[MaxGapAfter].
	MaxGapAfter int
}

// Length is the window's span, its first statement's start to its COMMIT's end.
func (w XidWindow) Length() time.Duration { return w.End.Sub(w.Start) }

// Violations are the D6 rules the window breaks, empty when it keeps them.
func (w XidWindow) Violations() []string {
	var broken []string
	if w.MaxGap > XidMaxGap {
		broken = append(broken, fmt.Sprintf("a statement-free gap of %s (over %s) follows statement %d: %s",
			XidMillis(w.MaxGap), XidMillis(XidMaxGap), w.MaxGapAfter, xidSQL(w.Statements[w.MaxGapAfter].SQL)))
	}
	if w.GapTime > w.StatementTime {
		broken = append(broken, fmt.Sprintf("its gaps take %s, more than its statements' %s",
			XidMillis(w.GapTime), XidMillis(w.StatementTime)))
	}
	return broken
}

// Summary is the window in one line: what a passing run logs and a red run quotes.
func (w XidWindow) Summary() string {
	return fmt.Sprintf("backend %d: xid window %s, %d statements taking %s, gaps %s, largest gap %s after statement %d (%s)",
		w.Transaction.PID, XidMillis(w.Length()), len(w.Statements), XidMillis(w.StatementTime), XidMillis(w.GapTime),
		XidMillis(w.MaxGap), w.MaxGapAfter, xidSQL(w.Statements[w.MaxGapAfter].SQL))
}

// Detail lists every statement of the window and every gap between them, times relative to the
// window's start, so a red run shows where the time with the xid went.
func (w XidWindow) Detail() string {
	var b strings.Builder
	begin := w.Transaction.Statements[0].Start
	unseen := "none"
	if !w.Transaction.Unseen.IsZero() {
		unseen = XidMillis(w.Transaction.Unseen.Sub(begin))
	}
	sighted := "never (the window is its own statements')"
	if !w.Transaction.Sighted.IsZero() {
		sighted = XidMillis(w.Transaction.Sighted.Sub(begin))
	}
	fmt.Fprintf(&b, "  transaction began at 0.000 ms; last poll without its xid sent at %s; xid first seen at %s; window %s -> %s\n",
		unseen, sighted, XidMillis(w.Start.Sub(begin)), XidMillis(w.End.Sub(begin)))
	for i, statement := range w.Statements {
		if i > 0 {
			gap := statement.Start.Sub(w.Statements[i-1].End)
			flag := ""
			if gap > XidMaxGap {
				flag = "  <-- over " + XidMillis(XidMaxGap)
			}
			fmt.Fprintf(&b, "      gap %10s%s\n", XidMillis(gap), flag)
		}
		fmt.Fprintf(&b, "  %3d %10s +%9s  %s\n", i, XidMillis(statement.Start.Sub(w.Start)),
			XidMillis(statement.End.Sub(statement.Start)), xidSQL(statement.SQL))
	}
	return b.String()
}

// XidReport is what one Measure saw.
type XidReport struct {
	// Windows are the operation's committed transactions that held an xid, in commit order: each
	// one a poll saw hold it, or whose own statements show it wrote (XidStatement.Wrote).
	Windows []XidWindow
	// Unsighted are the operation's committed transactions no poll saw hold an xid and whose
	// statements show no write of their own: nothing says they held one.
	Unsighted []XidTransaction
	// Unjudged are the operation's transactions that rolled back or were still open when the
	// operation returned.
	Unjudged []XidTransaction
	// Problems are the reasons the rig cannot judge the operation: its verdict would be about the
	// rig, not the code under test.
	Problems []string
	// MayHoldNoXid lets an operation that may write nothing - a read under an advisory lock, a
	// write whose statements all matched no row - pass with no window at all. Unset, a report with
	// no window is a rig problem: the operation wrote nothing a statement's result showed and no
	// poll saw it hold an xid, so either it made no write or the wrong transaction was marked.
	MayHoldNoXid bool
	Duration     time.Duration
	Polls        int
	// LongestPoll is the longest time between two polls' sends, how blind the poller was at its
	// worst.
	LongestPoll time.Duration
}

// xidWriteSQL is a statement that may assign an xid. It never decides that a transaction held one:
// it picks the window's first statement among those the polls leave possible (xidWindowOf). What a
// statement did assign is read off its result instead (xidAssigned).
var xidWriteSQL = regexp.MustCompile(`(?is)\b(insert\s+into|update\s+\S+\s+set|delete\s+from)\b|\bfor\s+(no\s+key\s+)?update\b|\bfor\s+(key\s+)?share\b|pg_current_xact_id\(\)`)

// xidRowLockSQL is a select that takes a row lock, which Postgres gives the transaction an xid for
// as it does a write.
var xidRowLockSQL = regexp.MustCompile(`(?is)\bfor\s+(no\s+key\s+update|update|key\s+share|share)\b`)

// xidForcedSQL is a statement that asks Postgres for the transaction's xid outright, which assigns
// one.
var xidForcedSQL = regexp.MustCompile(`(?i)pg_current_xact_id\(\)`)

// xidAssigned reports whether a statement's own result says it took a transaction id: an insert,
// update or delete that touched a row, a merge, a select that locked one, a copy that wrote one,
// or a call of pg_current_xact_id(). Postgres assigns an xid when a statement first changes or
// locks a row, so a statement that touched none - an update whose where clause matched nothing -
// took none, and the command tag's row count is what says so. The tag counts the statement's own
// rows and not a trigger's, so a statement that wrote nothing itself and whose trigger wrote is
// left to the poller, which sees an xid whichever write assigned it.
func xidAssigned(sql string, tag pgconn.CommandTag) bool {
	if xidForcedSQL.MatchString(sql) {
		return true
	}
	if strings.HasPrefix(tag.String(), "MERGE") {
		return true
	}
	switch {
	case tag.Insert(), tag.Update(), tag.Delete(), strings.HasPrefix(tag.String(), "COPY"):
		return tag.RowsAffected() > 0
	case tag.Select():
		return tag.RowsAffected() > 0 && xidRowLockSQL.MatchString(sql)
	}
	return false
}

func analyzeXidWindows(statements []XidStatement, polls []xidPoll, duration time.Duration) *XidReport {
	report := &XidReport{Duration: duration, Polls: len(polls)}
	for i := 1; i < len(polls); i++ {
		report.LongestPoll = max(report.LongestPoll, polls[i].sent.Sub(polls[i-1].sent))
	}
	byBackend := map[uint32][]XidStatement{}
	for _, statement := range statements {
		byBackend[statement.PID] = append(byBackend[statement.PID], statement)
	}
	var transactions []XidTransaction
	for pid, run := range byBackend {
		slices.SortFunc(run, func(a, b XidStatement) int { return a.Start.Compare(b.Start) })
		var open *XidTransaction
		for _, statement := range run {
			switch xidVerb(statement.SQL) {
			case "begin":
				if open != nil {
					transactions = append(transactions, *open)
				}
				open = &XidTransaction{PID: pid, Statements: []XidStatement{statement}}
			case "commit", "rollback":
				if open == nil {
					continue
				}
				open.Statements = append(open.Statements, statement)
				open.Committed = xidVerb(statement.SQL) == "commit"
				transactions = append(transactions, *open)
				open = nil
			default:
				if open != nil {
					open.Statements = append(open.Statements, statement)
				}
			}
		}
		if open != nil {
			transactions = append(transactions, *open)
		}
	}
	slices.SortFunc(transactions, func(a, b XidTransaction) int {
		return a.Statements[len(a.Statements)-1].End.Compare(b.Statements[len(b.Statements)-1].End)
	})
	for _, transaction := range transactions {
		if !slices.ContainsFunc(transaction.Statements, func(s XidStatement) bool { return s.Marked }) {
			continue
		}
		if !transaction.Committed {
			report.Unjudged = append(report.Unjudged, transaction)
			continue
		}
		begin := transaction.Statements[0].Start
		end := transaction.Statements[len(transaction.Statements)-1].End
		// A poll sent before BEGIN started could still be showing the xid of the transaction the
		// same connection ran before this one, so only a poll sent after it counts.
		for _, sample := range polls {
			if sample.sent.Before(begin) || sample.received.After(end) {
				continue
			}
			if slices.Contains(sample.holders, transaction.PID) {
				transaction.Sighted = sample.received
				break
			}
			transaction.Unseen = sample.sent
		}
		// A transaction whose statements show no write of their own and that no poll saw took no
		// xid to judge: an operation that may write nothing passes on MayHoldNoXid, and one that
		// was to write is the report's own rig problem (problems).
		if transaction.Sighted.IsZero() && xidFirstWrote(transaction) < 0 {
			report.Unsighted = append(report.Unsighted, transaction)
			continue
		}
		report.Windows = append(report.Windows, xidWindowOf(transaction))
	}
	return report
}

// xidWindowOf cuts transaction at its xid window and measures it. The window starts at the earlier
// of what the two sources say, and runs to the end of COMMIT.
//
// The polls say: the plan's rule starts the window at the last statement that began before the
// first poll that saw the xid answered. That poll answers a round trip after its snapshot, and on
// a busy box later still, so a statement or two may have begun meanwhile; the statement that
// assigned the xid is one that was still running after the last poll that saw none was sent. When
// a statement in that blind spot may assign an xid (xidWriteSQL), the window starts at the first
// such one instead, so a fast statement after the assigning one cannot hide the time between them.
// The polls bound that choice: text only picks among the statements they leave possible.
//
// The statements say, exactly: the first one whose own result shows it wrote rows (XidStatement
// .Wrote) held an xid from its start, whatever the polls saw. A write phase that is one batch can
// hold its xid for less than a poll interval, so a transaction the poller never saw is judged from
// this alone; a transaction it did see is judged from whichever source names the earlier statement,
// since both name a statement that already held the xid.
func xidWindowOf(transaction XidTransaction) XidWindow {
	first := 0
	if !transaction.Sighted.IsZero() {
		for i, statement := range transaction.Statements {
			if statement.Start.Before(transaction.Sighted) {
				first = i
			}
		}
		unseen := transaction.Unseen
		if unseen.IsZero() {
			unseen = transaction.Statements[0].Start
		}
		for i, statement := range transaction.Statements[1:max(first, 1)] {
			if statement.End.After(unseen) && xidWriteSQL.MatchString(statement.SQL) {
				first = i + 1
				break
			}
		}
	}
	if wrote := xidFirstWrote(transaction); wrote >= 0 && (transaction.Sighted.IsZero() || wrote < first) {
		first = wrote
	}
	window := XidWindow{Transaction: transaction, Statements: transaction.Statements[first:]}
	window.Start = window.Statements[0].Start
	window.End = window.Statements[len(window.Statements)-1].End
	for i, statement := range window.Statements {
		window.StatementTime += statement.End.Sub(statement.Start)
		if i == 0 {
			continue
		}
		gap := max(statement.Start.Sub(window.Statements[i-1].End), 0)
		window.GapTime += gap
		if gap > window.MaxGap {
			window.MaxGap = gap
			window.MaxGapAfter = i - 1
		}
	}
	return window
}

// xidFirstWrote indexes the transaction's first statement whose own result shows it wrote rows,
// and so held an xid from its start (XidStatement.Wrote); -1 when none did.
func xidFirstWrote(transaction XidTransaction) int {
	return slices.IndexFunc(transaction.Statements, func(s XidStatement) bool { return s.Wrote })
}

// xidVerb classifies a statement as one that opens or ends a transaction. A savepoint's "rollback
// to savepoint" ends nothing.
func xidVerb(sql string) string {
	verb := strings.ToLower(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(verb, "begin"):
		return "begin"
	case verb == "commit":
		return "commit"
	case verb == "rollback":
		return "rollback"
	}
	return ""
}

func xidSQL(sql string) string {
	sql = strings.Join(strings.Fields(sql), " ")
	if len(sql) > xidSQLWidth {
		sql = sql[:xidSQLWidth]
	}
	return sql
}

// XidMillis formats d as the rig's reports do: milliseconds to three places.
func XidMillis(d time.Duration) string {
	return fmt.Sprintf("%.3f ms", float64(d)/float64(time.Millisecond))
}

// problems are the report's rig problems: Problems, and a report that judged no window when the
// operation was to hold an xid.
func (r *XidReport) problems() []string {
	problems := r.Problems
	if len(r.Windows) == 0 && !r.MayHoldNoXid {
		problems = append(slices.Clone(problems), fmt.Sprintf(
			"no transaction of the operation held an xid: none wrote a row a statement's result showed and no poll saw one hold an xid (%d polls, %d unsighted, %d unjudged transactions); the rig judged nothing",
			r.Polls, len(r.Unsighted), len(r.Unjudged)))
	}
	return problems
}

// Err is the report's verdict: nil when every window keeps D6 and the rig judged one (or the
// operation MayHoldNoXid). A rig problem comes first, worded so a red run cannot be mistaken for a
// finding.
func (r *XidReport) Err() error {
	if problems := r.problems(); len(problems) > 0 {
		return fmt.Errorf("XID RIG PROBLEM (not a finding about the code): %s", strings.Join(problems, "; "))
	}
	var b strings.Builder
	for _, window := range r.Windows {
		if broken := window.Violations(); len(broken) > 0 {
			fmt.Fprintf(&b, "XID WINDOW VIOLATION (INFRA-636 D6) %s\n  %s\n%s", window.Summary(), strings.Join(broken, "\n  "), window.Detail())
		}
	}
	if b.Len() > 0 {
		return fmt.Errorf("%s", b.String())
	}
	return nil
}

// AssertXidWindows logs every window the report judged, so a run quotes its window length and
// largest gap whichever way it goes, then fails t when the rig could not judge the operation or a
// window breaks D6.
func AssertXidWindows(t testing.TB, report *XidReport) {
	t.Helper()
	t.Logf("xid rig: operation took %s, %d polls, longest poll interval %s, %d window(s), %d committed transaction(s) never seen with an xid, %d unjudged",
		XidMillis(report.Duration), report.Polls, XidMillis(report.LongestPoll), len(report.Windows), len(report.Unsighted), len(report.Unjudged))
	for _, window := range report.Windows {
		t.Logf("xid rig: %s", window.Summary())
	}
	if err := report.Err(); err != nil {
		if len(report.problems()) > 0 {
			t.Fatal(err)
		}
		t.Error(err)
	}
}

// xidWords is the load harness's vocabulary (packages/envoy/loadtest/seed.go), so the fixture
// reads like the documents the load reproduction measured.
var xidWords = strings.Fields(`the outbox publishes each committed event to its issue topic and every session that
watches the issue receives it within a second while the listener keeps interests in a key value bucket
dispatch reads issues documents comments and asks from postgres through a shared pool of connections
a reviewer anchors a comment to a line of the specification and the author answers with a new version
latency throughput memory goroutines profile scaling knee saturation connection queue lock contention`)

func xidSentence(r *rand.Rand, n int) string {
	words := make([]string, n)
	for i := range words {
		words[i] = xidWords[r.IntN(len(xidWords))]
	}
	return strings.Join(words, " ")
}

// XidSpec is the xid window tests' fixture document: a table-heavy spec of about 40 KB, built as
// the load harness builds its large specs (specMarkdown) - sections of prose, each with an
// eighteen-row, six-column table whose every row carries a unique requirement cell an anchor can
// quote - with one "Revision token r0" line an edit can bump. It answers the markdown and its
// requirement cells in document order; the same seed gives the same document.
func XidSpec(project string, seed uint64) (string, []string) {
	r := rand.New(rand.NewPCG(seed, 636))
	var b strings.Builder
	var quotes []string
	fmt.Fprintf(&b, "# Spec for %s\n\nRevision token r0\n\n", project)
	row, section := 0, 0
	for b.Len() < 40000 {
		section++
		fmt.Fprintf(&b, "## Section %d\n\n%s.\n\n%s.\n\n", section, xidSentence(r, 40), xidSentence(r, 30))
		b.WriteString("| Id | Requirement | Owner | Status | Evidence | Notes |\n|---|---|---|---|---|---|\n")
		for range 18 {
			row++
			quote := fmt.Sprintf("Requirement %s-R%d holds", project, row)
			quotes = append(quotes, quote)
			fmt.Fprintf(&b, "| R%d | %s | team-%d | open | %s | %s |\n", row, quote, r.IntN(9), xidSentence(r, 6), xidSentence(r, 5))
		}
		b.WriteString("\n")
	}
	return b.String(), quotes
}
