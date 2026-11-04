package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/pglisten"
)

// createOutcomeIssue files an issue in project as alice and returns its key.
func createOutcomeIssue(t *testing.T, handler http.Handler, project, title string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": project, "title": title, "force": true,
	}, "alice")
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue %q: status=%d body=%s", title, response.Code, response.Body.String())
	}
	return decodeBody[testIssueWithSuggestions](t, response).Key
}

// postOutcomeMessage posts a message on issueKey as actor, which appends a message.created event
// on that issue.
func postOutcomeMessage(t *testing.T, handler http.Handler, issueKey, actor, body string) {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/messages", map[string]any{
		"body": body,
	}, actor)
	if response.Code != http.StatusCreated {
		t.Fatalf("post a message on %s: status=%d body=%s", issueKey, response.Code, response.Body.String())
	}
}

// outcomeCursor reads the sweep's cursor, nil while no pass has set it.
func outcomeCursor(t *testing.T, database *store.Store) *int64 {
	t.Helper()
	var cursor *int64
	if err := database.Pool.QueryRow(context.Background(), `select event_id from suggestion_outcome_cursor`).Scan(&cursor); err != nil {
		t.Fatalf("read the suggestion outcome cursor: %v", err)
	}
	return cursor
}

// lastEventID is the highest event id the database holds.
func lastEventID(t *testing.T, database *store.Store) int64 {
	t.Helper()
	var id int64
	if err := database.Pool.QueryRow(context.Background(), `select coalesce(max(id), 0) from events`).Scan(&id); err != nil {
		t.Fatalf("read the last event id: %v", err)
	}
	return id
}

// ageIssueEvents moves every event of issueKey back by by, so a test that needs an event the
// sweep reads as settle old sets that age on the row rather than waiting the window out. Every
// one of the issue's events moves together, so the order the writes made is the order that
// stands.
func ageIssueEvents(t *testing.T, database *store.Store, issueKey string, by time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update events set created_at = created_at - $2::float8 * interval '1 second' where issue_key = $1`,
		issueKey, by.Seconds()); err != nil {
		t.Fatalf("age the events of %s: %v", issueKey, err)
	}
}

// ageSuggestion moves the suggestion sourceIssueKey offered back by by, which is how a test makes
// one settle old without waiting: the pass judges a suggestion by now() - created_at, as it judges
// an event (readSuggestionPage, touchedSuggestions).
func ageSuggestion(t *testing.T, database *store.Store, sourceIssueKey string, by time.Duration) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(),
		`update write_suggestions set created_at = created_at - $2::float8 * interval '1 second' where source_issue_key = $1`,
		sourceIssueKey, by.Seconds()); err != nil {
		t.Fatalf("age the suggestion of %s: %v", sourceIssueKey, err)
	}
}

// suggestionAge is how old the suggestion sourceIssueKey offered is by the database's clock, the
// one the pass compares against settle.
func suggestionAge(t *testing.T, database *store.Store, sourceIssueKey string) time.Duration {
	t.Helper()
	var seconds float64
	if err := database.Pool.QueryRow(context.Background(),
		`select extract(epoch from now() - created_at)::float8 from write_suggestions where source_issue_key = $1`,
		sourceIssueKey).Scan(&seconds); err != nil {
		t.Fatalf("read the age of the suggestion of %s: %v", sourceIssueKey, err)
	}
	return time.Duration(seconds * float64(time.Second))
}

// sweepSuggestionOutcomes judges everything there is to judge at the server's page size, holding
// nothing back to settle, up to a safe horizon proven after the call (safeHorizon), so the pass
// reads every event committed before it: what a test that has just written what it wants judged
// needs.
func sweepSuggestionOutcomes(t *testing.T, srv *server) error {
	t.Helper()
	_, err := sweepSuggestionOutcomePages(t.Context(), srv.deps.Store.Pool, safeHorizon(t, srv), suggestionOutcomePage, 0)
	return err
}

// idleSweep is a sweep running as the server runs it - its woken loop on a real pglisten.Hub's
// wake on EventsChannel - with a short gap between passes and the settle it is given, on a counted
// pool, once its boot pass and the pass the hub's signal after its LISTEN earned have both run and
// it has gone quiet.
type idleSweep struct {
	handler        http.Handler
	server         *server
	counter        *storetest.StatementCounter
	target, source string
}

// idleSweepGap is the idle sweep's gap between passes, far shorter than the server's minute so
// the window a test watches holds many of them.
const idleSweepGap = 200 * time.Millisecond

func startIdleSweep(t *testing.T, project string, settle time.Duration) idleSweep {
	t.Helper()
	handler, database, deps := newTestServer(t, testServerOptions{})
	createSuggestionProject(t, handler, project)
	target := createOutcomeIssue(t, handler, project, "Target issue")
	source := createOutcomeIssue(t, handler, project, "Source issue")

	hub := &pglisten.Hub{Owner: "suggestion outcome test", Dial: database.Pool.Listener}
	wake := hub.Wake(EventsChannel)
	connected := make(chan struct{})
	hub.OnConnected(sync.OnceFunc(func() { close(connected) }))
	hubCtx, stopHub := context.WithCancel(context.Background())
	hubDone := make(chan struct{})
	go func() {
		defer close(hubDone)
		hub.Run(hubCtx)
	}()
	t.Cleanup(func() {
		stopHub()
		<-hubDone
	})

	srv := directServer(deps)
	counter := &storetest.StatementCounter{}
	counted := &store.Store{Pool: storetest.Traced(t, database.Pool, counter, nil)}
	schedule := suggestionSchedule{minGap: idleSweepGap, settle: settle, page: suggestionOutcomePage}
	started := time.Now()
	// The sweep reads through the counted pool and observes the horizon through the server's own
	// (store.Pool.Horizon), so the counter sees the sweep's own statements alone.
	runSweep(t, func(ctx context.Context) { runSuggestionOutcomeSweep(ctx, counted.Pool, srv.horizon(), wake, schedule) })
	select {
	case <-connected:
	case <-time.After(10 * time.Second):
		t.Fatal("the hub did not connect within 10s")
	}
	waitUntil(t, 10*time.Second, "the boot pass to set the cursor", func() bool { return outcomeCursor(t, database) != nil })
	// The hub's signal after its LISTEN earns one more pass, no sooner than the gap after the boot
	// pass began; after it the loop is idle.
	time.Sleep(time.Until(started.Add(idleSweepGap + 300*time.Millisecond)))
	storetest.AwaitQuiet(t, counter, 500*time.Millisecond, 10*time.Second)
	return idleSweep{handler: handler, server: srv, counter: counter, target: target, source: source}
}

// With no event written, the sweep makes no statement at all - no pass on a timer, no lease
// attempt - over a window fifteen times its gap between passes, though a suggestion is pending.
func TestTheSuggestionSweepMakesNoStatementWithNoEvents(t *testing.T) {
	// No t.Parallel: this test measures a window of wall-clock time, fifteen gaps of the sweep
	// it runs, and asserts nothing was done in it; a parallel test's CPU is what would stretch
	// that window past a pass the sweep then makes inside it.
	sweep := startIdleSweep(t, "OIDLE", idleSweepGap)
	seedWriteSuggestion(t, sweep.server, "issue", sweep.source, "", "alice", "issue", sweep.target, sweep.target)

	before := sweep.counter.Count()
	time.Sleep(15 * idleSweepGap)
	if made := sweep.counter.Count() - before; made != 0 {
		t.Fatalf("the sweep made %d statements over %v with no event written; want none", made, 15*idleSweepGap)
	}
	if row := readSuggestionOutcome(t, sweep.server, sweep.source, sweep.target); row.Outcome != "ignored" {
		t.Fatalf("outcome = %+v with no event written; want ignored", row)
	}
}

// An event the suggestion's actor writes on the suggested issue wakes the idle sweep through its
// notification on a real pglisten.Hub, and the suggestion is marked acted_on within seconds.
func TestAnEventByTheActorOnTheSuggestedIssueWakesTheSweepAndMarksActedOn(t *testing.T) {
	// No t.Parallel: startIdleSweep measures a quiet window of the sweep it runs
	// (storetest.AwaitQuiet) before the test's own write, and a parallel test's CPU is what
	// would stretch that window.
	sweep := startIdleSweep(t, "OWAKE", idleSweepGap)
	seedWriteSuggestion(t, sweep.server, "issue", sweep.source, "", "alice", "issue", sweep.target, sweep.target)

	postOutcomeMessage(t, sweep.handler, sweep.target, "alice", "Adding the new detail here instead.")
	waitUntil(t, 5*time.Second, "the suggestion to be marked acted_on", func() bool {
		return readSuggestionOutcome(t, sweep.server, sweep.source, sweep.target).Outcome == "acted_on"
	})
	if row := readSuggestionOutcome(t, sweep.server, sweep.source, sweep.target); row.Detail != "the suggested issue was updated directly" {
		t.Fatalf("outcome = %+v; want the direct-update detail", row)
	}
}

// An agent that comments on what it filed and, moments later, updates the suggested issue acted
// on the suggestion. The comment wakes the sweep at once, but an event is judged by only once it
// is settle old, by which time the update is written too, so the rules see both and the acted_on
// rules run before the overridden one: the comment is not judged alone as an override.
func TestWritesMomentsApartAreJudgedTogetherAsActedOn(t *testing.T) {
	const settle = 1500 * time.Millisecond
	sweep := startIdleSweep(t, "OTOGETHER", settle)
	seedWriteSuggestion(t, sweep.server, "issue", sweep.source, "", "alice", "issue", sweep.target, sweep.target)

	postOutcomeMessage(t, sweep.handler, sweep.source, "alice", "Looking into this here first.")
	time.Sleep(300 * time.Millisecond)
	postOutcomeMessage(t, sweep.handler, sweep.target, "alice", "Adding the new detail here instead.")
	waitUntil(t, 10*time.Second, "the suggestion to be judged", func() bool {
		return readSuggestionOutcome(t, sweep.server, sweep.source, sweep.target).Outcome != "ignored"
	})
	if row := readSuggestionOutcome(t, sweep.server, sweep.source, sweep.target); row.Outcome != "acted_on" {
		t.Fatalf("outcome = %+v; want acted_on, the update written moments after the comment", row)
	}
}

// A suggestion younger than settle is not judged, whatever older event a page carries: here
// another person's message on the suggested issue, written before the suggestion, is settle old
// while the suggestion is not. The agent comments on what it filed and, moments later, updates
// the suggested issue; judged at the older event's turn, the comment alone would read as an
// override. Judged once it is settle old, the rules see both writes, and it is acted_on.
func TestAYoungSuggestionIsNotJudgedByAnOlderEventOfItsIssues(t *testing.T) {
	// No t.Parallel: this test reads the event log by id cursor up to the safe horizon, which is
	// instance-wide - it is proven from pg_snapshot_xmin, the oldest transaction id running
	// anywhere on the Postgres server (internal/dispatch/events, horizon.go) - so any open
	// transaction, this package's tests' included, holds it back.
	//
	// settle is the window the pass is given, a parameter of every call below rather than a span
	// the test waits out: an hour, so no load between the age the test checks and the pass's own
	// now() can age the young suggestion past it. The two ages the test turns on are set on the
	// rows themselves (ageIssueEvents, ageSuggestion), as seedWriteSuggestion stamps its row ten
	// milliseconds back rather than sleeping for the gap it needs.
	const settle = time.Hour
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OYOUNG")
	target := createOutcomeIssue(t, handler, "OYOUNG", "Target issue")
	source := createOutcomeIssue(t, handler, "OYOUNG", "Source issue")
	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// Filing the source recorded suggestions of its own; the test judges only the one it seeds.
	if _, err := database.Pool.Exec(ctx, `delete from write_suggestions where source_issue_key = $1`, source); err != nil {
		t.Fatalf("clear the suggestions filing recorded: %v", err)
	}

	postOutcomeMessage(t, handler, target, "bob", "A note of my own on the target.")
	// bob's message is settle old, and still older than the suggestion the test seeds next.
	ageIssueEvents(t, database, target, settle+time.Minute)
	seedWriteSuggestion(t, srv, "issue", source, "", "alice", "issue", target, target)
	postOutcomeMessage(t, handler, source, "alice", "Looking into this here first.")

	if age := suggestionAge(t, database, source); age >= settle {
		t.Fatalf("the suggestion is already %v old; the test needs it younger than %v here", age, settle)
	}
	if _, err := sweepSuggestionOutcomePages(ctx, database.Pool, safeHorizon(t, srv), suggestionOutcomePage, settle); err != nil {
		t.Fatalf("sweep while the suggestion is young: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "ignored" {
		t.Fatalf("outcome = %+v while the suggestion is younger than settle; want ignored until it is settle old", row)
	}

	postOutcomeMessage(t, handler, target, "alice", "Adding the new detail here instead.")
	// The suggestion is settle old now, with both of alice's writes still after it and both made
	// while it was young: what the pass has to judge together.
	ageSuggestion(t, database, source, settle+30*time.Second)
	if _, err := sweepSuggestionOutcomePages(ctx, database.Pool, safeHorizon(t, srv), suggestionOutcomePage, settle); err != nil {
		t.Fatalf("sweep once the suggestion is settle old: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "acted_on" {
		t.Fatalf("outcome = %+v; want acted_on, the update written moments after the comment", row)
	}
}

// A suggestion is recorded in a transaction of its own, after the write that offered it commits
// (persistSuggestions), and that transaction can stay open across a later write: the row carries
// a created_at from before an event it never saw - now() is the transaction's start - and becomes
// visible only after a pass has read that event and moved its cursor past it. The next pass
// judges it against that event all the same, with no further event written, because a suggestion
// no pass has swept is judged against all its events rather than only the page's.
func TestASuggestionCommittedAfterALaterStartedEventIsStillJudged(t *testing.T) {
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "ORACE")
	target := createOutcomeIssue(t, handler, "ORACE", "Target issue")
	source := createOutcomeIssue(t, handler, "ORACE", "Source issue")
	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("first sweep: %v", err)
	}

	// The suggestion's transaction starts before the event, which is what makes its created_at
	// older than one, and takes no transaction id until it inserts below, so it holds back
	// neither the event's commit nor the safe horizon the pass between them reads.
	recording, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("open the suggestion's transaction: %v", err)
	}
	defer func() { _ = recording.Rollback(context.Background()) }()
	// No wait: this transaction's now() is its start, before the event's own transaction, which
	// is what makes the suggestion's created_at the older of the two.
	postOutcomeMessage(t, handler, target, "alice", "Adding the new detail here instead.")

	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("sweep before the suggestion commits: %v", err)
	}
	if cursor, last := outcomeCursor(t, database), lastEventID(t, database); cursor == nil || *cursor != last {
		t.Fatalf("cursor = %v after the pass that read the event; want the last event id %d", cursor, last)
	}
	if _, err := recording.Exec(ctx, `
		insert into write_suggestions
			(source_kind, source_issue_key, actor_kind, actor_id, role, rank, suggested_kind, suggested_id, suggested_issue_key)
		values ('issue', $1, 'user', 'alice', 'related', 0, 'issue', $2, $2)
	`, source, target); err != nil {
		t.Fatalf("record the suggestion: %v", err)
	}
	if err := recording.Commit(ctx); err != nil {
		t.Fatalf("commit the suggestion: %v", err)
	}

	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("sweep after the suggestion commits: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "acted_on" {
		t.Fatalf("outcome = %+v; want acted_on, from the event the earlier pass had already read", row)
	}
}

// A backlog longer than a page is read page after page within one sweep: the event that resolves
// the suggestion is on the third page of two events each, and one page alone reports itself full
// and leaves the suggestion pending.
func TestASweepReadsEveryPageOfABacklog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OPAGE")
	target := createOutcomeIssue(t, handler, "OPAGE", "Target issue")
	source := createOutcomeIssue(t, handler, "OPAGE", "Source issue")
	noise := createOutcomeIssue(t, handler, "OPAGE", "Unrelated issue")
	seedWriteSuggestion(t, srv, "issue", source, "", "alice", "issue", target, target)
	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	for range 4 {
		postOutcomeMessage(t, handler, noise, "bob", "Unrelated progress.")
	}
	postOutcomeMessage(t, handler, target, "alice", "Adding the new detail here instead.")

	const page = 2
	full, _, err := judgeSuggestionPageInTransaction(ctx, database.Pool, safeHorizon(t, srv), page, 0)
	if err != nil {
		t.Fatalf("one page: %v", err)
	}
	if !full {
		t.Fatal("a page of two out of five new events reported itself short")
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "ignored" {
		t.Fatalf("outcome = %+v after the first page; want ignored, its event two pages on", row)
	}
	if _, err := sweepSuggestionOutcomePages(ctx, database.Pool, safeHorizon(t, srv), page, 0); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "acted_on" {
		t.Fatalf("outcome = %+v after the sweep; want acted_on", row)
	}
	if cursor, last := outcomeCursor(t, database), lastEventID(t, database); cursor == nil || *cursor != last {
		t.Fatalf("cursor = %v after the sweep; want the last event id %d", cursor, last)
	}
}

// touchedPassCost is what one page of a later pass cost: its statements, and the rows it read
// from each table it reads (sequential reads plus live rows fetched through an index).
type touchedPassCost struct {
	statements       int64
	suggestionRows   int64
	eventRows        int64
	sequentialReads  int64
	resolvedAsWanted bool
}

// oldSuggestions is how many old suggestions measureTouchedPass seeds: half on an issue nothing
// writes to, half on an ask on a project document nothing writes to.
const oldSuggestions = 1000

// measureTouchedPass seeds oldSuggestions suggestions on an issue and a project document nothing
// writes to, pending when pending is set and already resolved when not, lets a sweep pass over
// them, then records one new suggestion, the event that resolves it and an event on another
// project document, and measures the one page that judges them. The page runs in a transaction of
// the test's own, so pg_stat_xact_user_tables reads exactly that page's row counts before it
// commits. The document event is there so the page looks pending suggestions up by document too,
// the lookup migration 0155 indexed.
//
// Both measurements seed the same rows, so the table, its indexes and its statistics are the same
// size and Postgres plans each read the same way; the only difference is whether the old rows are
// pending. The page's connection also runs with enable_seqscan off: a test's table holds too few
// rows for Postgres to plan a read as it would at production's size (storetest.Planned says the
// same), and with it off a read that has an index reads only the rows it matches, while a read
// with none is still a sequential scan of every row. The table is vacuumed before the page, as
// autovacuum leaves it, so the dead index entries of rows the earlier pass marked swept are not
// part of the count.
func measureTouchedPass(t *testing.T, project string, pending bool) touchedPassCost {
	t.Helper()
	ctx := context.Background()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, project)
	target := createOutcomeIssue(t, handler, project, "Target issue")
	source := createOutcomeIssue(t, handler, project, "Source issue")
	quietIssue := createOutcomeIssue(t, handler, project, "Quiet issue")
	quietDocument := createProjectDocument(t, handler, project, "Quiet notes", "# Quiet notes\n")
	activeDocument := createProjectDocument(t, handler, project, "Active notes", "# Active notes\n")
	asked := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+quietDocument.ID+"/asks", map[string]any{
		"question": "Which quiet option do we keep?",
	}, "alice")
	if asked.Code != http.StatusCreated {
		t.Fatalf("open an ask on the quiet document: status=%d body=%s", asked.Code, asked.Body.String())
	}
	quietAsk := decodeBody[testAskWithSuggestions](t, asked).ID
	outcome := "overridden"
	if pending {
		outcome = "ignored"
	}
	if _, err := database.Pool.Exec(ctx, `
		insert into write_suggestions
			(source_kind, source_issue_key, source_artifact_id, source_ask_id, actor_kind, actor_id, role, rank,
			 suggested_kind, suggested_id, suggested_issue_key, outcome)
		select 'issue', $1::text, null, null, 'user', 'alice', 'related', 0, 'issue', $1::text || '-' || n, $1::text, $5
		  from generate_series(1, $4::int / 2) n
		union all
		select 'ask', null, $2::uuid, $3::uuid, 'user', 'alice', 'related', 0, 'issue', $1::text || '-' || n, $1::text, $5
		  from generate_series(1, $4::int / 2) n
	`, quietIssue, quietDocument.ID, quietAsk, oldSuggestions, outcome); err != nil {
		t.Fatalf("seed old suggestions: %v", err)
	}
	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	var stillPending int
	if err := database.Pool.QueryRow(ctx, `
		select count(*) from write_suggestions
		 where (source_issue_key = $1 or source_artifact_id = $2) and suggested_id like $1 || '-%' and outcome = 'ignored'
	`, quietIssue, quietDocument.ID).Scan(&stillPending); err != nil {
		t.Fatalf("count old pending suggestions: %v", err)
	}
	if want := map[bool]int{true: oldSuggestions, false: 0}[pending]; stillPending != want {
		t.Fatalf("%d of the old suggestions are pending after the first sweep; want %d", stillPending, want)
	}
	seedWriteSuggestion(t, srv, "issue", source, "", "alice", "issue", target, target)
	postOutcomeMessage(t, handler, target, "alice", "Adding the new detail here instead.")
	commented := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+activeDocument.ID+"/comments", map[string]any{
		"body": "Progress on the active notes.",
	}, "bob")
	if commented.Code != http.StatusCreated {
		t.Fatalf("comment on the active document: status=%d body=%s", commented.Code, commented.Body.String())
	}
	if _, err := database.Pool.Exec(ctx, `vacuum analyze write_suggestions`); err != nil {
		t.Fatalf("vacuum write_suggestions: %v", err)
	}
	// The events table's statistics are this half's own rows, as write_suggestions' are. A
	// database the harness recycled carries the statistics of whatever the test before it wrote:
	// its reset is a truncate, which empties a table without analysing it, so pg_statistic still
	// describes the rows that stood there (the reset of storetest.Open's free list). That decided
	// the plan of the two rules that probe events by issue and by document, and so how many rows
	// of events the page read: 8 for a half on a freshly cloned database against 10 for a half on
	// the database recycled from the test that analyses a log of 200,000 events, which is CI run
	// 41789493503's failure, reproduced by running that test and this one in one go. Both halves
	// hold the same nine events, so statistics read from them are the same statistics.
	if _, err := database.Pool.Exec(ctx, `analyze events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}

	// The page is bounded by this database's own event head, not by the safe horizon. Both are
	// an id every event at or below which is final, and here every event is this test's and
	// committed; but the horizon is proven from pg_snapshot_xmin, the oldest transaction id
	// running anywhere on the instance (internal/dispatch/events, horizon.go), so a transaction
	// another test or the harness's reset workers held open while one of the two measurements
	// ran left that half a shorter range of the log to read, and the two then disagreed about
	// how many rows of events a page reads - two rows apart in CI run 41762847452. The head is
	// the same bound for both halves whatever else is running.
	safe := eventHead(t, database)
	counter := &storetest.StatementCounter{}
	counted := storetest.Traced(t, database.Pool, counter, map[string]string{"enable_seqscan": "off"})
	tx, err := counted.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the page's transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	before := counter.Count()
	if _, _, err := judgeSuggestionPage(ctx, tx, safe, suggestionOutcomePage, 0); err != nil {
		t.Fatalf("judge the page: %v", err)
	}
	cost := touchedPassCost{statements: counter.Count() - before}
	rows, err := tx.Query(ctx, `
		select relname, seq_tup_read, coalesce(idx_tup_fetch, 0)
		  from pg_stat_xact_user_tables
		 where relname in ('write_suggestions', 'events')`)
	if err != nil {
		t.Fatalf("read the page's table statistics: %v", err)
	}
	for rows.Next() {
		var table string
		var sequential, fetched int64
		if err := rows.Scan(&table, &sequential, &fetched); err != nil {
			t.Fatalf("scan the page's table statistics: %v", err)
		}
		cost.sequentialReads += sequential
		switch table {
		case "write_suggestions":
			cost.suggestionRows = sequential + fetched
		case "events":
			cost.eventRows = sequential + fetched
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the page's table statistics: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the page: %v", err)
	}
	cost.resolvedAsWanted = readSuggestionOutcome(t, srv, source, target).Outcome == "acted_on"
	return cost
}

// A thousand suggestions still pending on an issue nothing writes to cost a later pass nothing: the
// page that judges a new suggestion and the event that resolves it makes as many statements, and
// reads as many rows of write_suggestions and of events, with them as with none pending (the same
// thousand rows, resolved), and reads no table sequentially. The pass of the previous release read
// every pending row on every pass.
func TestOldPendingSuggestionsOnOtherIssuesCostAPassNothing(t *testing.T) {
	// No t.Parallel: this test reads the event log by id cursor, and the safe horizon it waits
	// on is instance-wide — any open transaction anywhere on the Postgres server holds it back.
	without := measureTouchedPass(t, "OCOST", false)
	with := measureTouchedPass(t, "OCOST", true)
	t.Logf("none pending: %+v; 1,000 pending: %+v", without, with)
	if !without.resolvedAsWanted || !with.resolvedAsWanted {
		t.Fatalf("the page did not mark the new suggestion acted_on (without old pending: %v, with: %v)", without.resolvedAsWanted, with.resolvedAsWanted)
	}
	if with.statements != without.statements {
		t.Errorf("the page made %d statements with 1,000 old pending suggestions and %d without", with.statements, without.statements)
	}
	if with.suggestionRows != without.suggestionRows {
		t.Errorf("the page read %d write_suggestions rows with 1,000 old pending suggestions and %d without", with.suggestionRows, without.suggestionRows)
	}
	if with.eventRows != without.eventRows {
		t.Errorf("the page read %d events rows with 1,000 old pending suggestions and %d without", with.eventRows, without.eventRows)
	}
	if with.sequentialReads != 0 || without.sequentialReads != 0 {
		t.Errorf("the page read %d rows sequentially with 1,000 old pending suggestions and %d without; want none", with.sequentialReads, without.sequentialReads)
	}
}

// safeHorizon is the safe horizon of srv's one events.Horizon once an observation taken during
// the call is proven safe (events.Horizon.Await), so it is at or above every event committed
// before the call: what a test that has just written what it wants read needs.
func safeHorizon(t *testing.T, srv *server) int64 {
	t.Helper()
	safe, err := srv.horizon().Await(t.Context())
	if err != nil {
		t.Fatalf("await the safe horizon: %v", err)
	}
	return safe
}

// suggestionOutcomeAt is when a suggestion was judged, nil while it is still pending.
func suggestionOutcomeAt(t *testing.T, srv *server, sourceIssueKey, suggestedID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := srv.deps.Store.Pool.QueryRow(context.Background(), `
		select outcome_at from write_suggestions where source_issue_key = $1 and suggested_id = $2
	`, sourceIssueKey, suggestedID).Scan(&at); err != nil {
		t.Fatalf("read when the suggestion was judged: %v", err)
	}
	return at
}

// An event that can still commit is never passed over. Transaction A appends an event on the
// suggested issue, the one that resolves the suggestion, and stays open while transaction B
// appends a later id and commits, so the highest committed id stands above an id that can still
// commit. A pass run then reads only up to the id the log is final through
// (suggestionOutcomeFloor): A holds a write lock on this database's events, so no mark the pass
// takes counts and the floor falls back to the horizon already proven, which stands below A's
// id while A is open. It judges nothing, leaves the cursor below that id, and asks for a pass of
// its own no sooner than the loop's gap rather than spinning on the gap above the floor.
// Once A commits, the next pass reads its event and judges the suggestion by it, once. A pass
// reading `id > cursor` uncapped moves the cursor past A's event on the first pass, and the
// suggestion is then never judged.
func TestTheSuggestionSweepTrailsTheSafeHorizon(t *testing.T) {
	ctx := t.Context()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OHORIZON")
	target := createOutcomeIssue(t, handler, "OHORIZON", "Target issue")
	source := createOutcomeIssue(t, handler, "OHORIZON", "Source issue")
	other := createOutcomeIssue(t, handler, "OHORIZON", "Other issue")
	if err := sweepSuggestionOutcomes(t, srv); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	// Filing the source recorded suggestions of its own; the test judges only the one it seeds,
	// whose actor is the one appendOutOfOrder writes its events as.
	if _, err := database.Pool.Exec(ctx, `delete from write_suggestions where source_issue_key = $1`, source); err != nil {
		t.Fatalf("clear the suggestions filing recorded: %v", err)
	}
	// created_at ten milliseconds back, as seedWriteSuggestion writes it: the events below must
	// postdate this row, and the stamp is that gap.
	if _, err := database.Pool.Exec(ctx, `
		insert into write_suggestions
			(source_kind, source_issue_key, actor_kind, actor_id, role, rank, suggested_kind, suggested_id, suggested_issue_key, created_at)
		values ('issue', $1, 'session', 'seed', 'related', 0, 'issue', $2, $2, now() - interval '10 milliseconds')
	`, source, target); err != nil {
		t.Fatalf("record the suggestion: %v", err)
	}

	schedule := suggestionSchedule{minGap: time.Second, page: suggestionOutcomePage}
	// One floor across the three passes below, as the loop keeps one across its own
	// (runSuggestionOutcomeSweep): the marks it takes are what a later pass reads the log up to.
	floor := &suggestionOutcomeFloor{horizon: srv.horizon()}
	low, _, commitLow := appendOutOfOrder(t, database, deps.Events, target, other)
	started := time.Now()
	next, err := suggestionOutcomePass(ctx, database.Pool, floor, schedule)
	if err != nil {
		t.Fatalf("the pass run while the lower id is still open: %v", err)
	}
	switch cursor := outcomeCursor(t, database); {
	case cursor == nil:
		t.Fatalf("the cursor is unset after a pass run while event %d could still commit", low)
	case *cursor >= low:
		t.Fatalf("cursor = %d after a pass run while event %d could still commit; want it below that id", *cursor, low)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "ignored" {
		t.Fatalf("outcome = %+v while the event that resolves the suggestion could still commit; want ignored", row)
	}
	if next.IsZero() || next.Before(started.Add(schedule.minGap)) {
		t.Fatalf("the pass held above the horizon asked to run again at %v, %v after it started; want a due time no sooner than its %v gap",
			next, next.Sub(started), schedule.minGap)
	}

	commitLow()
	if safe := safeHorizon(t, srv); safe < low {
		t.Fatalf("the safe horizon is %d once event %d has committed; want it at or above that id", safe, low)
	}
	if _, err := suggestionOutcomePass(ctx, database.Pool, floor, schedule); err != nil {
		t.Fatalf("the pass run once the lower id has committed: %v", err)
	}
	if row := readSuggestionOutcome(t, srv, source, target); row.Outcome != "acted_on" || row.Detail != "the suggested issue was updated directly" {
		t.Fatalf("outcome = %+v once the event that resolves the suggestion has committed; want acted_on by the direct update", row)
	}
	switch cursor := outcomeCursor(t, database); {
	case cursor == nil:
		t.Fatalf("the cursor is unset after the pass that read event %d", low)
	case *cursor < low:
		t.Fatalf("cursor = %d after the pass that read event %d; want it at or above that id", *cursor, low)
	}
	judged := suggestionOutcomeAt(t, srv, source, target)
	if _, err := suggestionOutcomePass(ctx, database.Pool, floor, schedule); err != nil {
		t.Fatalf("the pass after the suggestion was judged: %v", err)
	}
	if again := suggestionOutcomeAt(t, srv, source, target); judged == nil || again == nil || !again.Equal(*judged) {
		t.Fatalf("the suggestion was judged at %v and again at %v; want one judgement", judged, again)
	}
}

// A pass reads the log and moves its cursor at once while a transaction in another database of
// the same Postgres instance holds a transaction id. The pass caps its page at a writer floor
// (suggestionOutcomeFloor, events.Horizon.WriterFloor), not at the safe horizon. Both are an id
// every event at or below which is final, which is all a page needs (judgeSuggestionPage); they
// differ only in what they wait on. The horizon is proven from pg_snapshot_xmin, the oldest
// transaction id running anywhere on the instance (internal/dispatch/events, horizon.go), so an
// open transaction in any database on it - another service's write, an operator's psql session,
// a `create database ... template`, another package's test - holds it back, and a process whose
// horizon has proven nothing yet, as one just started has, can only await it: the whole hold,
// with every judgement the sweep owes behind it. The floor counts only the transactions that
// hold a write lock on this database's events (oldestEventWriterSQL), which are the only ones
// that can hold an id of this log open, so a mark taken now counts at once and the pass answers
// under a hold of any length.
func TestTheSuggestionSweepDoesNotWaitOnATransactionInAnotherDatabase(t *testing.T) {
	// No t.Parallel: holdClusterTransactionID below holds a transaction id on the whole Postgres
	// instance for thirty seconds, which keeps every horizon on it from being proven
	// (internal/dispatch/events, horizon.go), and this test bounds how long one pass takes.
	ctx := t.Context()
	handler, database, deps := newTestServer(t, testServerOptions{})
	// srv's is the one events.Horizon of a process that has just started: nothing proven and no
	// observation taken, as the server's is on its boot pass.
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OELSEWHERE")
	target := createOutcomeIssue(t, handler, "OELSEWHERE", "Target issue")
	postOutcomeMessage(t, handler, target, "alice", "A message on the target issue.")
	newest := lastEventID(t, database)
	if cursor := outcomeCursor(t, database); cursor != nil {
		t.Fatalf("cursor = %d before any pass has run; want it null, so this pass is a just-started process's first", *cursor)
	}

	holdClusterTransactionID(t, 30*time.Second)
	schedule := suggestionSchedule{minGap: time.Second, settle: time.Minute, page: suggestionOutcomePage}
	began := time.Now()
	if _, err := suggestionOutcomePass(ctx, database.Pool, &suggestionOutcomeFloor{horizon: srv.horizon()}, schedule); err != nil {
		t.Fatalf("the pass run while another database holds a transaction id: %v", err)
	}
	if waited := time.Since(began); waited > 5*time.Second {
		t.Fatalf("the pass took %v while another database held a transaction id for thirty seconds; want a pass that waits on no transaction of another database", waited)
	}
	switch cursor := outcomeCursor(t, database); {
	case cursor == nil:
		t.Fatalf("the cursor is unset after a pass run while another database held a transaction id; want it at %d, the newest event, which the floor bounds though no horizon is proven", newest)
	case *cursor != newest:
		t.Fatalf("cursor = %d after that pass; want %d, the newest event: the floor the pass read bounds the log where the horizon it could only await does not", *cursor, newest)
	}
}

// No prepared transaction of this database can hold an event id, and the floor stays below one
// that holds the lock an insert takes anyway.
//
// PREPARE TRANSACTION keeps a transaction, its locks and its right to commit, but leaves it no
// backend, and pg_locks lists its locks with a null pid. So the arm of oldestEventWriterSQL that
// joins a lock on events to its holder's transactionid lock on pid matches nothing for a
// prepared transaction, and a floor read from that arm alone would count a mark while one could
// still commit an event below it. Two things close that gap, and this test is both halves.
//
// Postgres refuses to prepare a transaction that has executed NOTIFY, and the trigger on every
// insert into events notifies dispatch_events (migration 0107), so a transaction that has drawn
// an event id cannot be prepared at all: the first half. And the floor's second arm counts every
// prepared transaction of this database straight from pg_prepared_xacts, whatever it holds, so
// one holding the RowExclusiveLock an insert takes keeps every mark the reader has from counting
// and leaves the floor at the horizon already proven: the second half, which is what would hold
// were a later schema to insert an event without notifying.
//
// The server has to allow prepared transactions: with max_prepared_transactions at 0, the stock
// Postgres image's setting, none can be prepared and the test has no subject. Every CI job that
// runs this package raises it (.github/workflows/ci.yaml, envoy-go and envoy-go-race-api), so in
// CI a zero is a failure rather than a skip: a skip there would be a pass proving nothing, in the
// one place this test has to hold.
func TestNoPreparedTransactionHoldsAnEventIDAndTheFloorTrailsOneAnyway(t *testing.T) {
	// No t.Parallel: this test leaves a prepared transaction on the Postgres instance across the
	// floor's statements, which holds the instance-wide safe horizon back for every test beside
	// it.
	ctx := t.Context()
	handler, database, deps := newTestServer(t, testServerOptions{})
	srv := directServer(deps)
	var allowed int
	if err := database.Pool.QueryRow(ctx, `select current_setting('max_prepared_transactions')::int`).Scan(&allowed); err != nil {
		t.Fatalf("read max_prepared_transactions: %v", err)
	}
	if allowed == 0 {
		const why = "this Postgres server runs with max_prepared_transactions = 0 and can prepare no transaction"
		if os.Getenv("CI") != "" {
			t.Fatalf("%s; the job must raise it", why)
		}
		t.Skip(why)
	}
	createSuggestionProject(t, handler, "OPREPARED")
	target := createOutcomeIssue(t, handler, "OPREPARED", "Target issue")
	other := createOutcomeIssue(t, handler, "OPREPARED", "Other issue")
	// A connection of this test's own: PREPARE TRANSACTION ends the session's transaction and
	// leaves it on the server, which no connection handed back to the pool may be left in.
	config := database.Pool.Config().ConnConfig.Copy()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to prepare a transaction: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	gid := "dispatch-floor-" + config.Database

	// A transaction that has appended an event cannot be prepared: its insert notified.
	appended, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction that appends an event: %v", err)
	}
	held, err := deps.Events.Append(ctx, appended, model.Event{
		IssueKey: new(target),
		Type:     "test.prepared",
		Actor:    model.Actor{Kind: "session", ID: "seed"},
		Payload:  map[string]any{},
	})
	if err != nil {
		t.Fatalf("append the event the transaction holds: %v", err)
	}
	switch _, err := appended.Exec(ctx, fmt.Sprintf(`prepare transaction '%s'`, gid)); {
	case err == nil:
		_, _ = database.Pool.Exec(context.Background(), fmt.Sprintf(`rollback prepared '%s'`, gid))
		t.Fatalf("a transaction holding event %d was prepared; want Postgres to refuse it, since the insert's trigger notified dispatch_events", held.ID)
	case !strings.Contains(err.Error(), "LISTEN, UNLISTEN, or NOTIFY"):
		t.Fatalf("preparing a transaction holding event %d failed with %v; want the refusal of a transaction that has notified", held.ID, err)
	}
	if err := appended.Rollback(ctx); err != nil {
		t.Fatalf("roll back the transaction that could not be prepared: %v", err)
	}

	// The horizon this process has proven, which is the floor's only bound once no mark counts.
	proven := safeHorizon(t, srv)
	if proven == 0 {
		t.Fatalf("no horizon is proven before the transaction below is prepared; this test could not tell the floor's fallback from a mark it took")
	}
	// A prepared transaction holding the lock an insert takes on events, and its transaction id,
	// as a writer holds both (the writer invariant, events.Broker.Append).
	locking, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the transaction to prepare: %v", err)
	}
	if _, err := locking.Exec(ctx, `select pg_current_xact_id()`); err != nil {
		t.Fatalf("take the prepared transaction's id: %v", err)
	}
	if _, err := locking.Exec(ctx, `lock table events in row exclusive mode`); err != nil {
		t.Fatalf("lock events as an insert does: %v", err)
	}
	if _, err := locking.Exec(ctx, fmt.Sprintf(`prepare transaction '%s'`, gid)); err != nil {
		t.Fatalf("prepare the transaction holding the lock on events: %v", err)
	}
	resolved := false
	t.Cleanup(func() {
		if resolved {
			return
		}
		if _, err := database.Pool.Exec(context.Background(), fmt.Sprintf(`rollback prepared '%s'`, gid)); err != nil {
			t.Errorf("roll back the prepared transaction %q, which would otherwise stay on the server: %v", gid, err)
		}
	})
	// An event commits while it is prepared, so the mark the floor takes sees a newest id above
	// the horizon already proven: a floor that counted that mark would stand above an id a
	// writer of this database could still be holding.
	postOutcomeMessage(t, handler, other, "alice", "Unrelated progress on another issue.")
	newest := lastEventID(t, database)
	if newest <= proven {
		t.Fatalf("the newest committed id is %d, not above the proven horizon %d: this test proves nothing", newest, proven)
	}

	floor := &suggestionOutcomeFloor{horizon: srv.horizon()}
	read, gap, err := floor.read(ctx)
	if err != nil {
		t.Fatalf("read the floor while a prepared transaction holds the lock on events: %v", err)
	}
	if read != proven {
		t.Fatalf("the floor is %d while a prepared transaction holds the lock an insert takes on events; want the horizon %d already proven, the only bound left while no mark counts", read, proven)
	}
	if !gap {
		t.Fatalf("the floor %d reports no id above it while the log holds %d; want the pass told it read less than the log holds", read, newest)
	}

	if _, err := database.Pool.Exec(ctx, fmt.Sprintf(`rollback prepared '%s'`, gid)); err != nil {
		t.Fatalf("roll back the prepared transaction: %v", err)
	}
	resolved = true
	switch read, _, err := floor.read(ctx); {
	case err != nil:
		t.Fatalf("read the floor once the prepared transaction has ended: %v", err)
	case read < newest:
		t.Fatalf("the floor is %d once the prepared transaction has ended; want it at %d, the newest committed id, which nothing holds open any more", read, newest)
	}
}

// The first pass of a just-started process starts at the newest event, not at 0. A fresh
// events.Horizon has proven nothing, and one snapshot never proves its own A (events.Horizon), so
// the one observation the first pass takes leaves H at 0. With the cursor still null, a pass that
// read that 0 as its safe horizon would move the cursor to the highest event id at or below it
// (startSuggestionCursor), which is 0, and every later pass would then page through the whole
// event log before reaching anything it has to judge.
func TestTheFirstPassOfAJustStartedProcessStartsAtTheNewestEvent(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	// srv's is the one events.Horizon of a process that has just started: on the store's horizon
	// pool, with nothing proven and no observation taken, as the server's is on its boot pass.
	srv := directServer(deps)
	createSuggestionProject(t, handler, "OSTART")
	target := createOutcomeIssue(t, handler, "OSTART", "Target issue")
	postOutcomeMessage(t, handler, target, "alice", "A message on the target issue")
	if cursor := outcomeCursor(t, database); cursor != nil {
		t.Fatalf("cursor = %d before any pass has run; want it null", *cursor)
	}

	newest := lastEventID(t, database)
	schedule := suggestionSchedule{minGap: time.Second, settle: time.Minute, page: suggestionOutcomePage}
	if _, err := suggestionOutcomePass(t.Context(), database.Pool, &suggestionOutcomeFloor{horizon: srv.horizon()}, schedule); err != nil {
		t.Fatalf("the first pass of a just-started process: %v", err)
	}
	switch cursor := outcomeCursor(t, database); {
	case cursor == nil:
		t.Fatalf("the cursor is unset after the first pass; want it at event %d, the newest there is", newest)
	case *cursor != newest:
		t.Fatalf("cursor = %d after the first pass of a just-started process; want %d, the newest event id, so the passes after it read the log from there rather than from the start",
			*cursor, newest)
	}
}
