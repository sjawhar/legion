package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// stalledRoleKV is a role bucket whose claim read does not answer: a Get waits stall, or until the
// test ends, and then fails, the way a legacy nats.go KV call waits out the JetStream MaxWait on a
// bus that has stopped answering. Every other call goes to the real bucket. stall is longer than
// sendRequestBudget, so a route that waits the call out answers after its budget, and one bounded
// by its budget answers at it.
type stalledRoleKV struct {
	nats.KeyValue
	stallGet bool
	stall    time.Duration
	released <-chan struct{}
}

func (kv stalledRoleKV) wait() {
	select {
	case <-kv.released:
	case <-time.After(kv.stall):
	}
}

func (kv stalledRoleKV) Get(key string) (nats.KeyValueEntry, error) {
	if kv.stallGet {
		kv.wait()
		return nil, errors.New("the role bucket's read never answered")
	}
	return kv.KeyValue.Get(key)
}

// A role send through the publish route resolves its role's holder within sendRequestBudget of the request's own
// context, so a role-bucket call that does not answer - the claim read, which takes no context in
// nats.go - is reported inside the caller's attempt (the client gives each 5 s,
// packages/envoy-client/src/transport.ts) rather than after it: the bus-unavailable 503 with
// Retry-After: 1, naming the call that did not answer. It must answer at its budget and before
// stalledRoleKV's stall: answering before the budget would mean something other than the stalled
// call ended the request, and answering after it that the resolution waited the call out. A
// lookup's same case is TestARoleLookupWhoseStoreDoesNotAnswerIsRefusedAsTheBusNotAnswering's.
func TestARoleResolutionWhoseStoreCallDoesNotAnswerAnswersInsideTheBudget(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_unanswered_holder"
		// within is the bound each answer must land inside: the budget plus scheduling, and well
		// short of the 6 s stall and of the client's 5 s attempt.
		within = 4 * time.Second
	)
	topic := contracts.RoleTopicPrefix + role
	for _, tc := range []struct {
		name       string
		stallGet   bool
		unanswered string
	}{
		{name: "the claim read", stallGet: true, unanswered: "read role reviewer's claim: context deadline exceeded"},
	} {
		// open seeds a claim whose holder never registered, so a resolution reads the claim, finds
		// its holder gone and reports it lapsed - taking nothing from it - and then stalls the
		// role bucket's tc call.
		open := func(t *testing.T) *listenerDeps {
			t.Helper()
			client := setupPublishTestClient(t)
			registry, sessions := setupSessionsTest(t, nil, nil)
			if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
				t.Fatalf("seed the role claim: %v", err)
			}
			released := make(chan struct{})
			t.Cleanup(func() { close(released) })
			setRegistryRoleKV(t, registry, stalledRoleKV{
				KeyValue: registryRoleKV(t, registry),
				stallGet: tc.stallGet,
				stall:    6 * time.Second,
				released: released,
			})
			return withPublishRoute(t, &listenerDeps{client: client, registry: registry, sessions: sessions})
		}
		answeredInTime := func(t *testing.T, elapsed time.Duration) {
			t.Helper()
			if elapsed < sendRequestBudget || elapsed >= within {
				t.Fatalf("answered after %s, want at sendRequestBudget (%s) and inside %s", elapsed, sendRequestBudget, within)
			}
		}
		t.Run("publish/"+tc.name, func(t *testing.T) {
			state := open(t)
			started := time.Now()
			recorder, record := serveRecorded(t,
				func(logger *logging.Logger) http.HandlerFunc { return publishHandler(state, logger) },
				postJSON("/v1/messages/publish", sendBody(map[string]any{
					"topic":          topic,
					"source":         "agent",
					"source_session": "ses_unanswered_publisher",
					"message":        "is the holder there?",
				})))
			elapsed := time.Since(started)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("publish whose role bucket did not answer: status %d, body %s; want 503", recorder.Code, recorder.Body.String())
			}
			answeredInTime(t, elapsed)
			if got := recorder.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("Retry-After = %q, want 1", got)
			}
			var refusal apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) || !strings.Contains(refusal.Error, tc.unanswered) {
				t.Fatalf("the refusal reads %q, want it to begin %q and name %q", refusal.Error, busUnansweredPrefix, tc.unanswered)
			}
			assertRecordFields(t, record, map[string]any{
				"msg":     "listener publish",
				"level":   "WARN",
				"outcome": "bus_unavailable",
				"status":  float64(http.StatusServiceUnavailable),
				"topic":   topic,
			})
		})
	}
}

// A lookup whose role store does not answer inside its budget is a bus that did not answer, and
// GET /v1/roles/<role> answers it as every other route does (writeBusUnavailable): 503, Retry-After:
// 1, and a body that begins busUnansweredPrefix and names the call that did not answer. A 500 here
// reads as a broken listener rather than an unreachable bus, the distinction the spec gives
// bus-down its 503 for, so an operator's filter on that prefix finds this outage too; and the
// publish route already answers the same stalled store this way, in the test above. The bus is
// connected throughout, so nothing but the stalled role-bucket call can end the lookup. A lookup
// releases nothing, so the claim read is the only role-bucket call it makes.
func TestARoleLookupWhoseStoreDoesNotAnswerIsRefusedAsTheBusNotAnswering(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_lookup_unanswered_holder"
		within = 4 * time.Second
	)
	for _, tc := range []struct {
		name       string
		stallGet   bool
		unanswered string
	}{
		{name: "the claim read", stallGet: true, unanswered: "read role reviewer's claim: context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := setupPublishTestClient(t)
			registry, sessions := setupSessionsTest(t, nil, nil)
			if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
				t.Fatalf("seed the role claim: %v", err)
			}
			released := make(chan struct{})
			t.Cleanup(func() { close(released) })
			setRegistryRoleKV(t, registry, stalledRoleKV{
				KeyValue: registryRoleKV(t, registry),
				stallGet: tc.stallGet,
				stall:    6 * time.Second,
				released: released,
			})
			state := &listenerDeps{client: client, registry: registry, sessions: sessions}

			started := time.Now()
			recorder := httptest.NewRecorder()
			roleGetHandler(state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
			elapsed := time.Since(started)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("lookup whose role store did not answer: status %d, body %s; want 503 - the bus did not answer, the listener did not fail", recorder.Code, recorder.Body.String())
			}
			if elapsed >= within {
				t.Fatalf("answered after %s, want inside %s", elapsed, within)
			}
			if got := recorder.Header().Get("Retry-After"); got != "1" {
				t.Fatalf("Retry-After = %q, want 1", got)
			}
			var refusal apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if !strings.HasPrefix(refusal.Error, busUnansweredPrefix) || !strings.Contains(refusal.Error, tc.unanswered) {
				t.Fatalf("the refusal reads %q, want it to begin %q and name %q", refusal.Error, busUnansweredPrefix, tc.unanswered)
			}
		})
	}
}

// A publish the server denies under this listener's own grant (bus.ErrPublishDenied) is answered
// as the permanent refusal it is: 403, no Retry-After, and a body naming the denial and the
// server's own violation text, so the client -- which retries every 5xx and no 4xx
// (packages/envoy-client/src/transport.ts) -- is not sent back to try something that can never
// succeed. The same classification is driven with a plain unreachable bus in the same test, so the
// two are separated rather than merely renamed: that one keeps the 503, the Retry-After and the
// bus_unavailable record. Both routes are driven, because refuseBusUnavailable is the one place
// either of them classifies a bus failure.
//
// What it does not prove: that the NATS server's denial arrives as bus.ErrPublishDenied. That is
// internal/bus/confirm.go's own ground, covered where an nkey-granted server can be started; here
// the error is built exactly as confirmPublished builds it and classified by the code the handlers
// call, so what is pinned is the answer, not the detection.
func TestADeniedPublishIsRefusedPermanentlyAndNotRecordedAsBusUnavailable(t *testing.T) {
	subject := contracts.RoleTopicPrefix + "reviewer"
	// As internal/bus/confirm.go's confirmPublished names it: ErrPublishDenied around the subject
	// and the connection's last error, which is nats.go's permissions violation.
	denied := fmt.Errorf("bus: %w to %q: %w", bus.ErrPublishDenied, subject,
		fmt.Errorf("%w: Permissions Violation for Publish to %q", nats.ErrPermissionViolation, subject))
	unreachable := fmt.Errorf("bus: publish to %q: %w", subject, nats.ErrConnectionClosed)

	for _, route := range []struct {
		name    string
		line    string
		laneKey string
		lane    string
	}{
		{"publish", "listener publish", "topic", subject},
		{"send", "listener send", "target_session", "ses_denied_target"},
	} {
		for _, tc := range []struct {
			name        string
			err         error
			wantStatus  int
			wantRetry   string
			wantBody    string
			wantOutcome string
		}{
			{
				name:        "a publish the grant denies",
				err:         denied,
				wantStatus:  http.StatusForbidden,
				wantRetry:   "",
				wantBody:    publishDeniedPrefix + ": " + denied.Error(),
				wantOutcome: "publish_denied",
			},
			{
				name:        "a bus that did not answer",
				err:         unreachable,
				wantStatus:  http.StatusServiceUnavailable,
				wantRetry:   "1",
				wantBody:    busUnansweredPrefix + ": " + unreachable.Error(),
				wantOutcome: "bus_unavailable",
			},
		} {
			t.Run(route.name+"/"+tc.name, func(t *testing.T) {
				recorder, record := serveRecorded(t, func(logger *logging.Logger) http.HandlerFunc {
					return func(w http.ResponseWriter, r *http.Request) {
						rec := &requestRecord{logger: logger, line: route.line, laneKey: route.laneKey, lane: route.lane}
						rec.refuseBusUnavailable(w, tc.err)
					}
				}, postJSON("/v1/messages/publish", "{}"))
				if recorder.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d; body = %s", recorder.Code, tc.wantStatus, recorder.Body.String())
				}
				if got := recorder.Header().Get("Retry-After"); got != tc.wantRetry {
					t.Fatalf("Retry-After = %q, want %q", got, tc.wantRetry)
				}
				var refusal apiError
				if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
					t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
				}
				if refusal.Error != tc.wantBody {
					t.Fatalf("the refusal reads %q, want %q", refusal.Error, tc.wantBody)
				}
				assertRecordFields(t, record, map[string]any{
					"msg":         route.line,
					"level":       "WARN",
					"outcome":     tc.wantOutcome,
					"status":      float64(tc.wantStatus),
					route.laneKey: route.lane,
				})
			})
		}
	}
}
