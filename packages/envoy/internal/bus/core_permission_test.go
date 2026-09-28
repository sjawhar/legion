package bus_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

const (
	grantedRole = "notifications.role.granted"
	deniedRole  = "notifications.role.denied"
)

// grantedClient connects a bus client as a user NATS lets publish only to the granted role topic
// and to inboxes, and returns it with a plain connection of the same user that the tests subscribe
// through.
func grantedClient(t *testing.T) (*bus.Client, *natsgo.Conn) {
	t.Helper()
	seed, public := testUser(t)
	uri := testnats.StartNkeyPublishAllowed(t, public, grantedRole, "_INBOX.>")
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	user, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		t.Fatalf("parse seed: %v", err)
	}
	subscriber, err := natsgo.Connect(uri, natsgo.Nkey(public, user.Sign))
	if err != nil {
		t.Fatalf("connect subscriber: %v", err)
	}
	t.Cleanup(subscriber.Close)
	return client, subscriber
}

func roleEnvelope(topic string) contracts.Envelope {
	return contracts.Envelope{
		EventID:       "evt-" + topic,
		Source:        "agent",
		SourceEventID: "source-" + topic,
		Topic:         topic,
		DedupeKey:     "dedupe-" + topic,
		IssuedAt:      contracts.NowMillis(),
	}
}

// A core publish the server denies returns the denial, naming the subject, where core NATS itself
// reports nothing to the publisher; a granted one after it is still accepted and delivered, so
// the connection's recorded violation does not fail every later publish.
func TestPublishCoreReportsAPublishNATSDenies(t *testing.T) {
	client, subscriber := grantedClient(t)
	received, err := subscriber.SubscribeSync(grantedRole)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	err = client.PublishCore(roleEnvelope(deniedRole))
	if !errors.Is(err, bus.ErrPublishDenied) || !errors.Is(err, natsgo.ErrPermissionViolation) {
		t.Fatalf("denied publish returned %v, want ErrPublishDenied wrapping the permissions violation", err)
	}
	if !strings.Contains(err.Error(), `Permissions Violation for Publish to "`+deniedRole+`"`) {
		t.Fatalf("denied publish error %q does not carry the server's violation for %s", err, deniedRole)
	}

	if err := client.PublishCore(roleEnvelope(grantedRole)); err != nil {
		t.Fatalf("granted publish after a denial returned %v", err)
	}
	if _, err := received.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("granted publish was not delivered: %v", err)
	}
}

// A role-lane forward the server denies returns the denial at once, never ErrReceiptTimeout,
// which the listener treats as delivered to a live holder.
func TestRequestCoreToReportsAForwardNATSDenies(t *testing.T) {
	client, _ := grantedClient(t)
	started := time.Now()
	err := client.RequestCoreTo(deniedRole, roleEnvelope(deniedRole), 2*time.Second)
	if !errors.Is(err, bus.ErrPublishDenied) || errors.Is(err, bus.ErrReceiptTimeout) {
		t.Fatalf("denied forward returned %v, want ErrPublishDenied", err)
	}
	if elapsed := time.Since(started); elapsed >= 2*time.Second {
		t.Fatalf("denied forward waited %s, the whole receipt window", elapsed)
	}
}
