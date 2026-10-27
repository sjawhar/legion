package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// latencyProxy relays TCP connections to target, holding every chunk read from either side for
// half of rtt before writing it on, in the order it was read: a connection through it sees rtt
// added to each round trip and nothing else, as Dispatch's connection to a remote NATS hub does.
type latencyProxy struct {
	listener net.Listener
	target   string
	delay    time.Duration

	mu    sync.Mutex
	conns []net.Conn
}

// startLatencyProxy relays to target (host:port) with rtt added to every round trip, and returns
// the NATS URL of its own address. It closes when t ends.
func startLatencyProxy(t *testing.T, target string, rtt time.Duration) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the latency proxy: %v", err)
	}
	proxy := &latencyProxy{listener: listener, target: target, delay: rtt / 2}
	go proxy.serve(listener)
	t.Cleanup(proxy.close)
	return "nats://" + listener.Addr().String()
}

// startOutageProxy relays to target (host:port) with rtt added to every round trip, until down
// cuts every connection through it and refuses new ones; up listens on the same address again. It
// returns the proxy and the NATS URL of its address, and closes when t ends.
func startOutageProxy(t *testing.T, target string, rtt time.Duration) (*latencyProxy, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the outage proxy: %v", err)
	}
	proxy := &latencyProxy{listener: listener, target: target, delay: rtt / 2}
	go proxy.serve(listener)
	t.Cleanup(proxy.close)
	return proxy, "nats://" + listener.Addr().String()
}

// down cuts every connection through the proxy and stops listening.
func (p *latencyProxy) down() { p.close() }

// up listens on the proxy's address again.
func (p *latencyProxy) up(t *testing.T) {
	t.Helper()
	listener, err := net.Listen("tcp", p.listener.Addr().String())
	if err != nil {
		t.Fatalf("listen again: %v", err)
	}
	p.mu.Lock()
	p.listener = listener
	p.mu.Unlock()
	go p.serve(listener)
}

func (p *latencyProxy) serve(listener net.Listener) {
	for {
		client, err := listener.Accept()
		if err != nil {
			return
		}
		server, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = client.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, client, server)
		p.mu.Unlock()
		go p.relay(server, client)
		go p.relay(client, server)
	}
}

// relay copies src to dst, each chunk written delay after it was read.
func (p *latencyProxy) relay(dst, src net.Conn) {
	type chunk struct {
		data []byte
		due  time.Time
	}
	queue := make(chan chunk, 4096)
	go func() {
		defer close(queue)
		buf := make([]byte, 64<<10)
		for {
			n, err := src.Read(buf)
			if n > 0 {
				queue <- chunk{data: bytes.Clone(buf[:n]), due: time.Now().Add(p.delay)}
			}
			if err != nil {
				return
			}
		}
	}()
	for c := range queue {
		time.Sleep(time.Until(c.due))
		if _, err := dst.Write(c.data); err != nil {
			break
		}
	}
	_ = dst.Close()
	_ = src.Close()
}

func (p *latencyProxy) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	_ = p.listener.Close()
	for _, conn := range p.conns {
		_ = conn.Close()
	}
	p.conns = nil
}

// busyPoolSize is the shared pool's size in the busy case. A full pool is what the case needs, of
// any size; filling one of production's store.sharedPoolSize would hold that many server
// connections while the other packages' tests share the server, past a default Postgres's
// hundred.
const busyPoolSize = 8

// busySharedPool opens a second store on database's database whose shared pool holds
// busyPoolSize connections, and keeps every one of them busy until t ends, with as many callers
// again queued behind them, the way the API's requests keep the pool the outbox used to share
// with them. The outbox pool it hands out is derived from it, as production's is.
func busySharedPool(t *testing.T, database *store.Store) *store.Store {
	t.Helper()
	config := database.Pool.Config().Copy()
	config.MaxConns = busyPoolSize
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open the busy shared pool: %v", err)
	}
	busy := &store.Store{Pool: store.NewPool(pool)}
	t.Cleanup(busy.Pool.Close)
	ctx, cancel := context.WithCancel(context.Background())
	var callers sync.WaitGroup
	for range 2 * busyPoolSize {
		callers.Go(func() {
			for ctx.Err() == nil {
				_, _ = busy.Pool.Exec(ctx, `select pg_sleep(0.01)`)
			}
		})
	}
	t.Cleanup(func() {
		cancel()
		callers.Wait()
	})
	return busy
}

// seedThroughputBacklog appends count events across issues issues, each routed to role:reviewer,
// alternating a plain human message (its issue topic and the role route) and a human reply to a
// session's message (those and the session's own topic, after a lookup of the parent's author).
// It returns the events in id order.
func seedThroughputBacklog(t *testing.T, database *store.Store, broker *events.Broker, issues, count int) []model.Event {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('TT', 'Test')`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	keys := make([]string, issues)
	parents := make([]string, issues)
	for index := range issues {
		keys[index] = fmt.Sprintf("TT-%d", index+1)
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, route, created_by, rank)
			values ($1, 'TT', $2, 'Load', 'role:reviewer', '{"kind":"user","id":"alice"}', 'U')
		`, keys[index], index+1); err != nil {
			t.Fatalf("create issue %s: %v", keys[index], err)
		}
		parents[index] = seedMessage(t, database, keys[index], model.Actor{Kind: "session", ID: fmt.Sprintf("agent-%d", index)}, "Status?", nil)
	}
	seeded := make([]model.Event, 0, count)
	for index := range count {
		key := keys[index%issues]
		message := model.Message{ID: fmt.Sprintf("message-%d", index), IssueKey: new(key), Body: "Load"}
		if index%2 == 1 {
			message.InReplyTo = new(parents[index%issues])
		}
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.MessageEventPayload{Message: message},
		}))
	}
	return seeded
}

// unpublishedCount is how many of database's events are not yet marked published.
func unpublishedCount(t *testing.T, database *store.Store) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(context.Background(),
		`select count(*) from events where published_at is null`).Scan(&count); err != nil {
		t.Fatalf("count unpublished events: %v", err)
	}
	return count
}

// streamOrder reads every message the notification stream holds, in stream order, and returns the
// dispatch event id each carries, grouped by subject, and the ids of all of them in stream order.
func streamOrder(t *testing.T, conn *natsgo.Conn) (map[string][]int64, []int64) {
	t.Helper()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	bySubject := map[string][]int64{}
	var all []int64
	for sequence := info.State.FirstSeq; sequence <= info.State.LastSeq && info.State.Msgs > 0; sequence++ {
		message, err := js.GetMsg(bus.Stream, sequence)
		if err != nil {
			t.Fatalf("read stream message %d: %v", sequence, err)
		}
		id := envelopeEventID(t, message.Data)
		bySubject[message.Subject] = append(bySubject[message.Subject], id)
		all = append(all, id)
	}
	return bySubject, all
}

func envelopeEventID(t *testing.T, data []byte) int64 {
	t.Helper()
	var envelope contracts.Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode an envelope: %v", err)
	}
	id, err := strconv.ParseInt(envelope.SourceEventID, 10, 64)
	if err != nil {
		t.Fatalf("envelope source event id %q: %v", envelope.SourceEventID, err)
	}
	return id
}

// ascending reports the first position at which ids decreases, or -1.
func firstDescent(ids []int64) int {
	for index := 1; index < len(ids); index++ {
		if ids[index] < ids[index-1] {
			return index
		}
	}
	return -1
}

// throughputRig is a real NATS server with its notification stream, the deployed bus client
// reaching it through a latency proxy, a direct connection that reads what arrived, and a migrated
// database whose shared pool is kept busy.
type throughputRig struct {
	database *store.Store
	client   *bus.Client
	reader   *natsgo.Conn
	broker   *events.Broker
	// role is every envelope that arrived on the role lane, in arrival order.
	roleMu sync.Mutex
	role   []int64
}

func newThroughputRig(t *testing.T, rtt time.Duration) *throughputRig {
	t.Helper()
	server, url := testnats.Start(t)
	_ = server
	proxied := startLatencyProxy(t, strings.TrimPrefix(url, "nats://"), rtt)
	client, err := bus.ConnectOwningStream([]string{proxied})
	if err != nil {
		t.Fatalf("connect the outbox's client through the latency proxy: %v", err)
	}
	t.Cleanup(client.Close)
	reader, err := natsgo.Connect(url)
	if err != nil {
		t.Fatalf("connect the reader: %v", err)
	}
	t.Cleanup(reader.Close)
	rig := &throughputRig{database: storetest.Open(t), client: client, reader: reader, broker: events.NewBroker()}
	if _, err := reader.Subscribe(roleTopic, func(message *natsgo.Msg) {
		id := envelopeEventID(t, message.Data)
		rig.roleMu.Lock()
		rig.role = append(rig.role, id)
		rig.roleMu.Unlock()
	}); err != nil {
		t.Fatalf("subscribe to the role lane: %v", err)
	}
	if err := reader.Flush(); err != nil {
		t.Fatalf("flush the role subscription: %v", err)
	}
	return rig
}

func (r *throughputRig) roleArrivals() []int64 {
	r.roleMu.Lock()
	defer r.roleMu.Unlock()
	return append([]int64(nil), r.role...)
}

// The outbox publishes a backlog at well above the rate Dispatch creates events under load (2.3 to
// 4.5 a second in production) when every NATS round trip carries a remote hub's latency and the
// API keeps the shared database pool busy, and it keeps the order consumers rely on: each issue's
// events reach its subject in id order (the Legion daemon fences an issue's facts on their
// sequence and drops one older than the last it applied), and on the success path the stream and
// the role lane hold them in id order overall. The rate is logged for comparison.
//
// The burst cases put a batch of events on one issue, the shape whose events share every
// destination, and log the drain time: from the outbox's start until every event is marked
// published. They hold no rate floor; the drain time is the reading.
func TestOutboxThroughputUnderRemoteLatency(t *testing.T) {
	const (
		backlogIssues = 20
		backlogCount  = 2 * batchSize
		burstCount    = batchSize
		minRate       = 25.0
		waitTime      = 3 * time.Minute
	)
	for _, tc := range []struct {
		rtt   time.Duration
		busy  bool
		burst bool
	}{
		{50 * time.Millisecond, false, false},
		{50 * time.Millisecond, true, false},
		{50 * time.Millisecond, false, true},
		{150 * time.Millisecond, false, false},
		{150 * time.Millisecond, true, false},
		{150 * time.Millisecond, false, true},
	} {
		rtt := tc.rtt
		name := rtt.String() + " idle pool"
		if tc.busy {
			name = rtt.String() + " busy pool"
		}
		issues, count := backlogIssues, backlogCount
		if tc.burst {
			name = rtt.String() + " single-issue burst"
			issues, count = 1, burstCount
		}
		t.Run(name, func(t *testing.T) {
			rig := newThroughputRig(t, rtt)
			seeded := seedThroughputBacklog(t, rig.database, rig.broker, issues, count)
			outboxStore := rig.database
			if tc.busy {
				outboxStore = busySharedPool(t, rig.database)
			}

			started := time.Now()
			stop := runOutbox(t, outboxStore, rig.client, rig.broker)
			defer stop()
			deadline := started.Add(waitTime)
			for unpublishedCount(t, rig.database) > 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			elapsed := time.Since(started)
			left := unpublishedCount(t, rig.database)
			rate := float64(count-left) / elapsed.Seconds()
			if tc.burst {
				t.Logf("%s: drained %d of %d events on one issue in %s: %.1f events/s", name, count-left, count, elapsed.Round(time.Millisecond), rate)
				if left > 0 {
					t.Fatalf("%s: %d of %d events left unpublished after %s", name, left, count, waitTime)
				}
			} else {
				t.Logf("%s: published %d of %d events in %s: %.1f events/s", name, count-left, count, elapsed.Round(time.Millisecond), rate)
				if left > 0 || rate < minRate {
					t.Fatalf("%s: %.1f events/s with %d left unpublished, want every event at %.0f events/s or more", name, rate, left, minRate)
				}
			}

			bySubject, all := streamOrder(t, rig.reader)
			ownerSubjects := 0
			for subject, ids := range bySubject {
				if strings.HasPrefix(subject, "notifications.dispatch.issue.") {
					ownerSubjects++
				}
				if at := firstDescent(ids); at >= 0 {
					t.Fatalf("subject %s holds event %d after event %d: %v", subject, ids[at], ids[at-1], ids)
				}
			}
			if ownerSubjects != issues {
				t.Fatalf("the stream holds %d issue subjects, want %d", ownerSubjects, issues)
			}
			wantStream := count + count/2 // each event's issue topic, and the replies' author topic
			if len(all) != wantStream {
				t.Fatalf("the stream holds %d messages, want %d", len(all), wantStream)
			}
			if at := firstDescent(all); at >= 0 {
				t.Fatalf("the stream holds event %d after event %d", all[at], all[at-1])
			}
			waitFor(t, 5*time.Second, "every role-lane envelope", func() bool { return len(rig.roleArrivals()) == len(seeded) })
			if roles := rig.roleArrivals(); firstDescent(roles) >= 0 {
				at := firstDescent(roles)
				t.Fatalf("the role lane received event %d after event %d", roles[at], roles[at-1])
			}
		})
	}
}

// runOutbox runs the outbox as cmd/dispatch wires it, on database's outbox pool and publishing
// through client, until the returned func stops it.
func runOutbox(t *testing.T, database *store.Store, client *bus.Client, broker *events.Broker) func() {
	t.Helper()
	pool, err := database.Pool.Outbox()
	if err != nil {
		t.Fatalf("open the outbox's pool: %v", err)
	}
	return runDeps(t, Deps{Store: &store.Store{Pool: pool}, Publisher: client, Broker: broker, Listen: database.Pool.Listener}, 10*time.Second)
}
