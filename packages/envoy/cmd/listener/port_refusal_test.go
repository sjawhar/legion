package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/testnats"
)

// This file holds dispatch://INFRA-889's acceptance A1: the listener delivers nothing to a
// session's serve port, so a registration naming one is refused by name, a registration with no
// port is accepted and answered port 0 by the session list, and the two environment variables the
// deleted paths were configured by are warned about once each and ignored.

// A registration whose port is above 0 is answered 400, whatever the port, and the refusal names
// the port field in its expected list, which is how every other refusal of this route names the
// field a caller must change (writeJSONError). A session that registered a port would wait for
// messages nothing sends: no listener opens a connection to a session any more. The refusal is
// answered before anything is stored, so neither the session's interests nor its session entry
// appear.
func TestARegistrationWithAPortIsAnswered400NamingThePortField(t *testing.T) {
	rig := newSessionConsumerRig(t)

	for index, port := range []int{1, 4242, 65535} {
		t.Run(fmt.Sprintf("port %d", port), func(t *testing.T) {
			sessionID := fmt.Sprintf("ses_a1_refused_%d", index)
			recorder := httptest.NewRecorder()
			rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe",
				strings.NewReader(fmt.Sprintf(`{"session_id":%q,"self_subscribed":true,"topics":["notifications.team.example"],"port":%d}`, sessionID, port))))

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", recorder.Code, recorder.Body.String())
			}
			var refusal apiError
			if err := json.Unmarshal(recorder.Body.Bytes(), &refusal); err != nil {
				t.Fatalf("decode the refusal %s: %v", recorder.Body.String(), err)
			}
			if !reflect.DeepEqual(refusal.Expected, []string{"port"}) {
				t.Fatalf("the refusal expects %v, want [port]: body %s", refusal.Expected, recorder.Body.String())
			}
			if !strings.Contains(refusal.Error, "port") {
				t.Fatalf("the refusal reads %q, want it naming the port field", refusal.Error)
			}
			if interest, err := rig.harness.registry.Get(context.Background(), sessionID); err == nil {
				t.Fatalf("the refused registration wrote interests %+v", interest)
			}
			if _, err := rig.harness.sessions.Get(sessionID); err == nil {
				t.Fatalf("the refused registration wrote a session entry for %s", sessionID)
			}
		})
	}
}

// A registration with no port that asks for the session's own consumer is accepted, and the
// session list answers port 0 for it: the field stays in the answer, always 0, because every
// released client parses it (packages/envoy-client/src/transport.ts). The stored entry records
// port 0 too, and the host the registration carried, which nothing dials, as it came.
func TestARegistrationWithNoPortIsAcceptedAndListedAtPortZero(t *testing.T) {
	rig := newSessionConsumerRig(t)
	// The list is joined from the interest copy and the session copy, and each answers the
	// refilling 503 rather than a list that could be missing a live agent until it has completed
	// a read of its bucket (sessionsHandler). Both reads are waited out here, before the
	// registration below, so the registration is written into copies that follow their buckets
	// and the list read at the end is one the route serves.
	waitForRegistry(t, rig.harness.registry)
	waitForSessions(t, rig.harness.sessions)
	const sessionID = "ses_a1_accepted"
	const host = "example-host:80"

	registration, err := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"self_subscribed": true,
		"topics":          []string{},
		"host":            host,
		"delivery":        map[string]any{"consumer": true, "epoch": "epoch-1"},
	})
	if err != nil {
		t.Fatalf("encode the registration of %s: %v", sessionID, err)
	}
	answer := rig.subscribe(t, string(registration))
	if answer.Delivery == nil || answer.Delivery.Mode != "consumer" {
		t.Fatalf("answer %s, want delivery mode consumer", answer.raw)
	}
	entry, err := rig.harness.sessions.Get(sessionID)
	if err != nil {
		t.Fatalf("read the stored entry of %s: %v", sessionID, err)
	}
	if entry.Port != 0 {
		t.Fatalf("the stored entry records port %d, want none: no registration carries one", entry.Port)
	}
	if entry.Host != host {
		t.Fatalf("the stored entry records host %q, want %q as it came", entry.Host, host)
	}

	recorder := httptest.NewRecorder()
	rig.mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/sessions", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /v1/sessions: status %d %s, want 200", recorder.Code, recorder.Body.String())
	}
	var listed []sessionInfo
	if err := json.Unmarshal(recorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode the session list %s: %v", recorder.Body.String(), err)
	}
	var found bool
	for _, info := range listed {
		if info.SessionID != sessionID {
			continue
		}
		found = true
		if info.Port != 0 {
			t.Fatalf("the list answers port %d for %s, want 0", info.Port, sessionID)
		}
	}
	if !found {
		t.Fatalf("the session list %s does not hold %s", recorder.Body.String(), sessionID)
	}
}

// A listener started with ENVOY_HOST_BRIDGE or ENVOY_SUBSCRIBE_ALL_TOPICS set, to any value,
// starts and serves: a deployment that still carries either setting is not an outage. Each is
// warned about exactly once, by name, so the operator reading the log knows what to remove, and
// neither is read for anything else.
func TestAListenerStartedWithTheDeletedVariablesWarnsOncePerVariableAndServes(t *testing.T) {
	uri := testnats.URL(t)
	machineID := "ignored-env-" + id.New()
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, machineID,
		"ENVOY_HOST_BRIDGE=anything", "ENVOY_SUBSCRIBE_ALL_TOPICS=anything")
	listener.waitHealthy(t)

	output := listener.Output.String()
	for _, name := range []string{"ENVOY_HOST_BRIDGE", "ENVOY_SUBSCRIBE_ALL_TOPICS"} {
		var warnings int
		for line := range strings.SplitSeq(output, "\n") {
			if strings.Contains(line, name+" is set and ignored") {
				warnings++
			}
		}
		if warnings != 1 {
			t.Fatalf("the listener logged %d warnings naming %s, want exactly one:\n%s", warnings, name, output)
		}
	}

	// The listener serves, and a registration through it is the ordinary one: neither variable
	// turned anything on that a registration could ask for.
	sessionID := "ses_ignored_env_" + id.New()
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "epoch-1"))
	listener.awaitSessionMachine(t, sessionID, machineID)
}
