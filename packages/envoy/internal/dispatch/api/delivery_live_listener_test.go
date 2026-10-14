package api

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// listenerNote is one notification a listener under test reported.
type listenerNote struct{ channel, payload string }

// listenerProbe records what a deliveryListener under test does: each dial, each connection a
// dial returned, each onConnected and each notification.
type listenerProbe struct {
	dials     atomic.Int32
	connected chan struct{}
	notes     chan listenerNote
	stop      chan struct{}
	mu        sync.Mutex
	conns     []*pgx.Conn
}

func newListenerProbe() *listenerProbe {
	return &listenerProbe{connected: make(chan struct{}, 16), notes: make(chan listenerNote, 64), stop: make(chan struct{})}
}

// listener is a deliveryListener that dials with dial and reports to the probe, at its defaults
// otherwise.
func (p *listenerProbe) listener(dial func(context.Context) (*pgx.Conn, error)) *deliveryListener {
	return &deliveryListener{
		dial: func(ctx context.Context) (*pgx.Conn, error) {
			p.dials.Add(1)
			conn, err := dial(ctx)
			if err == nil {
				p.mu.Lock()
				p.conns = append(p.conns, conn)
				p.mu.Unlock()
			}
			return conn, err
		},
		onConnected: func() {
			select {
			case p.connected <- struct{}{}:
			case <-p.stop:
			}
		},
		onNotify: func(channel, payload string) {
			select {
			case p.notes <- listenerNote{channel: channel, payload: payload}:
			case <-p.stop:
			}
		},
	}
}

// lastConn is the connection the listener's latest successful dial returned.
func (p *listenerProbe) lastConn(t *testing.T) *pgx.Conn {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.conns) == 0 {
		t.Fatal("the listener has dialed no connection")
	}
	return p.conns[len(p.conns)-1]
}

func (p *listenerProbe) waitConnected(t *testing.T, within time.Duration, when string) {
	t.Helper()
	select {
	case <-p.connected:
	case <-time.After(within):
		t.Fatalf("%s: onConnected did not fire within %v (%d dials)", when, within, p.dials.Load())
	}
}

func (p *listenerProbe) waitNote(t *testing.T, within time.Duration, when string) listenerNote {
	t.Helper()
	select {
	case note := <-p.notes:
		return note
	case <-time.After(within):
		t.Fatalf("%s: onNotify was not called within %v", when, within)
		return listenerNote{}
	}
}

// runDeliveryListener runs listener until the returned cancel is called or the test ends; done
// closes once run has returned. The test's cleanup fails it when run does not return within ten
// seconds of its context ending.
func runDeliveryListener(t *testing.T, probe *listenerProbe, listener *deliveryListener) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		listener.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		close(probe.stop)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("run did not return within 10s of its context ending")
		}
	})
	return cancel, done
}

func notifyChannel(t *testing.T, database *store.Store, channel, payload string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `select pg_notify($1, $2)`, channel, payload); err != nil {
		t.Fatalf("pg_notify(%q, %q): %v", channel, payload, err)
	}
}

func terminateBackend(t *testing.T, database *store.Store, pid uint32) {
	t.Helper()
	var terminated bool
	if err := database.Pool.QueryRow(context.Background(), `select pg_terminate_backend($1)`, int(pid)).Scan(&terminated); err != nil || !terminated {
		t.Fatalf("pg_terminate_backend(%d) = %v, %v; want true", pid, terminated, err)
	}
}

// waitBackendGone waits until Postgres no longer lists the backend pid.
func waitBackendGone(t *testing.T, database *store.Store, pid uint32, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var count int
		if err := database.Pool.QueryRow(context.Background(), `select count(*) from pg_stat_activity where pid = $1`, int(pid)).Scan(&count); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if count == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("backend %d is still connected %v after the listener returned", pid, within)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestTheListenerReconnectsAfterItsBackendIsKilled(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	runDeliveryListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, 5*time.Second, "first connection")
	killed := probe.lastConn(t).PgConn().PID()

	terminateBackend(t, database, killed)
	probe.waitConnected(t, 5*time.Second, "after its backend was killed")
	if dials := probe.dials.Load(); dials < 2 {
		t.Fatalf("dials after the kill = %d, want a second", dials)
	}
	if pid := probe.lastConn(t).PgConn().PID(); pid == killed {
		t.Fatalf("the listener reconnected on backend %d, the one that was killed", pid)
	}

	notifyChannel(t, database, "delivery_changes", "")
	if note := probe.waitNote(t, 5*time.Second, "a notification after the reconnect"); note != (listenerNote{channel: "delivery_changes"}) {
		t.Fatalf("notification after the reconnect = %+v, want delivery_changes with an empty payload", note)
	}
}

// blindingProxy forwards each TCP connection it accepts to target until blind is called. From then
// on the connections it already holds forward nothing, and neither side of them is closed, so each
// looks open to both ends and stays silent: a half-open connection. A connection it accepts after
// blind forwards as usual.
type blindingProxy struct {
	listener net.Listener
	target   string
	closed   chan struct{}
	mu       sync.Mutex
	shut     bool
	gates    []*atomic.Bool
	conns    []net.Conn
}

func newBlindingProxy(t *testing.T, target string) *blindingProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the proxy: %v", err)
	}
	proxy := &blindingProxy{listener: listener, target: target, closed: make(chan struct{})}
	go proxy.serve()
	t.Cleanup(proxy.close)
	return proxy
}

func (p *blindingProxy) addr() string { return p.listener.Addr().String() }

func (p *blindingProxy) serve() {
	for {
		client, err := p.listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		gate := &atomic.Bool{}
		p.mu.Lock()
		if p.shut {
			p.mu.Unlock()
			_ = client.Close()
			_ = server.Close()
			return
		}
		p.gates = append(p.gates, gate)
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(gatedWriter{to: server, gate: gate, closed: p.closed}, client) }()
		go func() { _, _ = io.Copy(gatedWriter{to: client, gate: gate, closed: p.closed}, server) }()
	}
}

// blind stops every connection the proxy holds from forwarding, closing neither side.
func (p *blindingProxy) blind() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, gate := range p.gates {
		gate.Store(true)
	}
}

func (p *blindingProxy) close() {
	p.mu.Lock()
	p.shut = true
	conns := p.conns
	p.mu.Unlock()
	close(p.closed)
	_ = p.listener.Close()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// gatedWriter forwards writes until its gate is set, then holds every write until the proxy closes.
type gatedWriter struct {
	to     io.Writer
	gate   *atomic.Bool
	closed <-chan struct{}
}

func (w gatedWriter) Write(b []byte) (int, error) {
	if w.gate.Load() {
		<-w.closed
		return 0, net.ErrClosed
	}
	return w.to.Write(b)
}

// proxiedDial dials the test database through proxy.
func proxiedDial(database *store.Store, proxy *blindingProxy) func(context.Context) (*pgx.Conn, error) {
	config := database.Pool.Config().ConnConfig
	config.DialFunc = func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "tcp", proxy.addr())
	}
	return func(ctx context.Context) (*pgx.Conn, error) { return pgx.ConnectConfig(ctx, config) }
}

func databaseAddress(database *store.Store) string {
	config := database.Pool.Config().ConnConfig
	return net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port)))
}

// A healthy connection the listener has been idle on for several idle periods is pinged, not
// replaced: one dial, one onConnected, the backend's last statement the ping, and a notification
// still reaches onNotify on it.
func TestTheListenerKeepsAHealthyIdleConnection(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	listener.listenIdle, listener.pingTimeout = 200*time.Millisecond, time.Second
	runDeliveryListener(t, probe, listener)
	probe.waitConnected(t, 5*time.Second, "first connection")
	pid := probe.lastConn(t).PgConn().PID()

	time.Sleep(1500 * time.Millisecond)
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials after seven idle periods = %d, want 1", dials)
	}
	select {
	case <-probe.connected:
		t.Fatal("onConnected fired again on an idle, healthy connection")
	default:
	}
	var query string
	if err := database.Pool.QueryRow(context.Background(), `select query from pg_stat_activity where pid = $1`, int(pid)).Scan(&query); err != nil {
		t.Fatalf("read the listener's backend: %v", err)
	}
	if query != "-- ping" {
		t.Fatalf("the idle listener's last statement = %q, want its ping", query)
	}
	notifyChannel(t, database, "dispatch_events", "")
	if note := probe.waitNote(t, 5*time.Second, "a notification after the idle periods"); note != (listenerNote{channel: "dispatch_events"}) {
		t.Fatalf("notification = %+v, want dispatch_events with an empty payload", note)
	}
}

func TestTheListenerReplacesAHalfOpenConnection(t *testing.T) {
	database := storetest.Open(t)
	proxy := newBlindingProxy(t, databaseAddress(database))
	probe := newListenerProbe()
	listener := probe.listener(proxiedDial(database, proxy))
	listener.listenIdle, listener.pingTimeout = time.Second, time.Second
	runDeliveryListener(t, probe, listener)
	probe.waitConnected(t, 5*time.Second, "first connection")
	notifyChannel(t, database, "delivery_changes", "")
	if note := probe.waitNote(t, 5*time.Second, "a notification through the proxy"); note != (listenerNote{channel: "delivery_changes"}) {
		t.Fatalf("notification through the proxy = %+v, want delivery_changes with an empty payload", note)
	}

	proxy.blind()
	probe.waitConnected(t, 5*time.Second, "after its connection went half-open")
	if dials := probe.dials.Load(); dials < 2 {
		t.Fatalf("dials after the connection went half-open = %d, want a second", dials)
	}
	notifyChannel(t, database, "delivery_changes", "")
	if note := probe.waitNote(t, 5*time.Second, "a notification on the replacement"); note != (listenerNote{channel: "delivery_changes"}) {
		t.Fatalf("notification on the replacement = %+v, want delivery_changes with an empty payload", note)
	}
}

// onNotify gets each of the three channels' payloads exactly as sent, in the order sent, and
// nothing from a channel the listener does not listen on.
func TestTheListenerPassesEachPayloadUnchanged(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	runDeliveryListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, 5*time.Second, "first connection")

	freshness := `{"last_event_at" : "2026-10-12T15:04:05.123456+00:00", "last_reconcile_at" : null, "last_error" : "GitHub answered 502: \"bad gateway\" (réessayer)"}`
	sent := []listenerNote{
		{channel: "delivery_changes", payload: ""},
		{channel: "delivery_freshness", payload: freshness},
		{channel: "dispatch_events", payload: ""},
	}
	notifyChannel(t, database, "delivery_unheard", "never reported")
	for _, note := range sent {
		notifyChannel(t, database, note.channel, note.payload)
	}
	for index, want := range sent {
		if got := probe.waitNote(t, 5*time.Second, "notification "+strconv.Itoa(index)); got != want {
			t.Fatalf("notification %d = %+v, want %+v", index, got, want)
		}
	}
	if !json.Valid([]byte(freshness)) {
		t.Fatal("the freshness payload sent is not JSON")
	}
	select {
	case note := <-probe.notes:
		t.Fatalf("an extra notification reached onNotify: %+v", note)
	case <-time.After(300 * time.Millisecond):
	}
}

// The listener's three channels are the ones Dispatch's own writers notify: a settings write
// notifies delivery_changes and delivery_freshness (migration 0100) and an event insert
// dispatch_events (migration 0107).
func TestTheListenerHearsTheTriggersItListensFor(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	runDeliveryListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, 5*time.Second, "first connection")
	ctx := context.Background()

	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobName: "widgets-release / widgets-release", PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
		PopulationAuthors: []string{"octocat"},
	}, model.Actor{Kind: "system", ID: "test"}); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}
	for _, statement := range []string{
		`insert into projects (key, name) values ('LIVE', 'Live')`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('LIVE-1', 'LIVE', 1, 'x', '{"kind":"user","id":"alice"}', 'U')`,
		`insert into events (issue_key, seq, type, actor, payload, notify) values ('LIVE-1', 1, 'issue.created', '{"kind":"user","id":"alice"}', '{}', false)`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	heard := map[string]string{}
	for len(heard) < 3 {
		note := probe.waitNote(t, 5*time.Second, "the triggers' notifications")
		heard[note.channel] = note.payload
	}
	if heard["delivery_changes"] != "" || heard["dispatch_events"] != "" {
		t.Fatalf("delivery_changes and dispatch_events payloads = %q, %q; want both empty", heard["delivery_changes"], heard["dispatch_events"])
	}
	var freshness map[string]any
	if err := json.Unmarshal([]byte(heard["delivery_freshness"]), &freshness); err != nil {
		t.Fatalf("delivery_freshness payload %q is not JSON: %v", heard["delivery_freshness"], err)
	}
	for _, key := range []string{"last_event_at", "last_reconcile_at", "last_error"} {
		if _, ok := freshness[key]; !ok {
			t.Fatalf("delivery_freshness payload %q lacks %q", heard["delivery_freshness"], key)
		}
	}
}

func TestTheListenerReturnsWithItsConnectionClosedWhenItsContextEnds(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	cancel, done := runDeliveryListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, 5*time.Second, "first connection")
	conn := probe.lastConn(t)
	pid := conn.PgConn().PID()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not return within 2s of its context ending")
	}
	if !conn.IsClosed() {
		t.Fatal("run returned with its connection open")
	}
	waitBackendGone(t, database, pid, 3*time.Second)
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials = %d, want 1", dials)
	}
}

func TestTheListenerReturnsWhileBackingOffWhenItsContextEnds(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	backingOff := make(chan int, 4)
	listener.backoff = func(attempt int) time.Duration {
		backingOff <- attempt
		return time.Hour
	}
	cancel, done := runDeliveryListener(t, probe, listener)
	probe.waitConnected(t, 5*time.Second, "first connection")
	conn := probe.lastConn(t)
	terminateBackend(t, database, conn.PgConn().PID())
	select {
	case attempt := <-backingOff:
		if attempt != 1 {
			t.Fatalf("the first backoff after a lost connection is attempt %d, want 1", attempt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the listener did not back off within 5s of its backend being killed")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("run did not return within 1s of its context ending while it backed off")
	}
	if !conn.IsClosed() {
		t.Fatal("run returned with its lost connection open")
	}
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials = %d, want 1: the backoff was cut short by a redial", dials)
	}
}

// The default backoff waits a second, doubling to thirty, and adds up to half of that again.
func TestTheListenerBacksOffFromOneSecondToThirtyWithJitter(t *testing.T) {
	for attempt, base := range map[int]time.Duration{
		1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 5: 16 * time.Second,
		6: 30 * time.Second, 7: 30 * time.Second, 50: 30 * time.Second,
	} {
		for range 200 {
			if wait := deliveryListenerBackoff(attempt); wait < base || wait > base+base/2 {
				t.Fatalf("deliveryListenerBackoff(%d) = %v, want within [%v, %v]", attempt, wait, base, base+base/2)
			}
		}
	}
	waits := map[time.Duration]bool{}
	for range 50 {
		waits[deliveryListenerBackoff(1)] = true
	}
	if len(waits) < 2 {
		t.Fatalf("deliveryListenerBackoff(1) returned %v every time, want jitter", waits)
	}
}
