package bus

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A bind in flight when Drain stops the client, such as the role lane's restore on a connection
// the client dialled to replace a closed one, can end after the stop with role messages the server
// has already routed to the subscription it made. None of them is discarded with that
// subscription. While Drain is still taking the subscriptions it drains, the deliveries finish with
// the rest, before the connection drains, so each forward can still subscribe its receipt inbox.
// Once Drain has taken its last, the connection's drain lets them finish.
func TestDrainLetsTheDeliveriesOfABindEndingAfterTheStopFinish(t *testing.T) {
	for _, tc := range []struct {
		name string
		// late has the bind end once Drain has taken the last subscription it drains itself.
		late bool
	}{
		{name: "before Drain takes the subscriptions"},
		{name: "after Drain has taken its last", late: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := testnats.URL(t)
			client, err := ConnectOwningStream([]string{uri})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			defer client.Close()
			const holderSubject = "test.drain-late-bind.holder"
			holder := testnats.Connect(t, uri)
			defer holder.Close()
			if _, err := holder.Subscribe(holderSubject, func(msg *nats.Msg) { _ = msg.Respond(nil) }); err != nil {
				t.Fatalf("subscribe the holder: %v", err)
			}
			if err := holder.Flush(); err != nil {
				t.Fatalf("flush the holder: %v", err)
			}

			// The handler forwards each role message to the holder, as the listener's role lane
			// does, once the drain is under way. Late, the connection is already draining, which
			// refuses the receipt inbox, so it only records the delivery.
			release := make(chan struct{})
			releaseHandlers := sync.OnceFunc(func() { close(release) })
			defer releaseHandlers()
			started := make(chan struct{}, 2)
			var finished, forwarded atomic.Int32
			var forwardErr atomic.Value
			handler := func(*nats.Msg) {
				defer finished.Add(1)
				started <- struct{}{}
				<-release
				if tc.late {
					return
				}
				if err := client.RequestCoreTo(holderSubject, contracts.Envelope{EventID: "drain-late-bind"}, 2*time.Second); err != nil {
					forwardErr.Store(err.Error())
					return
				}
				forwarded.Add(1)
			}

			// The restore claims the role lane's registration and binds it, as restoreSubscriptions
			// does, and the server routes two role messages to the subscription the bind made.
			const roleSubject = "notifications.role.drain-late-bind"
			client.subscriptionsMu.Lock()
			slot := &client.subscriptions[coreSubscription]
			*slot = recoverableSubscription{transport: coreSubscription, subject: roleSubject, queue: "drain-late-bind", handler: handler, binding: make(chan struct{})}
			registration := *slot
			client.subscriptionsMu.Unlock()
			sub, err := bind(registration, client.Conn, client.js)
			if err != nil {
				t.Fatalf("bind: %v", err)
			}
			if err := client.Conn.Flush(); err != nil {
				t.Fatalf("flush the bind: %v", err)
			}
			publisher := testnats.Connect(t, uri)
			defer publisher.Close()
			for range 2 {
				if err := publisher.Publish(roleSubject, []byte(`{}`)); err != nil {
					t.Fatalf("publish: %v", err)
				}
			}
			if err := publisher.Flush(); err != nil {
				t.Fatalf("flush the publishes: %v", err)
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("the first role message never reached the handler")
			}
			// nats.go counts a delivery as pending until its handler returns.
			for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
				if pending, _, _ := sub.Pending(); pending == 2 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the second role message never queued behind the first")
				}
			}
			// Drain's stop lands, and then the bind ends.
			client.subscriptionsMu.Lock()
			client.stopMode = clientDraining
			if tc.late {
				client.drainCollected = true
			}
			client.subscriptionsMu.Unlock()
			client.stop()
			_ = client.endBind(slot, sub, nil)
			drained := make(chan error, 1)
			go func() { drained <- client.Drain(5 * time.Second) }()
			// Well within this, a drain that takes no subscription has begun draining the connection.
			time.Sleep(200 * time.Millisecond)
			releaseHandlers()
			select {
			case err := <-drained:
				if err != nil {
					t.Fatalf("drain: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Drain never returned")
			}
			if finished := finished.Load(); finished != 2 {
				t.Fatalf("%d of the 2 role messages routed to the bind finished in the handler before Drain returned; want both", finished)
			}
			if forwarded := forwarded.Load(); !tc.late && forwarded != 2 {
				t.Fatalf("%d of the 2 role messages routed to the bind were forwarded (last forward error: %v); want both", forwarded, forwardErr.Load())
			}
		})
	}
}

// Close may arrive after a subscription's raw bind succeeded and before it has installed the
// subscription. That bind belongs to the stopped client: it must not be recorded as active.
func TestCloseDoesNotInstallABindThatFinishesAfterStop(t *testing.T) {
	client, err := ConnectOwningStream([]string{testnats.URL(t)})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	const subject = "notifications.role.close-after-bind"
	client.subscriptionsMu.Lock()
	slot := &client.subscriptions[coreSubscription]
	*slot = recoverableSubscription{
		transport: coreSubscription,
		subject:   subject,
		handler:   func(*nats.Msg) {},
		binding:   make(chan struct{}),
	}
	registration := *slot
	client.subscriptionsMu.Unlock()
	sub, err := bind(registration, client.Conn, client.js)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	client.Close()
	if err := client.endBind(slot, sub, nil); !errors.Is(err, errStopped) {
		t.Fatalf("end bind after Close: %v, want %v", err, errStopped)
	}
	client.subscriptionsMu.Lock()
	active := slot.active
	client.subscriptionsMu.Unlock()
	if active != nil {
		t.Fatal("a bind that finished after Close was installed as active")
	}
}

// Once a client stops, neither subscription transport starts another registration.
func TestStoppedClientRefusesNewSubscriptions(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stop    func(*Client) error
		stopped bool
	}{
		{name: "Running"},
		{
			name:    "Close",
			stopped: true,
			stop: func(client *Client) error {
				client.Close()
				return nil
			},
		},
		{
			name:    "Drain",
			stopped: true,
			stop: func(client *Client) error {
				return client.Drain(5 * time.Second)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := ConnectOwningStream([]string{testnats.URL(t)})
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			if tc.stop != nil {
				if err := tc.stop(client); err != nil {
					t.Fatalf("stop: %v", err)
				}
			}
			t.Cleanup(client.Close)

			for _, subscribe := range []struct {
				name string
				run  func() error
			}{
				{"Subscribe", func() error {
					_, err := client.Subscribe("notifications.github.acme.widgets.push.branch.main", func(*nats.Msg) {})
					return err
				}},
				{"SubscribeCore", func() error {
					_, err := client.SubscribeCore("notifications.role.close-after-stop", func(*nats.Msg) {})
					return err
				}},
			} {
				err := subscribe.run()
				if tc.stopped && !errors.Is(err, errStopped) {
					t.Fatalf("%s after %s: %v, want %v", subscribe.name, tc.name, err, errStopped)
				}
				if !tc.stopped && err != nil {
					t.Fatalf("%s while %s: %v", subscribe.name, tc.name, err)
				}
			}
		})
	}
}
