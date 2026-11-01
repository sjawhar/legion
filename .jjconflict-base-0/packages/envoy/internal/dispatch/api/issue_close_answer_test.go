package api

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// What a close and a reopen answer about the issue they moved: the sequence their write phase
// committed, and the asks as they leave them.

// TestClosingAndReopeningAnswerTheSequenceTheWritePhaseCommitted drives a close and a reopen whose
// asks' document write queues an event of its own - the refresh of a comment whose stored quote is
// stale - before the status event. The patch's response, the status event's stored payload and a
// later read of the issue must all name the sequence that event took, which a count of the events
// the route itself queued answers short.
func TestClosingAndReopeningAnswerTheSequenceTheWritePhaseCommitted(t *testing.T) {
	f := newCloseAsksFixture(t)
	anchored := createThreadComment(t, f.handler, f.issue, map[string]any{
		"body":   "Does this still hold?",
		"anchor": map[string]any{"artifact": "spec", "quote": "Context"},
	}, "alice")
	for _, move := range []struct {
		name      string
		status    string
		eventType string
	}{
		{name: "the close", status: "done", eventType: "issue.closed"},
		{name: "the reopen", status: "todo", eventType: "issue.updated"},
	} {
		t.Run(move.name, func(t *testing.T) {
			staleAnchorQuote(t, f.database, anchored.ID)
			before := readIssueDetail(t, f.handler, f.issue)
			patched := dispatchRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
				"status": move.status,
			}, "alice")
			if patched.Code != http.StatusOK {
				t.Fatalf("%s: status=%d body=%s", move.name, patched.Code, patched.Body.String())
			}
			response := decodeBody[model.Issue](t, patched)
			event := latestIssueEvent(t, f.database, f.issue, move.eventType)
			if refreshes := anchorRefreshesWithin(t, f.database, f.issue, before.LastSeq, event.seq); refreshes != 1 {
				t.Fatalf("%s appended %d comment.anchor_refreshed events of its own, want 1: the stale quote no longer makes the asks' document write queue one",
					move.name, refreshes)
			}
			detail := readIssueDetail(t, f.handler, f.issue)
			if response.LastSeq != event.seq || event.payloadLastSeq != event.seq || detail.LastSeq != event.seq {
				t.Fatalf("%s answered last_seq %d and stored %s at seq %d with payload last_seq %d, and a read of the issue answers %d: want all four at %d, the sequence the write phase committed",
					move.name, response.LastSeq, move.eventType, event.seq, event.payloadLastSeq, detail.LastSeq, event.seq)
			}
		})
	}
}

// TestCloseAndReopenAdviceDescribeTheAsksThePatchLeaves closes an issue on which the closing
// session holds open asks, and reopens it. A close withdraws every open ask of its issue, so the
// advice it answers names none; the reopen restores them, so the advice it answers names the
// session's own again. Both read their advice in the patch's read phase, before the withdrawal
// and the restoration are written, so the rows the read finds are the ones the patch moves.
func TestCloseAndReopenAdviceDescribeTheAsksThePatchLeaves(t *testing.T) {
	f := newCloseAsksFixture(t)
	closed := sessionRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
		"status": "done", "actor": sessionActor(),
	})
	advice := adviceFromResponse(t, closed.Code, closed.Body.String())
	if len(advice.YourOpenAsks) != 0 || advice.YourOpenAsks == nil {
		t.Fatalf("the close answered your_open_asks %#v, want []: it withdrew every open ask of the issue", advice.YourOpenAsks)
	}
	reopened := sessionRequest(t, f.handler, http.MethodPatch, "/api/v1/issues/"+f.issue, map[string]any{
		"status": "todo", "actor": sessionActor(),
	})
	advice = adviceFromResponse(t, reopened.Code, reopened.Body.String())
	want := []testAdviceAsk{
		{ID: f.approvalAsk, Question: "Approve spec.md (version 1)?"},
		{ID: f.personAsk, Question: "Which region?"},
	}
	if !reflect.DeepEqual(advice.YourOpenAsks, want) {
		t.Fatalf("the reopen answered your_open_asks %#v, want the two oldest asks it restored for the reopening session, %#v", advice.YourOpenAsks, want)
	}
}

// staleAnchorQuote leaves comment's stored anchor quoting text the document does not hold, so the
// next write of the document refreshes that anchor and appends the event of the refresh.
func staleAnchorQuote(t *testing.T, database *store.Store, comment string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		update comments set anchor = jsonb_set(anchor, '{quote}', to_jsonb('stale'::text)) where id = $1
	`, comment); err != nil {
		t.Fatalf("stale the anchor of comment %s: %v", comment, err)
	}
}

// issueEventRow is a stored issue event's own sequence and the last_seq its payload records.
type issueEventRow struct {
	seq            int
	payloadLastSeq int
}

func latestIssueEvent(t *testing.T, database *store.Store, issue, eventType string) issueEventRow {
	t.Helper()
	var row issueEventRow
	if err := database.Pool.QueryRow(context.Background(), `
		select seq, (payload->>'last_seq')::int from events
		where issue_key = $1 and type = $2 order by seq desc limit 1
	`, issue, eventType).Scan(&row.seq, &row.payloadLastSeq); err != nil {
		t.Fatalf("read the latest %s of %s: %v", eventType, issue, err)
	}
	return row
}

// anchorRefreshesWithin counts the comment anchor refreshes issue took in (after, through]: the
// sequences one write appended, read from the issue's sequence before it and the sequence of its
// own last event.
func anchorRefreshesWithin(t *testing.T, database *store.Store, issue string, after, through int) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(), `
		select count(*) from events
		where issue_key = $1 and type = 'comment.anchor_refreshed' and seq > $2 and seq <= $3
	`, issue, after, through).Scan(&count); err != nil {
		t.Fatalf("count the anchor refreshes of %s: %v", issue, err)
	}
	return count
}
