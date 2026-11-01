package pglisten

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
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

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// testChannels are the channels the listener under test listens on: the delivery hub's three, the
// one deployment where three channels share a connection (api/delivery_live_hub.go).
var testChannels = []string{"delivery_changes", "delivery_freshness", "dispatch_events"}

// listenerNote is one notification a listener under test reported.
type listenerNote struct{ channel, payload string }

// listenerProbe records what a Listener under test does: each dial, each connection a dial
// returned, each OnConnected and each notification.
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

// listener is a Listener that dials with dial and reports to the probe, at its defaults
// otherwise.
func (p *listenerProbe) listener(dial func(context.Context) (*pgx.Conn, error)) *Listener {
	return &Listener{
		Dial:     p.dialing(dial),
		Channels: testChannels,
		OnConnected: func() {
			select {
			case p.connected <- struct{}{}:
			case <-p.stop:
			}
		},
		OnNotify: p.notify,
	}
}

// dialing is dial, counted and recorded by the probe.
func (p *listenerProbe) dialing(dial func(context.Context) (*pgx.Conn, error)) func(context.Context) (*pgx.Conn, error) {
	return func(ctx context.Context) (*pgx.Conn, error) {
		p.dials.Add(1)
		conn, err := dial(ctx)
		if err == nil {
			p.mu.Lock()
			p.conns = append(p.conns, conn)
			p.mu.Unlock()
		}
		return conn, err
	}
}

func (p *listenerProbe) notify(channel, payload string) {
	select {
	case p.notes <- listenerNote{channel: channel, payload: payload}:
	case <-p.stop:
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
		t.Fatalf("%s: OnConnected did not fire within %v (%d dials)", when, within, p.dials.Load())
	}
}

func (p *listenerProbe) waitNote(t *testing.T, within time.Duration, when string) listenerNote {
	t.Helper()
	select {
	case note := <-p.notes:
		return note
	case <-time.After(within):
		t.Fatalf("%s: OnNotify was not called within %v", when, within)
		return listenerNote{}
	}
}

// runListener runs listener until the returned cancel is called or the test ends; done closes
// once Run has returned. The test's cleanup fails it when Run does not return within ten seconds
// of its context ending.
func runListener(t *testing.T, probe *listenerProbe, listener *Listener) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		listener.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		close(probe.stop)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("Run did not return within 10s of its context ending")
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
	runListener(t, probe, probe.listener(database.Pool.Listener))
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
// replaced: one dial, one OnConnected, the backend's last statement the ping, and a notification
// still reaches OnNotify on it.
func TestTheListenerKeepsAHealthyIdleConnection(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	listener.ListenIdle, listener.PingTimeout = 200*time.Millisecond, time.Second
	runListener(t, probe, listener)
	probe.waitConnected(t, 5*time.Second, "first connection")
	pid := probe.lastConn(t).PgConn().PID()

	time.Sleep(1500 * time.Millisecond)
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials after seven idle periods = %d, want 1", dials)
	}
	select {
	case <-probe.connected:
		t.Fatal("OnConnected fired again on an idle, healthy connection")
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
	listener.ListenIdle, listener.PingTimeout = time.Second, time.Second
	runListener(t, probe, listener)
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

// OnNotify gets each of the listener's three channels' payloads exactly as sent, in the order
// sent, and nothing from a channel the listener does not listen on.
func TestTheListenerPassesEachPayloadUnchanged(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	runListener(t, probe, probe.listener(database.Pool.Listener))
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
		t.Fatalf("an extra notification reached OnNotify: %+v", note)
	case <-time.After(300 * time.Millisecond):
	}
}

// A listener listens on the channels it names and no others, and needs no OnConnected: that is
// the document rooms' evictor, which listens on one channel and catches nothing up (docs).
func TestAListenerOnOneChannelWithoutAnOnConnectedHearsThatChannelAlone(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := &Listener{
		Dial:     probe.dialing(database.Pool.Listener),
		Channels: []string{"dispatch_doc_changed"},
		OnNotify: probe.notify,
	}
	runListener(t, probe, listener)

	// The LISTEN has to be in place before a notification can reach it, and this listener reports
	// no connection: a notification repeated until one arrives is what a test without OnConnected
	// has. Every repeat is delivered once the LISTEN stands, so the first note is the first that
	// found it.
	go func() {
		for {
			select {
			case <-probe.stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
			if _, err := database.Pool.Exec(context.Background(), `select pg_notify('dispatch_doc_changed', 'artifact-1')`); err != nil {
				return
			}
		}
	}()
	if note := probe.waitNote(t, 10*time.Second, "a notification on the one channel"); note != (listenerNote{channel: "dispatch_doc_changed", payload: "artifact-1"}) {
		t.Fatalf("notification = %+v, want dispatch_doc_changed carrying artifact-1", note)
	}

	notifyChannel(t, database, "delivery_changes", "")
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case note := <-probe.notes:
			if note.channel != "dispatch_doc_changed" {
				t.Fatalf("a channel the listener does not name reached OnNotify: %+v", note)
			}
		case <-deadline:
			return
		}
	}
}

// The channels Dispatch's own writers notify reach a listener that names them: a settings write
// notifies delivery_changes and delivery_freshness (migration 0100) and an event insert
// dispatch_events (migration 0107).
func TestTheListenerHearsTheTriggersItListensFor(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	runListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, 5*time.Second, "first connection")
	ctx := context.Background()

	if _, err := delivery.PutSettings(ctx, database.Pool, delivery.DeliverySettings{
		DeployRepo: "acme/widgets", DeployWorkflowPath: ".github/workflows/deploy.yml",
		ProductionJobNames: []string{"widgets-release / widgets-release"}, PRChecksWorkflowPath: ".github/workflows/pr-checks.yml",
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
	cancel, done := runListener(t, probe, probe.listener(database.Pool.Listener))
	probe.waitConnected(t, 5*time.Second, "first connection")
	conn := probe.lastConn(t)
	pid := conn.PgConn().PID()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of its context ending")
	}
	if !conn.IsClosed() {
		t.Fatal("Run returned with its connection open")
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
	listener.Backoff = func(attempt int) time.Duration {
		backingOff <- attempt
		return time.Hour
	}
	cancel, done := runListener(t, probe, listener)
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
		t.Fatal("Run did not return within 1s of its context ending while it backed off")
	}
	if !conn.IsClosed() {
		t.Fatal("Run returned with its lost connection open")
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
			if wait := RedialBackoff(attempt); wait < base || wait > base+base/2 {
				t.Fatalf("RedialBackoff(%d) = %v, want within [%v, %v]", attempt, wait, base, base+base/2)
			}
		}
	}
	waits := map[time.Duration]bool{}
	for range 50 {
		waits[RedialBackoff(1)] = true
	}
	if len(waits) < 2 {
		t.Fatalf("RedialBackoff(1) returned %v every time, want jitter", waits)
	}
}

// A caller whose wake is sooner than its idle wait is woken then, on the listener's own
// goroutine, and nothing is pinged for it here: the ping falls due on the time since the
// connection last proved a round trip (listener.go), and the three wakes below are over well
// inside one ListenIdle of the LISTEN that proved this one, so none is owed. Wakes that run on
// until that point are pinged all the same, which is the row below this one. That caller is the
// document rooms' evictor, whose work has to stay on this goroutine (docs/stale_room.go). A
// notification that arrives first is still reported.
func TestTheListenerWakesItsCallerBeforeItsIdleWait(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	// An idle wait far longer than the wake and than this test's own waits: what ends each wait
	// below can only be the wake, or the notification at the end.
	listener.ListenIdle, listener.PingTimeout = 10*time.Second, time.Second
	const wake = 100 * time.Millisecond
	wakes := make(chan time.Time, 16)
	listener.NextWake = func() time.Duration { return wake }
	listener.OnWake = func() {
		select {
		case wakes <- time.Now():
		default:
		}
	}
	runListener(t, probe, listener)
	probe.waitConnected(t, 5*time.Second, "first connection")
	pid := probe.lastConn(t).PgConn().PID()

	started := time.Now()
	for round := range 3 {
		select {
		case <-wakes:
		case <-time.After(5 * time.Second):
			t.Fatalf("wake %d did not come within 5s of a %v wake", round+1, wake)
		}
	}
	if waited := time.Since(started); waited < 3*wake {
		t.Fatalf("three %v wakes came in %v, so the listener is not waiting the wake out", wake, waited)
	} else if waited >= listener.ListenIdle {
		t.Fatalf("three %v wakes took %v, as long as the idle wait (%v): the wake is not what ends these waits", wake, waited, listener.ListenIdle)
	}
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials over three wakes = %d, want 1", dials)
	}
	var query string
	if err := database.Pool.QueryRow(context.Background(), `select query from pg_stat_activity where pid = $1`, int(pid)).Scan(&query); err != nil {
		t.Fatalf("read the listener's backend: %v", err)
	}
	if query == "-- ping" {
		t.Fatal("the listener pinged within one idle period of the LISTEN that proved this connection: the ping falls due on the time since the last proven round trip, which these wakes are short of")
	}

	notifyChannel(t, database, "dispatch_events", "woken")
	if note := probe.waitNote(t, 5*time.Second, "a notification inside a wake's wait"); note != (listenerNote{channel: "dispatch_events", payload: "woken"}) {
		t.Fatalf("notification inside a wake's wait = %+v, want dispatch_events with payload woken", note)
	}
}

// A caller whose wake is always sooner than the idle period is pinged all the same: the ping
// hangs on the time since this connection last proved a round trip, not on what ended the last
// wait, so waitFor cuts such a wake short at the moment the ping falls due and listen pings there
// (listener.go). That shape is the evictor's own in steady state - wakeIn is the minimum over
// every deferred room, and rooms entering at their own times keep that minimum under the idle
// period for ever, whatever ceiling each room's re-arm has (docs/stale_room.go) - and before the
// stamp the listener took its wake branch for ever, so a connection that had gone half-open sat
// in the loop silent with nothing left to find it.
func TestAListenerWokenShorterThanItsIdlePeriodIsStillPinged(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	listener.ListenIdle, listener.PingTimeout = 300*time.Millisecond, time.Second
	// Sooner than the idle period every time it is asked, and never zero: that is what wakeIn
	// returns once three rooms twenty seconds apart are each armed on their own window.
	const wake = 80 * time.Millisecond
	var wakes atomic.Int32
	listener.NextWake = func() time.Duration { return wake }
	listener.OnWake = func() { wakes.Add(1) }
	runListener(t, probe, listener)
	probe.waitConnected(t, 5*time.Second, "first connection")
	pid := probe.lastConn(t).PgConn().PID()

	// The ping is due one idle period after the LISTEN; ten of them to find it, so a loaded
	// machine that runs the wakes late still passes while a listener that never pings fails.
	started := time.Now()
	deadline := started.Add(10 * listener.ListenIdle)
	var query string
	for {
		if err := database.Pool.QueryRow(context.Background(), `select query from pg_stat_activity where pid = $1`, int(pid)).Scan(&query); err != nil {
			t.Fatalf("read the listener's backend: %v", err)
		}
		if query == "-- ping" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the listener's last statement after %v of wakes %v apart = %q, want its ping: a wake is a local timer and proves nothing about the socket, so it must not hold the ping off", time.Since(started), wake, query)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if woken := wakes.Load(); woken < 2 {
		t.Fatalf("wakes before the ping = %d, want at least two: this row has to be a listener taking its wake branch, not one ignoring NextWake", woken)
	}
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials = %d, want 1: the ping was owed on the connection the listener already had", dials)
	}
	notifyChannel(t, database, "dispatch_events", "pinged")
	if note := probe.waitNote(t, 5*time.Second, "a notification after the ping"); note != (listenerNote{channel: "dispatch_events", payload: "pinged"}) {
		t.Fatalf("notification after the ping = %+v, want dispatch_events with payload pinged", note)
	}
}

// A caller that sets NextWake and leaves OnWake nil is not woken, and the listener lives: the
// pair is absent unless both are set (waitFor), so the wake is not even asked for, the wait
// stays the idle period, and the ping that belongs to that wait still runs. Without the pairing
// the first wait NextWake ended called a nil func, and the panic took the whole process with it
// - the delivery hub's listener and the event stream relay's included, since they run in it.
func TestAListenerWithNoOnWakeIsNotWokenAndDoesNotPanic(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	listener.ListenIdle, listener.PingTimeout = 200*time.Millisecond, time.Second
	// Far sooner than the idle period, so a listener that honoured it would end every wait on it
	// and the backend's last statement below would be the LISTEN, not the ping.
	listener.NextWake = func() time.Duration { return time.Millisecond }
	runListener(t, probe, listener)
	probe.waitConnected(t, 5*time.Second, "first connection")
	pid := probe.lastConn(t).PgConn().PID()

	time.Sleep(1500 * time.Millisecond)
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials over seven idle periods = %d, want 1", dials)
	}
	var query string
	if err := database.Pool.QueryRow(context.Background(), `select query from pg_stat_activity where pid = $1`, int(pid)).Scan(&query); err != nil {
		t.Fatalf("read the listener's backend: %v", err)
	}
	if query != "-- ping" {
		t.Fatalf("the listener's last statement = %q, want its idle ping: a wake nothing can run must not take the wait the ping belongs to", query)
	}
	notifyChannel(t, database, "dispatch_events", "heard")
	if note := probe.waitNote(t, 5*time.Second, "a notification to a listener with no OnWake"); note != (listenerNote{channel: "dispatch_events", payload: "heard"}) {
		t.Fatalf("notification = %+v, want dispatch_events with payload heard", note)
	}
}

// A listener that names channels and leaves OnNotify nil drops what arrives on them and lives:
// OnNotify is nil-checked where OnConnected is, so the caller loses its notifications and
// nothing else - the connection it heard them on is still the one it waits and pings on.
// Without the check the first notification called a nil func, and the panic took the whole
// process with it - the delivery hub's listener and the event stream relay's included, since
// they run in it.
func TestAListenerWithNoOnNotifyDropsItsNotificationsAndDoesNotPanic(t *testing.T) {
	database := storetest.Open(t)
	probe := newListenerProbe()
	listener := probe.listener(database.Pool.Listener)
	listener.OnNotify = nil
	listener.ListenIdle, listener.PingTimeout = 200*time.Millisecond, time.Second
	runListener(t, probe, listener)
	probe.waitConnected(t, 5*time.Second, "first connection")
	pid := probe.lastConn(t).PgConn().PID()

	notifyChannel(t, database, "dispatch_events", "dropped")
	// Long enough for the notification to arrive and for the wait after it to end on the idle
	// period and be pinged.
	time.Sleep(500 * time.Millisecond)
	if dials := probe.dials.Load(); dials != 1 {
		t.Fatalf("dials after a notification the listener has nowhere to report = %d, want 1: the notification cost it the connection", dials)
	}
	var query string
	if err := database.Pool.QueryRow(context.Background(), `select query from pg_stat_activity where pid = $1`, int(pid)).Scan(&query); err != nil {
		t.Fatalf("read the listener's backend: %v", err)
	}
	if query != "-- ping" {
		t.Fatalf("the listener's last statement = %q, want its idle ping: a notification it has nowhere to report must leave it waiting on the same connection", query)
	}
}
