package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/testnats"
)

// holding returns an outbox on deps whose instance holds the publisher hold's row for the next
// hour, as a process the hold names does, for a test that scans it without running it: a record's
// back-offs are written only while the hold is its own (recordBatch).
func holding(t *testing.T, deps Deps) *Outbox {
	t.Helper()
	o := New(deps)
	holdRow(t, deps.Store, o.instance)
	return o
}

// holdRow writes the publisher hold's row naming holder, lapsing an hour from now on the database's
// clock.
func holdRow(t *testing.T, database *store.Store, holder uuid.UUID) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		insert into duty_holds (name, holder, expires_at) values ($1, $2, now() + interval '1 hour')
		on conflict (name) do update set holder = excluded.holder, expires_at = excluded.expires_at
	`, publisherHold, holder); err != nil {
		t.Fatalf("write the publisher hold's row: %v", err)
	}
}

// statementTracer records every statement its pool's connections run, with its arguments.
type statementTracer struct {
	mu         sync.Mutex
	statements []tracedStatement
}

type tracedStatement struct {
	sql  string
	args []any
}

func (s *statementTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statements = append(s.statements, tracedStatement{sql: data.SQL, args: slices.Clone(data.Args)})
	return ctx
}

func (s *statementTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// matching returns the statements recorded so far whose SQL holds every one of fragments.
func (s *statementTracer) matching(fragments ...string) []tracedStatement {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found []tracedStatement
	for _, statement := range s.statements {
		if !slices.ContainsFunc(fragments, func(fragment string) bool { return !strings.Contains(statement.sql, fragment) }) {
			found = append(found, statement)
		}
	}
	return found
}

// tracedStore opens a store on database's database whose every statement tracer records.
func tracedStore(t *testing.T, database *store.Store, tracer *statementTracer) *store.Store {
	t.Helper()
	config := database.Pool.Config().Copy()
	config.ConnConfig.Tracer = tracer
	opened, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatalf("open a traced pool: %v", err)
	}
	pool := store.NewPool(opened)
	t.Cleanup(pool.Close)
	return &store.Store{Pool: pool}
}

// sendClock is a bus client whose windows record when the first envelope was sent.
type sendClock struct {
	*bus.Client
	once  sync.Once
	first chan struct{}
}

func (c *sendClock) Window() bus.Sender { return sendClockSender{Sender: c.Client.Window(), clock: c} }

type sendClockSender struct {
	bus.Sender
	clock *sendClock
}

func (s sendClockSender) PublishAsync(item contracts.Envelope) <-chan error {
	s.clock.once.Do(func() { close(s.clock.first) })
	return s.Sender.PublishAsync(item)
}

// An issue's events each go to its topic family and to session S's own subject. S's subject fails
// for the first event - the stream stops capturing agent subjects, so NATS answers its publish with
// no responders - and the first event is backed off. Two later events written while it waits reach
// the topic family at once, but S only after the first one does, and S takes all three in order.
func TestABackedOffEventHoldsBackOnlyItsDestination(t *testing.T) {
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	reader := testnats.Connect(t, url)
	js, err := reader.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	info, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	capturing := info.Config
	notAgents := info.Config
	notAgents.Subjects = slices.DeleteFunc(slices.Clone(capturing.Subjects), func(subject string) bool {
		return subject == contracts.AgentTopicPrefix+">"
	})
	if _, err := js.UpdateStream(&notAgents); err != nil {
		t.Fatalf("stop the stream capturing agent subjects: %v", err)
	}

	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "session:S"
	seedIssue(t, database, "T-1", &route)
	write := func(body string) model.Event {
		return appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: body},
		})
	}
	first := write("first")
	stop := runOutbox(t, database, client, broker)
	defer stop()
	waitFor(t, 10*time.Second, "the first event backed off for S's subject", func() bool {
		_, attempts := publishedDestinations(t, database, first.ID)
		return attempts > 0
	})
	if _, err := js.UpdateStream(&capturing); err != nil {
		t.Fatalf("let the stream capture agent subjects again: %v", err)
	}
	second, third := write("second"), write("third")

	waitFor(t, 30*time.Second, "all three events published", func() bool {
		return publishedAt(t, database, first.ID) != nil && publishedAt(t, database, second.ID) != nil &&
			publishedAt(t, database, third.ID) != nil
	})
	for _, event := range []model.Event{second, third} {
		if _, attempts := publishedDestinations(t, database, event.ID); attempts != 0 {
			t.Fatalf("event %d counted %d attempts, want none: it only waited on the first event", event.ID, attempts)
		}
	}
	topic := "notifications.dispatch.issue.T-1.message.created"
	session := contracts.AgentSubject("S")
	order := map[string][]int64{}
	sequence := map[string]uint64{}
	stream, err := js.StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	for seq := stream.State.FirstSeq; seq <= stream.State.LastSeq; seq++ {
		message, err := js.GetMsg(bus.Stream, seq)
		if err != nil {
			t.Fatalf("read stream message %d: %v", seq, err)
		}
		id := envelopeEventID(t, message.Data)
		order[message.Subject] = append(order[message.Subject], id)
		sequence[fmt.Sprintf("%s %d", message.Subject, id)] = seq
	}
	want := []int64{first.ID, second.ID, third.ID}
	if !slices.Equal(order[topic], want) {
		t.Fatalf("the topic family holds %v, want %v", order[topic], want)
	}
	if !slices.Equal(order[session], want) {
		t.Fatalf("S's subject holds %v, want %v: each event only after the one before it", order[session], want)
	}
	firstAtS := sequence[fmt.Sprintf("%s %d", session, first.ID)]
	for _, event := range []model.Event{second, third} {
		if at := sequence[fmt.Sprintf("%s %d", topic, event.ID)]; at > firstAtS {
			t.Fatalf("event %d reached the topic family at stream sequence %d, after the first event reached S at %d: it waited on S",
				event.ID, at, firstAtS)
		}
	}
}

// shiftedClock moves database's clock an hour ahead of the pod's, as a database host whose clock
// disagrees with the pod's has it, and returns a store whose connections read it: every now() they
// call answers an hour later than the pod's clock.
func shiftedClock(t *testing.T, database *store.Store) *store.Store {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `
		create schema testclock;
		create function testclock.now() returns timestamptz language sql stable
			as $$ select pg_catalog.now() + interval '1 hour' $$;
		do $$ begin
			execute format('alter database %I set search_path = testclock, pg_catalog, public', current_database());
		end $$;
	`); err != nil {
		t.Fatalf("shift the database's clock: %v", err)
	}
	return storetest.SecondProcess(t, database)
}

// NATS that does not acknowledge an event's envelope on a connection still current backs it off:
// no responders (no stream captures the subject) and a stream that never answers (the relay
// frozen, the connection still current) each count an attempt and record the error, and the
// event's next attempt is set on the database's clock, so it is not sent again before then. A
// connection that is lost instead releases the event with no attempt, and it is sent once NATS is
// back.
func TestAnEventNATSNeverAcknowledgesIsBackedOffAndRecorded(t *testing.T) {
	seed := func(t *testing.T, database *store.Store) model.Event {
		t.Helper()
		seedIssue(t, database, "T-1", nil)
		return appendEvent(t, database, events.NewBroker(), model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: "5a660655-04ad-4ce0-8a9b-93dd03c412b7", IssueKey: new("T-1"), Body: "acknowledge me"},
		})
	}
	backedOff := func(t *testing.T, database *store.Store, event model.Event, cause string) {
		t.Helper()
		var attempts int
		var failure *string
		if err := database.Pool.QueryRow(context.Background(),
			`select attempt_count, publish_error from events where id = $1`, event.ID).Scan(&attempts, &failure); err != nil {
			t.Fatalf("read the event: %v", err)
		}
		if attempts != 1 || failure == nil || !strings.Contains(*failure, cause) || publishedAt(t, database, event.ID) != nil {
			t.Fatalf("attempt_count %d, publish_error %v, published %v: want one attempt and an error naming %q",
				attempts, failure, publishedAt(t, database, event.ID) != nil, cause)
		}
	}

	t.Run("no responders", func(t *testing.T) {
		_, url := testnats.Start(t)
		// A client that owns no stream, on a server that has none: no stream captures the subject.
		client, err := bus.Connect([]string{url})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		database := storetest.Open(t)
		event := seed(t, database)
		shifted := shiftedClock(t, database)
		counter := &sendCounter{}
		outbox := holding(t, Deps{Store: shifted, Publisher: countingClient{Client: client, counter: counter}})
		outbox.scan(context.Background(), unfenced{})

		backedOff(t, database, event, "no responders")
		var due float64
		var nextAttempt time.Time
		if err := shifted.Pool.QueryRow(context.Background(),
			`select extract(epoch from next_attempt_at - now()), next_attempt_at from events where id = $1`, event.ID).Scan(&due, &nextAttempt); err != nil {
			t.Fatalf("read the event's next attempt: %v", err)
		}
		if due <= 0 || due > 1 {
			t.Fatalf("the next attempt is %.3fs past the database's now(), want within the first back-off's second", due)
		}
		if until := time.Until(nextAttempt); until < 50*time.Minute {
			t.Fatalf("the next attempt is %v past the pod's clock, want it on the database's, an hour ahead", until)
		}
		sent, _ := counter.of(event.ID)
		outbox.scan(context.Background(), unfenced{})
		if again, _ := counter.of(event.ID); again != sent {
			t.Fatalf("the backed-off event was sent %d more times by the next scan, want none before its back-off", again-sent)
		}
	})

	t.Run("a stream that never answers", func(t *testing.T) {
		server, url := testnats.Start(t)
		client, err := bus.ConnectOwningStream([]string{url})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		database := storetest.Open(t)
		event := seed(t, database)
		clock := &sendClock{Client: client, first: make(chan struct{})}
		thaw := server.Freeze(t)
		thawed := make(chan struct{})
		go func() {
			defer close(thawed)
			<-clock.first
			// Past the five seconds the window waits for an acknowledgement, and well inside
			// the flush probe that follows, so the connection answers that probe.
			time.Sleep(5*time.Second + 300*time.Millisecond)
			thaw()
		}()
		holding(t, Deps{Store: database, Publisher: clock}).scan(context.Background(), unfenced{})
		<-thawed
		backedOff(t, database, event, bus.ErrNotAcknowledged.Error())
	})

	t.Run("a lost connection", func(t *testing.T) {
		_, url := testnats.Start(t)
		proxy, proxied := startOutageProxy(t, strings.TrimPrefix(url, "nats://"), 0)
		client, err := bus.ConnectOwningStream([]string{proxied})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		t.Cleanup(client.Close)
		database := storetest.Open(t)
		event := seed(t, database)
		proxy.down()
		waitFor(t, 5*time.Second, "the client noticing the cut", func() bool { return !client.Connected() })
		outbox := holding(t, Deps{Store: database, Publisher: client})
		outbox.scan(context.Background(), unfenced{})
		var attempts int
		var nextAttempt *time.Time
		var failure *string
		if err := database.Pool.QueryRow(context.Background(),
			`select attempt_count, next_attempt_at, publish_error from events where id = $1`, event.ID).Scan(&attempts, &nextAttempt, &failure); err != nil {
			t.Fatalf("read the event: %v", err)
		}
		if attempts != 0 || nextAttempt != nil || failure != nil || publishedAt(t, database, event.ID) != nil {
			t.Fatalf("attempt_count %d, next_attempt_at %v, publish_error %v after a lost connection, want it released untouched",
				attempts, nextAttempt, failure)
		}
		proxy.up(t)
		waitFor(t, 10*time.Second, "the client reconnecting", client.Connected)
		outbox.scan(context.Background(), unfenced{})
		if publishedAt(t, database, event.ID) == nil {
			t.Fatal("the released event was not published once NATS was back")
		}
	})
}

// A batch of 100 events of every kind whose destinations need a lookup - replies to comments, ask
// answers, replies to messages and comments on documents - reads their followers and authors in at
// most four statements, one per kind, and publishes every one of them.
func TestTheBatchResolvesFollowersAndAuthorsInAtMostFourStatements(t *testing.T) {
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	const each = batchSize / 4
	var seeded []model.Event
	alice := model.Actor{Kind: "user", ID: "alice"}
	for index := range each {
		writer := model.Actor{Kind: "session", ID: fmt.Sprintf("writer-%d", index)}
		root := seedComment(t, database, "T-1", writer, "Draft", nil)
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{Comment: model.Comment{
				ID: uuid.NewString(), IssueKey: new("T-1"), Body: "Reviewed", ReplyTo: &root,
			}},
		}))
		askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: fmt.Sprintf("asker-%d", index)}, "Ship it?")
		seedFollower(t, database, askID, fmt.Sprintf("follower-%d", index))
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "ask.answered", Actor: alice,
			Payload: map[string]any{"id": askID, "question": "Ship it?"},
		}))
		message := seedMessage(t, database, "T-1", model.Actor{Kind: "session", ID: fmt.Sprintf("sender-%d", index)}, "Status?", nil)
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "message.created", Actor: alice,
			Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: "Fine", InReplyTo: &message},
		}))
		document := seedDocument(t, database, new("T-1"), fmt.Sprintf("notes-%d", index), model.Actor{Kind: "session", ID: fmt.Sprintf("author-%d", index)})
		seeded = append(seeded, appendEvent(t, database, broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: map[string]any{"id": uuid.NewString(), "body": "A note", "anchor": anchorOn(document)},
		}))
	}

	tracer := &statementTracer{}
	publisher := &recordingPublisher{}
	holding(t, Deps{Store: tracedStore(t, database, tracer), Publisher: publisher}).scan(context.Background(), unfenced{})

	for _, event := range seeded {
		if publishedAt(t, database, event.ID) == nil {
			t.Fatalf("event %d (%s) was not published", event.ID, event.Type)
		}
	}
	agents := 0
	for _, item := range publisher.all() {
		if strings.HasPrefix(item.Topic, contracts.AgentTopicPrefix) {
			agents++
		}
	}
	// Each comment reply reaches its root's author, each ask answer its two followers, each message
	// reply its parent's sender, and each document comment the document's author.
	if want := each * 5; agents != want {
		t.Fatalf("the batch reached %d sessions' own subjects, want %d", agents, want)
	}
	var lookups []string
	for _, fragment := range []string{"ask_followers", "from comments", "from messages", "artifact_versions"} {
		for _, statement := range tracer.matching(fragment) {
			lookups = append(lookups, statement.sql)
		}
	}
	if len(lookups) > 4 {
		t.Fatalf("the batch of %d events ran %d follower and author lookups, want at most 4", len(seeded), len(lookups))
	}
	t.Logf("the batch of %d events ran %d follower and author lookups", len(seeded), len(lookups))
}

// An event is written once, when its batch records it: reading the batch writes nothing, so however
// many destinations it has, a published event costs its row exactly one update.
func TestAPublishedEventIsWrittenOnce(t *testing.T) {
	alice := model.Actor{Kind: "user", ID: "alice"}
	for _, tc := range []struct {
		destinations int
		seed         func(t *testing.T, database *store.Store, broker *events.Broker) model.Event
	}{
		{1, func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
			return appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "message.created", Actor: alice,
				Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: "one destination"},
			})
		}},
		{3, func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
			root := seedComment(t, database, "T-1", model.Actor{Kind: "session", ID: "writer"}, "Draft", nil)
			return appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
				Payload: model.CommentEventPayload{Comment: model.Comment{
					ID: uuid.NewString(), IssueKey: new("T-1"), Body: "Reviewed", ReplyTo: &root,
				}},
			})
		}},
		{7, func(t *testing.T, database *store.Store, broker *events.Broker) model.Event {
			askID := seedAsk(t, database, "T-1", model.Actor{Kind: "session", ID: "asker"}, "Ship it?")
			for index := range 4 {
				seedFollower(t, database, askID, fmt.Sprintf("follower-%d", index))
			}
			return appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "ask.answered", Actor: alice,
				Payload: map[string]any{"id": askID, "question": "Ship it?"},
			})
		}},
	} {
		t.Run(fmt.Sprintf("%d destinations", tc.destinations), func(t *testing.T) {
			ctx := context.Background()
			database := storetest.Open(t)
			broker := events.NewBroker()
			route := "role:reviewer"
			seedIssue(t, database, "T-1", &route)
			if tc.destinations == 1 {
				if _, err := database.Pool.Exec(ctx, `update issues set route = null where key = 'T-1'`); err != nil {
					t.Fatalf("clear the route: %v", err)
				}
			}
			const published = 3
			var seeded []model.Event
			for range published {
				seeded = append(seeded, tc.seed(t, database, broker))
			}
			updates := func() int64 {
				var count int64
				if err := database.Pool.QueryRow(ctx, `select pg_stat_force_next_flush()`).Scan(new(any)); err != nil {
					t.Fatalf("flush this session's statistics: %v", err)
				}
				if err := database.Pool.QueryRow(ctx,
					`select n_tup_upd from pg_stat_user_tables where relid = 'events'::regclass`).Scan(&count); err != nil {
					t.Fatalf("read the events table's updates: %v", err)
				}
				return count
			}
			// A backend reports its statistics within a second of going idle: read the count once
			// it has held still that long.
			settled := func() int64 {
				last := updates()
				for {
					time.Sleep(1500 * time.Millisecond)
					now := updates()
					if now == last {
						return now
					}
					last = now
				}
			}
			before := settled()
			holding(t, Deps{Store: database, Publisher: &recordingPublisher{}}).scan(ctx, unfenced{})
			for _, event := range seeded {
				if publishedAt(t, database, event.ID) == nil {
					t.Fatalf("event %d was not published", event.ID)
				}
				if destinations, _ := publishedDestinations(t, database, event.ID); len(destinations) != tc.destinations {
					t.Fatalf("event %d reached %v, want %d destinations", event.ID, destinations, tc.destinations)
				}
			}
			waitFor(t, 15*time.Second, "the scan's updates reported", func() bool { return updates()-before >= published })
			if written := settled() - before; written != published {
				t.Fatalf("publishing %d events of %d destinations each updated events %d times, want %d: one write per event",
					published, tc.destinations, written, published)
			}
		})
	}
}

// planNode is one node of EXPLAIN (FORMAT JSON)'s plan.
type planNode struct {
	NodeType         string     `json:"Node Type"`
	Relation         string     `json:"Relation Name"`
	Index            string     `json:"Index Name"`
	RemovedByFilter  float64    `json:"Rows Removed by Filter"`
	RemovedByRecheck float64    `json:"Rows Removed by Index Recheck"`
	Plans            []planNode `json:"Plans"`
}

func (n planNode) walk(visit func(planNode)) {
	visit(n)
	for _, child := range n.Plans {
		child.walk(visit)
	}
}

// The statement that chooses a batch reads neither the pending subscription commands, which are
// never published, nor the backed-off events of owners that have nothing due: with 5,000 of each
// in the table, no node of its plan that reads events removes a row by filter or recheck, none
// scans events in full, and it runs in under half a millisecond.
func TestTheClaimSkipsPendingCommandsAndBackedOffRows(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, created_by, rank)
		select 'O-' || n, 'TT', 100 + n, 'Waiting', '{"kind":"user","id":"alice"}', 'W'
		from generate_series(1, 50) as n;
		insert into events (issue_key, seq, type, actor, payload, notify, attempt_count, next_attempt_at)
		select 'O-' || (n % 50 + 1), n, 'message.created', '{"kind":"user","id":"alice"}', '{"body":"backed off"}', true,
		       1, now() + interval '1 hour'
		from generate_series(1, 5000) as n;
		insert into events (issue_key, seq, type, actor, payload, notify)
		select 'T-1', 100000 + n, 'subscription.remove_requested', '{"kind":"user","id":"alice"}',
		       '{"session_id":"gone","pending":true}', true
		from generate_series(1, 5000) as n;
	`); err != nil {
		t.Fatalf("seed the backed-off events and the pending commands: %v", err)
	}
	fresh := func() {
		for index := range 10 {
			appendEvent(t, database, broker, model.Event{
				IssueKey: new("T-1"), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
				Payload: model.Message{ID: uuid.NewString(), IssueKey: new("T-1"), Body: fmt.Sprintf("fresh %d", index)},
			})
		}
	}
	fresh()
	if _, err := database.Pool.Exec(ctx, `analyze events`); err != nil {
		t.Fatalf("analyze events: %v", err)
	}

	tracer := &statementTracer{}
	holding(t, Deps{Store: tracedStore(t, database, tracer), Publisher: &recordingPublisher{}}).scan(ctx, unfenced{})
	claims := tracer.matching("published_at is null", "limit")
	if len(claims) == 0 {
		t.Fatal("the scan ran no statement choosing a batch")
	}
	claim := claims[0]
	// Ten more due events, so the statement explained has a batch to choose.
	fresh()

	fastest := math.Inf(1)
	var plan []byte
	// The fastest of several runs: the box is small and shared, and a run another test's work
	// delayed says nothing about the statement.
	const runs = 20
	for range runs {
		tx, err := database.Pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if err := tx.QueryRow(ctx, "explain (analyze, buffers, format json) "+claim.sql, claim.args...).Scan(&plan); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("explain the claim: %v\n%s", err, claim.sql)
		}
		_ = tx.Rollback(ctx)
		var explained []struct {
			Plan          planNode `json:"Plan"`
			ExecutionTime float64  `json:"Execution Time"`
		}
		if err := json.Unmarshal(plan, &explained); err != nil || len(explained) != 1 {
			t.Fatalf("decode the plan: %v\n%s", err, plan)
		}
		explained[0].Plan.walk(func(node planNode) {
			if node.Relation != "events" {
				return
			}
			if node.NodeType == "Seq Scan" || node.RemovedByFilter != 0 || node.RemovedByRecheck != 0 {
				t.Fatalf("the claim's %s of events (%s) removed %.0f rows by filter and %.0f by recheck, want none:\n%s",
					node.NodeType, node.Index, node.RemovedByFilter, node.RemovedByRecheck, plan)
			}
		})
		fastest = min(fastest, explained[0].ExecutionTime)
	}
	t.Logf("the claim ran in %.3f ms at its fastest of %d:\n%s", fastest, runs, plan)
	if fastest >= 0.5 {
		t.Fatalf("the claim ran in %.3f ms at its fastest, want under 0.5 ms:\n%s", fastest, plan)
	}
}

// A process that lost the publisher hold mid-batch records what is true whoever holds it - the
// destinations its events reached, and the events that reached all of them published - and
// schedules nothing: no back-off, no attempt, no error, no hold-back.
func TestARecordFromAProcessThatLostTheHoldWritesNoBackOff(t *testing.T) {
	ctx := context.Background()
	database := storetest.Open(t)
	broker := events.NewBroker()
	seedIssue(t, database, "T-1", nil)
	if _, err := database.Pool.Exec(ctx, `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ('T-2', 'TT', 2, 'Routed', 'role:reviewer', '{"kind":"user","id":"alice"}', 'V')
	`); err != nil {
		t.Fatalf("create the routed issue: %v", err)
	}
	message := func(issue string) model.Event {
		return appendEvent(t, database, broker, model.Event{
			IssueKey: new(issue), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
			Payload: model.Message{ID: uuid.NewString(), IssueKey: new(issue), Body: "lost"},
		})
	}
	done, failing, behind := message("T-1"), message("T-2"), message("T-2")
	publisher := &recordingPublisher{failTopic: roleTopic}
	outbox := New(Deps{Store: database, Publisher: publisher})
	// Another process holds the hold now.
	holdRow(t, database, uuid.New())

	end := outbox.scan(ctx, unfenced{})
	if !end.fenced {
		t.Fatalf("the scan ended %+v, want it stopped as one whose hold is another's", end)
	}
	if publishedAt(t, database, done.ID) == nil {
		t.Fatal("the event that reached every destination was not marked published")
	}
	for _, event := range []model.Event{failing, behind} {
		var attempts int
		var nextAttempt *time.Time
		var failure *string
		var destinations []string
		if err := database.Pool.QueryRow(ctx,
			`select attempt_count, next_attempt_at, publish_error, published_destinations from events where id = $1`, event.ID,
		).Scan(&attempts, &nextAttempt, &failure, &destinations); err != nil {
			t.Fatalf("read event %d: %v", event.ID, err)
		}
		if attempts != 0 || nextAttempt != nil || failure != nil {
			t.Fatalf("event %d: attempt_count %d, next_attempt_at %v, publish_error %v, want nothing scheduled by a process without the hold",
				event.ID, attempts, nextAttempt, failure)
		}
		if !slices.Equal(destinations, []string{"notifications.dispatch.issue.T-2.message.created"}) {
			t.Fatalf("event %d recorded %v, want the topic it reached", event.ID, destinations)
		}
	}
}
