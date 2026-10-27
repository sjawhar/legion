package bus_test

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

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
	seed, public := testnats.User(t)
	uri := testnats.StartNkeyGranted(t, public, grantedRole, "_INBOX.>").URL
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	subscriber, err := bus.Dial("role-subscriber", []string{uri}, os.LookupEnv)
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

// A window holding several denied core sends among granted ones answers every denied send with
// its denial and every granted one accepted, and delivers the granted ones: a granted send whose
// flush another send's denial left unconfirmed is published again once the window's flushes are
// in, one at a time, so a further denial cannot leave that republish unconfirmed too.
func TestAWindowWithSeveralDenialsConfirmsEveryGrantedSend(t *testing.T) {
	const pairs = 20
	client, subscriber := grantedClient(t)
	received, err := subscriber.SubscribeSync(grantedRole)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := subscriber.Flush(); err != nil {
		t.Fatalf("flush subscription: %v", err)
	}

	window := client.Window()
	denied := make([]<-chan error, 0, pairs)
	granted := make([]<-chan error, 0, pairs)
	for index := range pairs {
		denied = append(denied, window.PublishAsync(roleEnvelope(deniedRole+"-"+strconv.Itoa(index))))
		granted = append(granted, window.PublishAsync(roleEnvelope(grantedRole)))
	}
	answer := func(outcome <-chan error) error {
		select {
		case err := <-outcome:
			return err
		case <-time.After(30 * time.Second):
			t.Fatal("a send was never answered")
			return nil
		}
	}
	for index := range pairs {
		if err := answer(denied[index]); !errors.Is(err, bus.ErrPublishDenied) {
			t.Fatalf("denied send %d was answered %v, want ErrPublishDenied", index, err)
		}
		if err := answer(granted[index]); err != nil {
			t.Fatalf("granted send %d was answered %v, want accepted", index, err)
		}
	}
	for index := range pairs {
		if _, err := received.NextMsg(5 * time.Second); err != nil {
			t.Fatalf("granted envelope %d was not delivered: %v", index, err)
		}
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

// The server's refusal of the core subscription the client sent again on reconnecting reads as an
// inactive subscription until a server that grants it accepts it again. nats.go keeps a refused
// subscription valid and only records the refusal, so the listener's live topics lane, reconnected
// onto a server restarted without its grant on notifications.>, would deliver nothing while its
// /healthz answered healthy and subscribed.
func TestCoreSubOKIsFalseWhileTheServerRefusesTheResentSubscription(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "refusal-test"
		topic   = "notifications.team.refusal-test"
	)
	seed, public := testnats.User(t)
	granted := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, granted, []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	received := make(chan string, 1)
	held, err := client.SubscribeCore(subject, func(msg *natsgo.Msg) { received <- msg.Subject }, queue)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	if err := client.Conn.LastError(); err != nil {
		t.Fatalf("the server that grants %s refused it: %v", subject, err)
	}
	if !client.CoreSubOK() {
		t.Fatal("CoreSubOK is false on a server that grants the subscription")
	}

	server.Restart(t, testnats.SubscribeGrantConfig(public, "_INBOX.>"))
	waitFor(t, 30*time.Second, "CoreSubOK to read the restarted server's refusal of the re-sent subscription", func() bool {
		return client.Connected() && !client.CoreSubOK()
	})
	if !held.IsValid() {
		t.Fatal("nats.go dropped the refused subscription; the test proves nothing about a refusal it keeps valid")
	}
	if err := client.Conn.LastError(); !errors.Is(err, natsgo.ErrPermissionViolation) {
		t.Fatalf("the restarted server's last error is %v, want its refusal of %s", err, subject)
	}

	server.Restart(t, granted)
	waitFor(t, 30*time.Second, "CoreSubOK once a server that grants the subscription has it again", func() bool {
		return client.Connected() && client.CoreSubOK()
	})
	// The flush returns once the server has handled every protocol line the client sent before it,
	// the re-sent subscription among them.
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the re-sent subscription: %v", err)
	}
	publisher, err := bus.Dial("refusal-publisher", []string{uri}, os.LookupEnv)
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	if err := publisher.Publish(topic, []byte("{}")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := publisher.Flush(); err != nil {
		t.Fatalf("flush the publish: %v", err)
	}
	select {
	case got := <-received:
		if got != topic {
			t.Fatalf("the subscription received %s, want %s", got, topic)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the subscription the server granted again received nothing")
	}
	if !client.CoreSubOK() {
		t.Fatal("CoreSubOK is false after the server accepted the subscription and delivered to it")
	}
}

// A reload that takes the grant away revokes the core subscription the server had accepted, and
// the server says so in a refusal of its own form (`Permissions Violation for Subscription to
// "<subject>" (sid "<sid>")`), naming no queue group: CoreSubOK reads it as refused while the
// connection stays up, though nats.go keeps the subscription valid and leaves the refusal of a
// queue subscription unrecorded. A restart onto a configuration that grants it again, to which the
// client re-sends the subscription, makes it OK again.
func TestCoreSubOKIsFalseOnceAReloadRevokesTheSubscription(t *testing.T) {
	const (
		subject = "notifications.>"
		queue   = "revocation-test"
	)
	seed, public := testnats.User(t)
	granted := testnats.SubscribeGrantConfig(public, subject, "_INBOX.>")
	server, uri := testnats.StartConfig(t, granted, []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	held, err := client.SubscribeCore(subject, func(*natsgo.Msg) {}, queue)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := client.Conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	if !client.CoreSubOK() {
		t.Fatal("CoreSubOK is false on a server that grants the subscription")
	}

	if err := server.Reload(t, testnats.SubscribeGrantConfig(public, "_INBOX.>")); err != nil {
		t.Fatalf("reload without the grant: %v", err)
	}
	waitFor(t, 30*time.Second, "CoreSubOK to read the reload's revocation of the subscription", func() bool {
		return !client.CoreSubOK()
	})
	if !client.Connected() {
		t.Fatal("the client lost its connection to a server that only reloaded")
	}
	if !held.IsValid() {
		t.Fatal("nats.go dropped the revoked subscription; the test proves nothing about a refusal it keeps valid")
	}
	if err := client.Conn.LastError(); !errors.Is(err, natsgo.ErrPermissionViolation) ||
		!strings.Contains(err.Error(), `Permissions Violation for Subscription to "`+subject+`" (sid "`) {
		t.Fatalf("the server's last error is %v, want its revocation of %s naming the sid", err, subject)
	}

	server.Restart(t, granted)
	waitFor(t, 30*time.Second, "CoreSubOK once a restarted server that grants the subscription has it again", func() bool {
		return client.Connected() && client.CoreSubOK()
	})
}
