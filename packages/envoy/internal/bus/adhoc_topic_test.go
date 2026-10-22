package bus

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

const grantedTopic = "notifications.team.granted"

// Which transport each kind of topic takes: the role lanes and their exception lanes, and every
// notification topic the stream does not capture, over core NATS; every topic the stream captures,
// its exception lane included, and every subject outside notifications., through JetStream.
// An exception lane follows the same rule as any other topic: the stream captures the lane of an
// agent subject and no other lane, so the lane of a captured non-agent topic takes core NATS.
func TestUsesCoreTransportForRoleLanesAndEveryNotificationTheStreamDoesNotCapture(t *testing.T) {
	for _, tc := range []struct {
		topic string
		core  bool
	}{
		{contracts.RoleTopicPrefix + "reviewer", true},
		{ExceptionTopicPrefix + contracts.RoleTopicPrefix + "reviewer", true},
		{"notifications.team.deploy-squad", true},
		{"notifications.team.deploy-squad.all", true},
		{ExceptionTopicPrefix + "notifications.team.deploy-squad", true},
		{ExceptionTopicPrefix + "notifications.github.acme.widgets.pr.1.comment", true},
		{"notifications.githubx.acme", true},
		{"notifications.github", true},
		// A `*` or `>` in a published topic is a character of it, not a wildcard: the stream's
		// literal-prefix subjects capture none of these, so they take core NATS.
		{"notifications.>", true},
		{"notifications.*.deploy", true},
		{ExceptionTopicPrefix + ">", true},
		{contracts.AgentTopicPrefix + "ses_a", false},
		{ExceptionTopicPrefix + contracts.AgentTopicPrefix + "ses_a", false},
		{"notifications.dispatch.issue.ACME-1.ask.opened", false},
		{"notifications.github.acme.widgets.pr.1", false},
		{"notifications.slack.T1.C1.message", false},
		{"notifications.legion.ACME.controller", false},
		{"notifications.ghostwispr.s1.summary.ready", false},
		{"notifications.whatsapp.15551234567.5551234567@s.whatsapp.net.message", false},
		{"team.deploy-squad.all", false},
		{"$KV.envoy_roles.reviewer", false},
	} {
		if got := usesCoreTransport(tc.topic); got != tc.core {
			t.Errorf("usesCoreTransport(%q) = %t, want %t", tc.topic, got, tc.core)
		}
	}
}

// streamCaptures reads each stream subject as a literal prefix and `.>`; a subject of any other
// shape needs a matcher that splits tokens.
func TestEveryStreamSubjectIsALiteralPrefixAndWildcard(t *testing.T) {
	for _, subject := range streamSubjects {
		prefix, ok := strings.CutSuffix(subject, ".>")
		if !ok || prefix == "" || strings.ContainsAny(prefix, "*>") {
			t.Errorf("stream subject %q is not a literal prefix followed by .>", subject)
		}
	}
}

// An ad-hoc topic, one no stream subject captures, is published over core NATS: the publish
// answers at once, a subscriber receives the envelope, and the stream retains nothing for it. Before
// the fix the publish went to JetStream, which never answers it while a core subscriber holds the
// subject, so it failed at its five-second acknowledgement deadline.
func TestAnAdHocTopicIsPublishedOverCoreNATSAndReachesItsSubscriber(t *testing.T) {
	uri := testnats.URL(t)
	client, err := ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	subscriber := testnats.Connect(t, uri)
	t.Cleanup(subscriber.Close)

	const topic = "notifications.team.deploy-squad"
	received, err := subscriber.SubscribeSync(topic)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	envelope := contracts.Envelope{
		EventID:        "evt-adhoc",
		Source:         "agent",
		SourceEventID:  "source-adhoc",
		Topic:          topic,
		DedupeKey:      "publish.adhoc",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "deploy complete",
		TraceID:        "trace-adhoc",
	}
	started := time.Now()
	duplicate, err := client.PublishReportingDuplicate(envelope)
	if err != nil {
		t.Fatalf("publish to an ad-hoc topic: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("publish to an ad-hoc topic took %s; a core publish answers once the server confirms it", elapsed)
	}
	if duplicate {
		t.Fatal("a core publish has no stream verdict and reports no duplicate")
	}
	msg, err := received.NextMsg(5 * time.Second)
	if err != nil {
		t.Fatalf("the subscriber received nothing: %v", err)
	}
	if msg.Reply != "" {
		t.Fatalf("the envelope arrived as a request with reply %q; a JetStream publish would be one", msg.Reply)
	}

	if _, err := client.JS().GetLastMsg(Stream, topic); !errors.Is(err, nats.ErrMsgNotFound) {
		t.Fatalf("the stream's last message on %s: %v, want none", topic, err)
	}

	// The control: a topic the stream captures still goes to JetStream and is retained.
	captured := envelope
	captured.EventID = "evt-captured"
	captured.Topic = "notifications.github.acme.widgets.pr.1"
	if err := client.Publish(captured); err != nil {
		t.Fatalf("publish to a captured topic: %v", err)
	}
	if _, err := client.JS().GetLastMsg(Stream, captured.Topic); err != nil {
		t.Fatalf("the stream did not retain the captured topic: %v", err)
	}
}

// A publish to an ad-hoc topic the server denies the publisher is a refusal naming the subject, not
// a five-second wait: a listener whose grant leaves the topic out reports that at once.
func TestAnAdHocPublishNATSDeniesReturnsTheDenial(t *testing.T) {
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyGranted(t, public, grantedTopic, "_INBOX.>").URL
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)

	envelope := contracts.Envelope{
		EventID:        "evt-denied",
		Source:         "agent",
		SourceEventID:  "source-denied",
		Topic:          "notifications.team.denied",
		DedupeKey:      "publish.denied",
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "denied",
		TraceID:        "trace-denied",
	}
	started := time.Now()
	err = client.Publish(envelope)
	if !errors.Is(err, ErrPublishDenied) {
		t.Fatalf("denied ad-hoc publish returned %v, want ErrPublishDenied", err)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("denied ad-hoc publish waited %s", elapsed)
	}
	envelope.Topic = grantedTopic
	if err := client.Publish(envelope); err != nil {
		t.Fatalf("granted ad-hoc publish after a denial: %v", err)
	}
}
