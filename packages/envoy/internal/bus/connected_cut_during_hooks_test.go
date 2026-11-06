package bus_test

import (
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
)

// connectedClearedWithin is how long Connected may go on reading up after the link was cut while
// a run of the reconnect hooks is under way. The clear is one atomic store from the connection's
// own disconnect callback, so this is scheduling alone. The bound it stands for is cmd/listener's
// delivery lease: its holder releases the machine's record once the receive signal has been down
// for 3 s (leaseReceiveLost), and a flag that reads up for as long as a hook runs pushes that
// release out by as long again.
const connectedClearedWithin = time.Second

// A cut must not wait behind the reconnect hooks. nats.go runs every callback of a connection one
// at a time on that connection's own dispatcher goroutine, so a reconnect callback that ran the
// hooks there - restoring the subscriptions and making the hooks' real requests - held the next
// disconnect callback behind it: the client read connected for as long as the hooks ran, which a
// deep review saw last 12.19 s after a cut. The client runs the hooks on a goroutine of its own
// instead, so the callback returns at once and the cut's clear lands on the dispatcher behind it.
func TestConnectedReadsDownAtACutWhileTheReconnectHooksRun(t *testing.T) {
	_, uri := startNATS(t)
	proxy := startRelayProxy(t, uri)
	client, err := bus.ConnectOwningStream([]string{proxy.url()}, bus.WithName(t.Name()))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(client.Close)
	running := make(chan struct{}, 1)
	finish := make(chan struct{})
	var ended atomic.Bool
	t.Cleanup(func() {
		if ended.CompareAndSwap(false, true) {
			close(finish)
		}
	})
	var runs atomic.Int32
	client.AddReconnectHook(func(*natsgo.Conn) error {
		// Only the first run holds: the hooks run again for the cut below, and a second hook
		// stuck here would outlive the test.
		if runs.Add(1) == 1 {
			running <- struct{}{}
			<-finish
		}
		return nil
	})

	// The first cut: the relay forwards again at once, so nats.go reconnects in place and the
	// reconnect's run of the hooks blocks in the hook above.
	proxy.drop()
	select {
	case <-running:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconnect hooks never ran after the reconnect")
	}
	if !client.Connected() {
		t.Fatal("the client reads disconnected while the reconnect's hooks run on a live connection")
	}

	// Every connection from here is held before the proxy dials the server, so the reconnect that
	// follows the cut cannot put the flag back up while the row reads it.
	proxy.delay.Store(int64(time.Minute))
	cut := time.Now()
	proxy.drop()
	for client.Connected() {
		if took := time.Since(cut); took > connectedClearedWithin {
			t.Fatalf("the client still reads connected %s after the link was cut while its reconnect hooks run, want the cut read within %s", took.Round(time.Millisecond), connectedClearedWithin)
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Logf("the client read the cut %s after it, with the reconnect hooks still running", time.Since(cut).Round(time.Millisecond))

	// The hooks end and the link comes back: the flag is kept by the callbacks throughout, so the
	// client reads connected again.
	if ended.CompareAndSwap(false, true) {
		close(finish)
	}
	proxy.delay.Store(0)
	waitFor(t, 30*time.Second, "the client to read connected again once its link is back", client.Connected)
}
