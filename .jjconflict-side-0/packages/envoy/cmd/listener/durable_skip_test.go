package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// The durable matches a stored message's subject against this machine's interests (the registry's
// subject index) before it decodes the message, and acknowledges at once one no session needs from
// it: on a subject nobody here follows, on one only a session that takes stored messages itself
// follows (counted as its skipped delivery), or on the agent subject of another listener's session.
// A followed subject, and the agent subject of a session nobody has, are decoded as before. Every
// message here is undecodable, so a decode shows as the handler's decode failure.
func TestSubjectIndexAcksADurableMessageNoSessionNeedsBeforeDecodingIt(t *testing.T) {
	harness := newListenerDeliveryHarness(t, listenerTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("unexpected prompt_async request")
	}))
	harness.registerTarget(t, "ses_core", "notifications.dispatch.issue.FOLLOWED-1.>")
	if _, err := harness.registry.Upsert(store.Interest{SessionID: "ses_self", MachineID: "test-machine"}, []string{"notifications.dispatch.issue.SELF-1.*"}); err != nil {
		t.Fatalf("register the self-subscribed session's interest: %v", err)
	}
	if err := harness.sessions.Put("ses_self", session.SessionEntry{SelfSubscribed: true, MachineID: "test-machine"}); err != nil {
		t.Fatalf("register the self-subscribed session: %v", err)
	}
	if _, err := harness.registry.Upsert(store.Interest{SessionID: "ses_elsewhere", MachineID: "other-machine"}, []string{contracts.AgentSubject("ses_elsewhere")}); err != nil {
		t.Fatalf("register another listener's session: %v", err)
	}
	decoded := func() int { return strings.Count(harness.logs.String(), "listener decode failed") }
	undecodable := []byte("not an envelope")

	for _, subject := range []string{
		"notifications.dispatch.issue.OTHER-1.comment",
		"notifications.dispatch.issue.SELF-1.comment",
		contracts.AgentSubject("ses_elsewhere"),
	} {
		harness.handler(&natsgo.Msg{Subject: subject, Data: undecodable})
		if decoded() != 0 {
			t.Fatalf("a message on %s, which no session here needs from the durable, was decoded", subject)
		}
	}
	if body := deliveredMetrics(t, harness); !strings.Contains(body, `test_messages_delivered{delivery_status="skipped"} 1`) {
		t.Fatalf("delivery metrics %s, want the self-subscribed session's delivery skipped once", body)
	}

	for want, subject := range []string{
		"notifications.dispatch.issue.FOLLOWED-1.comment",
		contracts.AgentSubject("ses_absent"),
	} {
		harness.handler(&natsgo.Msg{Subject: subject, Data: undecodable})
		if decoded() != want+1 {
			t.Fatalf("a message on %s, which the durable path delivers, was not decoded", subject)
		}
	}
}
