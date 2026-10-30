package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// fullIssueRead is an issue holding one of everything GET /api/v1/issues/{key} answers, so a
// test of the read exercises every statement its batches queue.
type fullIssueRead struct {
	key     string
	openAsk string
}

// buildFullIssueRead writes, through the API alone, an issue with two documents of several
// versions each - one approved at its latest version, one with an open approval request - a
// file, an open ask with a reply and one without, an answered ask, a child with a grandchild and
// a link of its own, an external link and a blocker, and another issue's mention of both the
// issue and one of its asks, so its backlink counts are not zero.
func buildFullIssueRead(t *testing.T, handler http.Handler) fullIssueRead {
	t.Helper()
	createReferenceAPIProject(t, handler, "BATCH")
	createReferenceAPIProject(t, handler, "CITE")

	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "BATCH", "title": "Batched issue read", "spec": "# Spec\n\nThe issue's own document.\n",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create the read's issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[model.Issue](t, created)

	blocker := createReferenceAPIIssue(t, handler, "BATCH")
	if patched := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]any{
		"blocked_by":     []string{blocker.Key},
		"external_links": []map[string]string{{"url": "https://example.com/batched-issue-read"}},
	}, "alice"); patched.Code != http.StatusOK {
		t.Fatalf("block and link the issue: status=%d body=%s", patched.Code, patched.Body.String())
	}

	approved := uploadIssueDocument(t, handler, issue.Key, "Design notes.md", "# Design\n\nFirst.\n")
	uploadIssueDocument(t, handler, issue.Key, "Design notes.md", "# Design\n\nSecond.\n")
	awaiting := uploadIssueDocument(t, handler, issue.Key, "Rollout plan.md", "# Rollout\n\nFirst.\n")
	uploadIssueDocument(t, handler, issue.Key, "Rollout plan.md", "# Rollout\n\nSecond.\n")
	if response := multipartRequest(t, handler, "/api/v1/issues/"+issue.Key+"/artifacts",
		map[string]string{"name": "notes.txt"}, "notes.txt", "text/plain", []byte("a file, not a document\n"), "alice",
	); response.Code != http.StatusCreated {
		t.Fatalf("upload the issue's file: status=%d body=%s", response.Code, response.Body.String())
	}
	if review := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+approved.ID+"/reviews", map[string]any{
		"state": "approved",
	}, "alice"); review.Code != http.StatusCreated && review.Code != http.StatusOK {
		t.Fatalf("approve the design notes: status=%d body=%s", review.Code, review.Body.String())
	}
	if requested := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+awaiting.ID+"/approval-requests", map[string]any{
		"actor": sessionActor(), "summary": "The rollout plan is ready to read.",
	}); requested.Code != http.StatusCreated && requested.Code != http.StatusOK {
		t.Fatalf("request approval of the rollout plan: status=%d body=%s", requested.Code, requested.Body.String())
	}

	replied := createIssueAsk(t, handler, issue.Key, "Which release should ship first?")
	if reply := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "The one with the migration.", "ask_id": replied,
	}, "alice"); reply.Code != http.StatusCreated {
		t.Fatalf("reply to the open ask: status=%d body=%s", reply.Code, reply.Body.String())
	}
	createIssueAsk(t, handler, issue.Key, "Who owns the rollout?")
	answered := createIssueAsk(t, handler, issue.Key, "Is the migration reversible?")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/asks/"+answered+"/answer", map[string]any{
		"selected": []string{"Yes"},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("answer the answered ask: status=%d body=%s", response.Code, response.Body.String())
	}

	child := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "BATCH", "title": "Child of the read", "parent": issue.Key,
	}, "alice")
	if child.Code != http.StatusCreated {
		t.Fatalf("create the child: status=%d body=%s", child.Code, child.Body.String())
	}
	childKey := decodeBody[model.Issue](t, child).Key
	if linked := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+childKey, map[string]any{
		"external_links": []map[string]string{{"url": "https://example.com/child"}},
	}, "alice"); linked.Code != http.StatusOK {
		t.Fatalf("link the child: status=%d body=%s", linked.Code, linked.Body.String())
	}
	if grandchild := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "BATCH", "title": "Grandchild of the read", "parent": childKey,
	}, "alice"); grandchild.Code != http.StatusCreated {
		t.Fatalf("create the grandchild: status=%d body=%s", grandchild.Code, grandchild.Body.String())
	}

	citer := createReferenceAPIIssue(t, handler, "CITE")
	if message := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+citer.Key+"/messages", map[string]any{
		"actor": sessionActor(),
		"body":  "Blocked on dispatch://" + issue.Key + " and dispatch://" + issue.Key + "/ask/" + replied + ".",
	}); message.Code != http.StatusCreated {
		t.Fatalf("cite the issue and its ask: status=%d body=%s", message.Code, message.Body.String())
	}
	return fullIssueRead{key: issue.Key, openAsk: replied}
}

// createIssueAsk opens one question on the issue and returns its id.
func createIssueAsk(t *testing.T, handler http.Handler, key, question string) string {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/asks", map[string]any{
		"question": question,
		"options":  []map[string]string{{"label": "Yes"}, {"label": "No"}},
		"actor":    sessionActor(),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create issue ask %q: status=%d body=%s", question, response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response).ID
}

// sequentialIssueRead is the body getIssue composed before it batched its reads: the statements
// and the scans the loaders every other route calls still run, in the order it called them,
// written with WriteJSON's own encoding into the one type the read answers (issueDetail).
func sequentialIssueRead(t *testing.T, deps Deps, key string) []byte {
	t.Helper()
	served := directServer(deps)
	ctx := context.Background()
	pool := deps.Store.Pool
	issue, progress, err := served.loadIssueWithProgress(ctx, pool, key)
	if err != nil {
		t.Fatalf("load the issue: %v", err)
	}
	artifacts, err := served.loadArtifacts(ctx, pool, issue.Key)
	if err != nil {
		t.Fatalf("load the artifacts: %v", err)
	}
	asks, replies, err := served.queryOwnerAsks(ctx, pool, issueOwner(issue.Key), "open")
	if err != nil {
		t.Fatalf("load the open asks: %v", err)
	}
	rows, err := pool.Query(ctx, issueChildrenStatement, issue.Key)
	if err != nil {
		t.Fatalf("query the children: %v", err)
	}
	children, err := scanIssueChildren(rows)
	rows.Close()
	if err != nil {
		t.Fatalf("load the children: %v", err)
	}
	counts, err := refs.BacklinkCounts(ctx, pool, "issue", []string{issue.Key}, nil)
	if err != nil {
		t.Fatalf("count the issue's backlinks: %v", err)
	}
	recorder := httptest.NewRecorder()
	WriteJSON(recorder, http.StatusOK, issueDetail{
		Issue:             issue,
		Progress:          progress,
		Artifacts:         artifacts,
		OpenAsks:          issueOpenAsksOf(asks, replies),
		Children:          children,
		ReferencedByCount: counts[issue.Key],
	})
	return recorder.Body.Bytes()
}

// The batched read answers exactly what the sequential loaders answered, byte for byte: the two
// paths run the same statements and the same scans, so an issue holding one of everything reads
// the same either way.
func TestBatchedIssueReadAnswersTheSequentialLoadersBytes(t *testing.T) {
	handler, _, deps := newTestServer(t, testServerOptions{})
	fixture := buildFullIssueRead(t, handler)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+fixture.key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read the issue: status=%d body=%s", response.Code, response.Body.String())
	}
	want := sequentialIssueRead(t, deps, fixture.key)
	if got := response.Body.String(); got != string(want) {
		t.Fatalf("the batched read answered\n%s\nthe sequential loaders answered\n%s", got, want)
	}
	for _, part := range []string{
		`"external_links":[{"url":"https://example.com/batched-issue-read","kind":"url"}]`,
		`"state":"approved"`, `"state":"awaiting"`, `"kind":"file"`,
		`"last_reply"`, `"open_asks":[`, `"children":[`,
	} {
		if !strings.Contains(response.Body.String(), part) {
			t.Fatalf("the read answered no %s: %s", part, response.Body.String())
		}
	}
	read := decodeBody[struct {
		Artifacts []struct {
			Kind     string `json:"kind"`
			Versions []struct {
				Number int `json:"number"`
			} `json:"versions"`
		} `json:"artifacts"`
		OpenAsks []struct {
			ID                string `json:"id"`
			OpenedEventID     *int64 `json:"opened_event_id"`
			ReferencedByCount *int   `json:"referenced_by_count"`
		} `json:"open_asks"`
		Children          []struct{} `json:"children"`
		ReferencedByCount int        `json:"referenced_by_count"`
	}](t, response)
	if read.ReferencedByCount == 0 {
		t.Fatal("the issue's referenced_by_count is zero though another issue cites it")
	}
	if len(read.Children) != 1 {
		t.Fatalf("children = %d, want the one child", len(read.Children))
	}
	if len(read.Artifacts) != 4 {
		t.Fatalf("artifacts = %d, want the spec, two documents and the file", len(read.Artifacts))
	}
	for _, artifact := range read.Artifacts {
		if artifact.Kind == "doc" && len(artifact.Versions) == 0 {
			t.Fatal("a document carries no version")
		}
	}
	cited := false
	for _, ask := range read.OpenAsks {
		if ask.OpenedEventID == nil || ask.ReferencedByCount == nil {
			t.Fatalf("open ask %q carries no opened_event_id or referenced_by_count", ask.ID)
		}
		if ask.ID == fixture.openAsk && *ask.ReferencedByCount > 0 {
			cited = true
		}
	}
	if !cited {
		t.Fatal("the cited ask's referenced_by_count is zero though another issue's message cites it")
	}
	if len(read.OpenAsks) != 3 {
		t.Fatalf("open asks = %d, want the two questions and the approval request, never the answered ask", len(read.OpenAsks))
	}
}

// countedRead marks the context of the one request a roundTrips counts. Every statement the
// handler runs is run on a context derived from the request's, which pgx hands the tracer
// (TraceQueryStart, TraceBatchStart), so the count is that request's own statements and not a
// settlement's or another request's on the same traced pool.
type countedRead struct{}

// countedIssueRead reads the issue through the handler on a request a roundTrips counts.
func countedIssueRead(t *testing.T, handler http.Handler, key string) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.WithValue(context.Background(), countedRead{}, true)
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/issues/"+key, nil)
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// roundTrips counts what a traced pool sends to Postgres for a counted request: one for each
// statement run on its own and one for each batch, whatever that batch holds.
type roundTrips struct {
	mu         sync.Mutex
	statements []string
}

// take returns the statements counted since the last take, a batch as one "batch" entry.
func (r *roundTrips) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	taken := r.statements
	r.statements = nil
	return taken
}

func (r *roundTrips) record(ctx context.Context, statement string) {
	if ctx.Value(countedRead{}) == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.statements = append(r.statements, statement)
}

func (r *roundTrips) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.record(ctx, data.SQL)
	return ctx
}

func (r *roundTrips) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *roundTrips) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	r.record(ctx, "batch of "+strconv.Itoa(data.Batch.Len())+" statements")
	return ctx
}

func (r *roundTrips) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (r *roundTrips) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// The issue read costs two round trips, and one for an issue with nothing the second batch is
// for, where the sequential loaders spent a statement on each part of the answer.
func TestIssueReadCostsTwoRoundTrips(t *testing.T) {
	database := storetest.Open(t)
	counter := &roundTrips{}
	traced := storetest.Traced(t, database, counter)
	handler, _, deps := newTestServer(t, testServerOptions{
		store: traced,
		// The caller's identity is resolved on the untraced pool, so the count is the read's
		// own statements and not the sign-in lookup every request makes, which runs on the
		// request's own context and would otherwise be one of the statements it marks.
		identity: headerIdentity(database),
		// A settlement of one of the issue's documents is database work of its own, on the same
		// pool; a delay no test run reaches keeps it off the issue's rows, and the server's
		// shutdown settles the documents after the counts.
		settle: time.Hour,
	})
	fixture := buildFullIssueRead(t, handler)

	response := countedIssueRead(t, handler, fixture.key)
	batched := counter.take()
	if response.Code != http.StatusOK {
		t.Fatalf("read the issue: status=%d body=%s", response.Code, response.Body.String())
	}
	if len(batched) != 2 {
		t.Fatalf("the read made %d round trips, want 2: %v", len(batched), batched)
	}

	bare := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]any{
		"project": "BATCH", "title": "Nothing to read",
	}, "alice")
	if bare.Code != http.StatusCreated {
		t.Fatalf("create the bare issue: status=%d body=%s", bare.Code, bare.Body.String())
	}
	bareKey := decodeBody[model.Issue](t, bare).Key
	// Every issue is created with its empty spec, and the route that removes a document is not
	// one the API serves, so the issue whose read needs no second batch is made by taking its
	// artifacts out from under it - once the document service has closed its rooms, so nothing
	// settles what is no longer there. The delete runs on the untraced pool, outside the count.
	if err := deps.Docs.Quiesce(context.Background()); err != nil {
		t.Fatalf("quiesce the documents: %v", err)
	}
	for _, statement := range []string{
		`delete from artifact_versions where artifact_id in (select id from artifacts where issue_key = $1)`,
		`delete from doc_updates where artifact_id in (select id from artifacts where issue_key = $1)`,
		`delete from artifacts where issue_key = $1`,
	} {
		if _, err := database.Pool.Exec(context.Background(), statement, bareKey); err != nil {
			t.Fatalf("take the bare issue's artifacts out (%s): %v", statement, err)
		}
	}
	bareRead := countedIssueRead(t, handler, bareKey)
	bareTrips := counter.take()
	if bareRead.Code != http.StatusOK {
		t.Fatalf("read the bare issue: status=%d body=%s", bareRead.Code, bareRead.Body.String())
	}
	if len(bareTrips) != 1 {
		t.Fatalf("the bare issue's read made %d round trips, want 1: %v", len(bareTrips), bareTrips)
	}
}

// A key no issue carries answers what it always has: the issue row's callback records the missing
// row, and the read answers that row's pgx.ErrNoRows once its batch has closed.
func TestBatchedIssueReadStillAnswersAMissingKey(t *testing.T) {
	handler := newTestHandler(t)
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/NOPE-1", nil, "alice")
	if response.Code != http.StatusNotFound {
		t.Fatalf("read a missing issue: status=%d body=%s", response.Code, response.Body.String())
	}
	if body := response.Body.String(); body != `{"code":"NOT_FOUND","error":"not found"}`+"\n" {
		t.Fatalf("read a missing issue: body=%s", body)
	}
}

// A missing key costs the connection nothing. pgx invalidates every statement of a batch whose
// results Close answers an error, in that connection's statement and description caches
// (invalidateCachesOnBatchResultsError, pgx's batch.go), and the next read on it deallocates those
// statements and prepares them again; the issue read closes a missing key's batch without an error,
// so the statements the read prepared are still prepared after it.
//
// The read runs on a pool of one connection, read by this test alone, so the read after the
// missing key is on the connection that read it and pg_prepared_statements - a session's own
// prepared statements - is that connection's. Every read on it runs through the same statement
// cache a request's does: the pool is opened from the server's own configuration.
func TestMissingIssueReadKeepsTheConnectionsPreparedStatements(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	fixture := buildFullIssueRead(t, handler)
	deps.Store = singleConnectionStore(t, database)
	served := directServer(deps)
	ctx := context.Background()

	if _, err := served.readIssue(ctx, fixture.key); err != nil {
		t.Fatalf("read the issue: %v", err)
	}
	prepared := preparedStatementNames(t, deps.Store)
	if len(prepared) == 0 {
		t.Fatal("the read prepared no statement, so a deallocation would not show here")
	}

	if _, err := served.readIssue(ctx, "NOPE-1"); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("read a missing issue: %v, want pgx.ErrNoRows", err)
	}
	after := preparedStatementNames(t, deps.Store)
	for _, name := range prepared {
		if !slices.Contains(after, name) {
			t.Fatalf("the missing key's read left %s deallocated: %v are prepared, %v were", name, after, prepared)
		}
	}
	if _, err := served.readIssue(ctx, fixture.key); err != nil {
		t.Fatalf("read the issue after the missing key: %v", err)
	}
	if again := preparedStatementNames(t, deps.Store); len(again) != len(after) {
		t.Fatalf("the read after the missing key prepared statements again: %v, where %v were prepared", again, after)
	}
}

// singleConnectionStore opens a pool of one connection on the database database is already on,
// from its own configuration, closed after t ends.
func singleConnectionStore(t *testing.T, database *store.Store) *store.Store {
	t.Helper()
	config := database.Pool.Config()
	config.MaxConns = 1
	config.MinConns = 0
	config.MinIdleConns = 0
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a pool of one connection on the same database: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}
}

// preparedStatementNames is what the pool's connection has prepared, which a statement pgx
// invalidated is deallocated from before this very read runs
// (Conn.deallocateInvalidatedCachedStatements).
func preparedStatementNames(t *testing.T, database *store.Store) []string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select name from pg_prepared_statements order by name`)
	if err != nil {
		t.Fatalf("read the connection's prepared statements: %v", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("scan the connection's prepared statements: %v", err)
	}
	return names
}
