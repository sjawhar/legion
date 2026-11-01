// The listener's kept lane for role messages: the copy it stores when a role message has no live
// holder to take it (bus.KeptRoleSubject), and the drain that hands those copies to the role's
// next holder when the role is claimed (bus.KeptRoleCursorSubject). Both subjects are the
// listener's own: no session may register a topic under them, and no consumer of the notification
// stream delivers one (bus.StreamSubjects carries no kept prefix), so a kept copy never re-enters
// the delivery path it was taken out of.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// roleKeepWindow is how old a kept copy may be and still be forwarded to the role's next holder.
// A role token names a tree role rather than one run, so the holder a later claim hands a kept
// message to can be a generation the sender never addressed: handing it a day-old control notice
// is the regression sjawhar/legion#753 removed. One hour bounds that, and a forwarded copy carries
// the time it was kept (contracts.Envelope.KeptAt) so its holder can judge the message's age
// itself.
const roleKeepWindow = time.Hour

// roleKeepDrainMax is how many kept copies one walk of a role's lane forwards. The claim answers
// its caller at once and the drain runs behind it, so one walk is bounded work rather than however
// much a role's kept lane has gathered; the handover goes on in a walk of its own while the role's
// holder is still live (drainKeptRole), since nothing else would start another drain for it.
const roleKeepDrainMax = 20

// roleKeepScanMax bounds the walk itself, where roleKeepDrainMax bounds the forwards it makes. A
// copy past roleKeepWindow, and one no read can decode, is passed over rather than forwarded, and
// a walk that counted those against its forward budget would walk twenty dead copies and hand
// the role's new holder nothing. The stream keeps a kept copy for 72 h while the keep window is
// one hour, so a role's lane can hold nothing but copies past the window: this is what still ends
// that walk, which the handover's next walk carries on from.
const roleKeepScanMax = 200

// roleKeepPublishWindow bounds the keep's own publish. The keep happens on a send that is already
// out of time or on a lane delivery that answers nobody, so it takes a context of its own rather
// than whatever is left of its caller's, and that context is short: a bus that does not store the
// copy promptly is a keep that failed, which its caller answers for.
const roleKeepPublishWindow = 2 * time.Second

// roleKeepDrainWindow bounds one walk of a role's kept lane, which runs off the claim, the keep or
// the sweep that started the handover. It covers roleKeepDrainMax forwards of roleReceiptTimeout
// each, and the stream read and the claim of each of the roleKeepScanMax copies the walk may read
// between them, so a walk whose holder answers no receipt still ends rather than outliving the
// claim it serves. A handover's pass takes it for the lane-head read it opens with
// (handOverKeptRole, readKeptLaneHead), the one read of that lane a pass makes, which it hands
// to the walk it starts (walkKeptRole).
const roleKeepDrainWindow = 60 * time.Second

// roleKeepClaimWindow is how long one walk's claim of a kept copy holds that role's lane against
// the walks of the other listeners of the deployment (roleKeepCursor.Claiming). It is
// roleKeepDrainWindow because that is the whole of a walk's life: a claim younger than it may
// still belong to a walk that is forwarding that copy right now, which another must not forward a
// second time, and a claim older than it belongs to a walk whose context has ended, so the copy it
// claimed is free for the next one to forward. A walk that ends on a forward it could not make
// does not wait this out: it releases its claim (roleKeepLaneCursor.release).
//
// The age is one listener's clock against the time the stream stored the cursor, which is the
// server's: the two sides of the comparison are read on different clocks, and this window assumes
// they are within a minute of each other, which a deployment whose hosts run NTP holds to by orders
// of magnitude. Measuring both ends on one clock would cost a round trip per walk to ask the
// stream its time, and would buy nothing a listener an hour out of step could not break anyway:
// such a listener also drops every copy as past roleKeepWindow. What a clock that runs ahead costs
// is bounded - it takes a live claim over and the copy reaches two holders once, which is the
// duplicate the design already accepts for a drain killed between its forward and its confirm -
// so the assumption is stated here rather than paid for on every walk.
const roleKeepClaimWindow = roleKeepDrainWindow

// roleKeepDrainRetryPause is how long a handover waits before its next walk when the one before it
// failed: a read, a cursor write or a forward that did not answer. It is short because the copies
// behind it have senders already answered 202 kept and a holder waiting, and it is not zero
// because a bus that is failing must not be walked again at once. It is the first of the pauses a
// run of failures earns, which double from here to roleKeepDrainRetryCap (roleKeepDrainBackoff).
const roleKeepDrainRetryPause = time.Second

// roleKeepDrainRetryCap is the widest pause a run of failed walks earns (roleKeepDrainBackoff). A
// handover whose bus is not answering is worth retrying for as long as the role's holder is live -
// the copies it holds are messages whose senders were answered 202 kept - and the cost of doing so
// must fall to about one walk a half-minute rather than one a second. roleKeepHandoverWindow is
// what ends such a handover.
const roleKeepDrainRetryCap = 30 * time.Second

// roleKeepDrainIdleRounds is how many walks that made no progress one handover takes before it
// gives the role's lane up: the third such walk is its last. A walk made progress when it moved
// the role's cursor - it forwarded a copy, or dropped or passed one over - because the copies
// behind it are then nearer the role's holder, however long the lane is; a walk that moved nothing
// found the lane held by another listener's claim or the role moved to a holder this listener
// cannot read, and repeating that without end would be a drain that never stops. A walk that
// failed counts no round here: the bus it could not reach is waited out on a widening cadence
// instead (roleKeepDrainBackoff), since giving a lane up for a bus that is unwell would strand
// copies a retry would deliver. Giving up costs the lane the wait for the role's next claim, keep
// or sweep (sweepKeptRoleLanes).
const roleKeepDrainIdleRounds = 3

// roleKeepHandoverWindow bounds one handover of a role's kept lane: every walk one claim, one keep
// or one sweep makes, the pauses between them included (drainKeptRole). Each walk is bounded by
// roleKeepDrainWindow, the walks that make no progress by roleKeepDrainIdleRounds and the walks
// that failed by their widening pauses, so this is the last bound rather than the usual one - a
// handover ends when the lane is drained or the role has no live holder - and it is wide enough
// for the walks a lane of thousands of copies takes.
const roleKeepHandoverWindow = 10 * time.Minute

// roleKeepHolderWindow bounds a handover's resolution of the role's holder: the claim read, the
// holder's session read, and at most one release of a lapsed claim. Every pass that resolves at
// all resolves within it, which is every pass whose lane holds copies to hand over
// (handOverKeptRole). It answers no caller - the keep's own answer is written by
// then - so it ends rather than waiting out a bus that is not answering, and a resolution that did
// not answer costs only this copy's wait for the role's next claim.
const roleKeepHolderWindow = 5 * time.Second

// roleKeptDeliveryStatus is the delivery_status of a role message kept instead of delivered: the
// label the role lane counts under messagesDelivered and logs, and the one the publish route's own
// kept answer reads as (sendKept.deliveryStatus), so route and lane say the same word for it.
const roleKeptDeliveryStatus = "kept"

// roleKeepFailedLine is logged by both give-up paths - the publish route's and the role lane's -
// when the copy could not be kept, beside the answer each gives when nothing was kept.
const roleKeepFailedLine = "listener kept role message publish failed"

// roleKeepCursor is the body of a role's drain cursor: the two numbers that say how far that
// role's kept lane has been drained. Kept is the stream sequence on the role's kept subject the
// lane is finished with - every copy up to it was forwarded, dropped as expired or passed over -
// and the walk starts at the copy after it. Claiming is a copy one walk has claimed and not yet
// confirmed delivered, 0 when none is outstanding: a walk writes it before it forwards that
// copy and clears it with the same write that moves Kept past the copy, so a walk that ends
// between the two leaves Kept short of the copy and the next walk reads that copy again. Until
// it is older than roleKeepClaimWindow, measured against the time the stream stored the cursor
// carrying it, a claim is also the lane's lock: the walk that reads it leaves the lane alone,
// so the copy is forwarded a second time only once the walk that claimed it can no longer be
// forwarding it. It is the listener's own small body rather than an envelope, published with
// bus.Client.PublishCursorAt.
type roleKeepCursor struct {
	Kept     uint64 `json:"kept"`
	Claiming uint64 `json:"claiming"`
}

// roleKeepLaneCursor is one walk's hold on a role's drain cursor: everything each write of it
// needs - the role, the holder the walk forwards to, the cursor's subject - and the sequence the
// next compare-and-swap must name, which every write keeps itself. A walk writes the cursor three
// times for a copy it forwards and once for a copy it passes over, and each of those asks the
// same question of the answer, which write and release answer once here.
type roleKeepLaneCursor struct {
	cfg     listenerDeliveryHandlerConfig
	role    string
	holder  string
	subject string
	// seq is the stream sequence of the cursor message this hold last saw, 0 for a role whose
	// lane has never been drained, which the next write names as its compare-and-swap.
	seq uint64
}

// write writes cursor as the role's drain cursor under the compare-and-swap of the sequence this
// hold last saw (bus.Client.PublishCursorAt, 0 for a role whose lane has never been drained), and
// keeps the sequence it was stored at for the next write. A write the stream refused because
// another listener's walk took the cursor comes back wrapping bus.ErrCursorMoved: that listener
// owns this role's lane now, which is nothing wrong, so it is logged at debug here and ends the
// walk as roleKeepLaneBusy where the write is read (roleKeepWriteFailed). Every other failure is
// the error it is, and neither moves the sequence this hold names next.
func (c *roleKeepLaneCursor) write(ctx context.Context, cursor roleKeepCursor) error {
	body, err := json.Marshal(cursor)
	if err != nil {
		return fmt.Errorf("encode role %s's kept-lane cursor at %d: %w", c.role, cursor.Kept, err)
	}
	next, err := c.cfg.client.PublishCursorAt(ctx, c.subject, body, c.seq)
	if err != nil {
		if errors.Is(err, bus.ErrCursorMoved) {
			c.cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain lost the cursor to another listener",
				slog.String("role", c.role),
				slog.String("session_id", c.holder))
			return err
		}
		return fmt.Errorf("advance role %s's kept-lane cursor to kept %d claiming %d: %w", c.role, cursor.Kept, cursor.Claiming, err)
	}
	c.seq = next
	return nil
}

// release gives up the claim this walk holds on one copy, so the role's next walk reads that copy
// again at once: it writes the cursor the claim replaced, kept where the lane was finished and
// nothing claimed. It is the end of a walk that claimed a copy it will not forward after all, and
// answers nobody: a release the cursor refused is a claim another walk has taken over already,
// and a release that failed leaves a claim the next walk takes over once roleKeepClaimWindow has
// passed, so both are logged at debug and neither changes what the walk reports.
func (c *roleKeepLaneCursor) release(ctx context.Context, kept uint64) {
	if err := c.write(ctx, roleKeepCursor{Kept: kept}); err != nil {
		c.cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain could not release its claim",
			slog.String("role", c.role),
			slog.String("session_id", c.holder),
			slog.String("error", err.Error()))
	}
}

// roleKeepWriteFailed is what a walk ends with when a cursor write failed, which every write of
// the cursor asks the same way: a write the stream refused because the cursor moved
// (bus.ErrCursorMoved) leaves the lane to the listener whose walk took it, which is nothing wrong
// and ends this walk without an error, and every other failure ends it with the error it is.
func roleKeepWriteFailed(err error) (roleKeepDrainEnd, error) {
	if errors.Is(err, bus.ErrCursorMoved) {
		return roleKeepLaneBusy, nil
	}
	return roleKeepDrainFailed, err
}

// roleKeepGuards holds one guard per role, so two handovers of one role inside this listener do
// not walk its kept lane together and read the same copy twice. A role's guard is kept for the
// set's life, which costs one channel per role the listener it belongs to has seen claimed. One set
// is built per process (main.go), as listenerDeliveryHandlerConfig.roleKeepDrains.
type roleKeepGuards struct {
	guards sync.Map
}

func newRoleKeepGuards() *roleKeepGuards {
	return &roleKeepGuards{}
}

// guard is role's own guard, made on the first handover of that role: a channel of one, which the
// handover running on this listener holds a token in.
func (g *roleKeepGuards) guard(role string) chan struct{} {
	if guard, ok := g.guards.Load(role); ok {
		return guard.(chan struct{})
	}
	guard, _ := g.guards.LoadOrStore(role, make(chan struct{}, 1))
	return guard.(chan struct{})
}

// hold takes role's guard for the whole of one handover and returns the release of it, or reports
// false when ctx ended before the guard was free. It waits for the handover of that role running
// on this listener, which is what a claim's start and a keep's do: the handover ahead of them
// drains the lane, so the one behind it opens on a lane with nothing left and ends on its two
// stream reads, with no holder resolved at all. The wait ends with ctx - the handover's own
// window (roleKeepHandoverWindow) - because a handover whose window has run out walks nothing
// whatever it finds: waiting past it would queue one goroutine per keep behind a handover that is
// spending that window on a bus which is not answering.
func (g *roleKeepGuards) hold(ctx context.Context, role string) (func(), bool) {
	guard := g.guard(role)
	select {
	case guard <- struct{}{}:
		return func() { <-guard }, true
	case <-ctx.Done():
		return nil, false
	}
}

// tryHold is hold for a caller that will not wait: it reports false when a handover of role is
// already running on this listener. It is the sweep's test (sweepKeptRoleLanes), which leaves a
// lane that is being handed over to the handover holding it rather than starting a second one or
// blocking its own pass behind it.
func (g *roleKeepGuards) tryHold(role string) (func(), bool) {
	guard := g.guard(role)
	select {
	case guard <- struct{}{}:
		return func() { <-guard }, true
	default:
		return nil, false
	}
}

// roleKeepable reports whether a resolution that found no live holder for a role is one the
// listener keeps a copy for: a role nobody claims, and a lapsed claim the release dropped or
// found already absent. A lapsed claim the listener retained is the restart grace
// (store.ExpiredRoleClaimRetained): that holder keeps its claim and re-registers inside the
// session TTL without writing a new one, so no POST /v1/roles/set and no drain follow it, and a
// copy kept for it would wait for a claim that never comes. Both give-up paths read it - the
// publish route's noHolder and the role lane's resolveCoreRoleHolder - so a retained claim keeps
// nothing and is answered as it was before the kept lane: the route's 404 holder_lapsed, the
// lane's delivery_failed. The publish route's own attempts already read a retained claim that way,
// as an attempt that failed rather than a give-up (sendToRole).
func roleKeepable(result roleHolderResult) bool {
	switch result.state {
	case roleHolderUnclaimed:
		return true
	case roleHolderLapsed:
		return result.claimRelease != store.ExpiredRoleClaimRetained
	default:
		return false
	}
}

// keepRoleMessage stores item on role's kept lane, the copy the role's next holder is handed when
// it claims the role (drainKeptRole), and reports whether it was kept; a keep that failed is
// logged here, the one failure line both give-up paths write (the publish route's noHolder and
// the role lane's keepRoleLaneMessage), so neither carries a copy of it.
//
// It publishes under bus.KeptMsgID, the one id every listener that keeps this message writes it
// under: each takes the same raw role publish on its own role-lane subscription, so several may
// find the role undeliverable and keep it, and the stream's duplicate window collapses their
// copies into the single stored one. A duplicate is therefore success, not a fault, and is not
// reported. The envelope is stored as it is, so its topic stays the role topic it was published
// on and the next holder is handed the message as its sender sent it.
func keepRoleMessage(client *bus.Client, logger *logging.Logger, role string, item contracts.Envelope) bool {
	ctx, cancel := context.WithTimeout(context.Background(), roleKeepPublishWindow)
	defer cancel()
	if err := client.PublishStoredAs(ctx, bus.KeptRoleSubject(role), item, bus.KeptMsgID(item.DedupeKey, role)); err != nil {
		logger.Error(roleKeepFailedLine,
			slog.String("role", role),
			slog.String("event_id", item.EventID),
			slog.String("error", fmt.Errorf("keep the role %s message on its kept lane: %w", role, err).Error()))
		return false
	}
	return true
}

// keepForNextHolder keeps item for role's next holder and hands the lane to whoever holds the role
// now (runKeptRoleDrain), and reports whether anything was kept. It is the one keep both give-up
// paths take - the publish route's (roleSend.noHolder) and the role lane's (keepRoleLaneMessage) -
// so no give-up path can store a copy without offering it to the role's current holder, which is
// the claim this copy waits for having landed while the keep's caller was resolving the holder it
// did not find. The handover resolves that holder itself and ends with no walk when the role has
// none, so a keep for a role nobody holds costs one resolution.
//
// One message is not kept at all, and leaves its caller's give-up exactly as it was before the
// kept lane: one whose role token no claim can name (isValidRole, the pattern POST /v1/roles/set
// refuses), which both give-up paths must agree on, since a copy kept under such a token waits
// for a claim that cannot be made and expires unread.
//
// A sender that keeps a queue of its own and redelivers from it - the Legion daemon's notice
// queue and a phase worker's stall reminder - opts out on the publish route's own request field
// instead (keep false, read by roleSend.noHolder), which is the one give-up a sender can
// address: nothing publishes a raw role message it would redeliver, so the role lane always
// keeps.
func keepForNextHolder(cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope) bool {
	if !isValidRole(role) {
		return false
	}
	if !keepRoleMessage(cfg.client, cfg.logger, role, item) {
		return false
	}
	go runKeptRoleDrain(cfg, role)
	return true
}

// keepRoleLaneMessage keeps the role lane's copy of a message it found no live holder for, and
// reports whether it was kept. Kept, it writes the lane's line and counts the delivery under
// roleKeptDeliveryStatus and publishes no delivery exception: the message is not lost, the role's
// next holder is handed it. A keep that kept nothing is reported false, and the caller's own
// give-up - its exception, its metric and its line - stands exactly as it was.
func keepRoleLaneMessage(cfg listenerDeliveryHandlerConfig, role, holder string, item contracts.Envelope, state roleHolderState) bool {
	if !keepForNextHolder(cfg, role, item) {
		return false
	}
	applyDeliveryOutcome(cfg, item, deliveryOutcome{
		sessionID:    holder,
		metricStatus: roleKeptDeliveryStatus,
		log: func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelInfo, "listener kept role message for the role's next holder",
				holder, item.Topic, item.EventID, roleKeptDeliveryStatus,
				slog.String("role", role),
				slog.String("reason", string(state)))
		},
	})
	return true
}

// runKeptRoleDrain hands role's kept lane over to whoever holds the role, under a context of its
// own (roleKeepHandoverWindow), and logs a handover that failed. It is what the two starts of a
// handover run in a goroutine of their own - the claim's (roleSetHandler) and the keep's
// (keepForNextHolder) - since neither the claim's answer nor the keep's caller waits for a stream
// read or a forward. The sweep starts its handovers itself, under the guard it already took
// (sweepKeptRoleLanes).
func runKeptRoleDrain(cfg listenerDeliveryHandlerConfig, role string) {
	ctx, cancel := context.WithTimeout(context.Background(), roleKeepHandoverWindow)
	defer cancel()
	reportKeptRoleDrain(cfg, role, drainKeptRole(ctx, cfg, role, time.Now))
}

// reportKeptRoleDrain logs a handover that failed: the one line every start of a handover writes,
// the claim's and the keep's (runKeptRoleDrain) and the sweep's (sweepKeptRoleLanes).
func reportKeptRoleDrain(cfg listenerDeliveryHandlerConfig, role string, err error) {
	if err == nil {
		return
	}
	cfg.logger.Error("listener kept role drain failed",
		slog.String("role", role),
		slog.String("error", err.Error()))
}

// roleKeepDrainEnd is what ended one walk of a role's kept lane (walkKeptRole), which decides
// whether the handover walks it again and after how long (the switch in handOverKeptRole).
type roleKeepDrainEnd int

const (
	// roleKeepLaneDrained: every copy the lane held was forwarded, dropped or passed over. The
	// handover is finished.
	roleKeepLaneDrained roleKeepDrainEnd = iota
	// roleKeepWorkLeft: the walk's own bounds ended it (roleKeepDrainMax forwards, or
	// roleKeepScanMax copies read), with copies behind them.
	roleKeepWorkLeft
	// roleKeepLaneBusy: another listener's walk has the lane - its claim of a copy is younger
	// than roleKeepClaimWindow, or it took the cursor from under this one.
	roleKeepLaneBusy
	// roleKeepHolderMoved: the role's holder is no longer the session this walk forwards to.
	roleKeepHolderMoved
	// roleKeepDrainFailed: a read, a cursor write or a forward failed, and the walk returned the
	// error with it.
	roleKeepDrainFailed
)

// roleKeepDrainResult is what one walk of a role's kept lane did: what ended it, how many copies
// it forwarded, and how many it moved the role's cursor past without forwarding - one past
// roleKeepWindow, one no read can decode, one beforeForward refused. Either number above zero is
// the progress the handover's bound on walks that made none counts (roleKeepDrainIdleRounds),
// because both leave the copies behind them nearer the role's holder.
type roleKeepDrainResult struct {
	end       roleKeepDrainEnd
	forwarded int
	passed    int
}

// roleKeepDrainBackoff is the pause a handover waits after a walk that failed, or after a read it
// is retrying - the role's kept lane head, or the role's holder behind one:
// roleKeepDrainRetryPause for the first of a run, doubling to roleKeepDrainRetryCap, where last
// is the pause this handover waited before the failure it has just met and 0 is a handover that
// has not failed yet or has made progress since. A run of failures therefore costs the bus less
// the longer it lasts, and ends with roleKeepHandoverWindow rather than with a budget of walks.
func roleKeepDrainBackoff(last time.Duration) time.Duration {
	if last <= 0 {
		return roleKeepDrainRetryPause
	}
	return min(2*last, roleKeepDrainRetryCap)
}

// drainKeptRole hands role's kept lane to the session that holds the role. It is the whole of a
// handover under role's own guard: runKeptRoleDrain is the goroutine the claim route and the keep
// run it in, and tests call it directly for one role's lane.
//
// The guard is held for the handover rather than for each of its walks, so two handovers of one
// role inside this listener run one after the other and the sweep can tell that a role's lane is
// being handed over here already without waiting for it (roleKeepGuards.tryHold). A handover whose
// context ended while it waited for the guard gives that wait up and walks nothing: the handover
// holding the guard is draining that lane, and this one's own window has run out.
func drainKeptRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, now func() time.Time) error {
	release, taken := cfg.roleKeepDrains.hold(ctx, role)
	if !taken {
		return nil
	}
	defer release()
	return handOverKeptRole(ctx, cfg, role, now)
}

// handOverKeptRole is the handover itself, for a caller holding role's guard (drainKeptRole,
// sweepKeptRoleLanes).
//
// Each pass opens by reading the role's kept lane and its drain cursor (readKeptLaneHead) within
// roleKeepDrainWindow, resolves the role's holder by role delivery's own rule (resolveRoleHolder
// with laneHolderFresh) within roleKeepHolderWindow only when that lane holds copies its cursor
// has not reached, and walks the lane for the holder it found, handing that walk the head it
// read (walkKeptRole). The lane is read before the holder, so a pass that finds the lane empty
// ends the handover with no resolution at all: every claim of a role starts a handover and most
// roles have nothing kept for them, which would otherwise cost each of those claims a claim read
// and a session read, made from a goroutine its own answer has already outlived. The resolution
// is taken after the pause the walk before it earned rather than before it: a holder resolved
// before that pause can have released the role while it ran, and the walk would then have spent
// its budget on a session that no longer holds the role, which the walk's own check cannot
// catch (roleKeepDrainHolds reads an unclaimed role as no evidence). A resolution that finds no
// live holder ends the handover there, which is what the keep that stored a copy for a role
// nobody holds wants: a role nobody claims comes back roleHolderUnclaimed with no error at all.
//
// One walk of the lane is bounded - roleKeepDrainMax forwards, roleKeepScanMax copies read,
// roleKeepDrainWindow of time - and nothing but a claim, a keep or a sweep ever starts a handover.
// A keep happens only while a role has no live holder and a live holder claims no second time, so
// every bound a walk stopped on used to strand the copies behind it on the lane until they aged
// out unread, with every one of their senders answered 202 kept. The handover walks the lane again
// instead, for the role's holder as it is then, until the lane is drained, the role has no live
// holder, or the walks stop making progress (roleKeepDrainIdleRounds). What ended the walk says
// how long it waits first: a drained lane ends the handover; a walk the budget or the scan ended
// is followed at once, since the copies behind it have senders answered 202 kept; one that found
// the lane busy waits the claim out, since the walk holding it can still be forwarding; and one
// whose holder moved is followed at once, for the holder the next pass resolves. A walk that
// failed is not one that made no progress: it is waited out on a widening cadence
// (roleKeepDrainBackoff) for as long as ctx lives.
//
// A read that did not answer - the lane head's, on whichever pass it was made, or the holder's
// resolution behind one - says nothing about the holder, so what decides whether the handover
// reads again is the role's cached claim (retry, store.Registry.CachedRoleHolder, the read the
// sweep makes). A claim naming a session is a lane whose copies have a holder waiting: the read
// is retried on the widening cadence above and the failure is carried out of the handover, which
// is what lifts one that ends still failing above debug. A role no claim names - and a claim read
// that failed too, which says no holder is waiting either - is the keep's own case, a copy kept
// for a role nobody holds: the handover ends at debug, leaving the lane to the role's next claim.
//
// It returns the error of its last walk, or, while no walk has failed, that of the read it was
// still retrying when its time ran out, so either is logged where every failed handover is
// (reportKeptRoleDrain): a pass that reads again drops the read before it, which it no longer
// retries. ctx - roleKeepHandoverWindow in the listener - bounds the whole handover, pauses
// included.
//
// now is the clock a copy's age is measured against, time.Now in the listener: a test moves it
// rather than keeping a copy for an hour, since the age is taken from the stream's own store time,
// which no caller may choose. An outstanding claim's age is measured the same way, against the
// time the stream stored the cursor that carries it.
func handOverKeptRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role string, now func() time.Time) error {
	var (
		err, readErr   error
		idle           int
		backoff, pause time.Duration
	)
	// ended is what this handover reports at every exit: the error of its last walk, or, while no
	// walk has failed, the read it was still retrying, which is what lifts it above its debug line.
	ended := func() error {
		if err != nil {
			return err
		}
		return readErr
	}
	// waitOut earns the next of this handover's widening pauses, which a walk that failed and a
	// read it retries both wait (roleKeepDrainBackoff).
	waitOut := func() {
		backoff = roleKeepDrainBackoff(backoff)
		pause = backoff
	}
	// retry is what a pass does with a read that did not answer: it writes line for that read and
	// reports whether this handover reads again, which the role's cached claim decides. A claim
	// naming a session earns the next pause and carries the failure out of the handover (ended); a
	// role no claim names, and a claim read that failed too, end it here with nothing above debug.
	retry := func(line string, cause error) bool {
		cfg.logger.LogEvent(slog.LevelDebug, line,
			slog.String("role", role),
			slog.String("error", cause.Error()))
		claimed, claimErr := cfg.registry.CachedRoleHolder(role)
		if claimErr == nil && claimed != "" {
			readErr = cause
			waitOut()
			return true
		}
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain left the lane to the role's next claim",
			slog.String("role", role),
			slog.Bool("claim_read_failed", claimErr != nil))
		return false
	}
	for {
		if ctx.Err() != nil {
			return ended()
		}
		if pause > 0 && waitUnlessDone(ctx, pause) != nil {
			return ended()
		}
		// Past that pause, the read this pass makes is the one it reports: the read before it is
		// behind this handover, whether it answered or not.
		readErr = nil
		headCtx, cancelHead := context.WithTimeout(ctx, roleKeepDrainWindow)
		head, headErr := readKeptLaneHead(headCtx, cfg.client, role)
		cancelHead()
		if headErr != nil {
			if !retry("listener kept role message could not read the role's kept lane", headErr) {
				return ended()
			}
			continue
		}
		if !head.left {
			return ended()
		}
		holderCtx, cancelHolder := context.WithTimeout(ctx, roleKeepHolderWindow)
		resolved, holderErr := resolveRoleHolder(holderCtx, cfg.registry, cfg.sessions, role, laneHolderFresh)
		cancelHolder()
		if holderErr != nil {
			if !retry("listener kept role message could not resolve the role's holder", holderErr) {
				return ended()
			}
			continue
		}
		if resolved.state != roleHolderLive {
			return ended()
		}
		holder := resolved.holder
		var result roleKeepDrainResult
		result, err = walkKeptRole(ctx, cfg, role, holder, resolved.entry, head, now)
		progressed := result.forwarded > 0 || result.passed > 0
		if progressed {
			idle, backoff = 0, 0
		}
		switch result.end {
		case roleKeepLaneDrained:
			return ended()
		case roleKeepDrainFailed:
			waitOut()
			continue
		case roleKeepLaneBusy:
			pause = roleKeepClaimWindow
		case roleKeepWorkLeft, roleKeepHolderMoved:
			pause = 0
		}
		if progressed {
			continue
		}
		if idle++; idle >= roleKeepDrainIdleRounds {
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain gave the lane up with copies left on it",
				slog.String("role", role),
				slog.String("session_id", holder),
				slog.Int("idle_walks", idle))
			return ended()
		}
	}
}

// startKeptRoleSweep is the backstop under which no kept lane waits for a claim that will not
// come. A handover runs in a goroutine of this process, so a listener restarted or replaced
// mid-handover ends it, and nothing starts another: a keep happens only while a role has no live
// holder, and a live holder claims no second time. Every interval, and once as the listener
// starts, this hands over every lane left that way (sweepKeptRoleLanes).
//
// It runs under the context the listener's other KV loops run under and returns a channel closed
// once its goroutine has returned, as they do (store.Registry.StartReaper), so the shutdown ends
// it and waits for a pass in flight.
func startKeptRoleSweep(ctx context.Context, cfg listenerDeliveryHandlerConfig, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			sweepKeptRoleLanes(ctx, cfg)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// sweepKeptRoleLanes hands over every role the stream holds a kept copy on and a session claims.
// It reads every kept role subject the stream holds a copy on in one call
// (bus.Client.KeptRoleLanes), so a deployment where no role has a kept copy pays one round trip a
// pass and no per-role work at all; a role that has one costs the read of its claim, and a
// handover of its own when that claim names a session.
//
// A lane whose role no session claims is passed over rather than handed over, with nothing logged
// for it: nothing moves such a lane's cursor - the handover it would start resolves no live
// holder and walks nothing - so the role stays on the stream's list of kept lanes for as long as
// it holds those copies, the stream's 72 h and not roleKeepWindow's hour, and a line and a
// handover for it would cost a goroutine and a resolution every pass. The claim is read from the
// registry's watched copy of the role bucket (store.Registry.CachedRoleHolder): a role a session
// claims is in that view and costs no round trip, and an unclaimed one is not - the view drops a
// role as its claim is released - so it costs the one read of the role bucket that miss makes.
// The claim says only that a session holds the role: whether that session is live is the
// handover's own resolution to make.
//
// The lane itself is left to the handover to read, which is the read its first pass opens with
// (readKeptLaneHead): a lane its cursor is finished with ends that pass there, with no holder
// resolved and nothing walked, where a read here would cost every swept lane that read twice.
//
// A role whose handover is already running on this listener is left to it: the sweep takes that
// role's guard or passes the role over, and it never waits for one (roleKeepGuards.tryHold), so
// one slow handover holds up no other role's sweep. The handover the sweep starts runs under the
// guard the sweep took, in a goroutine of its own, so a pass starts the handovers of a hundred
// lanes without walking any of them itself.
func sweepKeptRoleLanes(ctx context.Context, cfg listenerDeliveryHandlerConfig) {
	roles, err := cfg.client.KeptRoleLanes(ctx)
	if err != nil {
		if ctx.Err() == nil {
			cfg.logger.LogEvent(slog.LevelWarn, "listener kept role sweep could not read the kept lanes",
				slog.String("error", err.Error()))
		}
		return
	}
	for _, role := range roles {
		if ctx.Err() != nil {
			return
		}
		holder, err := cfg.registry.CachedRoleHolder(role)
		if err != nil {
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep could not read a role's claim",
				slog.String("role", role),
				slog.String("error", err.Error()))
			continue
		}
		if holder == "" {
			continue
		}
		release, taken := cfg.roleKeepDrains.tryHold(role)
		if !taken {
			continue
		}
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role sweep is handing over a claimed role's kept lane",
			slog.String("role", role),
			slog.String("session_id", holder))
		go func() {
			defer release()
			handoverCtx, cancel := context.WithTimeout(ctx, roleKeepHandoverWindow)
			defer cancel()
			reportKeptRoleDrain(cfg, role, handOverKeptRole(handoverCtx, cfg, role, time.Now))
		}()
	}
}

// keptLaneHead is the opening read of a role's kept lane: what the stream holds on it, where the
// role's drain cursor stands, and whether there is anything left for a walk to do
// (readKeptLaneHead).
type keptLaneHead struct {
	// last is the stream sequence of the newest copy on the lane, 0 when the stream holds none.
	last uint64
	// cursorSeq is the stream sequence of the cursor message itself, which the walk's next
	// compare-and-swap write of the cursor must name (roleKeepLaneCursor.write); 0 for a lane
	// whose cursor has never been written.
	cursorSeq uint64
	// cursor is the drain cursor's body, its zero value for a lane that has never been drained.
	cursor roleKeepCursor
	// written is the time the stream stored the cursor, which is how old the claim it carries
	// is; the zero time when there is no cursor, which no claim is judged against since the body
	// claims nothing.
	written time.Time
	// left reports copies the cursor has not reached: the one test of whether a walk of this
	// lane has anything to do.
	left bool
}

// readKeptLaneHead reads role's kept lane and its drain cursor: the one pair of reads a pass of
// the handover makes of that lane, which answers "anything left on it?" and carries the cursor
// to the walk the pass starts (handOverKeptRole, walkKeptRole).
//
// The lane is read first, and a role nothing was ever kept for ends here, on one stream read
// rather than two (nats.ErrMsgNotFound): the cursor says nothing a lane with no message can act
// on. A lane the cursor has already been advanced past ends on the second read, as a
// cursor-first read would. A role whose lane has never been drained holds no cursor: the
// sequence its next write names is 0, the body is its zero value and the time is the zero time,
// which no claim is judged against since the body claims nothing.
func readKeptLaneHead(ctx context.Context, client *bus.Client, role string) (keptLaneHead, error) {
	last, err := client.LastStoredMessage(ctx, bus.KeptRoleSubject(role))
	if errors.Is(err, nats.ErrMsgNotFound) {
		return keptLaneHead{}, nil
	}
	if err != nil {
		return keptLaneHead{}, fmt.Errorf("read role %s's kept lane: %w", role, err)
	}
	head := keptLaneHead{last: last.Sequence}
	stored, err := client.LastStoredMessage(ctx, bus.KeptRoleCursorSubject(role))
	if err != nil && !errors.Is(err, nats.ErrMsgNotFound) {
		return keptLaneHead{}, fmt.Errorf("read role %s's kept-lane cursor: %w", role, err)
	}
	if err == nil {
		if err := json.Unmarshal(stored.Data, &head.cursor); err != nil {
			return keptLaneHead{}, fmt.Errorf("read role %s's kept-lane cursor stored at %d: %w", role, stored.Sequence, err)
		}
		head.cursorSeq = stored.Sequence
		head.written = stored.Time
	}
	head.left = head.cursor.Kept < head.last
	return head, nil
}

// walkKeptRole is one walk of role's kept lane for holder. It walks the copies stored after the
// cursor of head, the lane-head read the pass that started it made (handOverKeptRole,
// readKeptLaneHead) - at most roleKeepScanMax of them, and until roleKeepDrainMax have been
// forwarded - forwarding each copy that is still inside roleKeepWindow and that beforeForward
// allows to the holder's agent subject exactly as a live role send forwards it
// (roleForwardDedupePrefix, contracts.AgentSubject), and reports what ended it and how far it
// moved the role's cursor, so the handover around it knows whether the lane has copies left and
// whether this walk brought them nearer (handOverKeptRole).
//
// It bounds itself at roleKeepDrainWindow, taken when the walk starts rather than when the
// handover around it was: the handover holds the role's guard, so no part of a walk's window is
// spent waiting for the walks of another handover of the same role, which would leave a claim of
// a copy behind for roleKeepClaimWindow by failing mid-walk.
//
// Every step the walk takes with a copy it writes the role's cursor for, under the
// compare-and-swap of the cursor's own last sequence (roleKeepLaneCursor.write), and that write is
// what decides which walk owns the copy: the loser of it stops, where a cursor written once at
// the end of the walk let two listeners forward everything they read.
//
// A copy nothing will be forwarded for - one no read can decode, one past roleKeepWindow, one
// beforeForward refuses - is finished in one write, to {kept: its sequence, claiming: 0}, before
// its line, its counter and its delivery outcome, so a walk that lost that write neither counts
// nor logs a copy another walk reports; it is why a copy past the window is counted expired once
// (envoy_role_kept_expired_total) however many walks read the lane.
//
// A copy that will be forwarded takes three: the cursor is written to {kept: unchanged, claiming:
// its sequence}, the copy is forwarded, and the cursor is written to {kept: its sequence,
// claiming: 0}. What the claim buys is that a copy still reaches exactly one holder: a walk that
// reads a claim younger than roleKeepClaimWindow leaves the lane to the walk holding it and
// forwards nothing, and a walk whose own claim the cursor refused stops where it is. What the
// unmoved kept buys is that no copy is lost whatever ends the walk - a process killed, a forward
// that failed, a context that ran out - because the walk starts at kept + 1, so a copy claimed
// and never confirmed is simply the next copy a walk reads: the next one of all, where the walk
// released its claim on its way out (roleKeepLaneCursor.release), and the next one past
// roleKeepClaimWindow where nothing was left to release it. What that costs is one more copy when
// a walk ends between its forward and its confirm: the repeat carries the same stable id
// (bus.ForwardMsgID of the forward's dedupe key and the agent subject), so for the same holder the
// stream's duplicate window keeps one stored copy and the holder's own dedupe_key drops the
// second, and only a holder that changed meanwhile sees the copy reach two holders, once, on a
// failure.
//
// Before each copy it reads the role's holder from the registry's watched copy of the role bucket,
// which costs no round trip (roleKeepDrainHolds), and stops when the role has moved to another
// session: this walk forwards to the session the pass that started it resolved and never reads its
// claim again, so without that a claimant a later write superseded, or an old holder whose walk
// stepped aside for a live claim, would spend its budget on a session that no longer holds the
// role. The lane is left to the new holder, whose copies the handover's next walk forwards.
//
// A forward that reached the server and drew no receipt (bus.ErrReceiptTimeout) counts as
// forwarded and the walk goes on, and that copy is confirmed: its copy is stored on the holder's
// agent subject, which a returning session replays. It is reported receipt_timeout all the same,
// which is what the live role lane reports for the same outcome (finishRoleForward), so one event
// is not counted delivered here and receipt_timeout there. Any other forward failure ends the walk
// and is returned, with its copy released for the role's next walk and the rest of the lane behind
// it. A cursor write the stream refused because another listener's walk has the lane
// (bus.ErrCursorMoved) is nothing wrong: that listener owns this role's lane now, and this walk
// stops.
func walkKeptRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, head keptLaneHead, now func() time.Time) (roleKeepDrainResult, error) {
	ctx, cancel := context.WithTimeout(ctx, roleKeepDrainWindow)
	defer cancel()

	// A copy another walk holds a claim of is that walk's to forward, so this one leaves the
	// lane to it rather than forwarding the copy a second time; the claim is read only here,
	// where it can be judged against the walk that wrote it, so now is asked for once per copy
	// in the walk below and once more only while a claim is outstanding.
	if head.cursor.Claiming != 0 && now().Sub(head.written) < roleKeepClaimWindow {
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain left the lane to the drain holding a claim",
			slog.String("role", role),
			slog.String("session_id", holder),
			slog.Uint64("claiming", head.cursor.Claiming))
		return roleKeepDrainResult{end: roleKeepLaneBusy}, nil
	}

	keptSubject := bus.KeptRoleSubject(role)
	lane := &roleKeepLaneCursor{cfg: cfg, role: role, holder: holder, subject: bus.KeptRoleCursorSubject(role), seq: head.cursorSeq}
	subject := contracts.AgentSubject(holder)
	storedOnly := roleForwardStoredOnly(entry)
	// The record the role lane judges a forward against, read the same way here: what the holder
	// already took, and what is already on its way to it.
	seen := func(key, recipient string) bool {
		return cfg.dedupeCache.Seen(key, recipient) || cfg.attemptCache.Seen(key, recipient)
	}
	result := roleKeepDrainResult{}
	kept := head.cursor.Kept
	for scanned := 0; ; scanned++ {
		if scanned >= roleKeepScanMax || result.forwarded >= roleKeepDrainMax {
			result.end = roleKeepWorkLeft
			return result, nil
		}
		if !roleKeepDrainHolds(cfg, role, holder) {
			cfg.logger.LogEvent(slog.LevelInfo, "listener kept role drain left the lane to the role's new holder",
				slog.String("role", role),
				slog.String("session_id", holder))
			result.end = roleKeepHolderMoved
			return result, nil
		}
		stored, err := cfg.client.NextStoredMessage(ctx, keptSubject, kept+1)
		if errors.Is(err, nats.ErrMsgNotFound) {
			result.end = roleKeepLaneDrained
			return result, nil
		}
		if err != nil {
			result.end = roleKeepDrainFailed
			return result, fmt.Errorf("read role %s's kept lane after %d: %w", role, kept, err)
		}
		item, report, forwardable := judgeKeptRoleCopy(cfg, role, holder, entry, stored, now, seen)
		if !forwardable {
			if err := lane.write(ctx, roleKeepCursor{Kept: stored.Sequence}); err != nil {
				result.end, err = roleKeepWriteFailed(err)
				return result, err
			}
			kept = stored.Sequence
			result.passed++
			report()
			continue
		}
		if err := lane.write(ctx, roleKeepCursor{Kept: kept, Claiming: stored.Sequence}); err != nil {
			result.end, err = roleKeepWriteFailed(err)
			return result, err
		}
		forward := item
		forward.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
		keptAt := stored.Time.UnixMilli()
		forward.KeptAt = &keptAt
		// The copy is on its way to this holder, which the record the lane's own forward keeps
		// says the same way (roleTopicDelivery), so a role message under this key is not sent to
		// it twice while this forward is outstanding. A forward that failed clears it again, as
		// the lane's failure does (finishRoleForward), since the copy stays on the lane and the
		// next walk must offer it rather than pass it over as one the holder already took.
		cfg.attemptCache.Record(item.DedupeKey, holder)
		forwarded := cfg.forwardAndWait(subject, forward, roleReceiptTimeout, storedOnly)
		if forwarded != nil && !errors.Is(forwarded, bus.ErrReceiptTimeout) {
			// This walk is ending with the copy claimed and unforwarded, so it releases the
			// claim: the cursor goes back to what it was before the claim, and the role's next
			// walk reads this copy again at once rather than waiting the claim out. A release
			// that cannot be written leaves the claim, which the next walk takes over once it
			// is older than roleKeepClaimWindow; either way the copy is not lost.
			cfg.attemptCache.Clear(item.DedupeKey, holder)
			lane.release(ctx, kept)
			result.end = roleKeepDrainFailed
			return result, fmt.Errorf("forward role %s's kept message %s to %s: %w", role, item.EventID, holder, forwarded)
		}
		if err := lane.write(ctx, roleKeepCursor{Kept: stored.Sequence}); err != nil {
			result.end, err = roleKeepWriteFailed(err)
			return result, err
		}
		kept = stored.Sequence
		result.forwarded++
		// The copy reached the role's holder, so it is reported where every role forward's
		// outcome is reported (finishRoleForward): counted in envoy_messages_delivered_total,
		// recorded in the dedupe cache, and logged on the role forward's own line, with the role
		// and the time the copy was kept beside it. A forward that drew no receipt is reported
		// there as the lane's own is, under receipt_timeout.
		finishRoleForward(cfg, roleForward{item: item, sessionID: holder, storedOnly: storedOnly}, forwarded,
			slog.String("role", role),
			slog.String("dedupe_key", forward.DedupeKey),
			slog.Int64("kept_at", keptAt))
	}
}

// roleKeepDrainHolds reports whether holder still holds role, so this walk may forward to it. The
// claim is read from the registry's watched copy of the role bucket
// (store.Registry.CachedRoleHolder), which costs no round trip, as the role lane reads a message's
// holder.
//
// Only a claim naming another session stops a walk. A role the copy holds no claim for is no
// evidence this walk's session lost it - the pass that started this walk resolved that session
// live a moment ago, and a release writes no claim at all - and a read that did not answer is
// evidence of nothing, so each leaves the walk to the session it was started for. What a role
// released under the walk costs is bounded by the pass's own resolution, which is taken after the
// pause before it rather than before (handOverKeptRole).
func roleKeepDrainHolds(cfg listenerDeliveryHandlerConfig, role, holder string) bool {
	held, err := cfg.registry.CachedRoleHolder(role)
	if err != nil {
		return true
	}
	return held == "" || held == holder
}

// judgeKeptRoleCopy judges one copy of role's kept lane for holder, before any cursor is written
// for it: it decodes the copy, measures its age against roleKeepWindow and applies beforeForward
// (keptRoleForwardAllowed). It reports the decoded copy, whether the walk forwards it, and, when
// it does not, what that copy is owed - its line, its counter and its delivery outcome - which the
// walk runs only once the cursor write has given it the copy, so two walks never report one copy
// twice. now is called once per copy, whatever the verdict.
func judgeKeptRoleCopy(cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, stored bus.StoredMessage, now func() time.Time, seen func(key, recipient string) bool) (contracts.Envelope, func(), bool) {
	var item contracts.Envelope
	if err := json.Unmarshal(stored.Data, &item); err != nil {
		// A copy no read can decode reaches no holder however often it is read, so the cursor
		// is moved past it rather than every later claim's drain stopping on it.
		return item, func() {
			cfg.logger.Error("listener kept role message cannot be decoded",
				slog.String("role", role),
				slog.Uint64("sequence", stored.Sequence),
				slog.String("error", err.Error()))
		}, false
	}
	if age := now().Sub(stored.Time); age > roleKeepWindow {
		return item, func() {
			// No role label: a role token names a tree role of one issue, so a deployment
			// sees unboundedly many of them and each would be a series of its own, kept for
			// the process's life. The role is on the log line below.
			cfg.roleKeptExpired.Inc()
			cfg.logger.DeliveryLog(slog.LevelWarn, "listener dropped a kept role message older than the keep window",
				holder, item.Topic, item.EventID, "expired",
				slog.String("role", role),
				slog.String("age", age.String()),
				slog.String("dedupe_key", item.DedupeKey))
		}, false
	}
	if allowed, report := keptRoleForwardAllowed(cfg, role, holder, entry, item, seen); !allowed {
		return item, report, false
	}
	return item, nil, true
}

// keptRoleRefusalLines are the drain's own lines for a copy beforeForward refused, which it writes
// under the status and the exception every forward path gives that refusal
// (refusedRoleForwardOutcome).
var keptRoleRefusalLines = roleForwardRefusalLines{
	echo:             "listener passed over a kept role message its own sender has claimed the role for",
	repeat:           "listener passed over a kept role message the role's new holder already took",
	modeUnreadable:   "listener kept role message's delivery mode cannot be read unambiguously",
	modeUnadvertised: "listener kept role message's new holder does not advertise delivery mode",
}

// keptRoleForwardAllowed judges one copy to the role's new holder, whose session entry is entry,
// by beforeForward - the rule the role lane and the publish route both apply before they forward -
// and reports whether the walk may forward it, and what a copy it may not is owed. The drain is
// the third forward path and reads the same record of what the holder already took as the lane
// does (seen), so an echo of a message the holder itself sent, a copy it already took and a
// payload its delivery modes do not take are refused here as they are there, each counted and
// answered as the lane counts and answers it (refusedRoleForwardOutcome) under the drain's own
// lines. The copy is passed over, and the report is the walk's to run once its cursor write has
// moved the lane past the copy, so no later walk offers it again and no walk that lost that write
// reports it.
func keptRoleForwardAllowed(cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, item contracts.Envelope, seen func(key, recipient string) bool) (bool, func()) {
	verdict := beforeForward(item, holder, entry, seen)
	if verdict.kind == forwardAllowed {
		return true, nil
	}
	outcome := refusedRoleForwardOutcome(verdict, item, holder, keptRoleRefusalLines, slog.String("role", role))
	return false, func() { applyDeliveryOutcome(cfg, item, outcome) }
}
