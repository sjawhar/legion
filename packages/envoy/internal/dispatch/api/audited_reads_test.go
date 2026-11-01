package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/explaintest"
)

// auditedRead is one of the three reads the index audit of 2026-10-25 named: the statement, the
// relation whose read was unindexed, the index that serves it after migrations 20261101182426
// to 20261101182440, and the parameter type and literal its prepared form is explained with.
type auditedRead struct {
	name      string
	query     string
	relation  string
	index     string
	parameter string
	literal   string
}

var auditedReads = []auditedRead{
	{
		name:      "an issue's asks, every state",
		query:     listIssueAsksQueryAll,
		relation:  "asks",
		index:     "asks_issue_key",
		parameter: "text",
		literal:   "'TEST-1'",
	},
	{
		name:      "an issue's root messages",
		query:     listIssueRootMessagesQuery,
		relation:  "messages",
		index:     "messages_issue_roots",
		parameter: "text",
		literal:   "'TEST-1'",
	},
	{
		name:      "a document's open asks",
		query:     listArtifactAsksQueryOpen,
		relation:  "asks",
		index:     "asks_artifact_id",
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
	database := newAuditedReadFixture(t)
	ctx := context.Background()
	connection, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire a connection for the plans: %v", err)
	}
	defer connection.Release()
	firstKeys := explaintest.NewIndexKeys(database.Pool)
	for number, read := range auditedReads {
		t.Run(read.name, func(t *testing.T) {
			plan := explaintest.GenericPlan(t, ctx, connection, fmt.Sprintf("audited_read_%d", number),
				read.query, []string{read.parameter}, []string{read.literal},
				"set local enable_seqscan = off")
			if explaintest.SeqScansRelation(t, plan, read.relation) {
				t.Fatalf("%s scans %s sequentially; want %s:\n%s", read.name, read.relation, read.index, plan)
			}
			firstKeys.RequireIndexKeyed(t, plan, read.index, read.name, explaintest.CondRequired)
		})
	}
}

// createAuditedAsk opens one ask on an owner and answers it when answered is set, so each table
// the reads are planned over holds asks of both states.
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

// newAuditedReadFixture is the issue and the document the three reads are read for, in a project
// big enough that each read reaches a few rows of a table holding many: that is the shape
// production has, and the shape that decides a planner's choice between two indexes.
//
// It writes the rows the reads answer through the API, so each carries everything a writer
// fills, and then the bulk the planner needs as SQL, which is the only part of the fixture no
// read returns. It ends with `analyze`, without which the planner costs every index off the
// same default estimate.
func newAuditedReadFixture(t *testing.T) *store.Store {
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
	return database
}
