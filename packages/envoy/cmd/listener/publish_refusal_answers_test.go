package main

import (
	"context"
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

// supersedingRoleDeleteKV answers every conditional Delete of a role claim with nats.ErrKeyExists,
// the server's word for a revision that is no longer the stored one: a fresh claim landed first.
// store.ReleaseExpiredRoleClaim reads that as ExpiredRoleClaimSuperseded, so each attempt
// resolveLiveRoleHolder makes loses its compare-and-swap while every bus read it makes answers --
// the one state that spends all of roleHolderResolutionAttempts on supersessions. The claim's
// stored bytes are left alone: every other call goes to the real bucket.
type supersedingRoleDeleteKV struct {
	nats.KeyValue
}

func (kv supersedingRoleDeleteKV) Delete(key string, opts ...nats.DeleteOpt) error {
	return nats.ErrKeyExists
}

// A publish whose role claim is superseded on every resolution attempt is a lost race against a
// live replacement holder, not a bus that went quiet: each attempt's reads were answered, and only
// the conditional release lost. This asserts the sentinel resolveLiveRoleHolder returns for it and
// the whole answer publishHandler gives it -- the 503 and the Retry-After that ask for the retry a
// fresh read wins, the body that names the race, and the record that names it too, so the
// bus_unavailable bucket an operator reads afterwards keeps meaning a bus that did not answer.
//
// What it does not prove: that two listeners racing one claim produce this state. That needs two
// listeners and a real contended claim; here the supersession is forced by a KV whose conditional
// Delete always loses, which is the same answer store.ReleaseExpiredRoleClaim reads from the
// server when a real race is lost (internal/store/kv.go), reached without the timing.
func TestAPublishThatLosesTheRoleClaimRaceSaysSoRatherThanBlamingTheBus(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_superseded_holder"
	)
	topic := contracts.RoleTopicPrefix + role
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("seed the role claim: %v", err)
	}
	// The holder's session is never registered, so every attempt reads the claim, finds its holder
	// gone and tries to release it -- and loses that release to the KV above, every time.
	setRegistryRoleKV(t, registry, supersedingRoleDeleteKV{KeyValue: registryRoleKV(t, registry)})

	if _, err := resolveLiveRoleHolder(context.Background(), registry, sessions, role); !errors.Is(err, errRoleHolderChanged) {
		t.Fatalf("resolving a claim superseded on every attempt = %v, want errRoleHolderChanged", err)
	}

	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	recorder, record := serveRecorded(t,
		func(logger *logging.Logger) http.HandlerFunc { return publishHandler(state, logger) },
		postJSON("/v1/messages/publish", sendBody(map[string]any{
			"topic":          topic,
			"source":         "agent",
			"source_session": "ses_racing_publisher",
			"message":        "the claim moved while I was resolving it",
		})))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("publish into a lost claim race: status %d, body %s; want 503", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1: a fresh read wins this race as soon as the replacement claim settles", got)
	}
	var refusal apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	want := "envoy: the holder of role reviewer changed while this publish was resolving it"
	if refusal.Error != want {
		t.Fatalf("the refusal reads %q, want %q", refusal.Error, want)
	}
	if strings.Contains(refusal.Error, busUnansweredPrefix) {
		t.Fatalf("the refusal reads %q: every bus read answered, so it must not say the bus did not", refusal.Error)
	}
	assertRecordFields(t, record, map[string]any{
		"msg":     "listener publish",
		"level":   "WARN",
		"outcome": "role_holder_changed",
		"status":  float64(http.StatusServiceUnavailable),
		"topic":   topic,
	})
	t.Logf("lost claim race: %s", refusal.Error)
}

// GET /v1/roles/<role> resolves its holder exactly as a role-lane publish does, so it can lose the
// same race, and it answers it as the publish route does: the 503 and the Retry-After that ask for
// the retry a fresh lookup wins, and a body that names the race in a lookup's words. Every bus read
// answered, so neither the 500 this route gives a failed read nor the bus-unavailable prefix would
// be true of it.
func TestARoleLookupThatLosesTheRoleClaimRaceSaysSoRatherThanFailing(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_superseded_holder"
	)
	client := setupPublishTestClient(t)
	registry, sessions := setupSessionsTest(t, nil, nil)
	if _, err := registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("seed the role claim: %v", err)
	}
	// As in the publish test above: the holder is never registered, so every attempt finds it gone
	// and loses its conditional release.
	setRegistryRoleKV(t, registry, supersedingRoleDeleteKV{KeyValue: registryRoleKV(t, registry)})

	state := &listenerDeps{client: client, registry: registry, sessions: sessions}
	recorder := httptest.NewRecorder()
	roleGetHandler(state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("lookup into a lost claim race: status %d, body %s; want 503", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1: a fresh lookup wins this race as soon as the replacement claim settles", got)
	}
	var refusal apiError
	if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
	}
	want := "envoy: the holder of role reviewer changed while this lookup was resolving it"
	if refusal.Error != want {
		t.Fatalf("the refusal reads %q, want %q", refusal.Error, want)
	}
	if got, err := registry.RoleHolder(role); err != nil || got != holder {
		t.Fatalf("the claim after the lookup = %q, %v; want %s's, untouched", got, err, holder)
	}
}

// stalledRoleKV is a role bucket one of whose calls does not answer: a claim read (stallGet) or a
// conditional delete (stallDelete) waits stall, or until the test ends, and then fails, the way a
// legacy nats.go KV call waits out the JetStream MaxWait on a bus that has stopped answering. Every
// other call goes to the real bucket. stall is longer than sendRequestBudget, so a route that waits
// the call out answers after its budget, and one bounded by its budget answers at it.
type stalledRoleKV struct {
	nats.KeyValue
	stallGet, stallDelete bool
	stall                 time.Duration
	released              <-chan struct{}
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

func (kv stalledRoleKV) Delete(key string, opts ...nats.DeleteOpt) error {
	if kv.stallDelete {
		kv.wait()
		return errors.New("the role bucket's delete never answered")
	}
	return kv.KeyValue.Delete(key, opts...)
}

// The two routes that resolve a role's holder do it within sendRequestBudget of the request's own
// context, so a role-bucket call that does not answer - the claim read, or the conditional release
// of a claim whose holder is gone, neither of which takes a context in nats.go - is reported inside
// the caller's attempt (the client gives each 5 s, packages/envoy-client/src/transport.ts) rather
// than after it: a role-lane publish answers the bus-unavailable 503 with Retry-After: 1, a lookup
// its 500, each naming the call that did not answer. Each must answer at its budget and before
// stalledRoleKV's stall: answering before the budget would mean something other than the stalled
// call ended the request, and answering after it that the resolution waited the call out.
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
		name                  string
		stallGet, stallDelete bool
		unanswered            string
	}{
		{name: "the claim read", stallGet: true, unanswered: "read role reviewer's claim: context deadline exceeded"},
		{name: "the conditional release", stallDelete: true, unanswered: "release role reviewer's expired claim held by ses_unanswered_holder: context deadline exceeded"},
	} {
		// open seeds a claim whose holder never registered, so a resolution that reads it goes on
		// to release it, and then stalls the role bucket's tc call.
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
				KeyValue:    registryRoleKV(t, registry),
				stallGet:    tc.stallGet,
				stallDelete: tc.stallDelete,
				stall:       6 * time.Second,
				released:    released,
			})
			return &listenerDeps{client: client, registry: registry, sessions: sessions}
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
		t.Run("lookup/"+tc.name, func(t *testing.T) {
			state := open(t)
			started := time.Now()
			recorder := httptest.NewRecorder()
			roleGetHandler(state).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/roles/"+role, nil))
			elapsed := time.Since(started)
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("lookup whose role bucket did not answer: status %d, body %s; want 500", recorder.Code, recorder.Body.String())
			}
			answeredInTime(t, elapsed)
			var refusal apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if !strings.Contains(refusal.Error, tc.unanswered) {
				t.Fatalf("the refusal reads %q, want it to name %q", refusal.Error, tc.unanswered)
			}
		})
	}
}

// A lookup whose role store does not answer inside its budget is a bus that did not answer, and
// GET /v1/roles/<role> answers it as every other route does (writeBusUnavailable): 503, Retry-After:
// 1, and a body that begins busUnansweredPrefix and names the call that did not answer. A 500 here
// reads as a broken listener rather than an unreachable bus, the distinction the spec gives
// bus-down its 503 for, so an operator's filter on that prefix finds this outage too; and the
// publish route already answers the same stalled store this way, in the test above. The bus is
// connected throughout, so nothing but the stalled role-bucket call can end the lookup.
func TestARoleLookupWhoseStoreDoesNotAnswerIsRefusedAsTheBusNotAnswering(t *testing.T) {
	const (
		role   = "reviewer"
		holder = "ses_lookup_unanswered_holder"
		within = 4 * time.Second
	)
	for _, tc := range []struct {
		name                  string
		stallGet, stallDelete bool
		unanswered            string
	}{
		{name: "the claim read", stallGet: true, unanswered: "read role reviewer's claim: context deadline exceeded"},
		{name: "the conditional release", stallDelete: true, unanswered: "release role reviewer's expired claim held by " + holder + ": context deadline exceeded"},
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
				KeyValue:    registryRoleKV(t, registry),
				stallGet:    tc.stallGet,
				stallDelete: tc.stallDelete,
				stall:       6 * time.Second,
				released:    released,
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
