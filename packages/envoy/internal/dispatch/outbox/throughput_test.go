package outbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"slices"
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
// any size; filling one of production's store.SharedPoolSize would hold that many server
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
	keys, parents := seedLoadMessageIssues(t, database, issues)
	seeded := make([]model.Event, 0, count)
	for index := range count {
		seeded = append(seeded, appendLoadMessage(t, database, broker, index, keys[index%issues], parents[index%issues], index%2 == 1))
	}
	return seeded
}

// seedCommentReplyBacklog appends count events across issues issues. Every second event is a
// person's reply in a comment thread, on one of issues/2 issues that a session (claimant-N) claimed
// and that have no route. Each such thread is on a passage of the issue's document, which session
// writer-N wrote, and holds the person's root comment and writer-N's reply, so the reply reaches
// its issue topic, the claimant's topic and writer-N's topic. The other events are as
// seedThroughputBacklog's, on the other issues/2 issues, each routed to role:reviewer: a plain
// human message and a human reply to a session's message in turn. It returns the events in id
// order.
func seedCommentReplyBacklog(t *testing.T, database *store.Store, broker *events.Broker, issues, count int) []model.Event {
	t.Helper()
	ctx := context.Background()
	half := issues / 2
	messageKeys, parents := seedLoadMessageIssues(t, database, half)
	person := model.Actor{Kind: "user", ID: "alice"}
	personJSON, err := json.Marshal(person)
	if err != nil {
		t.Fatalf("encode the person: %v", err)
	}
	commentKeys := make([]string, half)
	roots := make([]string, half)
	for index := range half {
		number := half + index + 1
		commentKeys[index] = fmt.Sprintf("TT-%d", number)
		claimant, err := json.Marshal(model.Actor{Kind: "session", ID: fmt.Sprintf("claimant-%d", index)})
		if err != nil {
			t.Fatalf("encode the claimant: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank, claimed_by, claimed_at)
			values ($1, 'TT', $2, 'Load', '{"kind":"user","id":"alice"}', 'U', $3, now())
		`, commentKeys[index], number, claimant); err != nil {
			t.Fatalf("create claimed issue %s: %v", commentKeys[index], err)
		}
		writer := model.Actor{Kind: "session", ID: fmt.Sprintf("writer-%d", index)}
		document := seedDocument(t, database, new(commentKeys[index]), fmt.Sprintf("spec-%d", index), writer, []model.Actor{writer})
		anchor, err := json.Marshal(anchorOn(document))
		if err != nil {
			t.Fatalf("encode the anchor: %v", err)
		}
		if err := database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body, anchor) values ($1, $2, 'On this passage.', $3)
			returning id::text
		`, commentKeys[index], personJSON, anchor).Scan(&roots[index]); err != nil {
			t.Fatalf("create the thread root on %s: %v", commentKeys[index], err)
		}
		seedComment(t, database, commentKeys[index], writer, "Done.", &roots[index])
	}
	seeded := make([]model.Event, 0, count)
	for index := range count {
		if index%2 == 1 {
			message := index / 2
			seeded = append(seeded, appendLoadMessage(t, database, broker, index, messageKeys[message%half], parents[message%half], message%2 == 1))
			continue
		}
		thread := (index / 2) % half
		key, root := commentKeys[thread], roots[thread]
		reply := seedComment(t, database, key, person, "Load", &root)
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new(key), Type: "comment.created", Actor: person,
			Payload: model.CommentEventPayload{
				Comment: model.Comment{ID: reply, IssueKey: new(key), Author: person, Body: "Load", ReplyTo: new(root)},
			},
		}))
	}
	return seeded
}

// seedUnclaimedCommentBacklog inserts the project TT and issues issues TT-1 on, with no route and
// no claim, and appends count events across them: a person's comments, two on each issue in turn.
// The issue's primary document (artifacts.is_primary) was written by session writer-N, and holds a
// thread with the person's root comment and a reply from session replier-N. The first comment is
// on the issue's conversation and reaches its issue topic and writer-N's topic, through the query
// of the issues' primary documents. The second is a reply in the thread, which reaches its issue
// topic, writer-N's topic and replier-N's topic, through that query and the query of the threads'
// sessions. It returns the events in id order.
func seedUnclaimedCommentBacklog(t *testing.T, database *store.Store, broker *events.Broker, issues, count int) []model.Event {
	t.Helper()
	ctx := context.Background()
	if _, err := database.Pool.Exec(ctx, `insert into projects (key, name) values ('TT', 'Test')`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	person := model.Actor{Kind: "user", ID: "alice"}
	personJSON, err := json.Marshal(person)
	if err != nil {
		t.Fatalf("encode the person: %v", err)
	}
	keys := make([]string, issues)
	roots := make([]string, issues)
	for index := range issues {
		number := index + 1
		keys[index] = fmt.Sprintf("TT-%d", number)
		if _, err := database.Pool.Exec(ctx, `
			insert into issues (key, project_key, number, title, created_by, rank)
			values ($1, 'TT', $2, 'Load', '{"kind":"user","id":"alice"}', 'U')
		`, keys[index], number); err != nil {
			t.Fatalf("create issue %s: %v", keys[index], err)
		}
		writer := model.Actor{Kind: "session", ID: fmt.Sprintf("writer-%d", index)}
		document := seedDocument(t, database, new(keys[index]), "spec", writer, []model.Actor{writer})
		if _, err := database.Pool.Exec(ctx, `update artifacts set is_primary = true where id = $1`, document); err != nil {
			t.Fatalf("make the document of %s primary: %v", keys[index], err)
		}
		anchor, err := json.Marshal(anchorOn(document))
		if err != nil {
			t.Fatalf("encode the anchor: %v", err)
		}
		if err := database.Pool.QueryRow(ctx, `
			insert into comments (issue_key, author, body, anchor) values ($1, $2, 'On this passage.', $3)
			returning id::text
		`, keys[index], personJSON, anchor).Scan(&roots[index]); err != nil {
			t.Fatalf("create the thread root on %s: %v", keys[index], err)
		}
		seedComment(t, database, keys[index], model.Actor{Kind: "session", ID: fmt.Sprintf("replier-%d", index)}, "Done.", &roots[index])
	}
	seeded := make([]model.Event, 0, count)
	for index := range count {
		issue := (index / 2) % issues
		key := keys[issue]
		var replyTo *string
		if index%2 == 1 {
			replyTo = new(roots[issue])
		}
		comment := seedComment(t, database, key, person, "Load", replyTo)
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new(key), Type: "comment.created", Actor: person,
			Payload: model.CommentEventPayload{
				Comment: model.Comment{ID: comment, IssueKey: new(key), Author: person, Body: "Load", ReplyTo: replyTo},
			},
		}))
	}
	return seeded
}

// seedLoadMessageIssues inserts the project TT and issues issues TT-1 on, each routed to
// role:reviewer and holding a message from a session (agent-N), and returns their keys and those
// messages' ids.
func seedLoadMessageIssues(t *testing.T, database *store.Store, issues int) ([]string, []string) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `insert into projects (key, name) values ('TT', 'Test')`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	keys := make([]string, issues)
	parents := make([]string, issues)
	for index := range issues {
		number := index + 1
		keys[index] = fmt.Sprintf("TT-%d", number)
		if _, err := database.Pool.Exec(context.Background(), `
			insert into issues (key, project_key, number, title, route, created_by, rank)
			values ($1, 'TT', $2, 'Load', 'role:reviewer', '{"kind":"user","id":"alice"}', 'U')
		`, keys[index], number); err != nil {
			t.Fatalf("create issue %s: %v", keys[index], err)
		}
		parents[index] = seedMessage(t, database, keys[index], model.Actor{Kind: "session", ID: fmt.Sprintf("agent-%d", index)}, "Status?", nil)
	}
	return keys, parents
}

// appendLoadMessage appends a human message on key's issue, numbered index, and a reply to the
// session's message parent when reply is set.
func appendLoadMessage(t *testing.T, database *store.Store, broker *events.Broker, index int, key, parent string, reply bool) model.Event {
	t.Helper()
	message := model.Message{ID: fmt.Sprintf("message-%d", index), IssueKey: new(key), Body: "Load"}
	if reply {
		message.InReplyTo = new(parent)
	}
	return appendEvent(t, database, broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.MessageEventPayload{Message: message},
	})
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
// the role lane hold them in id order overall. It does so for a backlog of messages, for one whose
// half are a person's replies in comment threads, which take the issue's claimant and a query of
// the threads' sessions, and for one of a person's comments on issues with no route and no claim,
// which take a query of the issues' primary documents' sessions and, for a reply, of the threads'
// sessions. The rate, and the destinations each event type reached, are logged for comparison; the
// count of destinations is checked last, so a publisher that reaches fewer still reports its rate
// and order.
func TestOutboxThroughputUnderRemoteLatency(t *testing.T) {
	const (
		issues   = 20
		count    = 2 * batchSize
		minRate  = 25.0
		waitTime = 3 * time.Minute
	)
	for _, load := range []struct {
		name string
		seed func(t *testing.T, database *store.Store, broker *events.Broker, issues, count int) []model.Event
		// stream is how many messages the stream holds once the backlog is published.
		stream int
		// routed is how many of the backlog's events take the role lane.
		routed int
	}{
		// Each event's issue topic, and the replies' author topic.
		{"messages", seedThroughputBacklog, count + count/2, count},
		// Each comment reply's issue topic, its claimant's topic and its document writer's; each
		// message's issue topic, and the message replies' author topic.
		{"comment replies", seedCommentReplyBacklog, 3*count/2 + count/2 + count/4, count / 2},
		// Each comment's issue topic and its primary document writer's; each reply's thread
		// replier's.
		{"unclaimed comments", seedUnclaimedCommentBacklog, 2*count + count/2, 0},
	} {
		t.Run(load.name, func(t *testing.T) {
			for _, tc := range []struct {
				rtt  time.Duration
				busy bool
			}{
				{50 * time.Millisecond, false},
				{50 * time.Millisecond, true},
				{150 * time.Millisecond, false},
				{150 * time.Millisecond, true},
			} {
				rtt := tc.rtt
				pool := rtt.String() + " idle pool"
				if tc.busy {
					pool = rtt.String() + " busy pool"
				}
				name := load.name + ", " + pool
				t.Run(pool, func(t *testing.T) {
					rig := newThroughputRig(t, rtt)
					seeded := load.seed(t, rig.database, rig.broker, issues, count)
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
					t.Logf("%s: published %d of %d events in %s: %.1f events/s", name, count-left, count, elapsed.Round(time.Millisecond), rate)
					if left > 0 || rate < minRate {
						t.Fatalf("%s: %.1f events/s with %d left unpublished, want every event at %.0f events/s or more", name, rate, left, minRate)
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
					if at := firstDescent(all); at >= 0 {
						t.Fatalf("the stream holds event %d after event %d", all[at], all[at-1])
					}
					waitFor(t, 5*time.Second, "every role-lane envelope", func() bool { return len(rig.roleArrivals()) == load.routed })
					roles := rig.roleArrivals()
					if at := firstDescent(roles); at >= 0 {
						t.Fatalf("the role lane received event %d after event %d", roles[at], roles[at-1])
					}
					logDestinations(t, name, seeded, all, roles)
					if len(all) != load.stream {
						t.Fatalf("the stream holds %d messages, want %d", len(all), load.stream)
					}
				})
			}
		})
	}
}

// logDestinations logs, for each event type of seeded, how many destinations its events reached
// on average: the stream messages that carry them (stream) and the role-lane envelopes (roles). A
// reply in a comment thread counts apart from a comment that starts one.
func logDestinations(t *testing.T, name string, seeded []model.Event, stream, roles []int64) {
	t.Helper()
	types := make(map[int64]string, len(seeded))
	counts := map[string]int{}
	for _, event := range seeded {
		eventType := event.Type
		if payload, ok := event.Payload.(model.CommentEventPayload); ok && payload.Comment.ReplyTo != nil {
			eventType += " reply"
		}
		types[event.ID] = eventType
		counts[eventType]++
	}
	destinations := map[string]int{}
	for _, id := range slices.Concat(stream, roles) {
		destinations[types[id]]++
	}
	for _, eventType := range slices.Sorted(maps.Keys(counts)) {
		total := counts[eventType]
		t.Logf("%s: %d %s events reached %d destinations: %.2f per event", name, total, eventType, destinations[eventType], float64(destinations[eventType])/float64(total))
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
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Run(ctx, Deps{Store: &store.Store{Pool: pool}, Publisher: client, Broker: broker})
		close(done)
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("outbox did not stop")
		}
	}
}
