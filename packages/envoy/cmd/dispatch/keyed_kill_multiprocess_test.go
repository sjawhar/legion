//go:build multiprocess

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
		response, err := pair.a.sendKeyed(http.MethodPost, path, body, "killed-press")
		if err == nil {
			_ = response.Body.Close()
			err = errors.New("process A answered the held write: " + response.Status)
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

	repeat := func() (int, string, http.Header) {
		t.Helper()
		response, err := pair.b.sendKeyed(http.MethodPost, path, body, "killed-press")
		if err != nil {
			t.Fatalf("repeat on process B: %v", err)
		}
		defer response.Body.Close()
		answer, _ := io.ReadAll(response.Body)
		return response.StatusCode, string(answer), response.Header
	}
	status, answer, header := repeat()
	if status != http.StatusConflict || !json.Valid([]byte(answer)) || header.Get("Retry-After") != "1" {
		t.Fatalf("a repeat inside the dead run's lease: %d %s, want 409 %s with Retry-After: 1", status, answer, contracts.IdempotencyKeyInFlight)
	}
	var refusal struct{ Code string }
	_ = json.Unmarshal([]byte(answer), &refusal)
	if refusal.Code != contracts.IdempotencyKeyInFlight {
		t.Fatalf("a repeat inside the dead run's lease: code %s, want %s", refusal.Code, contracts.IdempotencyKeyInFlight)
	}
	for status == http.StatusConflict {
		if time.Since(killed) > contracts.IdempotencyLease+30*time.Second {
			t.Fatalf("process B never took the key over: still %d %s %v after the kill", status, answer, time.Since(killed))
		}
		time.Sleep(time.Second)
		status, answer, header = repeat()
	}
	if status != http.StatusCreated || header.Get(contracts.IdempotencyReplayedHeader) != "" {
		t.Fatalf("the repeat once the lease lapsed: %d %s replayed=%q, want the write made by process B", status, answer,
			header.Get(contracts.IdempotencyReplayedHeader))
	}
	if waited := time.Since(killed); waited < contracts.IdempotencyLease-5*time.Second {
		t.Fatalf("process B took the key over %v after the kill, inside the dead run's %v lease", waited, contracts.IdempotencyLease)
	}
	if status, answer, header = repeat(); status != http.StatusCreated || header.Get(contracts.IdempotencyReplayedHeader) != "?1" {
		t.Fatalf("a repeat after the takeover: %d %s, want the takeover's answer replayed", status, answer)
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
