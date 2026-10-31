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

// roleKeepDrainWindow bounds one walk of a role's kept lane, which runs off the claim or the keep
// that started the handover. It covers roleKeepDrainMax forwards of roleReceiptTimeout each, and
// the stream read and the claim of each of the roleKeepScanMax copies the walk may read between
// them, so a walk whose holder answers no receipt still ends rather than outliving the claim it
// serves.
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

// roleKeepDrainRetryPause is how long a handover waits before its next walk when the one before
// ended on something that may pass: a read, a cursor write or a forward that failed, or a claim
// this listener's own walk released. It is short because the copies behind it have senders already
// answered 202 kept and a holder waiting, and it is not zero because a bus that is failing must
// not be walked again at once; the rounds that make no progress are bounded as well
// (roleKeepDrainIdleRounds).
const roleKeepDrainRetryPause = time.Second

// roleKeepDrainIdleRounds is how many walks that forwarded nothing one handover takes before it
// gives the role's lane up. A walk that forwarded copies made progress and is followed by another
// at once, however many it takes to empty the lane; a walk that forwarded nothing ended on a
// failure, on another listener's claim of the lane, or on a holder that is no longer this
// handover's, and repeating that without end would be a drain that never stops. Giving up costs
// the lane the wait for the role's next claim or keep, which is what every end of a drain cost
// before the handover.
const roleKeepDrainIdleRounds = 3

// roleKeepHandoverWindow bounds one handover of a role's kept lane: every walk one claim or one
// keep makes, the pauses between them included (drainKeptRole). Each walk is bounded by
// roleKeepDrainWindow and the walks that make no progress by roleKeepDrainIdleRounds, so this is
// the last bound rather than the usual one - a handover ends when the lane is drained or the role
// has no live holder - and it is wide enough for the walks a lane of thousands of copies takes.
const roleKeepHandoverWindow = 10 * time.Minute

// roleKeepHolderWindow bounds a handover's resolution of the role's holder: the claim read, the
// holder's session read, and at most one release of a lapsed claim. It is what the keep's own
// start resolves within (drainAfterKeep) and what each later walk of a handover re-resolves
// within. It answers no caller - the keep's own answer is written by then - so it ends rather than
// waiting out a bus that is not answering, and a resolution that did not answer costs only this
// copy's wait for the role's next claim.
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
// times for a copy it forwards and once for a copy it passes over, and each of those asks the same
// two questions of the answer, which write and release answer once here.
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
// hold last saw, keeps the sequence it was stored at for the next write, and reports whether this
// walk still owns the role's lane (writeRoleKeepCursor).
func (c *roleKeepLaneCursor) write(ctx context.Context, cursor roleKeepCursor) (bool, error) {
	seq, owned, err := writeRoleKeepCursor(ctx, c.cfg, c.role, c.holder, c.subject, c.seq, cursor)
	c.seq = seq
	return owned, err
}

// release gives up the claim this walk holds on one copy, so the role's next walk reads that copy
// again at once: it writes the cursor the claim replaced, kept where the lane was finished and
// nothing claimed. It is the end of a walk that claimed a copy it will not forward after all, and
// answers nobody: a release the cursor refused is a claim another walk has taken over already,
// and a release that failed leaves a claim the next walk takes over once roleKeepClaimWindow has
// passed, so both are logged at debug and neither changes what the walk reports.
func (c *roleKeepLaneCursor) release(ctx context.Context, kept uint64) {
	if _, err := c.write(ctx, roleKeepCursor{Kept: kept}); err != nil {
		c.cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain could not release its claim",
			slog.String("role", c.role),
			slog.String("session_id", c.holder),
			slog.String("error", err.Error()))
	}
}

// roleKeepWriteEnd is what a walk ends with when a cursor write did not leave it holding the
// lane, which every write of the cursor asks the same way: a write that failed ends the walk with
// its error, and one the stream refused because the cursor moved leaves the lane to the listener
// whose walk took it. It reports false when the write landed and the walk goes on.
func roleKeepWriteEnd(owned bool, err error) (roleKeepDrainEnd, bool) {
	switch {
	case err != nil:
		return roleKeepDrainFailed, true
	case !owned:
		return roleKeepLaneBusy, true
	default:
		return roleKeepLaneDrained, false
	}
}

// roleKeepGuards holds one guard per role, so two walks of one role inside this listener do not
// walk its kept lane together and read the same copy twice. A role's guard is kept for the set's
// life, which costs one mutex per role the listener it belongs to has seen claimed. One set is
// built per process (main.go), as listenerDeliveryHandlerConfig.roleKeepDrains.
type roleKeepGuards struct {
	guards sync.Map
}

func newRoleKeepGuards() *roleKeepGuards {
	return &roleKeepGuards{}
}

// guard is role's own mutex, made on the first drain of that role.
func (g *roleKeepGuards) guard(role string) *sync.Mutex {
	guard, _ := g.guards.LoadOrStore(role, &sync.Mutex{})
	return guard.(*sync.Mutex)
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
// now (drainAfterKeep), and reports whether anything was kept. It is the one keep both give-up
// paths take - the publish route's (roleSend.noHolder) and the role lane's (keepRoleLaneMessage) -
// so no give-up path can store a copy without offering it to the role's current holder, which is
// the claim this copy waits for having landed while the keep's caller was resolving the holder it
// did not find.
//
// Two messages are not kept at all, and each leaves its caller's give-up exactly as it was before
// the kept lane:
//
//   - one whose sender keeps a queue of its own and says so (contracts.Envelope.Keep false, the
//     flag Dispatch's outbox sets on the role-routed event it publishes straight onto the lane).
//     Its sender redelivers it from that queue, so a copy kept here would reach the role's next
//     holder twice. No keep flag at all means keep, so every other publisher is unchanged;
//   - one whose role token no claim can name (isValidRole, the pattern POST /v1/roles/set
//     refuses), which both give-up paths must agree on: a copy kept under such a token waits for a
//     claim that cannot be made and expires unread.
func keepForNextHolder(cfg listenerDeliveryHandlerConfig, role string, item contracts.Envelope) bool {
	if item.Keep != nil && !*item.Keep {
		return false
	}
	if !isValidRole(role) {
		return false
	}
	if !keepRoleMessage(cfg.client, cfg.logger, role, item) {
		return false
	}
	drainAfterKeep(cfg, role)
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

// startKeptRoleDrain hands role's kept lane to holder, the session that has just claimed it, off
// the claim's own request: the handover reads the stream and forwards, and the claim's status and
// body must not wait for either, so it runs in a goroutine under a context of its own
// (runKeptRoleDrain).
func startKeptRoleDrain(cfg *listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry) {
	config := *cfg
	go runKeptRoleDrain(config, role, holder, entry)
}

// drainAfterKeep hands role's kept lane to whoever holds the role now, just after a copy was kept
// for it. The keep's own resolution found no holder, and a claim that landed while it was
// resolving has already run its drain, which read the lane before this copy was on it: without
// this the copy would wait for the role's next claim, which seldom comes inside roleKeepWindow.
// The holder is resolved by role delivery's own rule (resolveRoleHolder with laneHolderFresh)
// within roleKeepHolderWindow, and only a live holder is drained to; a resolution that failed is
// logged at debug and changes nothing, since the copy is stored and the role's next claim takes
// it.
//
// Like a claim's it runs in a goroutine: neither keep's caller - the publish route's answer, the
// role lane's handler - waits for any of it. A handover that finds the lane drained already costs
// one stream read, and two walks of one role never hand the same copy to two holders
// (drainKeptRole).
func drainAfterKeep(cfg listenerDeliveryHandlerConfig, role string) {
	go func() {
		holderCtx, cancel := context.WithTimeout(context.Background(), roleKeepHolderWindow)
		resolved, err := resolveRoleHolder(holderCtx, cfg.registry, cfg.sessions, role, laneHolderFresh)
		cancel()
		if err != nil {
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role message could not resolve the role's holder",
				slog.String("role", role),
				slog.String("error", err.Error()))
			return
		}
		if resolved.state != roleHolderLive {
			return
		}
		runKeptRoleDrain(cfg, role, resolved.holder, resolved.entry)
	}()
}

// runKeptRoleDrain hands role's kept lane over to holder under a context of its own
// (roleKeepHandoverWindow) and logs a handover that failed: the one line both starts of a drain
// write, the claim's (startKeptRoleDrain) and the keep's (drainAfterKeep).
func runKeptRoleDrain(cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), roleKeepHandoverWindow)
	defer cancel()
	if err := drainKeptRole(ctx, cfg, role, holder, entry, time.Now); err != nil {
		cfg.logger.Error("listener kept role drain failed",
			slog.String("role", role),
			slog.String("session_id", holder),
			slog.String("error", err.Error()))
	}
}

// roleKeepDrainEnd is what ended one walk of a role's kept lane (walkKeptRole), which decides
// whether the handover walks it again and after how long (roleKeepDrainAgain).
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

// roleKeepDrainResult is what one walk of a role's kept lane did: what ended it and how many
// copies it forwarded, which is the progress the handover's bound on idle walks counts
// (roleKeepDrainIdleRounds).
type roleKeepDrainResult struct {
	end       roleKeepDrainEnd
	forwarded int
}

// roleKeepDrainAgain reports whether a handover walks the lane again after a walk that ended this
// way, and what it waits first. A drained lane is the end of the handover; a walk the budget or
// the scan ended is followed at once, since the copies behind it have senders answered 202 kept;
// one that failed or released its claim waits roleKeepDrainRetryPause, so a bus that is failing is
// not walked again at once; one that found the lane busy waits the claim out, since the walk
// holding it can still be forwarding; and one whose holder moved is followed at once, for the
// holder the next walk resolves.
func roleKeepDrainAgain(end roleKeepDrainEnd) (time.Duration, bool) {
	switch end {
	case roleKeepWorkLeft, roleKeepHolderMoved:
		return 0, true
	case roleKeepLaneBusy:
		return roleKeepClaimWindow, true
	case roleKeepDrainFailed:
		return roleKeepDrainRetryPause, true
	default:
		return 0, false
	}
}

// drainKeptRole hands role's kept lane to the session that holds the role, starting with holder,
// the session the claim or the keep that started this handover resolved. It is the whole of a
// handover: startKeptRoleDrain and drainAfterKeep are goroutines around this call, and tests call
// it directly for one role's lane.
//
// One walk of the lane (walkKeptRole) is bounded - roleKeepDrainMax forwards, roleKeepScanMax
// copies read, roleKeepDrainWindow of time - and nothing but a claim or a keep ever started
// another. A keep happens only while a role has no live holder and a live holder claims no second
// time, so every bound a walk stopped on used to strand the copies behind it on the lane until
// they aged out unread, with every one of their senders answered 202 kept. The handover walks the
// lane again instead, for the role's holder as it is then, until the lane is drained, the role has
// no live holder, or the walks stop making progress (roleKeepDrainIdleRounds): what ended the walk
// says how long it waits first (roleKeepDrainAgain), and the holder is resolved again between
// walks by role delivery's own rule, so a role nobody holds costs one resolution and ends.
//
// It returns the error of the last walk, which is the error one of these handed back, and ctx -
// roleKeepHandoverWindow in the listener - bounds the whole handover, pauses included.
//
// now is the clock a copy's age is measured against, time.Now in the listener: a test moves it
// rather than keeping a copy for an hour, since the age is taken from the stream's own store time,
// which no caller may choose. An outstanding claim's age is measured the same way, against the
// time the stream stored the cursor that carries it.
func drainKeptRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, now func() time.Time) error {
	for idle := 0; ; {
		walkCtx, cancel := context.WithTimeout(ctx, roleKeepDrainWindow)
		result, err := walkKeptRole(walkCtx, cfg, role, holder, entry, now)
		cancel()
		pause, again := roleKeepDrainAgain(result.end)
		if !again {
			return err
		}
		if result.forwarded > 0 {
			idle = 0
		} else if idle++; idle > roleKeepDrainIdleRounds {
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain gave the lane up with copies left on it",
				slog.String("role", role),
				slog.String("session_id", holder),
				slog.Int("idle_walks", idle))
			return err
		}
		holderCtx, cancelHolder := context.WithTimeout(ctx, roleKeepHolderWindow)
		resolved, holderErr := resolveRoleHolder(holderCtx, cfg.registry, cfg.sessions, role, laneHolderFresh)
		cancelHolder()
		if holderErr != nil || resolved.state != roleHolderLive {
			return err
		}
		holder, entry = resolved.holder, resolved.entry
		if pause > 0 && waitUnlessDone(ctx, pause) != nil {
			return err
		}
	}
}

// walkKeptRole is one walk of role's kept lane for holder. It reads the role's cursor and walks the
// copies stored after it - at most roleKeepScanMax of them, and until roleKeepDrainMax have been
// forwarded - forwarding each copy that is still inside roleKeepWindow and that beforeForward
// allows to the holder's agent subject exactly as a live role send forwards it
// (roleForwardDedupePrefix, contracts.AgentSubject), and reports what ended it, so the handover
// around it knows whether the lane has copies left (drainKeptRole).
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
// session: this walk forwards to the session it was started for and never reads its claim again,
// so without that a claimant a later write superseded, or an old holder whose walk stepped aside
// for a live claim, would spend its budget on a session that no longer holds the role. The lane is
// left to the new holder, whose copies the handover's next walk forwards.
//
// A forward that reached the server and drew no receipt (bus.ErrReceiptTimeout) counts as
// forwarded and the walk goes on, and that copy is confirmed: its copy is stored on the holder's
// agent subject, which a returning session replays. Any other forward failure ends the walk and
// is returned, with its copy released for the role's next walk and the rest of the lane behind
// it. A cursor write the stream refused because another listener's walk has the lane
// (bus.ErrCursorMoved) is nothing wrong: that listener owns this role's lane now, and this walk
// stops.
func walkKeptRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, now func() time.Time) (roleKeepDrainResult, error) {
	guard := cfg.roleKeepDrains.guard(role)
	guard.Lock()
	defer guard.Unlock()

	// The role's kept lane is read first, and a claim of a role nothing was ever kept for ends
	// here, on one stream read rather than two: the cursor says nothing a lane with no message
	// can act on. A lane the cursor has already been advanced past ends on the second read, as
	// a cursor-first walk would.
	keptSubject := bus.KeptRoleSubject(role)
	last, err := cfg.client.LastStoredMessage(ctx, keptSubject)
	if errors.Is(err, nats.ErrMsgNotFound) {
		return roleKeepDrainResult{end: roleKeepLaneDrained}, nil
	}
	if err != nil {
		return roleKeepDrainResult{end: roleKeepDrainFailed}, fmt.Errorf("read role %s's kept lane: %w", role, err)
	}
	cursorSubject := bus.KeptRoleCursorSubject(role)
	cursorSeq, cursor, written, err := readRoleKeepCursor(ctx, cfg.client, cursorSubject)
	if err != nil {
		return roleKeepDrainResult{end: roleKeepDrainFailed}, fmt.Errorf("read role %s's kept-lane cursor: %w", role, err)
	}
	if cursor.Kept >= last.Sequence {
		return roleKeepDrainResult{end: roleKeepLaneDrained}, nil
	}
	// A copy another walk holds a claim of is that walk's to forward, so this one leaves the
	// lane to it rather than forwarding the copy a second time; the claim is read only here,
	// where it can be judged against the walk that wrote it, so now is asked for once per copy
	// in the walk below and once more only while a claim is outstanding.
	if cursor.Claiming != 0 && now().Sub(written) < roleKeepClaimWindow {
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain left the lane to the drain holding a claim",
			slog.String("role", role),
			slog.String("session_id", holder),
			slog.Uint64("claiming", cursor.Claiming))
		return roleKeepDrainResult{end: roleKeepLaneBusy}, nil
	}

	lane := &roleKeepLaneCursor{cfg: cfg, role: role, holder: holder, subject: cursorSubject, seq: cursorSeq}
	subject := contracts.AgentSubject(holder)
	storedOnly := roleForwardStoredOnly(entry)
	// The record the role lane judges a forward against, read the same way here: what the holder
	// already took, and what is already on its way to it.
	seen := func(key, recipient string) bool {
		return cfg.dedupeCache.Seen(key, recipient) || cfg.attemptCache.Seen(key, recipient)
	}
	result := roleKeepDrainResult{}
	kept := cursor.Kept
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
			owned, err := lane.write(ctx, roleKeepCursor{Kept: stored.Sequence})
			if end, stop := roleKeepWriteEnd(owned, err); stop {
				result.end = end
				return result, err
			}
			kept = stored.Sequence
			report()
			continue
		}
		owned, err := lane.write(ctx, roleKeepCursor{Kept: kept, Claiming: stored.Sequence})
		if end, stop := roleKeepWriteEnd(owned, err); stop {
			result.end = end
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
		if err := forwardKeptRoleMessage(cfg, subject, forward, storedOnly); err != nil {
			// This walk is ending with the copy claimed and unforwarded, so it releases the
			// claim: the cursor goes back to what it was before the claim, and the role's next
			// walk reads this copy again at once rather than waiting the claim out. A release
			// that cannot be written leaves the claim, which the next walk takes over once it
			// is older than roleKeepClaimWindow; either way the copy is not lost.
			cfg.attemptCache.Clear(item.DedupeKey, holder)
			lane.release(ctx, kept)
			result.end = roleKeepDrainFailed
			return result, fmt.Errorf("forward role %s's kept message %s to %s: %w", role, item.EventID, holder, err)
		}
		owned, err = lane.write(ctx, roleKeepCursor{Kept: stored.Sequence})
		if end, stop := roleKeepWriteEnd(owned, err); stop {
			result.end = end
			return result, err
		}
		kept = stored.Sequence
		result.forwarded++
		// The copy reached the role's holder, so it is reported where every role forward's
		// outcome is reported (finishRoleForward): counted in envoy_messages_delivered_total,
		// recorded in the dedupe cache, and logged on the role forward's own line, with the role
		// and the time the copy was kept beside it.
		finishRoleForward(cfg, roleForward{item: item, sessionID: holder, storedOnly: storedOnly}, nil,
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
// evidence this walk's session lost it - a keep's handover walks for a holder resolved live a
// moment ago, and a release writes no claim at all - and a read that did not answer is evidence of
// nothing, so each leaves the walk to the session it was started for, which the resolution that
// started it found live.
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

// writeRoleKeepCursor writes cursor as role's drain cursor for this walk, under the
// compare-and-swap of the cursor message's own last sequence (bus.Client.PublishCursorAt,
// cursorSeq, 0 for a role whose lane has never been drained). It returns the sequence the new
// cursor was stored at, which the walk's next write names, and whether this walk still owns the
// role's lane: a write the stream refused because the cursor moved (bus.ErrCursorMoved) means
// another listener's walk owns the lane, which is nothing wrong, so it is logged at debug and the
// walk stops. Every other failure is the error it is. One hold of the cursor writes it through
// roleKeepLaneCursor, which keeps the sequence each write leaves behind.
func writeRoleKeepCursor(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder, cursorSubject string, cursorSeq uint64, cursor roleKeepCursor) (uint64, bool, error) {
	body, err := json.Marshal(cursor)
	if err != nil {
		return cursorSeq, false, fmt.Errorf("encode role %s's kept-lane cursor at %d: %w", role, cursor.Kept, err)
	}
	next, err := cfg.client.PublishCursorAt(ctx, cursorSubject, body, cursorSeq)
	if err != nil {
		if errors.Is(err, bus.ErrCursorMoved) {
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain lost the cursor to another listener",
				slog.String("role", role),
				slog.String("session_id", holder))
			return cursorSeq, false, nil
		}
		return cursorSeq, false, fmt.Errorf("advance role %s's kept-lane cursor to kept %d claiming %d: %w", role, cursor.Kept, cursor.Claiming, err)
	}
	return next, true, nil
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

// readRoleKeepCursor reads a role's drain cursor and returns what the walk needs of it: the
// cursor message's own stream sequence, which its compare-and-swap write must name, the cursor
// body itself - the sequence the lane is finished with, where the walk starts, and the claim one
// walk holds, 0 for none - and the time the stream stored it, which is how old that claim is. A
// role whose lane has never been drained holds no cursor (nats.ErrMsgNotFound): the sequence is
// 0, the body is its zero value and the time is the zero time, which no claim is judged against
// since the body claims nothing.
func readRoleKeepCursor(ctx context.Context, client *bus.Client, subject string) (cursorSeq uint64, cursor roleKeepCursor, written time.Time, err error) {
	stored, err := client.LastStoredMessage(ctx, subject)
	if errors.Is(err, nats.ErrMsgNotFound) {
		return 0, roleKeepCursor{}, time.Time{}, nil
	}
	if err != nil {
		return 0, roleKeepCursor{}, time.Time{}, err
	}
	if err := json.Unmarshal(stored.Data, &cursor); err != nil {
		return 0, roleKeepCursor{}, time.Time{}, fmt.Errorf("decode the cursor stored at %d: %w", stored.Sequence, err)
	}
	return stored.Sequence, cursor, stored.Time, nil
}

// forwardKeptRoleMessage forwards one kept copy and waits for its outcome, as the publish route
// waits for a live role send's (listenerDeliveryHandlerConfig.forwardAndWait). A forward that
// reached the server and drew no receipt is delivery all the same: its copy is stored on the
// holder's agent subject, so it is reported as forwarded rather than ending the walk.
func forwardKeptRoleMessage(cfg listenerDeliveryHandlerConfig, subject string, item contracts.Envelope, storedOnly bool) error {
	if err := cfg.forwardAndWait(subject, item, roleReceiptTimeout, storedOnly); err != nil && !errors.Is(err, bus.ErrReceiptTimeout) {
		return err
	}
	return nil
}
