package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/events"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// roleHolder is the session heldRoles names as every role's holder.
const roleHolder = "role-holder"

// heldRoles is a role-claim store and session reader for a test publisher with no NATS: every role
// is claimed by roleHolder, a live self-subscribed session, so a role route is forwarded to it with
// a Request that the test publisher answers.
type heldRoles struct{}

func (heldRoles) RoleClaim(context.Context, string) (envoystore.RoleClaim, error) {
	return envoystore.RoleClaim{HolderSessionID: roleHolder}, nil
}

func (heldRoles) ReleaseExpiredRoleClaim(context.Context, string, string, time.Duration) (envoystore.ExpiredRoleClaimRelease, error) {
	return envoystore.ExpiredRoleClaimSuperseded, nil
}

func (heldRoles) Holder(_ context.Context, _ string, live func(session.SessionEntry) bool) (session.SessionEntry, error) {
	entry := session.SessionEntry{SelfSubscribed: true, UpdatedAt: time.Now().UnixMilli()}
	if !live(entry) {
		return session.SessionEntry{}, natsgo.ErrKeyNotFound
	}
	return entry, nil
}

func (heldRoles) TTL() time.Duration { return 5 * time.Minute }

// withHeldRoles is deps whose role routes resolve to roleHolder (heldRoles).
func withHeldRoles(deps Deps) Deps {
	deps.RoleClaims, deps.Sessions = heldRoles{}, heldRoles{}
	return deps
}

// roleRig is a real NATS server whose stream captures agent subjects, the outbox's client on it,
// the role and session registries a session plugin registers through, and a migrated database.
type roleRig struct {
	url      string
	client   *bus.Client
	conn     *natsgo.Conn
	registry *envoystore.Registry
	sessions *session.SessionRegistry
	database *store.Store
	broker   *events.Broker
}

func newRoleRig(t *testing.T) *roleRig {
	t.Helper()
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	return newRoleRigOn(t, url, client, testnats.Connect(t, url))
}

// newRoleRigOn is a roleRig on the server at url, with client as the outbox's client and conn as
// the test's own connection, which the registries write through.
func newRoleRigOn(t *testing.T, url string, client *bus.Client, conn *natsgo.Conn) *roleRig {
	t.Helper()
	registry, err := envoystore.Open(conn)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(conn)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	return &roleRig{url: url, client: client, conn: conn, registry: registry, sessions: sessions,
		database: storetest.Open(t), broker: events.NewBroker()}
}

// deps is the outbox's dependencies as cmd/dispatch wires them: role routes resolved through
// roles.Buckets on the outbox's own client.
func (r *roleRig) deps() Deps {
	holders := roles.NewBuckets(r.client)
	return Deps{Store: r.database, Publisher: r.client, Broker: r.broker, RoleClaims: holders, Sessions: holders}
}

// claim makes holder role's holder, registered with entry, as a session plugin's registration does.
func (r *roleRig) claim(t *testing.T, role, holder string, entry session.SessionEntry) {
	t.Helper()
	if _, err := r.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
	if err := r.sessions.Put(holder, entry); err != nil {
		t.Fatalf("register %s: %v", holder, err)
	}
}

// claimStale makes holder role's holder, registered and with a heartbeat older than
// session.ClaimStaleAfter, so the role's resolution holds its forward pending with nothing sent
// (forwardOf). The entry goes to the bucket directly: the registry stamps every entry it writes
// with now.
func (r *roleRig) claimStale(t *testing.T, role, holder string) {
	t.Helper()
	if _, err := r.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
	stale, err := json.Marshal(session.SessionEntry{SelfSubscribed: true, UpdatedAt: time.Now().Add(-session.ClaimStaleAfter - time.Minute).UnixMilli()})
	if err != nil {
		t.Fatalf("encode the stale entry: %v", err)
	}
	js, err := r.conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	kv, err := js.KeyValue(bus.SessionBucket)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	if _, err := kv.Put(holder, stale); err != nil {
		t.Fatalf("register the stale holder %s: %v", holder, err)
	}
}

// countingDeps is the rig's dependencies with a publish-failure counter on a registry of its own,
// so a test can read which reason a send that was not reached was counted under.
func (r *roleRig) countingDeps(t *testing.T) (Deps, *metrics.Registry) {
	t.Helper()
	registry := metrics.New()
	deps := r.deps()
	deps.PublishFailures = registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
		"Destinations of an outbox publish attempt that were not reached")
	return deps, registry
}

// failureReasons is what /metrics serves of registry, the counted reasons among it.
func failureReasons(t *testing.T, registry *metrics.Registry) string {
	t.Helper()
	response := httptest.NewRecorder()
	registry.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	return response.Body.String()
}

// routedEvent seeds issue key routed to role:role and appends a human message on it.
func (r *roleRig) routedEvent(t *testing.T, key, role string, actor model.Actor) model.Event {
	t.Helper()
	route := "role:" + role
	if _, err := r.database.Pool.Exec(context.Background(),
		`insert into projects (key, name) values ('TT', 'Test') on conflict do nothing`); err != nil {
		t.Fatalf("create project: %v", err)
	}
	var number int
	if err := r.database.Pool.QueryRow(context.Background(), `select count(*) + 1 from issues`).Scan(&number); err != nil {
		t.Fatalf("number the issue: %v", err)
	}
	if _, err := r.database.Pool.Exec(context.Background(), `
		insert into issues (key, project_key, number, title, route, created_by, rank)
		values ($1, 'TT', $2, 'Routed', $3, '{"kind":"user","id":"alice"}', 'U')
	`, key, number, route); err != nil {
		t.Fatalf("create issue %s: %v", key, err)
	}
	return appendEvent(t, r.database, r.broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: actor,
		Payload: model.Message{ID: fmt.Sprintf("8f14e45f-ceea-467a-9c1e-%012d", number), IssueKey: new(key), Body: "for the role"},
	})
}

// silent fails t when anything arrives on subject before the test ends.
func (r *roleRig) silent(t *testing.T, subject string) {
	t.Helper()
	arrived := subscribe(t, r.conn, subject)
	t.Cleanup(func() {
		select {
		case message := <-arrived:
			t.Errorf("%s carried %s, want nothing", subject, message.Data)
		default:
		}
	})
}

// receiver is a role holder's own subscription to its agent subject: it records every forward it
// receives and answers it with an empty receipt while answering is set.
type receiver struct {
	answering atomic.Bool
	mu        sync.Mutex
	forwards  []contracts.Envelope
	arrived   chan contracts.Envelope
}

func (r *roleRig) receiver(t *testing.T, holder string, answering bool) *receiver {
	t.Helper()
	recv := &receiver{arrived: make(chan contracts.Envelope, 64)}
	recv.answering.Store(answering)
	sub, err := r.conn.Subscribe(contracts.AgentSubject(holder), func(msg *natsgo.Msg) {
		var item contracts.Envelope
		if err := json.Unmarshal(msg.Data, &item); err != nil {
			return
		}
		recv.mu.Lock()
		recv.forwards = append(recv.forwards, item)
		recv.mu.Unlock()
		recv.arrived <- item
		if recv.answering.Load() && msg.Reply != "" {
			_ = msg.Respond(nil)
		}
	})
	if err != nil {
		t.Fatalf("subscribe to %s: %v", holder, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := r.conn.Flush(); err != nil {
		t.Fatalf("flush the subscription: %v", err)
	}
	return recv
}

func (recv *receiver) received() []contracts.Envelope {
	recv.mu.Lock()
	defer recv.mu.Unlock()
	return append([]contracts.Envelope(nil), recv.forwards...)
}

// storedCopies is how many messages the stream holds on the subjects filter matches.
func storedCopies(t *testing.T, client *bus.Client, filter string) uint64 {
	t.Helper()
	info, err := client.JS().StreamInfo(bus.Stream, &natsgo.StreamInfoRequest{SubjectsFilter: filter})
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	var copies uint64
	for _, count := range info.State.Subjects {
		copies += count
	}
	return copies
}

// eventState reads an event's attempts, error and destinations.
func eventState(t *testing.T, database *store.Store, id int64) (attempts int, failure *string, destinations []string) {
	t.Helper()
	if err := database.Pool.QueryRow(context.Background(),
		`select attempt_count, publish_error, published_destinations from events where id = $1`, id,
	).Scan(&attempts, &failure, &destinations); err != nil {
		t.Fatalf("read event %d: %v", id, err)
	}
	return attempts, failure, destinations
}

// dueNow makes a backed-off event due at once.
func dueNow(t *testing.T, database *store.Store, id int64) {
	t.Helper()
	if _, err := database.Pool.Exec(context.Background(), `update events set next_attempt_at = now() where id = $1`, id); err != nil {
		t.Fatalf("make event %d due: %v", id, err)
	}
}

// A self-subscribed holder gets the event forwarded to its own agent subject as a listener's role
// forward carries it - the role's topic, the forward's dedupe key, the event's id - and the event
// is published only once the holder's receipt is in. A holder that does not answer leaves the
// event backed off with its attempt and the reason; once it answers, a retry publishes it, and the
// stream, which captures the holder's subject, holds one copy of the forward however often it was
// sent. Nothing crosses the role's topic.
func TestARoleRouteIsForwardedToASelfSubscribedHolderAndPublishedOnItsReceipt(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_self_subscribed"
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+role)
	recv := rig.receiver(t, holder, false)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.deps())

	t.Run("published only after the receipt", func(t *testing.T) {
		// The holder takes the first forward and does not answer it.
		recv.answering.Store(false)
		scanned := make(chan struct{})
		go func() { outbox.scan(context.Background(), unfenced{}); close(scanned) }()
		var forward contracts.Envelope
		select {
		case forward = <-recv.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the holder received no forward")
		}
		if forward.Topic != contracts.RoleTopicPrefix+role || forward.DedupeKey != contracts.RoleForwardDedupePrefix+fmt.Sprintf("dispatch-%d", event.ID) ||
			forward.EventID != fmt.Sprintf("dispatch-%d", event.ID) {
			t.Fatalf("the holder received topic %q dedupe key %q event id %q, want the role forward of event %d",
				forward.Topic, forward.DedupeKey, forward.EventID, event.ID)
		}
		<-scanned
		if publishedAt(t, rig.database, event.ID) != nil {
			t.Fatal("the event was published though its holder never answered")
		}
		attempts, failure, destinations := eventState(t, rig.database, event.ID)
		if attempts != 1 || failure == nil || !strings.Contains(*failure, bus.ErrNotAcknowledged.Error()) ||
			!strings.Contains(*failure, `publish route "role:reviewer"`) {
			t.Fatalf("attempt_count %d, publish_error %v: want one attempt naming the role route and the missing receipt", attempts, failure)
		}
		if strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix) {
			t.Fatalf("published_destinations %v records the role route its holder never answered", destinations)
		}
	})

	t.Run("published on a retry once the holder answers", func(t *testing.T) {
		recv.answering.Store(true)
		dueNow(t, rig.database, event.ID)
		outbox.scan(context.Background(), unfenced{})
		if publishedAt(t, rig.database, event.ID) == nil {
			t.Fatal("the event was not published once its holder answered")
		}
		_, failure, destinations := eventState(t, rig.database, event.ID)
		if failure != nil || !strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix+role) {
			t.Fatalf("publish_error %v, published_destinations %v: want no error and the role topic recorded", failure, destinations)
		}
		if got := len(recv.received()); got != 2 {
			t.Fatalf("the holder received %d forwards, want the first and the retry", got)
		}
		if copies := storedCopies(t, rig.client, contracts.AgentSubject(holder)); copies != 1 {
			t.Fatalf("the stream holds %d copies of the forward on %s after a retry, want one", copies, contracts.AgentSubject(holder))
		}
	})
}

// A holder with a serving port is delivered from the stream's stored copy, which never answers a
// request, so the stream's acknowledgement of that copy is the receipt: the event is published with
// nobody subscribed to the holder's subject, and the stream holds the forward once.
func TestARoleRouteToAHolderWithAServingPortIsPublishedOnTheStreamsAcknowledgement(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_serving"
	rig.claim(t, role, holder, session.SessionEntry{Port: 40123})
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) == nil {
		attempts, failure, _ := eventState(t, rig.database, event.ID)
		t.Fatalf("the event was not published on the stream's acknowledgement: attempt_count %d, publish_error %v", attempts, failure)
	}
	if copies := storedCopies(t, rig.client, contracts.AgentSubject(holder)); copies != 1 {
		t.Fatalf("the stream holds %d copies of the forward, want one", copies)
	}
}

// A role nobody claims has no holder: its route counts as reached, the event is published, and
// nothing is sent to any session.
func TestARoleRouteWithNoClaimIsPublishedWithNothingSent(t *testing.T) {
	rig := newRoleRig(t)
	const role = "unclaimed"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentTopicPrefix+">")
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) == nil {
		t.Fatal("the event of an unclaimed role was not published")
	}
	if _, _, destinations := eventState(t, rig.database, event.ID); !strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix+role) {
		t.Fatalf("published_destinations %v, want the role route counted as reached", destinations)
	}
	if copies := storedCopies(t, rig.client, contracts.AgentTopicPrefix+">"); copies != 0 {
		t.Fatalf("the stream holds %d agent-subject messages, want none", copies)
	}
}

// A role route reached with nothing to send is no send of its batch. The event's first batch, with
// no role reader, reaches the issue's topic and leaves the role route pending; once the event is
// due again, the role, which nobody claims, is its only unreached destination, and the batch that
// publishes it reports nothing sent.
func TestABatchWhoseOnlyRouteLeftIsAnUnclaimedRoleSendsNothing(t *testing.T) {
	ctx := context.Background()
	rig := newRoleRig(t)
	const role = "unclaimed"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentTopicPrefix+">")
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	holder := holding(t, rig.deps()).instance

	first, err := scanBatch(ctx, Deps{Store: rig.database, Publisher: rig.client, Broker: rig.broker}, unfenced{}, holder)
	if err != nil {
		t.Fatalf("scan the first batch: %v", err)
	}
	if _, _, destinations := eventState(t, rig.database, event.ID); first.sent != 1 || publishedAt(t, rig.database, event.ID) != nil ||
		len(destinations) == 0 || strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix) {
		t.Fatalf("the first batch %+v recorded %v, want its topic reached and the role route pending", first, destinations)
	}
	dueNow(t, rig.database, event.ID)

	second, err := scanBatch(ctx, rig.deps(), unfenced{}, holder)
	if err != nil {
		t.Fatalf("scan the second batch: %v", err)
	}
	if publishedAt(t, rig.database, event.ID) == nil {
		t.Fatal("the event whose only route left is an unclaimed role was not published")
	}
	if second.sent != 0 || second.firstID != 0 || second.lastID != 0 || second.published != 1 {
		t.Fatalf("the second batch = %+v, want it to publish the event with nothing sent", second)
	}
}

// A claim whose holder's session is gone is released, with the listener's compare and set, and the
// role then has no holder: the event is published with nothing sent, and the claim is gone.
func TestARoleRouteWhoseHolderIsGoneReleasesTheClaimAndIsPublished(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_gone"
	if _, err := rig.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim the role for a session that never registered: %v", err)
	}
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentSubject(holder))
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) == nil {
		attempts, failure, _ := eventState(t, rig.database, event.ID)
		t.Fatalf("the event was not published: attempt_count %d, publish_error %v", attempts, failure)
	}
	if claimed, err := rig.registry.RoleHolder(role); err != nil || claimed != "" {
		t.Fatalf("the claim after the scan names %q (%v), want it released", claimed, err)
	}
}

// A holder whose session is there but has not heartbeated within session.ClaimStaleAfter keeps its
// claim, and the event stays unpublished, backed off with its attempt and the reason, to be
// retried.
func TestARoleRouteToAStaleHolderStaysPending(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_stale"
	rig.claimStale(t, role, holder)
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentSubject(holder))
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) != nil {
		t.Fatal("the event of a stale holder was published")
	}
	var nextAttempt *time.Time
	if err := rig.database.Pool.QueryRow(context.Background(), `select next_attempt_at from events where id = $1`, event.ID).Scan(&nextAttempt); err != nil {
		t.Fatalf("read the event's next attempt: %v", err)
	}
	attempts, failure, _ := eventState(t, rig.database, event.ID)
	if attempts != 1 || nextAttempt == nil || failure == nil || !strings.Contains(*failure, "has not heartbeated") {
		t.Fatalf("attempt_count %d, next_attempt_at %v, publish_error %v: want it backed off naming the stale holder", attempts, nextAttempt, failure)
	}
	if claimed, err := rig.registry.RoleHolder(role); err != nil || claimed != holder {
		t.Fatalf("the claim after the scan names %q (%v), want the stale holder's kept", claimed, err)
	}
}

// An event its role's own holder acted is not forwarded back to it, as the listener skips a role
// echo: the route counts as reached and the event is published. The broker marks a session's own
// event as one that notifies nobody (events.Broker.Notify), which leaves it no route at all, so the
// row is marked to notify here: the echo is what keeps such a row from reaching its own actor.
func TestARoleRouteWhoseHolderIsTheActorIsPublishedWithoutAForward(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_actor"
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentSubject(holder))
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "session", ID: holder})
	if _, err := rig.database.Pool.Exec(context.Background(), `update events set notify = true where id = $1`, event.ID); err != nil {
		t.Fatalf("mark the event to notify: %v", err)
	}

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) == nil {
		t.Fatal("the holder's own event was not published")
	}
	if _, _, destinations := eventState(t, rig.database, event.ID); !strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix+role) {
		t.Fatalf("published_destinations %v, want the role route counted as reached", destinations)
	}
	if copies := storedCopies(t, rig.client, contracts.AgentSubject(holder)); copies != 0 {
		t.Fatalf("the stream holds %d forwards to the actor, want none", copies)
	}
}

// One holder's forward that Dispatch's grant denies fails only its own event: in a batch of 50
// role-routed events on distinct issues, each with a live self-subscribed holder that answers, the
// 49 others are published with no attempt counted, and the denied one is backed off.
func TestOneRefusedSubjectFailsOnlyItsOwnEvent(t *testing.T) {
	const events, denied = 50, 17
	holderOf := func(index int) string { return fmt.Sprintf("ses_holder_%02d", index) }
	seed, public := testnats.User(t)
	config := fmt.Sprintf("jetstream {}\nauthorization {\n  users = [ { nkey: %q, permissions: { publish: { allow: [\">\"], deny: [%q] } } } ]\n}\n",
		public, contracts.AgentSubject(holderOf(denied)))
	_, url := testnats.StartConfig(t, config, []string{"-js"}, false)
	t.Setenv("NATS_NKEY_SEED", seed)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	conn, err := testnats.ConnectSeed(t, url, seed)
	if err != nil {
		t.Fatalf("connect the test's connection: %v", err)
	}
	t.Cleanup(conn.Close)
	rig := newRoleRigOn(t, url, client, conn)

	sub, err := conn.Subscribe(contracts.AgentTopicPrefix+">", func(msg *natsgo.Msg) {
		_ = msg.Respond(nil)
	})
	if err != nil {
		t.Fatalf("subscribe the holders: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush the holders' subscription: %v", err)
	}
	seeded := make([]model.Event, events)
	for index := range events {
		role := fmt.Sprintf("lane-%02d", index)
		rig.claim(t, role, holderOf(index), session.SessionEntry{SelfSubscribed: true})
		rig.silent(t, contracts.RoleTopicPrefix+role)
		seeded[index] = rig.routedEvent(t, fmt.Sprintf("T-%d", index+1), role, model.Actor{Kind: "user", ID: "alice"})
	}

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	for index, event := range seeded {
		attempts, failure, _ := eventState(t, rig.database, event.ID)
		if index == denied {
			if publishedAt(t, rig.database, event.ID) != nil || attempts != 1 || failure == nil ||
				!strings.Contains(*failure, bus.ErrPublishDenied.Error()) {
				t.Fatalf("the denied holder's event: published %v, attempt_count %d, publish_error %v; want it backed off with the denial",
					publishedAt(t, rig.database, event.ID) != nil, attempts, failure)
			}
			continue
		}
		if publishedAt(t, rig.database, event.ID) == nil || attempts != 0 {
			t.Fatalf("event %d of holder %s: published %v, attempt_count %d, publish_error %v; want it published with no attempt",
				index, holderOf(index), publishedAt(t, rig.database, event.ID) != nil, attempts, failure)
		}
	}
}

// A role forward its holder's resolution held pending is counted under the holder, not as a
// publish failure: nothing was sent, so NATS neither refused nor dropped it, while `failed` is
// what tells an operator the bus failed a send. The holder here is registered and has not
// heartbeated within session.ClaimStaleAfter; a claim the store kept, a claim that churned and a
// read that failed hold a forward pending the same way (forwardOf), and the event is backed off
// as TestARoleRouteToAStaleHolderStaysPending holds.
func TestAForwardHeldByItsHoldersResolutionIsCountedUnderTheHolder(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_stale_counted"
	rig.claimStale(t, role, holder)
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentSubject(holder))
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	deps, registry := rig.countingDeps(t)

	holding(t, deps).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) != nil {
		t.Fatal("the event whose role forward stayed pending was published")
	}
	body := failureReasons(t, registry)
	if !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="holder"} 1`) {
		t.Fatalf("/metrics body = %q, want the pending forward counted under its holder", body)
	}
	if strings.Contains(body, `reason="failed"`) {
		t.Fatalf("/metrics body = %q counts a forward that sent nothing as a publish failure", body)
	}
}

// A role forward NATS took whose holder sent no receipt is counted unanswered, not failed: the
// flush confirmed the publish, so what is missing is the holder's answer and not the bus. The
// holder here is registered and self-subscribed, as a session plugin registers it, with nothing
// subscribed to its agent subject, which is the shape a holder that stopped reading has.
func TestAForwardNoHolderAnsweredIsCountedUnanswered(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_silent"
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	deps, registry := rig.countingDeps(t)

	holding(t, deps).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) != nil {
		t.Fatal("the event whose role holder sent no receipt was published")
	}
	body := failureReasons(t, registry)
	if !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="unanswered"} 1`) {
		t.Fatalf("/metrics body = %q, want the forward its holder left unanswered counted unanswered", body)
	}
	if strings.Contains(body, `reason="failed"`) {
		t.Fatalf("/metrics body = %q counts a forward NATS took as a publish failure", body)
	}
}
