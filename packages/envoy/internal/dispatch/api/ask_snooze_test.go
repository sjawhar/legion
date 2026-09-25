package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// inboxSnoozes reads one login's inbox as the id -> snoozed_until map the SPA partitions from.
// A listed row with no snooze carries a JSON null, which decodes to the zero-value pointer.
func inboxSnoozes(t *testing.T, handler http.Handler, login string) map[string]*string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, login)
	if response.Code != http.StatusOK {
		t.Fatalf("read %s inbox: status=%d body=%s", login, response.Code, response.Body.String())
	}
	rows := decodeBody[[]struct {
		ID           string  `json:"id"`
		SnoozedUntil *string `json:"snoozed_until"`
	}](t, response)
	snoozes := map[string]*string{}
	for _, row := range rows {
		snoozes[row.ID] = row.SnoozedUntil
	}
	return snoozes
}

func createSnoozeAsk(t *testing.T, handler http.Handler, target, question string) string {
	t.Helper()
	response := sessionRequest(t, handler, http.MethodPost, target, map[string]any{
		"question": question, "actor": sessionActor(),
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("create ask on %s: status=%d body=%s", target, response.Code, response.Body.String())
	}
	return decodeBody[model.Ask](t, response).ID
}

// Snoozing is a viewer's own view state on a row everyone else still sees: it round-trips on
// the Inbox for its snoozer, moves rather than accumulating when snoozed again, reaches no
// other login, and is undone by the un-snooze. A document ask snoozes exactly like an issue
// ask - the whole reason the snooze is keyed on the ask rather than on its issue.
func TestAskSnoozeSurfacesOnTheSnoozersInboxOnly(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create CORE project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Issue",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[model.Issue](t, issueResponse)
	issueAsk := createSnoozeAsk(t, handler, "/api/v1/issues/"+issue.Key+"/asks", "Issue question")
	artifact := createProjectDocument(t, handler, "CORE", "Design notes", "# Design notes\n")
	documentAsk := createSnoozeAsk(t, handler, "/api/v1/artifacts/"+artifact.ID+"/asks", "Document question")

	before := inboxSnoozes(t, handler, "alice")
	if got, listed := before[issueAsk]; !listed || got != nil {
		t.Fatalf("issue ask before any snooze: listed=%t snoozed_until=%v, want listed and null", listed, got)
	}

	until := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Millisecond)
	for _, askID := range []string{issueAsk, documentAsk} {
		saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+askID+"/snooze", map[string]any{
			"snoozed_until": until.Format(time.RFC3339Nano),
		}, "alice")
		if saved.Code != http.StatusOK {
			t.Fatalf("snooze %s: status=%d body=%s", askID, saved.Code, saved.Body.String())
		}
		if got := decodeBody[userAskSnooze](t, saved); !parseTimestamp(t, got.SnoozedUntil).Equal(until) {
			t.Fatalf("snooze %s answered %q, want %s", askID, got.SnoozedUntil, until.Format(time.RFC3339Nano))
		}
	}

	snoozed := inboxSnoozes(t, handler, "alice")
	for _, askID := range []string{issueAsk, documentAsk} {
		got, listed := snoozed[askID]
		if !listed {
			t.Fatalf("snoozed ask %s left Alice's inbox; a snooze is surfaced, never filtered", askID)
		}
		if got == nil || !parseTimestamp(t, *got).Equal(until) {
			t.Fatalf("inbox snoozed_until for %s = %v, want %s", askID, got, until.Format(time.RFC3339Nano))
		}
	}

	// Snoozing again moves the moment; the row stays one row.
	later := until.Add(24 * time.Hour)
	if moved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+issueAsk+"/snooze", map[string]any{
		"snoozed_until": later.Format(time.RFC3339Nano),
	}, "alice"); moved.Code != http.StatusOK {
		t.Fatalf("re-snooze: status=%d body=%s", moved.Code, moved.Body.String())
	}
	moved := inboxSnoozes(t, handler, "alice")
	if len(moved) != 2 {
		t.Fatalf("Alice's inbox has %d rows after a re-snooze, want 2", len(moved))
	}
	if got := moved[issueAsk]; got == nil || !parseTimestamp(t, *got).Equal(later) {
		t.Fatalf("inbox snoozed_until after a re-snooze = %v, want %s", got, later.Format(time.RFC3339Nano))
	}

	for id, got := range inboxSnoozes(t, handler, "bob") {
		if got != nil {
			t.Fatalf("Alice's snooze reached Bob's inbox on %s: %q", id, *got)
		}
	}

	cleared := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/asks/"+issueAsk+"/snooze", nil, "alice")
	if cleared.Code != http.StatusNoContent {
		t.Fatalf("un-snooze: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	if got := inboxSnoozes(t, handler, "alice")[issueAsk]; got != nil {
		t.Fatalf("inbox snoozed_until after an un-snooze = %q, want null", *got)
	}
	// Un-snoozing a row nobody snoozed is the state the caller asked for.
	if repeated := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/asks/"+issueAsk+"/snooze", nil, "alice"); repeated.Code != http.StatusNoContent {
		t.Fatalf("repeated un-snooze: status=%d body=%s", repeated.Code, repeated.Body.String())
	}
}

// A human's actor id is the login as their identity source spells it - GitHub's display
// casing through a cookie, the header verbatim otherwise - so the snooze is keyed on the
// canonical login. One person reaching the API under either spelling holds one snooze, not
// two, and un-snoozes under either.
func TestAskSnoozeIsKeyedOnTheCanonicalLogin(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Casing", "spec")
	askID := createSnoozeAsk(t, handler, "/api/v1/issues/"+issue.Key+"/asks", "Which approach?")
	until := time.Now().UTC().Add(4 * time.Hour).Truncate(time.Millisecond)

	// Written under the display casing GitHub hands back.
	if saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+askID+"/snooze", map[string]any{
		"snoozed_until": until.Format(time.RFC3339Nano),
	}, "Alice"); saved.Code != http.StatusOK {
		t.Fatalf("snooze as Alice: status=%d body=%s", saved.Code, saved.Body.String())
	}
	for _, login := range []string{"Alice", "alice"} {
		got, listed := inboxSnoozes(t, handler, login)[askID]
		if !listed {
			t.Fatalf("%s's inbox does not list the ask", login)
		}
		if got == nil || !parseTimestamp(t, *got).Equal(until) {
			t.Fatalf("snoozed_until read back as %s = %v, want %s", login, got, until.Format(time.RFC3339Nano))
		}
	}

	// The un-snooze is issued under the display casing too. Under `alice` it would delete the
	// canonical row whether or not the delete canonicalises - raw and canonical are the same
	// string there - so that spelling pins nothing; `Alice` is what makes a raw delete miss.
	if cleared := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/asks/"+askID+"/snooze", nil, "Alice"); cleared.Code != http.StatusNoContent {
		t.Fatalf("un-snooze as Alice: status=%d body=%s", cleared.Code, cleared.Body.String())
	}
	if got := inboxSnoozes(t, handler, "alice")[askID]; got != nil {
		t.Fatalf("snoozed_until after an un-snooze under the display casing = %q, want null", *got)
	}
}

// The snooze window is absolute: an agent replying to a snoozed ask hands the turn back to
// the human, and the row must still carry its unchanged snooze so the Inbox keeps it folded
// away. Only the moment passing or an explicit un-snooze brings it back.
func TestAgentReplyLeavesASnoozeIntact(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Snoozed", "spec")
	askID := createSnoozeAsk(t, handler, "/api/v1/issues/"+issue.Key+"/asks", "Which approach?")
	// The reader defers it while the agent still owes the next move.
	if noted := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Working on it.", "ask_id": askID, "turn": "agent", "actor": sessionActor(),
	}); noted.Code != http.StatusCreated {
		t.Fatalf("progress note: status=%d body=%s", noted.Code, noted.Body.String())
	}
	until := time.Now().UTC().Add(6 * time.Hour).Truncate(time.Millisecond)
	if saved := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+askID+"/snooze", map[string]any{
		"snoozed_until": until.Format(time.RFC3339Nano),
	}, "alice"); saved.Code != http.StatusOK {
		t.Fatalf("snooze: status=%d body=%s", saved.Code, saved.Body.String())
	}

	// The agent answers back: the turn is the reader's again.
	if reply := sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issue.Key+"/comments", map[string]any{
		"body": "Here is what I found.", "ask_id": askID, "actor": sessionActor(),
	}); reply.Code != http.StatusCreated {
		t.Fatalf("agent reply: status=%d body=%s", reply.Code, reply.Body.String())
	}

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", response.Code, response.Body.String())
	}
	rows := decodeBody[[]struct {
		ID           string  `json:"id"`
		WaitingOn    string  `json:"waiting_on"`
		SnoozedUntil *string `json:"snoozed_until"`
	}](t, response)
	if len(rows) != 1 || rows[0].ID != askID {
		t.Fatalf("inbox rows = %#v, want the one snoozed ask", rows)
	}
	if rows[0].WaitingOn != "human" {
		t.Fatalf("waiting_on after the agent's reply = %q, want human", rows[0].WaitingOn)
	}
	if rows[0].SnoozedUntil == nil || !parseTimestamp(t, *rows[0].SnoozedUntil).Equal(until) {
		t.Fatalf("snoozed_until after the agent's reply = %v, want the unchanged %s",
			rows[0].SnoozedUntil, until.Format(time.RFC3339Nano))
	}
}

// Nothing sweeps user_ask_snooze, so a snooze whose moment has passed must read back as the
// ordinary row it is: still listed, carrying the moment the reader compares against now.
func TestInboxSurfacesASnoozeWhoseMomentHasPassed(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create CORE project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Issue",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[model.Issue](t, issueResponse)
	askID := createSnoozeAsk(t, handler, "/api/v1/issues/"+issue.Key+"/asks", "Issue question")

	// The route refuses a past moment, so only time can produce one: write it directly.
	lapsed := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	if _, err := database.Pool.Exec(context.Background(), `
		insert into user_ask_snooze (login, ask_id, snoozed_until) values ($1, $2, $3)
	`, "alice", askID, lapsed); err != nil {
		t.Fatalf("record a lapsed snooze: %v", err)
	}

	got, listed := inboxSnoozes(t, handler, "alice")[askID]
	if !listed {
		t.Fatal("a row whose snooze has passed left the inbox; the server never filters on the snooze")
	}
	if got == nil || !parseTimestamp(t, *got).Equal(lapsed) {
		t.Fatalf("inbox snoozed_until for a lapsed snooze = %v, want %s", got, lapsed.Format(time.RFC3339Nano))
	}
}

func TestAskSnoozeRefusesBadInputAndNonHumans(t *testing.T) {
	handler := newTestHandler(t)
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "CORE", "name": "Core",
	}, "alice"); response.Code != http.StatusCreated {
		t.Fatalf("create CORE project: status=%d body=%s", response.Code, response.Body.String())
	}
	issueResponse := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "CORE", "title": "Issue",
	}, "alice")
	if issueResponse.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", issueResponse.Code, issueResponse.Body.String())
	}
	issue := decodeBody[model.Issue](t, issueResponse)
	askID := createSnoozeAsk(t, handler, "/api/v1/issues/"+issue.Key+"/asks", "Issue question")
	future := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)

	for _, probe := range []struct {
		name   string
		askID  string
		body   any
		status int
		code   string
	}{
		{"a non-uuid ask id", "CORE-1", map[string]any{"snoozed_until": future}, http.StatusBadRequest, "ASK_ID_INPUT"},
		{"an ask that does not exist", "00000000-0000-4000-8000-000000000000", map[string]any{"snoozed_until": future}, http.StatusNotFound, "NOT_FOUND"},
		{"no snoozed_until", askID, map[string]any{}, http.StatusBadRequest, "SNOOZE_INPUT"},
		{"a blank snoozed_until", askID, map[string]any{"snoozed_until": ""}, http.StatusBadRequest, "SNOOZE_INPUT"},
		{"a malformed snoozed_until", askID, map[string]any{"snoozed_until": "tomorrow"}, http.StatusBadRequest, "SNOOZE_INPUT"},
		{"a snoozed_until in the past", askID, map[string]any{
			"snoozed_until": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		}, http.StatusBadRequest, "SNOOZE_INPUT"},
	} {
		response := dispatchRequest(t, handler, http.MethodPut, "/api/v1/me/asks/"+probe.askID+"/snooze", probe.body, "alice")
		if response.Code != probe.status {
			t.Fatalf("snooze with %s: status=%d body=%s, want %d", probe.name, response.Code, response.Body.String(), probe.status)
		}
		if got := decodeBody[struct {
			Code string `json:"code"`
		}](t, response); got.Code != probe.code {
			t.Fatalf("snooze with %s: code=%q, want %q", probe.name, got.Code, probe.code)
		}
	}

	// The inbox is human-only, so the snooze that folds one of its rows away is too.
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		response := sessionRequest(t, handler, method, "/api/v1/me/asks/"+askID+"/snooze", map[string]any{
			"snoozed_until": future,
		})
		if response.Code != http.StatusForbidden {
			t.Fatalf("session %s snooze: status=%d body=%s, want 403", method, response.Code, response.Body.String())
		}
	}
	// A non-uuid id is refused on the un-snooze too, before it reaches a uuid column.
	if response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/me/asks/CORE-1/snooze", nil, "alice"); response.Code != http.StatusBadRequest {
		t.Fatalf("un-snooze a non-uuid ask: status=%d body=%s, want 400", response.Code, response.Body.String())
	}
}
