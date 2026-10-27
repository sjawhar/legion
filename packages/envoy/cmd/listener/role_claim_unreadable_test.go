package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
)

// unreadableRoleClaims are role claim records the bus holds and no read can decode
// (store.ErrMalformedRoleClaim), each with the detail its answer names.
var unreadableRoleClaims = []struct {
	name, record, detail string
}{
	{name: "a claim naming no holder", record: `{"holder_session_id":""}`, detail: "malformed role claim: it names no holder"},
	{name: "JSON that is not a claim", record: `{"holder_session_id":7}`, detail: "malformed role claim: json: "},
	{name: "a legacy record with no holder", record: "  ", detail: "malformed role claim: a legacy record with an empty holder"},
}

// openUnreadableRoleClaim stores record raw in the role bucket as role's claim, past every writer
// that would refuse it, and returns the listener state that resolves it.
func openUnreadableRoleClaim(t *testing.T, role, record string) (*listenerDeps, *store.Registry) {
	t.Helper()
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if _, err := registryRoleKV(t, registry).Put(role, []byte(record)); err != nil {
		t.Fatalf("store the claim record %q: %v", record, err)
	}
	return &listenerDeps{client: client, registry: registry, sessions: sessions}, registry
}

// assertUnreadableRoleClaimAnswer holds the answer both role routes give a claim record no read can
// decode: the bus answered, and will answer the same record again, so it is a 500 with no
// Retry-After - which would only send the client (packages/envoy-client/src/transport.ts retries
// every 5xx) back for the same record - and not the bus-unavailable 503, whose prefix would send an
// operator after an outage that never happened. The record itself is left as it was: nothing
// releases a claim no one can read the holder of.
func assertUnreadableRoleClaimAnswer(t *testing.T, recorder *httptest.ResponseRecorder, registry *store.Registry, role, record, detail string) {
	t.Helper()
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, body %s; want 500", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want none: a retry reads the same record", got)
	}
	var refusal apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	head := "envoy: the claim record of role " + role + " is unreadable: "
	if !strings.HasPrefix(refusal.Error, head) || !strings.Contains(refusal.Error, detail) {
		t.Fatalf("the refusal reads %q, want it to begin %q and name %q", refusal.Error, head, detail)
	}
	if strings.Contains(refusal.Error, busUnansweredPrefix) {
		t.Fatalf("the refusal reads %q: the bus answered, so it must not say the bus did not", refusal.Error)
	}
	entry, err := registryRoleKV(t, registry).Get(role)
	if err != nil {
		t.Fatalf("read the claim record after the request: %v", err)
	}
	if !bytes.Equal(entry.Value(), []byte(record)) {
		t.Fatalf("the claim record after the request = %q, want %q, untouched", entry.Value(), record)
	}
}

func TestARoleLookupAnswersAnUnreadableClaimRecordAsAServerError(t *testing.T) {
	const role = "reviewer"
	for _, tc := range unreadableRoleClaims {
		t.Run(tc.name, func(t *testing.T) {
			state, registry := openUnreadableRoleClaim(t, role, tc.record)
			recorder := httptest.NewRecorder()
			roleGetHandler(state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
			assertUnreadableRoleClaimAnswer(t, recorder, registry, role, tc.record, tc.detail)
		})
	}
}

// A role-lane publish answers the same record as the lookup does, and records it under its own
// outcome rather than bus_unavailable, at WARN as every refusal is.
func TestARolePublishAnswersAnUnreadableClaimRecordAsAServerError(t *testing.T) {
	const role = "reviewer"
	topic := contracts.RoleTopicPrefix + role
	for _, tc := range unreadableRoleClaims {
		t.Run(tc.name, func(t *testing.T) {
			state, registry := openUnreadableRoleClaim(t, role, tc.record)
			recorder, record := serveRecorded(t,
				func(logger *logging.Logger) http.HandlerFunc { return publishHandler(state, logger) },
				postJSON("/v1/messages/publish", sendBody(map[string]any{
					"topic":          topic,
					"source":         "agent",
					"source_session": "ses_unreadable_claim_publisher",
					"message":        "who holds this role?",
				})))
			assertUnreadableRoleClaimAnswer(t, recorder, registry, role, tc.record, tc.detail)
			assertRecordFields(t, record, map[string]any{
				"msg":     "listener publish",
				"level":   "WARN",
				"outcome": "role_claim_unreadable",
				"status":  float64(http.StatusInternalServerError),
				"topic":   topic,
			})
		})
	}
}
