package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

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

// throughputHolder is the session that holds role:reviewer, every load issue's route, in the
// throughput rig.
const throughputHolder = "ses_reviewer"

// throughputRig is a real NATS server with its notification stream, the deployed bus client
// reaching it through a latency proxy, a direct connection that reads what arrived and on which
// role:reviewer's self-subscribed holder answers every forward with its receipt, and a migrated
// database whose shared pool is kept busy.
type throughputRig struct {
	database *store.Store
	client   *bus.Client
	reader   *natsgo.Conn
	broker   *events.Broker
	// role is every forward the role's holder received, in arrival order.
	roleMu sync.Mutex
	role   []int64
}

func newThroughputRig(t *testing.T, rtt time.Duration) *throughputRig {
	t.Helper()
	server, url := testnats.Start(t)
	_ = server
	proxied := testnats.StartProxy(t, url, testnats.ProxyLatency(rtt)).URL()
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
	if _, err := reader.Subscribe(contracts.AgentSubject(throughputHolder), func(message *natsgo.Msg) {
		id := envelopeEventID(t, message.Data)
		rig.roleMu.Lock()
		rig.role = append(rig.role, id)
		rig.roleMu.Unlock()
		_ = message.Respond(nil)
	}); err != nil {
		t.Fatalf("subscribe the role's holder: %v", err)
	}
	if err := reader.Flush(); err != nil {
		t.Fatalf("flush the holder's subscription: %v", err)
	}
	registry, err := envoystore.Open(reader)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(reader)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	if _, err := registry.SetRole(throughputHolder, "test-machine", "reviewer", false); err != nil {
		t.Fatalf("claim role:reviewer: %v", err)
	}
	if err := sessions.Put(throughputHolder, session.SessionEntry{SelfSubscribed: true}); err != nil {
		t.Fatalf("register the role's holder: %v", err)
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
// events reach each of its destinations in id order - its subject on the stream, a session's own
// subject, the role's holder, forwarded to with a receipt (the Legion daemon fences an issue's
// facts on their sequence and drops one older than the last it applied). Different issues' events
// interleave as their sends' answers allow. It does so for a backlog of messages, for one whose
// half are a person's replies in comment threads, which take the issue's claimant and a query of
// the threads' sessions, and for one of a person's comments on issues with no route and no claim,
// which take a query of the issues' primary documents' sessions and, for a reply, of the threads'
// sessions. The rate, and the destinations each event type reached, are logged for comparison; the
// count of destinations is checked last, so a publisher that reaches fewer still reports its rate
// and order.
//
// The burst cases put a batch of messages on one issue, the shape whose events share every
// destination, so each destination takes them one round trip after another, and log the drain
// time: from the outbox's start until every event is marked published. They hold no rate floor;
// the drain time is the reading.
func TestOutboxThroughputUnderRemoteLatency(t *testing.T) {
	const (
		backlogIssues = 20
		backlogCount  = 2 * batchSize
		burstCount    = batchSize
		minRate       = 25.0
		waitTime      = 3 * time.Minute
	)
	for _, load := range []struct {
		name string
		seed func(t *testing.T, database *store.Store, broker *events.Broker, issues, count int) []model.Event
		// stream is how many messages the stream holds on the issue topics and the sessions' own
		// subjects once count events of the load are published, the role's forwards aside.
		stream func(count int) int
		// routed is how many of count events of the load take the role route, each forwarded to
		// the role's holder, whose agent subject the stream captures too.
		routed func(count int) int
		// burst is whether the load also runs as a single-issue burst.
		burst bool
	}{
		// Each event's issue topic, and the replies' author topic.
		{"messages", seedThroughputBacklog, func(count int) int { return count + count/2 }, func(count int) int { return count }, true},
		// Each comment reply's issue topic, its claimant's topic and its document writer's; each
		// message's issue topic, and the message replies' author topic.
		{"comment replies", seedCommentReplyBacklog, func(count int) int { return 3*count/2 + count/2 + count/4 }, func(count int) int { return count / 2 }, false},
		// Each comment's issue topic and its primary document writer's; each reply's thread
		// replier's.
		{"unclaimed comments", seedUnclaimedCommentBacklog, func(count int) int { return 2*count + count/2 }, func(int) int { return 0 }, false},
	} {
		t.Run(load.name, func(t *testing.T) {
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
				if tc.burst && !load.burst {
					continue
				}
				rtt := tc.rtt
				shape := rtt.String() + " idle pool"
				if tc.busy {
					shape = rtt.String() + " busy pool"
				}
				issues, count := backlogIssues, backlogCount
				if tc.burst {
					shape = rtt.String() + " single-issue burst"
					issues, count = 1, burstCount
				}
				name := load.name + ", " + shape
				t.Run(shape, func(t *testing.T) {
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

					issueOf := map[int64]string{}
					for _, event := range seeded {
						issueOf[event.ID] = *event.IssueKey
					}
					bySubject, all := streamOrder(t, rig.reader)
					ownerSubjects := 0
					for subject, ids := range bySubject {
						if strings.HasPrefix(subject, "notifications.dispatch.issue.") {
							ownerSubjects++
						}
						// The holder's subject carries every issue's forwards, interleaved as their
						// receipts allow: the order is each issue's.
						byIssue := map[string][]int64{}
						for _, id := range ids {
							byIssue[issueOf[id]] = append(byIssue[issueOf[id]], id)
						}
						for issue, issueIDs := range byIssue {
							if at := firstDescent(issueIDs); at >= 0 {
								t.Fatalf("subject %s holds %s's event %d after its event %d: %v", subject, issue, issueIDs[at], issueIDs[at-1], issueIDs)
							}
						}
					}
					if ownerSubjects != issues {
						t.Fatalf("the stream holds %d issue subjects, want %d", ownerSubjects, issues)
					}
					roles := rig.roleArrivals()
					arrivals := map[string][]int64{}
					for _, id := range roles {
						arrivals[issueOf[id]] = append(arrivals[issueOf[id]], id)
					}
					for issue, ids := range arrivals {
						if at := firstDescent(ids); at >= 0 {
							t.Fatalf("the role's holder received %s's event %d after its event %d", issue, ids[at], ids[at-1])
						}
					}
					logDestinations(t, name, seeded, all)
					if want := load.stream(count) + load.routed(count); len(all) != want {
						t.Fatalf("the stream holds %d messages, want %d", len(all), want)
					}
					if want := load.routed(count); len(roles) != want {
						t.Fatalf("the role's holder received %d forwards, want one per routed event: %d", len(roles), want)
					}
				})
			}
		})
	}
}

// logDestinations logs, for each event type of seeded, how many destinations its events reached
// on average: the stream messages that carry them, the role's forwards among them, since the
// stream captures the holder's agent subject. A reply in a comment thread counts apart from a
// comment that starts one.
func logDestinations(t *testing.T, name string, seeded []model.Event, stream []int64) {
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
	for _, id := range stream {
		destinations[types[id]]++
	}
	for _, eventType := range slices.Sorted(maps.Keys(counts)) {
		total := counts[eventType]
		t.Logf("%s: %d %s events reached %d destinations: %.2f per event", name, total, eventType, destinations[eventType], float64(destinations[eventType])/float64(total))
	}
}

// runOutbox runs the outbox as cmd/dispatch wires it, on database's outbox pool, publishing through
// client and resolving role routes from the role and session buckets on client's connection, until
// the returned func stops it.
func runOutbox(t *testing.T, database *store.Store, client *bus.Client, broker *events.Broker) func() {
	t.Helper()
	pool, err := database.Pool.Outbox()
	if err != nil {
		t.Fatalf("open the outbox's pool: %v", err)
	}
	holders := roles.NewBuckets(client)
	return runDeps(t, Deps{Store: &store.Store{Pool: pool}, Publisher: client, Broker: broker, Listen: database.Pool.Listener,
		RoleClaims: holders, Sessions: holders}, 10*time.Second)
}
