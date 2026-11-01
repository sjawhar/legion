//go:build multiprocess

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
)

// INFRA-258. A process killed while it runs a keyed write leaves the write's key held under a lease
// nothing renews any more. The other process answers a repeat under that key 409 KEY_IN_FLIGHT while
// the lease lasts - the first run might still be alive - and, once the lease has lapsed with nothing
// committed, takes the key over and makes the write, once; a repeat after that is its replay.
//
// The slow-write test hook holds A's run after its key is claimed and before its handler writes,
// which is where a process dies with nothing of the write committed; the kill is SIGKILL, so no
// shutdown drains it.
func TestAKeyedWriteAProcessDiedRunningIsTakenOverOnceItsLeaseLapses(t *testing.T) {
	pair := startPair(t, nil)
	project := uniqueProjectKey(t, "KIL")
	pair.a.createProject(t, project, "Killed mid-write")
	issue, _ := pair.a.createIssueIn(t, project, "A write a process died running", "spec")
	path := "/api/v1/issues/" + issue + "/comments"
	const body = `{"body":"Made once, whoever makes it."}`
	pair.a.call(t, http.MethodPost, "/api/v1/writes/_test/slow",
		`{"method":"POST","path":"`+path+`","delay_ms":600000}`, http.StatusOK)

	first := make(chan error, 1)
	go func() {
		answer, err := pair.a.trySend(http.MethodPost, path, body, asKeyed(dispatchTestLogin, "killed-press"))
		if err == nil {
			err = errors.New("process A answered the held write: " + answer.String())
		}
		first <- err
	}()
	held := func() bool {
		var rows int
		if err := pair.database.Pool.QueryRow(context.Background(),
			`select count(*) from request_keys where idempotency_key = 'killed-press' and status is null`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		return rows == 1
	}
	for deadline := time.Now().Add(30 * time.Second); !held(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("process A never claimed the key:\n%s", pair.a.Output.String())
		}
	}
	killed := time.Now()
	if err := pair.a.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill process A: %v", err)
	}
	<-pair.a.Exited
	if err := <-first; err == nil {
		t.Fatal("the held write's client saw no failure when its process died")
	}

	repeat := func() httpAnswer {
		t.Helper()
		return pair.b.send(t, http.MethodPost, path, body, asKeyed(dispatchTestLogin, "killed-press"))
	}
	answer := repeat()
	if answer.status != http.StatusConflict || !json.Valid(answer.body) || answer.header.Get("Retry-After") != "1" {
		t.Fatalf("a repeat inside the dead run's lease: %v, want 409 %s with Retry-After: 1", answer, contracts.IdempotencyKeyInFlight)
	}
	if answer.code() != contracts.IdempotencyKeyInFlight {
		t.Fatalf("a repeat inside the dead run's lease: code %s, want %s", answer.code(), contracts.IdempotencyKeyInFlight)
	}
	for answer.status == http.StatusConflict {
		if time.Since(killed) > contracts.IdempotencyLease+30*time.Second {
			t.Fatalf("process B never took the key over: still %v %v after the kill", answer, time.Since(killed))
		}
		time.Sleep(time.Second)
		answer = repeat()
	}
	if answer.status != http.StatusCreated || answer.header.Get(contracts.IdempotencyReplayedHeader) != "" {
		t.Fatalf("the repeat once the lease lapsed: %v replayed=%q, want the write made by process B", answer,
			answer.header.Get(contracts.IdempotencyReplayedHeader))
	}
	if waited := time.Since(killed); waited < contracts.IdempotencyLease-5*time.Second {
		t.Fatalf("process B took the key over %v after the kill, inside the dead run's %v lease", waited, contracts.IdempotencyLease)
	}
	if answer = repeat(); answer.status != http.StatusCreated || answer.header.Get(contracts.IdempotencyReplayedHeader) != "?1" {
		t.Fatalf("a repeat after the takeover: %v, want the takeover's answer replayed", answer)
	}
	var comments int
	if err := pair.database.Pool.QueryRow(context.Background(),
		`select count(*) from comments where issue_key = $1 and body = 'Made once, whoever makes it.'`, issue).Scan(&comments); err != nil &&
		!errors.Is(err, pgx.ErrNoRows) {
		t.Fatal(err)
	}
	if comments != 1 {
		t.Fatalf("the issue holds %d comments, want the one process B made", comments)
	}
}
