package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// postHandoff posts body as the session's handoff for phase of issueKey.
func postHandoff(t *testing.T, handler http.Handler, issueKey, phase, body string) *httptest.ResponseRecorder {
	t.Helper()
	return sessionRequest(t, handler, http.MethodPost, "/api/v1/issues/"+issueKey+"/handoffs/"+phase, map[string]any{
		"actor": sessionActor(), "body": body,
	})
}

func recordedHandoff(t *testing.T, handler http.Handler, issueKey, phase, body string) model.HandoffSummary {
	t.Helper()
	response := postHandoff(t, handler, issueKey, phase, body)
	if response.Code != http.StatusCreated {
		t.Fatalf("write %s handoff: status=%d body=%s", phase, response.Code, response.Body.String())
	}
	return decodeBody[model.HandoffSummary](t, response)
}

// sameHandoff reports whether two reads carry one record's metadata. created_at is compared as an
// instant, since each read decodes a time zone of its own.
func sameHandoff(a, b model.HandoffSummary) bool {
	return a.ID == b.ID && a.IssueKey == b.IssueKey && a.Phase == b.Phase && a.Version == b.Version &&
		a.Author.SameAs(b.Author) && a.CreatedAt.Equal(b.CreatedAt)
}

// handoffIDs is each record's id, in the order listed.
func handoffIDs(handoffs []model.HandoffSummary) []string {
	ids := make([]string, len(handoffs))
	for index, handoff := range handoffs {
		ids[index] = handoff.ID
	}
	return ids
}

// fortyKilobyteHandoff is a handoff body of over 40 KB holding what a jsonb column would not
// keep as written: indentation, a repeated key, escapes spelled as the writer chose (\u00e9, \/)
// beside the raw character, and the characters an encoder escapes for HTML.
func fortyKilobyteHandoff() string {
	var body strings.Builder
	body.WriteString("{\n  \"schemaVersion\": 1,\n  \"note\": \"a < b && c > d, \\u00e9 é \\/ \\\"quoted\\\"\",\n  \"proof\": [")
	for i := 0; body.Len() < 40<<10; i++ {
		if i > 0 {
			body.WriteString(",")
		}
		fmt.Fprintf(&body, "\n    {\"criterion\": \"criterion %d\",   \"evidence\": \"seen\"}", i)
	}
	body.WriteString("\n  ],\n  \"phase\": \"implement\", \"phase\": \"implement\"\n}\n")
	return body.String()
}

// A session's writes to one phase are versions 1 and 2, another phase counts from 1 on its own,
// and each write answers its metadata with no body, under the session that wrote it.
func TestHandoffWritesNumberVersionsPerIssueAndPhase(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Handoff versions", "spec")

	first := recordedHandoff(t, handler, issue.Key, "implement", `{"round":1}`)
	second := recordedHandoff(t, handler, issue.Key, "implement", `{"round":2}`)
	plan := recordedHandoff(t, handler, issue.Key, "plan", `{}`)
	if first.Version != 1 || second.Version != 2 || plan.Version != 1 {
		t.Fatalf("versions = %d, %d, %d (plan); want 1, 2, 1", first.Version, second.Version, plan.Version)
	}
	if first.IssueKey != issue.Key || first.Phase != "implement" || first.ID == second.ID {
		t.Fatalf("first = %#v, second = %#v", first, second)
	}
	if first.Author.Kind != "session" || first.Author.ID != sessionActor()["id"] {
		t.Fatalf("author = %#v, want the writing session", first.Author)
	}
	raw := postHandoff(t, handler, issue.Key, "implement", `{"round":3}`)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw.Body.Bytes(), &fields); err != nil {
		t.Fatalf("decode write answer: %v", err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if want := []string{"author", "created_at", "id", "issue_key", "phase", "version"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("write answer keys = %v, want %v", keys, want)
	}
}

// Writes racing on one issue and phase each take their own version: the issue's row lock orders
// them, so none collides on the unique version.
func TestConcurrentHandoffWritesTakeDistinctVersions(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Concurrent handoffs", "spec")
	const writers = 5
	responses := make(chan *httptest.ResponseRecorder, writers)
	var wait sync.WaitGroup
	for i := range writers {
		wait.Go(func() {
			responses <- postHandoff(t, handler, issue.Key, "test", fmt.Sprintf(`{"writer":%d}`, i))
		})
	}
	wait.Wait()
	close(responses)
	var got []int
	for response := range responses {
		if response.Code != http.StatusCreated {
			t.Fatalf("concurrent write: status=%d body=%s", response.Code, response.Body.String())
		}
		got = append(got, decodeBody[model.HandoffSummary](t, response).Version)
	}
	sort.Ints(got)
	if want := []int{1, 2, 3, 4, 5}; !reflect.DeepEqual(got, want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}
}

// A 40 KB body reads back byte for byte, from the phase's latest and by id alike.
func TestHandoffBodyReadsBackExactlyAsWritten(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Handoff round trip", "spec")
	body := fortyKilobyteHandoff()
	if len(body) < 40<<10 {
		t.Fatalf("fixture body is %d bytes, want at least 40 KB", len(body))
	}
	written := recordedHandoff(t, handler, issue.Key, "implement", body)

	for _, path := range []string{
		"/api/v1/issues/" + issue.Key + "/handoffs/implement/latest",
		"/api/v1/handoffs/" + written.ID,
	} {
		response := sessionRequest(t, handler, http.MethodGet, path, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("GET %s: status=%d body=%s", path, response.Code, response.Body.String())
		}
		read := decodeBody[model.Handoff](t, response)
		if read.Body != body {
			t.Fatalf("GET %s body differs from the bytes written (%d read, %d written)", path, len(read.Body), len(body))
		}
		if !sameHandoff(read.HandoffSummary, written) {
			t.Fatalf("GET %s metadata = %#v, want %#v", path, read.HandoffSummary, written)
		}
	}
}

// The body cap is contracts.DispatchHandoffBodyMax bytes: a body of exactly that many is
// recorded, one byte more is 413 CAP_EXCEEDED naming the cap, and so is a request far past it.
func TestHandoffBodyCapIsEnforcedInBytes(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Handoff cap", "spec")
	wrapper := len(`{"a":""}`)
	atCap := `{"a":"` + strings.Repeat("x", contracts.DispatchHandoffBodyMax-wrapper) + `"}`
	if recordedHandoff(t, handler, issue.Key, "review", atCap).Version != 1 {
		t.Fatal("a body at the cap was not version 1")
	}
	for name, body := range map[string]string{
		"one byte over":        `{"a":"` + strings.Repeat("x", contracts.DispatchHandoffBodyMax-wrapper+1) + `"}`,
		"past the request cap": `{"a":"` + strings.Repeat("<", 2*contracts.DispatchHandoffBodyMax) + `"}`,
	} {
		response := postHandoff(t, handler, issue.Key, "review", body)
		if response.Code != http.StatusRequestEntityTooLarge || responseCode(t, response) != "CAP_EXCEEDED" ||
			!strings.Contains(response.Body.String(), fmt.Sprint(contracts.DispatchHandoffBodyMax)) {
			t.Fatalf("%s: status=%d body=%.300s, want 413 CAP_EXCEEDED naming the cap", name, response.Code, response.Body.String())
		}
	}
	if latest := decodeBody[model.Handoff](t, sessionRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/handoffs/review/latest", nil)); latest.Version != 1 {
		t.Fatalf("a refused write was recorded: latest version %d", latest.Version)
	}
}

// What is not a handoff write is refused before anything is written: a body that is not one JSON
// object, a missing body, a phase that is not a slug, a bearer naming no session, and a person.
func TestHandoffWriteRefusals(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Handoff refusals", "spec")
	path := "/api/v1/issues/" + issue.Key + "/handoffs/implement"
	for _, body := range []string{"[1]", `"text"`, "null", "{not json", "{} {}", ""} {
		response := postHandoff(t, handler, issue.Key, "implement", body)
		if response.Code != http.StatusBadRequest || responseCode(t, response) != "HANDOFF_INPUT" {
			t.Fatalf("body %q: status=%d body=%s, want 400 HANDOFF_INPUT", body, response.Code, response.Body.String())
		}
	}
	for name, tc := range map[string]struct {
		response *httptest.ResponseRecorder
		status   int
		code     string
	}{
		"no body": {
			sessionRequest(t, handler, http.MethodPost, path, map[string]any{"actor": sessionActor()}),
			http.StatusBadRequest, "HANDOFF_INPUT",
		},
		"a phase that is not a slug": {
			postHandoff(t, handler, issue.Key, "Implement", `{}`), http.StatusBadRequest, "HANDOFF_INPUT",
		},
		"a bearer naming no session": {
			sessionRequest(t, handler, http.MethodPost, path, map[string]any{"body": `{}`}),
			http.StatusBadRequest, "ACTOR_KIND",
		},
		"a bearer naming a user": {
			sessionRequest(t, handler, http.MethodPost, path, map[string]any{"actor": map[string]any{"kind": "user", "id": "alice"}, "body": `{}`}),
			http.StatusBadRequest, "ACTOR_KIND",
		},
		"a person's cookie": {
			dispatchRequest(t, handler, http.MethodPost, path, map[string]any{"body": `{}`}, "alice"),
			http.StatusForbidden, "HANDOFF_FORBIDDEN",
		},
		"an issue that does not exist": {
			postHandoff(t, handler, "TEST-999", "implement", `{}`), http.StatusNotFound, "NOT_FOUND",
		},
	} {
		if tc.response.Code != tc.status || responseCode(t, tc.response) != tc.code {
			t.Fatalf("%s: status=%d body=%s, want %d %s", name, tc.response.Code, tc.response.Body.String(), tc.status, tc.code)
		}
	}
	list := decodeBody[[]model.HandoffSummary](t, sessionRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/handoffs", nil))
	if len(list) != 0 {
		t.Fatalf("refused writes recorded %v", list)
	}
}

// The list is every handoff's metadata, newest first and without bodies; latest answers a phase's
// newest, and a phase or id with none is 404 HANDOFF_NOT_FOUND.
func TestHandoffReadsListNewestFirstAndRefuseWhatIsAbsent(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Handoff reads", "spec")
	planned := recordedHandoff(t, handler, issue.Key, "plan", `{"step":"plan"}`)
	implemented := recordedHandoff(t, handler, issue.Key, "implement", `{"step":"implement"}`)
	again := recordedHandoff(t, handler, issue.Key, "implement", `{"step":"implement again"}`)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/handoffs", nil, "alice")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"body"`) {
		t.Fatalf("list: status=%d body=%s, want 200 with no bodies", response.Code, response.Body.String())
	}
	list := decodeBody[[]model.HandoffSummary](t, response)
	if !reflect.DeepEqual(handoffIDs(list), []string{again.ID, implemented.ID, planned.ID}) || !sameHandoff(list[0], again) {
		t.Fatalf("list = %#v, want newest first", list)
	}
	latest := decodeBody[model.Handoff](t, sessionRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/handoffs/implement/latest", nil))
	if !sameHandoff(latest.HandoffSummary, again) || latest.Body != `{"step":"implement again"}` {
		t.Fatalf("latest = %#v, want version 2", latest)
	}

	for name, tc := range map[string]struct {
		path   string
		status int
		code   string
	}{
		"a phase with no handoff":  {"/api/v1/issues/" + issue.Key + "/handoffs/review/latest", http.StatusNotFound, "HANDOFF_NOT_FOUND"},
		"an issue with no handoff": {"/api/v1/issues/TEST-999/handoffs/plan/latest", http.StatusNotFound, "HANDOFF_NOT_FOUND"},
		"an id with no handoff":    {"/api/v1/handoffs/00000000-0000-0000-0000-000000000001", http.StatusNotFound, "HANDOFF_NOT_FOUND"},
		"an id that is not a uuid": {"/api/v1/handoffs/not-a-uuid", http.StatusBadRequest, "HANDOFF_ID_INPUT"},
		"a phase that is not slug": {"/api/v1/issues/" + issue.Key + "/handoffs/Plan/latest", http.StatusBadRequest, "HANDOFF_INPUT"},
	} {
		response := sessionRequest(t, handler, http.MethodGet, tc.path, nil)
		if response.Code != tc.status || responseCode(t, response) != tc.code {
			t.Fatalf("%s: status=%d body=%s, want %d %s", name, response.Code, response.Body.String(), tc.status, tc.code)
		}
	}
}

// A closed issue refuses a handoff write with 409 ISSUE_CLOSED, and its handoffs still read.
func TestClosedIssueRefusesHandoffWritesAndKeepsItsReads(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Closed handoffs", "spec")
	written := recordedHandoff(t, handler, issue.Key, "review", `{"verdict":"approve"}`)
	if closed := dispatchRequest(t, handler, http.MethodPatch, "/api/v1/issues/"+issue.Key, map[string]string{"status": "done"}, "alice"); closed.Code != http.StatusOK {
		t.Fatalf("close issue: status=%d body=%s", closed.Code, closed.Body.String())
	}

	refused := postHandoff(t, handler, issue.Key, "review", `{"verdict":"again"}`)
	if refused.Code != http.StatusConflict || responseCode(t, refused) != "ISSUE_CLOSED" {
		t.Fatalf("write on a closed issue: status=%d body=%s, want 409 ISSUE_CLOSED", refused.Code, refused.Body.String())
	}
	for _, path := range []string{
		"/api/v1/issues/" + issue.Key + "/handoffs",
		"/api/v1/issues/" + issue.Key + "/handoffs/review/latest",
		"/api/v1/handoffs/" + written.ID,
	} {
		if response := sessionRequest(t, handler, http.MethodGet, path, nil); response.Code != http.StatusOK {
			t.Fatalf("GET %s on a closed issue: status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	if list := decodeBody[[]model.HandoffSummary](t, sessionRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/handoffs", nil)); len(list) != 1 || !sameHandoff(list[0], written) {
		t.Fatalf("closed issue's handoffs = %#v, want only the one written before the close", list)
	}
}

// Each write appends one handoff.recorded on the issue, under the writing session: its payload is
// exactly {handoff_id, phase, version}, never the body, and it notifies no one.
func TestHandoffWriteAppendsHandoffRecordedWithoutTheBody(t *testing.T) {
	handler := newTestHandler(t)
	issue := createInteractionIssue(t, handler, "TEST", "Handoff event", "spec")
	const secret = "the body never rides on the event"
	first := recordedHandoff(t, handler, issue.Key, "test", `{"note":"`+secret+`"}`)
	second := recordedHandoff(t, handler, issue.Key, "test", `{"note":"`+secret+` again"}`)

	response := dispatchRequest(t, handler, http.MethodGet, "/api/v1/issues/"+issue.Key+"/events?after=0&limit=200", nil, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("read events: status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), secret) {
		t.Fatalf("an event carries the handoff body: %s", response.Body.String())
	}
	type recordedEvent struct {
		Type    string                     `json:"type"`
		Actor   model.Actor                `json:"actor"`
		Notify  bool                       `json:"notify"`
		Payload map[string]json.RawMessage `json:"payload"`
	}
	var recorded []recordedEvent
	for _, event := range decodeBody[[]recordedEvent](t, response) {
		if event.Type == "handoff.recorded" {
			recorded = append(recorded, event)
		}
	}
	if len(recorded) != 2 {
		t.Fatalf("handoff.recorded events = %d, want 2: %s", len(recorded), response.Body.String())
	}
	for index, handoff := range []model.HandoffSummary{first, second} {
		event := recorded[index]
		want := map[string]json.RawMessage{
			"handoff_id": json.RawMessage(fmt.Sprintf("%q", handoff.ID)),
			"phase":      json.RawMessage(`"test"`),
			"version":    json.RawMessage(fmt.Sprint(handoff.Version)),
		}
		if !reflect.DeepEqual(event.Payload, want) {
			t.Fatalf("event %d payload = %s, want exactly handoff_id, phase and version of %#v", index, event.Payload, handoff)
		}
		if event.Actor.Kind != "session" || event.Actor.ID != handoff.Author.ID || event.Notify {
			t.Fatalf("event %d actor = %#v notify = %v, want the writing session and no notification", index, event.Actor, event.Notify)
		}
	}
}
