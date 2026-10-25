package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

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

	if _, err := resolveLiveRoleHolder(registry, sessions, role); !errors.Is(err, errRoleHolderChanged) {
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
