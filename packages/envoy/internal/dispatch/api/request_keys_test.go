package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// keyedRequest sends body to target as login, under key when key is not empty, as the dashboard
// sends a write.
func keyedRequest(t *testing.T, handler http.Handler, method, target string, body any, login, key string) *httptest.ResponseRecorder {
	t.Helper()
	var data []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		data = encoded
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	if login != "" {
		request.Header.Set("X-Dispatch-User", login)
	}
	if key != "" {
		request.Header.Set(contracts.IdempotencyKeyHeader, key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// keyedRig is one write route behind the keyed layer, as register mounts every POST and PATCH
// route, whose handler counts its runs and answers as the test says.
type keyedRig struct {
	handler  http.Handler
	database *store.Store
	runs     atomic.Int32
}

const keyedRigPattern = "/test/{thing}/writes"

func newKeyedRig(t *testing.T, answer func(run int32, w http.ResponseWriter, r *http.Request)) *keyedRig {
	t.Helper()
	_, database, _, served := newRegisteredTestServer(t, testServerOptions{})
	rig := &keyedRig{database: database}
	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodPost+" "+keyedRigPattern, trackTransactions(served.keyedWrites(
		http.MethodPost, keyedRigPattern,
		func(w http.ResponseWriter, r *http.Request) { answer(rig.runs.Add(1), w, r) },
		nil,
	)))
	rig.handler = mux
	return rig
}

// send posts body to thing's writes as login under key.
func (rig *keyedRig) send(t *testing.T, thing string, body any, login, key string) *httptest.ResponseRecorder {
	t.Helper()
	return keyedRequest(t, rig.handler, http.MethodPost, "/test/"+thing+"/writes", body, login, key)
}

// rows counts the request_keys rows.
func (rig *keyedRig) rows(t *testing.T) int {
	t.Helper()
	return countRequestKeys(t, rig.database)
}

func countRequestKeys(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `select count(*) from request_keys`).Scan(&count); err != nil {
		t.Fatalf("count request keys: %v", err)
	}
	return count
}

// answerRun answers 201 with the run's number, so a replay can be told from a second run.
func answerRun(run int32, w http.ResponseWriter, _ *http.Request) {
	WriteJSON(w, http.StatusCreated, map[string]int32{"run": run})
}

func replayed(response *httptest.ResponseRecorder) bool {
	return response.Header().Get(contracts.IdempotencyReplayedHeader) == "?1"
}

// refusalOf reads a refusal's code and text.
func refusalOf(t *testing.T, response *httptest.ResponseRecorder) (code, text string) {
	t.Helper()
	refusal := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, response)
	return refusal.Code, refusal.Error
}

// heldUntilReleased is a channel a held handler waits on, and its one release, which the test's
// cleanup runs too, so a test that fails while a handler waits never leaves it waiting.
func heldUntilReleased(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	release := make(chan struct{})
	once := sync.OnceFunc(func() { close(release) })
	t.Cleanup(once)
	return release, once
}

// INFRA-258. A repeat under a key - a retry after a lost answer, a double press - is answered the
// first request's stored status and body, marked replayed, and the handler runs once. A key sent
// as a structured-field string is the same key as its bare form.
func TestARepeatUnderAKeyReplaysTheFirstAnswerAndRunsOnce(t *testing.T) {
	rig := newKeyedRig(t, answerRun)
	first := rig.send(t, "a", map[string]any{"n": 1}, "alice", "press-1")
	if first.Code != http.StatusCreated || replayed(first) {
		t.Fatalf("first: status=%d replayed=%v body=%s, want 201 run fresh", first.Code, replayed(first), first.Body.String())
	}
	for _, key := range []string{"press-1", `"press-1"`} {
		repeat := rig.send(t, "a", map[string]any{"n": 1}, "alice", key)
		if repeat.Code != http.StatusCreated || !replayed(repeat) || repeat.Body.String() != first.Body.String() ||
			repeat.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("repeat under %s: status=%d replayed=%v type=%q body=%s, want the first answer replayed",
				key, repeat.Code, replayed(repeat), repeat.Header().Get("Content-Type"), repeat.Body.String())
		}
	}
	if runs := rig.runs.Load(); runs != 1 {
		t.Fatalf("the handler ran %d times, want once", runs)
	}
}

// A key names one request. Reused for another body, path value or query, it is refused 422,
// naming the request that used it, and nothing runs or is written.
func TestAKeyReusedForAnotherRequestIsRefused(t *testing.T) {
	rig := newKeyedRig(t, answerRun)
	if first := rig.send(t, "a", map[string]any{"n": 1}, "alice", "reused"); first.Code != http.StatusCreated {
		t.Fatalf("first: status=%d body=%s", first.Code, first.Body.String())
	}
	before := databaseFingerprint(t, rig.database)
	for name, request := range map[string]struct {
		target string
		body   any
	}{
		"another body":       {"/test/a/writes", map[string]any{"n": 2}},
		"another path value": {"/test/b/writes", map[string]any{"n": 1}},
		"another query":      {"/test/a/writes?dry_run=true", map[string]any{"n": 1}},
	} {
		t.Run(name, func(t *testing.T) {
			response := keyedRequest(t, rig.handler, http.MethodPost, request.target, request.body, "alice", "reused")
			code, text := refusalOf(t, response)
			if response.Code != http.StatusUnprocessableEntity || code != contracts.IdempotencyKeyReused ||
				!strings.Contains(text, "POST /test/a/writes") || !strings.Contains(text, "this request was not run") {
				t.Fatalf("status=%d code=%s error=%q, want 422 %s naming POST /test/a/writes", response.Code, code, text, contracts.IdempotencyKeyReused)
			}
		})
	}
	assertUnchanged(t, "a reused key", before, databaseFingerprint(t, rig.database))
	if runs := rig.runs.Load(); runs != 1 {
		t.Fatalf("the handler ran %d times, want once", runs)
	}
}

// A repeat while the first request still runs is 409 KEY_IN_FLIGHT with Retry-After: 1, which a
// client asks again after; once the first has answered, the repeat is that answer.
func TestARepeatWhileTheFirstRunsIsInFlight(t *testing.T) {
	running := make(chan struct{})
	release, releaseOnce := heldUntilReleased(t)
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			close(running)
			<-release
		}
		answerRun(run, w, r)
	})
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", map[string]any{"n": 1}, "alice", "slow") }()
	<-running

	repeat := rig.send(t, "a", map[string]any{"n": 1}, "alice", "slow")
	if code, _ := refusalOf(t, repeat); repeat.Code != http.StatusConflict || code != contracts.IdempotencyKeyInFlight ||
		repeat.Header().Get("Retry-After") != "1" {
		t.Fatalf("a repeat in flight: status=%d code=%s retry-after=%q, want 409 %s with Retry-After: 1",
			repeat.Code, code, repeat.Header().Get("Retry-After"), contracts.IdempotencyKeyInFlight)
	}
	releaseOnce()
	first := awaitResponse(t, answered)
	after := rig.send(t, "a", map[string]any{"n": 1}, "alice", "slow")
	if after.Code != http.StatusCreated || !replayed(after) || after.Body.String() != first.Body.String() {
		t.Fatalf("a repeat once answered: status=%d body=%s, want the first answer %s", after.Code, after.Body.String(), first.Body.String())
	}
	if runs := rig.runs.Load(); runs != 1 {
		t.Fatalf("the handler ran %d times, want once", runs)
	}
}

// Many attempts under one key at once: one runs, and every other is told it is in flight or, once
// the one has answered, given its answer.
func TestConcurrentAttemptsUnderOneKeyRunOnce(t *testing.T) {
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		answerRun(run, w, r)
	})
	const attempts = 8
	var wait sync.WaitGroup
	responses := make(chan *httptest.ResponseRecorder, attempts)
	for range attempts {
		wait.Go(func() { responses <- rig.send(t, "a", map[string]any{"n": 1}, "alice", "race") })
	}
	wait.Wait()
	close(responses)
	fresh := 0
	for response := range responses {
		switch {
		case response.Code == http.StatusCreated && !replayed(response):
			fresh++
		case response.Code == http.StatusCreated && replayed(response):
		case response.Code == http.StatusConflict:
			if code, _ := refusalOf(t, response); code != contracts.IdempotencyKeyInFlight {
				t.Errorf("409 %s, want %s", code, contracts.IdempotencyKeyInFlight)
			}
		default:
			t.Errorf("status=%d body=%s", response.Code, response.Body.String())
		}
	}
	if fresh != 1 || rig.runs.Load() != 1 {
		t.Fatalf("%d fresh answers and %d runs, want one of each", fresh, rig.runs.Load())
	}
}

// An answer that says nothing of whether the write can succeed - a 5xx - or only that this
// attempt was not heard - a 401, 408 or 429 - is not stored: the next attempt runs again, and the
// key is free. Any other 4xx is the request's verdict and is replayed.
func TestOnlyAVerdictIsStored(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusUnauthorized,
		http.StatusRequestTimeout, http.StatusTooManyRequests,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
				if run == 1 {
					writeError(w, "SOMETHING", status, "this attempt")
					return
				}
				answerRun(run, w, r)
			})
			if first := rig.send(t, "a", nil, "alice", "k"); first.Code != status {
				t.Fatalf("first: status=%d, want %d", first.Code, status)
			}
			if rows := rig.rows(t); rows != 0 {
				t.Fatalf("a %d left %d key rows, want none", status, rows)
			}
			again := rig.send(t, "a", nil, "alice", "k")
			if again.Code != http.StatusCreated || replayed(again) || rig.runs.Load() != 2 {
				t.Fatalf("the next attempt: status=%d replayed=%v runs=%d, want the handler run again", again.Code, replayed(again), rig.runs.Load())
			}
		})
	}
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			rig := newKeyedRig(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
				writeError(w, "REFUSED", status, "refused")
			})
			first := rig.send(t, "a", nil, "alice", "k")
			again := rig.send(t, "a", nil, "alice", "k")
			if again.Code != status || !replayed(again) || again.Body.String() != first.Body.String() || rig.runs.Load() != 1 {
				t.Fatalf("a repeat of a %d: status=%d replayed=%v runs=%d, want the refusal replayed", status, again.Code, replayed(again), rig.runs.Load())
			}
		})
	}
	// A handler that writes nothing answers 200 with an empty body, and that is what is replayed.
	rig := newKeyedRig(t, func(int32, http.ResponseWriter, *http.Request) {})
	rig.send(t, "a", nil, "alice", "k")
	if again := rig.send(t, "a", nil, "alice", "k"); again.Code != http.StatusOK || !replayed(again) || again.Body.Len() != 0 || rig.runs.Load() != 1 {
		t.Fatalf("a repeat of an empty answer: status=%d body=%q runs=%d", again.Code, again.Body.String(), rig.runs.Load())
	}
}

// A handler that panics answers nothing worth keeping: its key is released, and the next attempt
// runs the request again.
func TestAPanickingHandlerReleasesItsKey(t *testing.T) {
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			panic("the handler's own failure")
		}
		answerRun(run, w, r)
	})
	func() {
		defer func() {
			if recovered := recover(); recovered != "the handler's own failure" {
				t.Fatalf("recovered %v, want the handler's panic carried on to net/http", recovered)
			}
		}()
		rig.send(t, "a", nil, "alice", "k")
	}()
	if rows := rig.rows(t); rows != 0 {
		t.Fatalf("a panic left %d key rows, want none", rows)
	}
	if again := rig.send(t, "a", nil, "alice", "k"); again.Code != http.StatusCreated || rig.runs.Load() != 2 {
		t.Fatalf("the next attempt: status=%d runs=%d, want the request run again", again.Code, rig.runs.Load())
	}
}

// A key belongs to the login that sent it. Another person's identical request under the same key
// is another request; the same person under another casing of the identity header is the same
// person; the shared agent token's keys are its own.
func TestAKeyBelongsToItsLogin(t *testing.T) {
	rig := newKeyedRig(t, answerRun)
	alices := rig.send(t, "a", nil, "alice", "shared")
	if bobs := rig.send(t, "a", nil, "bob", "shared"); bobs.Code != http.StatusCreated || replayed(bobs) {
		t.Fatalf("bob under alice's key: status=%d replayed=%v, want a run of his own", bobs.Code, replayed(bobs))
	}
	if again := rig.send(t, "a", nil, "Alice", "shared"); !replayed(again) || again.Body.String() != alices.Body.String() {
		t.Fatalf("Alice's repeat: replayed=%v body=%s, want alice's answer %s", replayed(again), again.Body.String(), alices.Body.String())
	}
	request := httptest.NewRequest(http.MethodPost, "/test/a/writes", nil)
	request.Header.Set("Authorization", "Bearer agent-token")
	request.Header.Set(contracts.IdempotencyKeyHeader, "shared")
	agent := httptest.NewRecorder()
	rig.handler.ServeHTTP(agent, request)
	if agent.Code != http.StatusCreated || replayed(agent) {
		t.Fatalf("the shared agent token under the key: status=%d replayed=%v, want a run of its own", agent.Code, replayed(agent))
	}
	if runs := rig.runs.Load(); runs != 3 {
		t.Fatalf("the handler ran %d times, want alice's, bob's and the agent's", runs)
	}

	owner, service := "Carol@example.com", "system:serviceaccount:legion:worker"
	for name, test := range map[string]struct {
		actor model.Actor
		human bool
		want  string
	}{
		"a person":                {model.Actor{Kind: "user", ID: " Carol@Example.com "}, true, "carol@example.com"},
		"that person's own token": {model.Actor{Kind: "session", Owner: &owner}, false, "carol@example.com"},
		"a service token":         {model.Actor{Service: &service}, false, "service:" + service},
		"the shared agent token":  {model.Actor{}, false, requestKeyAgentToken},
	} {
		if got := requestKeyLogin(test.actor, test.human); got != test.want {
			t.Errorf("%s's keys belong to %q, want %q", name, got, test.want)
		}
	}
}

// A key is kept IdempotencyKeyTTL. The sweep deletes an older one, and a repeat that finds one the
// sweep has not reached yet treats the key as unused: the request runs again.
func TestAKeyPastItsWindowIsUnusedAgain(t *testing.T) {
	rig := newKeyedRig(t, answerRun)
	rig.send(t, "a", nil, "alice", "old")
	rig.send(t, "a", nil, "alice", "new")
	age := func(key string) {
		t.Helper()
		if _, err := rig.database.Pool.Exec(context.Background(),
			`update request_keys set created_at = now() - interval '25 hours' where idempotency_key = $1`, key); err != nil {
			t.Fatalf("age %s: %v", key, err)
		}
	}
	age("old")
	if err := sweepRequestKeys(context.Background(), rig.database.Pool); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	var kept []string
	rows, err := rig.database.Pool.Query(context.Background(), `select idempotency_key from request_keys`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, key)
	}
	if !equalStrings(kept, []string{"new"}) {
		t.Fatalf("after the sweep the keys are %v, want the one inside its window", kept)
	}
	if again := rig.send(t, "a", nil, "alice", "old"); replayed(again) || rig.runs.Load() != 3 {
		t.Fatalf("a swept key: replayed=%v runs=%d, want the request run again", replayed(again), rig.runs.Load())
	}
	age("new")
	if again := rig.send(t, "a", nil, "alice", "new"); replayed(again) || rig.runs.Load() != 4 {
		t.Fatalf("a key past its window, not yet swept: replayed=%v runs=%d, want the request run again", replayed(again), rig.runs.Load())
	}
}

// A write without a key runs as it always did, every time, and leaves no key behind.
func TestAWriteWithoutAKeyRunsEachTime(t *testing.T) {
	rig := newKeyedRig(t, answerRun)
	for range 2 {
		response := rig.send(t, "a", map[string]any{"n": 1}, "alice", "")
		if response.Code != http.StatusCreated || replayed(response) || response.Header().Get(contracts.IdempotencyKeyHeader) != "" {
			t.Fatalf("a keyless write: status=%d replayed=%v key=%q", response.Code, replayed(response), response.Header().Get(contracts.IdempotencyKeyHeader))
		}
	}
	if runs, rows := rig.runs.Load(), rig.rows(t); runs != 2 || rows != 0 {
		t.Fatalf("two keyless writes ran %d times and left %d key rows, want 2 and none", runs, rows)
	}
}

// Every answer to a keyed request names its key back - run, replayed, refused 422, refused 409 and
// a 5xx the key does not keep - so a client can tell a Dispatch that judged the key from one that
// ignored the header, and retries a 5xx under its key only against the first.
func TestEveryKeyedAnswerNamesItsKey(t *testing.T) {
	running := make(chan struct{})
	var slowRuns atomic.Int32
	release, releaseOnce := heldUntilReleased(t)
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		switch {
		case r.PathValue("thing") == "broken":
			writeError(w, "BROKEN", http.StatusInternalServerError, "the handler's own failure")
		case r.PathValue("thing") == "slow":
			if slowRuns.Add(1) == 1 {
				close(running)
				<-release
			}
			answerRun(run, w, r)
		default:
			answerRun(run, w, r)
		}
	})
	names := func(name string, response *httptest.ResponseRecorder, status int, key string) {
		t.Helper()
		if response.Code != status || response.Header().Get(contracts.IdempotencyKeyHeader) != key {
			t.Errorf("%s: status=%d key=%q, want %d naming %q", name, response.Code, response.Header().Get(contracts.IdempotencyKeyHeader), status, key)
		}
	}
	names("a run", rig.send(t, "a", nil, "alice", "echo-1"), http.StatusCreated, "echo-1")
	names("a replay", rig.send(t, "a", nil, "alice", `"echo-1"`), http.StatusCreated, "echo-1")
	names("a reused key", rig.send(t, "b", nil, "alice", "echo-1"), http.StatusUnprocessableEntity, "echo-1")
	names("a 5xx", rig.send(t, "broken", nil, "alice", "echo-2"), http.StatusInternalServerError, "echo-2")
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "slow", nil, "alice", "echo-3") }()
	<-running
	names("a key in flight", rig.send(t, "slow", nil, "alice", "echo-3"), http.StatusConflict, "echo-3")
	releaseOnce()
	names("the slow run", awaitResponse(t, answered), http.StatusCreated, "echo-3")
}

// A header that is not a key is refused before anything runs; a key of every allowed class, of
// the longest length, bare or quoted, is taken.
func TestAMalformedKeyIsRefusedBeforeAnythingRuns(t *testing.T) {
	rig := newKeyedRig(t, answerRun)
	for name, values := range map[string][]string{
		"a space":              {"press 1"},
		"a slash":              {"press/1"},
		"past 128 characters":  {strings.Repeat("k", contracts.IdempotencyKeyMax+1)},
		"empty":                {""},
		"empty quotes":         {`""`},
		"an unclosed quote":    {`"press-1`},
		"two headers":          {"press-1", "press-2"},
		"a quoted inner quote": {`"pre"ss"`},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/test/a/writes", nil)
			request.Header.Set("X-Dispatch-User", "alice")
			for _, value := range values {
				request.Header.Add(contracts.IdempotencyKeyHeader, value)
			}
			response := httptest.NewRecorder()
			rig.handler.ServeHTTP(response, request)
			if code, _ := refusalOf(t, response); response.Code != http.StatusBadRequest || code != contracts.IdempotencyKeyInvalid {
				t.Fatalf("status=%d code=%s, want 400 %s", response.Code, code, contracts.IdempotencyKeyInvalid)
			}
		})
	}
	if runs := rig.runs.Load(); runs != 0 {
		t.Fatalf("the handler ran %d times for refused keys", runs)
	}
	for _, key := range []string{strings.Repeat("k", contracts.IdempotencyKeyMax), "A1._:-z", `"A1._:-y"`} {
		if response := rig.send(t, "a", nil, "alice", key); response.Code != http.StatusCreated {
			t.Fatalf("key %q: status=%d body=%s, want 201", key, response.Code, response.Body.String())
		}
	}
}

// A holder whose lease lapsed - its process presumed dead - loses its key to the first repeat past
// the lease, which runs the request. The old holder, only slow, still answers its own client, but
// writes nothing to the key: the repeat's answer is the one replayed.
func TestALapsedHolderIsTakenOver(t *testing.T) {
	running := make(chan struct{})
	release, releaseOnce := heldUntilReleased(t)
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		if run == 1 {
			close(running)
			<-release
		}
		answerRun(run, w, r)
	})
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rig.send(t, "a", nil, "alice", "stalled") }()
	<-running
	if _, err := rig.database.Pool.Exec(context.Background(),
		`update request_keys set leased_at = now() - interval '2 minutes'`); err != nil {
		t.Fatalf("lapse the lease: %v", err)
	}
	takeover := rig.send(t, "a", nil, "alice", "stalled")
	if takeover.Code != http.StatusCreated || replayed(takeover) || !strings.Contains(takeover.Body.String(), `"run":2`) {
		t.Fatalf("the repeat past the lease: status=%d body=%s, want the request run as run 2", takeover.Code, takeover.Body.String())
	}
	releaseOnce()
	if first := awaitResponse(t, answered); first.Code != http.StatusCreated || !strings.Contains(first.Body.String(), `"run":1`) {
		t.Fatalf("the slow holder's own answer: status=%d body=%s, want run 1", first.Code, first.Body.String())
	}
	if again := rig.send(t, "a", nil, "alice", "stalled"); !replayed(again) || again.Body.String() != takeover.Body.String() {
		t.Fatalf("a later repeat: replayed=%v body=%s, want the takeover's answer %s", replayed(again), again.Body.String(), takeover.Body.String())
	}
}

// An answer past what a key keeps is answered in full the first time, and replayed as its status
// with a body saying it was not kept.
func TestAnAnswerPastTheKeptSizeReplaysItsStatusAlone(t *testing.T) {
	large := strings.Repeat("x", keyedAnswerKept+10)
	rig := newKeyedRig(t, func(_ int32, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, large[:keyedAnswerKept/2])
		_, _ = io.WriteString(w, large[keyedAnswerKept/2:])
	})
	first := rig.send(t, "a", nil, "alice", "large")
	if first.Code != http.StatusCreated || first.Body.String() != large {
		t.Fatalf("first: status=%d, %d bytes, want 201 with all %d", first.Code, first.Body.Len(), len(large))
	}
	again := rig.send(t, "a", nil, "alice", "large")
	if code, text := refusalOf(t, again); again.Code != http.StatusCreated || !replayed(again) ||
		code != contracts.IdempotencyAnswerNotKept || !strings.Contains(text, "answered 201") {
		t.Fatalf("repeat: status=%d replayed=%v code=%s error=%q, want 201 %s", again.Code, replayed(again), code, text, contracts.IdempotencyAnswerNotKept)
	}
	if runs := rig.runs.Load(); runs != 1 {
		t.Fatalf("the handler ran %d times, want once", runs)
	}
}

// A client mints a multipart body's boundary afresh for every attempt, so a retry of one upload
// under its key is a repeat whatever its boundary; a different file is another request. A body past
// what the layer holds in memory reaches the handler whole.
func TestAMultipartRetryUnderANewBoundaryIsARepeat(t *testing.T) {
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read the body: %v", err)
		}
		WriteJSON(w, http.StatusCreated, map[string]any{"run": run, "bytes": len(data)})
	})
	content := bytes.Repeat([]byte("0123456789abcdef"), (keyedBodyInMemory/16)*2)
	upload := func(boundary string, file []byte) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		if err := writer.SetBoundary(boundary); err != nil {
			t.Fatal(err)
		}
		if err := writer.WriteField("name", "notes.bin"); err != nil {
			t.Fatal(err)
		}
		part, err := writer.CreateFormFile("file", "notes.bin")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = part.Write(file)
		_ = writer.Close()
		size := body.Len()
		request := httptest.NewRequest(http.MethodPost, "/test/a/writes", &body)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.Header.Set("X-Dispatch-User", "alice")
		request.Header.Set(contracts.IdempotencyKeyHeader, "upload-1")
		response := httptest.NewRecorder()
		rig.handler.ServeHTTP(response, request)
		if response.Code == http.StatusCreated && !replayed(response) && !strings.Contains(response.Body.String(), fmt.Sprintf(`"bytes":%d`, size)) {
			t.Fatalf("the handler read %s, want all %d bytes", response.Body.String(), size)
		}
		return response
	}
	first := upload("first-attempt-boundary", content)
	if first.Code != http.StatusCreated {
		t.Fatalf("first: status=%d body=%s", first.Code, first.Body.String())
	}
	if again := upload("second-attempt-boundary-0123456789", content); !replayed(again) || again.Body.String() != first.Body.String() {
		t.Fatalf("a retry under a new boundary: status=%d replayed=%v body=%s, want the first answer", again.Code, replayed(again), again.Body.String())
	}
	other := append(bytes.Clone(content), '!')
	if changed := upload("third", other); changed.Code != http.StatusUnprocessableEntity {
		t.Fatalf("another file under the key: status=%d body=%s, want 422", changed.Code, changed.Body.String())
	}
	if runs := rig.runs.Load(); runs != 1 {
		t.Fatalf("the handler ran %d times, want once", runs)
	}
}

// boundaryMask replaces every boundary however the body is cut into writes.
func TestBoundaryMaskFindsABoundaryAcrossWrites(t *testing.T) {
	body := "--BOUNDARY\r\nfield\r\n--BOUNDARY--\r\n"
	want := strings.ReplaceAll(body, "BOUNDARY", string(maskedBoundary))
	for size := 1; size <= len(body); size++ {
		var out bytes.Buffer
		mask := &boundaryMask{w: &out, boundary: []byte("BOUNDARY")}
		for at := 0; at < len(body); at += size {
			_, _ = mask.Write([]byte(body[at:min(at+size, len(body))]))
		}
		mask.flush()
		if out.String() != want {
			t.Fatalf("writes of %d bytes masked %q, want %q", size, out.String(), want)
		}
	}
}

// Every POST and PATCH route takes a key, and no other: a header that is not a key is refused on
// each of them before its handler runs, and ignored by a PUT and a DELETE, which are idempotent
// already. A GET writes nothing and is never wrapped.
func TestEveryPostAndPatchRouteTakesAKey(t *testing.T) {
	handler, _, _ := newTestServer(t, testServerOptions{})
	for _, route := range (&server{}).routes() {
		if route.Method == http.MethodGet {
			continue
		}
		target := route.Pattern
		for _, name := range patternWildcards(route.Pattern) {
			target = strings.Replace(target, "{"+name+"}", "x", 1)
			target = strings.Replace(target, "{"+name+"...}", "x", 1)
		}
		request := httptest.NewRequest(route.Method, target, strings.NewReader("{}"))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Dispatch-User", "alice")
		request.Header.Set(contracts.IdempotencyKeyHeader, "not a key")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		refused := strings.Contains(response.Body.String(), `"code":"`+contracts.IdempotencyKeyInvalid+`"`)
		if keyedMethod(route.Method) != refused {
			t.Errorf("%s %s: status=%d body=%.200s, want the key judged only on a POST or a PATCH",
				route.Method, route.Pattern, response.Code, response.Body.String())
		}
	}
}

// The browser case on a real route: a comment posted again under its key, its first answer lost,
// lands once, and the repeat is answered the comment the first made.
func TestACommentPostedAgainUnderItsKeyLandsOnce(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	issue := createTestIssue(t, handler, "KEYS", "Keyed comments")
	post := func() *httptest.ResponseRecorder {
		t.Helper()
		return keyedRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue+"/comments",
			map[string]any{"body": "Looks right to me."}, "alice", "comment-press-1")
	}
	first, again := post(), post()
	if first.Code != http.StatusCreated || again.Code != http.StatusCreated || !replayed(again) {
		t.Fatalf("first=%d repeat=%d replayed=%v, want 201 and its replay", first.Code, again.Code, replayed(again))
	}
	if decodeBody[model.Comment](t, first).ID != decodeBody[model.Comment](t, again).ID {
		t.Fatalf("the repeat answered another comment: %s vs %s", first.Body.String(), again.Body.String())
	}
	var comments int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from comments where issue_key = $1 and body = 'Looks right to me.'`, issue).Scan(&comments); err != nil {
		t.Fatal(err)
	}
	if comments != 1 {
		t.Fatalf("the issue holds %d comments, want one", comments)
	}
}

// requestDigestOf is the digest keyedWrites judges request by under pattern, so a test can leave
// the row a process that died mid-request leaves.
func requestDigestOf(t *testing.T, request *http.Request, pattern string) string {
	t.Helper()
	digest := sha256.New()
	writeRequestFields(digest, request, request.Method, pattern, patternWildcards(pattern))
	body, err := readKeyedBody(request.Body, digest, request.Header.Get("Content-Type"))
	defer body.close()
	if err != nil {
		t.Fatalf("read the request's body: %v", err)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// heldKey is a request_keys row as a request left it.
type heldKey struct {
	login, key string
	request    *http.Request
	pattern    string
	leasedAgo  time.Duration
	status     *int
	recovery   any
}

// holdRequestKey writes held's row: in flight when status is nil, leased leasedAgo.
func holdRequestKey(t *testing.T, database *store.Store, held heldKey) {
	t.Helper()
	var recovery []byte
	if held.recovery != nil {
		encoded, err := json.Marshal(held.recovery)
		if err != nil {
			t.Fatal(err)
		}
		recovery = encoded
	}
	if _, err := database.Pool.Exec(context.Background(), `
		insert into request_keys (login, idempotency_key, request_digest, method, path, leased_at, status, response, recovery)
		values ($1, $2, $3, $4, $5, now() - make_interval(secs => $6), $7, case when $7::int is null then null else ''::bytea end, $8)
	`, held.login, held.key, requestDigestOf(t, held.request, held.pattern), held.request.Method, held.request.URL.Path,
		held.leasedAgo.Seconds(), held.status, recovery); err != nil {
		t.Fatalf("hold the key %s: %v", held.key, err)
	}
}

// jsonRequest is a JSON request for a digest, its path values set as the mux sets them.
func jsonRequest(t *testing.T, method, target string, body any, pathValues map[string]string) *http.Request {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(data))
	request.Header.Set("Content-Type", "application/json")
	for name, value := range pathValues {
		request.SetPathValue(name, value)
	}
	return request
}
