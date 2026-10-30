package delivery

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/dispatch/duty"
)

// durableBindInterval is how often an intake that does not deliver tries for its durable's hold,
// and for the durable under it. It is cmd/listener/durable.go's interval: a standby polls every two
// seconds, so it takes the durable over within one interval of the holder releasing its hold, and
// within one interval of the hold's lapse when the holder dies (intakeHoldTTL).
//
// Unlike the listener, which gives up at its durableBindDeadline and exits so the runtime starts
// it again, Dispatch's standby polls for the life of the process: there is no deadline and no
// exit. A durable another envoy-dispatch process holds is not a failed start but the ordinary
// state of every process but one, which has every other thing a Dispatch server does to get on
// with meanwhile.
const durableBindInterval = 2 * time.Second

// standbyLogEvery bounds how often one intake says it is standing by for its durable. The first
// refusal is logged as it happens, so an operator sees at once which process stood by and why;
// the hold is then tried every durableBindInterval, and an unthrottled line would be 1,800 an hour
// per durable, each saying exactly what the first said.
const standbyLogEvery = 30 * time.Second

// The hold a process delivers from a durable under (intakeHoldName, duty.Hold): it lasts
// intakeHoldTTL on the database's clock after each renewal, is renewed every intakeHoldRenew, and
// reports itself lost intakeHoldMargin before it lapses, so the holder stops delivering before any
// standby can take it. A holder that dies is replaced within intakeHoldTTL plus one
// durableBindInterval. The holder checks that the durable still delivers to it on the same
// interval it renews (heldDurable.check).
const (
	intakeHoldTTL    = 15 * time.Second
	intakeHoldRenew  = 5 * time.Second
	intakeHoldMargin = 2 * time.Second
)

// intakeHoldName names the hold a process delivers from durable under. Its token records what the
// next holder needs (intakeHoldToken).
func intakeHoldName(durable string) string { return "delivery-intake-" + durable }

// intakeHoldToken is what an intake's hold records as its token:
//
//   - subject, the deliver subject the last holder made the durable deliver to, which tells the
//     next holder whether a binding it finds is a holder's (heldDurable.boundElsewhere);
//   - start, the stream sequence a take-over that deleted the durable creates it again at, or 0
//     for none. A take-over records it before its delete and clears it once its create succeeds.
//     What a take-over that never creates the durable again leaves in the row, and who reads
//     that start next, is on heldDurable.finishTakeOver. A holder that stops with the start set
//     lapses its hold rather than deleting the row (heldDurable.stop), so the next holder still
//     reads it.
//
// The token is the subject alone, or the subject followed by intakeHoldTokenStart and the
// sequence in decimal. A deliver subject holds no space, so the two never mix. A token of any
// other shape reads as a subject alone.
type intakeHoldToken struct {
	subject string
	start   uint64
}

// intakeHoldTokenStart separates an intake hold token's subject from its start.
const intakeHoldTokenStart = " start="

func (t intakeHoldToken) String() string {
	if t.start == 0 {
		return t.subject
	}
	return t.subject + intakeHoldTokenStart + strconv.FormatUint(t.start, 10)
}

// parseIntakeHoldToken reads the token a hold's row records (intakeHoldToken).
func parseIntakeHoldToken(token string) intakeHoldToken {
	subject, start, found := strings.Cut(token, intakeHoldTokenStart)
	if !found {
		return intakeHoldToken{subject: token}
	}
	sequence, err := strconv.ParseUint(start, 10, 64)
	if err != nil || sequence == 0 {
		return intakeHoldToken{subject: token}
	}
	return intakeHoldToken{subject: subject, start: sequence}
}

// intakeLeases is the pool an intake takes its durable's hold and consumer lease on and renews the
// hold through: store.Pool.Leases, which StartIntakes hands each intake. A *pgxpool.Pool is both.
type intakeLeases interface {
	duty.Leases
	duty.Holds
}

// consumerLease names the lease every change to one durable runs under: this build's take-over
// (heldDurable.takeOver), and the configuration reconciliation of the build before holds, whose
// processes still run during a rolling release and take the same lease. So the two never delete,
// create or update one durable at once.
//
// The name carries the durable because that, and only that, is what the exclusion is about: two
// processes over one durable. One name over both delivery durables coupled two independent
// reconciliations, and INFRA-66 measured what that cost against a real nats-server and the real
// envoy-dispatch binary, with the Dispatch user denied exactly
// $JS.API.CONSUMER.CREATE.ENVOY_NOTIFICATIONS.delivery-events.> (envoy-infra's live row "without
// the delivery consumer create Dispatch leaves only that intake unbound"). A denied AddConsumer
// draws no reply at all, so the create blocks for the client's whole JetStream MaxWait -- 10 s
// (internal/bus/nats.go, nats.MaxWait) -- and the lease is held for every second of it. The events
// intake took the lease again on its next durableBindInterval poll and held it for another 10 s,
// round after round:
//
//	17:57:16.232 ERROR dispatch delivery: create NATS consumer name=delivery-events error="context deadline exceeded"
//	17:57:16.232 INFO  dispatch duty: leased pass ran  lease=delivery-consumers
//	17:57:26.235 ERROR dispatch delivery: create NATS consumer name=delivery-events error="context deadline exceeded"
//	17:57:26.236 INFO  dispatch duty: leased pass ran  lease=delivery-consumers
//
// Under that one name the job intake, whose own durable is created by a pass under the same lease,
// could only get in during the milliseconds between those holds: in CI's run of that row it never
// did, and the row failed with "consumer delivery-jobs was not push-bound within 20s". A lease per
// durable keeps a stuck change to the durable it is stuck on.
func consumerLease(durable string) string { return "delivery-consumer-" + durable }

// bindThrottle is one intake's own standby-log state for its own durable: when it last said it
// was standing by for it, and why. Per intake rather than a package-global map keyed by consumer
// name, because an intake is exactly what the window is about -- one process's repeated polls of
// one durable -- and a global outlives the durable it throttles: a second intake of the same name
// would stand by silently for a durable it has said nothing about.
type bindThrottle struct {
	mu sync.Mutex
	// standby is when the last standby line was logged and standbyReason the message it carried,
	// so a change of state -- the hold going from another process's to a binding no hold recorded
	// -- is said at once rather than waiting out the window of the line before it.
	standby       time.Time
	standbyReason string
}

// reportStandby logs message at INFO -- this durable is not this process's to deliver from yet,
// for the reason message names -- unless this intake logged that same message within
// standbyLogEvery.
func (t *bindThrottle) reportStandby(name, message string) {
	t.mu.Lock()
	if t.standbyReason == message && time.Since(t.standby) < standbyLogEvery {
		t.mu.Unlock()
		return
	}
	t.standby, t.standbyReason = time.Now(), message
	t.mu.Unlock()
	slog.Info(message, "name", name)
}

// heldDurable is one intake's delivery from its durable, which it does only while it holds the
// durable's hold (intakeHoldName), so that of every envoy-dispatch process -- of this build and of
// the build before it, during a rolling release -- one delivers from the durable at a time:
//
//   - A standby tries for the hold every durableBindInterval and takes it when no holder holds it
//     live: released, lapsed, or never taken.
//   - Under the hold, and under the durable's consumer lease, it takes the durable over
//     (takeOver): it records the stream sequence after the durable's ack floor in the hold's
//     token, deletes the durable, subscribes to a fresh deliver subject, creates the durable again
//     at that sequence on that subject, and records the subject alone as the hold's token
//     (intakeHoldToken). The delete and create, rather than an update of the deliver subject,
//     redeliver a dead holder's unacked messages at once and fence the holder off: a
//     subscription its client sends again after a reconnect is on a subject the durable no
//     longer delivers to.
//     nats-server refuses that update anyway while the old subject still has interest, which a
//     dead connection keeps for minutes; a delete has no such check. Subscribing before the
//     create leaves the durable push-bound from the moment it exists. A durable bound to a
//     subject no hold recorded is a process of the build before holds, which binds by lookup and
//     holds nothing: it is never taken from it, and the take-over waits until it lets go.
//   - The holder stops delivering at once when its hold is lost, and when its check finds the
//     durable delivering to another subject (check); it hands a message back rather than handle
//     it once its hold's loss deadline has passed (intakeDelivery.receive).
//   - When its process stops, it stops delivering, handing back every message it was delivered
//     and did not handle, and then releases the hold (stop), so a standby takes over at once and
//     is redelivered those messages within seconds rather than after the ack wait.
//
// The run loop owns hold, recorded and subject; mu guards delivering, which stopDelivering reads
// from any goroutine.
type heldDurable struct {
	nats   *bus.Client
	leases intakeLeases
	// holder names this intake in its hold's row: each intake draws one as it is built.
	holder uuid.UUID
	// throttle is this intake's own standby-log state (bindThrottle).
	throttle *bindThrottle
	// config is the durable's config in this build; each call draws a fresh deliver subject.
	config func() natsgo.ConsumerConfig
	// name is the durable's name: config's Durable, which every call returns alike.
	name string
	// handle handles one message on the subscription's delivery goroutine and acknowledges it.
	handle natsgo.MsgHandler

	// hold is the durable's hold while this intake holds it.
	hold *duty.Hold
	// recorded is the token the hold's row records (intakeHoldToken): the deliver subject the last
	// holder made the durable deliver to, which may be this intake's own, and the start a take-over
	// that deleted the durable and did not create it again recorded.
	recorded intakeHoldToken
	// subject is the deliver subject this intake delivers at, and empty while it delivers nothing.
	subject string

	mu sync.Mutex
	// delivering is the latest subscription subscribe made, which stopDelivering stops.
	delivering *intakeDelivery
}

// newHeldDurable builds the delivery from config's durable on natsClient, taking its hold and
// consumer lease on leases and handing each message to handle.
func newHeldDurable(natsClient *bus.Client, leases intakeLeases, config func() natsgo.ConsumerConfig, handle natsgo.MsgHandler) heldDurable {
	return heldDurable{nats: natsClient, leases: leases, holder: uuid.New(), throttle: &bindThrottle{}, config: config, name: config().Durable, handle: handle}
}

// run delivers from the durable while this intake holds its hold, and stands by for it otherwise,
// until ctx ends; then it stops delivering and releases the hold (stop). Each durableBindInterval
// while it delivers nothing, and ready reports true, it tries for the hold and the durable
// (attempt); each intakeHoldRenew while it delivers, it checks the durable still delivers to it
// (check); and the moment its hold is lost it stops delivering. extra and onExtra are a caller's
// own ticker and what it does on each tick, nil for none.
func (d *heldDurable) run(ctx context.Context, extra <-chan time.Time, onExtra func(), ready func() bool) {
	defer d.stop()
	binds := time.NewTicker(durableBindInterval)
	defer binds.Stop()
	checks := time.NewTicker(intakeHoldRenew)
	defer checks.Stop()
	attempt := func() {
		if d.subject == "" && ready() {
			d.attempt(ctx)
		}
	}
	attempt()
	for {
		// lost stays nil, and so never ready, while this intake holds no hold.
		var lost <-chan struct{}
		if d.hold != nil {
			lost = d.hold.Lost()
		}
		select {
		case <-ctx.Done():
			return
		case <-extra:
			onExtra()
		case <-binds.C:
			attempt()
		case <-checks.C:
			d.check(ctx)
		case <-lost:
			slog.Warn("dispatch delivery: the intake's hold is lost; it stops delivering", "name", d.name)
			d.stopDelivering()
			d.hold, d.subject = nil, ""
		}
	}
}

// attempt tries to make this intake the one delivering from the durable: it takes the hold when
// no other process holds it live, then takes the durable over under it (takeOver). A durable
// bound to a subject no hold recorded is read first outside the consumer lease, so a holder that
// waits for a process of the build before holds to let go takes no lease on every poll.
func (d *heldDurable) attempt(ctx context.Context) {
	if d.hold == nil && !d.takeHold(ctx) {
		return
	}
	info, err := d.lookupDurable(ctx)
	if err == nil && d.boundElsewhere(info) {
		return
	}
	// The take-over asked for mutual exclusion and nothing else, and no tick is due (the zero time).
	duty.Exclusive(ctx, d.leases, consumerLease(d.name), time.Time{}, 0, d.takeOver)
}

// lookupDurable reads the durable's state from the server, bounded by intakeHoldRenew.
func (d *heldDurable) lookupDurable(ctx context.Context) (*natsgo.ConsumerInfo, error) {
	lookup, cancel := context.WithTimeout(ctx, intakeHoldRenew)
	defer cancel()
	return d.nats.JS().ConsumerInfo(bus.Stream, d.name, natsgo.Context(lookup))
}

// heldElsewhere is the standby line of an intake whose durable's hold another process holds live.
const heldElsewhere = "dispatch delivery: NATS consumer held by another process; standing by"

// takeHold takes the durable's hold when no holder holds it live, keeping the token its row
// records, and reports whether this intake holds it now. duty.TryHold records its own token as it
// takes a lapsed hold, so the lapsed hold's token is read first (duty.HoldToken).
func (d *heldDurable) takeHold(ctx context.Context) bool {
	holdName := intakeHoldName(d.name)
	bounded, cancel := context.WithTimeout(ctx, intakeHoldRenew)
	defer cancel()
	token, live, err := duty.HoldToken(bounded, d.leases, holdName)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			slog.Error("dispatch delivery: read the intake's hold", "name", d.name, "hold", holdName, "error", err)
		}
		return false
	case live:
		d.throttle.reportStandby(d.name, heldElsewhere)
		return false
	}
	hold, err := duty.TryHold(bounded, d.leases, holdName, d.holder, duty.HoldOptions{
		TTL: intakeHoldTTL, Renew: intakeHoldRenew, Margin: intakeHoldMargin, Token: token,
	})
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("dispatch delivery: take the intake's hold", "name", d.name, "hold", holdName, "error", err)
		}
		return false
	}
	if hold == nil {
		d.throttle.reportStandby(d.name, heldElsewhere)
		return false
	}
	d.hold, d.recorded = hold, parseIntakeHoldToken(token)
	slog.Info("dispatch delivery: took the intake's hold", "name", d.name, "hold", holdName)
	return true
}

// boundElsewhere reports whether info is a durable bound to a subject the hold does not record: a
// process of the build before holds, which this intake never takes the durable from. It says so
// at INFO, throttled.
func (d *heldDurable) boundElsewhere(info *natsgo.ConsumerInfo) bool {
	if !info.PushBound || info.Config.DeliverSubject == d.recorded.subject {
		return false
	}
	d.throttle.reportStandby(info.Config.Durable, "dispatch delivery: NATS consumer bound to a process that holds no hold; standing by until it lets go")
	return true
}

// holds reports whether this intake may still act under its hold: the hold's loss deadline has
// not passed (duty.Hold.Deadline).
func (d *heldDurable) holds() bool {
	return d.hold != nil && time.Now().Before(d.hold.Deadline())
}

// takeOver makes the durable deliver to this intake, under the hold and the durable's consumer
// lease. A durable that does not exist is created with this build's config, at the start a
// take-over before this one recorded when it deleted the durable and did not create it again
// (intakeHoldToken). A durable bound to a subject no hold recorded is left as it is
// (boundElsewhere). Any other -- unbound, or bound to the subject the hold records, which is a
// holder that died or was cut off -- is deleted and created again at its ack floor on a fresh
// deliver subject (takeOverConfig), and that start is recorded as the hold's token before the
// delete. The durable's state is read again here, under the lease, since a process of the build
// before holds can bind it between attempt's read and this one.
//
// This intake subscribes to the fresh deliver subject before it creates the durable, so the
// durable is push-bound from the moment it exists. A process of the build before holds binds by
// looking the durable up and binding it when the lookup reports it unbound; a durable created
// before its subscriber would leave a moment in which that lookup finds it unbound, and both
// processes would then be handed every message on one subject. The subscribe sends its SUB on
// the client's connection before the create's request goes out on the same connection, and the
// server handles one connection's protocol in order, so the subject has interest before the
// create is even read (TestATakeOverCreatesTheDurableAlreadyBound). Then this intake records the
// subject alone as the hold's token, still under the lease.
//
// From the delete on -- or from the lookup, when it finds no durable -- the take-over runs to its
// end inside this one lease (finishTakeOver, which says what a lease given up with the durable
// absent leaves behind).
func (d *heldDurable) takeOver(ctx context.Context) {
	js := d.nats.JS()
	wanted := d.config()
	name := wanted.Durable
	create := wanted
	deleteFirst := false
	info, err := js.ConsumerInfo(bus.Stream, name, natsgo.Context(ctx))
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
		if d.recorded.start != 0 {
			create = startingAt(wanted, d.recorded.start)
		}
	case err != nil:
		slog.Error("dispatch delivery: look up NATS consumer", "name", name, "error", err)
		return
	case d.boundElsewhere(info):
		return
	default:
		create = takeOverConfig(info, wanted)
		if !d.holds() {
			return
		}
		pending := intakeHoldToken{subject: d.recorded.subject, start: create.OptStartSeq}
		if err := d.hold.SetToken(ctx, pending.String()); err != nil {
			slog.Error("dispatch delivery: record the take-over's start before deleting NATS consumer", "name", name, "start_seq", create.OptStartSeq, "error", err)
			return
		}
		d.recorded = pending
		deleteFirst = true
	}
	d.finishTakeOver(create, deleteFirst)
}

// takeOverConfig is the config a take-over recreates the durable deployed describes with: the
// deployed config when a newer build stamped it with a higher generation, this build's (wanted)
// otherwise, on a fresh deliver subject, starting at the stream sequence after the durable's ack
// floor. Starting there, the recreated durable redelivers at once every message the durable had
// not acknowledged -- what a dead holder had been handed, what a stopped one handed back --
// replays nothing it had, and skips nothing (TestBindReplacesAPluralFilterDurableAtItsOwnAckFloor).
// The take-over records that start in its hold's token before it deletes the durable, and
// finishTakeOver says what a create that fails after the delete leaves behind.
// The deployed metadata keeps its own keys and drops the ones nats-server sets itself (the
// `_nats.` prefix), which the server sets again on create.
func takeOverConfig(deployed *natsgo.ConsumerInfo, wanted natsgo.ConsumerConfig) natsgo.ConsumerConfig {
	config := wanted
	if bus.MetadataGeneration(deployed.Config.Metadata, consumerGenerationKey) > consumerConfigGeneration {
		config = deployed.Config
		config.DeliverSubject = natsgo.NewInbox()
		config.OptStartTime = nil
		config.Metadata = bus.OwnMetadata(deployed.Config.Metadata, 0)
	}
	return startingAt(config, deployed.AckFloor.Stream+1)
}

// startingAt is config delivering from stream sequence start on.
func startingAt(config natsgo.ConsumerConfig, start uint64) natsgo.ConsumerConfig {
	config.DeliverPolicy = natsgo.DeliverByStartSequencePolicy
	config.OptStartSeq = start
	return config
}

// The bounds on finishTakeOver, the part of a take-over that runs after it has recorded the
// durable's start, and that must not give up the consumer lease with the durable deleted.
//
// takeOverFinishWithin bounds the whole of it, from its first step to its last try. It is under
// intakeHoldTTL - intakeHoldMargin (13 s), how long a hold lasts on its last renewal: a hold taken
// just before the take-over (takeHold) therefore holds through all 10 s of the steps, even when
// every renewal after it fails. A take-over that runs again after a check found the durable
// delivering elsewhere (check) begins at any point in the renewal cycle of a hold this intake
// already held, so up to intakeHoldRenew (5 s) after its last renewal: with the renewals from
// there on failing, about 8 s of the hold are left against the 10 s bound, and retryHeld stops at
// the hold's deadline. That exit is the safe one -- no step acts once another process can take the
// hold, and the next attempt takes the durable over again. takeOverFinishWithin is also well
// inside stopGrace (cmd/dispatch), since a stop that comes meanwhile waits for it to end.
//
// takeOverCallTimeout bounds each try of each step, so that one call that draws no answer -- a
// JetStream request during a leader change, or one the server denies and never answers -- leaves
// room for the tries after it. A JetStream leader election ordinarily ends within it.
//
// A failed try waits takeOverFirstRetry before the next one, doubling after each try up to
// takeOverLastRetry: with calls that fail at once, that is eight tries within takeOverFinishWithin.
const (
	takeOverFinishWithin = 10 * time.Second
	takeOverCallTimeout  = 3 * time.Second
	takeOverFirstRetry   = 250 * time.Millisecond
	takeOverLastRetry    = 2 * time.Second
)

// finishTakeOver makes the durable deliver to this intake at create's start, under the consumer
// lease its caller (takeOver) holds: it deletes the durable when deleteFirst is set, subscribes
// to create's deliver subject, creates the durable with create, and records the subject alone as
// the hold's token. Each step that fails is tried again (retryHeld) until it succeeds, this intake
// no longer holds its hold, or takeOverFinishWithin has passed. It runs on a context of its own
// rather than the run's, so a stop that comes while the durable is deleted does not leave it so.
// Every step is safe to try again: a delete of a durable already gone succeeds, and a create of
// a durable that a timed-out try did create, with this same config, returns that durable.
//
// A create that fails after the delete is therefore tried again inside the one consumer lease, so
// among builds that take holds such a create loses neither the durable nor its start. The lease is
// given up with the durable absent only when this process is killed between the delete and the
// create, when it loses its hold, or when every try fails within takeOverFinishWithin. The start
// then stays in the hold's token (intakeHoldToken), and the next take-over of this build -- by
// this intake, or by the process that takes the hold next -- creates the durable at that old ack
// floor rather than at the default start (deliveryConsumerConfig names none, so the stream's first
// message). A process of the build before holds reconciles the durable under the same lease and
// never reads the token: finding the durable absent, it creates it at its own default start, the
// stream's first message for the events durable and the next new message for the jobs durable
// (TestATakeOverWhoseCreateFailsCreatesTheDurableBeforeAPreviousBuildReconciles).
func (d *heldDurable) finishTakeOver(create natsgo.ConsumerConfig, deleteFirst bool) {
	js := d.nats.JS()
	finish, cancel := context.WithTimeout(context.Background(), takeOverFinishWithin)
	defer cancel()
	if deleteFirst {
		if d.retryHeld(finish, "delete NATS consumer to take it over", func(call context.Context) error {
			if err := js.DeleteConsumer(bus.Stream, d.name, natsgo.Context(call)); err != nil && !errors.Is(err, natsgo.ErrConsumerNotFound) {
				return err
			}
			return nil
		}) != nil {
			return
		}
	}
	if d.retryHeld(finish, "bind NATS consumer", func(context.Context) error {
		_, err := d.subscribe(create.DeliverSubject)
		return err
	}, "subject", create.FilterSubject) != nil {
		return
	}
	if d.retryHeld(finish, "create NATS consumer", func(call context.Context) error {
		_, err := createDurable(js, &create, natsgo.Context(call))
		return err
	}, "subject", create.FilterSubject, "start_seq", create.OptStartSeq) != nil {
		d.stopDelivering()
		return
	}
	bound := intakeHoldToken{subject: create.DeliverSubject}
	if d.retryHeld(finish, "record the intake's deliver subject", func(call context.Context) error {
		return d.hold.SetToken(call, bound.String())
	}) != nil {
		// The durable delivers here, but the hold does not say so: the next holder would take
		// the binding for a process that holds nothing and wait for it. Stop, and take the
		// durable over again on the next attempt.
		d.stopDelivering()
		return
	}
	d.recorded, d.subject = bound, create.DeliverSubject
	slog.Info("dispatch delivery: NATS consumer bound", "name", d.name, "subject", create.FilterSubject,
		"start_seq", create.OptStartSeq, "generation", bus.MetadataGeneration(create.Metadata, consumerGenerationKey))
}

// errHoldNotHeld is retryHeld finding that this intake no longer holds its hold.
var errHoldNotHeld = errors.New("the intake no longer holds its hold")

// retryHeld calls step until it succeeds, while this intake holds its hold and finish has not
// ended, waiting takeOverFirstRetry after the first failed try and twice as long after each one
// after it, up to takeOverLastRetry. Each try runs on a context that ends takeOverCallTimeout
// after it starts, or at the hold's loss deadline if that comes first, so no step acts once
// another process can hold the hold. A failed try is logged at WARN as doing what names, and the
// failure it gives up on at ERROR by failedStep, so a caller writes no step's text a second time
// and keeps only what it does next. It returns nil once step succeeds, and otherwise the last
// try's error, or errHoldNotHeld. attributes are the caller's own on that ERROR line.
func (d *heldDurable) retryHeld(finish context.Context, doing string, step func(context.Context) error, attributes ...any) error {
	wait := takeOverFirstRetry
	for {
		if !d.holds() {
			return d.failedStep(doing, errHoldNotHeld, attributes)
		}
		deadline := time.Now().Add(takeOverCallTimeout)
		if lost := d.hold.Deadline(); lost.Before(deadline) {
			deadline = lost
		}
		call, cancel := context.WithDeadline(finish, deadline)
		err := step(call)
		cancel()
		if err == nil {
			return nil
		}
		slog.Warn("dispatch delivery: take-over step failed; trying it again", "name", d.name, "step", doing, "retry_in", wait, "error", err)
		timer := time.NewTimer(wait)
		select {
		case <-finish.Done():
			timer.Stop()
			return d.failedStep(doing, err, attributes)
		case <-timer.C:
		}
		wait = min(2*wait, takeOverLastRetry)
	}
}

// failedStep logs the take-over step retryHeld gave up on at ERROR, as "dispatch delivery: " and
// doing, with the durable's name, the caller's own attributes and err, and answers err.
func (d *heldDurable) failedStep(doing string, err error, attributes []any) error {
	slog.Error("dispatch delivery: "+doing, append(append([]any{"name", d.name}, attributes...), "error", err)...)
	return err
}

// createDurable creates the durable config describes on the notification stream. A var so a test
// can make a take-over's create fail after its delete.
var createDurable = func(js natsgo.JetStreamContext, config *natsgo.ConsumerConfig, opts ...natsgo.JSOpt) (*natsgo.ConsumerInfo, error) {
	return js.AddConsumer(bus.Stream, config, opts...)
}

// check stops delivering when the durable no longer delivers to this intake's deliver subject:
// something deleted it, or replaced it with one on another subject. The intake keeps its hold, and
// its next attempt takes the durable over again, or waits for a process that binds without a
// hold. A lookup that fails says nothing, and changes nothing.
func (d *heldDurable) check(ctx context.Context) {
	if d.subject == "" {
		return
	}
	info, err := d.lookupDurable(ctx)
	switch {
	case errors.Is(err, natsgo.ErrConsumerNotFound):
	case err != nil:
		return
	case info.Config.DeliverSubject == d.subject:
		return
	}
	slog.Warn("dispatch delivery: the NATS consumer no longer delivers to this intake; it stops delivering", "name", d.name)
	d.stopDelivering()
	d.subject = ""
}

// stop is the end of run: it stops delivering, then releases the hold, so a standby takes the
// durable over at once and is redelivered whatever this intake handed back. A hold whose token
// still records a take-over's start (intakeHoldToken) is lapsed instead, which frees it as
// Release does and keeps the row, so the next holder reads that start.
func (d *heldDurable) stop() {
	d.stopDelivering()
	if d.hold == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), intakeHoldRenew)
	defer cancel()
	release := d.hold.Release
	if d.recorded.start != 0 {
		release = d.hold.Lapse
	}
	if err := release(ctx); err != nil {
		slog.Error("dispatch delivery: release the intake's hold", "name", d.name, "error", err)
	}
	d.hold, d.subject = nil, ""
}

// subscribeDurable subscribes deliver to wanted's durable through client.Subscribe at
// deliverSubject, acknowledging by hand. Bind with DeliverSubject and SkipConsumerLookup
// subscribes without looking the durable up, so the durable need not exist yet: nats.go sends
// the SUB and creates nothing (js.go, the SkipConsumerLookup branch of subscribe with Bind). It
// is for a caller that creates the durable with that deliver subject right after, under its
// consumer lease (takeOver).
func subscribeDurable(client *bus.Client, wanted natsgo.ConsumerConfig, deliverSubject string, deliver natsgo.MsgHandler) (*natsgo.Subscription, error) {
	options := []natsgo.SubOpt{
		natsgo.Bind(bus.Stream, wanted.Durable),
		natsgo.ManualAck(),
		natsgo.DeliverSubject(deliverSubject),
		natsgo.SkipConsumerLookup(),
	}
	// The subject must be the consumer's own filter subject: nats.go checks a bound
	// subscription's subject against it, and refuses an empty one ("subject does not match
	// consumer").
	return client.Subscribe(wanted.FilterSubject, deliver, options...)
}

// intakeDelivery is one subscription of an intake's: the client it subscribed on, the
// subscription, the handler its callback (receive) hands messages to, and the hold it delivers
// under. Each subscribe makes one of its own, so stopping one ends that subscription and nothing a
// later subscribe starts.
type intakeDelivery struct {
	nats   *bus.Client
	sub    *natsgo.Subscription
	handle natsgo.MsgHandler
	// hold is the hold the subscription delivers under: once its loss deadline has passed, every
	// message is handed back.
	hold *duty.Hold
	// handling is held while a message is in its handler, so stop waits for it.
	handling sync.Mutex
	stopping atomic.Bool
	stopped  sync.Once
}

// subscribe subscribes the intake's handling to its durable's push deliveries at deliverSubject
// (subscribeDurable), for a caller that creates the durable with it right after. The
// subscription's callback (receive) handles each message on the subscription's delivery
// goroutine, one at a time, under the hold this intake holds. stopDelivering stops the
// subscription this made last, so a caller subscribing again stops the one before first.
func (d *heldDurable) subscribe(deliverSubject string) (*natsgo.Subscription, error) {
	delivery := &intakeDelivery{nats: d.nats, handle: d.handle, hold: d.hold}
	sub, err := subscribeDurable(d.nats, d.config(), deliverSubject, delivery.receive)
	if err != nil {
		return nil, err
	}
	delivery.sub = sub
	d.mu.Lock()
	d.delivering = delivery
	d.mu.Unlock()
	return sub, nil
}

// stopDelivering stops this intake's subscription so that another process can take the durable
// over with nothing waiting out the ack wait: it drains the subscription, so the server delivers
// it nothing more; negatively acknowledges every message the callback is handed from then on,
// those NATS had already delivered to the client's buffer included, so the server redelivers them
// at once; lets the message in its handler finish and be acknowledged; and returns once the drain
// has ended -- at the latest intakeDrainWait after that handler returned, when it unsubscribes
// instead -- having given up the subscription's registration on the client (bus.Client.Release),
// so that no reconnect binds it again and a later subscribe on the same client starts afresh. Run
// calls it when ctx ends and when the hold is lost. It is safe from any goroutine and idempotent: a
// second call returns once the first has stopped, and a call with nothing subscribed returns at
// once.
func (d *heldDurable) stopDelivering() {
	d.mu.Lock()
	delivery := d.delivering
	d.mu.Unlock()
	if delivery != nil {
		delivery.stop()
	}
}

// stop is stopDelivering for this subscription. The drain goes first, so the server has the
// subscription's interest gone before any negative acknowledgement reaches it, and redelivers
// what this one gives back to the next subscriber rather than straight back here.
func (d *intakeDelivery) stop() {
	d.stopped.Do(func() {
		d.stopping.Store(true)
		drained := true
		if err := d.sub.Drain(); err != nil {
			slog.Error("dispatch delivery: drain the intake's subscription", "error", err)
			drained = false
		}
		// The message in its handler, if any, finishes and is acknowledged; every one after it
		// finds stopping set.
		d.handling.Lock()
		d.handling.Unlock()
		// nats.go ends a drain on a goroutine of its own, once the callback has run every message
		// the client had buffered, and reports it only by the subscription turning invalid.
		deadline := time.Now().Add(intakeDrainWait)
		for drained && d.sub.IsValid() {
			if !time.Now().Before(deadline) {
				slog.Error("dispatch delivery: the intake's subscription did not finish draining; unsubscribing it", "waited", intakeDrainWait)
				if err := d.sub.Unsubscribe(); err != nil {
					slog.Error("dispatch delivery: unsubscribe the intake's subscription", "error", err)
				}
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		// The client binds again a registration whose subscription is no longer valid, a drained
		// one included, so without this the next reconnect would bind this stopped callback, which
		// negatively acknowledges every message it is handed, straight back to the durable.
		d.nats.Release(d.sub)
	})
}

// intakeDrainWait bounds how long stop waits for the drain to end once the message in its handler
// has finished. By then all that is left is nats.go's: the callback negatively acknowledging what
// the client had buffered, which never blocks, and the drain's flush to the server. So the bound is
// the one nats.go itself gives a connection's drain, natsgo.DefaultDrainTimeout -- bus connections
// never set their own -- rather than intakeMessageTimeout, which bounds a handler, and the handler
// is done.
const intakeDrainWait = natsgo.DefaultDrainTimeout

// receive is the subscription's callback. A message once stopping is set, or once the hold's loss
// deadline has passed, is negatively acknowledged: this intake may no longer act on the durable's
// messages, and the server redelivers them to the next holder. Any other goes to the handler.
func (d *intakeDelivery) receive(msg *natsgo.Msg) {
	d.handling.Lock()
	defer d.handling.Unlock()
	if d.stopping.Load() || (d.hold != nil && !time.Now().Before(d.hold.Deadline())) {
		negativelyAcknowledge(msg)
		return
	}
	d.handle(msg)
}
