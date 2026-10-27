package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/reearth/ygo/crdt"

	"github.com/sjawhar/envoy/internal/dispatch/admission"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// assertRetryable fails the test unless response has status and code, Retry-After: 1 and
// Cache-Control: no-store, in Dispatch's {error, code} body.
func assertRetryable(t *testing.T, response *http.Response, status int, code string) {
	t.Helper()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	var body struct{ Error, Code string }
	if response.StatusCode != status || json.Unmarshal(data, &body) != nil || body.Code != code || body.Error == "" {
		t.Fatalf("status=%d body=%s, want %d %s with a message", response.StatusCode, data, status, code)
	}
	if got := response.Header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

// holdIssueRow locks key's issue row in a transaction of the test's own, so every write to the
// issue waits on it, until the returned release runs; the test's end releases it too.
func holdIssueRow(t *testing.T, database *store.Store, key string) func() {
	t.Helper()
	holder, err := database.Pool.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin the issue row holder: %v", err)
	}
	if _, err := holder.Exec(context.Background(), `select 1 from issues where key = $1 for update`, key); err != nil {
		_ = holder.Rollback(context.Background())
		t.Fatalf("lock issue %s: %v", key, err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_ = holder.Rollback(context.Background())
		}
	}
	t.Cleanup(release)
	return release
}

// A write that finds its class's one unit held waits its 75 ms in the queue and is refused 429
// OVERLOADED, which tells its client to retry, within the 100 ms a refusal is answered in: it never
// runs, so it never queues behind the held write for as long as that write is held.
func TestARequestPastTheAdmissionLimitIsRefusedOverloaded(t *testing.T) {
	limiter := admission.New(admission.Limits{Read: 1, Write: 1})
	handler, database, _ := newTestServer(t, testServerOptions{admission: limiter})
	issue := createInteractionIssue(t, handler, "TEST", "Held write", "A spec")

	release := holdIssueRow(t, database, issue.Key)
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		held <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"title": "Renamed"}, "alice")
	}()
	waitForWriteInFlight(t, limiter)

	started := time.Now()
	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTHER", "name": "Other"}, "alice")
	if waited := time.Since(started); waited >= 100*time.Millisecond {
		t.Fatalf("the refusal took %v, want under 100ms", waited)
	}
	assertRetryable(t, refused.Result(), http.StatusTooManyRequests, admission.CodeOverloaded)
	if stats := limiter.Stats()[admission.Write]; stats.WaitExceeded != 1 {
		t.Fatalf("write stats %+v, want one wait_exceeded refusal", stats)
	}

	release()
	if response := awaitResponse(t, held); response.Code != http.StatusOK {
		t.Fatalf("the held write: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
}

// waitForWriteInFlight returns once limiter has admitted one write.
func waitForWriteInFlight(t *testing.T, limiter *admission.Limiter) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for limiter.Stats()[admission.Write].InFlight != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the held write was never admitted")
		}
		time.Sleep(time.Millisecond)
	}
}

// countingReader counts the bytes read from it.
type countingReader struct {
	io.Reader
	read atomic.Int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.read.Add(int64(n))
	return n, err
}

// A refused upload is answered only once the server has read its whole body, so its client reads
// the 429 rather than a connection reset under the bytes it is still sending: the body is far
// past what the sockets buffer between the two, so a server that answered and closed first would
// leave most of it unread.
func TestARefusedPostIsAnsweredAfterItsBodyIsDrained(t *testing.T) {
	limiter := admission.New(admission.Limits{Read: 1, Write: 1})
	handler, _, _ := newTestServer(t, testServerOptions{admission: limiter})
	issue := createInteractionIssue(t, handler, "TEST", "Refused upload", "A spec")
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	held, err := limiter.Admit(context.Background(), admission.Write, 1)
	if err != nil {
		t.Fatalf("hold the write class's one unit: %v", err)
	}
	defer held.Release()

	const size = 8 << 20 // within the upload route's limit (maxUploadRequestBytes)
	body := &countingReader{Reader: bytes.NewReader(make([]byte, size))}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, httpServer.URL+"/api/v1/issues/"+issue.Key+"/artifacts", body)
	if err != nil {
		t.Fatalf("build the upload: %v", err)
	}
	request.ContentLength = size
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Dispatch-User", "alice")
	response, err := httpServer.Client().Do(request)
	if err != nil {
		t.Fatalf("the refused upload: %v, want its 429 read", err)
	}
	defer response.Body.Close()
	assertRetryable(t, response, http.StatusTooManyRequests, admission.CodeOverloaded)
	if sent := body.read.Load(); sent != size {
		t.Fatalf("the client sent %d of the body's %d bytes before the answer, want all of them", sent, size)
	}
}

// A read whose work runs past its deadline is stopped there and answered 503 REQUEST_TIMEOUT, not
// left holding its connection for as long as the lock it waits on is held.
func TestAReadHeldPastItsDeadlineAnswersRequestTimeout(t *testing.T) {
	const deadline = time.Second
	handler, database, _ := newTestServer(t, testServerOptions{readDeadline: deadline})
	issue := createInteractionIssue(t, handler, "TEST", "Read past its deadline", "A spec")
	lockTables(t, database, "issues")

	started := time.Now()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice")
	answered := time.Since(started)
	assertRetryable(t, response.Result(), http.StatusServiceUnavailable, admission.CodeRequestTimeout)
	if answered < deadline || answered > deadline+200*time.Millisecond {
		t.Fatalf("answered after %v, want within %v of the deadline %v", answered, 200*time.Millisecond, deadline)
	}
}

// lockTables takes an access exclusive lock on tables in a transaction of the test's own, so
// every read of them waits until the returned release runs; the test's end releases it too.
func lockTables(t *testing.T, database *store.Store, tables string) func() {
	t.Helper()
	ctx := context.Background()
	locker, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect the table locker: %v", err)
	}
	t.Cleanup(func() { _ = locker.Close(context.Background()) })
	locked, err := locker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the table lock: %v", err)
	}
	release := func() { _ = locked.Rollback(context.Background()) }
	t.Cleanup(release)
	if _, err := locked.Exec(ctx, `lock table `+tables+` in access exclusive mode`); err != nil {
		t.Fatalf("lock %s: %v", tables, err)
	}
	return release
}

// A write has no deadline: one held past the deadline a read would get still runs to its end and
// commits once what held it lets go.
func TestAWriteHeldPastTheReadDeadlineCompletesAndCommits(t *testing.T) {
	const deadline = time.Second
	handler, database, _ := newTestServer(t, testServerOptions{readDeadline: deadline})
	issue := createInteractionIssue(t, handler, "TEST", "Write past the deadline", "A spec")

	release := holdIssueRow(t, database, issue.Key)
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"title": "Renamed"}, "alice")
	}()
	time.Sleep(deadline + 300*time.Millisecond)
	select {
	case response := <-responses:
		t.Fatalf("the write answered while the issue row was held: status=%d body=%s", response.Code, response.Body.String())
	default:
	}
	release()
	if response := awaitResponse(t, responses); response.Code != http.StatusOK {
		t.Fatalf("the held write: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	stored := decodeBody[struct {
		Title string `json:"title"`
	}](t, dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key, nil, "alice"))
	if stored.Title != "Renamed" {
		t.Fatalf("issue title %q after the write, want Renamed", stored.Title)
	}
}

// A request that cannot get a database connection within the request pool's acquire bound is
// answered 503 DATABASE_BUSY, which its client may retry, rather than 500.
func TestARequestOnAFullPoolAnswersDatabaseBusy(t *testing.T) {
	const bound = 300 * time.Millisecond
	handler, database, _ := newTestServer(t, testServerOptions{requestPoolSize: 1, acquireBound: bound})
	held, err := database.Pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("hold the request pool's one connection: %v", err)
	}
	defer held.Release()

	started := time.Now()
	response := sessionRequest(t, handler, http.MethodGet, "/api/v1/projects", nil)
	if answered := time.Since(started); answered > bound+200*time.Millisecond {
		t.Fatalf("answered after %v, want within the acquire bound %v + 200ms", answered, bound)
	}
	assertRetryable(t, response.Result(), http.StatusServiceUnavailable, admission.CodeDatabaseBusy)
}

// An event stream is neither admitted nor bounded by the read deadline: it stays open, and keeps
// writing its heartbeat, long after an ordinary read's deadline would have ended it.
func TestAnEventStreamOutlivesTheReadDeadline(t *testing.T) {
	const deadline = 100 * time.Millisecond
	handler, _, _ := newTestServer(t, testServerOptions{readDeadline: deadline, streamHeartbeat: 20 * time.Millisecond})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	opened := time.Now()
	scanner := openSSEStream(t, httpServer.URL+"/api/v1/events")
	for scanner.Scan() {
		if scanner.Text() == ": heartbeat" && time.Since(opened) > 3*deadline {
			return
		}
	}
	t.Fatalf("the event stream ended after %v (%v), within three read deadlines", time.Since(opened), scanner.Err())
}

// holdCommitsLockKey is the advisory lock the commit-holding trigger waits on.
const holdCommitsLockKey = 592001

// holdCommits makes every commit that wrote a document version wait at its commit for an advisory
// lock the test holds, until the returned release runs: a deferred constraint trigger runs when
// its transaction commits, so the commit itself is what waits. A browser's own update appends no
// version, so the room's persistence of it never waits. The test's end releases the lock too.
func holdCommits(t *testing.T, database *store.Store) func() {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `
		create function hold_commit_for_test() returns trigger language plpgsql as $$
		begin
			perform pg_advisory_xact_lock_shared(592001);
			return null;
		end $$;
		create constraint trigger hold_commit_for_test after insert on artifact_versions
			deferrable initially deferred for each row execute function hold_commit_for_test();
	`); err != nil {
		t.Fatalf("create the commit-holding trigger: %v", err)
	}
	holder, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect the commit holder: %v", err)
	}
	t.Cleanup(func() { _ = holder.Close(context.Background()) })
	if _, err := holder.Exec(ctx, `select pg_advisory_lock($1)`, holdCommitsLockKey); err != nil {
		t.Fatalf("take the commit-holding lock: %v", err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			_, _ = holder.Exec(context.Background(), `select pg_advisory_unlock($1)`, holdCommitsLockKey)
		}
	}
	t.Cleanup(release)
	return release
}

// waitForHeldCommit returns once a commit waits on holdCommits' lock.
func waitForHeldCommit(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and wait_event = 'advisory'
				and lower(query) = 'commit'
		`).Scan(&waiting); err != nil {
			t.Fatalf("look for the held commit: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no commit ever waited on the held lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// storedText renders the document the store holds, update by update, as a room loading it would.
func storedText(t *testing.T, database *store.Store, artifactID string) string {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `select update from doc_updates where artifact_id = $1 order by version`, artifactID)
	if err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	defer rows.Close()
	document := crdt.New()
	for rows.Next() {
		var update []byte
		if err := rows.Scan(&update); err != nil {
			t.Fatalf("scan a stored document update: %v", err)
		}
		if err := crdt.ApplyUpdateV1(document, update, nil); err != nil {
			t.Fatalf("apply a stored document update: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the stored document: %v", err)
	}
	return renderDocument(t, document)
}

// A write whose client goes away while its commit is in flight still commits: the commit runs
// apart from the request's cancellation, so its outcome is known, and the room it wrote stays
// resident - its browser still connected - holding the text the store holds. A commit the client's
// disconnect cut short would leave its outcome unknown and fail the room, closing the browser's
// connection and reloading the document.
func TestAWriteWhoseClientDisconnectsMidCommitKeepsItsRoom(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{settle: time.Hour, browserPeers: true})
	service := deps.Docs.(*docs.Service)
	issue := createInteractionIssue(t, handler, "TEST", "Commit after the client left", "Before.")
	peer := connectBrowserPeer(t, service, issue.PrimaryArtifactID)
	t.Cleanup(peer.close)
	release := holdCommits(t, database)

	const written = "Committed after the client left."
	data, err := json.Marshal(map[string]any{"ops": []map[string]string{{"op": "insert", "markdown": written, "after": "end"}}})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	ctx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", bytes.NewReader(data)).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	served := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		served <- response
	}()
	waitForHeldCommit(t, database)
	disconnect()
	time.Sleep(200 * time.Millisecond) // long enough for the disconnect to reach anything it could
	select {
	case response := <-served:
		t.Fatalf("the write ended while its commit was held: status=%d body=%s", response.Code, response.Body.String())
	default:
	}
	release()

	awaitResponse(t, served)
	peer.barrier(t) // fails when the room was failed and its browser's connection closed
	stored := storedText(t, database, issue.PrimaryArtifactID)
	if !strings.Contains(stored, written) {
		t.Fatalf("stored document %q lacks the committed write %q", stored, written)
	}
	live, err := service.Text(context.Background(), issue.PrimaryArtifactID)
	if err != nil {
		t.Fatalf("read the room: %v", err)
	}
	if live != stored {
		t.Fatalf("room holds %q, the store %q", live, stored)
	}
}

// The delivery timeline runs its reads side by side, at most two at once, so one timeline holds
// at most two of the request pool's connections, the weight admission counts it at. Every read is
// held on a table lock the test takes, so each read in flight holds its connection until the
// test lets go.
func TestTheDeliveryTimelineHoldsAtMostTwoConnections(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{})
	ctx := context.Background()
	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"widgets-release / widgets-release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("seed delivery settings: %v", err)
	}
	release := lockTables(t, database, "delivery_pull_requests, delivery_runs")

	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- sessionRequest(t, handler, http.MethodGet, "/api/v1/delivery/timeline", nil)
	}()
	waitForAcquiredConns(t, database, 2)
	highWater := database.Pool.Stat().AcquiredConns()
	for range 50 {
		time.Sleep(5 * time.Millisecond)
		highWater = max(highWater, database.Pool.Stat().AcquiredConns())
	}
	release()
	if response := awaitResponse(t, responses); response.Code != http.StatusOK {
		t.Fatalf("timeline: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if highWater != 2 {
		t.Fatalf("the timeline held %d request connections at once, want 2", highWater)
	}
}
