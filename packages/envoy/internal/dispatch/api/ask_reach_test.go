package api

import (
	"context"
	"net/http"
	"testing"
)

// askReachRead is the claimant, route and route holder an ask read carries, at the single-ask
// read's top level and on every thread the Inbox and an issue's ask list hand back.
type askReachRead struct {
	Claimant    *string `json:"claimant"`
	Route       *string `json:"route"`
	RouteHolder *string `json:"route_holder"`
}

func describeOptionalString(value *string) string {
	if value == nil {
		return "null"
	}
	return `"` + *value + `"`
}

func (reach askReachRead) String() string {
	return "claimant=" + describeOptionalString(reach.Claimant) + " route=" + describeOptionalString(reach.Route) +
		" route_holder=" + describeOptionalString(reach.RouteHolder)
}

func (reach askReachRead) matches(want askReachRead) bool {
	return stringPointersEqual(reach.Claimant, want.Claimant) && stringPointersEqual(reach.Route, want.Route) &&
		stringPointersEqual(reach.RouteHolder, want.RouteHolder)
}

// A person's answer and replies on an issue's ask reach the session holding the issue's claim and
// the issue's route as well as the followers, so every ask read that renders whom an ask reaches
// carries both, and the live session the route reaches now, so a reader counts each session once:
// the single-ask read, an Inbox row's thread and an issue ask list's thread. Only a session's
// claim names a claimant (a person has no Envoy topic); the route holder is the routed session
// while the listener lists it, or the role's live holder, and null for a route nobody holds; a
// project document's ask has no issue, so none of the three.
func TestAskReadsCarryTheIssueClaimantAndRoute(t *testing.T) {
	roster := &fakeRoster{}
	roster.set("session-routed")
	roster.set("ses-reviewer", "reviewer")
	handler, database := routeStatusServer(t, roster)
	ctx := context.Background()

	sessionClaimed := createInteractionIssue(t, handler, "TEST", "Transport rollout", "A spec").Key
	personClaimed := createAnswerListIssue(t, handler, "Billing export").Key
	vacantRole := createAnswerListIssue(t, handler, "Release notes").Key
	unclaimed := createAnswerListIssue(t, handler, "Onboarding copy").Key
	issues := []struct {
		name      string
		key       string
		claimedBy *string
		want      askReachRead
	}{
		{
			name:      "session claim, live session route",
			key:       sessionClaimed,
			claimedBy: new(`{"kind":"session","id":"session-claimant"}`),
			want: askReachRead{
				Claimant: new("session-claimant"), Route: new("session:session-routed"), RouteHolder: new("session-routed"),
			},
		},
		{
			name:      "person claim, held role route",
			key:       personClaimed,
			claimedBy: new(`{"kind":"user","id":"alice"}`),
			want:      askReachRead{Route: new("role:reviewer"), RouteHolder: new("ses-reviewer")},
		},
		{name: "no claim, role route nobody holds", key: vacantRole, want: askReachRead{Route: new("role:sre")}},
		{name: "no claim, no route", key: unclaimed},
	}
	type expectation struct {
		name     string
		issueKey string
		want     askReachRead
	}
	expected := map[string]expectation{}
	for _, issue := range issues {
		askID := openAskAs(t, handler, issue.key, "session-asker", issue.name+"?")
		if _, err := database.Pool.Exec(ctx, `
			update issues set claimed_by = $2::jsonb,
				claimed_at = case when $2::jsonb is null then null else now() end, route = $3
			where key = $1
		`, issue.key, issue.claimedBy, issue.want.Route); err != nil {
			t.Fatalf("set claim and route of %s: %v", issue.key, err)
		}
		expected[askID] = expectation{name: issue.name, issueKey: issue.key, want: issue.want}
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
		before := roster.lookups.Load()
		read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/asks/"+askID, nil, "alice")
		if read.Code != http.StatusOK {
			t.Fatalf("read %s ask: status=%d body=%s", want.name, read.Code, read.Body.String())
		}
		if got := decodeBody[askReachRead](t, read); !got.matches(want.want) {
			t.Fatalf("%s: ask read %s, want %s", want.name, got, want.want)
		}
		// The listener is asked only when the ask's issue has a route, and then once.
		wantCalls := int32(0)
		if want.want.Route != nil {
			wantCalls = 1
		}
		if calls := roster.lookups.Load() - before; calls != wantCalls {
			t.Fatalf("%s: ask read made %d listener calls, want %d", want.name, calls, wantCalls)
		}
	}

	before := roster.lookups.Load()
	inbox := dispatchRequest(t, handler, http.MethodGet, "/api/v1/inbox", nil, "alice")
	if inbox.Code != http.StatusOK {
		t.Fatalf("read inbox: status=%d body=%s", inbox.Code, inbox.Body.String())
	}
	if calls := roster.lookups.Load() - before; calls != 1 {
		t.Fatalf("an inbox read made %d listener calls, want 1 for every routed ask on it", calls)
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
		if !row.Thread.matches(want.want) {
			t.Fatalf("%s: inbox thread %s, want %s", want.name, row.Thread, want.want)
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
		got := askReachRead{Claimant: rows[0].Thread.Claimant, Route: rows[0].Thread.Route, RouteHolder: rows[0].Thread.RouteHolder}
		if !got.matches(want.want) {
			t.Fatalf("%s: issue ask list thread %s, want %s", want.name, got, want.want)
		}
	}
}
