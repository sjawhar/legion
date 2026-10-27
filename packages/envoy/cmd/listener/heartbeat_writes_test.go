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
	"github.com/sjawhar/envoy/internal/testnats"
)

// TestHeartbeatKVWritesPerHeartbeat counts the KV writes one heartbeat costs. N sessions each send
// M identical POST /v1/interests/subscribe calls (same topics, dir and listener machine every
// time) through the listener's real /v1 routes on a real nats-server, and the test reads how far
// the interest and session buckets' streams moved over the M-1 heartbeats after each session's
// first registration. It logs both rates. It asserts only that each heartbeat writes the session
// row once, which a heartbeat always does; the interest rate is logged, not asserted, since an
// unchanged interest need not be written on every heartbeat.
func TestHeartbeatKVWritesPerHeartbeat(t *testing.T) {
	const (
		sessionCount = 20
		heartbeats   = 5
	)
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
		sessionID := fmt.Sprintf("ses_heartbeat_%02d", index)
		body := fmt.Sprintf(`{"session_id":%q,"dir":"/work/%s","topics":["notifications.team.heartbeat"],"self_subscribed":true,"title":"heartbeat %02d"}`, sessionID, sessionID, index)
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/interests/subscribe", strings.NewReader(body)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("subscribe %s: status %d, body %s", sessionID, recorder.Code, recorder.Body.String())
		}
	}

	for index := range sessionCount {
		heartbeat(index)
	}
	interestsBefore, sessionsBefore := lastSeq(bus.InterestBucket), lastSeq(bus.SessionBucket)
	for range heartbeats - 1 {
		for index := range sessionCount {
			heartbeat(index)
		}
	}
	interestWrites := lastSeq(bus.InterestBucket) - interestsBefore
	sessionWrites := lastSeq(bus.SessionBucket) - sessionsBefore

	counted := float64(sessionCount * (heartbeats - 1))
	interestRate := float64(interestWrites) / counted
	sessionRate := float64(sessionWrites) / counted
	t.Logf("interest writes/heartbeat=%.2f session writes/heartbeat=%.2f total=%.2f", interestRate, sessionRate, interestRate+sessionRate)
	if sessionWrites != uint64(counted) {
		t.Fatalf("session bucket took %d writes over %v heartbeats, want one per heartbeat", sessionWrites, counted)
	}
}
