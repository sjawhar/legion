package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The session reaper a started listener runs deletes the consumer of every record it reaps. A
// pass deletes no consumer of its own: it deletes the one the SessionReaper it was given names
// (session.SessionReaper.DeleteConsumer), and main is the only place the listener's own session
// consumers are handed to it, so a pass built without that deleter reaps records and leaves their
// consumers on the stream until the inactive threshold takes them hours later. Nothing a test
// builds its own reaper in can see that: this one starts the real binary, registers a session
// through its /v1 as a plugin's heartbeat does, and leaves that session to reach the gone
// horizon.
//
// The windows come from the environment rather than from a clock a test moves, which another
// process has none of: a session is gone two seconds after its registration and a pass runs every
// 200 ms. The bucket's maximum age is left at its default, minutes longer than that, so the
// record is taken away by the pass and never by the bucket's own backstop.
func TestTheSessionReaperAStartedListenerRunsDeletesTheReapedSessionsConsumer(t *testing.T) {
	_, uri := testnats.Start(t)
	listener := startListenerProcess(t, cmdtest.Build(t, "envoy-listener"), uri, "session-consumer-reap-wiring",
		"ENVOY_SESSION_LIVE_WINDOW=1s",
		"ENVOY_SESSION_GONE_HORIZON=2s",
		"ENVOY_SESSION_REAP_INTERVAL=200ms",
	)
	listener.waitHealthy(t)
	// The test's own view of the session consumers, so what it reads of them is what the server
	// holds and not what the listener believes.
	consumers, err := bus.ConnectSessionConsumers([]string{uri})
	if err != nil {
		t.Fatalf("connect the test's view of the session consumers: %v", err)
	}
	t.Cleanup(consumers.Close)

	const sessionID = "ses_reap_wiring"
	postListener(t, listener.port, "/v1/interests/subscribe", consumerRegistration(sessionID, "e"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := consumers.Info(ctx, sessionID); err != nil {
		t.Fatalf("the consumer of %s after its registration: %v; want the stream holding it", sessionID, err)
	}

	// The record delete is the pass's own work and is logged by the pass itself, so a consumer
	// still on the stream after that line is a deleter the reaper was never given, not a pass
	// that never ran.
	listener.WaitForOutput(t, `"msg":"session reaper deleted the record of a session that reached the gone horizon with no clean stop"`)
	if output := listener.Output.String(); !strings.Contains(output, `"session_id":"`+sessionID+`"`) {
		t.Fatalf("the pass the listener ran names no record of %s:\n%s", sessionID, output)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		info, err := consumers.Info(ctx, sessionID)
		if errors.Is(err, bus.ErrSessionConsumerNotFound) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the consumer of %s is still on the stream after the pass deleted its record: %+v, %v; want the reaper the listener itself starts deleting it with the record",
				sessionID, info, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
