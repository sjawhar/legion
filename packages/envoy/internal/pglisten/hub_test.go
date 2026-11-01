package pglisten

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// hubDialer records every connection a hub under test dials, so a test can kill the backend it
// listens on.
type hubDialer struct {
	database *store.Store
	mu       sync.Mutex
	conns    []*pgx.Conn
}

func (d *hubDialer) dial(ctx context.Context) (*pgx.Conn, error) {
	conn, err := d.database.Pool.Listener(ctx)
	if err == nil {
		d.mu.Lock()
		d.conns = append(d.conns, conn)
		d.mu.Unlock()
	}
	return conn, err
}

func (d *hubDialer) lastPID(t *testing.T) uint32 {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.conns) == 0 {
		t.Fatal("the hub has dialed no connection")
	}
	return d.conns[len(d.conns)-1].PgConn().PID()
}

// runHub runs hub until the test ends, and fails it when Run does not return within 10s of its
// context ending.
func runHub(t *testing.T, hub *Hub) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("Run did not return within 10s of its context ending")
		}
	})
}

// counting is a callback that sends on the returned channel each time it runs, without ever
// blocking the listener's goroutine.
func counting() (func(), chan struct{}) {
	ran := make(chan struct{}, 16)
	return func() {
		select {
		case ran <- struct{}{}:
		default:
		}
	}, ran
}

func waitSignal(t *testing.T, signal <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(within):
		t.Fatalf("%s: no signal within %v", what, within)
	}
}

func wantNoSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
		t.Fatalf("%s: signalled, want not", what)
	default:
	}
}

// A NOTIFY on one channel signals every Wake of that channel, and no other channel's. Every Wake
// is also signalled once the first LISTEN succeeds, the catch-up every subscriber gets after a
// connect.
func TestAHubNotificationSignalsItsChannelsWakes(t *testing.T) {
	database := storetest.Open(t)
	dialer := &hubDialer{database: database}
	hub := &Hub{Owner: "test hub", Dial: dialer.dial}
	first, second := hub.Wake("hub_test_a"), hub.Wake("hub_test_a")
	other := hub.Wake("hub_test_b")
	after := hub.Wake("hub_test_c")
	onConnected, connected := counting()
	hub.OnConnected(onConnected)
	runHub(t, hub)

	waitSignal(t, connected, 5*time.Second, "first connection")
	waitSignal(t, first, time.Second, "first wake of hub_test_a after the LISTEN")
	waitSignal(t, second, time.Second, "second wake of hub_test_a after the LISTEN")
	waitSignal(t, other, time.Second, "wake of hub_test_b after the LISTEN")
	waitSignal(t, after, time.Second, "wake of hub_test_c after the LISTEN")

	// One connection sends both, so the listener reports them in that order: once hub_test_c's
	// wake is signalled, hub_test_a's notification has been handled in full.
	ctx := context.Background()
	sender, err := database.Pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire the sender's connection: %v", err)
	}
	defer sender.Release()
	if _, err := sender.Exec(ctx, `select pg_notify('hub_test_a', 'payload')`); err != nil {
		t.Fatalf("notify hub_test_a: %v", err)
	}
	if _, err := sender.Exec(ctx, `select pg_notify('hub_test_c', 'payload')`); err != nil {
		t.Fatalf("notify hub_test_c: %v", err)
	}
	waitSignal(t, after, 5*time.Second, "wake of hub_test_c after its NOTIFY")
	wantSignalled := func(wake <-chan struct{}, what string) {
		t.Helper()
		select {
		case <-wake:
		default:
			t.Fatalf("%s was not signalled by the NOTIFY", what)
		}
	}
	wantSignalled(first, "the first wake of hub_test_a")
	wantSignalled(second, "the second wake of hub_test_a")
	wantNoSignal(t, other, "the wake of hub_test_b, after NOTIFYs on hub_test_a and hub_test_c")
}

// A connection lost under the hub redials, runs OnConnected and signals every Wake again, so each
// subscriber catches up what was committed while nothing listened; notifications signal their
// wakes on the new connection.
func TestAHubSignalsEveryWakeAgainAfterAReconnect(t *testing.T) {
	database := storetest.Open(t)
	dialer := &hubDialer{database: database}
	hub := &Hub{Owner: "test hub", Dial: dialer.dial, Backoff: func(int) time.Duration { return 10 * time.Millisecond }}
	a, b := hub.Wake("hub_test_a"), hub.Wake("hub_test_b")
	onConnected, connected := counting()
	hub.OnConnected(onConnected)
	runHub(t, hub)

	waitSignal(t, connected, 5*time.Second, "first connection")
	waitSignal(t, a, time.Second, "wake of hub_test_a after the first LISTEN")
	waitSignal(t, b, time.Second, "wake of hub_test_b after the first LISTEN")
	killed := dialer.lastPID(t)

	terminateBackend(t, database, killed)
	waitSignal(t, connected, 5*time.Second, "OnConnected after the redial")
	if pid := dialer.lastPID(t); pid == killed {
		t.Fatalf("the hub reconnected on backend %d, the one that was killed", pid)
	}
	waitSignal(t, a, time.Second, "wake of hub_test_a after the redial")
	waitSignal(t, b, time.Second, "wake of hub_test_b after the redial")

	notifyChannel(t, database, "hub_test_b", "after")
	waitSignal(t, b, 5*time.Second, "wake of hub_test_b after a NOTIFY on the new connection")
}

// Registration is closed once Run has started: a channel added later would be missing from the
// LISTEN statement already sent and never heard.
func TestAHubRefusesRegistrationAfterRun(t *testing.T) {
	hub := &Hub{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hub.Run(ctx) // no channels: opens nothing, returns when ctx ends
	for name, register := range map[string]func(){
		"Wake":        func() { hub.Wake("hub_test_a") },
		"OnConnected": func() { hub.OnConnected(func() {}) },
		"Run":         func() { hub.Run(ctx) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Errorf("%s after Run did not panic", name)
				}
			}()
			register()
		})
	}
}
