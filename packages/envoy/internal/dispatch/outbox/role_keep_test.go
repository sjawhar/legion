package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/sjawhar/envoy/internal/session"
	envoystore "github.com/sjawhar/envoy/internal/store"
)

// keptCopies is every copy the stream holds on role's kept lane, in the order the drain walks
// them: from the lane's first sequence on, one NextStoredMessage per copy, which is the read the
// listener's handover makes (cmd/listener/role_keep.go, walkKeptRole). The drain reads the lane
// by stream sequence and not by who wrote it, so a copy Dispatch's outbox kept is read here
// exactly as one the listener kept.
func keptCopies(t *testing.T, client *bus.Client, role string) []contracts.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var copies []contracts.Envelope
	for seq := uint64(1); ; {
		stored, err := client.NextStoredMessage(ctx, bus.KeptRoleSubject(role), seq)
		if errors.Is(err, natsgo.ErrMsgNotFound) {
			return copies
		}
		if err != nil {
			t.Fatalf("read role %s's kept lane after %d: %v", role, seq, err)
		}
		var item contracts.Envelope
		if err := json.Unmarshal(stored.Data, &item); err != nil {
			t.Fatalf("decode role %s's kept copy at %d: %v", role, stored.Sequence, err)
		}
		copies = append(copies, item)
		seq = stored.Sequence + 1
	}
}

// publisherOn is the rig's dependencies with publisher in the client's place, for a test that
// answers the outbox's sends itself.
func (r *roleRig) publisherOn(publisher Publisher) Deps {
	deps := r.deps()
	deps.Publisher = publisher
	return deps
}

// keptOnce is a client whose first kept publish is made and then answered as a lost
// acknowledgement: the stream stores the copy and the outbox is told nothing came back, which is
// what a connection lost between the publish and its acknowledgement leaves behind
// (bus.ErrNotAcknowledged). Every later send is the client's own.
type keptOnce struct {
	*bus.Client
	keeps atomic.Int32
}

func (p *keptOnce) WindowUntil(deadline func() time.Time) bus.Sender {
	return lostAckWindow{Sender: p.Client.WindowUntil(deadline), publisher: p}
}

type lostAckWindow struct {
	bus.Sender
	publisher *keptOnce
}

func (w lostAckWindow) PublishStoredAsync(subject string, item contracts.Envelope, msgID string) <-chan error {
	answer := w.Sender.PublishStoredAsync(subject, item, msgID)
	if w.publisher.keeps.Add(1) > 1 {
		return answer
	}
	// The publish is made and its answer dropped, as a lost connection drops it.
	go func() { <-answer }()
	return answered(fmt.Errorf("the acknowledgement of the kept copy was lost: %w", bus.ErrNotAcknowledged))
}

// keepFailsOnce is a client whose first kept publish sends nothing and fails, and whose later
// sends are its own: the keep that failed left the stream holding nothing of the copy.
type keepFailsOnce struct {
	*bus.Client
	keeps atomic.Int32
}

func (p *keepFailsOnce) WindowUntil(deadline func() time.Time) bus.Sender {
	return failingKeepWindow{Sender: p.Client.WindowUntil(deadline), publisher: p}
}

type failingKeepWindow struct {
	bus.Sender
	publisher *keepFailsOnce
}

func (w failingKeepWindow) PublishStoredAsync(subject string, item contracts.Envelope, msgID string) <-chan error {
	if w.publisher.keeps.Add(1) == 1 {
		return answered(fmt.Errorf("the kept copy was not stored: %w", bus.ErrNotAcknowledged))
	}
	return w.Sender.PublishStoredAsync(subject, item, msgID)
}

// keptPublish is one stored publish a test publisher took, with the subject and the Nats-Msg-Id it
// was made under.
type keptPublish struct {
	subject string
	msgID   string
	item    contracts.Envelope
}

// keepRecorder is a test publisher with no NATS that takes every publish and records the kept
// publishes among them, so a test can hold what the outbox kept against what it should have.
type keepRecorder struct {
	publishOnly
	mu   sync.Mutex
	kept []keptPublish
}

func (p *keepRecorder) WindowUntil(deadline func() time.Time) bus.Sender {
	return deadlineWindow{Sender: p, deadline: deadline}
}

func (p *keepRecorder) PublishAsync(contracts.Envelope) <-chan error { return answered(nil) }

func (p *keepRecorder) PublishStoredAsync(subject string, item contracts.Envelope, msgID string) <-chan error {
	p.mu.Lock()
	p.kept = append(p.kept, keptPublish{subject: subject, msgID: msgID, item: item})
	p.mu.Unlock()
	return answered(nil)
}

func (p *keepRecorder) keeps() []keptPublish {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]keptPublish(nil), p.kept...)
}

// retainedClaim is a role-claim store whose claim the restart grace retained: the holder's session
// is gone and the release keeps the claim, which is the grace a listener gives a claim it restored
// (store.Registry.ReleaseExpiredRoleClaim).
type retainedClaim struct{}

const retainedHolder = "ses_restored"

func (retainedClaim) RoleClaim(context.Context, string) (envoystore.RoleClaim, error) {
	return envoystore.RoleClaim{HolderSessionID: retainedHolder}, nil
}

func (retainedClaim) ReleaseExpiredRoleClaim(context.Context, string, string, time.Duration) (envoystore.ExpiredRoleClaimRelease, error) {
	return envoystore.ExpiredRoleClaimRetained, nil
}

func (retainedClaim) Holder(context.Context, string, func(session.SessionEntry) bool) (session.SessionEntry, error) {
	return session.SessionEntry{}, natsgo.ErrKeyNotFound
}

func (retainedClaim) TTL() time.Duration { return 5 * time.Minute }

// unclaimedRoles is a role-claim store in which no role is claimed, for a test that keeps without
// a NATS server.
type unclaimedRoles struct{}

func (unclaimedRoles) RoleClaim(context.Context, string) (envoystore.RoleClaim, error) {
	return envoystore.RoleClaim{}, nil
}

func (unclaimedRoles) ReleaseExpiredRoleClaim(context.Context, string, string, time.Duration) (envoystore.ExpiredRoleClaimRelease, error) {
	return envoystore.ExpiredRoleClaimMissing, nil
}

func (unclaimedRoles) Holder(context.Context, string, func(session.SessionEntry) bool) (session.SessionEntry, error) {
	return session.SessionEntry{}, natsgo.ErrKeyNotFound
}

func (unclaimedRoles) TTL() time.Duration { return 5 * time.Minute }

// keptRoleEvent seeds an event of its own issue routed to role, for a test publisher with no NATS.
func keptRoleEvent(t *testing.T, key, role string) (*store.Store, *events.Broker, model.Event) {
	t.Helper()
	database := storetest.Open(t)
	broker := events.NewBroker()
	route := "role:" + role
	seedIssue(t, database, key, &route)
	event := appendEvent(t, database, broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "0f9c2d2b-6a16-4b2f-9b2a-7c5a1a6d4f10", IssueKey: new(key), Body: "for the role"},
	})
	return database, broker, event
}

// A role no session holds is not a destination that reaches nobody: Dispatch's outbox keeps the
// event on that role's kept lane, as the listener keeps a role message no live holder can take, so
// the session that claims the role next is handed it (cmd/listener/role_keep.go). The route counts
// as reached once the stream acknowledges that copy, and nothing crosses the role's topic or any
// agent subject. The copy is the envelope as its sender sent it, on its own role topic and under
// its own dedupe key, which is what the drain's forward to the next holder reads.
func TestARoleRouteToAnUnheldRoleIsKeptForTheRolesNextHolder(t *testing.T) {
	rig := newRoleRig(t)
	const role = "kept-unclaimed"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	rig.silent(t, contracts.AgentTopicPrefix+">")
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})

	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) == nil {
		attempts, failure, _ := eventState(t, rig.database, event.ID)
		t.Fatalf("the event of an unheld role was not published: attempt_count %d, publish_error %v", attempts, failure)
	}
	if _, _, destinations := eventState(t, rig.database, event.ID); !strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix+role) {
		t.Fatalf("published_destinations %v, want the kept role route recorded reached", destinations)
	}
	kept := keptCopies(t, rig.client, role)
	if len(kept) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies, want the one the outbox kept", role, len(kept))
	}
	key := fmt.Sprintf("dispatch-%d", event.ID)
	if kept[0].Topic != contracts.RoleTopicPrefix+role || kept[0].DedupeKey != key || kept[0].EventID != key {
		t.Fatalf("the kept copy is topic %q dedupe key %q event id %q, want the envelope as its sender sent it under %q",
			kept[0].Topic, kept[0].DedupeKey, kept[0].EventID, key)
	}
	if copies := storedCopies(t, rig.client, contracts.AgentTopicPrefix+">"); copies != 0 {
		t.Fatalf("the stream holds %d agent-subject messages, want none: a kept copy reaches no session", copies)
	}
}

// A keep whose acknowledgement was lost leaves the event pending, and the retry keeps it under the
// same Nats-Msg-Id (bus.KeptMsgID, the dedupe key and the role), so the stream holds one copy of
// it however often the outbox retries: the role's next holder is handed the message once.
func TestAKeptRoleRouteRetriedAfterALostAcknowledgementIsKeptOnce(t *testing.T) {
	rig := newRoleRig(t)
	const role = "kept-retried"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	publisher := &keptOnce{Client: rig.client}
	outbox := holding(t, rig.publisherOn(publisher))

	outbox.scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) != nil {
		t.Fatal("the event whose keep was never acknowledged was published")
	}
	attempts, failure, destinations := eventState(t, rig.database, event.ID)
	if attempts != 1 || failure == nil || !strings.Contains(*failure, `publish route "role:`+role+`"`) ||
		strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix) {
		t.Fatalf("attempt_count %d, publish_error %v, published_destinations %v: want the keep backed off and unrecorded",
			attempts, failure, destinations)
	}
	dueNow(t, rig.database, event.ID)

	outbox.scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) == nil {
		_, failure, _ := eventState(t, rig.database, event.ID)
		t.Fatalf("the retry of the kept event did not publish it: publish_error %v", failure)
	}
	if got := publisher.keeps.Load(); got != 2 {
		t.Fatalf("the outbox made %d kept publishes, want the first and its retry", got)
	}
	if kept := keptCopies(t, rig.client, role); len(kept) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies after the retry, want one", role, len(kept))
	}
}

// A keep that failed stores nothing, so the event stays pending with its attempt counted and the
// role route unrecorded, and the outbox's own retry keeps it: nothing of the message is lost to a
// keep NATS did not take.
func TestAKeptRoleRouteWhoseKeepFailedStaysPendingAndIsKeptOnTheRetry(t *testing.T) {
	rig := newRoleRig(t)
	const role = "kept-failed"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	event := rig.routedEvent(t, "T-1", role, model.Actor{Kind: "user", ID: "alice"})
	outbox := holding(t, rig.publisherOn(&keepFailsOnce{Client: rig.client}))

	outbox.scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) != nil {
		t.Fatal("the event whose keep failed was published")
	}
	attempts, failure, destinations := eventState(t, rig.database, event.ID)
	if attempts != 1 || failure == nil || !strings.Contains(*failure, `publish route "role:`+role+`"`) ||
		strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix) {
		t.Fatalf("attempt_count %d, publish_error %v, published_destinations %v: want the failed keep backed off and unrecorded",
			attempts, failure, destinations)
	}
	if kept := keptCopies(t, rig.client, role); len(kept) != 0 {
		t.Fatalf("role %s's kept lane holds %d copies after a keep that failed, want none", role, len(kept))
	}
	dueNow(t, rig.database, event.ID)

	outbox.scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, event.ID) == nil {
		_, failure, _ := eventState(t, rig.database, event.ID)
		t.Fatalf("the retry after the failed keep did not publish the event: publish_error %v", failure)
	}
	if kept := keptCopies(t, rig.client, role); len(kept) != 1 {
		t.Fatalf("role %s's kept lane holds %d copies after the retry, want the one the retry kept", role, len(kept))
	}
}

// A claim the restart grace retained is the one no-holder case that is not kept: that holder keeps
// its claim and writes no new one, so a copy kept for it would wait for a claim that never comes.
// The event stays pending with the error it had before the kept lane, and nothing is kept.
func TestARoleRouteWhoseClaimTheGraceRetainedErrsWithNothingKept(t *testing.T) {
	const role = "kept-retained"
	database, broker, event := keptRoleEvent(t, "T-1", role)
	publisher := &keepRecorder{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, RoleClaims: retainedClaim{}, Sessions: retainedClaim{}}

	holding(t, deps).scan(context.Background(), unfenced{})

	if publishedAt(t, database, event.ID) != nil {
		t.Fatal("the event of a claim the grace retained was published")
	}
	attempts, failure, _ := eventState(t, database, event.ID)
	if attempts != 1 || failure == nil || !strings.Contains(*failure, "is gone and its claim was kept") {
		t.Fatalf("attempt_count %d, publish_error %v: want it backed off naming the claim the grace kept", attempts, failure)
	}
	if kept := publisher.keeps(); len(kept) != 0 {
		t.Fatalf("the outbox kept %d copies for a claim the grace retained, want none: %+v", len(kept), kept)
	}
}

// A role token no claim can ever name (roles.Valid, the pattern POST /v1/roles/set refuses) is
// skipped as it was before the kept lane: the route counts as reached, the batch sends nothing
// for it, and nothing is kept, since a copy kept under such a token waits for a claim that cannot
// be made and expires unread. An issue's route takes a token no claim route takes - one that
// begins with the hyphen model.ParseRoute allows and roles.Valid refuses - so the two patterns
// are not one, and this is the token a route can carry to a role nobody may ever hold.
func TestARoleTokenNoClaimCanNameIsSkippedWithNothingKept(t *testing.T) {
	const role = "-reviewer"
	database, broker, event := keptRoleEvent(t, "T-1", role)
	publisher := &keepRecorder{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, RoleClaims: unclaimedRoles{}, Sessions: unclaimedRoles{}}
	instance := holding(t, deps).instance
	// The first batch has no role reader, so it reaches the issue's topic and leaves the role
	// route pending; the batch under test is the one whose only route left is that role, as
	// TestABatchWhoseOnlyRouteLeftIsAnUnclaimedRoleKeepsItForTheNextHolder reads its own.
	if _, err := scanBatch(context.Background(), Deps{Store: database, Publisher: publisher, Broker: broker}, unfenced{}, instance); err != nil {
		t.Fatalf("scan the first batch: %v", err)
	}
	dueNow(t, database, event.ID)

	batch, err := scanBatch(context.Background(), deps, unfenced{}, instance)
	if err != nil {
		t.Fatalf("scan the batch of the skipped role route: %v", err)
	}

	if publishedAt(t, database, event.ID) == nil {
		_, failure, _ := eventState(t, database, event.ID)
		t.Fatalf("the event of a role token no claim can name was not published: publish_error %v", failure)
	}
	if _, _, destinations := eventState(t, database, event.ID); !strings.Contains(strings.Join(destinations, ","), contracts.RoleTopicPrefix+role) {
		t.Fatalf("published_destinations %v, want the skipped role route recorded reached", destinations)
	}
	if batch.published != 1 || batch.sent != 0 {
		t.Fatalf("the batch = %+v, want it to publish the event with nothing sent", batch)
	}
	if kept := publisher.keeps(); len(kept) != 0 {
		t.Fatalf("the outbox kept %d copies under a role token no claim can name, want none: %+v", len(kept), kept)
	}
}

// A keep is one JetStream publish of the envelope on the role's kept lane, under the id every
// writer of that lane keeps this message under: the subject bus.KeptRoleSubject names and the id
// bus.KeptMsgID makes of the dedupe key and the role, which is what the listener's own keep
// publishes (cmd/listener/role_keep.go, keepRoleMessage). The two writers' copies of one message
// therefore carry one id, and the stream stores it once.
func TestAKeptRoleRouteIsPublishedOnTheRolesKeptLaneUnderTheKeptMsgID(t *testing.T) {
	const role = "kept-named"
	database, broker, event := keptRoleEvent(t, "T-1", role)
	publisher := &keepRecorder{}
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, RoleClaims: unclaimedRoles{}, Sessions: unclaimedRoles{}}

	holding(t, deps).scan(context.Background(), unfenced{})

	kept := publisher.keeps()
	if len(kept) != 1 {
		t.Fatalf("the outbox made %d kept publishes, want one: %+v", len(kept), kept)
	}
	key := fmt.Sprintf("dispatch-%d", event.ID)
	if kept[0].subject != bus.KeptRoleSubject(role) || kept[0].msgID != bus.KeptMsgID(key, role) {
		t.Fatalf("the keep went to %q under %q, want %q under %q",
			kept[0].subject, kept[0].msgID, bus.KeptRoleSubject(role), bus.KeptMsgID(key, role))
	}
	if kept[0].item.Topic != contracts.RoleTopicPrefix+role || kept[0].item.DedupeKey != key {
		t.Fatalf("the kept envelope is topic %q dedupe key %q, want the message as its sender sent it under %q",
			kept[0].item.Topic, kept[0].item.DedupeKey, key)
	}
}
