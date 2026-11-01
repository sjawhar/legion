package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// auditedRead is one of the three reads the index audit of 2026-10-25 named: the statement, the
// statement main runs, the relation whose read was unindexed, the index that serves it after
// migrations 0159 to 0161, whether its owner is an issue or a document, and the parameter type
// and literal its prepared form is explained with.
type auditedRead struct {
	name      string
	query     string
	oracle    string
	relation  string
	index     string
	owner     string
	parameter string
	literal   string
}

// mainListIssueAsksQueryAll and the two below it are the statements main runs, held here as the
// oracle of the equality test. This change edits no statement text, so each predicate and order
// below is a copy of main's, and the column list and the joins are shared the way main shares
// them. An edit that moved a predicate or an order off its index would move the read away from
// this oracle and fail that test.
var (
	mainListIssueAsksQueryAll = `select ` + askReadColumns + ` ` + askReadFrom + `
		where a.issue_key = $1 order by a.created_at, a.id`
	mainListArtifactAsksQueryOpen = `select ` + askReadColumns + ` ` + askReadFrom + `
		where a.artifact_id = $1 and a.state = 'open' order by a.created_at, a.id`
	mainListIssueRootMessagesQuery = `
		select ` + messageColumns + `
		from messages
		where issue_key = $1 and in_reply_to is null
		order by created_at, id
	`
)

var auditedReads = []auditedRead{
	{
		name:      "an issue's asks, every state",
		query:     listIssueAsksQueryAll,
		oracle:    mainListIssueAsksQueryAll,
		relation:  "asks",
		index:     "asks_issue_key",
		owner:     "issue",
		parameter: "text",
		literal:   "'TEST-1'",
	},
	{
		name:      "an issue's root messages",
		query:     listIssueRootMessagesQuery,
		oracle:    mainListIssueRootMessagesQuery,
		relation:  "messages",
		index:     "messages_issue_roots",
		owner:     "issue",
		parameter: "text",
		literal:   "'TEST-1'",
	},
	{
		name:      "a document's open asks",
		query:     listArtifactAsksQueryOpen,
		oracle:    mainListArtifactAsksQueryOpen,
		relation:  "asks",
		index:     "asks_artifact_id",
		owner:     "artifact",
		parameter: "uuid",
		literal:   "'00000000-0000-4000-8000-00000000000a'",
	},
}

// Each audited read reaches its table through the index the audit's migration gives it, under a
// generic plan, which this test forces: a prepared statement settles on one plan for every
// call, and an index whose eligibility depends on the bound value is lost there
// (TestListIssueAsksOpenQueryUsesAsksOpenIndex says why a fixture this small cannot reach that
// plan on its own). It reads the plans over a seeded table rather than an empty one, because on
// an empty table every plan costs about the same and the planner's choice says nothing: with no
// rows it reads an issue's root messages through messages_session_roots, the partial index on
// (target) whose predicate is this read's own `in_reply_to is null`, and filters the issue out
// of every root in the instance. Naming the index is the assertion, not merely the absence of a
// sequential scan: a scan can carry an index condition on a column its index does not lead with
// and still read the whole index, so the scan's condition is held to the index's own first key
// column too.
func TestTheAuditedReadsSearchTheirIndex(t *testing.T) {
	fixture := newAuditedReadFixture(t)
	database := fixture.database
	ctx := context.Background()
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	firstKeys := explaintest.NewIndexKeys(database.Pool)
	for number, read := range auditedReads {
		t.Run(read.name, func(t *testing.T) {
			plan := auditedReadPlan(t, ctx, connection, fmt.Sprintf("audited_read_%d", number), read)
			if explaintest.SeqScansRelation(t, plan, read.relation) {
				t.Fatalf("%s scans %s sequentially; want %s:\n%s", read.name, read.relation, read.index, plan)
			}
			firstKey := firstKeys.FirstKey(t, read.index)
			for _, scan := range explaintest.IndexScans(t, plan) {
				if scan.Index != read.index {
					continue
				}
				if scan.Cond == "" {
					t.Fatalf("%s reads all of %s, under no index condition:\n%s", read.name, read.index, plan)
				}
				if !explaintest.CondNamesFirstKey(scan.Cond, firstKey) {
					t.Fatalf("%s reads %s on `%s`, which names nothing of its first key column `%s`:\n%s",
						read.name, read.index, scan.Cond, firstKey, plan)
				}
				return
			}
			t.Fatalf("%s searches no %s:\n%s", read.name, read.index, plan)
		})
	}
}

// auditedReadPlan is the plan Postgres builds for a read without knowing the value it will be
// executed with, with sequential scans off. EXECUTE takes no bind parameter, so the owner is
// written into it as a literal; the prepared statement outlives the transaction that explains it
// and is deallocated by name on the one connection that holds it.
func auditedReadPlan(t *testing.T, ctx context.Context, connection *pgxpool.Conn, name string, read auditedRead) json.RawMessage {
	t.Helper()
	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the plan's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range []string{
		"set local enable_seqscan = off",
		"set local plan_cache_mode = force_generic_plan",
	} {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	if _, err := tx.Exec(ctx, "prepare "+name+" ("+read.parameter+") as "+read.query); err != nil {
		t.Fatalf("prepare %s: %v", read.name, err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, "explain (format json) execute "+name+"("+read.literal+")").Scan(&raw); err != nil {
		t.Fatalf("explain %s: %v", read.name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the plan's transaction: %v", err)
	}
	if _, err := connection.Exec(ctx, "deallocate "+name); err != nil {
		t.Fatalf("deallocate %s: %v", read.name, err)
	}
	return explaintest.OnePlan(t, raw)
}

// Migration 0161 drops asks_open_artifact, so no schema this build migrates carries it. The plan
// test above is what says the drop costs nothing: a document's open asks are read through
// asks_artifact_id.
func TestTheUnusedOpenArtifactAskIndexIsDropped(t *testing.T) {
	_, database := newTestHandlerWithStore(t)
	var remaining int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from pg_indexes where schemaname = current_schema() and indexname = 'asks_open_artifact'
	`).Scan(&remaining); err != nil {
		t.Fatalf("read pg_indexes: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("pg_indexes lists asks_open_artifact %d times, want none after migration 0161", remaining)
	}
}

// Every audited read returns what main's own statement returns over the same rows, and returns
// it whichever plan it is read under: the index scan its migration gives it, and the table scan
// it had before, which is the plan main ran. An index changes which rows a read reaches, never
// which rows it answers, and this is what holds the three migrations to that.
func TestTheAuditedReadsReturnTheRowsMainsStatementsDo(t *testing.T) {
	fixture := newAuditedReadFixture(t)
	ctx := context.Background()
	for _, read := range auditedReads {
		t.Run(read.name, func(t *testing.T) {
			owner := fixture.issueKey
			if read.owner == "artifact" {
				owner = fixture.document
			}
			want := auditedReadRows(t, ctx, fixture.database, read.oracle, owner, auditedReadWithoutIndexes)
			got := auditedReadRows(t, ctx, fixture.database, read.query, owner, auditedReadWithoutSeqScans)
			if !slices.Equal(got, want) {
				t.Fatalf("%s read through %s returned\n%v\nwant main's rows\n%v", read.name, read.index, got, want)
			}
			if len(want) < 2 {
				t.Fatalf("%s returned %d rows, too few for the comparison to decide anything", read.name, len(want))
			}
		})
	}
}

// auditedReadWithoutIndexes is the plan main had for each of these reads: no index served any of
// them, so the planner read the table. auditedReadWithoutSeqScans is the plan after the
// migrations, the one the plan test above pins by name.
var (
	auditedReadWithoutIndexes = []string{
		"set local enable_indexscan = off",
		"set local enable_indexonlyscan = off",
		"set local enable_bitmapscan = off",
	}
	auditedReadWithoutSeqScans = []string{"set local enable_seqscan = off"}
)

// auditedReadRows runs statement for owner under settings and returns one string per row, in the
// order the statement returns them, which each of these statements fixes itself.
func auditedReadRows(t *testing.T, ctx context.Context, database *store.Store, statement, owner string, settings []string) []string {
	t.Helper()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the read's transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	for _, setting := range settings {
		if _, err := tx.Exec(ctx, setting); err != nil {
			t.Fatalf("%s: %v", setting, err)
		}
	}
	rows, err := tx.Query(ctx, statement, owner)
	if err != nil {
		t.Fatalf("run the read: %v", err)
	}
	defer rows.Close()
	read := []string{}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			t.Fatalf("read a row: %v", err)
		}
		read = append(read, fmt.Sprint(values...))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate the read: %v", err)
	}
	return read
}

// createAuditedAsk opens one ask on an owner and answers it when answered is set, so each list
// the equality test compares holds asks of both states.
func createAuditedAsk(t *testing.T, handler http.Handler, target, question string, answered bool) {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, target, map[string]any{
		"question": question,
		"actor":    sessionActor(),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create ask %q: status=%d body=%s", question, response.Code, response.Body.String())
	}
	if !answered {
		return
	}
	ask := decodeBody[model.Ask](t, response)
	if answer := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+ask.ID+"/answer", map[string]any{
		"text": "Yes.",
	}, "alice"); answer.Code != http.StatusOK {
		t.Fatalf("answer ask %q: status=%d body=%s", question, answer.Code, answer.Body.String())
	}
}

// auditedReadFixture is the issue and the document the three reads are read for, in a project
// big enough that each read reaches a few rows of a table holding many: that is the shape
// production has, and the shape that decides a planner's choice between two indexes.
type auditedReadFixture struct {
	database *store.Store
	issueKey string
	document string
}

// newAuditedReadFixture writes the rows the reads answer through the API, so each carries
// everything a writer fills, and then the bulk the planner needs as SQL, which is the only part
// of the fixture no read returns. It ends with `analyze`, without which the planner costs every
// index off the same default estimate.
func newAuditedReadFixture(t *testing.T) auditedReadFixture {
	t.Helper()
	handler, database := newTestHandlerWithStore(t)
	issue := createInteractionIssue(t, handler, "TEST", "Audited reads", "A spec")
	document := createProjectDocument(t, handler, "TEST", "Runbook", "# Runbook\n")
	createAuditedAsk(t, handler, "/api/v1/issues/"+issue.Key+"/asks", "Open issue question", false)
	createAuditedAsk(t, handler, "/api/v1/issues/"+issue.Key+"/asks", "Answered issue question", true)
	createAuditedAsk(t, handler, "/api/v1/artifacts/"+document.ID+"/asks", "Open document question", false)
	createAuditedAsk(t, handler, "/api/v1/artifacts/"+document.ID+"/asks", "Second open document question", false)
	createAuditedAsk(t, handler, "/api/v1/artifacts/"+document.ID+"/asks", "Answered document question", true)
	root := createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Ship the build tonight"}, "alice")
	createIssueMessage(t, handler, issue.Key, map[string]any{"body": "Sounds good", "in_reply_to": root.ID}, "bob")
	createIssueMessage(t, handler, issue.Key, map[string]any{"body": "A second thread"}, "alice")

	ctx := context.Background()
	for _, bulk := range []struct {
		what      string
		statement string
	}{
		{"the other issues", `
			insert into issues (key, project_key, number, title, created_by, rank)
			select 'TEST-' || n, 'TEST', n, 'Issue ' || n, '{"kind":"user","id":"alice"}', 'U' || n
			from generate_series(2, 40) n
			where $1 <> ''
		`},
		{"their root messages", `
			insert into messages (issue_key, author, body)
			select 'TEST-' || (n % 39 + 2), '{"kind":"user","id":"alice"}', 'Root message ' || n
			from generate_series(1, 400) n
			where $1 <> ''
		`},
		{"their replies", `
			insert into messages (issue_key, author, body, in_reply_to)
			select m.issue_key, '{"kind":"user","id":"bob"}', 'Reply to ' || m.body, m.id
			from messages m where m.in_reply_to is null and m.issue_key <> $1
		`},
		{"their asks", `
			insert into asks (issue_key, author, question)
			select 'TEST-' || (n % 39 + 2), '{"kind":"user","id":"alice"}', 'Question ' || n
			from generate_series(1, 400) n
			where $1 <> ''
		`},
	} {
		if _, err := database.Pool.Exec(ctx, bulk.statement, issue.Key); err != nil {
			t.Fatalf("seed %s: %v", bulk.what, err)
		}
	}
	if _, err := database.Pool.Exec(ctx, "analyze"); err != nil {
		t.Fatalf("analyze the seeded fixture: %v", err)
	}
	return auditedReadFixture{database: database, issueKey: issue.Key, document: document.ID}
}
