// The policy for an agent delivery this listener cannot place: its retry bounds, the deferral set
// that caps how many such messages are held at once and expires a slot whose message never comes
// back, and the defer-or-give-up decision itself.

package main

import (
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
)

// unplaceableRetryDelay is the delay an agent delivery this listener could not place asks for
// before the stream redelivers it. It is short enough that a window's worth of them is a dozen
// attempts rather than two - at the 30 s finalize(true) takes, a minute is one retry - and long
// enough that a session whose record is still propagating, or whose server is still binding its
// serve port, has time to finish between two of them.
const unplaceableRetryDelay = 5 * time.Second

// unplaceableMaxDeliveries is the hard stop on those retries beside the window's own count of
// deferrals, two short of the durable's own MaxDeliver (consumerMaxDeliver, durable.go: 20, so
// 18 here). JetStream stops redelivering at max_deliver silently - nothing in this repository
// consumes its advisory - so this listener must give up, and write its give-up record, before
// the stream does. The two-delivery margin covers the deliveries this handler does not ask for:
// one whose process died before it finalized and whose AckWait therefore expired still counts
// against max_deliver.
const unplaceableMaxDeliveries = consumerMaxDeliver - 2

// unplaceableDeferralCap bounds how many agent deliveries this listener holds deferred at one
// time, across every handler it builds (unplaceableDeferrals). A deferred message is NAKed
// rather than ACKed, so it keeps one of the durable's MaxAckPending slots
// (consumerMaxAckPending, durable.go: 256) for the whole window instead of giving it back at
// once: without a bound, enough concurrently unplaceable messages would hold every slot this
// machine's durable has and stall it for every topic, including the deliveries that would land
// on their first attempt. A quarter of the budget leaves three quarters for that traffic. A
// message that arrives while the cap is reached is given up now, with the ordinary give-up
// record. TestUnplaceableRetryWindowFitsTheBoundsItDependsOn pins the relation.
const unplaceableDeferralCap = consumerMaxAckPending / 4

// defaultUnplaceableRetryWindow is the window main.go uses when ENVOY_UNPLACEABLE_RETRY_WINDOW
// is unset or unreadable: a minute of retries, which at unplaceableRetryDelay is 12 deferrals
// and so 13 deliveries, 15 with the margin above and so inside consumerMaxDeliver, and well
// inside the attempt cache's TTL (attemptCacheTTL, main.go), the entry whose clearing keeps each
// redelivery from being swallowed as an attempt-dedupe skip.
// TestUnplaceableRetryWindowFitsTheBoundsItDependsOn pins both.
const defaultUnplaceableRetryWindow = 60 * time.Second

// pushFailedDeferrals is how many times a push_failed delivery (session.ReasonPushFailed) is
// deferred, whatever the window would buy the other two causes: one, so such a message is
// delivered at most twice. The other causes push nothing, so retrying them repeats nothing; this
// one is different. A deferral clears the attempt-cache entry the delivery recorded, the entry
// that otherwise keeps a redelivery from pushing a second time, and session.Deliverer's prompt
// returns the same error for a session server that refused the prompt, a connection that never
// reached it, and an answer lost after the server had taken the prompt - so every retry of a
// push that may have landed repeats an instruction the session may already have. One retry
// covers the blip the retry exists for, a peer restarting or a connection refused for a moment,
// and bounds that repeat at one; a window's dozen would let the same instruction arrive thirteen
// times. TestAPushFailureIsDeferredOnceWhereOtherCausesAreDeferredFurther pins it.
const pushFailedDeferrals = 1

// unplaceableDeferralExpiry is how long a slot in unplaceableDeferrals lasts after the deferral
// that took it, the longest a deferred message can legitimately stay pending before a delivery of
// it reaches this listener's handler again and gives the slot back. It is the sum of three waits:
//   - unplaceableRetryDelay (5 s), the NAK delay the deferral asks for: the stream redelivers no
//     sooner;
//   - consumerAckWait (durable.go, 60 s): finalizeAfter's NAK is fire-and-forget, and one lost on
//     the wire leaves the message pending until the AckWait of the delivery that was deferred
//     runs out, which began before the slot was taken, so it ends inside this term;
//   - a margin of one more consumerAckWait for the redelivery to wait in this listener's client
//     buffer behind the deliveries ahead of it before its handler runs the release. A handler that
//     is further behind than that has let the stream redeliver the message again in the meantime.
//
// 125 s in all. A slot older than that belongs to a message that is not coming back here - the
// stream dropped or purged it, or its redelivery reached another subscriber - and take reclaims
// it. TestUnplaceableRetryWindowFitsTheBoundsItDependsOn pins that it exceeds the first two
// terms, so a message that is still legitimately pending never has its slot reclaimed early.
const unplaceableDeferralExpiry = unplaceableRetryDelay + consumerAckWait + consumerAckWait

// unplaceableDeferrals is the set of agent deliveries this listener holds deferred right now,
// keyed on each message's stream sequence (nats.MsgMetadata.Sequence.Stream), and the budget
// unplaceableDeferralCap bounds. It is keyed rather than counted so that only a slot this
// listener took ever comes back: a redelivery is no evidence that this listener deferred the
// message - the interest path's ShouldNAK and an expired AckWait redeliver too, and they come in
// step with deferrals (a local session whose server is down produces interest-path NAKs and
// direct-path deferrals at once) - so a count released on every redelivery could read zero while
// the cap's worth of messages were still held.
//
// What the set guarantees: it never holds more than unplaceableDeferralCap live entries, and an
// entry is added only by a deferral this listener made. It leaves by one of two routes: that same
// message's next delivery releases it, or it expires unplaceableDeferralExpiry after the deferral
// that took it, the longest that message can legitimately stay pending before a delivery of it
// reaches this handler, and the next take or size sweeps it out. So at any moment the messages
// this process is holding deferred, within that bound, number at most the cap. What it cannot
// know, since it is this process's memory and not the stream's:
//   - a listener restart forgets it. The previous process's deferrals stay pending in the durable
//     until each one's NAK delay ends and it is redelivered, when the new process either takes a
//     slot for it or gives it up; until then the durable can hold the old process's cap beside
//     the new one's, which TestUnplaceableRetryWindowFitsTheBoundsItDependsOn keeps under
//     consumerMaxAckPending (twice the cap is below it).
//   - a message deferred and never delivered here again - one the stream drops or purges first,
//     or whose redelivery reaches some other subscriber - keeps its entry until it expires. Such
//     an entry holds no slot of the durable's; until it expires it holds one of the set's, so a
//     cap's worth of them gives each unplaceable message up on its first delivery, with its
//     record, for at most unplaceableDeferralExpiry before the retry resumes.
//   - a message still pending past unplaceableDeferralExpiry - a handler further behind its
//     redeliveries than the expiry's margin allows - loses its slot while the durable still
//     holds it, and the set undercounts by one such message until its delivery arrives, takes
//     nothing back on release, and is placed, deferred again or given up like any other.
type unplaceableDeferrals struct {
	mu sync.Mutex
	// held maps each held message's stream sequence to the time its slot expires.
	held map[uint64]time.Time
	// now is the clock the expiries are read against: time.Now, which a test replaces to step
	// past an expiry without waiting it out.
	now func() time.Time
}

func newUnplaceableDeferrals() *unplaceableDeferrals {
	return &unplaceableDeferrals{held: make(map[uint64]time.Time, unplaceableDeferralCap), now: time.Now}
}

// take records the message at stream sequence seq as deferred and reports whether it fits inside
// unplaceableDeferralCap. It first sweeps out every slot whose expiry has come
// (unplaceableDeferralExpiry), so neither the cap check nor the held check below counts a message
// that is not coming back; an expired entry for seq itself is swept like any other, and seq is
// taken afresh. Past the cap it records nothing and reports false, and the caller gives the
// message up. A sequence already held reports true without a second slot: one message holds one
// slot however many deliveries of it race, and its expiry is counted again from this deferral,
// whose NAK restarts the wait the expiry bounds. The sweep, the check and the insert are one
// critical section, so two deliveries racing for the last slot cannot both take it.
func (d *unplaceableDeferrals) take(seq uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	d.sweepLocked(now)
	if _, ok := d.held[seq]; !ok && len(d.held) >= unplaceableDeferralCap {
		return false
	}
	d.held[seq] = now.Add(unplaceableDeferralExpiry)
	return true
}

// release gives back the slot the message at stream sequence seq holds, and nothing when it holds
// none: a delivery this listener never deferred has no slot to return.
func (d *unplaceableDeferrals) release(seq uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.held, seq)
}

// size reports how many live slots are held, for a test to read the budget back. It sweeps the
// expired ones out first, as take does, so it reports the count take's cap check would see.
func (d *unplaceableDeferrals) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sweepLocked(d.now())
	return len(d.held)
}

// sweepLocked deletes every slot whose expiry is at or before now. The caller holds d.mu.
func (d *unplaceableDeferrals) sweepLocked(now time.Time) {
	for seq, expiry := range d.held {
		if !now.Before(expiry) {
			delete(d.held, seq)
		}
	}
}

// unplaceableAgentDelivery answers an agent delivery that nothing placed and nothing failed hard
// enough to NAK (agentSubjectDelivery's undelivered, error-free result). It either defers the
// message for redelivery, finalizing it itself and reporting true so the caller returns at once,
// or gives it up with its record and reports false, leaving the caller to finalize the message as
// it finalizes every other outcome.
func unplaceableAgentDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string, metadata *nats.MsgMetadata, result session.HandleAgentResult) bool {
	// Nothing placed this message and nothing failed hard enough to NAK: the session has
	// no record this listener could find, its record names nothing to push to, or the push
	// to the port it names did not land. Each can be true of a session that is about to be
	// placeable - a record still propagating from the bucket, a server still binding its
	// serve port, a session process restarting - so the message is kept in the stream and
	// redelivered for as long as cfg.unplaceableRetryWindow, or once for a push that failed
	// (pushFailedDeferrals), and only then given up, with a record and a counter of its own
	// rather than the ACKed skip that lost it.
	//
	// reason names which of the three causes this is, for the operator and for the counter
	// label. session.ReasonNone keeps no_push_target's label only because the switch needs a
	// default: in a deployed listener nothing reaches here with it. HandleAgentMessage
	// answers undelivered, error-free and ReasonNone only for session.ErrWrongMachine, and
	// no production code returns that error - its one producer is the test transport in
	// TestE2E_AgentToAgent_RemoteMachine (internal/integration/e2e_test.go). The arm is
	// defensive, and no_push_target on a dashboard never means it.
	reason := "no_push_target"
	giveUpStatus := "unplaceable_no_push_target"
	switch result.Reason {
	case session.ReasonNoSessionRecord:
		reason = "no_session_record"
		giveUpStatus = "unplaceable_no_record"
	case session.ReasonPushFailed:
		reason = "push_failed"
		giveUpStatus = "unplaceable_push_failed"
	}
	// The window is counted from this message's first delivery, not from the time the
	// stream stored it: a backlog replayed into a listener whose caches are still cold -
	// after a restart, during a rolling deploy - is minutes old when it is first delivered,
	// and that is the very case this retry exists for, which a store-timestamp clock gives
	// up on at once. Each deferral costs unplaceableRetryDelay, so the window buys
	// window/unplaceableRetryDelay of them, and md.NumDelivered counts them: the first
	// delivery is 1, and the attempt after the last deferral the window paid for is given
	// up. It is a bound, not a promise of that much wall-clock time - a delivery this
	// handler never asked for, one whose AckWait expired because the handler holding it
	// died, counts too.
	var windowDeferrals uint64
	if cfg.unplaceableRetryWindow > 0 {
		windowDeferrals = uint64(cfg.unplaceableRetryWindow / unplaceableRetryDelay)
	}
	// A push that failed may have landed, so it is retried once and no more
	// (pushFailedDeferrals); the two causes that push nothing keep the window's whole
	// allowance. The deferral line below reports this allowance, the number the decision
	// was made on, under window_deferrals.
	if result.Reason == session.ReasonPushFailed {
		windowDeferrals = min(windowDeferrals, pushFailedDeferrals)
	}
	// What does not hold the retry open is session.ReasonNone, the defensive arm above:
	// should a deliverer ever return session.ErrWrongMachine, the session it names is
	// another listener's to place, however often this one sees the message. Every outcome a
	// deployed listener reaches here with carries one of the three reasons - no entry is
	// ReasonNoSessionRecord, a record with no port ReasonNoPushTarget, and any error the
	// push itself returns, a refusal or an unreachable peer, ReasonPushFailed
	// (internal/session/handler.go, reasonFor) - and the interest path's failures return
	// ShouldNAK with an Err set and are answered by agentSubjectDelivery. A read that did not
	// answer is no evidence of absence and must not end the retries, and it does not: it leaves
	// no entry, and an attempt with no entry reports ReasonNoSessionRecord here, so the reason
	// alone already holds the window open for it.
	placementMayChange := result.Reason != session.ReasonNone
	deferred := false
	if metadata != nil && placementMayChange && metadata.NumDelivered <= windowDeferrals &&
		metadata.NumDelivered < unplaceableMaxDeliveries {
		// The slot is this message's own, keyed on its stream sequence, so only this
		// message's next delivery gives it back, or its expiry (unplaceableDeferralExpiry) when
		// that delivery never reaches this listener. A full set takes nothing, and the delivery
		// falls through to the ordinary give-up below.
		deferred = cfg.deferredUnplaceable.take(metadata.Sequence.Stream)
	}
	if deferred {
		// No exception and no delivery counter: the message is not disposed of yet, and
		// publishing the no_holder notice per attempt would send a dozen of them, each one
		// read back by every listener in the fleet. The attempt-cache entry agentSubjectDelivery
		// recorded must go, or the redelivery this NAK asks for is swallowed as an attempt-dedupe
		// skip before it reaches the session at all.
		//
		// The line carries the two numbers the decision was made on. It carried the
		// message's stream age before, which no longer bounds anything: a line naming it
		// would invite exactly the reading this clock was changed to end.
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID: sessionID,
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener agent delivery deferred", sessionID, item.Topic, item.EventID, "deferred",
					slog.String("reason", reason),
					slog.Uint64("attempt", metadata.NumDelivered),
					slog.Uint64("window_deferrals", windowDeferrals),
				)
			},
			clearAttempt: true,
		})
		cfg.messagesNAKed.Inc()
		message.finalizeAfter(unplaceableRetryDelay)
		return true
	}
	// Given up: the deferrals this cause is allowed are spent, the delivery count is reached,
	// this listener is already holding unplaceableDeferralCap messages, the metadata could
	// not be read, or this is a core delivery nothing would redeliver. The give-up is
	// terminal whatever becomes of its no_holder notice: once the push allowance is spent,
	// nothing here may restore the state that permits another push, and the record of the
	// give-up must not depend on the bus that just failed to place the message.
	//
	// So applyDeliveryOutcome writes the counter and the give-up line first, then publishes
	// the notice, once per message rather than once per attempt. A notice that does not go
	// out is logged exceptionPublishFailedLine and nothing else: it neither NAKs the message
	// nor clears its attempt-cache entry. The entry is kept rather than cleared because it is
	// what stops a further delivery of this message pushing again - the redelivery an ACK
	// lost on the wire brings (finalize's ACK is fire-and-forget), which arrives after the
	// durable's AckWait and well inside attemptCacheTTL - and agentSubjectDelivery ACKs the
	// message once this returns.
	//
	// What this gives up is the notice itself, when it cannot be published. That is cheaper
	// than the retry it replaces. For an agent subject nothing in this deployment reads the
	// no_holder exception: the Legion daemon's NATS grant subscribes only to the role lanes'
	// exception subjects (infra/core, DaemonPermissions), while the give-up line, its counter
	// and exceptionPublishFailedLine are what an operator does read. The retry NAKed the
	// message and cleared its attempt entry, so every redelivery ran the whole handler again
	// and pushed a message whose push allowance was already spent; and the failure it retried
	// is correlated with the fault (the bus that will not take the notice is the one whose
	// trouble made the message unplaceable) and can be permanent - the notice embeds the
	// original payload, so an envelope near the server's max payload yields a notice refused
	// bus.ErrTooLarge on every attempt - which ran the message to the durable's MaxDeliver
	// with no give-up record at all.
	applyDeliveryOutcome(cfg, item, deliveryOutcome{
		sessionID:    sessionID,
		metricStatus: giveUpStatus,
		log: func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelWarn, "listener agent delivery given up", sessionID, item.Topic, item.EventID, giveUpStatus, slog.String("reason", reason))
		},
		exceptionReason: "no_holder",
	})
	return false
}
