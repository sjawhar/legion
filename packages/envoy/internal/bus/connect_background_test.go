package bus_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// stoppedNATS is a NATS server with JetStream that a test started and then stopped, and its URL:
// the server's relay keeps the URL's port and closes each connection it accepts until the test
// starts the server again (testnats.Server.Start).
func stoppedNATS(t *testing.T) (*testnats.Server, string) {
	t.Helper()
	srv, uri := testnats.Start(t)
	srv.Stop(t)
	return srv, uri
}

// An owning client made while its server is down is returned at once, not connected, and
// reconciles the stream on its first connection: before WithConnectInBackground the constructor
// failed, which ended envoy-dispatch's boot.
func TestAnOwningClientMadeWhileTheServerIsDownCreatesTheStreamOnItsFirstConnection(t *testing.T) {
	srv, uri := stoppedNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri}, bus.WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect owning the stream while the server is down: %v", err)
	}
	t.Cleanup(client.Close)
	if client.Connected() {
		t.Fatal("a client whose server is down reports itself connected")
	}

	srv.Start(t)
	waitFor(t, 15*time.Second, "the client's first connection", client.Connected)
	js, err := testnats.Connect(t, uri).JetStream()
	if err != nil {
		t.Fatalf("JetStream context: %v", err)
	}
	waitFor(t, 15*time.Second, "the stream the client owns", func() bool {
		_, err := js.StreamInfo(bus.Stream)
		return err == nil
	})
}

// A JetStream and a core subscription made before the client's first connection deliver once it
// has connected. The JetStream bind cannot reach the server before then, so it fails and its
// registration stays for the first connection to bind; the core subscription is sent by nats.go
// as it connects, and SubscribeCore returns at once rather than waiting out the outage.
func TestSubscriptionsMadeBeforeTheFirstConnectionDeliverAfterIt(t *testing.T) {
	srv, uri := testnats.Start(t)
	owner, err := bus.ConnectOwningStream([]string{uri})
	if err != nil {
		t.Fatalf("create the stream: %v", err)
	}
	owner.Close()
	srv.Stop(t)

	client, err := bus.Connect([]string{uri}, bus.WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect while the server is down: %v", err)
	}
	t.Cleanup(client.Close)

	var streamed, core atomic.Int32
	if _, err := subscribeAllNotifications(client, func(msg *natsgo.Msg) {
		streamed.Add(1)
		_ = msg.Ack()
	}); err == nil {
		t.Fatal("a JetStream bind before the first connection reported success")
	}
	const lane = "notifications.role.background-core"
	subscribed := make(chan error, 1)
	go func() {
		_, err := client.SubscribeCore(lane, func(*natsgo.Msg) { core.Add(1) })
		subscribed <- err
	}()
	select {
	case err := <-subscribed:
		if err != nil {
			t.Fatalf("a core subscription before the first connection: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SubscribeCore waited for a connection the server cannot give")
	}

	srv.Start(t)
	waitFor(t, 15*time.Second, "the client's first connection", client.Connected)
	publisher, err := bus.Connect([]string{uri})
	if err != nil {
		t.Fatalf("connect the publisher: %v", err)
	}
	t.Cleanup(publisher.Close)
	publishOnce(t, publisher, "before-the-first-connection")
	waitFor(t, 15*time.Second, "the JetStream subscription's delivery", func() bool { return streamed.Load() > 0 })
	// Core NATS keeps nothing for a subscriber the server has not seen yet, and nats.go reports the
	// connection CONNECTED a moment before the server has read the subscriptions it re-sent, so the
	// lane is published to until the subscription has received one.
	waitFor(t, 15*time.Second, "the core subscription's delivery", func() bool {
		_ = publisher.PublishCore(contracts.Envelope{
			EventID: "evt-core", Source: "agent", SourceEventID: "src-core", Topic: lane,
			DedupeKey: "core-before-the-first-connection", IssuedAt: contracts.NowMillis(),
		})
		return core.Load() > 0
	})
}

// shortAcknowledgementClock bounds a publish's wait to its own duration, so a test sees the
// deadline pass without waiting out the five seconds a publish has.
type shortAcknowledgementClock time.Duration

func (c shortAcknowledgementClock) WithTimeout(parent context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, time.Duration(c))
}

// A publish before the client's first connection waits within its own deadline and then fails, as
// one does while the client reconnects; the same client publishes once its server answers.
func TestAPublishBeforeTheFirstConnectionFailsWithinItsOwnDeadline(t *testing.T) {
	srv, uri := stoppedNATS(t)
	client, err := bus.ConnectOwningStream([]string{uri},
		bus.WithConnectInBackground(),
		bus.WithPublishAcknowledgementClock(shortAcknowledgementClock(300*time.Millisecond)))
	if err != nil {
		t.Fatalf("connect while the server is down: %v", err)
	}
	t.Cleanup(client.Close)
	envelope := contracts.Envelope{
		EventID: "evt-before-connect", Source: "github", SourceEventID: "src-before-connect",
		Topic:     "notifications.github.acme.widgets.push.branch.main",
		DedupeKey: "publish-before-connect", IssuedAt: contracts.NowMillis(),
	}
	started := time.Now()
	err = client.Publish(envelope)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a publish before the first connection = %v, want its deadline passed", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("a publish before the first connection took %s, past its 300ms deadline", elapsed)
	}

	srv.Start(t)
	waitFor(t, 15*time.Second, "the client's first connection", client.Connected)
	// The stream is reconciled by the first connection's hook, which may still be running.
	waitFor(t, 15*time.Second, "a publish after the first connection", func() bool {
		return client.Publish(envelope) == nil
	})
}

// Dial with WithConnectInBackground returns a connection that has not connected, and a core
// subscription made on it before it connects relays once it has: envoy-dispatch's agent
// conversation relay takes its viewers' subscriptions on it while the bus is down.
func TestDialInTheBackgroundRelaysACoreSubscriptionMadeBeforeItConnects(t *testing.T) {
	srv, uri := stoppedNATS(t)
	conn, err := bus.Dial("background-dial-test", []string{uri}, lookupOf(nil), bus.WithConnectInBackground())
	if err != nil {
		t.Fatalf("dial while the server is down: %v", err)
	}
	t.Cleanup(conn.Close)
	if conn.IsConnected() {
		t.Fatal("a connection whose server is down reports itself connected")
	}
	const subject = "agent.background-dial.frames"
	var relayed atomic.Int32
	if _, err := conn.Subscribe(subject, func(*natsgo.Msg) { relayed.Add(1) }); err != nil {
		t.Fatalf("subscribe before the connection connects: %v", err)
	}

	srv.Start(t)
	waitFor(t, 15*time.Second, "the connection's first connection", conn.IsConnected)
	publisher := testnats.Connect(t, uri)
	waitFor(t, 15*time.Second, "the relayed message", func() bool {
		_ = publisher.Publish(subject, []byte("frame"))
		_ = publisher.Flush()
		return relayed.Load() > 0
	})
}

// A configuration error still fails each constructor with WithConnectInBackground, before any
// dial: a seed that is not one, a server that is not this machine's, a SOCKS5 setting that is not
// an address. Only an unreachable or refusing server waits for the background.
func TestConfigurationErrorsStillFailTheConstructorInTheBackground(t *testing.T) {
	unsetNkeyEnvironment(t)
	local := []string{"nats://127.0.0.1:1"}
	cases := []struct {
		name        string
		urls        []string
		environment map[string]string
		want        string
	}{
		{"an unusable seed", local, map[string]string{"NATS_NKEY_SEED": "hunter2"}, "NATS_NKEY_SEED does not hold a valid nkey seed"},
		{"another machine's server", []string{"nats://nats.example.test:4222"}, nil, bus.ErrRemoteNATS.Error()},
		{"a proxy that is not an address", local, map[string]string{bus.SOCKS5ProxyEnvVar: "127.0.0.1", bus.AllowRemoteEnvVar: "1"}, bus.SOCKS5ProxyEnvVar},
	}
	constructors := map[string]func(urls []string, environment func(string) (string, bool)) error{
		"Connect": func(urls []string, environment func(string) (string, bool)) error {
			client, err := bus.Connect(urls, bus.WithEnvironment(environment), bus.WithConnectInBackground())
			if err == nil {
				client.Close()
			}
			return err
		},
		"ConnectOwningStream": func(urls []string, environment func(string) (string, bool)) error {
			client, err := bus.ConnectOwningStream(urls, bus.WithEnvironment(environment), bus.WithConnectInBackground())
			if err == nil {
				client.Close()
			}
			return err
		},
		"Dial": func(urls []string, environment func(string) (string, bool)) error {
			conn, err := bus.Dial("background-config-test", urls, environment, bus.WithConnectInBackground())
			if err == nil {
				conn.Close()
			}
			return err
		},
	}
	for _, tc := range cases {
		for name, construct := range constructors {
			started := time.Now()
			err := construct(tc.urls, lookupOf(tc.environment))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s with %s: %v, want a refusal naming %q", name, tc.name, err, tc.want)
			}
			if elapsed := time.Since(started); elapsed >= time.Second {
				t.Fatalf("%s with %s took %s to fail: it dialled", name, tc.name, elapsed)
			}
		}
	}
}

// A server that refuses the client's credential does not fail the constructor with
// WithConnectInBackground: the client keeps trying, never gives the connection up as nats.go
// otherwise does on a second refusal, and logs the refusal at ERROR once a minute, not once an
// attempt. nats.go reports no refusal before a connection has first connected, so without the
// client's own report the process would sit refused with nothing in its log.
func TestARefusedCredentialIsLoggedOnceAMinuteAndRetried(t *testing.T) {
	unsetNkeyEnvironment(t)
	_, allowed := testnats.User(t)
	refused, _ := testnats.User(t)
	uri := testnats.StartNkeyAuthorized(t, allowed)
	logs := captureBusLogs(t)
	const refusal = "envoy nats authorization refused"
	clock := &reconnectErrClock{now: time.Now()}
	name := "refusal-test-" + t.Name()
	client, err := bus.Connect([]string{uri},
		bus.WithName(name),
		bus.WithEnvironment(lookupOf(map[string]string{"NATS_NKEY_SEED": refused})),
		bus.WithReconnectErrClock(clock.Now),
		bus.WithConnectInBackground())
	if err != nil {
		t.Fatalf("connect with a refused credential: %v", err)
	}
	t.Cleanup(client.Close)

	waitFor(t, 15*time.Second, "the refusal's line", func() bool { return logs.count(name, refusal) > 0 })
	if line := logs.lineAt("ERROR", refusal); line == "" || !strings.Contains(strings.ToLower(line), "authorization violation") {
		t.Fatalf("the refusal is not an ERROR naming the server's answer:\n%s", logs.String())
	}
	// nats.go retries about once a second: three more attempts, and still one line.
	time.Sleep(3500 * time.Millisecond)
	if got := logs.count(name, refusal); got != 1 {
		t.Fatalf("%d refusal lines within a minute, want 1:\n%s", got, logs.String())
	}
	if client.Conn.IsClosed() {
		t.Fatal("the client gave its connection up over a refused credential")
	}
	clock.Advance(time.Minute)
	waitFor(t, 15*time.Second, "the next minute's refusal line", func() bool { return logs.count(name, refusal) == 2 })
	if client.Connected() {
		t.Fatal("a client whose credential is refused reports itself connected")
	}
}

// A connection whose server stops answering without closing it (packets dropped, no reset) is
// declared lost within seconds with WithSilentCutDetection, and reconnects once the traffic
// moves again. With nats.go's defaults (a ping every two minutes, two outstanding) the client
// would read connected for minutes into the cut.
func TestASilentCutIsNoticedWithinSecondsAndHealsWhenTrafficResumes(t *testing.T) {
	srv, uri := testnats.Start(t)
	client, err := bus.Connect([]string{uri}, bus.WithSilentCutDetection())
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	waitFor(t, 15*time.Second, "the client's connection", client.Connected)

	thaw := srv.Freeze(t)
	// Cleanups run last-in first-out, so this thaw precedes the server's shutdown.
	t.Cleanup(thaw)
	cut := time.Now()
	for client.Connected() {
		if time.Since(cut) > 5*time.Second {
			t.Fatalf("the client still reads connected %s into a silent cut", time.Since(cut))
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Logf("the cut was noticed %s after it began", time.Since(cut))

	thaw()
	waitFor(t, 15*time.Second, "the client to reconnect once the traffic moves again", client.Connected)
}
