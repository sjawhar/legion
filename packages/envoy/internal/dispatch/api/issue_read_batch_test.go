package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
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

// sequentialIssueRead is the body getIssue composed before it batched its reads: the sequential
// loaders, in the order it called them, written with WriteJSON's own encoding. It answers the
// artifacts it read as well, which is how many version queries that composition made before
// loadArtifacts read every artifact's versions in one statement.
func sequentialIssueRead(t *testing.T, deps Deps, key string) ([]byte, int) {
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
	openAsks, err := served.loadOpenAsks(ctx, pool, issue.Key)
	if err != nil {
		t.Fatalf("load the open asks: %v", err)
	}
	children, err := served.loadChildren(ctx, pool, issue.Key)
	if err != nil {
		t.Fatalf("load the children: %v", err)
	}
	counts, err := refs.BacklinkCounts(ctx, pool, "issue", []string{issue.Key}, nil)
	if err != nil {
		t.Fatalf("count the issue's backlinks: %v", err)
	}
	recorder := httptest.NewRecorder()
	WriteJSON(recorder, http.StatusOK, struct {
		model.Issue
		model.IssueRouteReach
		Progress          model.IssueProgress `json:"progress"`
		Artifacts         []model.Artifact    `json:"artifacts"`
		OpenAsks          []issueOpenAsk      `json:"open_asks"`
		Children          []model.IssueChild  `json:"children"`
		ReferencedByCount int                 `json:"referenced_by_count"`
	}{
		Issue:             issue,
		Progress:          progress,
		Artifacts:         artifacts,
		OpenAsks:          openAsks,
		Children:          children,
		ReferencedByCount: counts[issue.Key],
	})
	return recorder.Body.Bytes(), len(artifacts)
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
	want, _ := sequentialIssueRead(t, deps, fixture.key)
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

// roundTrips counts what a traced pool sends to Postgres while it is counting: one for each
// statement run on its own and one for each batch, whatever that batch holds.
type roundTrips struct {
	mu         sync.Mutex
	counting   bool
	statements []string
}

func (r *roundTrips) start() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counting = true
	r.statements = nil
}

// stop ends the count and returns the statements it saw, a batch as one "batch" entry.
func (r *roundTrips) stop() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counting = false
	return r.statements
}

func (r *roundTrips) record(statement string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counting {
		r.statements = append(r.statements, statement)
	}
}

func (r *roundTrips) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	r.record(data.SQL)
	return ctx
}

func (r *roundTrips) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (r *roundTrips) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	r.record("batch of " + strconv.Itoa(data.Batch.Len()) + " statements")
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
		// own statements and not the sign-in lookup every request makes.
		identity: headerIdentity(database),
		// A settlement of one of the issue's documents is database work of its own, on the same
		// pool; a delay no test run reaches keeps it out of the counts, and the server's
		// shutdown settles the documents after them.
		settle: time.Hour,
	})
	fixture := buildFullIssueRead(t, handler)

	counter.start()
	_, artifacts := sequentialIssueRead(t, deps, fixture.key)
	sequential := counter.stop()
	t.Logf(
		"the sequential loaders read this issue in %d round trips, and the composition before this change made %d of them (a version query per artifact, %d here, where the loaders now read every artifact's versions in one statement): %v",
		len(sequential), len(sequential)+artifacts-1, artifacts, sequential,
	)

	counter.start()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+fixture.key, nil, "alice")
	batched := counter.stop()
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
	counter.start()
	bareRead := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+bareKey, nil, "alice")
	bareTrips := counter.stop()
	if bareRead.Code != http.StatusOK {
		t.Fatalf("read the bare issue: status=%d body=%s", bareRead.Code, bareRead.Body.String())
	}
	if len(bareTrips) != 1 {
		t.Fatalf("the bare issue's read made %d round trips, want 1: %v", len(bareTrips), bareTrips)
	}
}

// A key no issue carries answers what it always has: the issue row is the batch's first
// statement, so its pgx.ErrNoRows is the error the read returns.
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
