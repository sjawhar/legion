package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// firstKeep is a client whose first kept publish is the test's own and whose every later send is
// the client's: first is handed the publish the outbox made, to make or to leave unmade, and
// answers the outbox in its place, which is how one wrapper drives each of the ways a keep can
// go. keeps counts the kept publishes the outbox has made.
type firstKeep struct {
	*bus.Client
	keeps atomic.Int32
	first func(publish func() <-chan error) <-chan error
}

func (p *firstKeep) WindowUntil(deadline func() time.Time) bus.Sender {
	return firstKeepWindow{Sender: p.Client.WindowUntil(deadline), publisher: p}
}

type firstKeepWindow struct {
	bus.Sender
	publisher *firstKeep
}

func (w firstKeepWindow) PublishStoredAsync(subject string, item contracts.Envelope, msgID string) <-chan error {
	publish := func() <-chan error { return w.Sender.PublishStoredAsync(subject, item, msgID) }
	if w.publisher.keeps.Add(1) > 1 {
		return publish()
	}
	return w.publisher.first(publish)
}

// lostKeepAck is a client whose first kept publish is made and then answered as a lost
// acknowledgement: the stream stores the copy and the outbox is told nothing came back, which is
// what a connection lost between the publish and its acknowledgement leaves behind
// (bus.ErrNotAcknowledged).
func lostKeepAck(client *bus.Client) *firstKeep {
	return &firstKeep{Client: client, first: func(publish func() <-chan error) <-chan error {
		// The publish is made and its answer dropped, as a lost connection drops it.
		answer := publish()
		go func() { <-answer }()
		return answered(fmt.Errorf("the acknowledgement of the kept copy was lost: %w", bus.ErrNotAcknowledged))
	}}
}

// failedKeep is a client whose first kept publish sends nothing and fails: the keep that failed
// left the stream holding nothing of the copy.
func failedKeep(client *bus.Client) *firstKeep {
	return &firstKeep{Client: client, first: func(func() <-chan error) <-chan error {
		return answered(fmt.Errorf("the kept copy was not stored: %w", bus.ErrNotAcknowledged))
	}}
}

// claimedUnderTheKeep is a client that runs claim before its first kept publish, so the claim
// lands inside the window the keep's own race leaves open: after the batch resolved the role
// unheld and before the copy is on its lane (handOverKept).
func claimedUnderTheKeep(client *bus.Client, claim func()) *firstKeep {
	return &firstKeep{Client: client, first: func(publish func() <-chan error) <-chan error {
		claim()
		return publish()
	}}
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

// roleClaimStub is a role-claim store and session reader with one claim and no session at all,
// for a test that resolves a role without a NATS server: holder is the session the claim names,
// empty for a role no session claims, and release is what the release of that claim comes to.
// Every session read answers "gone", so a claim this store names is always a lapsed one.
type roleClaimStub struct {
	holder  string
	release envoystore.ExpiredRoleClaimRelease
}

const retainedHolder = "ses_restored"

// retainedClaim is the claim the restart grace retained: the holder's session is gone and the
// release keeps the claim, which is the grace a listener gives a claim it restored
// (store.Registry.ReleaseExpiredRoleClaim).
var retainedClaim = roleClaimStub{holder: retainedHolder, release: envoystore.ExpiredRoleClaimRetained}

// unclaimedRoles is the store in which no role is claimed, which releases nothing.
var unclaimedRoles = roleClaimStub{release: envoystore.ExpiredRoleClaimMissing}

func (s roleClaimStub) RoleClaim(context.Context, string) (envoystore.RoleClaim, error) {
	return envoystore.RoleClaim{HolderSessionID: s.holder}, nil
}

func (s roleClaimStub) ReleaseExpiredRoleClaim(context.Context, string, string, time.Duration) (envoystore.ExpiredRoleClaimRelease, error) {
	return s.release, nil
}

func (roleClaimStub) Holder(context.Context, string, func(session.SessionEntry) bool) (session.SessionEntry, error) {
	return session.SessionEntry{}, natsgo.ErrKeyNotFound
}

func (roleClaimStub) TTL() time.Duration { return 5 * time.Minute }

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
	publisher := lostKeepAck(rig.client)
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
	outbox := holding(t, rig.publisherOn(failedKeep(rig.client)))

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
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, RoleClaims: retainedClaim, Sessions: retainedClaim}

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
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, RoleClaims: unclaimedRoles, Sessions: unclaimedRoles}
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
	deps := Deps{Store: database, Publisher: publisher, Broker: broker, RoleClaims: unclaimedRoles, Sessions: unclaimedRoles}

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

// claimUnder makes holder role's holder, registered with entry, as a session plugin's
// registration does, and reports what failed rather than failing the test: it is called on the
// goroutine the outbox publishes its keep on, where a t.Fatalf would end that goroutine alone.
// roleRig.claim is this call made from a test's own goroutine.
func (r *roleRig) claimUnder(role, holder string, entry session.SessionEntry) error {
	if _, err := r.registry.SetRole(holder, "test-machine", role, false); err != nil {
		return fmt.Errorf("claim %s for %s: %w", role, holder, err)
	}
	if err := r.sessions.Put(holder, entry); err != nil {
		return fmt.Errorf("register %s: %w", holder, err)
	}
	return nil
}

// handedNow is the forward the holder has already taken, read without waiting: a forward the
// outbox made inside a scan is on this channel by the time that scan returned, since the holder
// records it before it answers the receipt and the send waits for that receipt. So a scan that
// forwarded nothing fails here at once rather than on a timeout.
func handedNow(t *testing.T, recv *receiver, what string) contracts.Envelope {
	t.Helper()
	select {
	case forward := <-recv.arrived:
		return forward
	default:
		t.Fatalf("%s: the holder had taken no forward by the time the scan returned", what)
		return contracts.Envelope{}
	}
}

// A role claimed while the keep was in flight is handed the kept copy by the keep itself, in the
// scan that kept it. The batch resolved the role unheld as it opened and published the keep at
// its end, so the claim landed between the two: that claim's own drain read the role's lane
// before the copy was on it and ended with nothing to hand over (cmd/listener/role_keep.go,
// drainKeptRole), and nothing walks that lane again until the listener's sweep, up to its
// interval later. Meanwhile the issue's next event resolves the holder live and is forwarded
// straight to its agent subject, so the holder would take event N + 1 before event N. The
// outbox's own handover closes that: it reads the claim again once the stream acknowledged the
// keep and forwards the copy itself (handOverKept).
//
// Nothing sweeps here - no listener runs in this test - so the keep's handover is the only way
// the copy can reach the holder, and the holder takes exactly one copy of it: the forward goes
// under the id the drain's own forward would carry, which is what collapses the two.
func TestACopyKeptForARoleClaimedUnderTheKeepReachesThatHolderBeforeTheIssuesNextEvent(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "kept-claimed-under-the-keep", "ses_claimed_under_the_keep"
	const key = "T-1"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	recv := rig.receiver(t, holder, true)
	kept := rig.routedEvent(t, key, role, model.Actor{Kind: "user", ID: "alice"})
	claimed := make(chan error, 1)
	publisher := claimedUnderTheKeep(rig.client, func() {
		claimed <- rig.claimUnder(role, holder, session.SessionEntry{SelfSubscribed: true})
	})
	outbox := holding(t, rig.publisherOn(publisher))

	started := time.Now()
	outbox.scan(context.Background(), unfenced{})
	keptScan := time.Since(started)

	if err := <-claimed; err != nil {
		t.Fatalf("the claim made under the keep: %v", err)
	}
	if publishedAt(t, rig.database, kept.ID) == nil {
		attempts, failure, _ := eventState(t, rig.database, kept.ID)
		t.Fatalf("the kept event was not published: attempt_count %d, publish_error %v", attempts, failure)
	}
	keptKey := fmt.Sprintf("dispatch-%d", kept.ID)
	handed := handedNow(t, recv, "the copy kept for a role claimed under the keep")
	if handed.EventID != keptKey || handed.DedupeKey != contracts.RoleForwardDedupePrefix+keptKey {
		t.Fatalf("the holder was handed event id %q under dedupe key %q, want the role forward of the kept %q",
			handed.EventID, handed.DedupeKey, keptKey)
	}
	// The handover's copy carries the time of the keep, as the drain's own forward of a copy
	// carries the stream's store time of it: for a holder with a serving port the stream keeps
	// whichever of the two reached its agent subject first, which is this one.
	if handed.KeptAt == nil || *handed.KeptAt < started.UnixMilli() || *handed.KeptAt > time.Now().UnixMilli() {
		t.Fatalf("the holder was handed kept_at %v, want the time the outbox kept the copy, between %d and now",
			handed.KeptAt, started.UnixMilli())
	}
	if copies := keptCopies(t, rig.client, role); len(copies) != 1 || copies[0].EventID != keptKey || copies[0].KeptAt != nil {
		t.Fatalf("role %s's kept lane holds %d copies, want the one the outbox kept with no kept_at of its own: the lane stays the authority for a later holder, and the time is stamped on the forward", role, len(copies))
	}

	// The issue's next event, which resolves the holder live and is forwarded straight to its
	// agent subject: it is seeded and sent after the kept copy was handed over, which is what
	// the holder must receive in that order.
	next := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "3b2f1a8c-5d44-4e7a-9b61-2c8f0a6d1e52", IssueKey: new(key), Body: "the issue's next event"},
	})
	nextStarted := time.Now()
	outbox.scan(context.Background(), unfenced{})
	nextScan := time.Since(nextStarted)
	handedNext := handedNow(t, recv, "the issue's next event")

	nextKey := fmt.Sprintf("dispatch-%d", next.ID)
	if handedNext.EventID != nextKey {
		t.Fatalf("the holder was handed event id %q after the kept copy, want the issue's next event %q", handedNext.EventID, nextKey)
	}
	received := recv.received()
	if len(received) != 2 || received[0].EventID != keptKey || received[1].EventID != nextKey {
		t.Fatalf("the holder received %d forwards (%v), want the kept %s first and the issue's next event %s after it",
			len(received), eventIDs(received), keptKey, nextKey)
	}
	subject := contracts.AgentSubject(holder)
	stored := storedMsgIDs(t, rig.conn, subject)
	forwards := []string{
		bus.ForwardMsgID(contracts.RoleForwardDedupePrefix+keptKey, subject),
		bus.ForwardMsgID(contracts.RoleForwardDedupePrefix+nextKey, subject),
	}
	if !slices.Equal(stored, forwards) {
		t.Fatalf("the stream holds %v on %s, want the handover's copy of the kept event and the next event's forward, each under the id the drain's own forward of it would carry: %v",
			stored, subject, forwards)
	}
	t.Logf("the holder had the kept copy when the scan that kept it returned, %s after that scan began; the issue's next event reached it in the scan that began %s later, which took %s. The copy is on the role's kept lane as well, where nothing but the listener's sweep, minutes apart, would have read it.",
		keptScan, nextStarted.Sub(started), nextScan)
}

// A copy kept for a role whose lane already holds older copies is not handed over: the holder
// that claimed the role under the keep takes a lane strictly in order from its cursor, and only
// the role's own drain hands it over (cmd/listener/role_keep.go, walkKeptRole). A handover of the
// copy just kept would overtake the copies before it - it is made a few round trips after the
// claim, while that claim's drain is still on its first reads - and since a role token names one
// tree role of one issue, those are this issue's own earlier events: the holder would see its
// later event first. The copies waiting there are also why nothing is lost by leaving this one:
// the claim's drain finds them and this one behind them and sends the lot, without waiting for
// the listener's sweep.
//
// Nothing drains the lane here - no listener runs in this test - so what the handover left is
// read on the lane itself: both copies in the order a drain hands them over, the earlier first,
// and nothing taken by the holder or stored on its agent subject.
func TestACopyKeptForARoleClaimedUnderTheKeepWaitsBehindTheCopiesOnItsLane(t *testing.T) {
	rig := newRoleRig(t)
	const role, holder = "kept-behind-older-copies", "ses_claimed_behind_older_copies"
	const key = "T-1"
	rig.silent(t, contracts.RoleTopicPrefix+role)
	recv := rig.receiver(t, holder, true)
	earlier := rig.routedEvent(t, key, role, model.Actor{Kind: "user", ID: "alice"})

	// The issue's earlier event is kept while the role is unheld and stays on the lane: no
	// claim lands in this scan, so its keep hands nothing over and no drain walks the lane.
	holding(t, rig.deps()).scan(context.Background(), unfenced{})

	if publishedAt(t, rig.database, earlier.ID) == nil {
		attempts, failure, _ := eventState(t, rig.database, earlier.ID)
		t.Fatalf("the earlier event was not kept: attempt_count %d, publish_error %v", attempts, failure)
	}
	later := appendEvent(t, rig.database, rig.broker, model.Event{
		IssueKey: new(key), Type: "message.created", Actor: model.Actor{Kind: "user", ID: "alice"},
		Payload: model.Message{ID: "6c1d9f20-7a83-4f15-8de4-19b70c3a5e88", IssueKey: new(key), Body: "the issue's later event"},
	})
	claimed := make(chan error, 1)
	publisher := claimedUnderTheKeep(rig.client, func() {
		claimed <- rig.claimUnder(role, holder, session.SessionEntry{SelfSubscribed: true})
	})

	holding(t, rig.publisherOn(publisher)).scan(context.Background(), unfenced{})

	if err := <-claimed; err != nil {
		t.Fatalf("the claim made under the keep: %v", err)
	}
	if publishedAt(t, rig.database, later.ID) == nil {
		attempts, failure, _ := eventState(t, rig.database, later.ID)
		t.Fatalf("the later event was not kept: attempt_count %d, publish_error %v", attempts, failure)
	}
	earlierKey, laterKey := fmt.Sprintf("dispatch-%d", earlier.ID), fmt.Sprintf("dispatch-%d", later.ID)
	if taken := recv.received(); len(taken) != 0 {
		t.Fatalf("the holder was handed %v in the scan that kept %s, want nothing: %s reaches it first, and only the drain of the role's lane hands either over",
			eventIDs(taken), laterKey, earlierKey)
	}
	if copies := storedCopies(t, rig.client, contracts.AgentSubject(holder)); copies != 0 {
		t.Fatalf("the stream holds %d copies on %s, want none: nothing was forwarded to the holder", copies, contracts.AgentSubject(holder))
	}
	kept := keptCopies(t, rig.client, role)
	if len(kept) != 2 || kept[0].EventID != earlierKey || kept[1].EventID != laterKey {
		t.Fatalf("role %s's kept lane holds %v, want %s before %s: the order the role's drain hands the issue's events to the holder in",
			role, eventIDs(kept), earlierKey, laterKey)
	}
}

// eventIDs is the event id of each envelope, for a failure that reports what a holder received.
func eventIDs(received []contracts.Envelope) []string {
	ids := make([]string, 0, len(received))
	for _, item := range received {
		ids = append(ids, item.EventID)
	}
	return ids
}

// storedMsgIDs is the Nats-Msg-Id of every copy the stream holds on subject, in stream order. A
// forward's id is what collapses the copies of one message sent to one subject into the one the
// holder is delivered (bus.ForwardMsgID), so it says which message a stored copy is a forward of.
func storedMsgIDs(t *testing.T, conn *natsgo.Conn, subject string) []string {
	t.Helper()
	var ids []string
	for _, message := range streamMessages(t, conn) {
		if message.Subject == subject {
			ids = append(ids, message.Header.Get(natsgo.MsgIdHdr))
		}
	}
	return ids
}
