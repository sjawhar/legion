//go:build apiprofile

// TEMPORARY. The request-cost measurement dispatch://INFRA-1075 asks where one api test's 217 ms
// of body and 66 ms of teardown go: into a handler's own work, into work every test repeats
// unshared, or into the harness around it. It is one file behind one build tag, run by one
// TEMPORARY step of the envoy-go job, and it comes out with that step before the pull request's
// final pair of measurement runs. Nothing outside this file knows the tag exists: no other file
// names it.
//
// It lives in package api because an api request's cost is what it measures, and it builds its
// server the way every api test builds one (newTestServer over a storetest clone) and makes the
// three requests the floor measurement timed (createReferenceAPIProject, createReferenceAPIIssue,
// one GET), in the order a test makes them. The one thing it does that a test does not is repeat
// them: a request made profileRounds times separates what every request costs from what the
// first one pays once - a cold shared cache, a plan built once, a pool connection opened on
// demand, a background worker started on first use - which is the difference between work a test
// needs and work a run of tests could share. A fourth request beside the three, a second issue
// in the project the round just made, prices what the first issue in a project pays extra.
//
// Two halves are reported, because a test's time is spent in two processes. The step profiles
// this test with `go test -cpuprofile`, which sees this Go process alone: the Postgres it talks
// to is a container of its own and not one microsecond of its CPU is in that profile. The other
// half is pg_stat_statements, which this file reads itself, over a window it opens by resetting
// the counters once its server is built and closes when its loop is done. TestAPIProfileStatements
// prints the same tables for whatever window its caller opened, which is how the step reports the
// four ordinary tests it profiles beside this one.

package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// profileRounds is how many times the loop makes the three requests. Forty keeps the loop's own
// wall well above the package setup a whole-binary profile also holds, and leaves this test at
// well under a minute. APIPROFILE_ROUNDS replaces it, which is how a local run keeps short.
const profileRounds = 40

// profileSettle is newTestServer's own default settle, repeated here for the idle document
// service built at the end, so its New and its Shutdown are priced on the settings a test's
// service runs with.
const profileSettle = 20 * time.Millisecond

// ---------------------------------------------------------------- tables

// profileTable prints as a Markdown table, which pastes into a report unchanged.
type profileTable struct {
	title  string
	header []string
	rows   [][]string
}

func (t *profileTable) add(cells ...string) { t.rows = append(t.rows, cells) }

func (t *profileTable) print() {
	widths := make([]int, len(t.header))
	for i, cell := range t.header {
		widths[i] = len(cell)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if i < len(widths) && len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}
	line := func(cells []string) {
		out := "|"
		for i, cell := range cells {
			out += fmt.Sprintf(" %-*s |", widths[i], cell)
		}
		fmt.Println(out)
	}
	fmt.Printf("\n%s\n\n", t.title)
	line(t.header)
	rule := make([]string, len(t.header))
	for i := range rule {
		for range widths[i] {
			rule[i] += "-"
		}
	}
	line(rule)
	for _, row := range t.rows {
		line(row)
	}
}

func profileMS(d time.Duration) string {
	return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond))
}

func profileFloat(value float64) string { return fmt.Sprintf("%.1f", value) }

// profileSeries is one measured thing's times, in the order they were taken.
type profileSeries struct {
	name    string
	samples []time.Duration
}

func (s *profileSeries) add(d time.Duration) { s.samples = append(s.samples, d) }

// steady is what every sample but the first averaged: the cost of the request itself, once
// whatever the first one warms up is warm.
func (s *profileSeries) steady() time.Duration {
	if len(s.samples) < 2 {
		return 0
	}
	var sum time.Duration
	for _, sample := range s.samples[1:] {
		sum += sample
	}
	return sum / time.Duration(len(s.samples)-1)
}

// row renders the series as one line of the per-request table: what the first one cost, what the
// rest averaged, and the spread of the rest. The first is kept out of the mean on purpose: it is
// the one that pays for whatever a run warms up.
func (s *profileSeries) row() []string {
	if len(s.samples) < 2 {
		return []string{s.name, strconv.Itoa(len(s.samples)), "-", "-", "-", "-", "-"}
	}
	sorted := slices.Clone(s.samples[1:])
	slices.Sort(sorted)
	return []string{
		s.name, strconv.Itoa(len(s.samples)), profileMS(s.samples[0]), profileMS(s.steady()),
		profileMS(sorted[len(sorted)/2]), profileMS(sorted[0]), profileMS(sorted[len(sorted)-1]),
	}
}

// ---------------------------------------------------------------- the server's own half

// profileMeasured is the window pg_stat_statements holds, as rows this file can print: a role
// guessed from the statement, whether a client issued the statement (toplevel) or it ran inside
// one that did (pg_stat_statements.track=all records both), the statement with its whitespace
// folded, its calls and its execution time in milliseconds. The report hides its own statements,
// which the server counts like any other.
const profileMeasured = `
with measured as (
    select
        case
            when query ~* '^\s*(create|drop|alter)\s+database' then 'harness'
            when query ~* '^\s*select \$[0-9]+ from only ' then 'fk check'
            when query ~* '^\s*insert into people \(email\)' then 'seed'
            when query ~* 'schema_migrations' then 'migration'
            when query ~* '^\s*(create|alter|drop)\s+(unique\s+)?(table|index|type|extension|schema|function|procedure|trigger|view|sequence|materialized|publication)\y' then 'migration'
            when query ~* '^\s*(comment\s+on|grant|revoke|analyze|vacuum|cluster|reindex)\y' then 'migration'
            when query ~* '^\s*(begin|commit|rollback|savepoint|release|set|show|discard|deallocate|listen|unlisten)\y' then 'session'
            when query ~* 'pg_advisory|pg_try_advisory' then 'lock'
            when query ~* 'pg_catalog\.|current_setting' then 'driver'
            else 'handler'
        end as role,
        toplevel,
        regexp_replace(btrim(query), '\s+', ' ', 'g') as statement,
        calls,
        total_exec_time
    from pg_stat_statements
    where query !~* 'pg_stat_statements|from measured'
)
`

// profileStatementColumns is the row both statement tables print, as text, so every cell arrives
// formatted and the two tables cannot disagree about a column.
const profileStatementColumns = `select role,
    case when toplevel then 'yes' else 'no' end,
    sum(calls)::text,
    round(sum(total_exec_time)::numeric, 1)::text,
    round((sum(total_exec_time) / greatest(sum(calls), 1))::numeric, 3)::text,
    left(statement, 96)
from measured
group by role, toplevel, statement
`

const (
	profileByTotalTime = profileMeasured + profileStatementColumns + "order by sum(total_exec_time) desc limit 20"
	profileByCalls     = profileMeasured + profileStatementColumns + "order by sum(calls) desc limit 20"
	// Top-level statements only: a nested one is counted again inside the one that called it, so
	// summing both would price the same work twice.
	profileByRole = profileMeasured + `select role, sum(calls)::bigint, sum(total_exec_time)
from measured
where toplevel
group by role
order by 3 desc`
)

// profileWindow is what the server executed over one window, by role, in milliseconds.
type profileWindow struct {
	calls map[string]int64
	ms    map[string]float64
}

func (w profileWindow) totalMS() float64 {
	var sum float64
	for _, ms := range w.ms {
		sum += ms
	}
	return sum
}

// profileStatements prints what pg_stat_statements holds for the window named, and answers its
// totals by role. A server with the extension unloaded is a note rather than a failure: the Go
// half of this measurement stands without it.
func profileStatements(t *testing.T, window string) profileWindow {
	t.Helper()
	empty := profileWindow{calls: map[string]int64{}, ms: map[string]float64{}}
	ctx := context.Background()
	raw := os.Getenv(storetest.DatabaseURLEnv)
	if raw == "" {
		t.Skip(storetest.DatabaseURLEnv + " must be set to read the server's statements")
	}
	conn, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatalf("connect to the server %s names: %v", storetest.DatabaseURLEnv, err)
	}
	defer conn.Close(ctx)
	var loaded bool
	if err := conn.QueryRow(ctx, "select count(*) > 0 from pg_extension where extname = 'pg_stat_statements'").Scan(&loaded); err != nil {
		t.Fatalf("read the server's extensions: %v", err)
	}
	if !loaded {
		fmt.Printf("\npg_stat_statements is not installed on this server, so %s has no statement table\n", window)
		return empty
	}
	header := []string{"role", "top", "calls", "total_ms", "mean_ms", "statement"}
	profilePrint(t, conn, fmt.Sprintf("the top 20 statements of %s by total execution time, in milliseconds, "+
		"summed over every database of this server. track=all, so a row with top=no is counted again inside "+
		"the top=yes row that called it, and the role is a guess from the statement", window),
		header, profileByTotalTime)
	profilePrint(t, conn, "the top 20 statements of "+window+" by calls, with the same columns", header, profileByCalls)

	rows, err := conn.Query(ctx, profileByRole)
	if err != nil {
		t.Fatalf("read %s by role: %v", window, err)
	}
	defer rows.Close()
	totals := profileWindow{calls: map[string]int64{}, ms: map[string]float64{}}
	out := &profileTable{
		title:  "every top-level statement of " + window + " by role",
		header: []string{"role", "calls", "total_ms"},
	}
	for rows.Next() {
		var role string
		var calls int64
		var ms float64
		if err := rows.Scan(&role, &calls, &ms); err != nil {
			t.Fatalf("read %s by role: %v", window, err)
		}
		totals.calls[role] = calls
		totals.ms[role] = ms
		out.add(role, strconv.FormatInt(calls, 10), profileFloat(ms))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s by role: %v", window, err)
	}
	out.add("all of them", strconv.FormatInt(profileCalls(totals), 10), profileFloat(totals.totalMS()))
	out.print()
	return totals
}

func profileCalls(w profileWindow) int64 {
	var sum int64
	for _, calls := range w.calls {
		sum += calls
	}
	return sum
}

// profilePrint runs one of the statement queries and prints its rows, every cell already text.
func profilePrint(t *testing.T, conn *pgx.Conn, title string, header []string, query string) {
	t.Helper()
	rows, err := conn.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("%s: %v", title, err)
	}
	defer rows.Close()
	out := &profileTable{title: title, header: header}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("%s: %v", title, err)
		}
		cells := make([]string, len(values))
		for i, value := range values {
			cells[i] = fmt.Sprint(value)
		}
		out.add(cells...)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", title, err)
	}
	out.print()
}

// ---------------------------------------------------------------- the measurement

// TestAPIProfileWorkload builds one api test's server and makes one api test's three requests
// over and over, with every part of the test around them timed and the server's own statements
// read beside them. The step that runs it profiles it.
func TestAPIProfileWorkload(t *testing.T) {
	rounds := profileRounds
	if raw := os.Getenv("APIPROFILE_ROUNDS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 2 {
			t.Fatalf("APIPROFILE_ROUNDS=%q: want a count of two or more", raw)
		}
		rounds = parsed
	}
	ctx := context.Background()
	testStart := time.Now()

	// Registered first, so it runs last: everything below has been measured by then, the
	// teardown this test is here to price included.
	var firstOpenWall, laterOpenWall, serverWall, bodyWall, idleNew, idleShutdown time.Duration
	var teardownStart, teardownEnd time.Time
	var window profileWindow
	var round *profileSeries
	t.Cleanup(func() {
		teardown := teardownEnd.Sub(teardownStart)
		parts := &profileTable{
			title:  "(c) what the test around those requests cost, once each",
			header: []string{"part of the test", "ms", "what it is"},
		}
		parts.add("storetest.Open, the first in the process (it also migrates the template)", profileMS(firstOpenWall), "the harness, once a package")
		parts.add("storetest.Open, a later one (the clone and the pool alone)", profileMS(laterOpenWall), "the harness, per test")
		parts.add("newTestServer (docs.New, seedPeople, NewDeps, register)", profileMS(serverWall), "the setup, per test")
		parts.add("the body (the loop)", profileMS(bodyWall), "the requests")
		parts.add("the test's own cleanups (waitBackground, docs.Shutdown)", profileMS(teardown), "the teardown, per test")
		parts.add("docs.New alone, over the same store", profileMS(idleNew), "a part of newTestServer")
		parts.add("docs.Shutdown of a service with no room opened", profileMS(idleShutdown), "a part of the teardown")
		parts.print()

		fmt.Printf("\n(d) the reading\n\n")
		requests := rounds * 4
		body := float64(bodyWall) / float64(time.Millisecond)
		handler := window.ms["handler"] + window.ms["fk check"] + window.ms["seed"]
		fmt.Printf("the loop made %d requests in %s ms: %.1f ms a request.\n", requests, profileFloat(body), body/float64(requests))
		if total := window.totalMS(); total > 0 {
			fmt.Printf("the server executed %s ms of statements over the same window, %s ms of them a handler's "+
				"(the rest is the harness's clone and the session): %.1f ms of server time a request, which is "+
				"%.0f per cent of what a request took.\n",
				profileFloat(total), profileFloat(handler), handler/float64(requests), 100*handler/body)
		}
		if round != nil && len(round.samples) > 1 {
			warm := round.samples[0] - round.steady()
			fmt.Printf("the first of the %d rounds cost %s ms and the rest averaged %s ms, so %s ms of a cold "+
				"test's body is warm-up a run of tests could share rather than work the body needs.\n",
				rounds, profileMS(round.samples[0]), profileMS(round.steady()), profileMS(warm))
		}
		fmt.Printf("the teardown cost %s ms, against %s ms to shut a document service with no room opened down: "+
			"what is left is the work this test's own requests left running.\n", profileMS(teardown), profileMS(idleShutdown))
		fmt.Printf("every test also pays %s ms for its clone and pool, whatever its body does.\n", profileMS(laterOpenWall))

		fmt.Printf("\nAPIPROFILE summary rounds=%d requests=%d first_open_ms=%.1f later_open_ms=%.1f server_build_ms=%.1f "+
			"body_ms=%.1f teardown_ms=%.1f idle_docs_new_ms=%.1f idle_docs_shutdown_ms=%.1f test_ms=%.1f\n",
			rounds, requests,
			float64(firstOpenWall)/float64(time.Millisecond), float64(laterOpenWall)/float64(time.Millisecond),
			float64(serverWall)/float64(time.Millisecond),
			body, float64(teardown)/float64(time.Millisecond),
			float64(idleNew)/float64(time.Millisecond), float64(idleShutdown)/float64(time.Millisecond),
			float64(time.Since(testStart))/float64(time.Millisecond))
	})

	openStart := time.Now()
	database := storetest.Open(t)
	firstOpenWall = time.Since(openStart)

	// Registered before the server is built, so it runs after the server's own cleanups: the end
	// of the teardown every api test pays.
	t.Cleanup(func() { teardownEnd = time.Now() })
	serverStart := time.Now()
	handler, _, _ := newTestServer(t, testServerOptions{store: database})
	serverWall = time.Since(serverStart)
	// Registered after them, so it runs first: the start of that teardown.
	t.Cleanup(func() { teardownStart = time.Now() })

	// From here the server-side window is this loop's. A server with no pg_stat_statements
	// loaded answers an error, which the table below reports as a note of its own.
	if _, err := database.Pool.Exec(ctx, "select pg_stat_statements_reset()"); err != nil {
		fmt.Printf("\npg_stat_statements_reset() answered %v\n", err)
	}

	project := &profileSeries{name: "POST /api/v1/projects"}
	issue := &profileSeries{name: "POST /api/v1/issues (the first in its project)"}
	read := &profileSeries{name: "GET /api/v1/issues/{key}"}
	second := &profileSeries{name: "POST /api/v1/issues (the second in that project)"}
	round = &profileSeries{name: "the three together (one api test's body)"}
	bodyStart := time.Now()
	for i := range rounds {
		key := fmt.Sprintf("PROF%03d", i)
		roundStart := time.Now()
		step := time.Now()
		createReferenceAPIProject(t, handler, key)
		project.add(time.Since(step))
		step = time.Now()
		created := createReferenceAPIIssue(t, handler, key)
		issue.add(time.Since(step))
		step = time.Now()
		if response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+created.Key, nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("read the issue back: status=%d body=%s", response.Code, response.Body.String())
		}
		read.add(time.Since(step))
		// The three above are one api test's body, timed as one. The fourth is outside it: a
		// second issue in a project that already holds one, which pays none of what the first
		// issue in a project pays. Its title shares no term with the first, so the duplicate
		// check answers 201 rather than 409 POSSIBLE_DUPLICATE.
		round.add(time.Since(roundStart))
		step = time.Now()
		if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
			"project": key, "title": "Another matter entirely",
		}, "alice"); response.Code != http.StatusCreated {
			t.Fatalf("create the second issue of %s: status=%d body=%s", key, response.Code, response.Body.String())
		}
		second.add(time.Since(step))
	}
	bodyWall = time.Since(bodyStart)

	// What a later test's storetest.Open costs, measured where it cannot be confused with the
	// template migration the first one in a process pays for. Its own cleanups run before the
	// teardown measured above, which stays the server's alone.
	openStart = time.Now()
	storetest.Open(t)
	laterOpenWall = time.Since(openStart)

	// A document service of its own, built and shut down with no room ever opened, so the
	// teardown reads as a fixed cost or as the work this test's requests gave it.
	idleStart := time.Now()
	idle := docs.New(docs.Deps{Store: database, Events: events.NewBroker(), ServerURL: testServerURL, Settle: profileSettle})
	idleNew = time.Since(idleStart)
	idleStart = time.Now()
	if err := idle.Shutdown(ctx); err != nil {
		t.Fatalf("shut an idle document service down: %v", err)
	}
	idleShutdown = time.Since(idleStart)

	requests := &profileTable{
		title:  fmt.Sprintf("(a) what a request cost, %d rounds of the three an api test's body makes and a fourth beside them (ms)", rounds),
		header: []string{"request", "n", "the first", "mean of the rest", "median", "fastest", "slowest"},
	}
	requests.add(project.row()...)
	requests.add(issue.row()...)
	requests.add(read.row()...)
	requests.add(second.row()...)
	requests.add(round.row()...)
	requests.print()

	fmt.Printf("\n(b) what the server ran for them\n")
	window = profileStatements(t, "the loop")
}

// TestAPIProfileStatements prints the statement tables for the window its caller opened: the
// step resets the counters, runs the tests it wants priced, and runs this.
func TestAPIProfileStatements(t *testing.T) {
	profileStatements(t, "the window since the last reset")
}
