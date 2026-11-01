package main

import (
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A REST edit whose client hangs up while the edit's transaction is still running must not fail
// the document's room: the edit's outcome is the server's to finish, and a failed room closes
// every editor's socket on it ("refuse a transaction's operation on a failed document room ...
// commit live document write: conn closed", seen four times in INFRA-644's editor load test).
// The test slows the edit's transaction at the database with a trigger on doc_updates, which
// every live write of a document appends to, and closes the REST client's connection while the
// transaction waits on it: once inside the COMMIT (a deferred trigger, which runs at COMMIT), and
// once inside a statement before it (an immediate trigger). Either way a signed-in editor stays
// connected, the room is not failed, and the room goes on taking edits; an edit whose client left
// during its COMMIT is committed, since the commit runs apart from the request's cancellation.
// On main before INFRA-592 the room failed and closed the editor within a millisecond of the
// client leaving during the COMMIT (INFRA-644's editor load test saw it four times).
func TestARestEditWhoseClientLeavesMidTransactionLeavesItsRoomOpen(t *testing.T) {
	for _, slow := range []struct {
		name, when string
		committed  bool
	}{
		{"during the commit", "deferrable initially deferred", true},
		{"during a statement before the commit", "not deferrable", false},
	} {
		t.Run(slow.name, func(t *testing.T) {
			database := storetest.Open(t)
			for _, statement := range []string{
				`create table tester_slow_gate (on_ boolean)`,
				`create function tester_slow_commit() returns trigger language plpgsql as $$
				begin
					if exists (select 1 from tester_slow_gate) then perform pg_sleep(4); end if;
					return null;
				end $$`,
				`create constraint trigger tester_slow_commit after insert on doc_updates ` + slow.when +
					` for each row execute function tester_slow_commit()`,
			} {
				if _, err := database.Pool.Exec(t.Context(), statement); err != nil {
					t.Fatalf("install the slow trigger: %v", err)
				}
			}
			process := startDispatchProcess(t, database.Pool.Config().ConnString())
			process.waitHealthy(t)
			_, artifactID := process.createIssue(t)
			editor := process.openEditor(t, artifactID, "before\n")
			// The new spec's own settlement writes doc_updates under the document's lock: armed
			// before it ends, the trigger would slow it and hold the edit at that lock instead.
			waitForSettlementOwed(t, database, artifactID, false)

			if _, err := database.Pool.Exec(t.Context(), `insert into tester_slow_gate values (true)`); err != nil {
				t.Fatalf("arm the slow trigger: %v", err)
			}
			connection, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(process.port))
			if err != nil {
				t.Fatalf("dial Dispatch: %v", err)
			}
			body := `{"ops":[{"op":"insert","after":"end","markdown":"written by a client that left"}]}`
			if _, err := fmt.Fprintf(connection, "POST /api/v1/artifacts/%s/edits HTTP/1.1\r\nHost: dispatch\r\n"+
				"X-Dispatch-User: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
				artifactID, dispatchTestLogin, len(body), body); err != nil {
				t.Fatalf("send the edit: %v", err)
			}
			time.Sleep(1500 * time.Millisecond)
			_ = connection.Close()
			left := time.Now()
			time.Sleep(4 * time.Second)
			if _, err := database.Pool.Exec(t.Context(), `delete from tester_slow_gate`); err != nil {
				t.Fatalf("disarm the slow trigger: %v", err)
			}

			select {
			case <-editor.Ended:
				t.Errorf("the editor's connection closed %v after the REST client left mid-transaction", editor.EndedAt().Sub(left))
			case <-time.After(3 * time.Second):
			}
			for _, line := range strings.Split(process.Output.String(), "\n") {
				if strings.Contains(line, "/edits") || strings.Contains(line, "level=ERROR") || strings.Contains(line, "level=WARN") {
					t.Logf("Dispatch logged: %s", line)
				}
				if strings.Contains(line, "failed document room") || strings.Contains(line, "commit live document write") {
					t.Errorf("Dispatch failed the room: %s", line)
				}
			}
			if t.Failed() {
				t.Logf("Dispatch's output:\n%s", process.Output.String())
				return
			}
			replaceText(t, editor, "before", "after")
			deadline := time.Now().Add(10 * time.Second)
			served := func() string {
				return string(process.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", http.StatusOK))
			}
			for !strings.Contains(served(), "after") {
				if time.Now().After(deadline) {
					t.Fatalf("the room took no edit after the REST client left; the document reads %s", served())
				}
				time.Sleep(200 * time.Millisecond)
			}
			if slow.committed && !strings.Contains(served(), "written by a client that left") {
				t.Errorf("the edit whose client left during its COMMIT was not committed; the document reads %s", served())
			}
		})
	}
}
