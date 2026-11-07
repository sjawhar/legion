package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestHeartbeatKVWritesPerHeartbeat counts the KV writes one heartbeat costs. N sessions, each
// holding one role claimed by the process its registrations name, send M identical POST
// /v1/interests/subscribe calls (same topics, dir, client instance and listener machine every
// time) through the listener's real /v1 routes on a real nats-server, and the test reads how far
// the interest, session and role buckets' streams moved over the M-1 heartbeats after each
// session's first registration. It logs all three rates and asserts two of them: the session row
// is written once per heartbeat, which a heartbeat always does, and the interest row not at all,
// since every heartbeat here lands well inside store.InterestRefreshAge of the registration that
// wrote the row. A regression of the skip on the route path (subscribeHandler through
// store.Registry.UpsertWithin) fails this test, where it was only logged.
//
// The role bucket's rate is measured and logged, not asserted: it is the price of a claim saying
// when its holder last answered for it (store.Registry.RenewRoleClaims, which subscribeHandler
// calls on every registration), one conditional write per heartbeat per role the session holds,
// and a number to read rather than a bound to hold. The read each renewal makes first costs no
// write and so moves no stream.
func TestHeartbeatKVWritesPerHeartbeat(t *testing.T) {
	const (
		sessionCount = 20
		heartbeats   = 5
		// instance is the process every registration here states, the one each session's
		// claim names as its holder: a renewal moves only a claim naming the registering
		// process, so the cost this counts is a holder renewing its own claims.
		instance = "4f1c2a9e8b7d6c5a4f1c2a9e8b7d6c5a"
	)
	sessionOf := func(index int) string { return fmt.Sprintf("ses_heartbeat_%02d", index) }
	roleOf := func(index int) string { return fmt.Sprintf("heartbeat-role-%02d", index) }
	registry, sessions := setupSessionsTest(t, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := registry.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("interest cache not ready: %v", err)
	}
	if err := sessions.WaitForCacheReady(ctx); err != nil {
		t.Fatalf("session cache not ready: %v", err)
	}
	routes := http.NewServeMux()
	registerV1Routes(routes, &listenerDeps{registry: registry, sessions: sessions}, "test-machine", logging.NewWithWriter("test", io.Discard))

	conn := testnats.Connect(t, testnats.URL(t))
	t.Cleanup(conn.Close)
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	lastSeq := func(bucket string) uint64 {
		t.Helper()
		info, err := js.StreamInfo("KV_" + bucket)
		if err != nil {
			t.Fatalf("stream info of bucket %s: %v", bucket, err)
		}
		return info.State.LastSeq
	}
	heartbeat := func(index int) {
		t.Helper()
		sessionID := sessionOf(index)
		body := fmt.Sprintf(`{"session_id":%q,"dir":"/work/%s","topics":["notifications.team.heartbeat"],"self_subscribed":true,"title":"heartbeat %02d","client_instance_id":%q}`, sessionID, sessionID, index, instance)
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(body)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("subscribe %s: status %d, body %s", sessionID, recorder.Code, recorder.Body.String())
		}
	}

	// The claims are taken before the first registration, so the role topic each one leaves on
	// its holder's interest record is written out by that registration, inside the first round
	// and before anything is counted: the heartbeats counted below are identical to each other.
	for index := range sessionCount {
		claim := store.RoleClaimRequest{SessionID: sessionOf(index), MachineID: "test-machine", ClientInstanceID: instance, Role: roleOf(index)}
		if _, err := registry.SetRoleWithPrevious(ctx, claim); err != nil {
			t.Fatalf("claim %s for %s: %v", roleOf(index), sessionOf(index), err)
		}
	}
	for index := range sessionCount {
		heartbeat(index)
	}
	interestsBefore, sessionsBefore, rolesBefore := lastSeq(bus.InterestBucket), lastSeq(bus.SessionBucket), lastSeq(bus.RoleClaimBucket)
	for range heartbeats - 1 {
		for index := range sessionCount {
			heartbeat(index)
		}
	}
	interestWrites := lastSeq(bus.InterestBucket) - interestsBefore
	sessionWrites := lastSeq(bus.SessionBucket) - sessionsBefore
	roleWrites := lastSeq(bus.RoleClaimBucket) - rolesBefore

	counted := float64(sessionCount * (heartbeats - 1))
	interestRate := float64(interestWrites) / counted
	sessionRate := float64(sessionWrites) / counted
	roleRate := float64(roleWrites) / counted
	t.Logf("interest writes/heartbeat=%.2f session writes/heartbeat=%.2f role writes/heartbeat=%.2f total=%.2f",
		interestRate, sessionRate, roleRate, interestRate+sessionRate+roleRate)
	if sessionWrites != uint64(counted) {
		t.Fatalf("session bucket took %d writes over %v heartbeats, want one per heartbeat", sessionWrites, counted)
	}
	if interestWrites != 0 {
		t.Fatalf("interest bucket took %d writes over %v identical heartbeats, want none: each one is inside store.InterestRefreshAge (%v) of the row's first write",
			interestWrites, counted, store.InterestRefreshAge)
	}
}
