package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
)

// keyedPost posts body to the rig's route on server as alice under key, giving up after timeout as
// a client does. It answers the status and body, or the client's own failure.
func keyedPost(server *httptest.Server, path, body, key string, timeout time.Duration) (int, string, http.Header, error) {
	request, err := http.NewRequest(http.MethodPost, server.URL+path, strings.NewReader(body))
	if err != nil {
		return 0, "", nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(contracts.IdempotencyKeyHeader, key)
	response, err := (&http.Client{Timeout: timeout}).Do(request)
	if err != nil {
		return 0, "", nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	return response.StatusCode, string(answer), response.Header, err
}

// INFRA-258. A write the server takes longer to finish than its client waits still lands, once:
// the handler runs on past the client's giving up, and the client's repeat under its key is told
// the first run is still going (409 KEY_IN_FLIGHT), then answered the first run's own stored
// answer. A handler bound to its client's cancellation rolled back at the client's deadline, and
// every repeat started again from nothing, so such a write could never land.
func TestAKeyedWriteSlowerThanItsClientFinishesAndIsReplayed(t *testing.T) {
	rig := newWritingRig(t, func(run int32, w http.ResponseWriter, r *http.Request, write rigWrite) {
		select {
		case <-time.After(20 * time.Second):
		case <-r.Context().Done():
			writeError(w, "CUT_OFF", http.StatusServiceUnavailable, "the run was cut off with its client")
			return
		}
		if err := write(r.Context(), nil); err != nil {
			t.Errorf("run %d's write: %v", run, err)
		}
		answerRun(run, w, r)
	})
	server := httptest.NewServer(rig.handler)
	t.Cleanup(server.Close)

	started := time.Now()
	if status, body, _, err := keyedPost(server, "/test/a/writes", `{"n":1}`, "slow-press", 15*time.Second); err == nil {
		t.Fatalf("the first attempt answered %d %s; want the client to give up at 15 s", status, body)
	}
	status, body, header, err := keyedPost(server, "/test/a/writes", `{"n":1}`, "slow-press", 5*time.Second)
	if err != nil || status != http.StatusConflict || !strings.Contains(body, contracts.IdempotencyKeyInFlight) ||
		header.Get("Retry-After") != "1" {
		t.Fatalf("the repeat after the client gave up: %d %s (%v), want 409 %s: the first run is still going",
			status, body, err, contracts.IdempotencyKeyInFlight)
	}
	for {
		status, body, header, err = keyedPost(server, "/test/a/writes", `{"n":1}`, "slow-press", 5*time.Second)
		if err != nil {
			t.Fatalf("a later repeat: %v", err)
		}
		if status != http.StatusConflict {
			break
		}
		if time.Since(started) > time.Minute {
			t.Fatalf("the first run never finished: still %d %s after a minute", status, body)
		}
		time.Sleep(time.Second)
	}
	if status != http.StatusCreated || header.Get(contracts.IdempotencyReplayedHeader) != "?1" || !strings.Contains(body, `"run":1`) {
		t.Fatalf("the repeat once the first run finished: %d %s replayed=%q, want the first run's 201 replayed",
			status, body, header.Get(contracts.IdempotencyReplayedHeader))
	}
	if elapsed := time.Since(started); elapsed < 20*time.Second {
		t.Fatalf("answered after %v, before the first run's 20 s were up", elapsed)
	}
	if runs, writes := rig.runs.Load(), rig.writes(t); runs != 1 || len(writes) != 1 {
		t.Fatalf("the handler ran %d times and wrote %v, want once and one row", runs, writes)
	}
}

// A run that outlives its key's lease keeps the key: it renews the lease while it runs, so a repeat
// past the lease is still told the first run is going rather than taking the key over and making
// the write a second time. Only a run whose renewals stopped - its process died - is taken over.
func TestARunningHandlerRenewsItsKeyPastTheLease(t *testing.T) {
	release, releaseOnce := heldUntilReleased(t)
	running := make(chan struct{})
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			close(running)
			<-release
		}
		answerRun(run, w, r)
	})
	rig.server.requestKeyLease, rig.server.requestKeyRenewal = 2*time.Second, 200*time.Millisecond
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", nil, "alice", "long-run") }()
	<-running
	time.Sleep(3 * time.Second)
	repeat := rig.send(t, "a", nil, "alice", "long-run")
	if code, _ := refusalOf(t, repeat); repeat.Code != http.StatusConflict || code != contracts.IdempotencyKeyInFlight {
		t.Fatalf("a repeat past the lease while the first run renews it: status=%d body=%s, want 409 %s",
			repeat.Code, repeat.Body.String(), contracts.IdempotencyKeyInFlight)
	}
	releaseOnce()
	first := awaitResponse(t, answered)
	if after := rig.send(t, "a", nil, "alice", "long-run"); !replayed(after) || after.Body.String() != first.Body.String() {
		t.Fatalf("the repeat once the first run answered: replayed=%v body=%s, want %s", replayed(after), after.Body.String(), first.Body.String())
	}
	if runs := rig.runs.Load(); runs != 1 {
		t.Fatalf("the handler ran %d times, want once", runs)
	}

	// A holder whose renewals stopped - its process gone - is taken over once its lease lapses.
	dead := httptest.NewRequest(http.MethodPost, "/test/a/writes", nil)
	dead.Header.Set("Content-Type", "application/json")
	dead.SetPathValue("thing", "a")
	holdRequestKey(t, rig.database, heldKey{login: "alice", key: "dead-run", request: dead, pattern: keyedRigPattern})
	if inside := rig.send(t, "a", nil, "alice", "dead-run"); inside.Code != http.StatusConflict {
		t.Fatalf("a repeat inside a dead holder's lease: status=%d body=%s, want 409", inside.Code, inside.Body.String())
	}
	time.Sleep(2500 * time.Millisecond)
	if takeover := rig.send(t, "a", nil, "alice", "dead-run"); takeover.Code != http.StatusCreated || replayed(takeover) {
		t.Fatalf("a repeat once the dead holder's lease lapsed: status=%d body=%s, want the request run", takeover.Code, takeover.Body.String())
	}
}

// The handler's context is the server's, not the client's: the client's cancellation mid-run does
// not reach it, and the server's own bound, keyedHandlerLimit, does.
func TestAKeyedHandlerRunsUnderTheServersOwnBound(t *testing.T) {
	running, proceed := make(chan struct{}), make(chan struct{})
	type seen struct {
		err  error
		left time.Duration
	}
	saw := make(chan seen, 1)
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		close(running)
		<-proceed
		observed := seen{err: r.Context().Err()}
		if deadline, ok := r.Context().Deadline(); ok {
			observed.left = time.Until(deadline)
		}
		saw <- observed
		answerRun(run, w, r)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/test/a/writes", bytes.NewReader([]byte("{}")))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(contracts.IdempotencyKeyHeader, "bounded")
	response := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		rig.handler.ServeHTTP(response, request)
	}()
	<-running
	cancel()
	close(proceed)
	<-served
	observed := <-saw
	if observed.err != nil || response.Code != http.StatusCreated {
		t.Fatalf("a run whose client went away mid-run: context error %v, status %d, want it run to its answer",
			observed.err, response.Code)
	}
	if observed.left < keyedHandlerLimit-time.Minute || observed.left > keyedHandlerLimit {
		t.Fatalf("the handler's deadline is %v away, want the server's %v bound", observed.left, keyedHandlerLimit)
	}
}

// A keyed handler still running is part of the server's background work: shutdown's wait does not
// return before it does, so the database pool is never closed under it.
func TestTheBackgroundWaitWaitsForARunningKeyedHandler(t *testing.T) {
	release, releaseOnce := heldUntilReleased(t)
	running := make(chan struct{})
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		close(running)
		<-release
		answerRun(run, w, r)
	})
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", nil, "alice", "draining") }()
	<-running
	waited := make(chan struct{})
	go func() {
		rig.server.detached.wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("the background wait returned while a keyed handler still ran")
	case <-time.After(500 * time.Millisecond):
	}
	releaseOnce()
	awaitResponse(t, answered)
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
		t.Fatal("the background wait never returned once the handler answered")
	}
}

// backgroundWork's wait returns only once nothing runs, however work starts and ends around it:
// work that starts while it waits, which a sync.WaitGroup forbids, is waited for too.
func TestTheBackgroundWaitWaitsForWorkStartedWhileItWaits(t *testing.T) {
	var work backgroundWork
	work.wait()
	work.start()
	waited := make(chan struct{})
	go func() {
		work.wait()
		close(waited)
	}()
	work.start()
	work.done()
	select {
	case <-waited:
		t.Fatal("the wait returned with work still running")
	case <-time.After(100 * time.Millisecond):
	}
	work.done()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait never returned once the work ended")
	}
}

// The slow-write test hook delays every keyed run of the method and path it was armed for, on the
// handler's own context, and is mounted only under test hooks. A run whose context ends during the
// delay never reaches its handler.
func TestTheSlowWriteHookDelaysEveryKeyedRunOfItsPath(t *testing.T) {
	handler, database, _ := newTestServer(t, testServerOptions{testHooks: true})
	issue := createTestIssue(t, handler, "SLOW", "Slow writes")
	path := "/api/v1/issues/" + issue + "/comments"
	for name, body := range map[string]map[string]any{
		"a method that takes no key": {"method": "PUT", "path": path, "delay_ms": 10},
		"a path that is not a path":  {"method": "POST", "path": "comments", "delay_ms": 10},
		"a negative delay":           {"method": "POST", "path": path, "delay_ms": -1},
		"an unknown field":           {"method": "POST", "path": path, "delay_ms": 10, "once": true},
	} {
		if refused := dispatchRequest(t, handler, http.MethodPost, "/api/v1/writes/_test/slow", body, "alice"); refused.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s, want 400", name, refused.Code, refused.Body.String())
		}
	}
	arm := func(delay int) {
		t.Helper()
		if armed := dispatchRequest(t, handler, http.MethodPost, "/api/v1/writes/_test/slow",
			map[string]any{"method": "POST", "path": path, "delay_ms": delay}, "alice"); armed.Code != http.StatusOK {
			t.Fatalf("arm %d ms: status=%d body=%s", delay, armed.Code, armed.Body.String())
		}
	}
	arm(700)
	for _, key := range []string{"slow-1", "slow-2"} {
		started := time.Now()
		if created := keyedRequest(t, handler, http.MethodPost, path, map[string]any{"body": key}, "alice", key); created.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d body=%s", key, created.Code, created.Body.String())
		}
		if elapsed := time.Since(started); elapsed < 700*time.Millisecond {
			t.Fatalf("%s answered after %v, inside the armed delay", key, elapsed)
		}
	}
	// A run whose context ends inside the delay never reaches the handler: answered 503, its key
	// released and nothing written.
	direct := directServer(Deps{TestHooksEnabled: true})
	direct.slowWrites.Store(http.MethodPost+" /cut", time.Minute)
	reached := false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cut := httptest.NewRecorder()
	direct.slowedByTestHook(func(http.ResponseWriter, *http.Request) { reached = true })(cut,
		httptest.NewRequestWithContext(ctx, http.MethodPost, "/cut", nil))
	if reached || cut.Code != http.StatusServiceUnavailable {
		t.Fatalf("a run cut off inside the delay: reached=%v status=%d, want 503 with the handler never run", reached, cut.Code)
	}
	arm(0)
	started := time.Now()
	if created := keyedRequest(t, handler, http.MethodPost, path, map[string]any{"body": "disarmed"}, "alice", "fast"); created.Code != http.StatusCreated ||
		time.Since(started) > 500*time.Millisecond {
		t.Fatalf("a run once disarmed: status=%d after %v, want it at once", created.Code, time.Since(started))
	}
	var comments int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from comments where issue_key = $1`, issue).Scan(&comments); err != nil {
		t.Fatal(err)
	}
	if comments != 3 {
		t.Fatalf("the issue holds %d comments, want the three keyed runs'", comments)
	}

	plain, _, _ := newTestServer(t, testServerOptions{})
	if response := dispatchRequest(t, plain, http.MethodPost, "/api/v1/writes/_test/slow",
		map[string]any{"method": "POST", "path": path, "delay_ms": 10}, "alice"); response.Code != http.StatusNotFound {
		t.Fatalf("the hook without DISPATCH_TEST_HOOKS: status=%d, want 404", response.Code)
	}
}

// countingBody is a request body that counts what is read from it.
type countingBody struct {
	io.Reader
	read atomic.Int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	b.read.Add(int64(n))
	return n, err
}

// INFRA-258 (FinalReview5 L3, gate-fixes S21). A repeat of a keyed write whose first request still
// runs is never spooled: the key is looked up by its login and key before the body is read, so a
// slow upload's repeats - the page asks again about once a second - are never written to disk again
// only to be told to wait. Its body is read to its end, discarded and written only to the digest it
// is judged by once it has arrived (TestARepeatFoundInFlightIsJudgedOnceItsBodyHasArrived); here
// the first still runs, so it is 409 KEY_IN_FLIGHT. With the temporary directory gone, a spool
// would answer 500.
func TestAnInFlightRepeatIsJudgedWithoutSpoolingItsBody(t *testing.T) {
	const pattern = "/api/v1/issues/{key}/artifacts"
	rig := newKeyedRigAt(t, pattern, answerRun)
	body := `{"name":"big.bin","content":"` + strings.Repeat("a", 2<<20) + `"}`
	first := httptest.NewRequest(http.MethodPost, "/api/v1/issues/UP-1/artifacts", strings.NewReader(body))
	first.Header.Set("Content-Type", "application/json")
	first.SetPathValue("key", "UP-1")
	holdRequestKey(t, rig.database, heldKey{login: "alice", key: "slow-upload", request: first, pattern: pattern})

	sent := &countingBody{Reader: strings.NewReader(body)}
	repeat := httptest.NewRequest(http.MethodPost, "/api/v1/issues/UP-1/artifacts", sent)
	repeat.Header.Set("Content-Type", "application/json")
	repeat.Header.Set("X-Dispatch-User", "alice")
	repeat.Header.Set(contracts.IdempotencyKeyHeader, "slow-upload")
	response := httptest.NewRecorder()
	unspooled(t, func() { rig.handler.ServeHTTP(response, repeat) })
	if code, _ := refusalOf(t, response); response.Code != http.StatusConflict || code != contracts.IdempotencyKeyInFlight ||
		response.Header().Get("Retry-After") != "1" || response.Header().Get(contracts.IdempotencyKeyHeader) != "slow-upload" {
		t.Fatalf("a repeat while the first upload runs: status=%d headers=%v body=%s, want 409 %s naming its key, unspooled",
			response.Code, response.Header(), response.Body.String(), contracts.IdempotencyKeyInFlight)
	}
	if read := sent.read.Load(); read != int64(len(body)) {
		t.Fatalf("the repeat was judged with %d of its %d bytes read, want all of them", read, len(body))
	}
	if runs := rig.runs.Load(); runs != 0 {
		t.Fatalf("the handler ran %d times, want none", runs)
	}
}

// A client that gives up while its key is being claimed leaves the claim to finish: the claim's
// statements run on a context the client's cancellation does not reach, so a claim the database
// made is never left in flight with no handler behind it, holding every repeat at KEY_IN_FLIGHT
// for a lease. Here the claim waits behind another transaction's hold on the key, the client gives
// up meanwhile, and once the hold goes the request is claimed, run once and answered under its key.
func TestAKeyedClaimFinishesWhenItsClientGivesUpDuringIt(t *testing.T) {
	rig := newKeyedRig(t, answerRun)
	ctx := context.Background()
	hold, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hold.Rollback(ctx) })
	if _, err := hold.Exec(ctx, `
		insert into request_keys (login, idempotency_key, request_digest, method, path)
		values ('alice', 'cut-claim', 'held', 'POST', '/test/a/writes')
	`); err != nil {
		t.Fatalf("hold the key: %v", err)
	}
	client, giveUp := context.WithCancel(ctx)
	defer giveUp()
	request := httptest.NewRequestWithContext(client, http.MethodPost, "/test/a/writes", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Dispatch-User", "alice")
	request.Header.Set(contracts.IdempotencyKeyHeader, "cut-claim")
	response := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		rig.handler.ServeHTTP(response, request)
	}()
	waitForDatabaseLocks(t, hold, 1)
	giveUp()
	// Long enough for a claim bound to the client's context to be cancelled in the database.
	time.Sleep(300 * time.Millisecond)
	if err := hold.Rollback(ctx); err != nil {
		t.Fatalf("release the hold: %v", err)
	}
	select {
	case <-served:
	case <-time.After(10 * time.Second):
		t.Fatal("the request never finished once the hold on its key was released")
	}
	if response.Code != http.StatusCreated || rig.runs.Load() != 1 {
		t.Fatalf("a request whose client gave up mid-claim: status=%d body=%s, ran %d times; want it claimed, run once and answered",
			response.Code, response.Body.String(), rig.runs.Load())
	}
	var status *int
	if err := rig.database.Pool.QueryRow(ctx,
		`select status from request_keys where login = 'alice' and idempotency_key = 'cut-claim'`).Scan(&status); err != nil {
		t.Fatalf("read the key: %v", err)
	}
	if status == nil || *status != http.StatusCreated {
		t.Fatalf("the key holds status %v, want the run's 201 kept for its repeats", status)
	}
}

// A handler that runs on past its limit - its context ended and it did not stop - holds its key no
// longer: the lease is renewed until the limit and no further, so once it lapses a repeat is
// answered as one whose holder died, here taking over a key the run made nothing under, rather than
// told KEY_IN_FLIGHT for as long as the handler ignores its context.
func TestARunPastItsLimitStopsRenewingItsLease(t *testing.T) {
	release, releaseOnce := heldUntilReleased(t)
	running := make(chan struct{})
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			close(running)
			<-release
		}
		answerRun(run, w, r)
	})
	rig.server.requestKeyLease, rig.server.requestKeyRenewal, rig.server.requestKeyRunLimit =
		2*time.Second, 200*time.Millisecond, time.Second
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", nil, "alice", "overrun") }()
	<-running
	// The limit, then a whole lease past the last renewal it allows.
	time.Sleep(4 * time.Second)
	takeover := rig.send(t, "a", nil, "alice", "overrun")
	if takeover.Code != http.StatusCreated || replayed(takeover) || !strings.Contains(takeover.Body.String(), `"run":2`) {
		t.Fatalf("a repeat a lease past the run's limit: status=%d body=%s, want the key taken over and run again",
			takeover.Code, takeover.Body.String())
	}
	releaseOnce()
	awaitResponse(t, answered)
}
