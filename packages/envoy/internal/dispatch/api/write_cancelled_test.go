package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// acquireWatch counts the acquires of a pool that have begun and not yet ended
// (pgxpool.AcquireTracer, which pgxpool reads off the connection tracer), so a test can wait
// until a request is inside the pool's queue rather than sleep for a guess. pgx takes the same
// tracer for statements, where this one counts nothing.
type acquireWatch struct{ started, ended atomic.Int64 }

func (w *acquireWatch) TraceAcquireStart(ctx context.Context, _ *pgxpool.Pool, _ pgxpool.TraceAcquireStartData) context.Context {
	w.started.Add(1)
	return ctx
}

func (w *acquireWatch) TraceAcquireEnd(context.Context, *pgxpool.Pool, pgxpool.TraceAcquireEndData) {
	w.ended.Add(1)
}

func (w *acquireWatch) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (w *acquireWatch) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// queued is how many acquires of the pool have not been answered yet.
func (w *acquireWatch) queued() int64 { return w.started.Load() - w.ended.Load() }

// seedCancelledWriteIssue creates the project and the issue whose spec the writes below edit,
// and answers the issue's key and the id of its spec, with every write those requests left
// running waited out, so a fingerprint taken next reads a database at rest.
func seedCancelledWriteIssue(t *testing.T, handler http.Handler, served *server) (issueKey, artifactID string) {
	t.Helper()
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create project: status=%d body=%s", response.Code, response.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Issue", "spec": "before",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	issue := decodeBody[struct {
		Key               string `json:"key"`
		PrimaryArtifactID string `json:"primary_artifact_id"`
	}](t, created)
	awaitBackgroundWrites(t, served)
	return issue.Key, issue.PrimaryArtifactID
}

// editRequest is the one edit of artifactID these tests send, ready to be served on a context of
// the caller's own: `before` replaced with `after`, as `dispatch doc-edit` sends it.
func editRequest(t *testing.T, artifactID string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}},
	})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	return request
}

// serveWhileCancelled serves request on a context the caller ends: it starts the request, waits
// for waiting to report it held up, ends its context - which is what net/http does to a request
// whose client closed its connection, and the `dispatch` command closes its own at the
// 60-second deadline it gives every call (packages/envoy-client/src/dispatch-http.ts:291-295,
// requestSignal, whose AbortSignal aborts the fetch) - and answers what the server wrote.
func serveWhileCancelled(t *testing.T, handler http.Handler, request *http.Request, waiting func()) *httptest.ResponseRecorder {
	t.Helper()
	ended, endRequest := context.WithCancel(context.Background())
	defer endRequest()
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request.WithContext(ended))
		responses <- recorder
	}()
	waiting()
	endRequest()
	return awaitResponse(t, responses)
}

// requireDocServiceUnavailable fails unless the answer is the busy refusal whose contract is
// that the caller repeats the same call.
func requireDocServiceUnavailable(t *testing.T, what string, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), `"code":"`+codeDocServiceUnavailable+`"`) {
		t.Fatalf("%s answered status=%d body=%s, want %d %s",
			what, response.Code, response.Body.String(), http.StatusServiceUnavailable, codeDocServiceUnavailable)
	}
}

// requireEditedOnce fails unless artifactID's text holds what one application of the edit above
// leaves: the word it wrote and not the word it replaced.
func requireEditedOnce(t *testing.T, handler http.Handler, artifactID string) {
	t.Helper()
	text := dispatchRequest(t, handler, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", nil, "alice")
	if text.Code != http.StatusOK || !strings.Contains(text.Body.String(), "after") || strings.Contains(text.Body.String(), "before") {
		t.Fatalf("the document reads status=%d body=%s, want the edit applied exactly once", text.Code, text.Body.String())
	}
}

// A write whose request ends while it waits for one of the shared pool's connections is answered
// 503 DOC_SERVICE_UNAVAILABLE - the busy refusal whose contract is that the caller repeats the
// same call - and not the 500 that tells a caller the server faulted. The request's own end is
// not a fault of the server's, and the write never ran: it never held a connection, so it never
// opened a transaction, and the database is exactly as it was.
//
// A burst of concurrent API writes to large documents meets this on every wait that is not the
// copy-heap bound's, which the document service already refuses 503 (docs' errWriteBusy): 247 of
// 320 writes of one drive were answered 500, with `begin transaction: context canceled`, bare
// `context canceled` and `begin document load` (INFRA-179, and cmd/dispatch's
// TestABurstOfConcurrentLiveWritesStaysUnderThePodsMemory, whose writes each run under the 60
// seconds the `dispatch` command gives a call).
func TestAWriteWhoseRequestEndsWhileItWaitsForAConnectionIsDocServiceUnavailable(t *testing.T) {
	watch := &acquireWatch{}
	// One connection for the whole server, so a request that needs the database while another
	// transaction holds it waits in the pool's queue, as a write of a burst waits for the shared
	// pool (store.sharedPoolSize).
	database := storetest.Reopened(t, storetest.Open(t), func(config *pgxpool.Config) {
		config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
		config.ConnConfig.Tracer = watch
	})
	// The settlement of the document the seed writes would queue for that one connection under
	// the transaction held below, so it is put beyond the test's end.
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{store: database, settle: time.Hour})
	_, artifactID := seedCancelledWriteIssue(t, handler, served)
	before := databaseFingerprint(t, database)

	held, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("hold the pool's one connection: %v", err)
	}
	defer held.Rollback(context.Background())
	queued := watch.queued()
	cancelledAcquires := database.Pool.Stat().CanceledAcquireCount()

	response := serveWhileCancelled(t, handler, editRequest(t, artifactID), func() {
		for waited := time.Now(); watch.queued() <= queued; time.Sleep(time.Millisecond) {
			if time.Since(waited) > 10*time.Second {
				t.Error("the write did not queue for a connection in 10 s")
				return
			}
		}
	})
	requireDocServiceUnavailable(t, "a write whose request ended while it waited for a connection", response)
	if database.Pool.Stat().CanceledAcquireCount() == cancelledAcquires {
		t.Fatal("no acquire of the pool ended with the request, so the write did not wait for a connection")
	}
	if err := held.Rollback(context.Background()); err != nil {
		t.Fatalf("release the pool's one connection: %v", err)
	}
	assertUnchanged(t, "a write whose request ended while it waited for a connection", before, databaseFingerprint(t, database))

	// The caller repeats the call, as the refusal's contract says it may, and the edit applies.
	if retried := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}},
	}, "alice"); retried.Code != http.StatusOK {
		t.Fatalf("the retry answered status=%d body=%s, want %d", retried.Code, retried.Body.String(), http.StatusOK)
	}
	requireEditedOnce(t, handler, artifactID)
}

// The same refusal for a write whose request ends inside its own transaction, waiting for the
// owner's row: every API write to a document takes that row so the owner cannot close under it
// (requireOpenOwnerStatus, ownerRowLock), and a burst of writes to one owner's documents queues
// on it. The transaction the write had opened rolls back with the request - it commits on that
// request's context (Ledger.Commit, given r.Context() in editArtifact) - so the document holds
// no new version and the stored history no new update, and the caller's retry applies the edit
// exactly once.
func TestAWriteWhoseRequestEndsWhileItWaitsForTheOwnersRowIsDocServiceUnavailable(t *testing.T) {
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{settle: time.Hour})
	issueKey, artifactID := seedCancelledWriteIssue(t, handler, served)
	before := databaseFingerprint(t, database)

	holder, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("open the transaction holding the owner's row: %v", err)
	}
	defer holder.Rollback(context.Background())
	var held string
	if err := holder.QueryRow(context.Background(),
		`select key from issues where key = $1 for update`, issueKey).Scan(&held); err != nil {
		t.Fatalf("hold the owner's row: %v", err)
	}

	response := serveWhileCancelled(t, handler, editRequest(t, artifactID), func() {
		waitForDatabaseLocks(t, holder, 1)
	})
	requireDocServiceUnavailable(t, "a write whose request ended while it waited for the owner's row", response)
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatalf("release the owner's row: %v", err)
	}
	assertUnchanged(t, "a write whose request ended while it waited for the owner's row", before, databaseFingerprint(t, database))

	if retried := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "before", "with": "after"}},
	}, "alice"); retried.Code != http.StatusOK {
		t.Fatalf("the retry answered status=%d body=%s, want %d", retried.Code, retried.Body.String(), http.StatusOK)
	}
	requireEditedOnce(t, handler, artifactID)
}
