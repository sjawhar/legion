package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/roles"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// roleForwardDedupePrefix marks an envelope already sent from the role arbiter, or the publish
// route, to its selected agent subject (contracts.RoleForwardDedupePrefix). A role lane that takes
// one again (an operator re-publishing a received frame) does not re-enter the arbiter with it,
// and a session's dedupe reads it as a forward. The durable acknowledges the stored forward copy
// by its subject (listenerDeliveryHandler) for every holder that has it another way; a holder on
// consumer delivery is relayed its own consumer's copy (sessionRelays).
const roleForwardDedupePrefix = contracts.RoleForwardDedupePrefix

const roleReceiptTimeout = 2 * time.Second

// exceptionPublishFailedLine is the line a delivery logs when its delivery exception could not be
// published. The deployed publish-failure metric filter, which the deployment repository's
// listener infrastructure defines, matches it exactly, so every site that logs it uses this constant.
const exceptionPublishFailedLine = "listener exception publish failed"

// ledgerUnavailableLine is the WARN a delivery logs when the delivery ledger could not be read or
// written: the durable lane then retries the message, while a core lane, and an ask made only for
// a record worth having (ledgerAsk.bestEffort), deliver without the ledger.
const ledgerUnavailableLine = "delivery ledger unavailable"

// durableRetryDelay is how long the durable waits before it redelivers a message the listener
// NAKed for a retry.
const durableRetryDelay = 30 * time.Second

func shouldNAKFanoutDelivery(sessionLive bool, err error) bool {
	return err != nil && sessionLive
}

type listenerDeliveryHandlerConfig struct {
	client *bus.Client
	// wakes publishes the wake reports an unplaceable agent delivery raises, off the delivery
	// path and under the listener's own lifetime (wakeReporter); nil in a test that builds the
	// handlers without it, which reports no wake.
	wakes *wakeReporter
	// forwardRole publishes a role-lane envelope to the holder's agent subject under msgID, its
	// Nats-Msg-Id when not empty, and returns the wait for its outcome; main.go wires
	// startRoleForward(client). The wait's bus.ErrReceiptTimeout means the forward was flushed to
	// the server and no receipt arrived inside timeout; bus.ErrForwardNotStored that the stream
	// did not acknowledge storing it; any other error — a raw nats.ErrTimeout from the flush
	// included, returned by the publish or the wait — means the forward is not known to have
	// reached the holder.
	// storedOnly asks for a forward that counts as delivered on the stream's acknowledgement
	// alone, with no receipt (roleForwardStoredOnly): an unresponsive holder answers none while it
	// is stalled, and reads the stored copy back on its return.
	forwardRole func(subject string, item contracts.Envelope, msgID string, timeout time.Duration, storedOnly bool) (wait func() error, err error)
	// roleForwards runs the role lane's forwards off its handler (roleForwarder); every delivery
	// path hands its forwards to it, so it is always set: wireRoleForwarder builds it from the
	// configuration it reports through, once that configuration is otherwise built, and main.go
	// and every test alike wire it there.
	roleForwards *roleForwarder
	registry     *store.Registry
	sessions     *session.SessionRegistry
	machineID    string
	// ledger is the delivery ledger every lane asks before it forwards a role-lane message to a
	// session, reports a refusal of one, or gives an unplaceable delivery up: the machine's
	// shared one (dedupe.KV), or, without the listener state bucket, this process's own
	// (dedupe.Memory). Every pod of the machine reads and writes the shared one, so a pod that
	// takes delivery over neither forwards again what the pod before it delivered nor reports a
	// refusal it already reported.
	ledger dedupe.Ledger
	// ledgerReconnected waits, for at most ledgerReconnectWait and while ctx lasts, until the
	// ledger's own NATS connection is connected, and reports whether it is; nil for a ledger that
	// needs no connection (dedupe.Memory). The durable lane asks it before it retries a message
	// whose ledger call failed, so a call that failed in a reconnect's moment is made again at
	// once rather than costing durableRetryDelay.
	ledgerReconnected func(ctx context.Context) bool
	logger            *logging.Logger
	messagesReceived  *metrics.Counter
	messagesDelivered *metrics.Counter
	messagesNAKed     *metrics.Counter
	// fanoutUnresponsiveSkips counts the topic fan-outs this listener skipped because the session
	// that registered the interest is past the live window (fanoutDelivery), and the ad-hoc topic
	// sends whose receipt it did not wait for, for the same reason (confirmTopicRecipient): that
	// send is forwarded and its copy stored, only unconfirmed. Neither is a failed delivery: the
	// count says how much of a box's traffic is going to sessions that have stalled, every
	// message of it kept for the session to read when it answers again.
	fanoutUnresponsiveSkips *metrics.Counter
	deliveryDuration        *metrics.Histogram
	// roleKeptExpired counts the kept role messages a claim's drain dropped unforwarded because
	// the stream had held them longer than roleKeepWindow (envoy_role_kept_expired_total,
	// drainKeptRole). A reading that climbs says roles are being claimed long after the messages
	// meant for them were sent, which is a routing problem rather than a delivery one.
	roleKeptExpired *metrics.Counter
	// unplaceableRetryWindow is how long an agent delivery this listener cannot place is held
	// in the stream and redelivered before it is given up with a record of its own
	// (agentSubjectDelivery). main.go reads it from ENVOY_UNPLACEABLE_RETRY_WINDOW, defaulting
	// to defaultUnplaceableRetryWindow. It is spent as whole deferrals of unplaceableRetryDelay
	// each, counted from the message's first delivery, so a window shorter than one delay - a
	// zero one, what a caller that does not set this field asks for - retries nothing: every
	// unplaceable delivery is given up on its first attempt.
	unplaceableRetryWindow time.Duration
	// deferredUnplaceable is the set of agent deliveries this listener is holding deferred right
	// now, the budget unplaceableDeferralCap bounds. It is required: main.go makes one and sets
	// it once, in its deliveryConfig, so every handler built from that config - the role lane's
	// core handler and the durable's, which the receive loop keeps across every
	// resubscribe - draws on the one budget. listenerDeliveryHandler refuses a config without one
	// rather than drawing a private set, since a set per handler would be a second budget beside
	// the first. A test that builds a config sets its own.
	deferredUnplaceable *unplaceableDeferrals
	// roleKeepDrains holds one guard per role, so two drains of one role inside this listener do
	// not walk that role's kept lane together and read the same copy twice (drainKeptRole).
	// Across listeners the guard is the kept lane's cursor: each drain claims a copy by advancing
	// the cursor under a compare-and-swap before it forwards it, and the loser of that claim stops
	// (bus.ErrCursorMoved, roleKeepLaneCursor.write). It is required, as deferredUnplaceable is:
	// main.go makes one and sets it once, so every handler and every route built from that config
	// guards one set of roles. A test that builds a config sets its own.
	roleKeepDrains *roleKeepGuards
}

// wireRoleForwarder builds the role forwarder every delivery path hands its forwards to and
// returns cfg carrying it. The forwarder keeps the configuration it is built from by value, so a
// caller wires it last, once every seam of cfg is set: a seam set after this is one the forwarder
// does not have. It has no loop to stop; the role lane's drain waits for the forwards it holds
// (roleLane.drain).
func wireRoleForwarder(cfg listenerDeliveryHandlerConfig) listenerDeliveryHandlerConfig {
	cfg.roleForwards = newRoleForwarder(cfg)
	return cfg
}

type deliveryMode uint8

const (
	coreDelivery deliveryMode = iota
	durableDelivery
)

type deliveryMessage struct {
	data []byte
	mode deliveryMode
	// msg is the NATS message: set for durableDelivery, which acknowledges it and reads its
	// subject.
	msg *nats.Msg
	// ctx is a durable message's handling context (durableReceiver): the receiver cancels it when
	// a shutdown's in-flight grace runs out, which aborts a delivery in flight. Nil on the core lanes.
	ctx context.Context
	// metadata is a durable message's JetStream metadata, read once by its handler: its stream
	// sequence, which says which session consumers hold it (storedSeq) and keys an unplaceable
	// deferral, and its delivery count, which bounds that retry. It is nil for a core
	// message, and for a durable one whose metadata will not parse, which can be bounded by
	// nothing (unplaceableAgentDelivery) and is in no consumer's window as far as this listener
	// can tell (storedSeq 0).
	metadata *nats.MsgMetadata
}

// cancelled reports whether the receiver cut this message's handling short. Whatever the
// handling had reached is then a retryable failure: the next holder of the lease delivers it.
func (message deliveryMessage) cancelled() bool {
	return message.ctx != nil && message.ctx.Err() != nil
}

// storedSeq is the stream sequence of a durable message, which decides whether a session's
// consumer holds it (session.SessionEntry.ConsumerHoldsStored): 0 for a core message, and
// for a durable one whose metadata will not parse, so such a message is delivered rather than left
// to a consumer that may not hold it.
func (message deliveryMessage) storedSeq() uint64 {
	if message.metadata == nil {
		return 0
	}
	return message.metadata.Sequence.Stream
}

// finalize makes core delivery's loss-on-failure semantics explicit: only a
// durable JetStream message receives an acknowledgement or retry request. A role-lane message is
// core NATS, sent once to whoever was subscribed, so nothing acknowledges or retries it.
// A durable retry waits durableRetryDelay (30 s) before redelivery; a path whose own bound sets
// the pace of its retries asks for its delay through retryAfter.
func (message deliveryMessage) finalize(retry bool) {
	if retry {
		message.retryAfter(durableRetryDelay)
		return
	}
	if message.mode != durableDelivery {
		return
	}
	_ = message.msg.Ack()
}

// retryAfter asks the stream to redeliver this durable message after delay, for a path whose own
// bound sets the pace of its retries rather than finalize's standard one, and at once when the
// receiver cancelled the message's handling on a shutdown, so the pod that takes the lease next
// delivers it first rather than after the messages behind it. Like finalize, it does nothing to a
// core message: nothing redelivers one, so there is no request to make.
func (message deliveryMessage) retryAfter(delay time.Duration) {
	if message.mode != durableDelivery {
		return
	}
	if message.cancelled() {
		delay = 0
	}
	_ = message.msg.NakWithDelay(delay)
}

// finish ends the handling of a message nothing more is delivered for: a retry after retryIn when
// a recipient asked for one (counted as a NAK), else an ack.
func (message deliveryMessage) finish(cfg listenerDeliveryHandlerConfig, retryIn time.Duration) {
	if retryIn > 0 {
		cfg.messagesNAKed.Inc()
		message.retryAfter(retryIn)
	} else {
		message.finalize(false)
	}
}

// durableHandler handles one message of the listener's durable under ctx, the message's handling
// context, and settles it: an ack, or a NAK for a retry.
type durableHandler func(ctx context.Context, msg *nats.Msg)

// jetStreamDeliveryHandler is the durable's handler. It reads the message's JetStream metadata
// once, for the paths that bound themselves by its delivery count and for the stream sequence that
// says which session consumers hold it. A message no session needs from the durable
// (durableNeedsDelivery) is acknowledged before it is decoded, each session that takes it another
// way counted as a skipped delivery, as the delivery paths count it.
func jetStreamDeliveryHandler(cfg listenerDeliveryHandlerConfig) durableHandler {
	handler := listenerDeliveryHandler(cfg)
	return func(ctx context.Context, msg *nats.Msg) {
		message := deliveryMessage{data: msg.Data, mode: durableDelivery, msg: msg, ctx: ctx}
		if metadata, err := msg.Metadata(); err == nil {
			message.metadata = metadata
		}
		needed, skipped := durableNeedsDelivery(cfg, msg.Subject, message.storedSeq())
		if !needed {
			for range skipped {
				cfg.messagesDelivered.Inc([2]string{"delivery_status", "skipped"})
			}
			_ = msg.Ack()
			return
		}
		handler(message)
	}
}

func coreNATSDeliveryHandler(cfg listenerDeliveryHandlerConfig) nats.MsgHandler {
	handler := listenerDeliveryHandler(cfg)
	return func(msg *nats.Msg) {
		handler(deliveryMessage{data: msg.Data, mode: coreDelivery})
	}
}

// clearLedgerAttempt gives attempt up so a retry delivers its pair again. A clear the ledger
// cannot write leaves the attempted record behind, which this process's next attempt for the pair
// takes over, and logs that.
func clearLedgerAttempt(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, attempt dedupe.Attempt) {
	if err := cfg.ledger.Clear(attempt); err != nil {
		cfg.logger.Warn(ledgerUnavailableLine, slog.String("operation", "clear"), slog.String("topic", item.Topic), slog.String("event_id", item.EventID), slog.String("error", err.Error()))
	}
}

// ledgerAsk is what one caller wants of the delivery ledger (ledgerAttempt).
type ledgerAsk struct {
	// role picks the role lane's skip line, `listener role dedupe skip`, for every skip.
	role bool
	// bestEffort asks for a copy this listener hands to nobody: one a self-subscribed recipient
	// reads on its own NATS subscription. The record is worth writing - it keeps the next copy
	// of the same event from being counted a second delivery - but never worth holding the
	// message for, so such an ask neither defers nor retries it, whichever lane it is on: the
	// ledger's answer is taken when it comes, and a ledger that cannot answer is passed over.
	bestEffort bool
}

// ledgerAttempt asks the delivery ledger for (item, sessionID) before the listener forwards the
// role-lane message to it or counts the copy it reads on its own subscription, and logs and
// counts any skip. It returns the attempt the delivery settles and deliver=true when the
// listener goes on to deliver; otherwise retryIn > 0 asks the durable lane to retry the message
// after it, and zero to ack it.
//
//   - Delivered (already delivered) and InFlight (this process's attempt whose delivery may still
//     be running, or one a delivery kept) skip the pair, and the message is acked.
//   - Deferred (another pod's attempt, which may still be delivering) is retried on the durable lane
//     once that attempt can be taken over, and skipped on a core lane, which retries nothing.
//   - A ledger that cannot be read retries the message on the durable lane after durableRetryDelay;
//     a core lane delivers without the ledger, under the zero attempt, which settles nothing. The
//     durable lane first waits for the ledger's connection (ledgerReconnected) and asks once more,
//     so a call that failed while that connection was reconnecting costs no 30 s.
//
// A best-effort ask (ledgerAsk.bestEffort) is answered as a core lane's is on either lane: it
// delays and retries nothing.
func ledgerAttempt(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string, ask ledgerAsk) (attempt dedupe.Attempt, deliver bool, retryIn time.Duration) {
	durable := message.mode == durableDelivery && !ask.bestEffort
	attempt, err := cfg.ledger.Attempt(item.DedupeKey, sessionID)
	// Only the durable lane waits for the ledger's connection and asks again: every other ask
	// retries nothing, so it delivers under the zero attempt rather than waiting. The wait runs
	// on the message's own context, or the background one for a message that carries none.
	if err != nil && durable && cfg.ledgerReconnected != nil {
		ctx := message.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		if cfg.ledgerReconnected(ctx) {
			first := err
			if attempt, err = cfg.ledger.Attempt(item.DedupeKey, sessionID); err == nil {
				cfg.logger.Info("delivery ledger answered once its connection was back", slog.String("session_id", sessionID), slog.String("event_id", item.EventID), slog.String("first_error", first.Error()))
			}
		}
	}
	if err != nil {
		cfg.logger.Warn(ledgerUnavailableLine, slog.String("operation", "attempt"), slog.String("session_id", sessionID), slog.String("topic", item.Topic), slog.String("event_id", item.EventID), slog.Bool("retry", durable), slog.String("error", err.Error()))
		if durable {
			return dedupe.Attempt{}, false, durableRetryDelay
		}
		return dedupe.Attempt{}, true, 0
	}
	if attempt.Verdict == dedupe.Proceed {
		return attempt, true, 0
	}
	metricStatus, line, status := "dedupe", "listener dedupe skip", "dedupe"
	switch attempt.Verdict {
	case dedupe.InFlight:
		line, status = "listener attempt-dedupe skip", "skipped"
	case dedupe.Deferred:
		metricStatus, line, status = "deferred", "listener delivery deferred to another pod's attempt", "deferred"
		if durable {
			retryIn = attempt.RetryIn
		}
	}
	if ask.role {
		line = "listener role dedupe skip"
	}
	applyDeliveryOutcome(cfg, item, deliveryOutcome{
		sessionID:    sessionID,
		metricStatus: metricStatus,
		log: func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelInfo, line, sessionID, item.Topic, item.EventID, status, slog.String("dedupe_key", item.DedupeKey), slog.String("ledger", attempt.Verdict.String()))
		},
	})
	return attempt, false, retryIn
}

// listenerDeliveryHandler decodes and validates the envelope (decodeStoredEnvelope, the rules the
// relay of a session's consumer reads a stored message under too), then routes it to exactly one
// of the three delivery paths based on its topic: a role lane (arbitrated to whichever session
// currently holds the role), a direct agent subject, or the general interest fanout.
//
// cfg arrives by value, so every handler built from one config shares the deferral set the
// unplaceable retry bounds itself with, and the guards one role's drains take, only because each
// field is a pointer; a config without either is a programming error, refused here at
// construction rather than at the first deferral or the first drain.
func listenerDeliveryHandler(cfg listenerDeliveryHandlerConfig) func(deliveryMessage) {
	if cfg.deferredUnplaceable == nil {
		panic("listener delivery handler built without its deferral set (deferredUnplaceable)")
	}
	if cfg.roleKeepDrains == nil {
		panic("listener delivery handler built without its kept-lane drain guards (roleKeepDrains)")
	}
	return func(message deliveryMessage) {
		var item contracts.Envelope
		if err := decodeStoredEnvelope(message.data, &item); err != nil {
			cfg.logger.Error("listener decode failed", slog.String("error", err.Error()))
			message.finalize(false)
			return
		}
		// The stream captures every agent subject, so it also stores each forward a sender made to
		// a session's agent subject while keeping the envelope's own topic: a role message the
		// lane or the publish route forwarded to its holder, an ad-hoc topic message the route
		// forwarded to each self-subscribed recipient. The forward reached its session over core
		// NATS, or its sender learned it did not; the stored copy is not a message for this
		// listener to deliver again.
		if message.mode == durableDelivery && strings.HasPrefix(message.msg.Subject, contracts.AgentTopicPrefix) && item.Topic != message.msg.Subject {
			cfg.logger.DeliveryLog(slog.LevelDebug, "listener acknowledged a stored forward copy", strings.TrimPrefix(message.msg.Subject, contracts.AgentTopicPrefix), item.Topic, item.EventID, "forward_copy", slog.String("subject", message.msg.Subject))
			message.finalize(false)
			return
		}
		cfg.messagesReceived.Inc([2]string{"source", item.Source}, [2]string{"topic_prefix", metrics.TopicPrefix(item.Topic)})
		cfg.logger.Info("listener received", slog.String("source", item.Source), slog.String("source_session", item.SourceSession), slog.String("topic", item.Topic), slog.String("event_id", item.EventID), slog.String("payload_summary", item.PayloadSummary))
		switch {
		case strings.HasPrefix(item.Topic, contracts.RoleTopicPrefix):
			roleTopicDelivery(cfg, message, item)
		case strings.HasPrefix(item.Topic, contracts.AgentTopicPrefix):
			agentSubjectDelivery(cfg, message, item)
		default:
			fanoutDelivery(cfg, message, item)
		}
	}
}

// resolveCoreRoleHolder resolves the current live holder for a role-lane delivery
// (roles.ResolveHolderFrom), applying the terminal no_holder/delivery_failed outcome itself and
// reporting ok=false when there is nobody to deliver to. A resolution that found no live holder
// keeps the message for the role's next holder first (keepRoleLaneMessage), in one decision for
// both give-up branches: roleKeepable says which states a copy is kept for - an unclaimed role,
// since no resolution of this build releases a claim - and a copy that was kept publishes no
// delivery exception, since the message is not lost. Every other case leaves its branch exactly
// as it was before the kept lane: an unclaimed role is no_holder; a holder that is not answering
// and every failed read are delivery_failed, and so is a keep that failed, each under a log line
// that names which it was. A failed read keeps nothing, since a read that did not answer is no
// evidence the role has no holder. A holder that is not answering keeps its claim through all of
// them: the lane takes nothing from it. Role delivery answers no caller, so it resolves under
// context.Background(): the holder's session read keeps roleHolderReadTimeout
// (roleHolderSessions.Holder's bound) as its own cap. Its claim read is the registry's watched
// copy of the role bucket (store.Registry.CachedRoleHolder, passed as the resolution's
// firstHolder), so a role message costs no bucket round trip.
func resolveCoreRoleHolder(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, role string) (string, session.SessionEntry, bool) {
	resolved, err := roles.ResolveHolderFrom(context.Background(), cfg.registry, cfg.registry.CachedRoleHolder, roleHolderSessions{cfg.sessions}, role)
	sessionID := resolved.Holder
	// Every error the resolution returns is a *roles.ReadError naming the read that failed.
	// One that is not would still be a read that failed, so it is logged as the claim read with
	// its own error rather than left without a step to branch on.
	var failure *roles.ReadError
	if err != nil && !errors.As(err, &failure) {
		failure = &roles.ReadError{Step: roles.ClaimRead, Err: err}
	}
	if err == nil {
		if resolved.State == roles.Live {
			return sessionID, resolved.Entry, true
		}
		if roleKeepable(resolved) && keepRoleLaneMessage(cfg, role, sessionID, item, resolved.State) {
			return "", session.SessionEntry{}, false
		}
		if resolved.State == roles.Unclaimed {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				metricStatus: "skipped",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelWarn, "listener role has no holder", "", item.Topic, item.EventID, "skipped")
				},
				exceptionReason: "no_holder",
			})
			return "", session.SessionEntry{}, false
		}
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role holder is not live", sessionID, item.Topic, item.EventID, "failed")
			},
			exceptionReason: "delivery_failed",
		})
		return "", session.SessionEntry{}, false
	}
	switch failure.Step {
	case roles.ClaimRead:
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.Error("listener role holder lookup failed", slog.String("role", role), slog.String("error", failure.Err.Error()))
			},
			exceptionReason: "delivery_failed",
		})
	case roles.SessionRead:
		// The holder's record could not be read, so nothing is known about its state and nothing
		// is taken from it.
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.Error("listener role holder session lookup failed", slog.String("role", role), slog.String("session_id", sessionID), slog.String("error", failure.Err.Error()))
			},
			exceptionReason: "delivery_failed",
		})
	}
	return "", session.SessionEntry{}, false
}

// roleTopicDelivery arbitrates a role-lane envelope to whichever session
// currently holds the role, forwarding it over core NATS request-reply so
// the sender learns immediately whether the holder received it. An echo of
// the holder's own message is disposed of first, with no forward and no
// exception (beforeForward's own first rule). Then comes the delivery-ledger
// skip (ledgerAttempt): a duplicate of an envelope already forwarded, or
// already being forwarded by this pod or another of the machine's, is
// disposed of the same way. Only after it is the holder judged by the
// payload's delivery mode (beforeForward's deliveryModeVerdict, the rules the
// publish route applies too): a holder that does not advertise the envelope's
// own targeted delivery mode, or whose mode cannot be read unambiguously, is
// refused like a stale holder. Role forwarding needs that guard of its own
// because it never goes through /v1/messages/send, and the ordering matters:
// guarding before the ledger check would re-evaluate an already-delivered
// duplicate against the holder's *current* capabilities, which can have
// changed since the original successful forward, and misreport a delivered
// message as delivery_failed -- a false signal the Legion daemon treats as
// cause to probe and potentially resume the worker. Every branch ACKs: role
// lanes have no durable transit to retry against, so a failed forward is
// reported via a delivery exception instead of a NAK. The forward itself is
// handed to the role forwarder (roleForwarder.enqueue) and its outcome
// reported when it ends (finishRoleForward), so the role lane's handler
// returns without waiting for the holder.
func roleTopicDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	// An envelope the arbiter, or the publish route, already sent to its holder's agent subject
	// (roleForwardDedupePrefix) is not arbitrated again: the holder has it, and a lane that takes
	// the frame once more - an operator re-publishing it - disposes of it here.
	if strings.HasPrefix(item.DedupeKey, roleForwardDedupePrefix) {
		message.finalize(false)
		return
	}
	role := strings.TrimPrefix(item.Topic, contracts.RoleTopicPrefix)
	sessionID, holder, ok := resolveCoreRoleHolder(cfg, item, role)
	if !ok {
		message.finalize(false)
		return
	}
	// The repeat is the delivery ledger's to report (ledgerAttempt below), not a cache's, so the
	// record beforeForward reads answers no here and its verdict is the echo's or the mode's.
	verdict := beforeForward(item, sessionID, holder, func(string, string) bool { return false })
	if verdict.kind == forwardEcho {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "skipped",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener skip role echo", sessionID, item.Topic, item.EventID, "skipped")
			},
		})
		message.finalize(false)
		return
	}
	attempt, deliver, _ := ledgerAttempt(cfg, message, item, sessionID, ledgerAsk{role: true})
	if !deliver {
		message.finalize(false)
		return
	}
	if verdict.kind != forwardAllowed {
		// The echo is disposed of above and the repeat is the ledger's, so what is left is a
		// holder whose delivery modes refuse the payload, or whose modes cannot be read: both
		// are failed, counted and answered as every forward path answers them
		// (refusedRoleForwardOutcome), and both give this lane's attempt up so a re-send is
		// judged against the holder of the moment.
		outcome := refusedRoleForwardOutcome(verdict, item, sessionID, roleLaneRefusalLines)
		outcome.attempt, outcome.settle = attempt, settleClear
		applyDeliveryOutcome(cfg, item, outcome)
		message.finalize(false)
		return
	}
	cfg.roleForwards.enqueue(roleForward{item: item, sessionID: sessionID, attempt: attempt})
	message.finalize(false)
}

// roleForwardRefusalLines are one forward path's lines for each refusal beforeForward can return.
// The role lane and the kept lane's drain each have their own wording - a copy the drain passed
// over is not the message the lane just took - and nothing else about a refusal differs between
// them, so each passes its lines to the one judgement both make (refusedRoleForwardOutcome).
type roleForwardRefusalLines struct {
	echo             string
	repeat           string
	modeUnreadable   string
	modeUnadvertised string
}

// roleLaneRefusalLines are the role lane's lines for a forward beforeForward refused.
var roleLaneRefusalLines = roleForwardRefusalLines{
	echo:             "listener skip role echo",
	repeat:           "listener role dedupe skip",
	modeUnreadable:   "listener role delivery mode cannot be read unambiguously",
	modeUnadvertised: "listener role holder does not advertise delivery mode",
}

// refusedRoleForwardOutcome is what a forward to a role's holder that beforeForward refused is
// owed, for either path that makes one: the role lane (roleTopicDelivery) and the kept lane's
// drain (keptRoleForwardAllowed). An echo of the holder's own message is skipped and a copy it
// already took is a dedupe, each counted and logged and neither reported to its sender; a payload
// its delivery modes do not take, and one whose mode cannot be read, are failed, counted and
// answered with the delivery_failed a stale holder is answered with. extra is what the path adds
// to every line of its own, the drain's role among them. How the path settles its ledger attempt
// is the path's own (deliveryOutcome.settle): the lane gives the attempt it holds up, where the
// drain's judgement is made before any attempt is taken.
func refusedRoleForwardOutcome(verdict forwardVerdict, item contracts.Envelope, sessionID string, lines roleForwardRefusalLines, extra ...slog.Attr) deliveryOutcome {
	outcome := deliveryOutcome{sessionID: sessionID}
	line, level, attrs := "", slog.LevelWarn, extra
	switch verdict.kind {
	case forwardEcho:
		line, level, outcome.metricStatus = lines.echo, slog.LevelInfo, "skipped"
	case forwardRepeat:
		line, level, outcome.metricStatus = lines.repeat, slog.LevelInfo, "dedupe"
		attrs = append(attrs, slog.String("dedupe_key", item.DedupeKey))
	case forwardModeUnreadable:
		line, outcome.metricStatus, outcome.exceptionReason = lines.modeUnreadable, "failed", "delivery_failed"
	case forwardModeUnadvertised:
		line, outcome.metricStatus, outcome.exceptionReason = lines.modeUnadvertised, "failed", "delivery_failed"
		attrs = append(attrs, slog.String("mode", verdict.mode))
	}
	status := outcome.metricStatus
	outcome.log = func(logger *logging.Logger) {
		logger.DeliveryLog(level, line, sessionID, item.Topic, item.EventID, status, attrs...)
	}
	return outcome
}

// finishRoleForward reports a role forward's outcome, err from its publish or its wait, with attrs
// on its line: the kept lane's drain names the role and the time it kept the copy there, where the
// role lane's own forward adds nothing. A forward that reached the server and drew no receipt from
// the live holder inside roleReceiptTimeout (bus.ErrReceiptTimeout, the one error keyed on) is
// receipt_timeout; one the stream did not acknowledge storing (bus.ErrForwardNotStored), or not
// known to have left this process -- the flush timed out or failed, the publish failed -- is
// delivery_failed like a stale holder or an unadvertised/unreadable delivery mode; no claim is
// no_holder. Either failure gives the forward's ledger attempt up, so a re-send is forwarded again
// by whichever pod of the machine takes it; a delivered forward records the pair in the ledger, so
// no pod forwards it again.
func finishRoleForward(cfg listenerDeliveryHandlerConfig, forward roleForward, err error, attrs ...slog.Attr) {
	item, sessionID := forward.item, forward.sessionID
	switch {
	case err == nil:
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			attempt:      forward.attempt,
			metricStatus: "delivered",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener role forwarded", sessionID, item.Topic, item.EventID, "delivered", attrs...)
			},
			settle: settleRecord,
		})
	case errors.Is(err, bus.ErrReceiptTimeout):
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			attempt:      forward.attempt,
			metricStatus: "receipt_timeout",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role receipt timed out", sessionID, item.Topic, item.EventID, "receipt_timeout", append(attrs, slog.String("timeout", roleReceiptTimeout.String()))...)
			},
			exceptionReason: "receipt_timeout",
			settle:          settleClear,
		})
	default:
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			attempt:      forward.attempt,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelError, "listener role forward failed", sessionID, item.Topic, item.EventID, "failed", append(attrs, slog.String("error", err.Error()))...)
			},
			exceptionReason: "delivery_failed",
			settle:          settleClear,
		})
	}
}

// deliverySessionReadTimeout is delivery's own bound on its read of the target session from the
// bucket. It is the role paths' bound (roleHolderReadTimeout, role_holder.go) today without being
// tied to it, and it can be looser than the send route's (sendSessionReadTimeout) because no HTTP
// caller is waiting on this read: the cost of waiting is a message held, not a request kept open,
// and the alternative - taking the cache's miss for an answer - ACKs the message away.
const deliverySessionReadTimeout = 2 * time.Second

// agentSubjectDelivery delivers to the session named by the topic
// (notifications.agent.<session>), which takes the message from its own consumer or off its own
// subscription as session.HandleAgentMessage decides. A session another listener owns is that
// listener's to deliver, so this one acknowledges the message silently; a session that records no
// way of receiving at all is held and retried, then given up (unplaceableAgentDelivery).
//
// A delivery this listener answers for asks the delivery ledger before it applies its outcome:
// the pair is attempted, and a delivery that lands records it delivered, so a second copy of the
// same message is a dedupe skip rather than a second delivery, on this pod or the one that takes
// delivery over, and a message already reported or given up for this session is skipped rather
// than reported a second time. The two routes this listener hands nothing to are settled before
// that ask and cost no ledger write at all: a message the relay of the session's own consumer
// delivers, and one a session another listener owns. A record of either would say this listener
// delivered a message it handed to nobody, and the relay, which asks the ledger for nothing,
// would be judged by it.
//
// The copy a self-subscribed session reads on its own NATS subscription is asked of the ledger
// too, and recorded, since a second copy of that event is the session's to take once and not
// twice; but the ask is best-effort (ledgerAsk.bestEffort), because the listener hands such a
// copy to nobody: a ledger that cannot answer leaves the copy counted skipped, with no record
// and no retry, rather than holding the message back for 30 s at a time. The fan-out follows
// the same rule (fanoutDelivery).
func agentSubjectDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	sessionID := strings.TrimPrefix(item.Topic, contracts.AgentTopicPrefix)
	// The JetStream metadata the durable's handler read carries everything unplaceableAgentDelivery
	// bounds this message by - its delivery count, against the deferrals the window buys and
	// against unplaceableMaxDeliveries - and its stream sequence, the key of the slot a deferral
	// of it holds. A core delivery has none, and a durable message whose metadata would not parse
	// can be bounded by nothing, which unplaceableAgentDelivery gives up on rather than retrying
	// without end.
	metadata := message.metadata
	// A message this listener deferred has left the deferred state by the time it is delivered
	// again, so its slot goes back before any disposition below takes one: the delivery itself is
	// what freed it, not what this attempt decides to do with the message. The release is keyed
	// on the message, so a delivery this listener never deferred - a first one, or a redelivery
	// another branch's NAK or an expired AckWait produced - gives nothing back.
	if metadata != nil {
		cfg.deferredUnplaceable.release(metadata.Sequence.Stream)
	}
	interest, err := cfg.registry.Get(context.Background(), sessionID)
	var interestPtr *store.Interest
	if err == nil {
		interestPtr = &interest
	}
	// The session record delivery acts on is read from the session bucket itself, as the
	// send route reads it: this listener's cache trails the bucket it follows, and is still being
	// refilled after its bus connection came back, so a session registered a moment ago, or one
	// this listener has not re-read since the reconnect, is in the bucket before it is in the
	// cache - and a message dropped on that miss is gone, since it is already ACKed away.
	// nats.ErrKeyNotFound is the bucket's own word that no record exists; any other error is a
	// read that did not answer, which is no evidence of absence at all - the bucket was never
	// consulted, so nothing about this session was learned, and an attempt that learned nothing
	// can never be the ground for giving a message up. Neither needs to be told from the other
	// below: both leave this delivery with no entry to deliver through, and an attempt with no
	// entry is held and retried for the whole window before it is given up
	// (unplaceableAgentDelivery), while the interest path's own failures NAK with an error
	// instead and never reach that branch.
	entry, _, entryErr := liveSessionEntry(context.Background(), cfg.sessions, sessionID, deliverySessionReadTimeout, notGone)
	var entryPtr *session.SessionEntry
	if entryErr == nil {
		entryPtr = &entry
	}
	deliveryTimer := metrics.NewTimer()
	// The durable path's message carries the stream sequence that says whether the session's
	// consumer already holds it, and the core path's message is in no consumer (storedSeq 0).
	result := session.HandleAgentMessage(item, sessionID, cfg.machineID, interestPtr, entryPtr, message.storedSeq())
	if result.Relayed {
		// The session's own consumer holds this message and the relay of that consumer delivers it
		// (session.SessionEntry.ConsumerRelaysStored), as on the fan-out path below. Nothing is
		// recorded: this path handed the message to nobody.
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "skipped",
			duration:     &deliveryTimer,
		})
		message.finalize(false)
		return
	}
	if result.OwnedElsewhere {
		// Every listener on the bus reads each inbox message from its own durable, and the owner
		// delivers it from its copy: a no_holder exception here would report a delivered message
		// as undeliverable, once per listener that does not own the session.
		cfg.logger.DeliveryLog(slog.LevelDebug, "listener agent session owned by another listener", sessionID, item.Topic, item.EventID, "owned_elsewhere")
		message.finalize(false)
		return
	}
	// Every route left reaches the session through this listener or not at all, so the ledger is
	// asked here, before the outcome below is applied: a second copy of a message this session
	// has taken is a dedupe skip against the record the first wrote, and a redelivery a lost ACK
	// brings, or a copy another pod reads, is skipped rather than reported or given up a second
	// time. The one route left that this listener hands nothing to - the session reads the copy
	// on its own subscription - asks best-effort, so a ledger that cannot answer costs that
	// copy nothing.
	attempt, deliver, retryIn := ledgerAttempt(cfg, message, item, sessionID, ledgerAsk{bestEffort: result.Delivered && result.Skipped})
	if !deliver {
		message.finish(cfg, retryIn)
		return
	}
	if result.Delivered {
		deliveryStatus := "delivered"
		deliveryLog := "listener agent delivered"
		if result.Skipped {
			// The session holds its own NATS subscription and the message reaches it there
			// (session.Resolve): this listener handed it to nobody, but the message did reach
			// the session, so the pair is recorded delivered and the next copy of it is skipped.
			deliveryStatus = "skipped"
			deliveryLog = "listener agent session takes the message off its own subscription"
		}
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			metricStatus: deliveryStatus,
			duration:     &deliveryTimer,
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, deliveryLog, sessionID, item.Topic, item.EventID, deliveryStatus)
			},
			settle: settleRecord,
		})
		message.finalize(false)
		return
	}
	if message.cancelled() {
		// The receiver cut this message's handling short on a shutdown: neither a failed delivery
		// to report nor a session found nowhere, but a retry for whichever pod holds the lease
		// next. The attempt taken above is given up, so the pod that takes the lease delivers the
		// message rather than skipping it as one already on its way.
		cfg.logger.DeliveryLog(slog.LevelWarn, "listener agent delivery cancelled at shutdown", sessionID, item.Topic, item.EventID, "retry")
		clearLedgerAttempt(cfg, item, attempt)
		cfg.messagesNAKed.Inc()
		message.finalize(true)
		return
	}
	if result.Err != nil {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			metricStatus: "failed",
			duration:     &deliveryTimer,
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelError, "listener agent delivery failed", sessionID, item.Topic, item.EventID, "failed", slog.String("error", result.Err.Error()))
			},
			exceptionReason: "delivery_failed",
			origin:          message.exceptionOrigin(),
			settle:          settleClearIf(result.ShouldNAK),
		})
		if result.ShouldNAK {
			cfg.messagesNAKed.Inc()
			message.finalize(true)
		} else {
			message.finalize(false)
		}
		return
	}
	unplaceableAgentDelivery(cfg, message, item, sessionID, metadata, attempt)
}

// fanoutDelivery delivers a general-topic envelope to every session that
// registered interest in it (registry.Match), aggregating per-recipient
// failures into a single NAK/ACK decision for the underlying message. Every
// recipient this path answers for is asked of the delivery ledger, after the
// echo check and before its delivery-mode checks: a recipient it refuses, so
// that one `delivery_failed` holds across the copies of an event stored on
// several of the session's topics, and a recipient that reads the message on
// its own NATS subscription, so that the second copy of that event is counted
// a dedupe rather than a second delivery. The second ask is best-effort
// (ledgerAsk.bestEffort): the listener hands such a copy to nobody, so a
// ledger that cannot answer leaves it counted skipped, with no record, rather
// than holding the whole message back for 30 s for every recipient of it. A
// recipient whose own consumer relays the message is asked nothing at all: a
// record written here would have the relay's own copy of the next event under
// that key skipped as one the session already has. The message is retried
// after the longest wait any recipient asks for: durableRetryDelay for a
// failure, or until another pod's attempt can be taken over.
func fanoutDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	items := cfg.registry.Match(cfg.machineID, item.Topic)
	if len(items) == 0 {
		// No interests is no failure: listenerDeliveryHandler routes every role and agent topic
		// elsewhere, so a fanout topic is never a control topic and owes no delivery exception,
		// and the message is ACKed with nothing to retry.
		cfg.logger.Info("listener no matching interests", slog.String("topic", item.Topic))
		message.finalize(false)
		return
	}
	mode, modeErr := envelopeDeliveryMode(item)

	var retryIn time.Duration
	var deadDeliveries int
	for _, interest := range items {
		verdict, target, _ := judgeFollowedTopic(cfg.sessions, interest.SessionID, item, mode, modeErr)
		if verdict == followedTopicEcho {
			// The echo is disposed of before the ledger is asked, as the role lane disposes of
			// one: a message the session published itself is no delivery to it.
			applyDeliveryOutcome(cfg, item, fanoutRefusalOutcome(interest.SessionID, item, verdict, mode, dedupe.Attempt{}, false))
			continue
		}
		// The caches, the echo rule and an unreadable delivery mode judge the message before the
		// session's own state does, as they always have; what is left is a message this path
		// would push, or refuse on the session's entry. A session past the live window is
		// answering nothing: its port, if it has one, does not take a push, and if it receives
		// on its own subscription this listener was never going to push to it anyway. Either way
		// nothing here failed - the message stays in the stream for the session to read back
		// (POST /v1/replay), its own consumer still holds the copy its relay pushes when it
		// answers again, and its record, its claims and its interests all stand - so this is a
		// skip with a status of its own and not a delivery failure, which would publish an
		// exception and NAK the message for a session that is simply slow. The state is the
		// session record's own age (session.SessionRegistry.Lookup) and never this cache's
		// membership, which is also why it is read here rather than taken from the entry the
		// judgement returned.
		if verdict == takeFollowedTopic || verdict == followedTopicModeNotAdvertised {
			if targetEntry, targetState := cfg.sessions.Lookup(interest.SessionID); targetState == session.Unresponsive {
				cfg.fanoutUnresponsiveSkips.Inc()
				applyDeliveryOutcome(cfg, item, deliveryOutcome{
					sessionID:    interest.SessionID,
					metricStatus: "unresponsive",
					log: func(logger *logging.Logger) {
						logger.DeliveryLog(slog.LevelInfo, "listener skip unresponsive session", interest.SessionID, item.Topic, item.EventID, "unresponsive", slog.Int64("last_seen", targetEntry.UpdatedAt))
					},
				})
				continue
			}
		}
		// One resolution per recipient decides every outcome below: it reads the entry the
		// judgement above returned for every verdict, its refusals among them, and makes no call
		// of its own.
		delivery, err := session.Resolve(item, interest.SessionID, target, message.storedSeq())
		relayed := err == nil && delivery.Relayed
		// A recipient whose own consumer relays this message is asked nothing: it is handed
		// nothing here. Nor is one this path would have delivered to whose resolution failed,
		// since that names a delivery that was never made. A refused recipient is asked however
		// its resolution went, because what its record covers is the refusal reported below. The
		// ask for a recipient that reads the message on its own subscription is best-effort,
		// since nothing of it is handed over either - only its record is worth having, and only
		// if the ledger answers.
		var attempt dedupe.Attempt
		if !relayed && (verdict != takeFollowedTopic || err == nil) {
			var deliver bool
			var wait time.Duration
			if attempt, deliver, wait = ledgerAttempt(cfg, message, item, interest.SessionID, ledgerAsk{bestEffort: verdict == takeFollowedTopic}); !deliver {
				retryIn = max(retryIn, wait)
				continue
			}
		}
		// Refusals are decided before the take case, since the session whose own consumer relays
		// the message is one this path still reports a refusal of: that relay judges nothing it
		// publishes, and the plugin drops a frame whose delivery mode it cannot read without a
		// word.
		if verdict != takeFollowedTopic {
			applyDeliveryOutcome(cfg, item, fanoutRefusalOutcome(interest.SessionID, item, verdict, mode, attempt, err == nil && !relayed))
			continue
		}
		deliveryTimer := metrics.NewTimer()
		if relayed {
			// The session's own consumer holds the message and the relay of that consumer
			// delivers it: this path delivers nothing and records nothing for it. A delivered
			// record written here would have the next copy of the same event skipped as one the
			// session already has, on the word of a path that handed it to nobody
			// (session.SessionEntry.ConsumerRelaysStored).
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				metricStatus: "skipped",
				duration:     &deliveryTimer,
			})
			continue
		}
		if err != nil {
			retryable := shouldNAKFanoutDelivery(target != nil, err)
			if !retryable {
				deadDeliveries++
			}
			if applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				metricStatus: "failed",
				duration:     &deliveryTimer,
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelError, "listener delivery failed", interest.SessionID, item.Topic, item.EventID, "failed", slog.String("error", err.Error()))
				},
				retry: retryable,
			}) {
				retryIn = max(retryIn, durableRetryDelay)
			}
		} else {
			// The session reads the message on its own NATS subscription: this path hands it
			// nothing, but the message did reach it, so the copy is recorded delivered and the
			// next copy of the same event is a dedupe skip. The record is the attempt taken
			// above, which is the zero attempt when the ledger could not answer, and settling
			// that writes nothing.
			deliveryStatus := "delivered"
			if delivery.Skipped {
				deliveryStatus = "skipped"
			}
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				attempt:      attempt,
				metricStatus: deliveryStatus,
				duration:     &deliveryTimer,
				settle:       settleRecord,
			})
		}
	}
	if deadDeliveries > 0 {
		cfg.logger.Info("listener skipped dead session deliveries", slog.String("topic", item.Topic), slog.Int("count", deadDeliveries))
	}
	message.finish(cfg, retryIn)
}
