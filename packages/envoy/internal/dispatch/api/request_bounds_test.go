package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

// A write that finds its class's one unit held waits its 1 s in the queue and is refused 429
// OVERLOADED, which tells its client to retry: it never runs, so it never queues behind the held
// write for as long as that write is held. The admission's own share of the refusal - its wait in
// the queue, which the limiter records - is its wait and within 250 ms past it. The round trip,
// which a loaded machine stretches with scheduling it does not control, is held only to arriving
// while the write is still held and inside the next bound a request meets, the request pool's 3 s
// acquire bound, with a second to spare.
func TestARequestPastTheAdmissionLimitIsRefusedOverloaded(t *testing.T) {
	limiter := admission.New(admission.Limits{Read: 1, Write: 1})
	handler, database, _ := newTestServer(t, testServerOptions{admission: limiter})
	issue := createInteractionIssue(t, handler, "TEST", "Held write", "A spec")

	release := holdIssueRow(t, database, issue.Key)
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		held <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"title": "Renamed"}, "alice")
	}()
	waitForWritesInFlight(t, limiter, 1)

	started := time.Now()
	refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{"key": "OTHER", "name": "Other"}, "alice")
	if waited := time.Since(started); waited >= admission.DefaultWait+time.Second {
		t.Fatalf("the refusal took %v, want it a second before the request pool's 3s acquire bound", waited)
	}
	select {
	case response := <-held:
		t.Fatalf("the held write answered %d before the refusal was read, want it still held", response.Code)
	default:
	}
	assertRetryable(t, refused.Result(), http.StatusTooManyRequests, admission.CodeOverloaded)
	stats := limiter.Stats()[admission.Write]
	if stats.WaitExceeded != 1 {
		t.Fatalf("write stats %+v, want one wait_exceeded refusal", stats)
	}
	if stats.LongestRefusedWait < admission.DefaultWait || stats.LongestRefusedWait >= admission.DefaultWait+250*time.Millisecond {
		t.Fatalf("the refused write waited %v in the queue, want its %v wait and within 250ms past it", stats.LongestRefusedWait, admission.DefaultWait)
	}

	release()
	if response := awaitResponse(t, held); response.Code != http.StatusOK {
		t.Fatalf("the held write: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
}

// waitForWritesInFlight returns once limiter has admitted writes of them.
func waitForWritesInFlight(t *testing.T, limiter *admission.Limiter, writes int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for limiter.Stats()[admission.Write].InFlight != writes {
		if time.Now().After(deadline) {
			t.Fatalf("%d writes were admitted, want %d", limiter.Stats()[admission.Write].InFlight, writes)
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

// The live socket is mounted outside the route table, so it is neither admitted nor bounded by the
// read deadline: it stays open, and keeps sending its heartbeat, long after an ordinary read's
// deadline would have ended it.
//
// The heartbeat is the socket's own liveness clock too: the page must answer each ping within two
// heartbeats or the socket closes, with no close frame (live_socket.go, pongWait). So the
// heartbeat here is long beside the read deadline, and the socket is held open across many
// deadlines: measured on a 3-CPU pod, a 20 ms heartbeat (40 ms to answer) dropped the socket in
// 6 runs of 400, and a 100 ms one in 1 of 600.
func TestTheLiveSocketOutlivesTheReadDeadline(t *testing.T) {
	const deadline, heartbeat = 20 * time.Millisecond, 250 * time.Millisecond
	rig := newLiveSocketRig(t, testServerOptions{
		readDeadline: deadline, streamHeartbeat: heartbeat,
	}, nil)

	opened := time.Now()
	client := rig.dial(t, asUser("alice"))
	for time.Since(opened) <= 3*heartbeat {
		if frame := decodeSocketFrame(t, client.take(t, time.Second, "a heartbeat")); frame == nil {
			t.Fatalf("the live socket sent a frame that is not one of its own after %v", time.Since(opened))
		}
	}
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

// A write whose commit the database does not answer within the commit's bound is answered 500
// COMMIT_OUTCOME_UNKNOWN with an ERROR line naming the unknown outcome, and never 503
// DATABASE_BUSY at DEBUG: the transaction may have committed, where a busy pool means nothing
// ran and the client may send the write again. The commit waits on the test's lock, and the
// bound is the test's (store.WithCommitBoundForTest) rather than production's ten seconds.
func TestAWriteWhoseCommitRunsOutItsBoundIsNotAnsweredDatabaseBusy(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "Commit past its bound", "Before.")
	release := holdCommits(t, database)
	defer release()
	logs := captureLogs(t)

	data, err := json.Marshal(map[string]any{"ops": []map[string]string{
		{"op": "insert", "markdown": "Written while the commit is held.", "after": "end"},
	}})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", bytes.NewReader(data)).
		WithContext(store.WithCommitBoundForTest(context.Background(), 300*time.Millisecond))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusInternalServerError || !strings.Contains(response.Body.String(), `"code":"COMMIT_OUTCOME_UNKNOWN"`) {
		t.Fatalf("the write whose commit ran out its bound: status=%d body=%s, want 500 COMMIT_OUTCOME_UNKNOWN",
			response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), admission.CodeDatabaseBusy) || response.Header().Get("Retry-After") != "" {
		t.Fatalf("the write was answered as a busy pool: status=%d headers=%v body=%s",
			response.Code, response.Header(), response.Body.String())
	}
	logged := logs.String()
	if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, "its outcome is unknown") {
		t.Fatalf("the commit that ran out its bound logged:\n%s\nwant an ERROR line naming the unknown outcome", logged)
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

// A client that sends its headers and then trickles its body holds no write unit while that
// body arrives: the reads of it park the ticket the request was admitted with and take it back
// only once the body has ended (drainedBody.park), so a write beside two such clients is
// admitted at once rather than waiting for them, and each trickler is cut within the idle bound
// its body arrives under. Twelve of them once held every write unit of a process for as long as
// they cared to, and every other write on it was refused 429 OVERLOADED.
func TestATricklingBodyDoesNotHoldItsWriteUnitWithoutEnd(t *testing.T) {
	const idle, queueWait = 2 * time.Second, 3 * time.Second
	limiter := admission.New(admission.Limits{Read: 4, Write: 2, Wait: queueWait})
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{admission: limiter})
	served.transfer = transferBounds{idle: idle}
	issue := createInteractionIssue(t, handler, "TEST", "Trickled writes", "A spec")
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("X-Dispatch-User", "alice")
	// net/http tells a client that asked to continue to do so at the first read of its body, so
	// the 100 Continue each trickler reads says its request is in that read with its ticket
	// parked, with no byte of its body sent.
	header.Set("Expect", "100-continue")
	tricklers := make([]*heldBackBody, 0, 2)
	for range cap(tricklers) {
		tricklers = append(tricklers, sendPartOfBody(t, httpServer.Listener.Addr().String(),
			"/api/v1/issues/"+issue.Key+"/comments", header, int(maxJSONRequestBytes), nil))
	}
	for _, trickler := range tricklers {
		if response, answer := trickler.answer(); response.StatusCode != http.StatusContinue {
			t.Fatalf("a trickler was answered %d %s before it sent a byte, want the 100 Continue of a body being read",
				response.StatusCode, answer)
		}
	}
	reading := time.Now()
	if held := limiter.Stats()[admission.Write].InFlight; held != 0 {
		t.Fatalf("the tricklers hold %d write units while their bodies arrive, want none", held)
	}

	started := time.Now()
	admitted := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"title": "Renamed"}, "alice")
	waited := time.Since(started)
	if admitted.Code != http.StatusOK {
		t.Fatalf("the write beside the tricklers: status=%d body=%s after %v, want 200 on the units they park",
			admitted.Code, admitted.Body.String(), waited)
	}
	if waited >= idle {
		t.Fatalf("the write beside the tricklers waited %v, want it admitted at once rather than once their %v idle bound cut them",
			waited, idle)
	}
	for _, trickler := range tricklers {
		response, answer := trickler.answer()
		if response.StatusCode != http.StatusBadRequest || refusalCode(answer) != "REQUEST_BODY_UNREADABLE" {
			t.Fatalf("a trickled write was answered %d %s, want 400 REQUEST_BODY_UNREADABLE once the idle bound cut its body",
				response.StatusCode, answer)
		}
	}
	if cut := time.Since(reading); cut < idle-100*time.Millisecond {
		t.Fatalf("the tricklers were cut %v after their first read, want them to have their %v idle bound", cut, idle)
	}
}

// A body that keeps arriving is taken however long it takes: the bound on it is on the silence
// between its bytes and on the rate they keep, not on the transfer's length, so an honest upload
// over a slow uplink - a 25 MiB file at 1 Mbit/s takes about three and a half minutes - is
// written rather than refused. Here the body is sent steadily for three times the grace its rate
// is judged after, which the total bound this replaced would have cut.
func TestABodyThatKeepsArrivingIsTakenHoweverLongItTakes(t *testing.T) {
	const grace, sending = time.Second, 3 * time.Second
	handler, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	served.transfer = transferBounds{idle: time.Second, grace: grace, floor: 1 << 10}
	issue := createInteractionIssue(t, handler, "TEST", "Slow uploads", "A spec")
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)

	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("X-Dispatch-User", "alice")
	body := []byte(`{"body":"` + strings.Repeat("a", 30_000) + `"}`)
	sent := sendPartOfBody(t, httpServer.Listener.Addr().String(),
		"/api/v1/issues/"+issue.Key+"/comments", header, len(body), body[:10])
	started := time.Now()
	response, answer := sent.sendSteadily(body[10:], 12, sending/12)
	elapsed := time.Since(started)

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		t.Fatalf("the steadily sent body was answered %d %s after %v, want it taken", response.StatusCode, answer, elapsed)
	}
	if elapsed < grace {
		t.Fatalf("the body arrived in %v, want it sent for longer than the %v its rate is judged after", elapsed, grace)
	}
}

// A client that goes away while its request waits for room is recorded as the 499 it is, not as
// the 200 of a handler that wrote nothing: under load a client that gives up while queued is the
// most frequent way a request ends with nothing written.
func TestARequestWhoseClientLeavesWhileItIsQueuedIsLoggedClientGone(t *testing.T) {
	limiter := admission.New(admission.Limits{Read: 4, Write: 1, Wait: 10 * time.Second})
	handler, database, _ := newTestServer(t, testServerOptions{admission: limiter})
	issue := createInteractionIssue(t, handler, "TEST", "Queued writes", "A spec")
	release := holdIssueRow(t, database, issue.Key)
	defer release()
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		held <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"title": "Renamed"}, "alice")
	}()
	waitForWritesInFlight(t, limiter, 1)
	logged := captureLog(t)

	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments",
			strings.NewReader(`{"body":"queued"}`)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		queued <- response
	}()
	waitForQueuedWrites(t, limiter, 1)
	cancel()

	if response := awaitResponse(t, queued); response.Body.Len() != 0 {
		t.Fatalf("the request whose client left while it was queued was answered %d %s, want nothing written",
			response.Code, response.Body.String())
	}
	line := accessLineOfRoute(t, logged, "/api/v1/issues/{key}/comments")
	if !strings.Contains(line, "status=499") {
		t.Fatalf("the request whose client left while it was queued logged:\n%s\nwant status=499", line)
	}
	release()
	if response := awaitResponse(t, held); response.Code != http.StatusOK {
		t.Fatalf("the held write: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
}

// waitForQueuedWrites returns once limiter holds writes of them in its queue.
func waitForQueuedWrites(t *testing.T, limiter *admission.Limiter, writes int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for limiter.Stats()[admission.Write].Queued != writes {
		if time.Now().After(deadline) {
			t.Fatalf("%d writes were queued, want %d", limiter.Stats()[admission.Write].Queued, writes)
		}
		time.Sleep(time.Millisecond)
	}
}

// accessLineOfRoute is the one access-log line logged holds for route.
func accessLineOfRoute(t *testing.T, logged *logBuffer, route string) string {
	t.Helper()
	var found []string
	for _, line := range accessLines(t, logged) {
		if strings.Contains(line, " route="+route+" ") {
			found = append(found, line)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d access-log lines name route %s:\n%s", len(found), route, strings.Join(found, "\n"))
	}
	return found[0]
}

// A commit the database does not answer within its bound is reported whether or not the client
// is still there to be told: the outcome is unknown, which is a fault an operator must see, so
// the ERROR line names it either way. Only a client still there is written the 500
// COMMIT_OUTCOME_UNKNOWN; one that went away is answered nothing and logged 499, as every gone
// client is. The commit waits on the test's lock and its bound is the test's
// (store.WithCommitBoundForTest), as TestAWriteWhoseCommitRunsOutItsBoundIsNotAnsweredDatabaseBusy
// holds the answer a client still there reads.
func TestACommitPastItsBoundIsLoggedWhenItsClientHasGone(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{settle: time.Hour})
	issue := createInteractionIssue(t, handler, "TEST", "Commit past its bound", "Before.")
	release := holdCommits(t, database)
	defer release()
	logged := captureLog(t)

	data, err := json.Marshal(map[string]any{"ops": []map[string]string{
		{"op": "insert", "markdown": "Written while the commit is held.", "after": "end"},
	}})
	if err != nil {
		t.Fatalf("encode the edit: %v", err)
	}
	ctx, cancel := context.WithCancel(store.WithCommitBoundForTest(context.Background(), 300*time.Millisecond))
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", bytes.NewReader(data)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", "alice")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		answered <- response
	}()
	waitForHeldCommit(t, database)
	cancel()

	if response := awaitResponse(t, answered); response.Body.Len() != 0 {
		t.Fatalf("the write whose client left mid-commit was answered %d %s, want nothing written",
			response.Code, response.Body.String())
	}
	if !strings.Contains(logged.String(), "level=ERROR") || !strings.Contains(logged.String(), "its outcome is unknown") {
		t.Fatalf("the commit that ran out its bound under a client that had gone logged:\n%s\nwant an ERROR line naming the unknown outcome",
			logged.String())
	}
	if line := accessLineOfRoute(t, logged, "/api/v1/artifacts/{id}/edits"); !strings.Contains(line, "status=499") {
		t.Fatalf("the write whose client left mid-commit logged:\n%s\nwant status=499", line)
	}
}

// awaitRequestPoolIdle returns how long it took the request pool to hold no connection, and fails
// the test when it still holds one after wait.
func awaitRequestPoolIdle(t *testing.T, pool *store.Pool, wait time.Duration) time.Duration {
	t.Helper()
	started := time.Now()
	for held := pool.Stat().AcquiredConns(); held != 0; held = pool.Stat().AcquiredConns() {
		if time.Since(started) > wait {
			t.Fatalf("%d request-pool connections were still held %v after the client stopped reading", held, wait)
		}
		time.Sleep(time.Millisecond)
	}
	return time.Since(started)
}

// A pinned read whose client stops reading its answer gives its request-pool connection back
// within the idle bound that answer is written under: the answer is larger than the sockets
// between the two buffer, so the handler blocks in Write, and the bound (boundedAnswer) is what
// ends that wait. The read's own deadline is far past it here, so only the idle bound can.
// Without it the read held its pinned connection, and the admission unit around it, for as long
// as the client cared to leave the answer unread.
func TestAPinnedReadGivesItsConnectionBackWhenItsClientStopsReading(t *testing.T) {
	const idle, deadline = 500 * time.Millisecond, 30 * time.Second
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	served.transfer = transferBounds{idle: idle}
	const pattern = "/api/v1/stalled-reader-test"
	pinned := make(chan struct{})
	route := apiRoute{routeRow: routeRow{Method: http.MethodGet, Pattern: pattern, Auth: authAny, Handler: func(w http.ResponseWriter, r *http.Request) {
		var one int
		if err := database.Pool.QueryRow(r.Context(), "select 1").Scan(&one); err != nil {
			t.Errorf("take the read's pinned connection: %v", err)
			return
		}
		close(pinned)
		// Far past what the sockets hold between a handler and a client that reads none of it.
		chunk := make([]byte, 64<<10)
		for written := 0; written < 64<<20; written += len(chunk) {
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}}}
	if !route.pinsConnection() {
		t.Fatal("the test's route is not pinned, so it does not hold a request-pool connection to give back")
	}
	mux := http.NewServeMux()
	mux.HandleFunc(route.Method+" "+pattern, served.serveRoute(route, admission.New(admission.DefaultLimits()), deadline))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)
	awaitQuietRequestPool(t, database.Pool)

	connection, err := net.Dial("tcp", httpServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the server: %v", err)
	}
	t.Cleanup(func() { connection.Close() })
	if _, err := fmt.Fprintf(connection, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", pattern, httpServer.Listener.Addr()); err != nil {
		t.Fatalf("send the read: %v", err)
	}
	<-pinned
	// The client reads nothing of the answer; only its idle bound can end the handler's write.
	if held := awaitRequestPoolIdle(t, database.Pool, 10*time.Second); held < idle {
		t.Fatalf("the pinned connection went back after %v, want it held until the answer's %v idle bound", held, idle)
	}
}

// A client that reads a large answer slowly but steadily is answered all of it: the bound on an
// answer is on the silence of each write, not on the answer's length, so a page or a download on
// a slow downlink is served however long it takes, while one that stops reading is cut within the
// idle bound (TestAPinnedReadGivesItsConnectionBackWhenItsClientStopsReading). Here the client
// takes several times that bound to read the answer, which the total bound this replaced cut off
// at it. It reads far faster than the bound asks, because what one write of the handler's waits
// for is not one read of the client's: the kernel takes the answer into its send buffer and wakes
// the blocked write only once a good part of that buffer has drained.
func TestASlowButSteadyReaderOfALargeAnswerIsAnsweredInFull(t *testing.T) {
	const idle, deadline, answer, chunk = time.Second, 30 * time.Second, 32 << 20, 256 << 10
	_, _, _, served := newRegisteredTestServer(t, testServerOptions{})
	served.transfer = transferBounds{idle: idle}
	const pattern = "/api/v1/steady-reader-test"
	route := apiRoute{routeRow: routeRow{Method: http.MethodGet, Pattern: pattern, Auth: authAny, Handler: func(w http.ResponseWriter, r *http.Request) {
		block := make([]byte, chunk)
		began := time.Now()
		for written := 0; written < answer; written += len(block) {
			wrote := time.Now()
			if _, err := w.Write(block); err != nil {
				t.Errorf("write the answer at %d bytes, %v into the answer and %v into this write: %v",
					written, time.Since(began), time.Since(wrote), err)
				return
			}
		}
	}}}
	mux := http.NewServeMux()
	mux.HandleFunc(route.Method+" "+pattern, served.serveRoute(route, admission.New(admission.DefaultLimits()), deadline))
	httpServer := httptest.NewServer(mux)
	t.Cleanup(httpServer.Close)

	connection, err := net.Dial("tcp", httpServer.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial the server: %v", err)
	}
	t.Cleanup(func() { connection.Close() })
	if _, err := fmt.Fprintf(connection, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", pattern, httpServer.Listener.Addr()); err != nil {
		t.Fatalf("send the read: %v", err)
	}
	if err := connection.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatalf("read the answer's head: %v", err)
	}
	defer response.Body.Close()

	started := time.Now()
	block := make([]byte, chunk)
	for read := 0; read < answer; {
		// A chunk every 20 ms: the whole answer takes about two and a half seconds, several
		// times the idle bound, and every write of the handler's wakes well inside it.
		time.Sleep(20 * time.Millisecond)
		n, err := io.ReadFull(response.Body, block)
		read += n
		if err != nil {
			t.Fatalf("the answer stopped %v in, after %d of %d bytes: %v", time.Since(started), read, answer, err)
		}
	}
	if elapsed := time.Since(started); elapsed < idle {
		t.Fatalf("the answer was read in %v, want a reader slower than its %v idle bound", elapsed, idle)
	}
}

// A write whose client hangs up while it waits in admission's queue is logged 499, as a read's is,
// over a real connection and not only a cancelled context: net/http notices a closed connection
// only once the request's body has been read, and a queued write's body has not been. On the real
// binary at 1176657a, a comment write closed 0.4 s into its queue wait was logged
// `status=429 elapsed_ms=1000 bytes=135` - its slot held for the whole wait and a refusal written to
// a client that had gone - while a GET closed the same way was logged 499.
func TestAQueuedWriteWhoseClientHangsUpIsLoggedClientGone(t *testing.T) {
	limiter := admission.New(admission.Limits{Read: 4, Write: 1, Wait: 10 * time.Second})
	handler, database, _ := newTestServer(t, testServerOptions{admission: limiter})
	server := httptest.NewServer(handler)
	defer server.Close()
	issue := createInteractionIssue(t, handler, "TEST", "Queued write hangs up", "A spec")
	release := holdIssueRow(t, database, issue.Key)
	defer release()
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		held <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"title": "Renamed"}, "alice")
	}()
	waitForWritesInFlight(t, limiter, 1)
	logged := captureLog(t)

	connection, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatalf("dial the server: %v", err)
	}
	body := `{"body":"queued, then gone"}`
	if _, err := fmt.Fprintf(connection, "POST /api/v1/issues/%s/comments HTTP/1.1\r\nHost: dispatch\r\n"+
		"X-Dispatch-User: alice\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		issue.Key, len(body), body); err != nil {
		t.Fatalf("send the queued write: %v", err)
	}
	waitForQueuedWrites(t, limiter, 1)
	_ = connection.Close()
	hungUp := time.Now()

	deadline := time.Now().Add(15 * time.Second)
	for limiter.Stats()[admission.Write].Queued != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the write whose client hung up was still queued 15 s later")
		}
		time.Sleep(10 * time.Millisecond)
	}
	left := time.Since(hungUp)
	time.Sleep(200 * time.Millisecond)
	line := accessLineOfRoute(t, logged, "/api/v1/issues/{key}/comments")
	if !strings.Contains(line, "status=499") {
		t.Fatalf("the queued write whose client hung up left the queue %v later and logged:\n%s\nwant status=499", left, line)
	}
	if left > 2*time.Second {
		t.Fatalf("the queued write whose client hung up held its queue slot %v, want it given up once its client had gone, not at the end of its 10 s wait", left)
	}
	release()
	if response := awaitResponse(t, held); response.Code != http.StatusOK {
		t.Fatalf("the held write: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
}

// A write that finds no room reads its whole body off the connection before it waits for that
// room (drainedBody.readWhole), so the time its body takes is none of its wait: a body sent for
// three times the whole admission wait is still admitted once the room comes free, and the
// write is never refused after it was admitted. It holds no unit and no queue slot while its
// body arrives, and the writes that arrive while it does are ahead of it in neither.
//
// A write admitted before its body, which parked its units at the body's first read and queued
// for them again once the body ended, waited for room twice: the second wait began behind
// everything that had arrived meanwhile, and refused writes the server had already admitted.
func TestAWriteWhoseBodyOutlastsItsWaitIsAdmittedAfterIt(t *testing.T) {
	const wait, sending = 500 * time.Millisecond, 1500 * time.Millisecond
	limiter := admission.New(admission.Limits{Read: 4, Write: 1, Wait: wait})
	handler, database, _, served := newRegisteredTestServer(t, testServerOptions{admission: limiter})
	served.transfer = transferBounds{idle: 5 * time.Second}
	issue := createInteractionIssue(t, handler, "TEST", "Slow write bodies", "A spec")
	httpServer := httptest.NewServer(handler)
	t.Cleanup(httpServer.Close)
	release := holdIssueRow(t, database, issue.Key)
	defer release()
	held := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		held <- dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"title": "Renamed"}, "alice")
	}()
	waitForWritesInFlight(t, limiter, 1)

	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("X-Dispatch-User", "alice")
	body := []byte(`{"body":"sent while the server has no room for it"}`)
	slow := sendPartOfBody(t, httpServer.Listener.Addr().String(),
		"/api/v1/issues/"+issue.Key+"/comments", header, len(body), body[:10])
	type answered struct {
		response *http.Response
		body     []byte
	}
	answers := make(chan answered, 1)
	started := time.Now()
	go func() {
		response, read := slow.sendSteadily(body[10:], 6, sending/6)
		answers <- answered{response, read}
	}()

	time.Sleep(2 * wait) // past its whole wait, with its body still arriving
	if stats := limiter.Stats()[admission.Write]; stats.Queued != 0 || stats.InFlight != 1 {
		t.Fatalf("while the slow body arrived the writes were %+v, want the held write alone in flight and nothing queued", stats)
	}
	waitForQueuedWrites(t, limiter, 1) // it asks for room once its body is off the connection
	release()

	select {
	case taken := <-answers:
		if taken.response.StatusCode != http.StatusCreated {
			t.Fatalf("the write whose body outlasted its wait: status=%d body=%s, want 201",
				taken.response.StatusCode, taken.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the write whose body outlasted its wait was never answered")
	}
	if elapsed := time.Since(started); elapsed < sending {
		t.Fatalf("the body was sent in %v, want it sent for longer than the %v wait of the queue it then joined", elapsed, wait)
	}
	if stats := limiter.Stats()[admission.Write]; stats.WaitExceeded != 0 || stats.QueueFull != 0 {
		t.Fatalf("the writes were %+v, want none refused: the slow body's own write waited for room once, after its body", stats)
	}
	if response := awaitResponse(t, held); response.Code != http.StatusOK {
		t.Fatalf("the held write: status=%d body=%s, want 200", response.Code, response.Body.String())
	}
}
