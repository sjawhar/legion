package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// INFRA-450's a14: what the Inbox's reads cost once a person has dismissed a very great many
// threads. A dismissal keeps the row rather than deleting it, so the pile only grows, and the
// question this measures is the one the person it is for asked - whether reading the Inbox, or
// reading a page of what he has dismissed, gets slower as it does.
//
// The seed is his own shape, an order of magnitude past where he is: 100,000 dismissed Reply
// threads, 10,000 dismissed Agent threads, 600 undismissed threads (300 of them unread) and 1,000
// more unread threads for Mark all read to work over - 1,000 rather than a token number because he
// had 804 unread replies the morning this was written.
//
// Six operations, 20 runs each, p50 and p95 of the server's own time: the whole Inbox read
// (state=inbox), the Inbox count's read (state=unread&limit=1), the first Dismissed page, a
// Dismissed page 50,000 rows deep, a dismissal naming 120 threads, and one Mark all read.
//
// # Three shapes, and which bound each number is held to
//
// Every shape is seeded by the same code and differs only in the pile:
//
//   - no dismissed: no dismissed row at all. The ratio baseline for the four operations whose
//     answer is identical in both shapes - state=inbox, the unread count, the 120-thread dismiss
//     and the Mark all read - since those four read the same 1,600 undismissed rows either way.
//   - one page dismissed: 200 dismissed threads, exactly one full Dismissed page. The ratio
//     baseline for the two Dismissed reads, because with no dismissed row at all those two answer
//     nothing: a ratio against an answer of 0 rows measures the empty case rather than the pile,
//     and reporting it as a pass would say nothing. What the person's worry actually asks is not
//     "is reading a page slow" but "does a pile make reading a page slower", which is one page at
//     200 against one page at 110,000.
//   - loaded: the whole pile, 100,000 + 10,000.
//
// Five operations are held to the same two bounds: 250 ms on the p95, and twice their own
// baseline's p50 - the no-dismissed shape for three of them, the one-page shape for the two
// Dismissed reads. The sixth, the Inbox count, is held to 50 ms on its p95 with its ratio
// recorded rather than bounded, for the reason written at dismissedScaleBounds.
//
// # What these numbers are, and what they are not
//
// They are one box: this pod's Postgres, which also carries every other suite running beside it,
// with a seed of one shape and one person's rows. They are not production latency, and a reading
// taken while the box is busy is a reading of a busy box - the ratio is the part that survives
// that, since both shapes are measured in the same run, minutes apart, and a machine that slows
// moves both. The time of record is the server's own: the clock runs around handler.ServeHTTP
// alone, with the request body encoded before it starts, so no client, no socket and no JSON
// encoding of the caller's is on it. The server's own access log prints an elapsed_ms for each of
// these requests too, which is the same span less the recorder's write.
//
// One more thing the seed is: every row of user_threads belongs to one login, because the
// measurement is one person's pile. That is the pessimistic case for the planner's choice between
// a state's partial index and a scan of the table, since the login this read narrows by rules out
// nothing here, where in production it rules out everyone else's rows. A plan that scans
// user_threads at this seed may still read the index at the same pile size on a table many people
// write in; a plan that reads the index here reads it there too.
//
// Each shape is a database of its own (storetest.Open), dropped when the test ends, so the
// 110,000 rows are gone by the time the next test runs.
const (
	// The pile, as the measurement asks for it.
	dismissedScaleReplyPile = 100_000
	dismissedScaleAgentPile = 10_000
	// One full Dismissed page, in the pile's own proportion of the two kinds: the second
	// baseline's whole pile.
	dismissedScalePageReplies = 180
	dismissedScalePageAgents  = 20
	// The Inbox every shape holds: 600 undismissed threads, 300 of them unread, of both kinds.
	dismissedScaleInboxReplies = 540
	dismissedScaleInboxAgents  = 60
	dismissedScaleReadReplies  = 270
	dismissedScaleReadAgents   = 30
	// The unread threads Mark all read works over, beside those 300.
	dismissedScaleMarkAll = 1_000
	// The measurement itself.
	dismissedScaleRuns        = 20
	dismissedScaleDismissRows = 120
	dismissedScaleCursorDepth = 50_000
	// The bounds verification.md v17 holds each read to. Five carry 250 ms on the p95 and twice
	// their own baseline's p50; the Inbox count carries 50 ms on its p95 alone
	// (dismissedScaleBounds). The ratio is taken on the p50 because a p95 of 20 samples is
	// nearly their maximum, and on this pod's shared Postgres a ratio of p95s failed a
	// different read in each of four runs, once with a baseline p95 (216.4 ms) above its own
	// loaded p95 (179.7 ms). What the pile costs the reads that stay inside it is locality: every
	// read that touches user_threads reads a 110,000-entry index over a 101,540-row heap instead
	// of 200 entries over 1,740 rows, which is the 1.3x to 1.7x measured at p50 rather than a
	// loop over the pile.
	dismissedScaleTimeBound      = 250 * time.Millisecond
	dismissedScaleCountTimeBound = 50 * time.Millisecond
	dismissedScaleRatioBound     = 2.0
	// The session every seeded Agent thread is answered by, and the login every row belongs to.
	dismissedScaleSession = "01a1058e-f14f-7684-87eb-3dc885955552"
	dismissedScaleLogin   = "alice"
	// dismissedScaleEnv is what a14's own command sets to run this measurement.
	dismissedScaleEnv = "LEGION_SCALE_TEST"
)

// The minute band each seed's threads are aged into, so the pile is older than the Inbox and no
// two bands overlap: the pile's newest reply is 100,003 minutes old, the mark-all's 2,003 and the
// Inbox's 3.
const (
	dismissedScalePileAge   = 100_000
	dismissedScaleMarkAge   = 2_000
	dismissedScaleInboxAge  = 0
	dismissedScaleAgentAge  = 100_000
	dismissedScaleAgentStep = 10
)

// The bodies each seed writes, which is also how a later statement finds its own rows.
const (
	dismissedScalePileLabel       = "dismissed"
	dismissedScaleInboxLabel      = "inbox"
	dismissedScaleMarkLabel       = "markall"
	dismissedScaleAgentPileLabel  = "dismissed agent"
	dismissedScaleAgentInboxLabel = "inbox agent"
)

// dismissedScaleShape is one seeded database and the server over it, with the rows the
// measurement needs to name: the cursor a page 50,000 deep resumes from, the 120 rows one
// dismissal names, and the unread rows a Mark all read marks, which the next run puts back.
type dismissedScaleShape struct {
	name             string
	handler          http.Handler
	database         *store.Store
	issue            string
	dismissedReplies int
	dismissedAgents  int
	deepCursor       string
	dismissRows      []string
	unreadThreads    []string
	unreadReplies    []string
}

// TestDismissedScaleHoldsTheInboxsReads is a14 itself: it seeds the three shapes, asserts each
// one's counts before it times anything, measures the six operations 20 runs each, explains every
// read at the pile's own size, and holds each operation to the two bounds above.
//
// It runs when LEGION_SCALE_TEST is 1, which is what a14's own command sets, and skips otherwise:
// seeding 110,000 rows three times over and taking 120 timed readings costs minutes, which no
// ordinary run of this package should pay.
func TestDismissedScaleHoldsTheInboxsReads(t *testing.T) {
	if os.Getenv(dismissedScaleEnv) != "1" {
		t.Skipf("set %s=1 to run the dismissed-scale measurement", dismissedScaleEnv)
	}
	loaded := seedDismissedScale(t, "loaded", dismissedScaleReplyPile, dismissedScaleAgentPile)
	none := seedDismissedScale(t, "no dismissed", 0, 0)
	onePage := seedDismissedScale(t, "one page dismissed", dismissedScalePageReplies, dismissedScalePageAgents)

	// The readings, each the p50 and p95 of 20 runs. The two Dismissed reads are measured on the
	// no-dismissed shape as well, where they answer nothing: that number is reported rather than
	// compared, so a reader can see what the empty case costs and why it is nobody's baseline.
	readings := map[string]dismissedScaleReading{}
	taken := []string{}
	record := func(operation string, shape *dismissedScaleShape, run func(*testing.T, *dismissedScaleShape) time.Duration) {
		name := operation + " @ " + shape.name
		readings[name] = measureDismissedScale(t, shape, operation, run)
		taken = append(taken, name)
	}
	for _, shape := range []*dismissedScaleShape{loaded, none} {
		record("state=inbox", shape, runDismissedScaleInbox)
		record("unread count", shape, runDismissedScaleUnreadCount)
		record("dismissed page 1", shape, runDismissedScaleFirstPage)
		record("120-thread dismiss", shape, runDismissedScaleDismiss)
		record("mark all read", shape, runDismissedScaleMarkAll)
	}
	record("dismissed page 1", onePage, runDismissedScaleFirstPage)
	record("dismissed page 50,000 deep", loaded, runDismissedScaleDeepPage)

	// The plans, at the pile's own size. They are asserted on the loaded shape alone: with no
	// dismissed row at all a scan of an empty table is the right plan and says nothing about
	// scale, so the other two shapes' plans are printed and nothing is held to them.
	explainDismissedScaleReads(t, loaded, true)
	explainDismissedScaleReads(t, none, false)
	explainDismissedScaleReads(t, onePage, false)

	// The table, in one block, which is what goes in the pull request's comment. Both statistics
	// are printed for every operation: the p95 each time bound is taken on, and the p50 the ratio
	// is taken on, which for the Inbox count is recorded rather than bounded.
	t.Log("operation\tshape\tp50 ms\tp95 ms\tbaseline p50 ms\tbaseline p95 ms\tratio of p50s\tbound")
	for _, held := range dismissedScaleBounds() {
		reading, ok := readings[held.operation+" @ loaded"]
		if !ok {
			t.Fatalf("no reading for %s", held.operation)
		}
		baseline := readings[held.baselineOperation+" @ "+held.baselineShape]
		ratio := float64(reading.p50) / float64(baseline.p50)
		bound := fmt.Sprintf("%s on p95 and %.2fx on p50 of %s", held.timeBound, held.ratio, held.baselineShape)
		if held.ratio == 0 {
			bound = fmt.Sprintf("%s on p95; the ratio is recorded, not bounded", held.timeBound)
		}
		t.Logf("%s\tloaded\t%.1f\t%.1f\t%.1f\t%.1f\t%.2f\t%s",
			held.operation, msOf(reading.p50), msOf(reading.p95), msOf(baseline.p50),
			msOf(baseline.p95), ratio, bound)
	}
	for _, name := range taken {
		reading := readings[name]
		t.Logf("%s\tp50 %.1f ms\tp95 %.1f ms\truns %v", name, msOf(reading.p50), msOf(reading.p95), reading.runs)
	}

	// The bounds, one subtest per operation so a read that is a finding names itself and the rest
	// still report their numbers. Each time bound is taken on the p95, which is the worst case a
	// person waits through; each ratio on the p50, which is the shape of the read. Why they are
	// taken on different statistics is measured rather than assumed: over seven runs of this test
	// on this pod, state=inbox's ratio of p95s read 0.83, 1.12, 1.12, 1.19, 1.31 and 2.04, and
	// Mark all read's 0.86, 0.90, 1.16, 1.20 and 1.63, with the failing read changing from run to
	// run and one run's baseline p95 (216.4 ms) coming in above its own loaded p95 (179.7 ms).
	// With 20 runs the p95 is the 19th of 20 - all but the maximum - and this Postgres carries
	// every other suite of the pod beside it, so a neighbour landing inside one sample moves it.
	// The p50s of the same runs moved little: state=inbox 1.30x-1.50x, Mark all read 1.27x-1.35x
	// (verification.md v17).
	for _, held := range dismissedScaleBounds() {
		t.Run(held.operation, func(t *testing.T) {
			reading := readings[held.operation+" @ loaded"]
			baseline := readings[held.baselineOperation+" @ "+held.baselineShape]
			if reading.p95 > held.timeBound {
				t.Errorf("%s: p95 %.1f ms over the %s bound (p50 %.1f ms, runs %v)",
					held.operation, msOf(reading.p95), held.timeBound, msOf(reading.p50), reading.runs)
			}
			ratio := float64(reading.p50) / float64(baseline.p50)
			if held.ratio > 0 && ratio > held.ratio {
				t.Errorf("%s: p50 %.1f ms is %.2fx its baseline (%s %s, p50 %.1f ms), over %.2fx",
					held.operation, msOf(reading.p50), ratio, held.baselineShape,
					held.baselineOperation, msOf(baseline.p50), held.ratio)
			}
		})
	}

	// The negative control, last: it rewrites an index inside a transaction it rolls back.
	dismissedScaleNegativeControl(t, loaded)
}

// dismissedScaleHeld is one operation, the p95 it is held to, the baseline its p50 ratio is taken
// against and the ratio it is held to. A zero ratio is one that is recorded rather than bounded.
type dismissedScaleHeld struct {
	operation         string
	timeBound         time.Duration
	baselineOperation string
	baselineShape     string
	ratio             float64
}

// dismissedScaleBounds names the six operations, the p95 each is held to and the baseline each
// ratio is taken against (verification.md v17). Five are held to 250 ms on the p95 and twice
// their baseline's p50. The two Dismissed reads are taken against one full page at 200 dismissed
// threads rather than against a shape with none, where they would answer no rows at all, and the
// other three against the no-dismissed shape, which holds the same 1,600 undismissed rows they
// read at the pile.
//
// The Inbox count is the sixth, and its ratio is recorded rather than bounded, for what the plan
// shows at the pile: 12.2 ms of its 13.1 ms Agent half is one index scan of the person's own
// one-off root messages - 10,060 entries at the pile against 60 at the baseline, of which about
// 5 ms is hashing their 10,000 dismissal marks once and 0.7 ms is the thread walk, which runs 60
// times and not 10,060. The read is one pass over the person's own history with no loop per
// dismissed thread, which is what a14 asks of it, and no read linear in a count that grows 167x
// is within 2x of itself. Measured at 1.96x, 2.10x, 2.10x and 2.57x over four runs. What holds it
// instead is a p95 of 50 ms: a read that grows with the pile fails that long before it reaches
// the 250 ms every other read carries, since the shape this change replaced - which asked each
// dismissed thread its own question - measured 681 ms here (verification.md v17).
func dismissedScaleBounds() []dismissedScaleHeld {
	return []dismissedScaleHeld{
		{"state=inbox", dismissedScaleTimeBound, "state=inbox", "no dismissed", dismissedScaleRatioBound},
		{"unread count", dismissedScaleCountTimeBound, "unread count", "no dismissed", 0},
		{"dismissed page 1", dismissedScaleTimeBound, "dismissed page 1", "one page dismissed", dismissedScaleRatioBound},
		{"dismissed page 50,000 deep", dismissedScaleTimeBound, "dismissed page 1", "one page dismissed", dismissedScaleRatioBound},
		{"120-thread dismiss", dismissedScaleTimeBound, "120-thread dismiss", "no dismissed", dismissedScaleRatioBound},
		{"mark all read", dismissedScaleTimeBound, "mark all read", "no dismissed", dismissedScaleRatioBound},
	}
}

// The five requests. Each returns the server's own time for one run: the body is encoded before
// the clock starts, and the clock covers handler.ServeHTTP alone.

func runDismissedScaleInbox(t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	taken, response := timeDismissedScaleRequest(t, shape, http.MethodGet,
		"/api/v1/me/replies?kind=reply,agent&state=inbox", nil)
	page := decodeDismissedScaleReplies(t, response)
	if len(page.Rows) != dismissedScaleInboxRows() {
		t.Fatalf("state=inbox answered %d rows, want the %d the seed holds", len(page.Rows), dismissedScaleInboxRows())
	}
	return taken
}

func runDismissedScaleUnreadCount(t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	taken, response := timeDismissedScaleRequest(t, shape, http.MethodGet,
		"/api/v1/me/replies?kind=reply,agent&state=unread&limit=1", nil)
	page := decodeDismissedScaleReplies(t, response)
	if page.Total != dismissedScaleUnreadRows() {
		t.Fatalf("the Inbox count read answered total=%d, want %d", page.Total, dismissedScaleUnreadRows())
	}
	return taken
}

func runDismissedScaleFirstPage(t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	taken, response := timeDismissedScaleRequest(t, shape, http.MethodGet,
		"/api/v1/me/replies?kind=reply,agent&state=dismissed", nil)
	page := decodeDismissedScaleReplies(t, response)
	want := min(shape.dismissedReplies+shape.dismissedAgents, myRepliesDismissedPage)
	if len(page.Rows) != want {
		t.Fatalf("the first Dismissed page answered %d rows, want %d", len(page.Rows), want)
	}
	return taken
}

func runDismissedScaleDeepPage(t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	taken, response := timeDismissedScaleRequest(t, shape, http.MethodGet,
		"/api/v1/me/replies?kind=reply,agent&state=dismissed&cursor="+shape.deepCursor, nil)
	page := decodeDismissedScaleReplies(t, response)
	if len(page.Rows) != myRepliesDismissedPage {
		t.Fatalf("the page %d deep answered %d rows, want a full page",
			dismissedScaleCursorDepth, len(page.Rows))
	}
	return taken
}

// runDismissedScaleDismiss times one dismissal naming 120 Reply rows and puts them back
// afterwards, off the clock, so every run of the 20 dismisses the same 120 threads out of the
// same Inbox. The 120 are threads the seed already marked read, so dismissing them moves no read
// mark and the unread count the other operations read is the same before and after.
//
// The versions it carries are read off the seed before the clock starts (dismissBody,
// api/inbox_dismiss_test.go), as a page would have carried them: nobody writes in these threads
// while the measurement runs, so each row still holds the version it was seeded with and every
// run dismisses all 120.
func runDismissedScaleDismiss(t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	rows := make([]string, 0, len(shape.dismissRows))
	for _, thread := range shape.dismissRows {
		rows = append(rows, inboxDismissReplyPrefix+thread)
	}
	body := dismissBody(t, shape.database, dismissedScaleLogin, rows)
	taken, response := timeDismissedScaleRequest(t, shape, http.MethodPost, "/api/v1/me/inbox/dismiss", body)
	if dismissed := decodeDismissedScaleCount(t, response, "dismissed"); dismissed != dismissedScaleDismissRows {
		t.Fatalf("the dismissal answered %d threads, want the %d it named", dismissed, dismissedScaleDismissRows)
	}
	restore := dismissDismissedScaleRestore(t, shape, rows)
	if restore != dismissedScaleDismissRows {
		t.Fatalf("the restore put back %d threads, want %d", restore, dismissedScaleDismissRows)
	}
	return taken
}

// runDismissedScaleMarkAll times one Mark all read over the unread threads the seed holds, with
// read_through taken from a list read's own as_of, as the page sends it, and puts every mark back
// afterwards, off the clock.
func runDismissedScaleMarkAll(t *testing.T, shape *dismissedScaleShape) time.Duration {
	t.Helper()
	asOf := dismissedScaleAsOf(t, shape)
	taken, response := timeDismissedScaleRequest(t, shape, http.MethodPost, "/api/v1/me/inbox/read",
		map[string]any{"read_through": asOf, "kind": []string{"reply", "agent"}})
	marked := decodeDismissedScaleCount(t, response, "marked")
	if marked != dismissedScaleUnreadRows() {
		t.Fatalf("Mark all read marked %d threads, want the %d unread the seed holds",
			marked, dismissedScaleUnreadRows())
	}
	unmarkDismissedScale(t, shape)
	return taken
}

// dismissedScaleInboxRows and dismissedScaleUnreadRows are what every shape's Inbox holds: the
// 600 undismissed threads plus the 1,000 the mark-all works over, and the unread among them.
func dismissedScaleInboxRows() int {
	return dismissedScaleInboxReplies + dismissedScaleInboxAgents + dismissedScaleMarkAll
}

func dismissedScaleUnreadRows() int {
	return dismissedScaleInboxReplies - dismissedScaleReadReplies +
		dismissedScaleInboxAgents - dismissedScaleReadAgents + dismissedScaleMarkAll
}

// timeDismissedScaleRequest runs one request and answers the server's own time for it. The body
// is encoded first, so only handler.ServeHTTP is on the clock.
func timeDismissedScaleRequest(
	t *testing.T, shape *dismissedScaleShape, method, target string, body any,
) (time.Duration, *httptest.ResponseRecorder) {
	t.Helper()
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s %s: %v", method, target, err)
		}
		reader = strings.NewReader(string(encoded))
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", dismissedScaleLogin)
	response := httptest.NewRecorder()
	started := time.Now()
	shape.handler.ServeHTTP(response, request)
	taken := time.Since(started)
	if response.Code != http.StatusOK {
		t.Fatalf("%s %s on %s: status=%d body=%s", method, target, shape.name, response.Code, response.Body.String())
	}
	return taken, response
}

func decodeDismissedScaleReplies(t *testing.T, response *httptest.ResponseRecorder) model.MyRepliesResponse {
	t.Helper()
	var page model.MyRepliesResponse
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode the Inbox read: %v", err)
	}
	return page
}

func decodeDismissedScaleCount(t *testing.T, response *httptest.ResponseRecorder, field string) int {
	t.Helper()
	counted := map[string]int{}
	if err := json.Unmarshal(response.Body.Bytes(), &counted); err != nil {
		t.Fatalf("decode the write's answer: %v", err)
	}
	return counted[field]
}

// dismissedScaleAsOf is the as_of of one list read, which is what the page sends a Mark all read
// as its read_through. It is off the clock.
func dismissedScaleAsOf(t *testing.T, shape *dismissedScaleShape) string {
	t.Helper()
	response := dispatchRequest(t, shape.handler, http.MethodGet,
		"/api/v1/me/replies?kind=reply,agent&state=unread&limit=1", nil, dismissedScaleLogin)
	if response.Code != http.StatusOK {
		t.Fatalf("read as_of on %s: status=%d body=%s", shape.name, response.Code, response.Body.String())
	}
	return decodeDismissedScaleReplies(t, response).AsOf.UTC().Format(time.RFC3339Nano)
}

// dismissDismissedScaleRestore puts the dismissed rows back, off the clock.
func dismissDismissedScaleRestore(t *testing.T, shape *dismissedScaleShape, rows []string) int {
	t.Helper()
	response := dispatchRequest(t, shape.handler, http.MethodPost, "/api/v1/me/inbox/dismiss",
		map[string]any{"threads": rows, "restore": true}, dismissedScaleLogin)
	if response.Code != http.StatusOK {
		t.Fatalf("restore on %s: status=%d body=%s", shape.name, response.Code, response.Body.String())
	}
	return decodeDismissedScaleCount(t, response, "dismissed")
}

// unmarkDismissedScale puts every read mark one Mark all read moved back where it was, off the
// clock: each Reply thread's mark to the minute before its newest reply, which is where the
// trigger left it, and the Agent threads' replies unread by deleting the rows the mark-all wrote
// by id. The dismissed pile's own read rows are named here one by one, so they stay.
func unmarkDismissedScale(t *testing.T, shape *dismissedScaleShape) {
	t.Helper()
	ctx := context.Background()
	tag, err := shape.database.Pool.Exec(ctx, `
		update user_threads set read_through = replied_at - interval '1 minute'
		where login = $1 and thread_id = any($2::uuid[])
	`, dismissedScaleLogin, shape.unreadThreads)
	if err != nil {
		t.Fatalf("put the thread marks back: %v", err)
	}
	if tag.RowsAffected() != int64(len(shape.unreadThreads)) {
		t.Fatalf("put back %d thread marks, want %d", tag.RowsAffected(), len(shape.unreadThreads))
	}
	deleted, err := shape.database.Pool.Exec(ctx, `
		delete from user_agent_reply_read where login = $1 and reply_id = any($2::uuid[])
	`, dismissedScaleLogin, shape.unreadReplies)
	if err != nil {
		t.Fatalf("put the agent marks back: %v", err)
	}
	if deleted.RowsAffected() != int64(len(shape.unreadReplies)) {
		t.Fatalf("put back %d agent marks, want %d", deleted.RowsAffected(), len(shape.unreadReplies))
	}
}

// dismissedScaleReading is one operation's 20 runs: the two percentiles and every reading, so a
// number in the table can be read against the spread it came from.
type dismissedScaleReading struct {
	p50  time.Duration
	p95  time.Duration
	runs []time.Duration
}

// measureDismissedScale runs one operation 20 times and answers its percentiles, with one warm-up
// run that is not counted: the first run of a statement on a fresh connection pays for the pages
// every run after it finds cached, and a measurement of the pile is not a measurement of a cold
// cache.
func measureDismissedScale(
	t *testing.T, shape *dismissedScaleShape, operation string,
	run func(*testing.T, *dismissedScaleShape) time.Duration,
) dismissedScaleReading {
	t.Helper()
	run(t, shape)
	taken := make([]time.Duration, 0, dismissedScaleRuns)
	for range dismissedScaleRuns {
		taken = append(taken, run(t, shape))
	}
	sorted := slices.Clone(taken)
	slices.Sort(sorted)
	reading := dismissedScaleReading{
		p50:  percentileOfDismissedScale(sorted, 0.50),
		p95:  percentileOfDismissedScale(sorted, 0.95),
		runs: taken,
	}
	t.Logf("%s @ %s: p50 %.1f ms, p95 %.1f ms over %d runs",
		operation, shape.name, msOf(reading.p50), msOf(reading.p95), dismissedScaleRuns)
	return reading
}

// percentileOfDismissedScale is the nearest-rank percentile of sorted readings: the ceil(q*n)th,
// so p50 of 20 is the tenth and p95 the nineteenth. No interpolation, so every number printed is
// a run that happened.
func percentileOfDismissedScale(sorted []time.Duration, quantile float64) time.Duration {
	rank := int(math.Ceil(quantile*float64(len(sorted)))) - 1
	return sorted[max(rank, 0)]
}

func msOf(taken time.Duration) float64 {
	return float64(taken.Microseconds()) / 1000
}
