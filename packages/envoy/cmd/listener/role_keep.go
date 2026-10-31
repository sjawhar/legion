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

// roleKeepDrainMax is how many kept copies one claim walks. The claim answers its caller at once
// and the drain runs behind it, so the drain is bounded work rather than however much a role's
// kept lane has gathered; the cursor it leaves means the role's next claim takes the rest.
const roleKeepDrainMax = 20

// roleKeepScanMax bounds the walk itself, where roleKeepDrainMax bounds the forwards it makes. A
// copy past roleKeepWindow, and one no read can decode, is passed over rather than forwarded, and
// a drain that counted those against its forward budget would walk twenty dead copies and hand
// the role's new holder nothing. The stream keeps a kept copy for 72 h while the keep window is
// one hour, so a role's lane can hold nothing but copies past the window: this is what still ends
// that drain.
const roleKeepScanMax = 200

// roleKeepPublishWindow bounds the keep's own publish. The keep happens on a send that is already
// out of time or on a lane delivery that answers nobody, so it takes a context of its own rather
// than whatever is left of its caller's, and that context is short: a bus that does not store the
// copy promptly is a keep that failed, which its caller answers for.
const roleKeepPublishWindow = 2 * time.Second

// roleKeepDrainWindow bounds one drain, which runs off the claim or the keep that started it. It
// covers roleKeepDrainMax forwards of roleReceiptTimeout each, and the stream read and the claim
// of each of the roleKeepScanMax copies the walk may read between them, so a drain whose holder
// answers no receipt still ends rather than outliving the claim it serves.
const roleKeepDrainWindow = 60 * time.Second

// roleKeepClaimWindow is how long one drain's claim of a kept copy holds that role's lane against
// the other drains of the deployment (roleKeepCursor.Claiming). It is roleKeepDrainWindow because
// that is the whole of a drain's life: a claim younger than it may still belong to a drain that
// is forwarding that copy right now, which another drain must not forward a second time, and a
// claim older than it belongs to a drain whose context has ended, so the copy it claimed is free
// for the next drain to forward. A drain that ends on a forward it could not make does not wait
// this out: it releases its claim (releaseRoleKeepClaim).
const roleKeepClaimWindow = roleKeepDrainWindow

// roleKeepHolderWindow bounds the holder resolution a keep makes after it stored its copy
// (drainAfterKeep): a claim read, the holder's session read, and at most one release of a lapsed
// claim. It answers no caller - the keep's own answer is written by then - so it ends rather than
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
// and the walk starts at the copy after it. Claiming is a copy one drain has claimed and not yet
// confirmed delivered, 0 when none is outstanding: a drain writes it before it forwards that
// copy and clears it with the same write that moves Kept past the copy, so a drain that ends
// between the two leaves Kept short of the copy and the next drain reads that copy again. Until
// it is older than roleKeepClaimWindow, measured against the time the stream stored the cursor
// carrying it, a claim is also the lane's lock: the drain that reads it leaves the lane alone,
// so the copy is forwarded a second time only once the drain that claimed it can no longer be
// forwarding it. It is the listener's own small body rather than an envelope, published with
// bus.Client.PublishCursorAt.
type roleKeepCursor struct {
	Kept     uint64 `json:"kept"`
	Claiming uint64 `json:"claiming"`
}

// roleKeepGuards holds one guard per role, so two drains of one role inside this listener do not
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

// keepRoleLaneMessage keeps the role lane's copy of a message it found no live holder for, and
// reports whether it was kept. Kept, it writes the lane's line and counts the delivery under
// roleKeptDeliveryStatus and publishes no delivery exception: the message is not lost, the role's
// next holder is handed it. A keep that failed is logged and reported false, and the caller's own
// give-up - its exception, its metric and its line - stands exactly as it was.
//
// The lane reads no keep flag. A raw role publish carries none: keeping is chosen on POST
// /v1/messages/publish (its keep field), and the one sender that opts out of it, the Legion
// daemon, publishes through that route, which delivers a role message itself and never puts one on
// the lane.
//
// A keep that landed hands the lane to whoever holds the role now (drainAfterKeep): the claim
// this copy waits for can have landed while the lane was resolving the holder it did not find.
func keepRoleLaneMessage(cfg listenerDeliveryHandlerConfig, role, holder string, item contracts.Envelope, state roleHolderState) bool {
	if !keepRoleMessage(cfg.client, cfg.logger, role, item) {
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
	drainAfterKeep(&cfg, role)
	return true
}

// startKeptRoleDrain hands role's kept lane to holder, the session that has just claimed it, off
// the claim's own request: the drain reads the stream and forwards, and the claim's status and
// body must not wait for either, so it runs in a goroutine under a context of its own bounded by
// roleKeepDrainWindow.
func startKeptRoleDrain(cfg *listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry) {
	config := *cfg
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), roleKeepDrainWindow)
		defer cancel()
		runKeptRoleDrain(ctx, config, role, holder, entry)
	}()
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
// Like a claim's drain it runs in a goroutine: neither keep's caller - the publish route's answer,
// the role lane's handler - waits for any of it. A drain that finds the lane drained already costs
// one stream read, and two drains of one role never hand the same copy to two holders
// (drainKeptRole).
func drainAfterKeep(cfg *listenerDeliveryHandlerConfig, role string) {
	config := *cfg
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), roleKeepDrainWindow)
		defer cancel()
		holderCtx, cancelHolder := context.WithTimeout(ctx, roleKeepHolderWindow)
		resolved, err := resolveRoleHolder(holderCtx, config.registry, config.sessions, role, laneHolderFresh)
		cancelHolder()
		if err != nil {
			config.logger.LogEvent(slog.LevelDebug, "listener kept role message could not resolve the role's holder",
				slog.String("role", role),
				slog.String("error", err.Error()))
			return
		}
		if resolved.state != roleHolderLive {
			return
		}
		runKeptRoleDrain(ctx, config, role, resolved.holder, resolved.entry)
	}()
}

// runKeptRoleDrain drains role's kept lane to holder and logs a drain that failed: the one line
// both starts of a drain write, the claim's (startKeptRoleDrain) and the keep's (drainAfterKeep).
func runKeptRoleDrain(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry) {
	if err := drainKeptRole(ctx, cfg, role, holder, entry, time.Now); err != nil {
		cfg.logger.Error("listener kept role drain failed",
			slog.String("role", role),
			slog.String("session_id", holder),
			slog.String("error", err.Error()))
	}
}

// drainKeptRole forwards role's kept copies to holder, the session that just claimed the role, and
// is the whole of the drain: startKeptRoleDrain and drainAfterKeep are goroutines around this
// call. It reads the role's cursor and walks the copies stored after it - at most roleKeepScanMax
// of them, and until roleKeepDrainMax have been forwarded - forwarding each copy that is still
// inside roleKeepWindow and that beforeForward allows to the holder's agent subject exactly as a
// live role send forwards it (roleForwardDedupePrefix, contracts.AgentSubject).
//
// Every step the drain takes with a copy it writes the role's cursor for, under the
// compare-and-swap of the cursor's own last sequence (writeRoleKeepCursor), and that write is
// what decides which drain owns the copy: the loser of it stops, where a cursor written once at
// the end of the walk let two listeners forward everything they read.
//
// A copy nothing will be forwarded for - one no read can decode, one past roleKeepWindow, one
// beforeForward refuses - is finished in one write, to {kept: its sequence, claiming: 0}, before
// its line, its counter and its delivery outcome, so a drain that lost that write neither counts
// nor logs a copy another drain reports; it is why a copy past the window is counted expired once
// (envoy_role_kept_expired_total) however many drains walk the lane.
//
// A copy that will be forwarded takes three: the cursor is written to {kept: unchanged, claiming:
// its sequence}, the copy is forwarded, and the cursor is written to {kept: its sequence,
// claiming: 0}. What the claim buys is that a copy still reaches exactly one holder: a drain that
// reads a claim younger than roleKeepClaimWindow leaves the lane to the drain holding it and
// forwards nothing, and a drain whose own claim the cursor refused stops where it is. What the
// unmoved kept buys is that no copy is lost whatever ends the drain - a process killed, a forward
// that failed, a context that ran out - because the walk starts at kept + 1, so a copy claimed
// and never confirmed is simply the next copy a drain reads: the next one of all, where the drain
// released its claim on its way out (releaseRoleKeepClaim), and the next one past
// roleKeepClaimWindow where nothing was left to release it. What that costs is one more copy when
// a drain ends between its forward and its confirm: the repeat carries the same stable id
// (bus.ForwardMsgID of the forward's dedupe key and the agent subject), so for the same holder the
// stream's duplicate window keeps one stored copy and the holder's own dedupe_key drops the
// second, and only a holder that changed meanwhile sees the copy reach two holders, once, on a
// failure.
//
// now is the clock a copy's age is measured against, time.Now in the listener: a test moves it
// rather than keeping a copy for an hour, since the age is taken from the stream's own store time,
// which no caller may choose. An outstanding claim's age is measured the same way, against the
// time the stream stored the cursor that carries it.
//
// A forward that reached the server and drew no receipt (bus.ErrReceiptTimeout) counts as
// forwarded and the walk goes on, and that copy is confirmed: its copy is stored on the holder's
// agent subject, which a returning session replays. Any other forward failure ends the drain and
// is returned, with its copy released for the role's next drain and the rest of the lane behind
// it. A cursor write the stream refused because another listener's drain has the lane
// (bus.ErrCursorMoved) is nothing wrong: that listener owns this role's lane now, and this drain
// stops.
func drainKeptRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, now func() time.Time) error {
	guard := cfg.roleKeepDrains.guard(role)
	guard.Lock()
	defer guard.Unlock()

	// The role's kept lane is read first, and a claim of a role nothing was ever kept for ends
	// here, on one stream read rather than two: the cursor says nothing a lane with no message
	// can act on. A lane the cursor has already been advanced past ends on the second read, as
	// a cursor-first drain would.
	keptSubject := bus.KeptRoleSubject(role)
	last, err := cfg.client.LastStoredMessage(ctx, keptSubject)
	if errors.Is(err, nats.ErrMsgNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read role %s's kept lane: %w", role, err)
	}
	cursorSubject := bus.KeptRoleCursorSubject(role)
	cursorSeq, cursor, written, err := readRoleKeepCursor(ctx, cfg.client, cursorSubject)
	if err != nil {
		return fmt.Errorf("read role %s's kept-lane cursor: %w", role, err)
	}
	if cursor.Kept >= last.Sequence {
		return nil
	}
	// A copy another drain holds a claim of is that drain's to forward, so this one leaves the
	// lane to it rather than forwarding the copy a second time; the claim is read only here,
	// where it can be judged against the drain that wrote it, so now is asked for once per copy
	// in the walk below and once more only while a claim is outstanding.
	if cursor.Claiming != 0 && now().Sub(written) < roleKeepClaimWindow {
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain left the lane to the drain holding a claim",
			slog.String("role", role),
			slog.String("session_id", holder),
			slog.Uint64("claiming", cursor.Claiming))
		return nil
	}

	subject := contracts.AgentSubject(holder)
	storedOnly := roleForwardStoredOnly(entry)
	// The record the role lane judges a forward against, read the same way here: what the holder
	// already took, and what is already on its way to it.
	seen := func(key, recipient string) bool {
		return cfg.dedupeCache.Seen(key, recipient) || cfg.attemptCache.Seen(key, recipient)
	}
	kept := cursor.Kept
	for scanned, forwarded := 0, 0; scanned < roleKeepScanMax && forwarded < roleKeepDrainMax; scanned++ {
		stored, err := cfg.client.NextStoredMessage(ctx, keptSubject, kept+1)
		if errors.Is(err, nats.ErrMsgNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read role %s's kept lane after %d: %w", role, kept, err)
		}
		item, report, forwardable := judgeKeptRoleCopy(cfg, role, holder, entry, stored, now, seen)
		if !forwardable {
			var owned bool
			cursorSeq, owned, err = writeRoleKeepCursor(ctx, cfg, role, holder, cursorSubject, cursorSeq, roleKeepCursor{Kept: stored.Sequence})
			if err != nil {
				return err
			}
			if !owned {
				return nil
			}
			kept = stored.Sequence
			report()
			continue
		}
		var owned bool
		cursorSeq, owned, err = writeRoleKeepCursor(ctx, cfg, role, holder, cursorSubject, cursorSeq, roleKeepCursor{Kept: kept, Claiming: stored.Sequence})
		if err != nil {
			return err
		}
		if !owned {
			return nil
		}
		forward := item
		forward.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
		keptAt := stored.Time.UnixMilli()
		forward.KeptAt = &keptAt
		if err := forwardKeptRoleMessage(cfg, subject, forward, storedOnly); err != nil {
			// This drain is ending with the copy claimed and unforwarded, so it releases the
			// claim: the cursor goes back to what it was before the claim, and the role's next
			// drain reads this copy again at once rather than waiting the claim out. A release
			// that cannot be written leaves the claim, which the next drain takes over once it
			// is older than roleKeepClaimWindow; either way the copy is not lost.
			releaseRoleKeepClaim(ctx, cfg, role, holder, cursorSubject, cursorSeq, kept)
			return fmt.Errorf("forward role %s's kept message %s to %s: %w", role, item.EventID, holder, err)
		}
		cursorSeq, owned, err = writeRoleKeepCursor(ctx, cfg, role, holder, cursorSubject, cursorSeq, roleKeepCursor{Kept: stored.Sequence})
		if err != nil {
			return err
		}
		if !owned {
			return nil
		}
		kept = stored.Sequence
		forwarded++
		cfg.logger.DeliveryLog(slog.LevelInfo, "listener forwarded a kept role message to the role's new holder",
			holder, item.Topic, item.EventID, "delivered",
			slog.String("role", role),
			slog.String("dedupe_key", forward.DedupeKey),
			slog.Int64("kept_at", keptAt))
	}
	return nil
}

// judgeKeptRoleCopy judges one copy of role's kept lane for holder, before any cursor is written
// for it: it decodes the copy, measures its age against roleKeepWindow and applies beforeForward
// (keptRoleForwardAllowed). It reports the decoded copy, whether the drain forwards it, and, when
// it does not, what that copy is owed - its line, its counter and its delivery outcome - which the
// drain runs only once the cursor write has given it the copy, so two drains never report one copy
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

// writeRoleKeepCursor writes cursor as role's drain cursor for this drain, under the
// compare-and-swap of the cursor message's own last sequence (bus.Client.PublishCursorAt,
// cursorSeq, 0 for a role whose lane has never been drained). It returns the sequence the new
// cursor was stored at, which the drain's next write names, and whether this drain still owns the
// role's lane: a write the stream refused because the cursor moved (bus.ErrCursorMoved) means
// another listener's drain owns the lane, which is nothing wrong, so it is logged at debug and the
// drain stops. Every other failure is the error it is.
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

// keptRoleForwardAllowed judges one copy to the role's new holder, whose session entry is entry,
// by beforeForward - the rule the role lane and the publish route both apply before they forward -
// and reports whether the drain may forward it, and what a copy it may not is owed. The drain is
// the third forward path and reads the same record of what the holder already took as the lane
// does (seen), so an echo of a message the holder itself sent, a copy it already took and a
// payload its delivery modes do not take are refused here as they are there, each under the lane's
// own line and count. The copy is passed over, and the report is the drain's to run once its
// cursor write has moved the lane past the copy, so no later drain offers it again and no drain
// that lost that write reports it.
func keptRoleForwardAllowed(cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, item contracts.Envelope, seen func(key, recipient string) bool) (bool, func()) {
	verdict := beforeForward(item, holder, entry, seen)
	if verdict.kind == forwardAllowed {
		return true, nil
	}
	outcome := deliveryOutcome{sessionID: holder}
	switch verdict.kind {
	case forwardEcho:
		outcome.metricStatus = "skipped"
		outcome.log = func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelInfo, "listener passed over a kept role message its own sender has claimed the role for",
				holder, item.Topic, item.EventID, "skipped", slog.String("role", role))
		}
	case forwardRepeat:
		outcome.metricStatus = "dedupe"
		outcome.log = func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelInfo, "listener passed over a kept role message the role's new holder already took",
				holder, item.Topic, item.EventID, "dedupe", slog.String("role", role), slog.String("dedupe_key", item.DedupeKey))
		}
	case forwardModeUnreadable:
		outcome.metricStatus = "failed"
		outcome.log = func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelWarn, "listener kept role message's delivery mode cannot be read unambiguously",
				holder, item.Topic, item.EventID, "failed", slog.String("role", role))
		}
		outcome.exceptionReason = "delivery_failed"
	case forwardModeUnadvertised:
		outcome.metricStatus = "failed"
		outcome.log = func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelWarn, "listener kept role message's new holder does not advertise delivery mode",
				holder, item.Topic, item.EventID, "failed", slog.String("role", role), slog.String("mode", verdict.mode))
		}
		outcome.exceptionReason = "delivery_failed"
	}
	return false, func() { applyDeliveryOutcome(cfg, item, outcome) }
}

// readRoleKeepCursor reads a role's drain cursor and returns what the drain needs of it: the
// cursor message's own stream sequence, which its compare-and-swap write must name, the cursor
// body itself - the sequence the lane is finished with, where the walk starts, and the claim one
// drain holds, 0 for none - and the time the stream stored it, which is how old that claim is. A
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

// releaseRoleKeepClaim gives up the claim this drain holds on one copy, so the role's next drain
// reads that copy again at once: it writes the cursor the drain's claim replaced, kept where the
// lane was finished and nothing claimed, under the same compare-and-swap every other write of the
// cursor makes. It is the end of a drain that claimed a copy it will not forward after all, and
// answers nobody: a release the cursor refused is a claim another drain has taken over already,
// and a release that failed leaves a claim the next drain takes over once roleKeepClaimWindow has
// passed, so both are logged at debug and neither changes what the drain returns.
func releaseRoleKeepClaim(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder, cursorSubject string, cursorSeq, kept uint64) {
	if _, _, err := writeRoleKeepCursor(ctx, cfg, role, holder, cursorSubject, cursorSeq, roleKeepCursor{Kept: kept}); err != nil {
		cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain could not release its claim",
			slog.String("role", role),
			slog.String("session_id", holder),
			slog.String("error", err.Error()))
	}
}

// forwardKeptRoleMessage forwards one kept copy and waits for its outcome, as the publish route
// waits for a live role send's (listenerDeps.forwardAndWait). A forward that reached the server
// and drew no receipt is delivery all the same: its copy is stored on the holder's agent subject,
// so it is reported as forwarded rather than ending the drain.
func forwardKeptRoleMessage(cfg listenerDeliveryHandlerConfig, subject string, item contracts.Envelope, storedOnly bool) error {
	wait, err := cfg.forwardRole(subject, item, roleReceiptTimeout, storedOnly)
	if err != nil {
		return err
	}
	if err := wait(); err != nil && !errors.Is(err, bus.ErrReceiptTimeout) {
		return err
	}
	return nil
}
