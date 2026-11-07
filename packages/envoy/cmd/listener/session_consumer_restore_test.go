package main

// The consumer a delete took comes back: to a session whose registration raced that delete, even
// where the delete was made on another listener, and at the position the session had acked
// through rather than at the start its record names. Two things order a delete against a
// registration, and a case here holds each: the session's lock, which a registration holds from
// its consumer call through its interest row, its session entry and the read of its consumer
// after it, and the bucket, which the restore reads the record from because the watched cache
// need not hold a write yet.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
)

// A registration answered 200 with consumer delivery holds a consumer however a delete on
// another listener interleaves with it. The listener's session lock is one process's, and every
// listener's reaper judges every record of the bucket, so the delete here is made through a
// second sessionDelivery over the same stream, which that lock does not reach: it deletes the
// record the registration is replacing, deletes the consumer the registration has just created,
// and reads the bucket again to find no record to restore from, since the registration has not
// written its entry yet. The registration then writes that entry and must come away with a
// consumer, which is the one thing a session is delivered through.
//
// The interleaving is built rather than waited for: afterRegisterFilter runs under the
// registering listener's session lock, after that registration's consumer call and before the row
// and the entry it writes, and the whole of the other listener's pass runs from there, so its
// record read really precedes the entry write. Before this listener read the consumer again after
// that write, the session was answered 200 with no consumer at all.
func TestARegistrationRacedByAnotherListenersDeleteKeepsItsConsumer(t *testing.T) {
	registering := newSessionConsumerReapRig(t)
	deleting := newSessionConsumerReapRig(t)
	const sessionID = "ses_two_listeners"
	registering.register(t, sessionID)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The second listener's copy of the record, read from the bucket rather than waited for from
	// its watch: its pass judges what its copy holds, and deletes at the revision it judged.
	if _, err := deleting.sessions.Refresh(ctx, sessionID); err != nil {
		t.Fatalf("the second listener could not read the record of %s: %v", sessionID, err)
	}
	// Only the second listener's clock is past the gone horizon, so its pass judges the record
	// the registration below replaces while the first listener still reads the session as live.
	deleting.now.advance(3 * time.Minute)
	registering.requireConsumer(t, sessionID, "after its first registration")

	reaped := make(chan int, 1)
	var raced sync.Once
	registering.delivery.afterRegisterFilter = func() {
		raced.Do(func() {
			reaped <- deleting.reap(t)
			// The window this registration is in: its consumer is created, its entry is not
			// written, and the other listener has taken the consumer away and restored nothing.
			if info, err := registering.consumers.Info(ctx, sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
				t.Errorf("the consumer of %s once the other listener's pass had deleted it: %+v, %v; want it gone, which is the window this registration answers in",
					sessionID, info, err)
			}
		})
	}
	answer := registering.register(t, sessionID)

	if answer.Delivery == nil || answer.Delivery.Mode != deliveryModeConsumer {
		t.Fatalf("the registration that raced the other listener's delete was answered delivery %+v, want %s",
			answer.Delivery, deliveryModeConsumer)
	}
	if records := <-reaped; records != 1 {
		t.Fatalf("the other listener's pass deleted %d records, want the one it judged gone", records)
	}
	registering.requireConsumer(t, sessionID, "after the delete another listener made while it registered")
	if _, state := registering.sessions.Lookup(sessionID); state != session.Live {
		t.Fatalf("%s is %s after the registration that raced the other listener's delete, want %s", sessionID, state, session.Live)
	}
}

// A session that races a delete is offered nothing it has acknowledged. The consumer deleted here
// has every frame it delivered acked, and its record names the start it was created at, far below
// that floor; the restore reads the floor under the session's lock and before the delete, and
// creates the consumer again at the sequence after it. Restoring at the record's start instead
// hands the session every frame it has written back again, up to the stream's 72 hours, and holds
// what is published next behind that backlog.
func TestARestoredConsumerDoesNotOfferTheAckedFramesAgain(t *testing.T) {
	rig := newRelayRig(t)
	sessionID := "ses_acked_" + id.New()
	agent := contracts.AgentSubject(sessionID)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	var written []contracts.Envelope
	var floor uint64
	for range 5 {
		item, seq := rig.storeEvent(t, agent)
		written = append(written, item)
		floor = seq
	}
	requireFrames(t, standIn.collect(t, len(written), 10*time.Second), written...)
	awaitAcked(t, rig.consumers, sessionID, floor)
	recorded := rig.entry(t, sessionID).ConsumerStart
	if recorded > floor {
		t.Fatalf("the entry of %s records consumer start %d, above the floor %d it has acked through: this row needs a record whose start is below the floor",
			sessionID, recorded, floor)
	}

	// The delete a clean stop racing its own relaunch makes, and the one a reaper pass makes of a
	// record a registration has written again: the record is in the bucket, so the consumer is
	// restored.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := rig.deps.consumers.deleteConsumer(ctx, rig.deps, sessionID); err != nil {
		t.Fatalf("delete the consumer of %s: %v", sessionID, err)
	}

	info, err := rig.consumers.Info(ctx, sessionID)
	if err != nil {
		t.Fatalf("read the consumer restored for %s: %v", sessionID, err)
	}
	if info.Config.OptStartSeq != floor+1 {
		t.Fatalf("the restored consumer of %s starts at %d, want %d, the sequence after the floor it had acked through; its record names start %d",
			sessionID, info.Config.OptStartSeq, floor+1, recorded)
	}
	if info.NumPending != 0 {
		t.Fatalf("the restored consumer of %s holds %d messages for delivery, want none: it has acked every one the stream holds for it",
			sessionID, info.NumPending)
	}
	standIn.none(t, time.Second)
}

// sessionEntryWriteDelay is the round trip the case below puts between the listener and its
// session bucket, and nothing else: its interest rows, its consumer calls and the delete's own
// reads go direct. A delete released at the interest row rather than at the end of the
// registration's pass therefore reads the bucket while the entry write is still in flight, which
// is the whole of what makes that interleaving certain: the delete's three calls take about a
// millisecond, a four-hundredth of this.
const sessionEntryWriteDelay = 400 * time.Millisecond

// A session that races a delete is offered nothing it has acknowledged, whatever the delete's
// calls fall between. The registration holds the session's lock from its consumer call through
// its session entry and the read of its consumer after it (listenerDeps.registerPass), so a
// delete made on this listener reads the ack floor and the record only once that entry is
// written: it reads the floor the session has acked through now, finds the record the
// registration wrote, and gives the consumer back at the sequence after that floor.
//
// A registration that released the lock at its interest row instead leaves the delete reading
// the bucket while the entry is unwritten: the delete finds no record and restores nothing, and
// the repair left is the registration's own (confirmRegistered), which creates the consumer at
// the floor its answer carried - the floor as it stood before the frames the session acked since
// the consumer call. Every one of those frames is then sent to the session again, and the
// position it had acked through is lost.
//
// The interleaving is built rather than waited for. The hook runs under the registration's lock,
// after its consumer call: there the session acks three more frames, so the floor the answer
// carries is behind the server's, and the record is deleted, as a reaper pass and a clean stop
// both delete the record before the consumer. The delete starts there and the hook holds the
// registration until it waits for the lock, so the delete's first call is made the moment that
// lock is released.
func TestASessionThatRacesADeleteThroughItsEntryWriteIsOfferedNothingItAcked(t *testing.T) {
	rig := newRelayRig(t)
	sessionID := "ses_entry_write_race_" + id.New()
	agent := contracts.AgentSubject(sessionID)
	hub := testnats.StartProxy(t, rig.url, testnats.ProxyLatency(sessionEntryWriteDelay))
	slow, err := session.OpenSessionRegistry(testnats.Connect(t, hub.URL()), session.WithSessionReplicas(1))
	if err != nil {
		t.Fatalf("open the registry the listener writes its session entries through: %v", err)
	}
	t.Cleanup(slow.StopWatch)
	waitForSessions(t, slow)
	// The session entries this listener writes go to the same bucket a round trip away, and
	// every other call it makes stays direct.
	rig.deps.sessions = slow
	// The delete reads the bucket on the listener's direct connection, as every call but the
	// entry write makes here: what this case holds open is that write alone. deleteConsumer
	// reads the record and the session's stored topics through the stores it is given
	// (restoreRegistered), and nothing else of them.
	deleteDeps := &listenerDeps{registry: rig.harness.registry, sessions: rig.harness.sessions}

	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	first, firstSeq := rig.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), first)
	awaitAcked(t, rig.consumers, sessionID, firstSeq)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var (
		deleteErr error
		acked     uint64
	)
	deleted := make(chan struct{})
	var raced sync.Once
	rig.deps.consumers.afterRegisterFilter = func() {
		raced.Do(func() {
			// Frames the session acks after this registration's consumer call: from here the
			// floor that call read, which the registration's own repair would restore at, is
			// behind the floor the server holds, and only a read made after this pass sees the
			// newer one.
			var sent []contracts.Envelope
			for range 3 {
				item, seq := rig.storeEvent(t, agent)
				sent = append(sent, item)
				acked = seq
			}
			requireFrames(t, standIn.collect(t, len(sent), 10*time.Second), sent...)
			awaitAcked(t, rig.consumers, sessionID, acked)
			// The record delete every delete of a consumer follows, so the bucket holds no
			// record of this session while the registration is between its row and its entry.
			if err := rig.harness.sessions.Delete(ctx, sessionID); err != nil {
				t.Errorf("delete the record of %s the delete below follows: %v", sessionID, err)
			}
			go func() {
				defer close(deleted)
				deleteErr = rig.deps.consumers.deleteConsumer(ctx, deleteDeps, sessionID)
			}()
			awaitDeleteWaitingOrDone(t, &rig.deps.consumers.locks, sessionID, deleted, 2*time.Second)
		})
	}
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	rig.deps.consumers.afterRegisterFilter = nil
	<-deleted
	if deleteErr != nil {
		t.Fatalf("the delete that raced %s's registration: %v", sessionID, deleteErr)
	}

	// The session is sent nothing it has acked, and its consumer stands at the sequence after
	// the floor the delete read of it.
	standIn.none(t, 2*time.Second)
	info, err := rig.consumers.Info(ctx, sessionID)
	if err != nil {
		t.Fatalf("read the consumer of %s after the delete that raced its registration: %v", sessionID, err)
	}
	if info.Config.OptStartSeq != acked+1 {
		t.Fatalf("the consumer of %s is back at %d, want %d, the sequence after the floor it had acked through when the delete read it",
			sessionID, info.Config.OptStartSeq, acked+1)
	}
}

// A session whose own registration repairs its consumer is offered nothing it had acknowledged
// when that registration's consumer call read the floor. The repair carries that floor
// (listenerDeps.registerPass hands confirmRegistered outcome.ackFloor) and creates the consumer
// again at the sequence after it, above the start the record names, which is the sequence the
// consumer was created at and far below every frame the session has acked since. A repair that
// dropped the floor would restore at that start and hand the session each of those frames again.
//
// The delete is made on a second listener, as a reaper pass or a clean stop on another task
// makes it: it takes no lock of this process, so the whole of it runs inside the window the hook
// holds open - after this registration's consumer call and before the entry write that would let
// the delete restore from the bucket. That delete therefore finds no record, restores nothing,
// and the only repair left is the registration's own.
func TestARegistrationsOwnRepairDoesNotOfferTheAckedFramesAgain(t *testing.T) {
	registering := newRelayRig(t)
	deleting := newSessionConsumerRig(t)
	sessionID := "ses_register_repair_" + id.New()
	agent := contracts.AgentSubject(sessionID)
	standIn := startSessionStandIn(t, registering.url, sessionID, answerDone)
	registering.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	// The frames the session acks before the registration below: its floor stands at the last of
	// them, and the start its record names stays the one its consumer was created at.
	var written []contracts.Envelope
	var floor uint64
	for range 5 {
		item, seq := registering.storeEvent(t, agent)
		written = append(written, item)
		floor = seq
	}
	requireFrames(t, standIn.collect(t, len(written), 10*time.Second), written...)
	awaitAcked(t, registering.consumers, sessionID, floor)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var raced sync.Once
	registering.deps.consumers.afterRegisterFilter = func() {
		raced.Do(func() {
			// The record delete every delete of a consumer follows, and the consumer delete
			// after it, both on the second listener and both inside this registration's window.
			if err := deleting.deps.sessions.Delete(ctx, sessionID); err != nil {
				t.Errorf("the second listener could not delete the record of %s: %v", sessionID, err)
				return
			}
			if err := deleting.deps.consumers.deleteConsumer(ctx, deleting.deps, sessionID); err != nil {
				t.Errorf("the delete the second listener made of %s's consumer: %v", sessionID, err)
				return
			}
			if info, err := deleting.consumers.Info(ctx, sessionID); !errors.Is(err, bus.ErrSessionConsumerNotFound) {
				t.Errorf("the consumer of %s once the second listener's delete was done: %+v, %v; want it gone, which is the window this registration repairs in",
					sessionID, info, err)
			}
		})
	}
	registering.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	registering.deps.consumers.afterRegisterFilter = nil

	// The record the repair reads, from the bucket as the repair reads it: its start is below the
	// floor, so the floor and not the record is what holds the session's position here.
	entry, err := registering.harness.sessions.Refresh(ctx, sessionID)
	if err != nil {
		t.Fatalf("read the record %s's registration wrote: %v", sessionID, err)
	}
	if entry.ConsumerStart > floor {
		t.Fatalf("the record of %s names consumer start %d, above the floor %d it has acked through: this row needs a start below that floor, which is the whole of what the repair's floor decides",
			sessionID, entry.ConsumerStart, floor)
	}

	// The session is sent none of the frames it acked, and the consumer its own registration
	// gave back delivers what is published next.
	standIn.none(t, 2*time.Second)
	next, _ := registering.storeEvent(t, agent)
	requireFrames(t, standIn.collect(t, 1, 10*time.Second), next)
}

// A delete made while a registration holds the session's lock gives the consumer back with the
// topics that registration stored, never with the ones it is replacing. The restore filters the
// consumer it creates on the session's stored interest row (restoreRegistered), and a
// registration holds the lock from the consumer call that set its filter through the write of
// that row (sessionDelivery.consumerFor, listenerDeps.registerPass), so a delete made on this
// listener reads that row wholly before the filter call or wholly after the write.
//
// A delete that took no lock reads it in between: it gives back a consumer filtered on the row
// the session followed before, and the registration's own read leaves it there, since it asks
// only whether a consumer exists (confirmRegistered). Every message published on the topic the
// session was answered 200 for is then skipped until its next heartbeat, which is the loss the
// lock exists for.
//
// The interleaving is built rather than waited for: the hook runs under the registration's lock,
// after its consumer call and before its row, and leaves that window once the delete waits for
// the lock - where the lock holds it until the pass is done - or, taking none, once the delete
// has already given the consumer back.
func TestADeleteThatRacesARegistrationRestoresTheConsumerWithTheTopicItAnswered(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID = "ses_delete_row_lock"
	const followed = "notifications.dispatch.issue.DELLOCK.>"
	// The row the restore must not go back to: this registration follows no topic of its own.
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var deleteErr error
	deleted := make(chan struct{})
	var raced sync.Once
	rig.deps.consumers.afterRegisterFilter = func() {
		raced.Do(func() {
			go func() {
				defer close(deleted)
				deleteErr = rig.deps.consumers.deleteConsumer(ctx, rig.deps, sessionID)
			}()
			awaitDeleteWaitingOrDone(t, &rig.deps.consumers.locks, sessionID, deleted, 10*time.Second)
		})
	}
	rig.subscribe(t, consumerRegistration(sessionID, "epoch-1", followed))
	rig.deps.consumers.afterRegisterFilter = nil
	<-deleted
	if deleteErr != nil {
		t.Fatalf("the delete that raced %s's registration: %v", sessionID, deleteErr)
	}

	requireFilters(t, rig, sessionID, followed)
	// A message on the topic the registration was answered 200 for is the consumer's to deliver.
	rig.store(t, "notifications.dispatch.issue.DELLOCK.comment")
	rig.awaitPending(t, sessionID, 1)
}

// A delete gives the consumer back to a session whose record its own listener's cache holds none
// of. Every delete of a consumer follows the delete of that session's record, so the read the
// restore makes is of a key this listener has just deleted: the bucket answers the record a
// registration made on another listener wrote since, and the watched cache answers that delete
// until its watch delivers the write. A restore reading the cache gives nothing back, and the
// session answered 200 with consumer delivery has no consumer until its next heartbeat.
//
// The window is built rather than waited for: the deleting listener's watch is stopped once its
// own delete has reached its cache and before the registration it must not hear, which is the
// same reader as a watch that has not delivered that write yet, held there for certain.
func TestADeleteRestoresTheConsumerOfASessionItsCacheHasNoRecordOf(t *testing.T) {
	registering := newSessionConsumerRig(t)
	deleting := newSessionConsumerRig(t)
	const sessionID = "ses_cache_behind"
	registering.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The record delete the consumer delete below follows: the first half of a clean stop, or of
	// a reaper pass, made on the second listener.
	if err := deleting.deps.sessions.Delete(ctx, sessionID); err != nil {
		t.Fatalf("the second listener could not delete the record of %s: %v", sessionID, err)
	}
	awaitCacheHasNoRecord(t, deleting.deps.sessions, sessionID)
	deleting.deps.sessions.StopWatch()

	// The registration that lands between that record delete and the consumer delete, made on
	// the listener the session registers through: the bucket holds its record, and the second
	// listener's cache will not hear of it.
	registering.subscribe(t, consumerRegistration(sessionID, "epoch-2"))
	registering.store(t, contracts.AgentSubject(sessionID))
	registering.awaitPending(t, sessionID, 1)
	if entry, err := deleting.deps.sessions.Get(sessionID); !errors.Is(err, nats.ErrKeyNotFound) {
		t.Fatalf("the cache of the deleting listener holds %+v (%v) for %s; this case needs it holding none, which is what the bucket read is for", entry, err, sessionID)
	}

	if err := deleting.deps.consumers.deleteConsumer(ctx, deleting.deps, sessionID); err != nil {
		t.Fatalf("the delete made on the listener whose cache holds no record of %s: %v", sessionID, err)
	}

	info, err := deleting.consumers.Info(ctx, sessionID)
	if err != nil {
		t.Fatalf("the consumer of %s after that delete: %v; want it given back from the record the bucket holds", sessionID, err)
	}
	if info.NumPending != 1 {
		t.Fatalf("the consumer given back to %s holds %d messages for delivery, want the one stored for it since it registered",
			sessionID, info.NumPending)
	}
}

// A delete gives the consumer back filtered on the topics the bucket holds, and not on the ones
// its own listener's interest cache holds. The restore reads the session's record from the bucket
// because the registration it repairs was made on another listener, and its topics are that same
// registration's write: a cache this listener's watch has not delivered it to is behind the row
// it reads the record of, so the filter it would build there leaves out the topic that
// registration was answered 200 for. The session then receives nothing on that topic, since its
// consumer is the one path to it, until its next heartbeat sets the filter again.
//
// What the case asserts is that delivery and not the consumer's existence: the consumer is there
// either way, filtered on one topic set or the other, and only a message published on the topic
// the registration added says which.
//
// The window is built rather than waited for: the deleting listener's interest watch is stopped
// once its cache holds the row as it stood before that registration, which is the same reader as
// a watch that has not delivered the registration's write yet, held there for certain. A cache
// holding no row at all is not that reader: it falls through to the bucket on a miss
// (store.Registry.Get).
func TestADeleteRestoresTheConsumerWithTheTopicItsCacheHasNotHeardOf(t *testing.T) {
	registering := newSessionConsumerRig(t)
	deleting := newSessionConsumerRig(t)
	const sessionID = "ses_topics_behind"
	const probe = "notifications.dispatch.issue.PROBE.>"
	registering.subscribe(t, consumerRegistration(sessionID, "epoch-1"))
	awaitCachedTopics(t, deleting.deps.registry, sessionID, contracts.AgentSubject(sessionID))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The record delete the consumer delete below follows: the first half of a clean stop, or of
	// a reaper pass, made on the second listener.
	if err := deleting.deps.sessions.Delete(ctx, sessionID); err != nil {
		t.Fatalf("the second listener could not delete the record of %s: %v", sessionID, err)
	}
	deleting.deps.registry.StopWatch()

	// The registration that lands between that record delete and the consumer delete, made on
	// the listener the session registers through: it adds the probe topic, writes the row and
	// the record, and the second listener's cache will not hear of it.
	answer := registering.subscribe(t, consumerRegistration(sessionID, "epoch-2", probe))
	if answer.Delivery == nil || answer.Delivery.Mode != deliveryModeConsumer {
		t.Fatalf("the registration that added %s to %s was answered delivery %+v, want %s",
			probe, sessionID, answer.Delivery, deliveryModeConsumer)
	}
	if interest, ok := deleting.deps.registry.Cached(sessionID); !ok || slices.Contains(interest.Topics, probe) {
		t.Fatalf("the interest cache of the deleting listener holds %v (held %t) for %s; this case needs it holding the row without %s, which is what the bucket read is for",
			interest.Topics, ok, sessionID, probe)
	}

	if err := deleting.deps.consumers.deleteConsumer(ctx, deleting.deps, sessionID); err != nil {
		t.Fatalf("the delete made on the listener whose cache is behind the row of %s: %v", sessionID, err)
	}

	// A message on the topic that registration was answered 200 for is the restored consumer's
	// to deliver.
	registering.store(t, "notifications.dispatch.issue.PROBE.comment")
	registering.awaitPending(t, sessionID, 1)
}

// A registration's own repair gives back the topic another listener's restore left out. The
// order is the one a consumer delete and a registration of the same session can really take
// across two listeners: the second listener deletes the record (a reaper pass, a clean stop),
// the session registers again on the first and writes the record and the row back, the
// registration after it adds a topic and makes its consumer call, and only then does the second
// listener delete the consumer and give it back from the record and the row it reads of the
// bucket - which are the ones the registration before wrote, this registration's row being
// unwritten. The consumer then stands filtered without the topic this registration is answered
// 200 for.
//
// A repair that asks only whether a consumer exists leaves that filter there, and every message
// published on the added topic is skipped until the session's next heartbeat. What the case
// asserts is that delivery and not the consumer's existence: the consumer is there either way,
// and only a message published on the added topic says which filter it carries.
//
// The interleaving is built rather than waited for: afterRegisterFilter runs under the
// registering listener's session lock, after that registration's consumer call and before the
// row and the entry it writes, and the second listener's whole delete runs from there, so its
// bucket reads really precede that row write. The second listener's lock is another process's
// and holds none of this one's calls.
func TestARegistrationsOwnRepairGivesBackTheTopicAnotherListenersRestoreLeftOut(t *testing.T) {
	registering := newSessionConsumerRig(t)
	deleting := newSessionConsumerRig(t)
	const sessionID = "ses_confirm_filter"
	const added = "notifications.dispatch.issue.CONFIRM.>"
	registering.subscribe(t, consumerRegistration(sessionID, "epoch-1"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// The record delete the consumer delete below follows: the first half of a clean stop, or of
	// a reaper pass, made on the second listener.
	if err := deleting.deps.sessions.Delete(ctx, sessionID); err != nil {
		t.Fatalf("the second listener could not delete the record of %s: %v", sessionID, err)
	}
	// The registration that writes the record and the row back, and so the pair the second
	// listener's restore reads: this session follows no topic of its own yet.
	registering.subscribe(t, consumerRegistration(sessionID, "epoch-2"))

	var raced sync.Once
	registering.deps.consumers.afterRegisterFilter = func() {
		raced.Do(func() {
			if err := deleting.deps.consumers.deleteConsumer(ctx, deleting.deps, sessionID); err != nil {
				t.Errorf("the delete the second listener made inside the registration of %s: %v", sessionID, err)
				return
			}
			if filters := registering.filters(t, sessionID); slices.Contains(filters, added) {
				t.Errorf("the consumer the second listener gave back to %s filters on %v; this case needs it given back without %s, which is the row the bucket held when it read it",
					sessionID, filters, added)
			}
		})
	}
	answer := registering.subscribe(t, consumerRegistration(sessionID, "epoch-2", added))
	registering.deps.consumers.afterRegisterFilter = nil
	if answer.Delivery == nil || answer.Delivery.Mode != deliveryModeConsumer {
		t.Fatalf("the registration that added %s to %s was answered delivery %+v, want %s",
			added, sessionID, answer.Delivery, deliveryModeConsumer)
	}

	// A message on the topic that registration was answered 200 for is its consumer's to
	// deliver: the repair reads the bucket's newest row, which is that registration's own, and
	// sets the filter the restore left.
	registering.store(t, "notifications.dispatch.issue.CONFIRM.comment")
	registering.awaitPending(t, sessionID, 1)
}

// awaitDeleteWaitingOrDone holds a registration inside its hook until the delete it started waits
// for the session's lock (two callers, this registration and it) or is done, which is what a
// delete taking no lock is by then: the delete's first call is made the moment the registration
// releases that lock. A deadline of within reached is reported and not fatal: the caller holds the
// session's lock, and the reads after the pass say what the delete did.
func awaitDeleteWaitingOrDone(t *testing.T, locks *sessionLocks, sessionID string, done <-chan struct{}, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for lockUsers(locks, sessionID) < 2 {
		select {
		case <-done:
			return
		default:
		}
		if time.Now().After(deadline) {
			t.Errorf("after %s the delete of %s's consumer neither waits for the session's lock nor is done with it", within, sessionID)
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitCacheHasNoRecord waits until sessions holds no cached record of sessionID, the state a
// delete made through it leaves once its own watch has delivered that delete.
func awaitCacheHasNoRecord(t *testing.T, sessions *session.SessionRegistry, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := sessions.Get(sessionID); errors.Is(err, nats.ErrKeyNotFound) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 10s the cache still holds a record of %s; want the delete made through it delivered", sessionID)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// awaitCachedTopics waits until registry's interest cache holds sessionID's row with each of
// topics in it, the state the listener's own watch leaves once it has delivered the write that
// stored them. A case that freezes a cache to hold it behind a later write waits here first: a
// cache holding no row reads the bucket on a miss (store.Registry.Get), so freezing it would
// hold nothing behind anything.
func awaitCachedTopics(t *testing.T, registry *store.Registry, sessionID string, topics ...string) {
	t.Helper()
	waitFor(t, 10*time.Second, "the interest cache to hold "+sessionID+"'s row with "+strings.Join(topics, ", "), func() bool {
		interest, ok := registry.Cached(sessionID)
		if !ok {
			return false
		}
		for _, topic := range topics {
			if !slices.Contains(interest.Topics, topic) {
				return false
			}
		}
		return true
	})
}
