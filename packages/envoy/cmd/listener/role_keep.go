// The listener's kept lane for role messages: the copy it stores when a role message has no live
// holder to take it (contracts.KeptRoleSubject), and the drain that hands those copies to the
// role's next holder when the role is claimed (contracts.KeptRoleCursorSubject). Both subjects are
// the listener's own: no session may register a topic under them, and no consumer of the
// notification stream delivers one (bus.DurableNotificationSubjects), so a kept copy never
// re-enters the delivery path it was taken out of.

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

// roleKeepPublishWindow bounds the keep's own publish. The keep happens on a send that is already
// out of time or on a lane delivery that answers nobody, so it takes a context of its own rather
// than whatever is left of its caller's, and that context is short: a bus that does not store the
// copy promptly is a keep that failed, which its caller answers for.
const roleKeepPublishWindow = 2 * time.Second

// roleKeepDrainWindow bounds one claim's whole drain, which runs off the claim's request. It
// covers roleKeepDrainMax forwards of roleReceiptTimeout each and the stream reads between them,
// so a drain whose holder answers no receipt still ends rather than outliving the claim it serves.
const roleKeepDrainWindow = 60 * time.Second

// roleKeptDeliveryStatus is the delivery_status of a role message kept instead of delivered: the
// label the role lane counts under messagesDelivered and logs, and the one the publish route's own
// kept answer reads as (sendKept.deliveryStatus), so route and lane say the same word for it.
const roleKeptDeliveryStatus = "kept"

// roleKeepFailedLine is logged by both give-up paths - the publish route's and the role lane's -
// when the copy could not be kept, beside the answer each gives when nothing was kept.
const roleKeepFailedLine = "listener kept role message publish failed"

// roleKeepCursor is the body of a role's drain cursor: the stream sequence on that role's kept
// subject up to which its copies have been forwarded or dropped. It is the listener's own small
// body rather than an envelope, published with bus.Client.PublishCursorAt.
type roleKeepCursor struct {
	Kept uint64 `json:"kept"`
}

// roleKeepDrains holds one guard per role, so two claims of one role inside this listener do not
// walk its kept lane together and forward the same copy twice. Across listeners the cursor's
// compare-and-swap is the guard: the loser's write is refused (bus.ErrCursorMoved) and it stops.
// A role's guard is kept for the process's life, which costs one mutex per role this listener has
// ever seen claimed.
var roleKeepDrains sync.Map

func roleKeepGuard(role string) *sync.Mutex {
	guard, _ := roleKeepDrains.LoadOrStore(role, &sync.Mutex{})
	return guard.(*sync.Mutex)
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
	if _, err := client.PublishStoredOn(ctx, contracts.KeptRoleSubject(role), item, bus.KeptMsgID(item.DedupeKey, role)); err != nil {
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
	return true
}

// startKeptRoleDrain hands role's kept lane to holder, the session that has just claimed it, off
// the claim's own request: the drain reads the stream and forwards, and the claim's status and
// body must not wait for either, so it runs in a goroutine under a context of its own bounded by
// roleKeepDrainWindow. A listener whose delivery configuration is not wired starts nothing: it has
// no forward to hand a kept copy to.
func startKeptRoleDrain(cfg *listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry) {
	if cfg == nil {
		return
	}
	config := *cfg
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), roleKeepDrainWindow)
		defer cancel()
		if err := drainKeptRole(ctx, config, role, holder, entry, time.Now); err != nil {
			config.logger.Error("listener kept role drain failed",
				slog.String("role", role),
				slog.String("session_id", holder),
				slog.String("error", err.Error()))
		}
	}()
}

// drainKeptRole forwards role's kept copies to holder, the session that just claimed the role, and
// is the whole of the drain: startKeptRoleDrain is a goroutine around this call. It reads the
// role's cursor, walks at most roleKeepDrainMax copies from where that cursor stands, forwards
// each one that is still inside roleKeepWindow to the holder's agent subject exactly as a live
// role send forwards it (roleForwardDedupePrefix, contracts.AgentSubject), and advances the cursor
// once at the end to the last copy it forwarded or dropped.
//
// now is the clock a copy's age is measured against, time.Now in the listener: a test moves it
// rather than keeping a copy for an hour, since the age is taken from the stream's own store time,
// which no caller may choose.
//
// A forward that reached the server and drew no receipt (bus.ErrReceiptTimeout) counts as
// forwarded and the walk goes on: its copy is stored on the holder's agent subject, which a
// returning session replays. Any other forward failure ends the walk with the cursor at the last
// copy that was forwarded or dropped, so the rest is handed to the role's next claim. A cursor
// write another listener's drain beat (bus.ErrCursorMoved) is that listener having drained this
// role at the same moment: nothing is wrong, and this drain stops.
func drainKeptRole(ctx context.Context, cfg listenerDeliveryHandlerConfig, role, holder string, entry session.SessionEntry, now func() time.Time) error {
	guard := roleKeepGuard(role)
	guard.Lock()
	defer guard.Unlock()

	// The role's kept lane is read first, and a claim of a role nothing was ever kept for ends
	// here, on one stream read rather than two: the cursor says nothing a lane with no message
	// can act on. A lane the cursor has already been advanced past ends on the second read, as
	// a cursor-first drain would.
	keptSubject := contracts.KeptRoleSubject(role)
	last, err := cfg.client.LastStoredMessage(ctx, keptSubject)
	if errors.Is(err, nats.ErrMsgNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read role %s's kept lane: %w", role, err)
	}
	cursorSubject := contracts.KeptRoleCursorSubject(role)
	cursorSeq, from, err := readRoleKeepCursor(ctx, cfg.client, cursorSubject)
	if err != nil {
		return fmt.Errorf("read role %s's kept-lane cursor: %w", role, err)
	}
	if from >= last.Sequence {
		return nil
	}

	subject := contracts.AgentSubject(holder)
	storedOnly := roleForwardStoredOnly(entry)
	kept := from
	var walk error
	for range roleKeepDrainMax {
		stored, err := cfg.client.NextStoredMessage(ctx, keptSubject, kept+1)
		if errors.Is(err, nats.ErrMsgNotFound) {
			break
		}
		if err != nil {
			walk = fmt.Errorf("read role %s's kept lane after %d: %w", role, kept, err)
			break
		}
		var item contracts.Envelope
		if err := json.Unmarshal(stored.Data, &item); err != nil {
			// A copy no read can decode reaches no holder however often it is read, so the
			// cursor moves past it rather than every later claim's drain stopping on it.
			cfg.logger.Error("listener kept role message cannot be decoded",
				slog.String("role", role),
				slog.Uint64("sequence", stored.Sequence),
				slog.String("error", err.Error()))
			kept = stored.Sequence
			continue
		}
		if age := now().Sub(stored.Time); age > roleKeepWindow {
			// No role label: a role token names a tree role of one issue, so a deployment
			// sees unboundedly many of them and each would be a series of its own, kept for
			// the process's life. The role is on the log line below.
			cfg.roleKeptExpired.Inc()
			cfg.logger.DeliveryLog(slog.LevelWarn, "listener dropped a kept role message older than the keep window",
				holder, item.Topic, item.EventID, "expired",
				slog.String("role", role),
				slog.String("age", age.String()),
				slog.String("dedupe_key", item.DedupeKey))
			kept = stored.Sequence
			continue
		}
		forward := item
		forward.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
		keptAt := stored.Time.UnixMilli()
		forward.KeptAt = &keptAt
		if err := forwardKeptRoleMessage(cfg, subject, forward, storedOnly); err != nil {
			walk = fmt.Errorf("forward role %s's kept message %s to %s: %w", role, item.EventID, holder, err)
			break
		}
		cfg.logger.DeliveryLog(slog.LevelInfo, "listener forwarded a kept role message to the role's new holder",
			holder, item.Topic, item.EventID, "delivered",
			slog.String("role", role),
			slog.String("dedupe_key", forward.DedupeKey),
			slog.Int64("kept_at", keptAt))
		kept = stored.Sequence
	}
	if kept == from {
		return walk
	}
	body, err := json.Marshal(roleKeepCursor{Kept: kept})
	if err != nil {
		return errors.Join(walk, fmt.Errorf("encode role %s's kept-lane cursor: %w", role, err))
	}
	if _, err := cfg.client.PublishCursorAt(ctx, cursorSubject, body, cursorSeq); err != nil {
		if errors.Is(err, bus.ErrCursorMoved) {
			cfg.logger.LogEvent(slog.LevelDebug, "listener kept role drain lost the cursor to another listener",
				slog.String("role", role),
				slog.String("session_id", holder))
			return walk
		}
		return errors.Join(walk, fmt.Errorf("advance role %s's kept-lane cursor to %d: %w", role, kept, err))
	}
	return walk
}

// readRoleKeepCursor reads a role's drain cursor and returns both numbers the drain needs: the
// cursor message's own stream sequence, which its compare-and-swap write must name, and the kept
// sequence its body names, where the walk starts. A role whose lane has never been drained holds
// no cursor (nats.ErrMsgNotFound), and both are 0.
func readRoleKeepCursor(ctx context.Context, client *bus.Client, subject string) (cursorSeq, kept uint64, err error) {
	stored, err := client.LastStoredMessage(ctx, subject)
	if errors.Is(err, nats.ErrMsgNotFound) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, err
	}
	var cursor roleKeepCursor
	if err := json.Unmarshal(stored.Data, &cursor); err != nil {
		return 0, 0, fmt.Errorf("decode the cursor stored at %d: %w", stored.Sequence, err)
	}
	return stored.Sequence, cursor.Kept, nil
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
