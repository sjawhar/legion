package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// liveRegistry is the roster the Envoy listener answers from, which a test moves to start or
// end a session. The listener goroutine reads it while the test writes it, so every access
// goes through the mutex: without it `go test -race` reports the test itself, not the server.
type liveRegistry struct {
	mu  sync.Mutex
	ids []string
}

func (r *liveRegistry) set(ids ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = ids
}

// snapshot is the answer a listener gives at the moment a request reaches it.
func (r *liveRegistry) snapshot() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	sessions := make([]map[string]any, 0, len(r.ids))
	for _, id := range r.ids {
		sessions = append(sessions, map[string]any{"session_id": id, "title": "worker " + id})
	}
	return sessions
}

// claimListener is the Envoy listener every claim test runs against. A claim asks about the
// holder's own session, by id (GET /v1/registry/{session_id}), so that read is the lookup a
// test gates. The listener decides its answer from the roster as it stood when the lookup
// arrived — a real listener does, so a lookup a test holds open must never answer from a
// roster the test set after it was let go — and hands each lookup's ordinal to gate, which is
// where a test announces that lookup and blocks it. The live list is served too, ungated,
// since no claim reads it and the reads that do are nobody's lookup here.
func claimListener(t *testing.T, live *liveRegistry, gate func(lookup int)) *httptest.Server {
	t.Helper()
	var lookups atomic.Int32
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sessions := live.snapshot()
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/registry/"):
			sessionID := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
			if gate != nil {
				gate(int(lookups.Add(1)))
			}
			for _, session := range sessions {
				if session["session_id"] == sessionID {
					_ = json.NewEncoder(w).Encode(session)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no live session " + sessionID})
		case r.URL.Path == "/v1/sessions":
			_ = json.NewEncoder(w).Encode(sessions)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(listener.Close)
	return listener
}

// claimHandler is a server whose Envoy listener lists exactly the sessions in live and never
// holds a lookup open, with the database behind it for a test that reaches past the routes.
func claimHandler(t *testing.T, live *liveRegistry) (http.Handler, *store.Store) {
	t.Helper()
	handler, database, _ := newTestServer(t, testServerOptions{envoyURL: claimListener(t, live, nil).URL})
	return handler, database
}

// awaitLookup waits for the listener lookup a gated test is expecting.
func awaitLookup(t *testing.T, lookups <-chan int, want int) {
	t.Helper()
	select {
	case got := <-lookups:
		if got != want {
			t.Fatalf("listener lookup %d, want lookup %d", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the request never took listener lookup %d", want)
	}
}

// claimIssueKey seeds a project and one issue at the given status.
func claimIssueKey(t *testing.T, handler http.Handler, status string) string {
	t.Helper()
	project := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": "TEST", "name": "Test project",
	}, "alice")
	if project.Code != http.StatusCreated && project.Code != http.StatusConflict {
		t.Fatalf("create project: status=%d body=%s", project.Code, project.Body.String())
	}
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", map[string]string{
		"project": "TEST", "title": "Claimable work",
	}, "alice")
	if created.Code != http.StatusCreated {
		t.Fatalf("create issue: status=%d body=%s", created.Code, created.Body.String())
	}
	key := decodeBody[struct {
		Key string `json:"key"`
	}](t, created).Key
	moved := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]string{
		"status": status,
	}, "alice")
	if moved.Code != http.StatusOK {
		t.Fatalf("move issue to %s: status=%d body=%s", status, moved.Code, moved.Body.String())
	}
	return key
}

// claimedIssue is the part of a claim answer a claim test asserts on: the issue, flat, with the
// one field both routes add beside it.
type claimedIssue struct {
	Status string `json:"status"`
	Claim  *struct {
		Actor struct {
			Kind   string `json:"kind"`
			ID     string `json:"id"`
			Origin *struct {
				SessionTitle string `json:"session_title"`
			} `json:"origin"`
		} `json:"actor"`
		At string `json:"at"`
	} `json:"claim"`
	// A pointer: the answer omits the field when the walk failed (claimAnswer, issue_claim.go),
	// and a test has to tell that apart from a stored false.
	LegionManagedStatus *bool `json:"legion_managed_status,omitempty"`
}

func claimActorBody(id, title string) map[string]any {
	return map[string]any{
		"kind":   "session",
		"id":     id,
		"origin": map[string]any{"session_title": title},
	}
}

// claimEventRow is one claim or release entry of an issue's event log.
type claimEventRow struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload"`
}

// claimEvents returns the issue's claim and release events, oldest first.
func claimEvents(t *testing.T, handler http.Handler, key string) []claimEventRow {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key+"/events", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", response.Code, response.Body.String())
	}
	all := decodeBody[[]claimEventRow](t, response)
	claims := all[:0]
	for _, event := range all {
		if event.Type == "issue.claimed" || event.Type == "issue.released" {
			claims = append(claims, event)
		}
	}
	return claims
}

func TestClaimRefusesALiveHolderAndPassesOnWhenThatSessionEnds(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one", "session-two")
	handler, _ := claimHandler(t, live)
	key := claimIssueKey(t, handler, "todo")

	first := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first claim: status=%d body=%s", first.Code, first.Body.String())
	}
	claimed := decodeBody[claimedIssue](t, first)
	if claimed.Claim == nil || claimed.Claim.Actor.ID != "session-one" {
		t.Fatalf("first claim holder: %+v", claimed)
	}

	contested := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-two", "Second agent"),
	})
	if contested.Code != http.StatusConflict {
		t.Fatalf("contested claim: status=%d body=%s", contested.Code, contested.Body.String())
	}
	conflict := decodeBody[struct {
		Code  string            `json:"code"`
		Error string            `json:"error"`
		Claim *model.IssueClaim `json:"claim"`
	}](t, contested)
	if conflict.Code != "ISSUE_CLAIMED" {
		t.Fatalf("contested claim code: %+v", conflict)
	}
	// The listener reports "worker session-one" for that session while the claim stamped
	// "Implementer": the refusal must name the title the holder is running under now, which is
	// what the refused caller needs to find it, and never the stale stamped one.
	// The body carries the claim itself, so a client renders who holds the issue without a
	// second read.
	if conflict.Claim == nil || conflict.Claim.Actor.ID != "session-one" || conflict.Claim.At.IsZero() {
		t.Fatalf("the refusal must carry the claim: %+v", conflict.Claim)
	}
	if !strings.Contains(conflict.Error, "session-one") || !strings.Contains(conflict.Error, "worker session-one") {
		t.Fatalf("the refusal must name the live holder and its live title: %q", conflict.Error)
	}
	if strings.Contains(conflict.Error, "Implementer") {
		t.Fatalf("the refusal must not name the stamped title: %q", conflict.Error)
	}

	// The holder's session ends: the listener no longer lists it, so any agent may take it.
	live.set("session-two")
	takeover := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-two", "Second agent"),
	})
	if takeover.Code != http.StatusOK {
		t.Fatalf("takeover: status=%d body=%s", takeover.Code, takeover.Body.String())
	}
	took := decodeBody[claimedIssue](t, takeover)
	if took.Claim == nil || took.Claim.Actor.ID != "session-two" {
		t.Fatalf("takeover holder: %+v", took)
	}
	events := claimEvents(t, handler, key)
	if len(events) != 2 {
		t.Fatalf("claim events: %+v", events)
	}
	last := events[1]
	if last.Type != "issue.claimed" || last.Payload["reason"] != "takeover" {
		t.Fatalf("takeover event: %+v", last)
	}
	previous, ok := last.Payload["previous_claim"].(map[string]any)
	if !ok {
		t.Fatalf("takeover event must carry the previous claim: %+v", last)
	}
	actor, _ := previous["actor"].(map[string]any)
	if actor["id"] != "session-one" {
		t.Fatalf("previous claimant: %+v", previous)
	}
}

// A refusal says only what its caller can act on. A human's claim had no liveness to check and
// has no session to message, so it names the person; and only POST /claim takes {force: true},
// so a release refusal never points at a force that route does not have.
func TestClaimRefusalNamesTheHolderAndOnlyWhatTheRouteOffers(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one")
	lookups := make(chan int, 8)
	listener := claimListener(t, live, func(lookup int) { lookups <- lookup })
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	key := claimIssueKey(t, handler, "todo")
	if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{}, "alice"); response.Code != http.StatusOK {
		t.Fatalf("human claim: status=%d body=%s", response.Code, response.Body.String())
	}
	refusal := func(method string) string {
		t.Helper()
		response := bearerRequest(t, handler, method, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody("session-one", "Implementer"),
		})
		if response.Code != http.StatusConflict {
			t.Fatalf("%s over a human's claim: status=%d body=%s", method, response.Code, response.Body.String())
		}
		body := decodeBody[struct {
			Code  string `json:"code"`
			Error string `json:"error"`
		}](t, response)
		if body.Code != "ISSUE_CLAIMED" {
			t.Fatalf("%s code = %q, want ISSUE_CLAIMED: %s", method, body.Code, body.Error)
		}
		return body.Error
	}

	claiming := refusal(http.MethodPost)
	if !strings.Contains(claiming, "alice") {
		t.Fatalf("a human holder is named: %q", claiming)
	}
	for _, session := range []string{"is still running", "ask that session"} {
		if strings.Contains(claiming, session) {
			t.Fatalf("a human holder has no session running and none to message: %q", claiming)
		}
	}
	if !strings.Contains(claiming, "force the claim") {
		t.Fatalf("POST /claim takes {force: true}, so its refusal says so: %q", claiming)
	}

	releasing := refusal(http.MethodDelete)
	if !strings.Contains(releasing, "alice") {
		t.Fatalf("a human holder is named on the release route too: %q", releasing)
	}
	if strings.Contains(releasing, "force") {
		t.Fatalf("DELETE /claim has no force to offer: %q", releasing)
	}
	if taken := len(lookups); taken != 0 {
		t.Fatalf("a human holder has no liveness to look up, took %d lookups", taken)
	}
}

func TestClaimRecordsTheRequestsOwnActor(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one")
	handler, _ := claimHandler(t, live)
	key := claimIssueKey(t, handler, "todo")

	// A human's claim is the human's, whatever session the body names: a cookie caller's
	// identity comes from the cookie, never the body.
	human := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Someone else"),
	}, "alice")
	if human.Code != http.StatusOK {
		t.Fatalf("human claim: status=%d body=%s", human.Code, human.Body.String())
	}
	held := decodeBody[claimedIssue](t, human)
	if held.Claim == nil || held.Claim.Actor.Kind != "user" || held.Claim.Actor.ID != "alice" {
		t.Fatalf("a human claim must record the human: %+v", held.Claim)
	}

	// The request has no parameter for claiming on another session's behalf: an invented one
	// is refused rather than quietly ignored. A bearer still declares its own session in
	// `actor`, as on every write — `dispatch claim` fills that from the host runtime.
	onBehalf := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor":      claimActorBody("session-one", "Implementer"),
		"session_id": "session-two",
	})
	if onBehalf.Code != http.StatusBadRequest {
		t.Fatalf("claiming for another session: status=%d body=%s", onBehalf.Code, onBehalf.Body.String())
	}

	release := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", nil, "alice")
	if release.Code != http.StatusOK {
		t.Fatalf("human release: status=%d body=%s", release.Code, release.Body.String())
	}
	session := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if session.Code != http.StatusOK {
		t.Fatalf("session claim: status=%d body=%s", session.Code, session.Body.String())
	}
	took := decodeBody[claimedIssue](t, session)
	if took.Claim == nil || took.Claim.Actor.Kind != "session" || took.Claim.Actor.ID != "session-one" {
		t.Fatalf("a bearer claim must record the requesting session: %+v", took.Claim)
	}
	if took.Claim.Actor.Origin == nil || took.Claim.Actor.Origin.SessionTitle != "Implementer" {
		t.Fatalf("the claim keeps the session's title for the surfaces that name it: %+v", took.Claim)
	}
}

// The claim and the status are separate records: one names the session implementing the issue,
// the other is how humans track where work has got to.
func TestClaimAndStatusMoveIndependently(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one")
	handler, _ := claimHandler(t, live)
	key := claimIssueKey(t, handler, "todo")

	claimed := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if claimed.Code != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", claimed.Code, claimed.Body.String())
	}
	if got := decodeBody[claimedIssue](t, claimed); got.Status != "todo" {
		t.Fatalf("claiming must not move the status: got %q, want todo", got.Status)
	}

	released := bearerRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if released.Code != http.StatusOK {
		t.Fatalf("release: status=%d body=%s", released.Code, released.Body.String())
	}
	moved := bearerRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]any{
		"status": "in_progress",
		"actor":  claimActorBody("session-one", "Implementer"),
	})
	if moved.Code != http.StatusOK {
		t.Fatalf("move to in_progress: status=%d body=%s", moved.Code, moved.Body.String())
	}
	after := decodeBody[claimedIssue](t, moved)
	if after.Status != "in_progress" {
		t.Fatalf("status after the move: %q", after.Status)
	}
	if after.Claim != nil {
		t.Fatalf("a status move must not claim the issue: %+v", after.Claim)
	}
}

// labelledIssueKey seeds one issue of project (created if it is not there yet) with the given
// labels under the given parent (empty for none), for the tests over who owns an issue's status:
// both the claim routes' and the write advice's.
func labelledIssueKey(t *testing.T, handler http.Handler, project, title string, labels []string, parent string) string {
	t.Helper()
	created := dispatchRequest(t, handler, http.MethodPost, "/api/v1/projects", map[string]string{
		"key": project, "name": project + " project",
	}, "alice")
	if created.Code != http.StatusCreated && created.Code != http.StatusConflict {
		t.Fatalf("create project %s: status=%d body=%s", project, created.Code, created.Body.String())
	}
	body := map[string]any{"project": project, "title": title, "force": true}
	if labels != nil {
		body["labels"] = labels
	}
	if parent != "" {
		body["parent"] = parent
	}
	issue := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues", body, "alice")
	if issue.Code != http.StatusCreated {
		t.Fatalf("create issue %s: status=%d body=%s", title, issue.Code, issue.Body.String())
	}
	return decodeBody[struct {
		Key string `json:"key"`
	}](t, issue).Key
}

// TestClaimAnswersWhoOwnsTheIssuesStatus covers the answer a session acts on: on an issue Legion
// runs only its daemon writes the status, so both routes have to say so — on the issue's own
// label whatever case it was typed in, and on a child that carries no label of its own.
func TestClaimAnswersWhoOwnsTheIssuesStatus(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one")
	handler, _ := claimHandler(t, live)

	lowercase := labelledIssueKey(t, handler, "TEST", "Rotate the staging certificates", []string{"legion"}, "")
	uppercase := labelledIssueKey(t, handler, "TEST", "Back up the metrics volume", []string{"Legion"}, "")
	parent := labelledIssueKey(t, handler, "TEST", "Split the billing worker", []string{"legion"}, "")
	child := labelledIssueKey(t, handler, "TEST", "Draft an invoice renderer", nil, parent)
	grandchild := labelledIssueKey(t, handler, "TEST", "Number the invoices", nil, child)
	thirdCase := labelledIssueKey(t, handler, "TEST", "Drain the delivery queue", []string{"LEGION"}, "")
	plain := labelledIssueKey(t, handler, "TEST", "Tidy the onboarding checklist", []string{"infra"}, "")

	for _, want := range []struct {
		name    string
		key     string
		managed bool
	}{
		{"the legion label", lowercase, true},
		{"the label as Legion", uppercase, true},
		{"a child under a labelled parent", child, true},
		// Two links below the label: the answer is the recursive walk's, not a parent lookup's.
		{"a grandchild under a labelled root", grandchild, true},
		// A third case: the match is on lower(label), not on the two spellings above.
		{"the label as LEGION", thirdCase, true},
		{"no such label on it or above it", plain, false},
	} {
		t.Run(want.name, func(t *testing.T) {
			claimed := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+want.key+"/claim", map[string]any{
				"actor": claimActorBody("session-one", "Implementer"),
			})
			if claimed.Code != http.StatusOK {
				t.Fatalf("claim: status=%d body=%s", claimed.Code, claimed.Body.String())
			}
			answer := decodeBody[claimedIssue](t, claimed)
			if answer.LegionManagedStatus == nil || *answer.LegionManagedStatus != want.managed {
				t.Fatalf("claim legion_managed_status=%v, want %v", answer.LegionManagedStatus, want.managed)
			}
			// The issue is still the body's own shape: the dashboard merges it into its cache.
			if answer.Claim == nil {
				t.Fatalf("the claim answer must still carry the issue it wrote: %s", claimed.Body.String())
			}

			released := bearerRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+want.key+"/claim", map[string]any{
				"actor": claimActorBody("session-one", "Implementer"),
			})
			if released.Code != http.StatusOK {
				t.Fatalf("release: status=%d body=%s", released.Code, released.Body.String())
			}
			if got := decodeBody[claimedIssue](t, released).LegionManagedStatus; got == nil || *got != want.managed {
				t.Fatalf("release legion_managed_status=%v, want %v", got, want.managed)
			}
			// A release of an issue nobody holds writes nothing and answers the issue as it
			// stands: that short-circuit has to answer who owns the status just the same.
			again := bearerRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+want.key+"/claim", map[string]any{
				"actor": claimActorBody("session-one", "Implementer"),
			})
			if again.Code != http.StatusOK {
				t.Fatalf("release an unclaimed issue: status=%d body=%s", again.Code, again.Body.String())
			}
			if got := decodeBody[claimedIssue](t, again).LegionManagedStatus; got == nil || *got != want.managed {
				t.Fatalf("second release legion_managed_status=%v, want %v", got, want.managed)
			}
		})
	}
}

// TestClaimSurvivesAFailingLegionLabelWalk: the walk that answers who owns the status runs inside
// the claim's own transaction, so its failure once rolled back a claim that had otherwise
// succeeded. A claim is what stops two sessions working one issue; the advisory answer is not.
// The failure is forced by pointing legionManagedStatusSQL at a statement Postgres refuses.
func TestClaimSurvivesAFailingLegionLabelWalk(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one")
	handler, _ := claimHandler(t, live)
	key := claimIssueKey(t, handler, "triage")

	// Swapping a package-level statement is safe only because no test in internal/dispatch/api
	// calls t.Parallel, so no other test's walk can read the broken one before the cleanup puts
	// it back; a test added here that does call t.Parallel breaks this one. The package's
	// purpose-built seam, server.adviceQueryHook, is not used instead because it fires from
	// computeWriteAdvice (write_advice.go) and never reaches claimWrite, and a hook that exists
	// only for this test has no business in the claim path.
	healthy := legionManagedStatusSQL
	legionManagedStatusSQL = `select from no_such_relation_for_the_legion_walk`
	t.Cleanup(func() { legionManagedStatusSQL = healthy })

	claimed := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if claimed.Code != http.StatusOK {
		t.Fatalf("claim with a failing label walk: status=%d body=%s", claimed.Code, claimed.Body.String())
	}
	if answer := decodeBody[claimedIssue](t, claimed); answer.Claim == nil || answer.Claim.Actor.ID != "session-one" {
		t.Fatalf("the claim was discarded by the failed walk: %s", claimed.Body.String())
	}
	if _, answered := rawClaimFields(t, claimed)["legion_managed_status"]; answered {
		t.Fatalf("a failed walk must omit legion_managed_status rather than answer it: %s", claimed.Body.String())
	}
	// The claim is the issue's, not the response's: a reader sees the same holder.
	read := bearerRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("read the issue back: status=%d body=%s", read.Code, read.Body.String())
	}
	if issue := decodeBody[claimedIssue](t, read); issue.Claim == nil || issue.Claim.Actor.ID != "session-one" {
		t.Fatalf("the issue holds no claim after a failed walk: %s", read.Body.String())
	}
}

// TestClaimSurvivesAHangingLegionLabelWalk: the walk used to run unbounded, on the request's own
// context, between the claim's event and its COMMIT. A walk that hung there held the issue's row
// lock for as long as it hung, and a cancellation landing in that window - a client hang-up, a
// write deadline - stopped the savepoint's rollback, failed the COMMIT and discarded a claim that
// had otherwise succeeded. Two changes close both: the walk now runs before any of the claim's
// writes, so nothing at all sits between appendEvent and COMMIT, and it runs detached from the
// request (context.WithoutCancel) inside a savepoint bounded at 500 ms, as the advice path bounds
// the same predicate (issue_claim.go, write_advice.go).
//
// The statement below stands in for a walk that hangs, as the test above stands in for one
// Postgres refuses, under the same package-level swap and the same no-t.Parallel constraint. It
// sleeps ten times the bound, so what ends it is the bound; nothing in this test races a Go
// timer against the database, and nothing asserts on elapsed time.
func TestClaimSurvivesAHangingLegionLabelWalk(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one")
	handler, _ := claimHandler(t, live)
	key := claimIssueKey(t, handler, "triage")

	healthy := legionManagedStatusSQL
	legionManagedStatusSQL = `select count(*) > 0 from (select pg_sleep(5)) slow where $1 <> ''`
	t.Cleanup(func() { legionManagedStatusSQL = healthy })

	claimed := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if claimed.Code != http.StatusOK {
		t.Fatalf("claim under a hanging walk: status=%d body=%s", claimed.Code, claimed.Body.String())
	}
	if answer := decodeBody[claimedIssue](t, claimed); answer.Claim == nil || answer.Claim.Actor.ID != "session-one" {
		t.Fatalf("the hanging walk cost the claim: %s", claimed.Body.String())
	}
	if _, answered := rawClaimFields(t, claimed)["legion_managed_status"]; answered {
		t.Fatalf("a timed-out walk must omit legion_managed_status rather than answer it: %s", claimed.Body.String())
	}
	// The claim is the issue's, not the response's, and the row the walk held is free: this read
	// and the release that follows are served rather than blocked behind it.
	read := bearerRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil)
	if read.Code != http.StatusOK {
		t.Fatalf("read the issue back: status=%d body=%s", read.Code, read.Body.String())
	}
	if issue := decodeBody[claimedIssue](t, read); issue.Claim == nil || issue.Claim.Actor.ID != "session-one" {
		t.Fatalf("the issue holds no claim after a hanging walk: %s", read.Body.String())
	}
	legionManagedStatusSQL = healthy
	released := bearerRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if released.Code != http.StatusOK {
		t.Fatalf("release after a hanging walk: status=%d body=%s", released.Code, released.Body.String())
	}
}

// rawClaimFields reads a claim answer as raw JSON, so a test can tell a field the answer omitted
// from one that came back false.
func rawClaimFields(t *testing.T, response *httptest.ResponseRecorder) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode claim answer: %v body=%s", err, response.Body.String())
	}
	return body
}

// releasedHolder is the session named by the newest issue.released event: the contract makes
// previous_claim required on a release (IssueReleasedEventPayload) and the dashboard reads it
// without a guard, so every release shape has to carry it.
func releasedHolder(t *testing.T, handler http.Handler, key string) string {
	t.Helper()
	events := claimEvents(t, handler, key)
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != "issue.released" {
			continue
		}
		previous, ok := events[index].Payload["previous_claim"].(map[string]any)
		if !ok {
			t.Fatalf("a release must name the claim it cleared: %+v", events[index].Payload)
		}
		actor, ok := previous["actor"].(map[string]any)
		if !ok {
			t.Fatalf("a release's previous claim must carry its actor: %+v", previous)
		}
		id, _ := actor["id"].(string)
		return id
	}
	t.Fatalf("no issue.released event on %s", key)
	return ""
}

func TestClaimReleaseBelongsToItsHolderAHumanOrAnEndedSession(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one", "session-two")
	handler, _ := claimHandler(t, live)
	key := claimIssueKey(t, handler, "todo")
	claim := func(id string) *httptest.ResponseRecorder {
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody(id, "worker "+id),
		})
	}
	release := func(id string) *httptest.ResponseRecorder {
		return bearerRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody(id, "worker "+id),
		})
	}

	if response := claim("session-one"); response.Code != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := release("session-two"); response.Code != http.StatusConflict {
		t.Fatalf("another live session must not release the claim: status=%d body=%s", response.Code, response.Body.String())
	}
	if response := release("session-one"); response.Code != http.StatusOK {
		t.Fatalf("the holder releases its own claim: status=%d body=%s", response.Code, response.Body.String())
	}
	if holder := releasedHolder(t, handler, key); holder != "session-one" {
		t.Fatalf("the holder's own release names its claim: got %q", holder)
	}

	if response := claim("session-one"); response.Code != http.StatusOK {
		t.Fatalf("re-claim: status=%d body=%s", response.Code, response.Body.String())
	}
	humanRelease := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", nil, "alice")
	if humanRelease.Code != http.StatusOK {
		t.Fatalf("a human releases any claim: status=%d body=%s", humanRelease.Code, humanRelease.Body.String())
	}
	if got := decodeBody[claimedIssue](t, humanRelease); got.Claim != nil {
		t.Fatalf("claim after release: %+v", got.Claim)
	}
	if holder := releasedHolder(t, handler, key); holder != "session-one" {
		t.Fatalf("a human's release names whose claim it cleared: got %q", holder)
	}

	if response := claim("session-one"); response.Code != http.StatusOK {
		t.Fatalf("claim before the session ends: status=%d body=%s", response.Code, response.Body.String())
	}
	live.set("session-two")
	if response := release("session-two"); response.Code != http.StatusOK {
		t.Fatalf("an ended session's claim may be cleared by anyone: status=%d body=%s", response.Code, response.Body.String())
	}
	if holder := releasedHolder(t, handler, key); holder != "session-one" {
		t.Fatalf("clearing an ended session's claim names that session: got %q", holder)
	}
}

func TestClosingAnIssueReleasesItsClaimAndAClosedIssueTakesNone(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one")
	handler, _ := claimHandler(t, live)
	key := claimIssueKey(t, handler, "todo")
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", response.Code, response.Body.String())
	}

	closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+key, map[string]string{
		"status": "done",
	}, "alice")
	if closed.Code != http.StatusOK {
		t.Fatalf("close: status=%d body=%s", closed.Code, closed.Body.String())
	}
	if got := decodeBody[claimedIssue](t, closed); got.Claim != nil {
		t.Fatalf("closing an issue releases its claim: %+v", got.Claim)
	}
	events := claimEvents(t, handler, key)
	if len(events) != 2 || events[1].Type != "issue.released" || events[1].Payload["reason"] != "closed" {
		t.Fatalf("close must record the release: %+v", events)
	}
	previous, _ := events[1].Payload["previous_claim"].(map[string]any)
	actor, _ := previous["actor"].(map[string]any)
	if actor["id"] != "session-one" {
		t.Fatalf("the release names whose claim it was: %+v", events[1].Payload)
	}

	// Releasing a closed issue stays the 200 no-op the docs and the skill promise: an agent
	// that stops working never has to know whether the close landed first.
	released := bearerRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if released.Code != http.StatusOK {
		t.Fatalf("release on a closed issue: status=%d body=%s", released.Code, released.Body.String())
	}
	if got := decodeBody[claimedIssue](t, released); got.Claim != nil {
		t.Fatalf("a closed issue has no claim to release: %+v", got.Claim)
	}

	reclaim := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	})
	if reclaim.Code != http.StatusConflict {
		t.Fatalf("claiming a closed issue: status=%d body=%s", reclaim.Code, reclaim.Body.String())
	}
	if code := decodeBody[struct {
		Code string `json:"code"`
	}](t, reclaim).Code; code != "ISSUE_CLOSED" {
		t.Fatalf("claiming a closed issue code: %q", code)
	}
}

func TestClaimIsOnEveryIssueRead(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one")
	handler, _ := claimHandler(t, live)
	key := claimIssueKey(t, handler, "todo")
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", response.Code, response.Body.String())
	}

	detail := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if detail.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", detail.Code, detail.Body.String())
	}
	if got := decodeBody[claimedIssue](t, detail); got.Claim == nil || got.Claim.Actor.ID != "session-one" {
		t.Fatalf("the issue detail carries the claim: %+v", got.Claim)
	}

	list := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues?project=TEST", nil, "alice")
	if list.Code != http.StatusOK {
		t.Fatalf("list issues: status=%d body=%s", list.Code, list.Body.String())
	}
	rows := decodeBody[[]claimedIssue](t, list)
	if len(rows) != 1 || rows[0].Claim == nil || rows[0].Claim.Actor.ID != "session-one" {
		t.Fatalf("a list row carries the claim: %+v", rows)
	}
	if rows[0].Claim.Actor.Origin == nil || rows[0].Claim.Actor.Origin.SessionTitle != "Implementer" {
		t.Fatalf("a list row can label the holder: %+v", rows[0].Claim)
	}
}

// The liveness lookup runs before the row lock, so a holder can change in between. The write
// must notice under the lock and redo the cycle rather than apply a verdict about a session
// that no longer holds the issue.
func TestClaimRedoesTheCycleWhenTheHolderChangesBeforeTheLock(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one", "session-two", "session-three")
	release := make(chan struct{})
	lookups := make(chan int, 8)
	listener := claimListener(t, live, func(lookup int) {
		lookups <- lookup
		// Exactly the first lookup waits, until the test has moved the claim to another
		// session; every later lookup (the third session's, and the redo's) answers at once.
		if lookup == 1 {
			<-release
		}
	})
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	key := claimIssueKey(t, handler, "todo")

	first := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "First holder"),
	})
	if first.Code != http.StatusOK {
		t.Fatalf("first claim: status=%d body=%s", first.Code, first.Body.String())
	}

	// session-two contests it; its liveness lookup blocks inside the listener, before any lock.
	contested := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		contested <- bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody("session-two", "Second agent"),
		})
	}()
	awaitLookup(t, lookups, 1)

	// While it waits, the claim moves to a third session — so the snapshot in flight is about a
	// holder that no longer has the issue — and that third session then ends. Only a redo can
	// see that: the first snapshot still lists session-three as live.
	live.set("session-three")
	moved := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-three", "Third agent"),
	})
	if moved.Code != http.StatusOK {
		t.Fatalf("the third session takes the ended holder's claim: status=%d body=%s", moved.Code, moved.Body.String())
	}
	live.set()
	close(release)

	// The redo re-reads the holder and re-asks the listener: session-three has ended, so the
	// contested claim lands as a takeover. With one attempt (no redo) this is a 409 instead,
	// because the stale snapshot still has session-three live.
	response := <-contested
	if response.Code != http.StatusOK {
		t.Fatalf("the redone claim must take the ended holder's claim: status=%d body=%s", response.Code, response.Body.String())
	}
	took := decodeBody[claimedIssue](t, response)
	if took.Claim == nil || took.Claim.Actor.ID != "session-two" {
		t.Fatalf("the redone claim's holder: %+v", took.Claim)
	}
	events := claimEvents(t, handler, key)
	last := events[len(events)-1]
	previous, _ := last.Payload["previous_claim"].(map[string]any)
	actor, _ := previous["actor"].(map[string]any)
	if last.Payload["reason"] != "takeover" || actor["id"] != "session-three" {
		t.Fatalf("the redo records what it took over: %+v", last.Payload)
	}
	if holder := currentHolder(t, handler, key); holder != "session-two" {
		t.Fatalf("the row is the arbiter: holder = %q", holder)
	}
}

// The liveness snapshot speaks only for the holder it was taken about. If the claim moves to
// another live session in between, the locked decision must not read that session's absence
// from the stale list as "ended" and take a live agent's work without force.
func TestClaimNeverTakesALiveHoldersClaimFoundAfterTheSnapshot(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one", "session-two")
	release := make(chan struct{})
	lookups := make(chan int, 8)
	listener := claimListener(t, live, func(lookup int) {
		lookups <- lookup
		if lookup == 1 {
			<-release
		}
	})
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	key := claimIssueKey(t, handler, "todo")

	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "First holder"),
	}); response.Code != http.StatusOK {
		t.Fatalf("first claim: status=%d body=%s", response.Code, response.Body.String())
	}

	contested := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		contested <- bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody("session-two", "Second agent"),
		})
	}()
	awaitLookup(t, lookups, 1)

	// session-one lets it go and session-three, which is live but not in the snapshot the
	// contested claim is holding, takes it.
	if response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", nil, "alice"); response.Code != http.StatusOK {
		t.Fatalf("human release: status=%d body=%s", response.Code, response.Body.String())
	}
	live.set("session-two", "session-three")
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-three", "Third agent"),
	}); response.Code != http.StatusOK {
		t.Fatalf("the third session claims the free issue: status=%d body=%s", response.Code, response.Body.String())
	}
	close(release)

	response := <-contested
	if response.Code != http.StatusConflict {
		t.Fatalf("a live holder found after the snapshot must not lose its claim: status=%d body=%s", response.Code, response.Body.String())
	}
	if holder := currentHolder(t, handler, key); holder != "session-three" {
		t.Fatalf("the live holder keeps the claim: holder = %q", holder)
	}
}

// currentHolder reads who holds the issue now.
func currentHolder(t *testing.T, handler http.Handler, key string) string {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read issue: status=%d body=%s", response.Code, response.Body.String())
	}
	issue := decodeBody[claimedIssue](t, response)
	if issue.Claim == nil {
		return ""
	}
	return issue.Claim.Actor.ID
}

// The wording of the answer after the bounded cycle runs out, pinned at the writer rather than
// through the route (TestContendedClaimEndsTheCycleAtTheBound drives that): the code is
// CLAIM_CONTENDED and never ISSUE_CLAIMED, the wording claims nothing about liveness (nothing
// was checked), and the claim the row shows rides along.
func TestContendedClaimAnswerNamesNoLiveness(t *testing.T) {
	live := &liveRegistry{}
	listener := claimListener(t, live, nil)
	handler, _, deps := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	key := claimIssueKey(t, handler, "todo")
	if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
		"actor": claimActorBody("session-one", "Implementer"),
	}); response.Code != http.StatusOK {
		t.Fatalf("claim: status=%d body=%s", response.Code, response.Body.String())
	}

	server := directServer(deps)
	recorder := httptest.NewRecorder()
	server.answerContendedClaim(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/issues/"+key+"/claim", nil), key)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", recorder.Code, recorder.Body.String())
	}
	body := decodeBody[struct {
		Code  string            `json:"code"`
		Error string            `json:"error"`
		Claim *model.IssueClaim `json:"claim"`
	}](t, recorder)
	if body.Code != "CLAIM_CONTENDED" {
		t.Fatalf("code = %q, want CLAIM_CONTENDED", body.Code)
	}
	if strings.Contains(body.Error, "is still running") {
		t.Fatalf("this answer checked nobody's liveness: %q", body.Error)
	}
	if !strings.Contains(body.Error, "session-one") || !strings.Contains(body.Error, "nothing was applied") {
		t.Fatalf("the answer must name the holder it read and say nothing was applied: %q", body.Error)
	}
	if body.Claim == nil || body.Claim.Actor.ID != "session-one" {
		t.Fatalf("the answer carries the claim the row shows: %+v", body.Claim)
	}
}

// resolutionAttempts is spent where a caller can see it: a holder that changes before each of
// the two locks ends the request with CLAIM_CONTENDED naming the holder of the moment. One
// attempt would answer after the first change (naming a session that has since lost the
// issue), three would judge a third time and refuse for the live holder instead — so this
// pins the bound itself, through the route, with the holder moved by writes that take no
// lookup of their own (a human release and a claim of a free issue are both decided without
// one).
func TestContendedClaimEndsTheCycleAtTheBound(t *testing.T) {
	// One holder change per attempt the bound allows, each made while that attempt's lookup is
	// held open. A later lookup is never held: a cycle that judged a third time would answer
	// from it rather than hang.
	changes := []string{"session-two", "session-three"}
	live := &liveRegistry{}
	live.set("session-one", "session-two", "session-three", "session-nine")
	lookups := make(chan int, 8)
	gate := make(chan struct{})
	listener := claimListener(t, live, func(lookup int) {
		lookups <- lookup
		if lookup <= len(changes) {
			<-gate
		}
	})
	handler, _, _ := newTestServer(t, testServerOptions{envoyURL: listener.URL})
	key := claimIssueKey(t, handler, "todo")
	claim := func(id string) *httptest.ResponseRecorder {
		return bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody(id, "worker "+id),
		})
	}
	if response := claim("session-one"); response.Code != http.StatusOK {
		t.Fatalf("first claim: status=%d body=%s", response.Code, response.Body.String())
	}

	contested := make(chan *httptest.ResponseRecorder, 1)
	go func() { contested <- claim("session-nine") }()
	for attempt, next := range changes {
		awaitLookup(t, lookups, attempt+1)
		if response := dispatchRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", nil, "alice"); response.Code != http.StatusOK {
			t.Fatalf("human release: status=%d body=%s", response.Code, response.Body.String())
		}
		if response := claim(next); response.Code != http.StatusOK {
			t.Fatalf("%s takes the free issue: status=%d body=%s", next, response.Code, response.Body.String())
		}
		gate <- struct{}{}
	}

	response := <-contested
	if response.Code != http.StatusConflict {
		t.Fatalf("a claim whose holder changed at every attempt: status=%d body=%s", response.Code, response.Body.String())
	}
	body := decodeBody[struct {
		Code  string            `json:"code"`
		Error string            `json:"error"`
		Claim *model.IssueClaim `json:"claim"`
	}](t, response)
	if body.Code != "CLAIM_CONTENDED" {
		t.Fatalf("code = %q, want CLAIM_CONTENDED: %s", body.Code, body.Error)
	}
	if body.Claim == nil || body.Claim.Actor.ID != "session-three" {
		t.Fatalf("the answer names the holder of the moment: %+v", body.Claim)
	}
	if !strings.Contains(body.Error, "session-three") {
		t.Fatalf("the answer names the holder it read: %q", body.Error)
	}
	if extra := len(lookups); extra != 0 {
		t.Fatalf("the cycle took %d listener lookups past the bound of %d", extra, resolutionAttempts)
	}
	if holder := currentHolder(t, handler, key); holder != "session-three" {
		t.Fatalf("nothing was applied: holder = %q", holder)
	}
}

// The pre-lock read takes a liveness snapshot only when the taking rule needs one, so a
// session releasing its own claim, and a claim on an issue nobody holds, are judged against
// nobody. If the holder changes under the row lock, that judgement cannot speak for the
// session the row now shows: the write must redo its cycle rather than read the snapshot it
// never took as "nobody is live" and take a running session's claim.
func TestClaimJudgedAgainstNobodyNeverTakesALiveHoldersClaim(t *testing.T) {
	t.Run("a stale self-release", func(t *testing.T) {
		live := &liveRegistry{}
		live.set("session-one", "session-two")
		handler, database := claimHandler(t, live)
		key := claimIssueKey(t, handler, "todo")
		if response := bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
			"actor": claimActorBody("session-one", "First holder"),
		}); response.Code != http.StatusOK {
			t.Fatalf("first claim: status=%d body=%s", response.Code, response.Body.String())
		}
		answer := whileTheRowIsHeld(t, database, key, "session-two", func() *httptest.ResponseRecorder {
			return bearerRequest(t, handler, http.MethodDelete, "/api/v1/issues/"+key+"/claim", map[string]any{
				"actor": claimActorBody("session-one", "First holder"),
			})
		})
		assertLiveHolderKeptItsClaim(t, handler, key, "session-two", answer)
	})

	t.Run("a claim judged against nobody", func(t *testing.T) {
		live := &liveRegistry{}
		live.set("session-one", "session-two")
		handler, database := claimHandler(t, live)
		key := claimIssueKey(t, handler, "todo")
		answer := whileTheRowIsHeld(t, database, key, "session-two", func() *httptest.ResponseRecorder {
			return bearerRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{
				"actor": claimActorBody("session-one", "First holder"),
			})
		})
		assertLiveHolderKeptItsClaim(t, handler, key, "session-two", answer)
	})
}

// whileTheRowIsHeld answers request with the claim moved to holder strictly between the
// request's pre-lock read and its locked re-read. The test holds the row itself — the same
// `select ... for no key update` claimWrite takes — waits until the request is queued behind that
// lock, writes the new holder and commits, so the change lands inside the request with no
// production seam and no second server.
func whileTheRowIsHeld(
	t *testing.T, database *store.Store, key string, holder string,
	request func() *httptest.ResponseRecorder,
) *httptest.ResponseRecorder {
	t.Helper()
	ctx := context.Background()
	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `select key from issues where key = $1 for no key update`, key).Scan(new(string)); err != nil {
		t.Fatalf("hold the issue row: %v", err)
	}
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- request() }()
	awaitRowLockWaiter(t, database)
	claim := model.IssueClaim{
		Actor: model.Actor{
			Kind:   "session",
			ID:     holder,
			Origin: &model.ActorOrigin{SessionTitle: "worker " + holder},
		},
		At: time.Now().UTC(),
	}
	if err := writeIssueClaim(ctx, tx, key, &claim); err != nil {
		t.Fatalf("move the claim to %s: %v", holder, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the holder change: %v", err)
	}
	select {
	case response := <-answered:
		return response
	case <-time.After(10 * time.Second):
		t.Fatal("the request never answered once the row was free")
		return nil
	}
}

// A claim and a release begin their transaction before they take the issue's row, so a write that
// holds the row while they wait and stamps updated_at at its own statement - a PATCH does - stamps
// a time after their start. Neither moves updated_at back below that stamp, and each answers the
// value the row holds, which a read after it returns too.
func TestAClaimWriteThatWaitedNeverMovesUpdatedAtBackward(t *testing.T) {
	for _, write := range []struct {
		name   string
		method string
		body   any
	}{
		{name: "a claim", method: http.MethodPost, body: map[string]any{}},
		{name: "a release", method: http.MethodDelete},
	} {
		t.Run(write.name, func(t *testing.T) {
			live := &liveRegistry{}
			live.set("session-one")
			handler, database := claimHandler(t, live)
			key := claimIssueKey(t, handler, "todo")
			if write.method == http.MethodDelete {
				if response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/issues/"+key+"/claim", map[string]any{}, "alice"); response.Code != http.StatusOK {
					t.Fatalf("claim before the release: status=%d body=%s", response.Code, response.Body.String())
				}
			}
			ctx := context.Background()
			holder, err := database.Pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if err := holder.QueryRow(ctx, `select key from issues where key = $1 for no key update`, key).Scan(new(string)); err != nil {
				t.Fatalf("hold the issue row: %v", err)
			}
			answered := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				answered <- dispatchRequest(t, handler, write.method, "/api/v1/issues/"+key+"/claim", write.body, "alice")
			}()
			waitForDatabaseLocks(t, holder, 1)
			var stamped time.Time
			if err := holder.QueryRow(ctx, `
				update issues set updated_at = clock_timestamp() where key = $1 returning updated_at
			`, key).Scan(&stamped); err != nil {
				t.Fatalf("stamp updated_at under the row lock: %v", err)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatalf("commit the stamp: %v", err)
			}
			response := awaitResponse(t, answered)
			if response.Code != http.StatusOK {
				t.Fatalf("%s after the wait: status=%d body=%s", write.name, response.Code, response.Body.String())
			}
			var stored time.Time
			if err := database.Pool.QueryRow(ctx, `select updated_at from issues where key = $1`, key).Scan(&stored); err != nil {
				t.Fatalf("read the stored updated_at: %v", err)
			}
			if stored.Before(stamped) {
				t.Fatalf("%s moved updated_at back from %s, stamped during its wait, to %s", write.name, stamped, stored)
			}
			if got := decodeBody[model.Issue](t, response).UpdatedAt; !got.Equal(stored) {
				t.Fatalf("%s answered updated_at %s, the row holds %s", write.name, got, stored)
			}
			read := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+key, nil, "alice")
			if read.Code != http.StatusOK {
				t.Fatalf("read after %s: status=%d body=%s", write.name, read.Code, read.Body.String())
			}
			if got := decodeBody[model.Issue](t, read).UpdatedAt; !got.Equal(stored) {
				t.Fatalf("a read after %s answered updated_at %s, the row holds %s", write.name, got, stored)
			}
		})
	}
}

// The statement's moment is a floor, not the value: a row whose updated_at is already past this
// statement's clock keeps it through a PATCH, a claim and a release, as it does through an event
// (TestEventDoesNotMoveIssueUpdatedAtBackward).
func TestAnIssueWriteKeepsAnUpdatedAtAheadOfItsClock(t *testing.T) {
	live := &liveRegistry{}
	live.set("session-one")
	handler, database := claimHandler(t, live)
	key := claimIssueKey(t, handler, "todo")
	future := time.Now().UTC().Truncate(time.Microsecond).Add(time.Hour)
	if _, err := database.Pool.Exec(context.Background(), `update issues set updated_at = $2 where key = $1`, key, future); err != nil {
		t.Fatalf("set a future updated_at: %v", err)
	}
	for _, write := range []struct {
		name   string
		method string
		target string
		body   any
	}{
		{name: "a PATCH", method: http.MethodPatch, target: "/api/v1/issues/" + key, body: map[string]string{"title": "Retitled"}},
		{name: "a claim", method: http.MethodPost, target: "/api/v1/issues/" + key + "/claim", body: map[string]any{}},
		{name: "a release", method: http.MethodDelete, target: "/api/v1/issues/" + key + "/claim"},
	} {
		response := dispatchRequest(t, handler, write.method, write.target, write.body, "alice")
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status=%d body=%s", write.name, response.Code, response.Body.String())
		}
		if got := decodeBody[model.Issue](t, response).UpdatedAt; !got.Equal(future) {
			t.Fatalf("%s answered updated_at %s, want the later %s the row held", write.name, got, future)
		}
	}
}

// awaitRowLockWaiter waits until a backend of this test's own database is waiting on a lock:
// the request under test, stopped at the issue's row lock inside claimWrite. Scoped to
// current_database() because one Postgres serves every test in the package.
func awaitRowLockWaiter(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := database.Pool.QueryRow(context.Background(), `
			select count(*) from pg_stat_activity
			where datname = current_database() and wait_event_type = 'Lock' and pid <> pg_backend_pid()
		`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the request never reached the issue's row lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// assertLiveHolderKeptItsClaim is the answer a caller gets when the holder the row shows is a
// session the caller never judged: the refusal names that live holder, and the row still shows
// it.
func assertLiveHolderKeptItsClaim(
	t *testing.T, handler http.Handler, key string, holder string, answer *httptest.ResponseRecorder,
) {
	t.Helper()
	if answer.Code != http.StatusConflict {
		t.Fatalf("a live holder found under the lock keeps its claim: status=%d body=%s", answer.Code, answer.Body.String())
	}
	body := decodeBody[struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}](t, answer)
	if body.Code != "ISSUE_CLAIMED" {
		t.Fatalf("code = %q, want ISSUE_CLAIMED: %s", body.Code, body.Error)
	}
	if !strings.Contains(body.Error, holder) {
		t.Fatalf("the refusal must name the live holder: %q", body.Error)
	}
	if got := currentHolder(t, handler, key); got != holder {
		t.Fatalf("the live session keeps the claim: holder = %q", got)
	}
}
