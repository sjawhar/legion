package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
)

// The committed-events count (events.Broker.Committed, the metrics endpoint's
// dispatch_events_committed_total) rises by exactly what the event log grows by across a workload
// of every kind of write that appends: an issue create, a plain and an anchored comment, an edit,
// an ask and a settlement. Each side is read with the document service quiet, so no settlement
// commits between the two reads.
func TestCommittedEventsRiseByTheEventLogsGrowth(t *testing.T) {
	handler, database, deps := newTestServer(t, testServerOptions{})
	documents := deps.Docs.(*docs.Service)
	ctx := context.Background()
	logged := func() int64 {
		t.Helper()
		var count int64
		if err := database.Pool.QueryRow(ctx, `select count(*) from events`).Scan(&count); err != nil {
			t.Fatalf("count the event log: %v", err)
		}
		return count
	}
	quiet := func() {
		t.Helper()
		if err := documents.Quiesce(ctx); err != nil {
			t.Fatalf("quiesce the document service: %v", err)
		}
	}
	quiet()
	loggedBefore, committedBefore := logged(), deps.Events.Committed()

	issue := createInteractionIssue(t, handler, "TEST", "Committed events", "The quick brown fox jumps.")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "A plain comment.",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("plain comment: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "About the fox.", "anchor": map[string]any{"artifact": "spec", "quote": "fox"},
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("anchored comment: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+issue.PrimaryArtifactID+"/edits", map[string]any{
		"ops": []map[string]string{{"op": "replace", "find": "quick", "with": "slow"}},
	}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("edit: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/asks", map[string]any{
		"question": "Ship it?", "actor": sessionActor(),
	}); response.Code != http.StatusCreated {
		t.Fatalf("ask: status=%d body=%s", response.Code, response.Body.String())
	}
	// A settlement indexes the marker ask block the edit inserts, appending its ask.opened.
	settleDocument(t, handler, issue.PrimaryArtifactID, issue.Key, "committed-events")
	quiet()

	grew, committed := logged()-loggedBefore, deps.Events.Committed()-committedBefore
	if grew < 7 {
		t.Fatalf("the workload appended %d events, want at least one per write", grew)
	}
	if int64(committed) != grew {
		t.Fatalf("Committed rose by %d while the event log grew by %d", committed, grew)
	}
}
