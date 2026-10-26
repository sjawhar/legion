package api

import (
	"context"
	"net/http"
	"testing"
)

// askReachRead is the claimant and route an ask read carries, at the single-ask read's top level
// and on every thread the Inbox and an issue's ask list hand back.
type askReachRead struct {
	Claimant *string `json:"claimant"`
	Route    *string `json:"route"`
}

func describeOptionalString(value *string) string {
	if value == nil {
		return "null"
	}
	return `"` + *value + `"`
}

func (reach askReachRead) matches(claimant, route *string) bool {
	return stringPointersEqual(reach.Claimant, claimant) && stringPointersEqual(reach.Route, route)
}

// A person's answer and replies on an issue's ask reach the session holding the issue's claim and
// the issue's route as well as the followers, so every ask read that renders whom an ask reaches
// carries both: the single-ask read, an Inbox row's thread and an issue ask list's thread. Only a
// session's claim names a claimant (a person has no Envoy topic), and a project document's ask has
// no issue, so neither.
func TestAskReadsCarryTheIssueClaimantAndRoute(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	ctx := context.Background()

	sessionClaimed := createInteractionIssue(t, handler, "TEST", "Transport rollout", "A spec").Key
	personClaimed := createAnswerListIssue(t, handler, "Billing export").Key
	unclaimed := createAnswerListIssue(t, handler, "Onboarding copy").Key
	issues := []struct {
		name         string
		key          string
		claimedBy    *string
		route        *string
		wantClaimant *string
	}{
		{
			name:         "session claim, session route",
			key:          sessionClaimed,
			claimedBy:    new(`{"kind":"session","id":"session-claimant"}`),
			route:        new("session:session-routed"),
			wantClaimant: new("session-claimant"),
		},
		{
			name:      "person claim, role route",
			key:       personClaimed,
			claimedBy: new(`{"kind":"user","id":"alice"}`),
			route:     new("role:reviewer"),
		},
		{name: "no claim, no route", key: unclaimed},
	}
	type expectation struct {
		name     string
		issueKey string
		claimant *string
		route    *string
	}
	expected := map[string]expectation{}
	for _, issue := range issues {
		askID := openAskAs(t, handler, issue.key, "session-asker", issue.name+"?")
		if _, err := database.Pool.Exec(ctx, `
			update issues set claimed_by = $2::jsonb,
				claimed_at = case when $2::jsonb is null then null else now() end, route = $3
			where key = $1
		`, issue.key, issue.claimedBy, issue.route); err != nil {
			t.Fatalf("set claim and route of %s: %v", issue.key, err)
		}
		expected[askID] = expectation{name: issue.name, issueKey: issue.key, claimant: issue.wantClaimant, route: issue.route}
	}

	artifact := createProjectDocument(t, handler, "TEST", "Design notes", "# Design notes\n")
	documentAsk := sessionRequest(t, handler, http.MethodPost, "/api/v1/artifacts/"+artifact.ID+"/asks", map[string]any{
		"question": "Document question", "actor": sessionActor(),
	})
	if documentAsk.Code != http.StatusCreated {
		t.Fatalf("create document ask: status=%d body=%s", documentAsk.Code, documentAsk.Body.String())
	}
	documentAskID := decodeBody[struct {
		ID string `json:"id"`
	}](t, documentAsk).ID
	expected[documentAskID] = expectation{name: "project document ask"}

	for askID, want := range expected {
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("read %s ask: status=%d body=%s", want.name, read.Code, read.Body.String())
		}
		if got := decodeBody[askReachRead](t, read); !got.matches(want.claimant, want.route) {
			t.Fatalf("%s: ask read claimant=%s route=%s, want claimant=%s route=%s", want.name,
				describeOptionalString(got.Claimant), describeOptionalString(got.Route),
				describeOptionalString(want.claimant), describeOptionalString(want.route))
		}
	}

	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	inboxRows := decodeBody[[]struct {
		ID     string       `json:"id"`
		Thread askReachRead `json:"thread"`
	}](t, inbox)
	seen := map[string]bool{}
	for _, row := range inboxRows {
		want, ok := expected[row.ID]
		if !ok {
			continue
		}
		seen[row.ID] = true
		if !row.Thread.matches(want.claimant, want.route) {
			t.Fatalf("%s: inbox thread claimant=%s route=%s, want claimant=%s route=%s", want.name,
				describeOptionalString(row.Thread.Claimant), describeOptionalString(row.Thread.Route),
				describeOptionalString(want.claimant), describeOptionalString(want.route))
		}
	}
	if len(seen) != len(expected) {
		t.Fatalf("inbox listed %d of the %d asks: %#v", len(seen), len(expected), inboxRows)
	}

	for askID, want := range expected {
		if want.issueKey == "" {
			continue
		}
		rows := listIssueAskRows(t, handler, want.issueKey, "?thread=1")
		if len(rows) != 1 || rows[0].ID != askID || rows[0].Thread == nil {
			t.Fatalf("%s: issue ask list = %#v, want its one ask with a thread", want.name, rows)
		}
		got := askReachRead{Claimant: rows[0].Thread.Claimant, Route: rows[0].Thread.Route}
		if !got.matches(want.claimant, want.route) {
			t.Fatalf("%s: issue ask list thread claimant=%s route=%s, want claimant=%s route=%s", want.name,
				describeOptionalString(got.Claimant), describeOptionalString(got.Route),
				describeOptionalString(want.claimant), describeOptionalString(want.route))
		}
	}
}
