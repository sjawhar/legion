package testnats

import (
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"
)

// proxiedClient connects to the proxy's URL as a deployed client does, reconnecting for as long as
// the test runs, and reports each disconnect and reconnect on the returned channels.
func proxiedClient(t *testing.T, proxy *Proxy) (conn *natsgo.Conn, disconnected, reconnected <-chan struct{}) {
	t.Helper()
	down, up := make(chan struct{}, 8), make(chan struct{}, 8)
	conn, err := natsgo.Connect(proxy.URL(),
		natsgo.Timeout(dialTimeout),
		natsgo.MaxReconnects(-1),
		natsgo.ReconnectWait(50*time.Millisecond),
		natsgo.DisconnectErrHandler(func(*natsgo.Conn, error) { down <- struct{}{} }),
		natsgo.ReconnectHandler(func(*natsgo.Conn) { up <- struct{}{} }),
	)
	if err != nil {
		t.Fatalf("connect through the proxy: %v", err)
	}
	t.Cleanup(conn.Close)
	return conn, down, up
}

func waitSignal(t *testing.T, signal <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(within):
		t.Fatalf("%s did not happen within %v", what, within)
	}
}

// A proxy relays a client's connection, credentials and all, until Cut: the connection drops at
// once and every dial is refused while it stays cut. Restore lets the client reconnect, and a new
// client connect, through the same URL.
func TestAProxyRelaysUntilCutAndAgainOnceRestored(t *testing.T) {
	proxy := StartProxy(t, URL(t))
	conn, disconnected, reconnected := proxiedClient(t, proxy)
	if err := conn.FlushTimeout(time.Second); err != nil {
		t.Fatalf("flush through the proxy: %v", err)
	}

	proxy.Cut()
	waitSignal(t, disconnected, 2*time.Second, "the client's disconnect after the cut")
	if refused, err := natsgo.Connect(proxy.URL(), natsgo.Timeout(dialTimeout), natsgo.NoReconnect()); err == nil {
		refused.Close()
		t.Fatal("a client connected through a cut proxy")
	}
	select {
	case <-reconnected:
		t.Fatal("the client reconnected through a cut proxy")
	case <-time.After(300 * time.Millisecond):
	}

	proxy.Restore()
	waitSignal(t, reconnected, 5*time.Second, "the client's reconnect after the restore")
	if err := conn.FlushTimeout(time.Second); err != nil {
		t.Fatalf("flush after the restore: %v", err)
	}
	fresh, err := natsgo.Connect(proxy.URL(), natsgo.Timeout(dialTimeout), natsgo.NoReconnect())
	if err != nil {
		t.Fatalf("connect through the restored proxy: %v", err)
	}
	fresh.Close()
}

// A frozen proxy keeps the client connected and moves nothing: what the client publishes does not
// reach a subscriber, and a flush gets no answer. Restore delivers what was held, and the
// connection that stayed open answers again.
func TestAFrozenProxyKeepsTheConnectionAndMovesNothingUntilRestored(t *testing.T) {
	uri := URL(t)
	proxy := StartProxy(t, uri)
	conn, disconnected, _ := proxiedClient(t, proxy)
	direct := Connect(t, uri)
	t.Cleanup(direct.Close)
	received := make(chan *natsgo.Msg, 4)
	if _, err := direct.ChanSubscribe("proxy.frozen", received); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := direct.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}

	proxy.Freeze()
	if err := conn.Publish("proxy.frozen", []byte("held")); err != nil {
		t.Fatalf("publish through the frozen proxy: %v", err)
	}
	if err := conn.FlushTimeout(500 * time.Millisecond); err == nil {
		t.Fatal("a flush through the frozen proxy was answered")
	}
	select {
	case msg := <-received:
		t.Fatalf("the subscriber received %q through a frozen proxy", msg.Data)
	case <-disconnected:
		t.Fatal("the client was disconnected by a freeze")
	case <-time.After(300 * time.Millisecond):
	}
	if !conn.IsConnected() {
		t.Fatalf("the client's status is %v behind a frozen proxy, want CONNECTED", conn.Status())
	}

	proxy.Restore()
	if err := conn.FlushTimeout(2 * time.Second); err != nil {
		t.Fatalf("flush after the restore: %v", err)
	}
	select {
	case msg := <-received:
		if string(msg.Data) != "held" {
			t.Fatalf("the subscriber received %q, want the held publish", msg.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the publish the freeze held never arrived after the restore")
	}
}

// ProxyLatency adds its round trip to every round trip through the proxy.
func TestAProxyWithLatencyAddsItsRoundTrip(t *testing.T) {
	const rtt = 200 * time.Millisecond
	proxy := StartProxy(t, URL(t), ProxyLatency(rtt))
	conn, _, _ := proxiedClient(t, proxy)
	started := time.Now()
	if err := conn.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush through the proxy: %v", err)
	}
	if took := time.Since(started); took < rtt {
		t.Fatalf("a round trip through the proxy took %v, want at least the %v it adds", took, rtt)
	}
}
