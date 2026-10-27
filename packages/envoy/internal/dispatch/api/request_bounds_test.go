package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// assertUnavailable fails the test unless response is a 503 with code, Retry-After: 1 and
// Cache-Control: no-store, in Dispatch's {error, code} body.
func assertUnavailable(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	var body struct{ Error, Code string }
	if response.Code != http.StatusServiceUnavailable || json.Unmarshal(response.Body.Bytes(), &body) != nil ||
		body.Code != code || body.Error == "" {
		t.Fatalf("status=%d body=%s, want 503 %s with a message", response.Code, response.Body.String(), code)
	}
	if got := response.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
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

// A write that finds its class's one unit held waits its second in the queue and is refused
// 503 OVERLOADED, which tells its client to retry: it never runs, so it never queues behind the
// held write for as long as that write is held.
func TestARequestPastTheAdmissionLimitIsRefusedOverloaded(t *testing.T) {
	limiter := admission.New(admission.Limits{Read: 1, Write: 1})
	handler, database, _ := newTestServer(t, testServerOptions{admission: limiter})
	issue := createInteractionIssue(t, handler, "TEST", "Held write", "A spec")

	release := holdIssueRow(t, database, issue.Key)
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		held <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"title": "Renamed"}, "alice")
	}()
	deadline := time.Now().Add(5 * time.Second)
	for limiter.Stats()[admission.Write].InFlight != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the held write was never admitted")
		}
		time.Sleep(time.Millisecond)
	}

	started := time.Now()
	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTHER", "name": "Other"}, "alice")
	if waited := time.Since(started); waited >= 1200*time.Millisecond {
		t.Fatalf("the refusal took %v, want under 1.2s", waited)
	}
	assertUnavailable(t, refused, admission.CodeOverloaded)
	if stats := limiter.Stats()[admission.Write]; stats.WaitExceeded != 1 {
		t.Fatalf("write stats %+v, want one wait_exceeded refusal", stats)
	}

	release()
	if response := awaitResponse(t, held); response.Code != http.StatusOK {
		t.Fatalf("the held write: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
}

// A request whose work runs past its deadline is stopped there and answered 503 REQUEST_TIMEOUT,
// not left holding its connection for as long as the lock it waits on is held.
func TestARequestHeldPastItsDeadlineAnswersRequestTimeout(t *testing.T) {
	const deadline = time.Second
	handler, database, _ := newTestServer(t, testServerOptions{requestDeadline: deadline})
	issue := createInteractionIssue(t, handler, "TEST", "Held past its deadline", "A spec")

	holdIssueRow(t, database, issue.Key)
	started := time.Now()
	response := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"title": "Renamed"}, "alice")
	answered := time.Since(started)
	assertUnavailable(t, response, admission.CodeRequestTimeout)
	if answered < deadline || answered > deadline+200*time.Millisecond {
		t.Fatalf("answered after %v, want within %v of the deadline %v", answered, 200*time.Millisecond, deadline)
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
	assertUnavailable(t, response, admission.CodeDatabaseBusy)
}

// An event stream is neither admitted nor bounded by the request deadline: it stays open, and
// keeps writing its heartbeat, long after an ordinary request's deadline would have ended it.
func TestAnEventStreamOutlivesTheRequestDeadline(t *testing.T) {
	const deadline = 100 * time.Millisecond
	handler, _, _ := newTestServer(t, testServerOptions{requestDeadline: deadline, streamHeartbeat: 20 * time.Millisecond})
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	opened := time.Now()
	scanner := openSSEStream(t, httpServer.URL+"/api/v1/events")
	for scanner.Scan() {
		if scanner.Text() == ": heartbeat" && time.Since(opened) > 3*deadline {
			return
		}
	}
	t.Fatalf("the event stream ended after %v (%v), within three request deadlines", time.Since(opened), scanner.Err())
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

// A write whose deadline passes while its commit is in flight still commits: the commit runs
// apart from the request's deadline, so its outcome is known, and the room it wrote stays
// resident - its browser still connected - holding the text the store holds. A commit the
// deadline cut short would leave its outcome unknown and fail the room, closing the browser's
// connection and reloading the document.
func TestAWriteWhoseDeadlinePassesMidCommitKeepsItsRoom(t *testing.T) {
	const deadline = time.Second
	handler, database, deps := newTestServer(t, testServerOptions{requestDeadline: deadline, settle: time.Hour, browserPeers: true})
	service := deps.Docs.(*docs.Service)
	issue := createInteractionIssue(t, handler, "TEST", "Commit past the deadline", "Before.")
	peer := connectBrowserPeer(t, service, issue.PrimaryArtifactID)
	t.Cleanup(peer.close)
	release := holdCommits(t, database)

	const written = "Committed past the deadline."
	started := time.Now()
	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		responses <- dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
			"ops": []map[string]string{{"op": "insert", "markdown": written, "after": "end"}},
		}, "alice")
	}()
	waitForHeldCommit(t, database)
	if reached := time.Since(started); reached >= deadline {
		t.Fatalf("the write reached its commit after %v, past its deadline %v: the deadline never passed mid-commit", reached, deadline)
	}
	time.Sleep(time.Until(started.Add(deadline + 100*time.Millisecond)))
	select {
	case response := <-responses:
		t.Fatalf("the write answered while its commit was held: status=%d body=%s", response.Code, response.Body.String())
	default:
	}
	release()

	response := awaitResponse(t, responses)
	if response.Code != http.StatusOK && !strings.Contains(response.Body.String(), `"code":"`+admission.CodeRequestTimeout+`"`) {
		t.Fatalf("the write: status=%d body=%s, want 200 or 503 %s", response.Code, response.Body.String(), admission.CodeRequestTimeout)
	}
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
	locker, err := pgx.ConnectConfig(ctx, database.Pool.Config().ConnConfig.Copy())
	if err != nil {
		t.Fatalf("connect the table locker: %v", err)
	}
	t.Cleanup(func() { _ = locker.Close(context.Background()) })
	tables, err := locker.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the table lock: %v", err)
	}
	t.Cleanup(func() { _ = tables.Rollback(context.Background()) })
	if _, err := tables.Exec(ctx, `lock table delivery_pull_requests, delivery_runs in access exclusive mode`); err != nil {
		t.Fatalf("lock the timeline's tables: %v", err)
	}

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
	if err := tables.Rollback(ctx); err != nil {
		t.Fatalf("release the timeline's tables: %v", err)
	}
	if response := awaitResponse(t, responses); response.Code != http.StatusOK {
		t.Fatalf("timeline: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
	if highWater != 2 {
		t.Fatalf("the timeline held %d request connections at once, want 2", highWater)
	}
}
