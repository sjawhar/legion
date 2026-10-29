package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// INFRA-258. A transaction that only locked rows - a claim's SELECT ... FOR UPDATE - is given a
// transaction id as a write is, and wrote nothing: its commit marks no key. A request whose write
// failed after such a commit made nothing, so its next attempt runs the request again rather than
// being told KEY_COMMITTED for a write that never landed.
func TestALockOnlyTransactionMarksNothing(t *testing.T) {
	var rig *keyedRig
	rig = newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		tx, err := rig.database.Pool.Begin(r.Context())
		if err != nil {
			t.Errorf("begin: %v", err)
			return
		}
		defer tx.Rollback(r.Context())
		if _, err := tx.Exec(r.Context(), `select n from keyed_rig_locked for update`); err != nil {
			t.Errorf("lock: %v", err)
		}
		if err := tx.Commit(r.Context()); err != nil {
			t.Errorf("commit the lock: %v", err)
		}
		if run == 1 {
			writeError(w, "AFTER_THE_LOCK", http.StatusInternalServerError, "failed after the lock")
			return
		}
		answerRun(run, w, r)
	})
	if _, err := rig.database.Pool.Exec(context.Background(),
		`create table keyed_rig_locked (n integer); insert into keyed_rig_locked values (0)`); err != nil {
		t.Fatal(err)
	}
	if first := rig.send(t, "a", nil, "alice", "locked"); first.Code != http.StatusInternalServerError {
		t.Fatalf("first: status=%d body=%s", first.Code, first.Body.String())
	}
	if again := rig.send(t, "a", nil, "alice", "locked"); again.Code != http.StatusCreated || replayed(again) || rig.runs.Load() != 2 {
		t.Fatalf("the next attempt after a lock-only commit: status=%d body=%s runs=%d, want the request run again",
			again.Code, again.Body.String(), rig.runs.Load())
	}
}

// A COMMIT that fails after the mark - a deferred constraint the write broke - rolls the mark back
// with the write: the key is not marked committed, and the next attempt runs the request again. A
// mark taken in a statement of its own before the COMMIT would stay, and every repeat would be told
// KEY_COMMITTED for a write that never landed.
func TestACommitThatFailsAfterTheMarkLeavesNoMark(t *testing.T) {
	var rig *keyedRig
	rig = newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		tx, err := rig.database.Pool.Begin(r.Context())
		if err != nil {
			t.Errorf("begin: %v", err)
			return
		}
		defer tx.Rollback(r.Context())
		// The first run inserts one value twice, which the deferred constraint refuses only at
		// COMMIT, after the commit step has marked the key in this very transaction.
		values := []int32{run}
		if run == 1 {
			values = []int32{7, 7}
		}
		for _, value := range values {
			if _, err := tx.Exec(r.Context(), `insert into keyed_rig_deferred (n) values ($1)`, value); err != nil {
				t.Errorf("insert %d: %v", value, err)
			}
		}
		if err := tx.Commit(r.Context()); err != nil {
			writeError(w, "COMMIT_FAILED", http.StatusInternalServerError, err.Error())
			return
		}
		answerRun(run, w, r)
	})
	if _, err := rig.database.Pool.Exec(context.Background(),
		`create table keyed_rig_deferred (n integer unique deferrable initially deferred)`); err != nil {
		t.Fatal(err)
	}
	first := rig.send(t, "a", nil, "alice", "deferred")
	if first.Code != http.StatusInternalServerError || !strings.Contains(first.Body.String(), "COMMIT_FAILED") {
		t.Fatalf("first: status=%d body=%s, want the COMMIT refused", first.Code, first.Body.String())
	}
	if rows := rig.rows(t); rows != 0 {
		t.Fatalf("request_keys holds %d rows after a refused COMMIT, want the key released: its mark rolled back with the write", rows)
	}
	again := rig.send(t, "a", nil, "alice", "deferred")
	if again.Code != http.StatusCreated || replayed(again) || rig.runs.Load() != 2 {
		t.Fatalf("the next attempt: status=%d body=%s runs=%d, want the request run again", again.Code, again.Body.String(), rig.runs.Load())
	}
	var stored int
	if err := rig.database.Pool.QueryRow(context.Background(), `select count(*) from keyed_rig_deferred`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 1 {
		t.Fatalf("keyed_rig_deferred holds %d rows, want the second run's one", stored)
	}
}

// The 24-hour window through the loop that keeps it: a pass of the maintenance loop deletes a key
// past IdempotencyKeyTTL and keeps one inside it, so a repeat of a day-old press is still answered
// from its key and one past the window is a new write.
func TestTheMaintenanceLoopKeepsAKeyForItsWholeWindow(t *testing.T) {
	rig := newKeyedRig(t, answerRun)
	for _, key := range []string{"inside", "past"} {
		if response := rig.send(t, "a", nil, "alice", key); response.Code != http.StatusCreated {
			t.Fatalf("%s: status=%d", key, response.Code)
		}
	}
	if _, err := rig.database.Pool.Exec(context.Background(), `
		update request_keys set created_at = now() - case idempotency_key
			when 'inside' then interval '23 hours' else interval '25 hours' end
	`); err != nil {
		t.Fatal(err)
	}
	logs := &storetest.LockedLog{}
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunSuggestionOutcomeSweep(ctx, rig.database.Pool, 50*time.Millisecond, sweepLeases(t, rig.database), nil)
	}()
	for deadline := time.Now().Add(30 * time.Second); countLeaseLines(logs, "dispatch duty: leased pass ran", suggestionOutcomeLease) == 0; {
		if time.Now().After(deadline) {
			t.Fatalf("the loop never made a pass:\n%s", logs.Lines("lease="))
		}
		time.Sleep(20 * time.Millisecond)
	}
	stop()
	<-done
	var keys []string
	rows, err := rig.database.Pool.Query(context.Background(), `select idempotency_key from request_keys order by 1`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	if !equalStrings(keys, []string{"inside"}) {
		t.Fatalf("after the loop's pass the keys are %v, want the 23-hour-old one kept and the 25-hour-old one gone", keys)
	}
	if inside := rig.send(t, "a", nil, "alice", "inside"); !replayed(inside) {
		t.Fatalf("a repeat of the 23-hour-old press: status=%d body=%s, want it replayed", inside.Code, inside.Body.String())
	}
}

// A replay carries every header the first answer's handler set, not its Content-Type alone, so a
// header a write answers with (a Location) reaches the client that asks again too.
func TestAReplayCarriesTheFirstAnswersHeaders(t *testing.T) {
	rig := newKeyedRig(t, func(run int32, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/test/a/writes/1")
		answerRun(run, w, r)
	})
	first := rig.send(t, "a", nil, "alice", "located")
	again := rig.send(t, "a", nil, "alice", "located")
	for name, response := range map[string]*httptest.ResponseRecorder{"first": first, "the replay": again} {
		if response.Header().Get("Location") != "/test/a/writes/1" || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
			t.Fatalf("%s: headers %v, want the handler's Location and Content-Type", name, response.Header())
		}
	}
	if !replayed(again) || again.Header().Get(contracts.IdempotencyKeyHeader) != "located" {
		t.Fatalf("the replay: headers %v, want it replayed under its key", again.Header())
	}
}

// A write that committed its recovery point and then answered a refusal - a delivery Dispatch could
// not record, say - is replayed as the resource it made, and nothing of the refusal's body is
// merged into it: only the members a write's answer carries beside the resource (its advice) are
// ever kept from the first answer.
func TestARecoveredReplayKeepsNothingOfARefusal(t *testing.T) {
	stored := []byte(`{"code":"MESSAGE_TARGET_CHANGED","error":"the target moved","advice":{"kept":true}}`)
	refused := http.StatusConflict
	held := heldRequestKey{status: &refused, response: stored, kept: true}
	if body := held.storedSuccess(); body != nil {
		t.Fatalf("a refusal's stored body = %s, want none handed to the replay", body)
	}
	created := http.StatusCreated
	held.status = &created
	if body := held.storedSuccess(); string(body) != string(stored) {
		t.Fatalf("a success's stored body = %s, want it handed to the replay", body)
	}
	response := httptest.NewRecorder()
	(&server{}).writeRefreshed(response, stored, map[string]any{"id": "m1"})
	var answer map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	if _, merged := answer["code"]; merged || string(answer["advice"]) != `{"kept":true}` || string(answer["id"]) != `"m1"` {
		t.Fatalf("the refreshed answer = %s, want the resource and the first answer's advice alone", response.Body.String())
	}
}

// request_keys holds its rows to their states: an answered key holds no lease, and nothing of an
// answer is stored without its status.
func TestARequestKeysRowIsOneOfItsStates(t *testing.T) {
	rig := newKeyedRig(t, answerRun)
	for name, statement := range map[string]string{
		"an answer under a lease": `insert into request_keys (login, idempotency_key, request_digest, method, path, status)
			values ('alice', 'a', 'd', 'POST', '/p', 201)`,
		"a body with no status": `insert into request_keys (login, idempotency_key, request_digest, method, path, response)
			values ('alice', 'b', 'd', 'POST', '/p', ''::bytea)`,
		"attempt zero": `insert into request_keys (login, idempotency_key, request_digest, method, path, attempt)
			values ('alice', 'c', 'd', 'POST', '/p', 0)`,
	} {
		if _, err := rig.database.Pool.Exec(context.Background(), statement); err == nil {
			t.Errorf("%s: stored, want the row refused", name)
		}
	}
}

// Every route the keyed layer's own tables name - those that replay a recovery point, never keep
// their answer, or take an upload's body - is a POST or PATCH route the server registers: a pattern
// spelled wrong would silently turn its rule off.
func TestEveryKeyedRouteTableNamesARoute(t *testing.T) {
	registered := map[string]bool{}
	for _, route := range (&server{}).routes() {
		if keyedMethod(route.Method) {
			registered[route.Method+" "+route.Pattern] = true
		}
	}
	tables := map[string][]string{}
	for route := range (&server{}).keyedReplays() {
		tables["keyedReplays"] = append(tables["keyedReplays"], route)
	}
	for route := range answerHoldsSecret {
		tables["answerHoldsSecret"] = append(tables["answerHoldsSecret"], route)
	}
	for route := range keyedUploads {
		tables["keyedUploads"] = append(tables["keyedUploads"], route)
	}
	for table, routes := range tables {
		for _, route := range routes {
			if !registered[route] {
				t.Errorf("%s names %q, which is no registered POST or PATCH route", table, route)
			}
		}
	}
}
