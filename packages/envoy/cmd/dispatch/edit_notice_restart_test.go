package main

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// A person's edit notice is a row, not a timer: a process killed while the notice is still inside
// its quiet window leaves it to the next process on the same database, which sends it once the
// window ends. Both processes take one DISPATCH_SIGNING_KEY, as every process of a deployment
// does: the killed process's registry row is still live when the second starts, and a key the
// second minted in its own data dir would hold its start up to a live window, until that row
// lapsed (processes.Register).
func TestEditNoticeRestart(t *testing.T) {
	database := storetest.Open(t)
	options := dispatchOptions{SigningKey: "edit-notice-restart-key", Env: map[string]string{"DISPATCH_TEST_EDIT_NOTICE_QUIET": "3s"}}
	first := startDispatchProcessWith(t, database.Pool.Config().ConnString(), options)
	first.waitHealthy(t)
	key, artifactID := first.createIssue(t)
	editor := first.openEditor(t, artifactID, "before\n")
	replaceText(t, editor, "before", "after")
	// A close drops whatever the socket has not delivered - the read loop that would have taken
	// the update breaks on the closed connection instead - so the room has to hold the edit before
	// the browser leaves, and the process's own read of the document is where it shows.
	waitForDocumentText(t, first, artifactID, "after\n", 10*time.Second)
	// The last browser leaving settles the room at once (settleLastPeer), so the edit's version and
	// its notice are written without waiting out the settle delay.
	editor.Close()
	waitForNotice(t, database, artifactID, 10*time.Second)
	if err := first.Cmd.Process.Kill(); err != nil {
		t.Fatalf("kill the first process: %v", err)
	}
	first.WaitExit(t, "SIGKILL")
	var sentBeforeKill int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from events where issue_key = $1 and type = 'artifact.human_edited'`, key).Scan(&sentBeforeKill); err != nil {
		t.Fatalf("count the notices sent before the kill: %v", err)
	}
	if sentBeforeKill != 0 {
		t.Fatalf("the first process sent %d notices before it was killed, want none: the quiet window had not ended", sentBeforeKill)
	}

	second := startDispatchProcessWith(t, database.Pool.Config().ConnString(), options)
	second.waitHealthy(t)
	deadline := time.Now().Add(15 * time.Second)
	for humanEditedEvents(t, second, key) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no artifact.human_edited within 15 s of the second process's start:\n%s", second.Output.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if count := humanEditedEvents(t, second, key); count != 1 {
		t.Fatalf("artifact.human_edited events = %d, want 1", count)
	}
}

// waitForDocumentText waits up to within for the process to render artifactID's document as want:
// the room has taken the editor's update, which is what the settlement the editor's close runs
// settles.
func waitForDocumentText(t *testing.T, process *dispatchProcess, artifactID, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		answer := process.call(t, http.MethodGet, "/api/v1/artifacts/"+artifactID+"/text", "", http.StatusOK)
		var document struct {
			Markdown string `json:"markdown"`
		}
		if err := json.Unmarshal(answer, &document); err != nil {
			t.Fatalf("decode the document's text: %v: %s", err, answer)
		}
		if document.Markdown == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the room holds %q within %v of the editor's edit, want %q:\n%s",
				document.Markdown, within, want, process.Output.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForNotice waits up to within for artifactID's pending edit notice to be written.
func waitForNotice(t *testing.T, database *store.Store, artifactID string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var pending int
		if err := database.Pool.QueryRow(context.Background(),
			`select count(*) from document_edit_notices where artifact_id = $1`, artifactID).Scan(&pending); err != nil {
			t.Fatalf("read the document's edit notice: %v", err)
		}
		if pending == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the person's settled edit wrote no edit notice within %v", within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// humanEditedEvents counts the artifact.human_edited events in the issue's event log, read through
// the process's API as any reader reads them.
func humanEditedEvents(t *testing.T, process *dispatchProcess, key string) int {
	t.Helper()
	body := process.call(t, http.MethodGet, "/api/v1/issues/"+key+"/events?limit=200", "", http.StatusOK)
	var events []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		t.Fatalf("decode the issue's events: %v: %s", err, body)
	}
	count := 0
	for _, event := range events {
		if event.Type == "artifact.human_edited" {
			count++
		}
	}
	return count
}
