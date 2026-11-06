package pglisten

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// hubProbe records what the callbacks of a Hub under test saw.
type hubProbe struct {
	connected    chan struct{}
	disconnected chan struct{}
	mu           sync.Mutex
	conns        []*pgx.Conn
}

func newHubProbe() *hubProbe {
	return &hubProbe{connected: make(chan struct{}, 16), disconnected: make(chan struct{}, 16)}
}

// hub is a Hub on dial whose OnConnected and OnDisconnected report to the probe, and which records
// every connection dial returned.
func (p *hubProbe) hub(dial func(context.Context) (*pgx.Conn, error)) *Hub {
	hub := &Hub{Owner: "pglisten hub test", Dial: func(ctx context.Context) (*pgx.Conn, error) {
		conn, err := dial(ctx)
		if err == nil {
			p.mu.Lock()
			p.conns = append(p.conns, conn)
			p.mu.Unlock()
		}
		return conn, err
	}}
	hub.OnConnected(func() { p.connected <- struct{}{} })
	hub.OnDisconnected(func() { p.disconnected <- struct{}{} })
	return hub
}

func (p *hubProbe) lastConn(t *testing.T) *pgx.Conn {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.conns) == 0 {
		t.Fatal("the hub has dialed no connection")
	}
	return p.conns[len(p.conns)-1]
}

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

// runHub runs hub until the returned stop is called or the test ends; stop returns once Run has.
func runHub(t *testing.T, hub *Hub) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		hub.Run(ctx)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("Run did not return within 10s of its context ending")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// recv waits for one value on ch.
func recv[T any](t *testing.T, ch <-chan T, when string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("%s: nothing arrived within 5s", when)
		var zero T
		return zero
	}
}

// quiet fails if anything arrives on ch within a short wait.
func quiet[T any](t *testing.T, ch <-chan T, when string) {
	t.Helper()
	select {
	case v := <-ch:
		t.Fatalf("%s: got %v, want nothing", when, v)
	case <-time.After(200 * time.Millisecond):
	}
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

// One connection carries every registered channel. Each notification reaches every handler and
// every wake registered on its channel, and no other channel's; each wake is also signalled after
// the LISTEN, so a caller that catches up on its wake catches up on its first connection too.
func TestTheHubDeliversEachChannelToEveryHandlerAndWakeRegisteredOnIt(t *testing.T) {
	database := storetest.Open(t)
	probe := newHubProbe()
	hub := probe.hub(database.Pool.Listener)
	first, second, other := make(chan string, 4), make(chan string, 4), make(chan string, 4)
	hub.Handle("hub_test_a", func(payload string) { first <- payload })
	hub.Handle("hub_test_a", func(payload string) { second <- payload })
	wakeA := hub.Wake("hub_test_a")
	wakeAAgain := hub.Wake("hub_test_a")
	hub.Handle("hub_test_b", func(payload string) { other <- payload })
	wakeB := hub.Wake("hub_test_b")
	runHub(t, hub)

	recv(t, probe.connected, "the first LISTEN")
	for name, wake := range map[string]<-chan struct{}{"a": wakeA, "a again": wakeAAgain, "b": wakeB} {
		recv(t, wake, "wake "+name+" after the first LISTEN")
	}
	probe.mu.Lock()
	dialed := len(probe.conns)
	probe.mu.Unlock()
	if dialed != 1 {
		t.Fatalf("the hub dialed %d connections for its two channels, want one", dialed)
	}

	notifyChannel(t, database, "hub_test_a", "on a")
	if got := recv(t, first, "the first handler of a"); got != "on a" {
		t.Fatalf("the first handler of a heard %q, want %q", got, "on a")
	}
	if got := recv(t, second, "the second handler of a"); got != "on a" {
		t.Fatalf("the second handler of a heard %q, want %q", got, "on a")
	}
	recv(t, wakeA, "wake a after a notification on a")
	recv(t, wakeAAgain, "the second wake of a after a notification on a")
	quiet(t, other, "b's handler after a notification on a")
	quiet(t, wakeB, "b's wake after a notification on a")

	notifyChannel(t, database, "hub_test_b", "on b")
	if got := recv(t, other, "b's handler"); got != "on b" {
		t.Fatalf("b's handler heard %q, want %q", got, "on b")
	}
	recv(t, wakeB, "b's wake after a notification on b")
	quiet(t, first, "a's handler after a notification on b")
	quiet(t, wakeA, "a's wake after a notification on b")
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
	hub := &Hub{Owner: "test hub", Dial: dialer.dial}
	hub.backoff = func(int) time.Duration { return 10 * time.Millisecond }
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

// A wake folds the signals its caller has not read yet into one, and never blocks the hub: a
// caller that reads late still learns to look again, and the notifications behind it still reach
// their handlers.
func TestAWakeNobodyReadsFoldsItsSignalsAndBlocksNothing(t *testing.T) {
	database := storetest.Open(t)
	probe := newHubProbe()
	hub := probe.hub(database.Pool.Listener)
	wake := hub.Wake("hub_test_a")
	heard := make(chan string, 8)
	hub.Handle("hub_test_a", func(payload string) { heard <- payload })
	runHub(t, hub)
	recv(t, probe.connected, "the first LISTEN")

	for _, payload := range []string{"1", "2", "3"} {
		notifyChannel(t, database, "hub_test_a", payload)
	}
	for _, want := range []string{"1", "2", "3"} {
		if got := recv(t, heard, "notification "+want); got != want {
			t.Fatalf("the handler heard %q, want %q", got, want)
		}
	}
	recv(t, wake, "the wake")
	quiet(t, wake, "the wake once read")
}

// OnDisconnected says the connection the LISTEN stood on is gone: once when it is lost, before the
// redial whose LISTEN runs OnConnected and signals every wake again, and once when Run ends
// holding one. A process that is no longer listening learns it at once, not a backoff later.
func TestTheHubReportsALostConnectionAndItsReturn(t *testing.T) {
	database := storetest.Open(t)
	probe := newHubProbe()
	hub := probe.hub(database.Pool.Listener)
	hub.backoff = func(int) time.Duration { return time.Hour }
	wake := hub.Wake("hub_test_a")
	stop := runHub(t, hub)
	recv(t, probe.connected, "the first LISTEN")
	recv(t, wake, "the wake after the first LISTEN")
	quiet(t, probe.disconnected, "OnDisconnected while the first connection stands")

	terminateBackend(t, database, probe.lastConn(t).PgConn().PID())
	// The backoff is an hour, so the callback reached here ran before the redial's wait.
	recv(t, probe.disconnected, "OnDisconnected after the backend was killed")
	quiet(t, probe.connected, "OnConnected during the redial's backoff")
	stop()
	quiet(t, probe.disconnected, "OnDisconnected when Run ends with no connection")

	probe = newHubProbe()
	hub = probe.hub(database.Pool.Listener)
	hub.backoff = func(int) time.Duration { return 5 * time.Millisecond }
	wake = hub.Wake("hub_test_a")
	stop = runHub(t, hub)
	recv(t, probe.connected, "the first LISTEN of the second hub")
	recv(t, wake, "the second hub's wake after its first LISTEN")
	terminateBackend(t, database, probe.lastConn(t).PgConn().PID())
	recv(t, probe.disconnected, "OnDisconnected after the second hub's backend was killed")
	recv(t, probe.connected, "OnConnected after the redial")
	recv(t, wake, "the wake after the redial's LISTEN")
	stop()
	recv(t, probe.disconnected, "OnDisconnected when Run ends holding a connection")
	quiet(t, probe.disconnected, "a second OnDisconnected for one connection")
}

// A hub that never gets a LISTEN to stand has no connection to lose, so a hub whose every dial
// fails reports neither a connection nor a disconnection, and runs no handler.
func TestAHubThatNeverListenedReportsNoDisconnection(t *testing.T) {
	var dials atomic.Int32
	probe := newHubProbe()
	hub := probe.hub(func(context.Context) (*pgx.Conn, error) {
		dials.Add(1)
		return nil, errors.New("the database is down")
	})
	hub.backoff = func(int) time.Duration { return time.Millisecond }
	hub.Handle("hub_test_a", func(string) { t.Error("a handler ran on a hub with no connection") })
	stop := runHub(t, hub)
	deadline := time.Now().Add(5 * time.Second)
	for dials.Load() < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("the hub dialed %d times in 5s, want it redialing", dials.Load())
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	quiet(t, probe.connected, "OnConnected with no connection")
	quiet(t, probe.disconnected, "OnDisconnected with no connection")
}

// Registration is closed once Run starts: a channel registered late would never be listened on,
// so it panics rather than drop that channel's notifications without a word. Run twice panics too.
func TestARegistrationAfterRunStartsPanics(t *testing.T) {
	dialed := make(chan struct{}, 1)
	hub := &Hub{Dial: func(ctx context.Context) (*pgx.Conn, error) {
		select {
		case dialed <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	hub.Handle("hub_test_a", func(string) {})
	runHub(t, hub)
	recv(t, dialed, "the hub's first dial")

	for name, register := range map[string]func(){
		"Handle":         func() { hub.Handle("hub_test_b", func(string) {}) },
		"Wake":           func() { hub.Wake("hub_test_b") },
		"OnConnected":    func() { hub.OnConnected(func() {}) },
		"OnDisconnected": func() { hub.OnDisconnected(func() {}) },
		"Run":            func() { hub.Run(context.Background()) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				recovered := recover()
				message, _ := recovered.(string)
				if !strings.Contains(message, "Hub."+name) {
					t.Fatalf("%s after Run started recovered %v, want a panic naming Hub.%s", name, recovered, name)
				}
			}()
			register()
		})
	}
}

// Registration is closed once Run has started, for a hub that registered no channel too: it opens
// nothing and waits for its context, and a channel added after that would be missing from a LISTEN
// statement no later Run sends.
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
