package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
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
	"github.com/sjawhar/envoy/internal/dispatch/retry"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
	"github.com/sjawhar/envoy/internal/logtest"
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
// a Request that the test publisher answers. It judges that session on the package's own windows,
// as roles.Buckets does (roles.Windows).
type heldRoles struct{ roles.Windows }

func (heldRoles) RoleClaim(context.Context, string) (envoystore.RoleClaim, error) {
	return envoystore.RoleClaim{HolderSessionID: roleHolder}, nil
}

func (heldRoles) Holder(context.Context, string) (session.SessionEntry, error) {
	return session.SessionEntry{SelfSubscribed: true, UpdatedAt: time.Now().UnixMilli()}, nil
}

func (heldRoles) LastSeen(string) int64 { return 0 }

// withHeldRoles is deps whose role routes resolve to roleHolder (heldRoles).
func withHeldRoles(deps Deps) Deps {
	deps.RoleClaims, deps.Sessions = heldRoles{}, heldRoles{}
	return deps
}

// errRoleBucketUnreachable is what unreadableRoleClaims answers every read of a claim with.
var errRoleBucketUnreachable = errors.New("the role bucket is unreachable")

// unreadableRoleClaims is a role-claim store whose every read of a claim fails, the shape of a
// role bucket the outbox cannot reach: the resolution answers a read error, and the role
// destination it resolves stays pending with nothing sent (roles.ResolveHolder, forwardOf). The
// release is never reached, a resolution that read no claim releasing none.
type unreadableRoleClaims struct{}

func (unreadableRoleClaims) RoleClaim(context.Context, string) (envoystore.RoleClaim, error) {
	return envoystore.RoleClaim{}, errRoleBucketUnreachable
}

func (unreadableRoleClaims) ReleaseExpiredRoleClaim(context.Context, string, string, time.Duration) (envoystore.ExpiredRoleClaimRelease, error) {
	return envoystore.ExpiredRoleClaimMissing, nil
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
	return newRoleRigWith(t)
}

// newRoleRigWithSessionMaxAge is a roleRig whose session bucket keeps a record for maxAge only,
// shorter than the gone horizon as a deployed bucket's own maximum age can be: a holder that
// stops heartbeating loses the record the bucket stored for it well before it is gone. The
// bucket is created with that age (WithSessionTTL), which is the one way to put an age on it
// under the duplicates window a KV bucket of two minutes' age carries.
func newRoleRigWithSessionMaxAge(t *testing.T, maxAge time.Duration) *roleRig {
	t.Helper()
	return newRoleRigWith(t, session.WithSessionTTL(maxAge))
}

// newRoleRigWith is a roleRig on a server of its own, its session registry opened with options.
func newRoleRigWith(t *testing.T, options ...session.SessionRegistryOption) *roleRig {
	t.Helper()
	_, url := testnats.Start(t)
	client, err := bus.ConnectOwningStream([]string{url})
	if err != nil {
		t.Fatalf("connect the outbox's client: %v", err)
	}
	t.Cleanup(client.Close)
	return newRoleRigOn(t, url, client, testnats.Connect(t, url), options...)
}

// newRoleRigOn is a roleRig on the server at url, with client as the outbox's client and conn as
// the test's own connection, which the registries write through.
func newRoleRigOn(t *testing.T, url string, client *bus.Client, conn *natsgo.Conn, options ...session.SessionRegistryOption) *roleRig {
	t.Helper()
	registry, err := envoystore.Open(conn)
	if err != nil {
		t.Fatalf("open the role registry: %v", err)
	}
	t.Cleanup(registry.StopWatch)
	sessions, err := session.OpenSessionRegistry(conn, options...)
	if err != nil {
		t.Fatalf("open the session registry: %v", err)
	}
	t.Cleanup(sessions.StopWatch)
	return &roleRig{url: url, client: client, conn: conn, registry: registry, sessions: sessions,
		database: storetest.Open(t), broker: events.NewBroker()}
}

// deps is the outbox's dependencies as cmd/dispatch wires them: role routes resolved through
// roles.Buckets on the outbox's own client, judged by the package's own windows, which is what
// a deployment that sets neither ENVOY_SESSION_LIVE_WINDOW nor ENVOY_SESSION_GONE_HORIZON
// gives it, and that client the reader of a role's kept lane and its drain cursor, which a
// keep's handover makes its pass over (Deps.KeptLanes).
func (r *roleRig) deps() Deps {
	return r.depsJudgedBy(roles.Windows{})
}

// depsJudgedBy is deps whose role-route holders are judged by windows, as cmd/dispatch judges
// them by the two the deployment gave its listeners (roleHolderWindows, cmd/dispatch/main.go).
func (r *roleRig) depsJudgedBy(windows roles.Windows) Deps {
	holders := roles.NewBuckets(r.client, windows)
	return Deps{Store: r.database, Publisher: r.client, Broker: r.broker, RoleClaims: holders, Sessions: holders,
		KeptLanes: r.client}
}

// claim makes holder role's holder, registered with entry, as a session plugin's registration
// does (claimUnder), and fails the test when that did not come off.
func (r *roleRig) claim(t *testing.T, role, holder string, entry session.SessionEntry) {
	t.Helper()
	if err := r.claimUnder(role, holder, entry); err != nil {
		t.Fatal(err)
	}
}

// claimRegisteredAt makes holder role's holder with a session record whose last registration is
// age old: unresponsive past session.LiveWindow and gone past session.GoneHorizon, which is what
// the resolution judges it by. The record goes to the session bucket directly, since the
// registry stamps every entry it writes with now, and it is written now, so the bucket holds it
// whatever its registration says.
func (r *roleRig) claimRegisteredAt(t *testing.T, role, holder string, age time.Duration) {
	t.Helper()
	if _, err := r.registry.SetRole(holder, "test-machine", role, false); err != nil {
		t.Fatalf("claim %s for %s: %v", role, holder, err)
	}
	record, err := json.Marshal(session.SessionEntry{SelfSubscribed: true, UpdatedAt: time.Now().Add(-age).UnixMilli()})
	if err != nil {
		t.Fatalf("encode the record of %s: %v", holder, err)
	}
	kv, err := r.sessionBucket(t)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	if _, err := kv.Put(holder, record); err != nil {
		t.Fatalf("register %s as last seen %s ago: %v", holder, age, err)
	}
}

// sessionBucket is the session bucket on the test's own connection, which these tests read and
// write records through.
func (r *roleRig) sessionBucket(t *testing.T) (natsgo.KeyValue, error) {
	t.Helper()
	js, err := r.conn.JetStream()
	if err != nil {
		t.Fatalf("open JetStream: %v", err)
	}
	return js.KeyValue(bus.SessionBucket)
}

// waitForExpiredSessionKey waits until the session bucket holds no record for sessionID: its
// maximum age passed and the server dropped the record it stored.
func (r *roleRig) waitForExpiredSessionKey(t *testing.T, sessionID string) {
	t.Helper()
	kv, err := r.sessionBucket(t)
	if err != nil {
		t.Fatalf("open the session bucket: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		_, err := kv.Get(sessionID)
		if errors.Is(err, natsgo.ErrKeyNotFound) {
			return
		}
		if err != nil {
			t.Fatalf("read %s's record: %v", sessionID, err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s's record is still in the session bucket well past its maximum age", sessionID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// roleClaim is role's claim as the role bucket holds it now.
func (r *roleRig) roleClaim(t *testing.T, role string) envoystore.RoleClaim {
	t.Helper()
	claim, err := r.registry.RoleClaim(context.Background(), role)
	if err != nil {
		t.Fatalf("read %s's claim: %v", role, err)
	}
	return claim
}

// countingDeps is the rig's dependencies with the outbox's two counters on a registry of its own,
// so a test can read which reason a send that was not reached was counted under, and which holder
// state a forward held pending was counted on.
func (r *roleRig) countingDeps(t *testing.T) (Deps, *metrics.Registry) {
	t.Helper()
	registry := metrics.New()
	deps := r.deps()
	deps.PublishFailures = registry.NewCounter("envoy_dispatch_outbox_publish_failures_total",
		"Destinations of an outbox publish attempt that were not reached")
	deps.RoleForwardsHeld = registry.NewCounter("envoy_dispatch_outbox_role_forwards_held_total",
		"Role forwards a holder's resolution held pending with nothing sent")
	return deps, registry
}

// failureReasons is what /metrics serves of registry, the counted reasons and holder states among
// it.
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
// nothing is sent to any session - the copy kept for the role's next holder goes to that role's
// kept lane, which is no session's subject (TestARoleRouteToAnUnheldRoleIsKeptForTheRolesNextHolder).
func TestARoleRouteWithNoClaimIsPublishedWithNothingSentToASession(t *testing.T) {
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

// The event's first batch, with no role reader, reaches the issue's topic and leaves the role
// route pending; once the event is due again, the role, which nobody claims, is its only
// unreached destination, and the batch that publishes it keeps the copy for the role's next
// holder, the one send it makes. A role route reached with nothing to send at all is no send of
// its batch, which TestARoleTokenNoClaimCanNameIsSkippedWithNothingKept holds.
func TestABatchWhoseOnlyRouteLeftIsAnUnclaimedRoleKeepsItForTheNextHolder(t *testing.T) {
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
	if second.sent != 1 || second.firstID != event.ID || second.lastID != event.ID || second.published != 1 {
		t.Fatalf("the second batch = %+v, want it to keep the event for the role's next holder and publish it", second)
	}
}

// A holder whose own record is past the gone horizon is gone, and the role route counts as
// reached: the event is published with nothing sent to that holder, its copy kept for the role's
// next holder as an unclaimed role's is, rather than being retried for a session that will not
// answer. The claim is not touched - releasing it is the listener's role-claim reaper's alone,
// and a resolution that released it here would take an unresponsive holder's role on the next
// event (dispatch://INFRA-392).
// The copy on the role's kept lane is the envelope as its sender sent it, kept for the holder the
// role-claim reaper's release of this claim lets claim the role next.
func TestARoleRouteWhoseHolderIsGoneKeepsTheClaimAndIsPublished(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_gone"
	rig.claimRegisteredAt(t, role, holder, session.GoneHorizon+time.Minute)
	claimedAt := rig.roleClaim(t, role).ClaimedAt
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentSubject(holder))
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) == nil {
		attempts, failure, _ := eventState(t, rig.database, event.ID)
		t.Fatalf("the event was not published: attempt_count %d, publish_error %v", attempts, failure)
	}
	if claim := rig.roleClaim(t, role); claim.HolderSessionID != holder || claim.ClaimedAt != claimedAt {
		t.Fatalf("the claim after the scan is %+v, want %s's untouched, claimed at %d", claim, holder, claimedAt)
	}
	kept := keptCopies(t, rig.client, role)
	if len(kept) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the one the outbox kept for its next holder", role, len(kept))
	}
	key := fmt.Sprintf("dispatch-%d", event.ID)
	if kept[0].Topic != contracts.RoleTopicPrefix+role || kept[0].DedupeKey != key || kept[0].EventID != key {
		t.Fatalf("the kept copy is topic %q dedupe key %q event id %q, want the envelope as its sender sent it under %q",
			kept[0].Topic, kept[0].DedupeKey, kept[0].EventID, key)
	}
	if copies := storedCopies(t, rig.client, contracts.AgentSubject(holder)); copies != 0 {
		t.Fatalf("the stream holds %d messages on the gone holder's subject, want none: the copy was kept, not sent", copies)
	}
}

// A holder whose record is there but whose last registration is past the live window is
// unresponsive, not gone: the event stays unpublished, backed off with its attempt and the
// reason, to be retried when the session answers again, and the claim stands.
func TestARoleRouteToAnUnresponsiveHolderStaysPending(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_stale"
	rig.claimRegisteredAt(t, role, holder, session.LiveWindow+time.Minute)
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentSubject(holder))
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) != nil {
		t.Fatal("the event of an unresponsive holder was published")
	}
	var nextAttempt *time.Time
	if err := rig.database.Pool.QueryRow(context.Background(), `select next_attempt_at from events where id = $1`, event.ID).Scan(&nextAttempt); err != nil {
		t.Fatalf("read the event's next attempt: %v", err)
	}
	attempts, failure, _ := eventState(t, rig.database, event.ID)
	if attempts != 1 || nextAttempt == nil || failure == nil || !strings.Contains(*failure, "has not heartbeated") {
		t.Fatalf("attempt_count %d, next_attempt_at %v, publish_error %v: want it backed off naming the unresponsive holder", attempts, nextAttempt, failure)
	}
	if claimed, err := rig.registry.RoleHolder(role); err != nil || claimed != holder {
		t.Fatalf("the claim after the scan names %q (%v), want the unresponsive holder's kept", claimed, err)
	}
}

// A role route's holder is judged by the windows Dispatch was given, never by the package's
// compiled ones. Raising the gone horizon is the answer this change names for a stall longer
// than an hour, and a deployment raises it in Dispatch as well as in its listeners
// (ENVOY_SESSION_GONE_HORIZON, cmd/dispatch's roleHolderWindows): a holder silent for longer
// than the compiled hour but inside the raised horizon is unresponsive, so its event stays
// pending with the claim untouched, where the compiled windows call the same holder gone and
// publish the event with nothing sent.
//
// That publish is the harm, not a slower delivery: the listener, carrying the operator's
// horizon, still calls the holder unresponsive and keeps its role, so an event Dispatch marks
// reached with nothing sent reaches the holder never, however long it stalled
// (dispatch://INFRA-392).
func TestARoleRouteIsJudgedOnTheWindowsDispatchWasGiven(t *testing.T) {
	rig := newRoleRig(t)
	// Silent for longer than the compiled gone horizon, and well inside a horizon raised to
	// three hours for a deployment whose sessions stall for longer than one.
	const silence = session.GoneHorizon + 30*time.Minute
	for _, tc := range []struct {
		name    string
		role    string
		holder  string
		windows roles.Windows
		pending bool
	}{
		{"the horizon the deployment raised", "reviewer", "ses_inside_raised_horizon", roles.Windows{Horizon: 3 * time.Hour}, true},
		{"the compiled windows", "scribe", "ses_past_compiled_horizon", roles.Windows{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig.claimRegisteredAt(t, tc.role, tc.holder, silence)
			claimedAt := rig.roleClaim(t, tc.role).ClaimedAt
			rig.silent(t, contracts.RoleTopicPrefix+tc.role)
			rig.silent(t, contracts.AgentSubject(tc.holder))
			event := rig.routedEvent(t, "T-"+tc.role, tc.role, model.Actor{Kind: "user", ID: "alice"})

			holding(t, rig.depsJudgedBy(tc.windows)).scan(context.Background(), unfenced{})

			published := publishedAt(t, rig.database, event.ID)
			attempts, failure, destinations := eventState(t, rig.database, event.ID)
			reached := strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix)
			switch {
			case tc.pending && published != nil:
				t.Fatalf("the event of a holder inside the raised horizon was published at %s with nothing sent; it is unresponsive, and its event waits for it", published)
			case tc.pending && (attempts != 1 || failure == nil || !strings.Contains(*failure, "unresponsive") || reached):
				t.Fatalf("attempt_count %d, publish_error %v, published_destinations %v: want it backed off naming the unresponsive holder with the role route still pending", attempts, failure, destinations)
			case !tc.pending && published == nil:
				t.Fatalf("the event of a holder past the compiled gone horizon was not published: attempt_count %d, publish_error %v", attempts, failure)
			case !tc.pending && !reached:
				t.Fatalf("published_destinations %v, want the gone holder's role route counted as reached", destinations)
			}
			if claim := rig.roleClaim(t, tc.role); claim.HolderSessionID != tc.holder || claim.ClaimedAt != claimedAt {
				t.Fatalf("the claim after the scan is %+v, want %s's untouched, claimed at %d", claim, tc.holder, claimedAt)
			}
		})
	}
}

// A holder whose session key has expired - the bucket's maximum age passed and the record it held
// is gone - keeps its role and holds its events back. The bucket's maximum age is how long a
// record is stored and no verdict on the session: it can be shorter than the gone horizon, as it
// is here, so a key it no longer holds says the bucket forgot the holder, not that the holder
// died. The event stays pending for the role-claim reaper or the holder's own return to settle,
// and is not published with nothing sent. This is the case that cost a stalled Legion architect
// its role: the resolution read the expired key as a session that is gone and released the claim
// on the next event routed to the role (dispatch://INFRA-392).
func TestARoleRouteWhoseHolderSessionKeyExpiredKeepsTheClaimAndStaysPending(t *testing.T) {
	rig := newRoleRigWithSessionMaxAge(t, time.Second)
	const role, holder = "reviewer", "ses_expired"
	rig.claimRegisteredAt(t, role, holder, session.LiveWindow+time.Minute)
	claimedAt := rig.roleClaim(t, role).ClaimedAt
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentSubject(holder))
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	rig.waitForExpiredSessionKey(t, holder)

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if published := publishedAt(t, rig.database, event.ID); published != nil {
		t.Fatalf("the event of a holder whose session key expired was published at %s with nothing sent", published)
	}
	attempts, failure, destinations := eventState(t, rig.database, event.ID)
	if attempts != 1 || failure == nil || !strings.Contains(*failure, holder) {
		t.Fatalf("attempt_count %d, publish_error %v: want it backed off naming the holder", attempts, failure)
	}
	if strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix) {
		t.Fatalf("published_destinations %v counts the role route reached, want it still pending", destinations)
	}
	if claim := rig.roleClaim(t, role); claim.HolderSessionID != holder || claim.ClaimedAt != claimedAt {
		t.Fatalf("the claim after the scan is %+v, want %s's untouched, claimed at %d", claim, holder, claimedAt)
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

// A role forward its holder's resolution held pending is a wait, not a failure: it is counted on
// the outbox's hold counter under the state its holder was held on, never on the publish-failure
// counter, and its event's back-off is logged below ERROR naming that holder and that state.
// Nothing was sent, so NATS neither refused nor dropped it, while a publish failure and an ERROR
// line are what tell an operator the bus failed a send. The holder here is registered and past the
// live window; a holder nothing dates and a read that failed hold a forward pending the same way
// (forwardOf), and the event is backed off as TestARoleRouteToAnUnresponsiveHolderStaysPending
// holds.
func TestAForwardHeldByItsHoldersResolutionIsAWaitNotAFailure(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "reviewer", "ses_stale_counted"
	rig.claimRegisteredAt(t, role, holder, session.LiveWindow+time.Minute)
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentSubject(holder))
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	deps, registry := rig.countingDeps(t)
	logs := &storetest.LockedLog{}
	logtest.SetDefault(t, slog.NewTextHandler(logs, nil))

	holding(t, deps).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) != nil {
		t.Fatal("the event whose role forward stayed pending was published")
	}
	body := failureReasons(t, registry)
	if !strings.Contains(body, `envoy_dispatch_outbox_role_forwards_held_total{holder_state="unresponsive"} 1`) {
		t.Fatalf("/metrics body = %q, want the pending forward counted a hold under its holder's state", body)
	}
	if strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{`) {
		t.Fatalf("/metrics body = %q counts a forward that sent nothing as a publish failure", body)
	}
	logged := logs.Lines(fmt.Sprintf("event_id=%d ", event.ID))
	if !strings.Contains(logged, `level=INFO msg="dispatch outbox: event waits for its role's holder"`) ||
		!strings.Contains(logged, " holder="+holder+" ") || !strings.Contains(logged, " holder_state=unresponsive ") {
		t.Fatalf("the held event logged %q, want its back-off below ERROR, naming its holder and that holder's state", logged)
	}
	if strings.Contains(logged, "level=ERROR") {
		t.Fatalf("the held event logged %q, want nothing of it at ERROR: a hold is a wait, not a failure", logged)
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

// seedHeldRoleFallbackCopy writes a comment on issue T-1, routed to role, that mentions s1 - the
// session the route resolved to when the comment was written - whose attempt the delivery sweep
// resends; scans its comment.created event through deps, which holds the route's copy as a
// fallback row on the role's own topic; and fails the resend, so the next fallback pass publishes
// that copy (seedHeldFallbackCopy, which holds a session route's copy the same way). It returns
// the comment's id.
func seedHeldRoleFallbackCopy(ctx context.Context, t *testing.T, deps Deps, role string) string {
	t.Helper()
	route := "role:" + role
	seedIssue(t, deps.Store, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	commentID := seedComment(t, deps.Store, "T-1", alice, "@s1 are you there?", nil)
	appendEvent(t, deps.Store, deps.Broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new("T-1"), Body: "@s1 are you there?", Mentions: mentionOf("s1"),
			},
			SuppressRoute: true, SuppressedRoute: route, SuppressedRouteSessionID: new("s1"),
		},
	})
	seedMentionDelivery(t, deps.Store, commentID, "s1", "resumed")
	holding(t, deps).scan(ctx, unfenced{})
	if held := readHeldCopy(t, deps.Store, commentID, contracts.RoleTopicPrefix+role); held.settled {
		t.Fatalf("the scan left the role route's held copy %+v, want it unsettled for the pass", held)
	}
	if _, err := deps.Store.Pool.Exec(ctx, `
		update comment_deliveries set state = 'failed' where comment_id = $1
	`, commentID); err != nil {
		t.Fatalf("fail the resend: %v", err)
	}
	return commentID
}

// seedHeldRoleFallbackCopyMentioningTwo writes a comment on issue T-1, routed to role, that
// mentions first - the session the route resolved to when the comment was written, whose attempt
// the delivery sweep resends - and second, whose own attempt stands as secondOutcome leaves it
// (seedDelivery): "failed" for a mention whose send landed before its attempt was recorded
// failed, "pending" for one whose send is live still. It scans the comment.created event through
// deps, which holds the route's copy as a fallback row keyed by first, and fails first's resend
// alone, so the next fallback pass decides that copy. It is seedHeldRoleFallbackCopy with a
// second mentioned session, which the row's own key does not name. It returns the comment's id.
func seedHeldRoleFallbackCopyMentioningTwo(
	ctx context.Context, t *testing.T, deps Deps, role, first, second, secondOutcome string,
) string {
	t.Helper()
	route := "role:" + role
	seedIssue(t, deps.Store, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	body := "@" + first + " @" + second + " are you there?"
	commentID := seedComment(t, deps.Store, "T-1", alice, body, nil)
	appendEvent(t, deps.Store, deps.Broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new("T-1"), Body: body,
				Mentions: append(mentionOf(first), mentionOf(second)...),
			},
			SuppressRoute: true, SuppressedRoute: route, SuppressedRouteSessionID: new(first),
		},
	})
	seedMentionDelivery(t, deps.Store, commentID, first, "resumed")
	seedMentionDelivery(t, deps.Store, commentID, second, secondOutcome)
	holding(t, deps).scan(ctx, unfenced{})
	if held := readHeldCopy(t, deps.Store, commentID, contracts.RoleTopicPrefix+role); held.settled {
		t.Fatalf("the scan left the role route's held copy %+v, want it unsettled for the pass", held)
	}
	if _, err := deps.Store.Pool.Exec(ctx, `
		update comment_deliveries set state = 'failed' where comment_id = $1 and target = $2
	`, commentID, "session:"+first); err != nil {
		t.Fatalf("fail the resend: %v", err)
	}
	return commentID
}

// A copy held back for a role route reaches the role's holder. The pass resolves the role as a
// batch resolves one its events route to and forwards the copy to the holder's agent subject
// (fallbackRoleForward, roleForward.send); published to notifications.role.* instead it would be
// refused by the stream, which captures no role subject, and the row would count a failed attempt
// and back off for ever with the holder never given the comment. A role nobody claims has no
// holder to forward to, so the copy is kept on that role's kept lane for the session that claims
// the role next, and the stream's acknowledgement of the keep settles the row published, as a
// holder's receipt of a forward settles it.
func TestAHeldCopyOfARoleRouteReachesTheRolesHolder(t *testing.T) {
	const role = "reviewer"
	roleTopic := contracts.RoleTopicPrefix + role

	t.Run("the holder is forwarded the copy", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		database := storetest.Open(t)
		publisher := &recordingPublisher{}
		deps := withHeldRoles(Deps{Store: database, Publisher: publisher, Broker: events.NewBroker()})
		commentID := seedHeldRoleFallbackCopy(ctx, t, deps, role)

		publishFallbackCopies(ctx, deps, unfenced{})

		held := readHeldCopy(t, database, commentID, roleTopic)
		if !held.published || !held.settled || held.attempts != 0 {
			t.Fatalf("the role route's held copy is attempts=%d settled=%v published=%v, want it published by the one pass",
				held.attempts, held.settled, held.published)
		}
		// recordingPublisher answers a publish to a subject the stream does not capture with
		// bus.ErrNotCaptured, and takes a Request only to roleHolder's agent subject under a role
		// forward's dedupe key, so a copy recorded on the role's topic is one that was forwarded.
		var forwarded []contracts.Envelope
		for _, item := range publisher.all() {
			if item.Topic == roleTopic {
				forwarded = append(forwarded, item)
			}
		}
		if len(forwarded) != 1 {
			t.Fatalf("the pass sent %v, want the held copy forwarded once to %s's holder", topicsOf(publisher.all()), role)
		}
		if !strings.HasPrefix(forwarded[0].DedupeKey, contracts.RoleForwardDedupePrefix) {
			t.Fatalf("the forwarded copy's dedupe key is %q, want a role forward's", forwarded[0].DedupeKey)
		}
	})

	t.Run("a role nobody claims keeps the copy for the role's next holder", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		database := storetest.Open(t)
		publisher := &keepRecorder{}
		deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker(),
			RoleClaims: unclaimedRoles, Sessions: unclaimedRoles}
		commentID := seedHeldRoleFallbackCopy(ctx, t, deps, role)

		publishFallbackCopies(ctx, deps, unfenced{})

		held := readHeldCopy(t, database, commentID, roleTopic)
		if !held.settled || !held.published || held.attempts != 0 {
			t.Fatalf("the held copy of an unclaimed role is attempts=%d settled=%v published=%v, want it settled by the keep's acknowledgement",
				held.attempts, held.settled, held.published)
		}
		// keepRecorder records the kept publishes alone, so a copy here is one that went to the
		// role's kept lane rather than to notifications.role.*, which the stream does not capture.
		kept := publisher.keeps()
		if len(kept) != 1 {
			t.Fatalf("the pass made %d kept publishes, want the one copy kept for %s's next holder: %+v", len(kept), role, kept)
		}
		if kept[0].subject != bus.KeptRoleSubject(role) || kept[0].item.Topic != roleTopic {
			t.Fatalf("the keep went to %q with topic %q, want %q with the role's own topic",
				kept[0].subject, kept[0].item.Topic, bus.KeptRoleSubject(role))
		}
		if want := bus.KeptMsgID(kept[0].item.DedupeKey, role); kept[0].msgID != want {
			t.Fatalf("the keep was made under %q, want %q, the id every writer of the lane keeps it under", kept[0].msgID, want)
		}
	})
}

// A copy held back for a role route whose holder is the session the row's key names reaches that
// session under the mention's own dedupe key, not a role forward's. A role fallback row exists
// only for a route a mention of the session that route resolved to suppressed, so the holder a
// pass resolves is usually that very session, and the mention's send may have landed before its
// attempt failed - a receipt timeout, or a send handed the listener before a kill whose resend
// then failed. The host drops a frame whose named dedupe key it claimed already, so a copy keyed
// like the mention is dropped there and the session is shown the comment once; carried under
// contracts.RoleForwardDedupePrefix, as a role route's own forward carries it, it would be a key
// no host has seen, with an event id of its own, and that session would be shown the comment
// twice (fallbackSend.send, contracts.RoleForwardDedupeKey). The prefix is right for every other
// holder, which TestAHeldCopyOfARoleRouteReachesTheRolesHolder holds.
func TestAFallbackPassForwardsARoleCopyToItsOwnMentionedHolderUnderTheMentionsKey(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	rig := newRoleRig(t)
	// s1 is the session seedHeldRoleFallbackCopy mentions and suppresses the route by, and it
	// holds the role the route names.
	const role, holder, target = "reviewer", "s1", "session:s1"
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+role)
	recv := rig.receiver(t, holder, true)
	deps := rig.deps()
	commentID := seedHeldRoleFallbackCopy(ctx, t, deps, role)

	publishFallbackCopies(ctx, deps, unfenced{})

	held := readHeldCopy(t, rig.database, commentID, contracts.RoleTopicPrefix+role)
	if !held.published || !held.settled || held.attempts != 0 {
		t.Fatalf("the role route's held copy is attempts=%d settled=%v published=%v, want it forwarded to %s by the one pass",
			held.attempts, held.settled, held.published, holder)
	}
	forwards := recv.received()
	if len(forwards) != 1 {
		t.Fatalf("%s was sent %d copies, want the held one: %+v", holder, len(forwards), forwards)
	}
	want := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	if forwards[0].DedupeKey != want {
		t.Fatalf("the copy forwarded to %s is keyed %q, want the mention's own key %q, which its host drops",
			holder, forwards[0].DedupeKey, want)
	}
	if strings.HasPrefix(forwards[0].DedupeKey, contracts.RoleForwardDedupePrefix) {
		t.Fatalf("the copy forwarded to the session its own mention names carries %q, want no role forward's prefix",
			forwards[0].DedupeKey)
	}
}

// A copy held for a role route that no live holder could take when the pass ran is kept on the
// role's kept lane, and the session the row's key names, claiming that role while the keep is
// in flight, is handed it by the keep's own handover (handOverKept) under the mention's own
// dedupe key, not a role forward's. The handover forwards the copy by the same method the
// pass's own forward uses (roleForward.forward), so the rule that keys a forward by its holder
// holds wherever that copy reaches that session from
// (contracts.RoleForwardDedupeKey); keyed with the prefix here, the copy would be a key no host
// has seen and would show the mentioned session the comment a second time, exactly as it would
// had the pass resolved the holder itself.
//
// Nothing drains the lane here - no listener runs in this test - so the keep's handover is the
// one path the copy has to the holder, and the stream holds its one forward of it on that
// session's agent subject under the id bus.ForwardMsgID makes of that same key, which is what
// the listener's own drain of the lane would forward it under.
func TestAKeptFallbackCopyHandedToItsOwnMentionedHolderCarriesTheMentionsKey(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	rig := newRoleRig(t)
	// s1 is the session seedHeldRoleFallbackCopy mentions and suppresses the route by; nobody
	// holds the role while the pass decides the row, so its copy is kept for the role's next
	// holder, and s1 is that holder, claiming the role under the keep.
	const role, holder, target = "reviewer", "s1", "session:s1"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	recv := rig.receiver(t, holder, true)
	commentID := seedHeldRoleFallbackCopy(ctx, t, rig.deps(), role)
	claimed := make(chan error, 1)
	publisher := claimedUnderTheKeep(rig.client, func() {
		claimed <- rig.claimUnder(role, holder, session.SessionEntry{SelfSubscribed: true})
	})

	publishFallbackCopies(ctx, rig.publisherOn(publisher), unfenced{})

	if err := <-claimed; err != nil {
		t.Fatalf("the claim made under the keep: %v", err)
	}
	held := readHeldCopy(t, rig.database, commentID, contracts.RoleTopicPrefix+role)
	if !held.published || !held.settled || held.attempts != 0 {
		t.Fatalf("the role route's held copy is attempts=%d settled=%v published=%v, want it kept and settled by the one pass",
			held.attempts, held.settled, held.published)
	}
	want := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	handed := handedNow(t, recv, "the copy kept for a role its own mentioned session claimed under the keep")
	if handed.DedupeKey != want {
		t.Fatalf("the handover carried %q to %s, want the mention's own key %q, which its host drops",
			handed.DedupeKey, holder, want)
	}
	if strings.HasPrefix(handed.DedupeKey, contracts.RoleForwardDedupePrefix) {
		t.Fatalf("the copy handed to the session its own mention names carries %q, want no role forward's prefix",
			handed.DedupeKey)
	}
	subject := contracts.AgentSubject(holder)
	stored := storedMsgIDs(t, rig.conn, subject)
	if len(stored) != 1 || stored[0] != bus.ForwardMsgID(want, subject) {
		t.Fatalf("the stream holds %v on %s, want the handover's one copy under %q, the id a drain's forward of it would carry too",
			stored, subject, bus.ForwardMsgID(want, subject))
	}
}

// A role route whose holder is the session the comment mentions, when that mention's send failed
// before the event was published - a receipt timeout, whose message the listener had already
// published, so the session has it - is forwarded to that holder under the mention's own key, as
// a session route's copy of a failed mention is (TestTheCopyOfAFailedMentionCarriesTheMentionsDedupeKey)
// and as a held copy forwarded to its own mentioned holder is: the host has claimed that key and
// drops the copy, so the session is shown the comment once. Forwarded under the event's key, the
// holder is shown it a second time - observed on the real binaries with a role route held by the
// session a comment mentions and the listener's send answer held past Dispatch's 5 s window: the
// session received agent.<s>.<comment>:session:<s>:steer and envoy.role.forward.dispatch-<event>.
func TestARoleCopyToTheHolderWhoseOwnMentionFailedCarriesTheMentionsKey(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	rig := newRoleRig(t)
	const role, holder, target = "reviewer", "s1", "session:s1"
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, contracts.RoleTopicPrefix+role)
	recv := rig.receiver(t, holder, true)
	deps := rig.deps()
	route := "role:" + role
	seedIssue(t, deps.Store, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	commentID := seedComment(t, deps.Store, "T-1", alice, "@s1 are you there?", nil)
	appendEvent(t, deps.Store, deps.Broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new("T-1"), Body: "@s1 are you there?", Mentions: mentionOf(holder),
			},
			SuppressRoute: true, SuppressedRoute: route, SuppressedRouteSessionID: new(holder),
		},
	})
	seedMentionDelivery(t, deps.Store, commentID, holder, "failed")

	holding(t, deps).scan(ctx, unfenced{})

	forwards := recv.received()
	if len(forwards) != 1 {
		t.Fatalf("%s was sent %d copies, want the route's one copy: %+v", holder, len(forwards), forwards)
	}
	want := contracts.AgentSendDedupeKey(holder, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	if forwards[0].DedupeKey != want {
		t.Fatalf("the role route's copy forwarded to %s, whose own mention failed, is keyed %q, want the mention's own key %q, which its host drops",
			holder, forwards[0].DedupeKey, want)
	}
}

// The same comment, its role claimed by nobody, is kept on that role's kept lane under the failed
// mention's key rather than the event's: the session that claims the role next is forwarded the
// stored key by the listener's drain or by the handover behind the keep, both of which apply
// contracts.RoleForwardDedupeKey (walkKeptRole in cmd/listener/role_keep.go, handOverKept), so a
// copy stored under dispatch-<event id> shows the comment a second time to a later holder whose
// own mention of it failed, exactly as the forward under that key did. No holder is known when
// the copy is kept, so it carries the lowest mentioned session's failed key, which is right for
// every other holder too: a host drops only a key it claimed itself, and a key naming another
// session is one this holder never saw, so it is shown the comment.
func TestARoleCopyKeptForARoleNobodyHoldsCarriesTheFailedMentionsKey(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	const role, mentioned, target = "unclaimed-mentioned", "s1", "session:s1"
	database := storetest.Open(t)
	broker := events.NewBroker()
	publisher := &keepRecorder{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker,
		RoleClaims: unclaimedRoles, Sessions: unclaimedRoles}
	route := "role:" + role
	seedIssue(t, database, "T-1", &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	commentID := seedComment(t, database, "T-1", alice, "@s1 are you there?", nil)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new("T-1"), Body: "@s1 are you there?", Mentions: mentionOf(mentioned),
			},
			SuppressRoute: true, SuppressedRoute: route, SuppressedRouteSessionID: new(mentioned),
		},
	})
	seedMentionDelivery(t, database, commentID, mentioned, "failed")

	holding(t, deps).scan(ctx, unfenced{})

	if publishedAt(t, database, event.ID) == nil {
		_, failure, _ := eventState(t, database, event.ID)
		t.Fatalf("the event whose only route is the unclaimed role was not published: publish_error %v", failure)
	}
	kept := publisher.keeps()
	if len(kept) != 1 {
		t.Fatalf("the outbox made %d kept publishes, want the one copy kept for %s's next holder: %+v", len(kept), role, kept)
	}
	want := contracts.AgentSendDedupeKey(mentioned, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	if kept[0].item.DedupeKey != want {
		t.Fatalf("the copy kept for %s's next holder is keyed %q, want the failed mention's own key %q, which the holder it names drops",
			role, kept[0].item.DedupeKey, want)
	}
	if kept[0].msgID != bus.KeptMsgID(want, role) {
		t.Fatalf("the keep was made under %q, want %q, the id every writer of the lane keeps that copy under",
			kept[0].msgID, bus.KeptMsgID(want, role))
	}
}

// A comment that mentions two sessions, both of whose mentions landed and were then recorded
// failed, is kept for the role nobody holds under one key, and either of them may be the session
// that claims the role next. The copy the handover behind the keep hands to the one that does
// (handOverKept) must carry that holder's own failed mention, not the key the copy was stored
// under: a host drops only a key it has claimed itself, so the second mentioned session, handed
// the first one's key with contracts.RoleForwardDedupePrefix in front of it, is shown the
// comment a second time - its own mention's send having landed before the attempt failed.
//
// It is TestAKeptFallbackCopyHandedToItsOwnMentionedHolderCarriesTheMentionsKey one door further
// out: there the holder is the session the stored key names, here it is the other mentioned
// session, which the keep could not know. The kept copy therefore carries every mentioned
// session's own key (contracts.Envelope.MentionDedupeKeys) and whoever forwards it picks the
// holder's, here the outbox's own handover, and under the listener's drain the same copy reaches
// the same holder under the same key
// (TestAKeptCopyReachingASecondMentionedHolderCarriesItsOwnMentionsKey in cmd/listener).
//
// Nothing drains the lane here - no listener runs in this test - so the keep's handover is the
// one path the copy has to the holder, and the stream holds its one forward of it under the id
// bus.ForwardMsgID makes of that holder's key, which is what the listener's own drain would
// forward it under.
func TestAKeptCopyHandedToTheSecondMentionedHolderCarriesItsOwnMentionsKey(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	rig := newRoleRig(t)
	const role, key = "kept-for-two-mentioned-sessions", "T-1"
	const first, second = "ses_a_mentioned_first", "ses_b_mentioned_second"
	const target = "session:" + second
	rig.silent(t, contracts.RoleTopicPrefix+role)
	recv := rig.receiver(t, second, true)
	route := "role:" + role
	seedIssue(t, rig.database, key, &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	body := "@" + first + " @" + second + " are you there?"
	commentID := seedComment(t, rig.database, key, alice, body, nil)
	// The route resolved to the first mentioned session when the comment was written, so the
	// comment names that route suppressed by it; a failed mention suppresses nothing, so the
	// event's own publish carries the role route.
	event := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new(key), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new(key), Body: body,
				Mentions: append(mentionOf(first), mentionOf(second)...),
			},
			SuppressRoute: true, SuppressedRoute: route, SuppressedRouteSessionID: new(first),
		},
	})
	seedMentionDelivery(t, rig.database, commentID, first, "failed")
	seedMentionDelivery(t, rig.database, commentID, second, "failed")
	// The second mentioned session, not the first, is the one that claims the role, and it claims
	// it inside the window the keep's own race leaves open.
	claimed := make(chan error, 1)
	publisher := claimedUnderTheKeep(rig.client, func() {
		claimed <- rig.claimUnder(role, second, session.SessionEntry{SelfSubscribed: true})
	})

	holding(t, rig.publisherOn(publisher)).scan(ctx, unfenced{})

	if err := <-claimed; err != nil {
		t.Fatalf("the claim made under the keep: %v", err)
	}
	if publishedAt(t, rig.database, event.ID) == nil {
		attempts, failure, _ := eventState(t, rig.database, event.ID)
		t.Fatalf("the event whose only route is the unheld role was not published: attempt_count %d, publish_error %v",
			attempts, failure)
	}
	want := contracts.AgentSendDedupeKey(second, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	handed := handedNow(t, recv, "the copy kept for a role the comment's second mentioned session claimed under the keep")
	if handed.DedupeKey != want {
		t.Fatalf("the handover carried %q to %s, whose own mention's send landed before its attempt failed, want that mention's own key %q, which its host drops",
			handed.DedupeKey, second, want)
	}
	if strings.HasPrefix(handed.DedupeKey, contracts.RoleForwardDedupePrefix) {
		t.Fatalf("the copy handed to the session whose own mention it repeats carries %q, want no role forward's prefix: a key no host has claimed shows the comment a second time",
			handed.DedupeKey)
	}
	subject := contracts.AgentSubject(second)
	stored := storedMsgIDs(t, rig.conn, subject)
	if len(stored) != 1 || stored[0] != bus.ForwardMsgID(want, subject) {
		t.Fatalf("the stream holds %v on %s, want the handover's one copy under %q, the id a drain's forward of it would carry too",
			stored, subject, bus.ForwardMsgID(want, subject))
	}
}

// A comment on an issue routed to a role nobody holds, both of whose mentions were delivered,
// is kept for the role's next holder under the event's own key: no mention of it failed, so
// there is no failed mention's key to store (roleFallbackKey). The map the copy carries is what
// keys it for the session that claims the role afterwards, and a session this comment mentioned
// claimed that mention's key at its host when the send landed, so a copy handed to it under the
// event's key with contracts.RoleForwardDedupePrefix in front - a key no host has claimed -
// shows it the comment a second time, beside the mention it already has.
//
// It is TestAKeptCopyHandedToTheSecondMentionedHolderCarriesItsOwnMentionsKey with the mentions
// delivered rather than failed, which is why the map the keep stores holds every settled
// mention of the comment and not its failed ones alone (mentionDeliveries.settledKeys,
// contracts.Envelope.MentionDedupeKeys).
//
// Nothing drains the lane here - no listener runs in this test - so the keep's handover is the
// one path the copy has to the holder, and the stream holds its one forward of it under the id
// bus.ForwardMsgID makes of that holder's key.
func TestAKeptCopyHandedToAHolderWhoseMentionLandedCarriesItsOwnMentionsKey(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	rig := newRoleRig(t)
	const role, key = "kept-for-two-delivered-mentions", "T-1"
	const first, second = "ses_a_mention_landed", "ses_b_mention_landed"
	const target = "session:" + second
	rig.silent(t, contracts.RoleTopicPrefix+role)
	recv := rig.receiver(t, second, true)
	route := "role:" + role
	seedIssue(t, rig.database, key, &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	body := "@" + first + " @" + second + " are you there?"
	commentID := seedComment(t, rig.database, key, alice, body, nil)
	// The route is the role itself and no mention of it, so the comment suppresses nothing and
	// the role route is published: each mentioned session's own copy is left to the mention
	// that delivered it (destinations.leftToMention), and the role no session holds is kept for.
	event := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new(key), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{
				ID: commentID, IssueKey: new(key), Body: body,
				Mentions: append(mentionOf(first), mentionOf(second)...),
			},
		},
	})
	seedMentionDelivery(t, rig.database, commentID, first, "sent")
	seedMentionDelivery(t, rig.database, commentID, second, "sent")
	// The second mentioned session claims the role inside the window the keep's own race leaves
	// open, so the handover behind the keep is what hands it the copy.
	claimed := make(chan error, 1)
	publisher := claimedUnderTheKeep(rig.client, func() {
		claimed <- rig.claimUnder(role, second, session.SessionEntry{SelfSubscribed: true})
	})

	holding(t, rig.publisherOn(publisher)).scan(ctx, unfenced{})

	if err := <-claimed; err != nil {
		t.Fatalf("the claim made under the keep: %v", err)
	}
	if publishedAt(t, rig.database, event.ID) == nil {
		attempts, failure, _ := eventState(t, rig.database, event.ID)
		t.Fatalf("the event whose only route is the unheld role was not published: attempt_count %d, publish_error %v",
			attempts, failure)
	}
	want := contracts.AgentSendDedupeKey(second, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	handed := handedNow(t, recv, "the copy kept for a role a session the comment mentioned claimed under the keep")
	if handed.DedupeKey != want {
		t.Fatalf("the handover carried %q to %s, whose own mention of the comment was delivered, want that mention's own key %q, which its host drops",
			handed.DedupeKey, second, want)
	}
	if strings.HasPrefix(handed.DedupeKey, contracts.RoleForwardDedupePrefix) {
		t.Fatalf("the copy handed to the session whose own mention delivered the comment carries %q, want no role forward's prefix: a key no host has claimed shows the comment a second time",
			handed.DedupeKey)
	}
	subject := contracts.AgentSubject(second)
	stored := storedMsgIDs(t, rig.conn, subject)
	if len(stored) != 1 || stored[0] != bus.ForwardMsgID(want, subject) {
		t.Fatalf("the stream holds %v on %s, want the handover's one copy under %q, the id a drain's forward of it would carry too",
			stored, subject, bus.ForwardMsgID(want, subject))
	}
}

// roleRoutedMentioningComment seeds an issue routed to role, a comment on it mentioning each of
// mentioned, the comment.created event of it, and one attempt of each mention in the state
// outcome names (seedDelivery). The route is the role itself and no mention of it, so the
// comment suppresses nothing and the role route is published.
func roleRoutedMentioningComment(
	t *testing.T, database *store.Store, broker *events.Broker, role, outcome string, mentioned ...string,
) (commentID string, event model.Event) {
	t.Helper()
	const key = "T-1"
	route := "role:" + role
	seedIssue(t, database, key, &route)
	alice := model.Actor{Kind: "user", ID: "alice"}
	body, mentions := "", []model.Mention(nil)
	for _, sessionID := range mentioned {
		body += "@" + sessionID + " "
		mentions = append(mentions, mentionOf(sessionID)...)
	}
	body += "are you there?"
	commentID = seedComment(t, database, key, alice, body, nil)
	event = appendEvent(t, database, broker, model.Event{
		IssueKey: new(key), Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{
			Comment: model.Comment{ID: commentID, IssueKey: new(key), Body: body, Mentions: mentions},
		},
	})
	for _, sessionID := range mentioned {
		seedMentionDelivery(t, database, commentID, sessionID, outcome)
	}
	return commentID, event
}

// A comment on an issue routed to a role nobody holds is read by the outbox within a millisecond
// of its commit, before either mention's send has settled: the attempts are live
// (model.MentionLiveSQL) and mentionDeliveries.settledKeys is empty. A copy kept then carries no
// mention_dedupe_keys at all, and the mentioned session that claims the role next is handed it
// under the stored key with contracts.RoleForwardDedupePrefix in front - a key no host has
// claimed - beside the mention its own host already took, so it is shown the comment twice: the
// defect the map on the copy closes for a comment whose mentions had settled
// (TestAKeptCopyHandedToAHolderWhoseMentionLandedCarriesItsOwnMentionsKey).
//
// So the keep waits. While an attempt of any of the comment's mentions is live, the role
// destination stays in the event's list marked waiting, sending nothing and holding its key, and
// the event is blocked for mentionPendingDelay (resolveRoleHolders, destination.waiting,
// batchEvent.mentionPending), and the copy is kept once they have settled,
// carrying each mentioned session's own key. Nobody waits on that copy: one is kept only for a
// role no session holds, and it waits for the role's next claim however soon it is stored.
//
// The wait is the attempt's own lease and nothing longer, which is the second case here: an
// attempt still pending past model.DeliveryClaimLapse reads resumed rather than live
// (model.MentionResumedSQL), so the keep proceeds with whatever settled by then - here nothing
// at all - rather than holding the copy for as long as a sender holds its send.
func TestACopyKeptForARoleWaitsForTheCommentsLiveMentions(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	const first, second = "ses_mentioned_first", "ses_mentioned_second"
	mentionKey := func(commentID, sessionID string) string {
		return contracts.AgentSendDedupeKey(sessionID,
			model.CommentMentionIdempotencyKey(commentID, "session:"+sessionID, "steer"))
	}
	keptFor := func(t *testing.T, outcome string) (*store.Store, *keepRecorder, string, model.Event, Deps) {
		t.Helper()
		database := storetest.Open(t)
		broker := events.NewBroker()
		publisher := &keepRecorder{}
		deps := Deps{Store: database, Publisher: publisher, Broker: broker,
			RoleClaims: unclaimedRoles, Sessions: unclaimedRoles}
		commentID, event := roleRoutedMentioningComment(t, database, broker, "unheld-"+outcome, outcome, first, second)
		return database, publisher, commentID, event, deps
	}

	t.Run("a live mention holds the keep until it settles", func(t *testing.T) {
		database, publisher, commentID, event, deps := keptFor(t, "pending")

		holding(t, deps).scan(ctx, unfenced{})

		if kept := publisher.keeps(); len(kept) > 0 {
			t.Fatalf("the outbox kept %d copies while the comment's mentions were still being sent, the first on %s under %q carrying mention_dedupe_keys %v, want none kept yet: a copy naming no mentioned session shows the comment a second time to whichever of them claims the role",
				len(kept), kept[0].subject, kept[0].item.DedupeKey, kept[0].item.MentionDedupeKeys)
		}
		if at := publishedAt(t, database, event.ID); at != nil {
			t.Fatalf("published_at = %v while the keep waits on the comment's mentions", at)
		}
		attempts, due := nextAttempt(t, database, event.ID)
		if attempts != 0 {
			t.Fatalf("attempt_count = %d while the keep waits on a mention, want 0: a wait is no failure", attempts)
		}
		if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
			t.Fatalf("next_attempt_at = %v, want about %v from now, the wait a blocked event takes", due, mentionPendingDelay)
		}

		// Both sends land and their senders settle the attempts, as they do within the lease.
		if _, err := database.Pool.Exec(ctx,
			`update comment_deliveries set state = 'sent' where comment_id = $1`, commentID); err != nil {
			t.Fatalf("settle the comment's mentions: %v", err)
		}
		dueNow(t, database, event.ID)

		holding(t, deps).scan(ctx, unfenced{})

		if publishedAt(t, database, event.ID) == nil {
			attempts, failure, _ := eventState(t, database, event.ID)
			t.Fatalf("the event whose only route is the unheld role was not published once its mentions settled: attempt_count %d, publish_error %v",
				attempts, failure)
		}
		kept := publisher.keeps()
		if len(kept) != 1 {
			t.Fatalf("the outbox made %d kept publishes once the mentions settled, want the one copy kept for the role's next holder: %+v",
				len(kept), kept)
		}
		want := map[string]string{first: mentionKey(commentID, first), second: mentionKey(commentID, second)}
		if !maps.Equal(kept[0].item.MentionDedupeKeys, want) {
			t.Fatalf("the copy kept for the role's next holder carries mention_dedupe_keys %v, want %v: each mentioned session's own settled mention, which is what keys the copy for whichever of them claims the role",
				kept[0].item.MentionDedupeKeys, want)
		}
	})

	t.Run("an attempt live past its lease lets the keep proceed", func(t *testing.T) {
		database, publisher, _, event, deps := keptFor(t, "resumed")

		holding(t, deps).scan(ctx, unfenced{})

		kept := publisher.keeps()
		if len(kept) != 1 {
			attempts, failure, _ := eventState(t, database, event.ID)
			t.Fatalf("the outbox made %d kept publishes for a comment whose attempts are pending past model.DeliveryClaimLapse = %v, want the one copy kept for the role's next holder: attempt_count %d, publish_error %v",
				len(kept), model.DeliveryClaimLapse, attempts, failure)
		}
		if len(kept[0].item.MentionDedupeKeys) != 0 {
			t.Fatalf("the copy kept past the mentions' lease carries mention_dedupe_keys %v, want none: no mention of the comment settled, and the keep waits for no attempt past its own lease",
				kept[0].item.MentionDedupeKeys)
		}
		if publishedAt(t, database, event.ID) == nil {
			attempts, failure, _ := eventState(t, database, event.ID)
			t.Fatalf("the event whose only route is the unheld role was not published though no attempt of its mentions is live: attempt_count %d, publish_error %v",
				attempts, failure)
		}
	})
}

// A copy that waits on a mention still holds its destination's place in its owner's order. Two
// events of one issue are routed to a role nobody holds: a comment whose mention is still being
// sent, whose copy waits for it (TestACopyKeptForARoleWaitsForTheCommentsLiveMentions), and the
// issue update written after it. Were the waiting copy simply dropped from the comment's
// destinations, the update's copy would be kept first, and the kept lane is drained to the
// role's next holder in stream order (walkKeptRole in cmd/listener/role_keep.go), so that holder
// would be shown the update before the comment it updates - the issue's own events out of order
// at the one session that takes them, which is what planChains holds shut for every other
// destination.
//
// So the waiting copy stays in the event's list, marked (destination.waiting), and holds its
// role topic's key: the update is held behind it and kept only once the comment's copy is
// (planChains, batchEvent.held). Nothing is kept while the mention is live, and once it settles
// both copies are kept in the order they were written. The mentioned session's own copy is the
// other case and is left out of the list entirely: the mention delivers that comment to that
// very session, so it holds none of the owner's later events back (destinations.leftTo).
func TestALaterEventIsNotKeptBeforeACopyWaitingOnACommentsMentions(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	const mentioned = "ses_mentioned_before_the_update"
	database := storetest.Open(t)
	broker := events.NewBroker()
	publisher := &keepRecorder{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker,
		RoleClaims: unclaimedRoles, Sessions: unclaimedRoles}
	_, comment := roleRoutedMentioningComment(t, database, broker, "unheld-ordered", "pending", mentioned)
	update := appendEvent(t, database, broker, model.Event{
		IssueKey: new("T-1"), Type: "issue.updated", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: map[string]any{"title": "Renamed while the mention was in flight"},
	})
	if !update.Notify {
		t.Fatalf("the later event does not notify, so it takes no role route and proves nothing: %#v", update)
	}

	holding(t, deps).scan(ctx, unfenced{})

	if kept := publisher.keeps(); len(kept) > 0 {
		t.Fatalf("the outbox kept %d copies while the comment's copy waited on its mention, the first of event %s under %q, want none: the role's next holder would be handed the issue's later event first",
			len(kept), kept[0].item.EventID, kept[0].msgID)
	}
	for _, event := range []model.Event{comment, update} {
		if at := publishedAt(t, database, event.ID); at != nil {
			t.Fatalf("event %d (%s) is published at %v while the comment's role copy waits on its mention", event.ID, event.Type, at)
		}
		attempts, due := nextAttempt(t, database, event.ID)
		if attempts != 0 {
			t.Fatalf("event %d (%s) counts attempt_count = %d while a copy waits on a mention, want 0: a wait is no failure",
				event.ID, event.Type, attempts)
		}
		if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
			t.Fatalf("event %d (%s) is due at %v, want about %v from now, the wait a blocked event takes",
				event.ID, event.Type, due, mentionPendingDelay)
		}
	}

	// The mention's send lands and its sender settles the attempt, as one does within the lease.
	if _, err := database.Pool.Exec(ctx,
		`update comment_deliveries set state = 'sent'`); err != nil {
		t.Fatalf("settle the comment's mention: %v", err)
	}
	dueNow(t, database, comment.ID)
	dueNow(t, database, update.ID)

	holding(t, deps).scan(ctx, unfenced{})

	kept := publisher.keeps()
	eventIDs := make([]string, 0, len(kept))
	for _, copy := range kept {
		eventIDs = append(eventIDs, copy.item.EventID)
	}
	want := []string{fmt.Sprintf("dispatch-%d", comment.ID), fmt.Sprintf("dispatch-%d", update.ID)}
	if !slices.Equal(eventIDs, want) {
		t.Fatalf("the outbox kept %v once the mention settled, want %v: the issue's events reach the role's next holder in the order they were written",
			eventIDs, want)
	}
	for _, event := range []model.Event{comment, update} {
		if publishedAt(t, database, event.ID) == nil {
			attempts, failure, _ := eventState(t, database, event.ID)
			t.Fatalf("event %d (%s) was not published once the mention settled: attempt_count %d, publish_error %v",
				event.ID, event.Type, attempts, failure)
		}
	}
}

// A copy the batch forwards to a role's live holder waits for that holder's own mention of the
// comment, while its send is still in flight (resolveRoleHolders, roleCopyWaitsForAMention). It
// is TestAFallbackCopyForwardedToALiveHolderWaitsForThatHoldersOwnMention on the event's own
// role route, and the same duplicate: the mention is on its way to that session under its own
// key, which no settled map names while its only attempt is live
// (mentionDeliveries.settledKeys), so the forward would go under the event's key with
// contracts.RoleForwardDedupePrefix in front - a key that host never claimed - and the host
// would show the comment twice, once for the copy and once for the mention landing after it.
//
// Nothing else protects it: the route is a role no mention suppressed - the comment's own
// suppressed_route_session_id names whoever the route resolved to when it was written, not this
// holder - and an attempt the sender has not settled is live for the whole of its lease. So the
// forward waits, the destination holding its key and the event read again, and once the mention
// settles the copy goes under that mention's own key, which the host drops (roleFallbackKey).
//
// The wait is the holder's own mention and no other, as the fallback pass's is, and the
// attempt's own lease and nothing longer - the second case here: an attempt still pending past
// model.DeliveryClaimLapse reads resumed rather than live, and the forward goes with whatever
// settled by then, under the prefixed event key.
func TestAForwardToALiveHolderWaitsForThatHoldersOwnMention(t *testing.T) {
	const role = "forwarded-to-a-live-mentioned-holder"
	const holder = "ses_holds_the_role_and_is_mentioned"
	roleTopic := contracts.RoleTopicPrefix + role
	mentionKey := func(commentID string) string {
		return contracts.AgentSendDedupeKey(holder,
			model.CommentMentionIdempotencyKey(commentID, "session:"+holder, "steer"))
	}
	// The holder claimed the role before the comment was published and is mentioned by it, which
	// is the normal shape in the seconds after a comment commits: its own attempt is still live.
	heldBy := func(t *testing.T, outcome string) (*roleRig, *receiver, Deps, string, model.Event) {
		t.Helper()
		rig := newRoleRig(t)
		rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
		rig.silent(t, roleTopic)
		recv := rig.receiver(t, holder, true)
		commentID, event := roleRoutedMentioningComment(t, rig.database, rig.broker, role, outcome, holder)
		return rig, recv, rig.deps(), commentID, event
	}

	t.Run("the holder's live mention holds the forward until it settles", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		rig, recv, deps, commentID, event := heldBy(t, "pending")

		holding(t, deps).scan(ctx, unfenced{})

		if forwards := recv.received(); len(forwards) > 0 {
			t.Fatalf("the batch sent %s %d copies while that holder's own mention of the comment was still being sent, the first under %q, want none yet: a copy under a key its host never claimed shows it the comment a second time beside the mention",
				holder, len(forwards), forwards[0].DedupeKey)
		}
		if at := publishedAt(t, rig.database, event.ID); at != nil {
			t.Fatalf("published_at = %v while the forward waits on the holder's own mention", at)
		}
		attempts, due := nextAttempt(t, rig.database, event.ID)
		if attempts != 0 {
			t.Fatalf("attempt_count = %d while the forward waits on a mention, want 0: a wait is no failure", attempts)
		}
		if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
			t.Fatalf("next_attempt_at = %v, want about %v from now, the wait a blocked event takes", due, mentionPendingDelay)
		}

		// The holder's send lands and its sender settles the attempt, as one does within the
		// lease.
		if _, err := rig.database.Pool.Exec(ctx, `
			update comment_deliveries set state = 'sent' where comment_id = $1 and target = $2
		`, commentID, "session:"+holder); err != nil {
			t.Fatalf("settle the holder's mention: %v", err)
		}
		dueNow(t, rig.database, event.ID)

		holding(t, deps).scan(ctx, unfenced{})

		if publishedAt(t, rig.database, event.ID) == nil {
			attempts, failure, _ := eventState(t, rig.database, event.ID)
			t.Fatalf("the comment was not published once the holder's mention settled: attempt_count %d, publish_error %v",
				attempts, failure)
		}
		forwards := recv.received()
		if len(forwards) != 1 {
			t.Fatalf("%s was sent %d copies, want the role route's once its own mention settled: %+v", holder, len(forwards), forwards)
		}
		if want := mentionKey(commentID); forwards[0].DedupeKey != want {
			t.Fatalf("the copy forwarded to %s, whose own mention of the comment settled while the event waited, is keyed %q, want that mention's own key %q, which its host drops",
				holder, forwards[0].DedupeKey, want)
		}
	})

	t.Run("an attempt live past its lease lets the forward proceed", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		rig, recv, deps, _, event := heldBy(t, "resumed")

		holding(t, deps).scan(ctx, unfenced{})

		forwards := recv.received()
		if len(forwards) != 1 {
			attempts, failure, _ := eventState(t, rig.database, event.ID)
			t.Fatalf("%s was sent %d copies though its own mention is pending past model.DeliveryClaimLapse = %v, want the role route's: attempt_count %d, publish_error %v",
				holder, len(forwards), model.DeliveryClaimLapse, attempts, failure)
		}
		if !strings.HasPrefix(forwards[0].DedupeKey, contracts.RoleForwardDedupePrefix) {
			t.Fatalf("the copy forwarded past the holder's own attempt's lease is keyed %q, want a role forward's prefixed key: no mention of this comment settled for that holder, and the forward waits for no attempt past its lease",
				forwards[0].DedupeKey)
		}
		if publishedAt(t, rig.database, event.ID) == nil {
			attempts, failure, _ := eventState(t, rig.database, event.ID)
			t.Fatalf("the comment was not published though no attempt of its mention is live: attempt_count %d, publish_error %v",
				attempts, failure)
		}
	})
}

// A copy that waits on a mention holds the owner's later events back and holds no copy of its
// own event: the copies of one event have no order among themselves, the order the outbox keeps
// being the one between an owner's successive events (planChains).
//
// The staging is the one a comment written on a routed issue the role's holder claims falls
// into. The holder of the role the issue is routed to is also the session that claims the
// issue, and the comment mentions an unanswered @role:Y, which the payload pins to no session
// and the mention's own sender pinned to that holder after the commit
// (model.MentionSessionSQL, coalesce(session_id, …)). So the holder's own mention is live in
// the role destination's states (destination.mentionStates) while nothing in the payload names
// that session: the event's role forward waits for that mention (roleCopyWaitsForAMention),
// and the claimant copy of the same event, bound for the key that forward holds, is left to
// that same mention, both reading the one comment's attempt rows (destinations.addSession,
// destinations.leftToMention). The holder is sent nothing of the comment in that pass; the
// mention delivers it.
//
// No copy of the event may read that wait. Held by a blocker carrying its own event, the event
// settles on an outcome settleEvent has not written yet, whose zero state holdBack reads as
// released: no delay, no mentionPendingDelay clamp, the batch logging that NATS is unreachable
// and stopping, the event read again at once, and once the attempt's lease lapsed the prefixed
// copy published beside the mention - the duplicate the wait exists to stop. The claimant copy
// of a session whose mention is live is left out of the list altogether, so this event has no
// copy to hold back, and the event settles blocked for mentionPendingDelay all the same.
//
// The owner's later events are held still, which is what the wait is for: the same comment's
// edit is bound for that holder on that same key, waits behind the forward, and reaches the
// holder only once the comment's own copy has.
func TestAWaitingRoleForwardHoldsNoCopyOfItsOwnEvent(t *testing.T) {
	ctx := store.WithTransactionTracking(context.Background())
	var logged lockedBuffer
	logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))

	const role = "routed-to-the-issues-claimant"
	const holder = "ses_claims_the_issue_and_holds_the_role"
	const bystander = "ses_mentioned_by_name"
	const unanswered = "role:unanswered-when-the-comment-was-written"
	roleTopic := contracts.RoleTopicPrefix + role
	holderSubject := contracts.AgentSubject(holder)

	rig := newRoleRig(t)
	rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
	rig.silent(t, roleTopic)
	recv := rig.receiver(t, holder, true)

	alice := model.Actor{Kind: "user", ID: "alice"}
	issue := new("T-1")
	seedIssue(t, rig.database, "T-1", new("role:"+role))
	claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: holder})
	body := "@" + bystander + " @" + unanswered + " are you there?"
	commentID := seedComment(t, rig.database, "T-1", alice, body, nil)
	// The bystander's mention is what has the batch read the comment's attempts at all
	// (eventDestinations); the unanswered role mention is the holder's, pinned to it by its
	// sender and by nothing the payload carries.
	mentions := append(mentionOf(bystander), model.Mention{Target: unanswered, Delivery: "steer"})
	comment := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: issue, Type: "comment.created", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: commentID, IssueKey: issue, Body: body, Mentions: mentions}},
	})
	seedMentionDelivery(t, rig.database, commentID, bystander, "sent")
	seedDelivery(t, rig.database, commentID, unanswered, holder, "pending")
	edit := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: issue, Type: "comment.edited", Actor: alice,
		Payload: model.CommentEventPayload{Comment: model.Comment{
			ID: commentID, IssueKey: issue, Body: body + " (edited)"}},
	})
	if !comment.Notify || !edit.Notify {
		t.Fatalf("comment.created notifies %v and comment.edited %v, want both: an event that does not notify takes neither the route nor the claimant route and proves nothing",
			comment.Notify, edit.Notify)
	}

	holding(t, rig.deps()).scan(ctx, unfenced{})

	// The defect's own signature: an event held by a blocker carrying itself reads an outcome
	// settleEvent has not written, whose zero state holdBack takes for a released one.
	released := 0
	var firstReleased string
	for line := range strings.SplitSeq(logged.String(), "\n") {
		if strings.Contains(line, "NATS is unreachable") {
			released++
			if firstReleased == "" {
				firstReleased = line
			}
		}
	}
	if released > 0 {
		t.Fatalf("the batch logged %d released outcomes with NATS up, the first %q: the claimant copy of this very event was held by the wait its own role forward to %s wrote, a blocker carrying the event itself (planChains, blocker.event), and holdBack read that event's unwritten outcome as released",
			released, firstReleased, holder)
	}
	_, _, reached := eventState(t, rig.database, comment.ID)
	if slices.Contains(reached, holderSubject) || slices.Contains(reached, roleTopic) {
		t.Fatalf("the comment reached %v, want neither %s nor %s among them: the role forward waits for the holder's live mention, and the claimant copy of the same holder is left to that very mention",
			reached, holderSubject, roleTopic)
	}
	attempts, due := nextAttempt(t, rig.database, comment.ID)
	if attempts != 0 {
		t.Fatalf("attempt_count = %d while the forward waits on a mention, want 0: a wait is no failure", attempts)
	}
	if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
		t.Fatalf("next_attempt_at = %v, want about %v from now, the wait a blocked event takes", due, mentionPendingDelay)
	}
	if at := publishedAt(t, rig.database, comment.ID); at != nil {
		t.Fatalf("published_at = %v while the role forward waits on the holder's own mention", at)
	}
	if forwards := recv.received(); len(forwards) > 0 {
		t.Fatalf("%s was sent %+v while its own mention of the comment was still being sent, want nothing: the forward waits for that mention and the claimant copy is left to it",
			holder, forwards)
	}

	// Round 15's finding: the owner's later events bound for that holder still wait behind the
	// waiting forward.
	_, _, editReached := eventState(t, rig.database, edit.ID)
	if slices.Contains(editReached, holderSubject) {
		t.Fatalf("the edit reached %v, want %s not among them: the owner's later events bound for that holder wait behind the forward, so that the holder is not shown the edit before the comment",
			editReached, holderSubject)
	}
	if at := publishedAt(t, rig.database, edit.ID); at != nil {
		t.Fatalf("the edit is published at %v while the comment's forward waits on the holder's own mention", at)
	}
	attempts, due = nextAttempt(t, rig.database, edit.ID)
	if attempts != 0 {
		t.Fatalf("the edit counts attempt_count = %d while it waits behind the comment's forward, want 0: a wait is no failure", attempts)
	}
	if due == nil || !due.After(time.Now()) || due.After(time.Now().Add(mentionPendingDelay+time.Second)) {
		t.Fatalf("the edit is due at %v, want about %v from now, the wait its blocker takes", due, mentionPendingDelay)
	}

	// The holder's own mention lands and its sender settles the attempt, as one does within the
	// lease.
	if _, err := rig.database.Pool.Exec(ctx, `
		update comment_deliveries set state = 'sent' where comment_id = $1 and target = $2
	`, commentID, unanswered); err != nil {
		t.Fatalf("settle the holder's mention: %v", err)
	}
	dueNow(t, rig.database, comment.ID)
	dueNow(t, rig.database, edit.ID)

	holding(t, rig.deps()).scan(ctx, unfenced{})

	for _, event := range []model.Event{comment, edit} {
		if publishedAt(t, rig.database, event.ID) == nil {
			attempts, failure, destinations := eventState(t, rig.database, event.ID)
			t.Fatalf("event %d (%s) was not published once the holder's mention settled: attempt_count %d, publish_error %v, reached %v",
				event.ID, event.Type, attempts, failure, destinations)
		}
	}
	var order []string
	for _, item := range recv.received() {
		order = append(order, item.EventID)
	}
	editID := fmt.Sprintf("dispatch-%d", edit.ID)
	seenEdit := false
	for _, id := range order {
		if id == editID {
			seenEdit = true
		} else if seenEdit {
			t.Fatalf("%s was sent %v, want every copy of the comment before the edit's: both ride that holder's one chain, in the order the events were written",
				holder, order)
		}
	}
	if !seenEdit {
		t.Fatalf("%s was sent %v, want the edit's copies once the comment's forward had gone", holder, order)
	}
}

// A session an unanswered @role: mention named is shown the comment once. The staging is the
// one a comment written on a routed issue the role's holder claims falls into, and the one the
// host's own frames showed: the session that claims the issue holds the role it is routed to,
// and the comment mentions that role and nothing else. Its listener lookup went unanswered
// within the comment's 250 ms window, so the payload's mention names no session at all and the
// mention's own sender pinned this one on the comment_deliveries row after the commit
// (coalesce(session_id, $6) in internal/dispatch/api/comment_delivery.go, model.MentionSessionSQL).
//
// The copy that showed it the comment a second time was the claimant's: the route's own copy is
// left to the mention of the target it carries (suppressesCurrentRoute, round 35), but the
// claimant, author and follower copies read the payload's mentioned sessions, which name this
// session nowhere, so the event's own copy went out under dispatch-<event id> - a key no mention
// claimed - beside the mention its host had taken under
// agent.<session>.<comment>:role:<role>:steer. The attempt rows name it either way, and they are
// what the keying of those copies is read from already, so every copy of a new comment bound for
// a session's own topic asks them (destinations.addSession, destinations.leftToMention).
//
// The rule is the whole of leftTo and not the attempt row's existence: a mention that failed
// leaves the session its copy, under that mention's own key, which is the second case here. The
// host drops that copy only when the failed send had landed after all, and a session no mention
// reached is left with nothing.
//
// Nothing else of the event is lost either: the other owner's comment in the same batch reaches
// the session that claims that issue, whom no mention of it names, under the event's own key.
func TestACommentAnUnansweredRoleMentionPinnedReachesThatSessionOnce(t *testing.T) {
	const role = "routed-to-the-session-its-mention-pinned"
	const holder = "ses_claims_the_issue_and_holds_the_role"
	const bystander = "ses_claims_the_other_issue"
	const elsewhere = "ses_mentioned_on_the_other_issue"
	target := "role:" + role
	roleTopic := contracts.RoleTopicPrefix + role
	holderSubject := contracts.AgentSubject(holder)
	mentionKey := func(commentID string) string {
		return contracts.AgentSendDedupeKey(holder,
			model.CommentMentionIdempotencyKey(commentID, target, "steer"))
	}
	// staged is that comment, with its mention's attempt in outcome, and another owner's
	// comment beside it in the same batch, mentioning a session of its own and claimed by one
	// no mention names.
	staged := func(t *testing.T, outcome string) (
		rig *roleRig, pinned, other *receiver, commentID string, comment, apart model.Event,
	) {
		t.Helper()
		rig = newRoleRig(t)
		rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
		rig.silent(t, roleTopic)
		pinned = rig.receiver(t, holder, true)
		other = rig.receiver(t, bystander, true)

		alice := model.Actor{Kind: "user", ID: "alice"}
		route := target
		seedIssue(t, rig.database, "T-1", &route)
		claimIssue(t, rig.database, "T-1", model.Actor{Kind: "session", ID: holder})
		body := "@" + target + " are you there?"
		commentID = seedComment(t, rig.database, "T-1", alice, body, nil)
		comment = appendEvent(t, rig.database, rig.broker, model.Event{
			IssueKey: new("T-1"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{
				Comment: model.Comment{ID: commentID, IssueKey: new("T-1"), Body: body,
					Mentions: []model.Mention{{Target: target, Delivery: "steer"}}},
				// What the creating transaction records of a mention whose lookup went
				// unanswered and whose target is the issue's route: the route string alone,
				// no session.
				SuppressRoute: true, SuppressedRoute: route,
			},
		})
		seedDelivery(t, rig.database, commentID, target, holder, outcome)

		if _, err := rig.database.Pool.Exec(context.Background(), `
			insert into issues (key, project_key, number, title, route, created_by, rank)
			values ('T-2', 'TT', 2, 'The other owner', null, '{"kind":"user","id":"alice"}', 'V')
		`); err != nil {
			t.Fatalf("create the other owner's issue: %v", err)
		}
		claimIssue(t, rig.database, "T-2", model.Actor{Kind: "session", ID: bystander})
		apartBody := "@" + elsewhere + " and you?"
		apartID := seedComment(t, rig.database, "T-2", alice, apartBody, nil)
		apart = appendEvent(t, rig.database, rig.broker, model.Event{
			IssueKey: new("T-2"), Type: "comment.created", Actor: alice,
			Payload: model.CommentEventPayload{
				Comment: model.Comment{ID: apartID, IssueKey: new("T-2"), Body: apartBody,
					Mentions: mentionOf(elsewhere)},
			},
		})
		seedMentionDelivery(t, rig.database, apartID, elsewhere, "sent")
		return rig, pinned, other, commentID, comment, apart
	}

	// claimantCopyOf fails t unless the session that claims the other owner's issue was sent
	// that comment's claimant copy, which no mention of it names it for.
	claimantCopyOf := func(t *testing.T, rig *roleRig, other *receiver, apart model.Event) {
		t.Helper()
		if at := publishedAt(t, rig.database, apart.ID); at == nil {
			attempts, failure, reached := eventState(t, rig.database, apart.ID)
			t.Fatalf("the other owner's comment was not published: attempt_count %d, publish_error %v, reached %v",
				attempts, failure, reached)
		}
		_, _, reached := eventState(t, rig.database, apart.ID)
		if !slices.Contains(reached, contracts.AgentSubject(bystander)) {
			t.Fatalf("the other owner's comment reached %v, want %s among them: a session no attempt of that comment names takes its claimant copy as if it had not been mentioned",
				reached, contracts.AgentSubject(bystander))
		}
		waitFor(t, 10*time.Second, "the other owner's claimant copy", func() bool {
			return len(other.received()) > 0
		})
		copies := other.received()
		if len(copies) != 1 || copies[0].EventID != fmt.Sprintf("dispatch-%d", apart.ID) ||
			copies[0].DedupeKey != fmt.Sprintf("dispatch-%d", apart.ID) {
			t.Fatalf("%s was sent %+v, want the claimant copy of event %d alone, under the event's own key",
				bystander, copies, apart.ID)
		}
	}

	t.Run("the pinned session's own copies are left to its mention", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		rig, pinned, other, commentID, comment, apart := staged(t, "sent")

		holding(t, rig.deps()).scan(ctx, unfenced{})

		if at := publishedAt(t, rig.database, comment.ID); at == nil {
			attempts, failure, reached := eventState(t, rig.database, comment.ID)
			t.Fatalf("the comment was not published though its mention delivered it: attempt_count %d, publish_error %v, reached %v",
				attempts, failure, reached)
		}
		_, _, reached := eventState(t, rig.database, comment.ID)
		if slices.Contains(reached, holderSubject) {
			t.Fatalf("the comment reached %v, want %s not among them: %s is the issue's claimant and the session the mention of %s was pinned to, so its claimant copy - under dispatch-%d, a key no mention of this comment claimed - shows it the comment a second time beside that mention",
				reached, holderSubject, holder, target, comment.ID)
		}
		claimantCopyOf(t, rig, other, apart)
		for _, item := range pinned.received() {
			t.Fatalf("%s was sent %+v, want nothing of the comment: its mention of %s delivered it, and every copy of the comment bound for that session's own topic is left to that mention (want no copy, or one under %q)",
				holder, item, target, mentionKey(commentID))
		}
	})

	t.Run("a pinned session whose mention failed is published to under that mention's key", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		rig, pinned, other, commentID, comment, apart := staged(t, "failed")

		holding(t, rig.deps()).scan(ctx, unfenced{})

		if at := publishedAt(t, rig.database, comment.ID); at == nil {
			attempts, failure, reached := eventState(t, rig.database, comment.ID)
			t.Fatalf("the comment was not published though its mention failed: attempt_count %d, publish_error %v, reached %v",
				attempts, failure, reached)
		}
		_, _, reached := eventState(t, rig.database, comment.ID)
		if !slices.Contains(reached, holderSubject) {
			t.Fatalf("the comment reached %v, want %s among them: the mention of %s failed, so the session it was pinned to is owed the outbox's copy and nothing else delivers it",
				reached, holderSubject, target)
		}
		waitFor(t, 10*time.Second, "the copy owed to the session whose mention failed", func() bool {
			return len(pinned.received()) > 0
		})
		for _, item := range pinned.received() {
			if item.DedupeKey != mentionKey(commentID) {
				t.Fatalf("%s was sent a copy keyed %q, want its failed mention's own key %q: a send that landed before its attempt failed gave that host this key, which is what has it drop the copy",
					holder, item.DedupeKey, mentionKey(commentID))
			}
		}
		claimantCopyOf(t, rig, other, apart)
	})
}

// A copy held back for a role route reaches a holder the row's own key does not name under that
// holder's own mention of the comment, wherever it reaches it from. The row is keyed by the
// session the route resolved to when the comment was written, whose mention suppressed the
// route; the same comment mentions a second session, whose own mention's send landed before its
// attempt was recorded failed, and that session holds the role by the time the pass runs -
// having claimed it after the comment was written, or under the pass's own keep. Handed the
// row's key with contracts.RoleForwardDedupePrefix in front of it - a key naming the first
// session, which this host never claimed - it is shown the comment a second time, beside the
// mention it has.
//
// It is TestAKeptCopyHandedToTheSecondMentionedHolderCarriesItsOwnMentionsKey and
// TestAKeptCopyHandedToAHolderWhoseMentionLandedCarriesItsOwnMentionsKey on the fallback pass's
// own role forward, which carried no per-session keys at all: the pass reads every mention of
// the comment to decide the row (mentionDeliveriesOf), so its forward carries the maps the
// event's own role route carries (decideFallbackCopy, roleForward.settledKeys), and the holder
// is keyed by its own settled mention whether the pass forwards the copy to it or keeps the copy
// for it (roleFallbackKey, contracts.Envelope.MentionDedupeKeys).
//
// Nothing drains the lane in the keep's case - no listener runs in this test - so the keep's
// handover is the one path the copy has to the holder, and the stream holds its one forward of
// it under the id bus.ForwardMsgID makes of that holder's key, which is what the listener's own
// drain of the lane would forward it under.
func TestAFallbackRoleCopyReachingASecondMentionedHolderCarriesItsOwnMentionsKey(t *testing.T) {
	const role = "held-for-two-mentioned-sessions"
	const first, second = "ses_a_mentioned_first", "ses_b_mentioned_second"
	const target = "session:" + second
	roleTopic := contracts.RoleTopicPrefix + role

	t.Run("the holder the pass resolves", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		rig := newRoleRig(t)
		// The second mentioned session claimed the role between the comment's write and this
		// pass, so the pass resolves it as the holder and forwards the held copy to it.
		rig.claim(t, role, second, session.SessionEntry{SelfSubscribed: true})
		rig.silent(t, roleTopic)
		recv := rig.receiver(t, second, true)
		deps := rig.deps()
		commentID := seedHeldRoleFallbackCopyMentioningTwo(ctx, t, deps, role, first, second, "failed")

		publishFallbackCopies(ctx, deps, unfenced{})

		held := readHeldCopy(t, rig.database, commentID, roleTopic)
		if !held.published || !held.settled || held.attempts != 0 {
			t.Fatalf("the role route's held copy is attempts=%d settled=%v published=%v, want it forwarded to %s by the one pass",
				held.attempts, held.settled, held.published, second)
		}
		forwards := recv.received()
		if len(forwards) != 1 {
			t.Fatalf("%s was sent %d copies, want the held one: %+v", second, len(forwards), forwards)
		}
		want := contracts.AgentSendDedupeKey(second, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
		if forwards[0].DedupeKey != want {
			t.Fatalf("the copy forwarded to %s, whose own mention of the comment settled, is keyed %q, want that mention's own key %q, which its host drops",
				second, forwards[0].DedupeKey, want)
		}
		if strings.HasPrefix(forwards[0].DedupeKey, contracts.RoleForwardDedupePrefix) {
			t.Fatalf("the copy forwarded to the session whose own mention it repeats carries %q, want no role forward's prefix: a key no host has claimed shows the comment a second time",
				forwards[0].DedupeKey)
		}
	})

	t.Run("the holder that claims the role under the keep", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		rig := newRoleRig(t)
		// Nobody holds the role while the pass decides the row, so its copy is kept for the
		// role's next holder, and the second mentioned session is that holder, claiming the role
		// inside the window the keep's own race leaves open.
		rig.silent(t, roleTopic)
		recv := rig.receiver(t, second, true)
		commentID := seedHeldRoleFallbackCopyMentioningTwo(ctx, t, rig.deps(), role, first, second, "failed")
		claimed := make(chan error, 1)
		publisher := claimedUnderTheKeep(rig.client, func() {
			claimed <- rig.claimUnder(role, second, session.SessionEntry{SelfSubscribed: true})
		})

		publishFallbackCopies(ctx, rig.publisherOn(publisher), unfenced{})

		if err := <-claimed; err != nil {
			t.Fatalf("the claim made under the keep: %v", err)
		}
		held := readHeldCopy(t, rig.database, commentID, roleTopic)
		if !held.published || !held.settled || held.attempts != 0 {
			t.Fatalf("the role route's held copy is attempts=%d settled=%v published=%v, want it kept and settled by the one pass",
				held.attempts, held.settled, held.published)
		}
		want := contracts.AgentSendDedupeKey(second, model.CommentMentionIdempotencyKey(commentID, target, "steer"))
		handed := handedNow(t, recv, "the copy kept for a role the comment's second mentioned session claimed under the keep")
		if handed.DedupeKey != want {
			t.Fatalf("the handover carried %q to %s, whose own mention of the comment settled, want that mention's own key %q, which its host drops",
				handed.DedupeKey, second, want)
		}
		if strings.HasPrefix(handed.DedupeKey, contracts.RoleForwardDedupePrefix) {
			t.Fatalf("the copy handed to the session whose own mention it repeats carries %q, want no role forward's prefix: a key no host has claimed shows the comment a second time",
				handed.DedupeKey)
		}
		subject := contracts.AgentSubject(second)
		stored := storedMsgIDs(t, rig.conn, subject)
		if len(stored) != 1 || stored[0] != bus.ForwardMsgID(want, subject) {
			t.Fatalf("the stream holds %v on %s, want the handover's one copy under %q, the id a drain's forward of it would carry too",
				stored, subject, bus.ForwardMsgID(want, subject))
		}
	})
}

// A copy the fallback pass keeps for a role nobody holds waits for the comment's own live
// mentions, as the keep of a batch's role route does (roleCopyWaitsForAMention,
// TestACopyKeptForARoleWaitsForTheCommentsLiveMentions). The row is no reading of the comment's
// other mentions: it was written because the attempt of its own key had lapsed past that
// attempt's lease (model.MentionResumedSQL), while another session the comment mentions can have
// a live attempt still - a Retry of that mention opens a fresh one at any time - and a copy kept
// then carries a mention_dedupe_keys naming that session nowhere (mentionDeliveries.settledKeys,
// mentionDeliveries.anyLive). The keep is the last reading of the mentions that copy gets:
// whoever hands it to the session that claims the role next - the keep's own handover, or the
// listener's drain of the kept lane - keys it by that map, so the session whose mention was in
// flight takes it under the prefixed stored key beside the mention its own host claimed, and is
// shown the comment twice.
//
// So this keep waits too (decideFallbackCopy): the row is left to a later pass for
// mentionPendingDelay, counting no attempt, and the copy is kept once the mentions have settled,
// carrying each mentioned session's own key. Nobody waits on that copy: one is kept only for a
// role no session holds, and it waits for the role's next claim however soon it is stored.
//
// The wait is the attempt's own lease and nothing longer, which is the second case here: an
// attempt pending past model.DeliveryClaimLapse reads resumed rather than live, so the keep
// proceeds with whatever settled by then - here the row's own failed mention alone - rather than
// holding the row for as long as a sender holds its send.
func TestAFallbackCopyKeptForARoleWaitsForTheCommentsLiveMentions(t *testing.T) {
	const role = "kept-while-a-mention-is-live"
	const first, second = "ses_a_mentioned_first", "ses_b_mentioned_second"
	roleTopic := contracts.RoleTopicPrefix + role
	mentionKey := func(commentID, sessionID string) string {
		return contracts.AgentSendDedupeKey(sessionID,
			model.CommentMentionIdempotencyKey(commentID, "session:"+sessionID, "steer"))
	}
	// Nobody holds the role, so the pass's forward keeps the copy on the role's kept lane, which
	// keepRecorder records (TestAHeldCopyOfARoleRouteReachesTheRolesHolder).
	keptFor := func(ctx context.Context, t *testing.T, outcome string) (*store.Store, *keepRecorder, Deps, string) {
		t.Helper()
		database := storetest.Open(t)
		publisher := &keepRecorder{}
		deps := Deps{Store: database, Publisher: publisher, Broker: events.NewBroker(),
			RoleClaims: unclaimedRoles, Sessions: unclaimedRoles}
		return database, publisher, deps,
			seedHeldRoleFallbackCopyMentioningTwo(ctx, t, deps, role, first, second, outcome)
	}

	t.Run("a live mention holds the keep until it settles", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		database, publisher, deps, commentID := keptFor(ctx, t, "pending")

		before := time.Now()
		publishFallbackCopies(ctx, deps, unfenced{})
		after := time.Now()

		if kept := publisher.keeps(); len(kept) > 0 {
			t.Fatalf("the pass kept %d copies while the comment's mention of %s was still being sent, the first on %s under %q carrying mention_dedupe_keys %v, want none kept yet: a copy naming that session nowhere shows it the comment a second time when it claims the role",
				len(kept), second, kept[0].subject, kept[0].item.DedupeKey, kept[0].item.MentionDedupeKeys)
		}
		row := readHeldCopy(t, database, commentID, roleTopic)
		if row.attempts != 0 || row.settled || row.published || row.due == nil {
			t.Fatalf("the held copy is attempts=%d settled=%v published=%v next_attempt_at=%v, want no attempt counted, unsettled, unpublished and due again: a wait is no failure",
				row.attempts, row.settled, row.published, row.due)
		}
		if row.due.Before(before.Add(mentionPendingDelay)) || row.due.After(after.Add(mentionPendingDelay)) {
			t.Fatalf("the held copy is due at %v, want mentionPendingDelay = %v from the pass, the wait a row whose mention is in flight takes",
				*row.due, mentionPendingDelay)
		}

		// The second session's send lands and its sender settles the attempt, as one does within
		// the lease, and the row's wait passes.
		if _, err := database.Pool.Exec(ctx, `
			update comment_deliveries set state = 'sent' where comment_id = $1 and target = $2
		`, commentID, "session:"+second); err != nil {
			t.Fatalf("settle the second mention: %v", err)
		}
		if _, err := database.Pool.Exec(ctx, `
			update comment_fallback_copies set next_attempt_at = now() - interval '1 second'
			where comment_id = $1 and settled_at is null
		`, commentID); err != nil {
			t.Fatalf("pass the held copy's wait: %v", err)
		}

		publishFallbackCopies(ctx, deps, unfenced{})

		if settled := readHeldCopy(t, database, commentID, roleTopic); !settled.settled || !settled.published ||
			settled.attempts != 0 {
			t.Fatalf("once the mentions had settled the held copy is attempts=%d settled=%v published=%v, want it kept and settled by that pass",
				settled.attempts, settled.settled, settled.published)
		}
		kept := publisher.keeps()
		if len(kept) != 1 {
			t.Fatalf("the passes made %d kept publishes, want the one copy kept for the role's next holder once the mentions settled: %+v",
				len(kept), kept)
		}
		want := map[string]string{first: mentionKey(commentID, first), second: mentionKey(commentID, second)}
		if !maps.Equal(kept[0].item.MentionDedupeKeys, want) {
			t.Fatalf("the copy kept for the role's next holder carries mention_dedupe_keys %v, want %v: each mentioned session's own settled mention, which is what keys the copy for whichever of them claims the role",
				kept[0].item.MentionDedupeKeys, want)
		}
	})

	t.Run("an attempt live past its lease lets the keep proceed", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		database, publisher, deps, commentID := keptFor(ctx, t, "resumed")

		publishFallbackCopies(ctx, deps, unfenced{})

		row := readHeldCopy(t, database, commentID, roleTopic)
		kept := publisher.keeps()
		if len(kept) != 1 {
			t.Fatalf("the pass made %d kept publishes for a comment whose second mention is pending past model.DeliveryClaimLapse = %v, want the one copy kept for the role's next holder: the held copy is attempts=%d settled=%v published=%v",
				len(kept), model.DeliveryClaimLapse, row.attempts, row.settled, row.published)
		}
		if !row.settled || !row.published || row.attempts != 0 {
			t.Fatalf("the held copy is attempts=%d settled=%v published=%v, want it kept and settled by the one pass",
				row.attempts, row.settled, row.published)
		}
		want := map[string]string{first: mentionKey(commentID, first)}
		if !maps.Equal(kept[0].item.MentionDedupeKeys, want) {
			t.Fatalf("the copy kept past the second mention's lease carries mention_dedupe_keys %v, want %v: the row's own failed mention alone, no attempt past its lease being waited on",
				kept[0].item.MentionDedupeKeys, want)
		}
	})
}

// A copy the fallback pass forwards to a role's live holder waits for that holder's own mention
// of the comment, while its send is still in flight (decideFallbackCopy,
// mentionDeliveries.liveFor). The comment is on its way to that session already, under the
// mention's own key, and this copy would reach it under the row's key with
// contracts.RoleForwardDedupePrefix in front - the row is keyed by the session the route
// resolved to, another session here - which that host never claimed, so it is shown the comment
// twice: once for the copy, and once for the mention that lands after it. Forwarded once that
// mention has settled, the copy carries the holder's own mention key instead and the host drops
// it (roleFallbackKey, TestAFallbackRoleCopyReachingASecondMentionedHolderCarriesItsOwnMentionsKey).
//
// The wait is narrow: it is the holder's own mention that holds the forward, not any mention of
// the comment, which is what a keep of this pass waits for instead
// (TestAFallbackCopyKeptForARoleWaitsForTheCommentsLiveMentions), and it is the attempt's own
// lease and nothing longer - the second case here: an attempt pending past
// model.DeliveryClaimLapse reads resumed rather than live, and the forward goes with whatever
// has settled by then, under the prefixed row key, rather than holding the row for as long as a
// sender holds its send.
func TestAFallbackCopyForwardedToALiveHolderWaitsForThatHoldersOwnMention(t *testing.T) {
	const role = "held-for-a-live-mentioned-holder"
	const first, second = "ses_a_mentioned_first", "ses_b_holds_the_role"
	roleTopic := contracts.RoleTopicPrefix + role
	mentionKey := func(commentID, sessionID string) string {
		return contracts.AgentSendDedupeKey(sessionID,
			model.CommentMentionIdempotencyKey(commentID, "session:"+sessionID, "steer"))
	}
	// The second mentioned session claimed the role between the comment's write and the pass, so
	// the pass resolves it as the holder the copy is forwarded to, and its own mention of that
	// comment is the one in flight.
	heldFor := func(ctx context.Context, t *testing.T, outcome string) (*roleRig, *receiver, Deps, string) {
		t.Helper()
		rig := newRoleRig(t)
		rig.claim(t, role, second, session.SessionEntry{SelfSubscribed: true})
		rig.silent(t, roleTopic)
		recv := rig.receiver(t, second, true)
		deps := rig.deps()
		return rig, recv, deps,
			seedHeldRoleFallbackCopyMentioningTwo(ctx, t, deps, role, first, second, outcome)
	}

	t.Run("the holder's live mention holds the forward until it settles", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		rig, recv, deps, commentID := heldFor(ctx, t, "pending")

		before := time.Now()
		publishFallbackCopies(ctx, deps, unfenced{})
		after := time.Now()

		if forwards := recv.received(); len(forwards) > 0 {
			t.Fatalf("the pass sent %s %d copies while that holder's own mention of the comment was still being sent, the first under %q, want none yet: a copy under a key its host never claimed shows it the comment a second time beside the mention",
				second, len(forwards), forwards[0].DedupeKey)
		}
		row := readHeldCopy(t, rig.database, commentID, roleTopic)
		if row.attempts != 0 || row.settled || row.published || row.due == nil {
			t.Fatalf("the held copy is attempts=%d settled=%v published=%v next_attempt_at=%v, want no attempt counted, unsettled, unpublished and due again: a wait is no failure",
				row.attempts, row.settled, row.published, row.due)
		}
		if row.due.Before(before.Add(mentionPendingDelay)) || row.due.After(after.Add(mentionPendingDelay)) {
			t.Fatalf("the held copy is due at %v, want mentionPendingDelay = %v from the pass, the wait a row whose mention is in flight takes",
				*row.due, mentionPendingDelay)
		}

		// The holder's send lands and its sender settles the attempt, as one does within the
		// lease, and the row's wait passes.
		if _, err := rig.database.Pool.Exec(ctx, `
			update comment_deliveries set state = 'sent' where comment_id = $1 and target = $2
		`, commentID, "session:"+second); err != nil {
			t.Fatalf("settle the holder's mention: %v", err)
		}
		if _, err := rig.database.Pool.Exec(ctx, `
			update comment_fallback_copies set next_attempt_at = now() - interval '1 second'
			where comment_id = $1 and settled_at is null
		`, commentID); err != nil {
			t.Fatalf("pass the held copy's wait: %v", err)
		}

		publishFallbackCopies(ctx, deps, unfenced{})

		if settled := readHeldCopy(t, rig.database, commentID, roleTopic); !settled.settled ||
			!settled.published || settled.attempts != 0 {
			t.Fatalf("once the holder's mention had settled the held copy is attempts=%d settled=%v published=%v, want it forwarded to %s by that pass",
				settled.attempts, settled.settled, settled.published, second)
		}
		forwards := recv.received()
		if len(forwards) != 1 {
			t.Fatalf("%s was sent %d copies, want the held one once its own mention settled: %+v", second, len(forwards), forwards)
		}
		want := mentionKey(commentID, second)
		if forwards[0].DedupeKey != want {
			t.Fatalf("the copy forwarded to %s, whose own mention of the comment settled while the row waited, is keyed %q, want that mention's own key %q, which its host drops",
				second, forwards[0].DedupeKey, want)
		}
	})

	t.Run("an attempt live past its lease lets the forward proceed", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		rig, recv, deps, commentID := heldFor(ctx, t, "resumed")

		publishFallbackCopies(ctx, deps, unfenced{})

		row := readHeldCopy(t, rig.database, commentID, roleTopic)
		forwards := recv.received()
		if len(forwards) != 1 {
			t.Fatalf("%s was sent %d copies though its own mention is pending past model.DeliveryClaimLapse = %v, want the held one: the held copy is attempts=%d settled=%v published=%v",
				second, len(forwards), model.DeliveryClaimLapse, row.attempts, row.settled, row.published)
		}
		if !row.settled || !row.published || row.attempts != 0 {
			t.Fatalf("the held copy is attempts=%d settled=%v published=%v, want it forwarded and settled by the one pass",
				row.attempts, row.settled, row.published)
		}
		if !strings.HasPrefix(forwards[0].DedupeKey, contracts.RoleForwardDedupePrefix) {
			t.Fatalf("the copy forwarded past the holder's own attempt's lease is keyed %q, want a role forward's prefixed key: no mention of this comment settled for that holder, and the forward waits for no attempt past its lease",
				forwards[0].DedupeKey)
		}
	})
}

// heldPendingCopy holds what one pass that reached no holder left of the copy held for
// destination: the row unsettled and unpublished with its one attempt and its back-off, one
// logged failure naming why the holder was not reached, and that destination counted under its
// holder alone. before and after bracket the pass, as every reading of a row's back-off brackets
// the pass that wrote it.
func heldPendingCopy(t *testing.T, rig *roleRig, registry *metrics.Registry, logged *lockedBuffer,
	commentID, destination, why string, before, after time.Time,
) {
	t.Helper()
	held := readHeldCopy(t, rig.database, commentID, destination)
	if held.attempts != 1 || held.settled || held.published || held.due == nil {
		t.Fatalf("the held copy is attempts=%d settled=%v published=%v next_attempt_at=%v, "+
			"want one attempt, unsettled and unpublished", held.attempts, held.settled, held.published, held.due)
	}
	if held.due.Before(before.Add(retry.Delay(1))) || held.due.After(after.Add(retry.Delay(1))) {
		t.Fatalf("the held copy is due at %v, want retry.Delay(1) = %v from the pass", *held.due, retry.Delay(1))
	}
	var failures []string
	for line := range strings.SplitSeq(logged.String(), "\n") {
		if strings.Contains(line, "dispatch outbox: publish fallback copy") {
			failures = append(failures, line)
		}
	}
	if len(failures) != 1 || !strings.Contains(failures[0], why) {
		t.Fatalf("the pass logged %q, want the one held copy's failure naming %q", failures, why)
	}
	body := failureReasons(t, registry)
	if !strings.Contains(body, `envoy_dispatch_outbox_publish_failures_total{reason="holder"} 1`) {
		t.Fatalf("/metrics body = %q, want the copy its resolution held pending counted under its holder", body)
	}
	if strings.Contains(body, `reason="failed"`) {
		t.Fatalf("/metrics body = %q counts a copy that sent nothing as a publish failure", body)
	}
}

// A copy held back for a role route whose holder the pass cannot reach is left for a later pass:
// nothing is sent, the row counts its attempt and backs off unsettled, and the destination is
// counted under its holder rather than as a publish failure, the bus having neither refused nor
// dropped anything (fallbackSend.send, Deps.countPublishFailure). These are the pass's two
// pending ends of a role destination - a resolution that could not be read, and a holder still
// registered that has not heartbeated within session.ClaimStaleAfter - which a batch's own role
// route reaches in TestARoleRouteToAStaleHolderStaysPending and
// TestAForwardHeldByItsHoldersResolutionIsCountedUnderTheHolder. Neither may send anything: no
// resolution vouched for the holder a forward would reach, and the copy's own role topic is one
// the stream does not capture, which would refuse it and back the row off for ever.
func TestAFallbackCopyWhoseRoleHolderIsNotReachedStaysPendingUnderItsHolder(t *testing.T) {
	const role = "reviewer"
	roleTopic := contracts.RoleTopicPrefix + role

	t.Run("the holder has not heartbeated", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		var logged lockedBuffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		rig := newRoleRig(t)
		const holder = "ses_stale_fallback"
		rig.claimRegisteredAt(t, role, holder, session.LiveWindow+time.Minute)
		rig.silent(t, roleTopic)
		rig.silent(t, contracts.AgentTopicPrefix+">")
		deps, registry := rig.countingDeps(t)
		commentID := seedHeldRoleFallbackCopy(ctx, t, deps, role)

		before := time.Now()
		publishFallbackCopies(ctx, deps, unfenced{})
		after := time.Now()

		heldPendingCopy(t, rig, registry, &logged, commentID, roleTopic, "has not heartbeated", before, after)
		if claimed, err := rig.registry.RoleHolder(role); err != nil || claimed != holder {
			t.Fatalf("the claim after the pass names %q (%v), want the stale holder's kept", claimed, err)
		}
	})

	t.Run("the role's claim could not be read", func(t *testing.T) {
		ctx := store.WithTransactionTracking(context.Background())
		var logged lockedBuffer
		logtest.SetDefault(t, slog.NewTextHandler(&logged, nil))
		rig := newRoleRig(t)
		const holder = "ses_unread_claim"
		rig.claim(t, role, holder, session.SessionEntry{SelfSubscribed: true})
		rig.silent(t, roleTopic)
		rig.silent(t, contracts.AgentTopicPrefix+">")
		deps, registry := rig.countingDeps(t)
		// The row is seeded through readers that read, the scan that holds the copy back
		// resolving the roles of the destinations it sends; the pass alone is given the bucket
		// it cannot read. The claim is a live holder's, so what holds the copy back is the read
		// and not what the bucket says.
		commentID := seedHeldRoleFallbackCopy(ctx, t, deps, role)
		unreadable := deps
		unreadable.RoleClaims = unreadableRoleClaims{}

		before := time.Now()
		publishFallbackCopies(ctx, unreadable, unfenced{})
		after := time.Now()

		heldPendingCopy(t, rig, registry, &logged, commentID, roleTopic, "resolve the role's holder", before, after)
	})
}
