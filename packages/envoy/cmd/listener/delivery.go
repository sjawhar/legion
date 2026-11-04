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
// by its subject (listenerDeliveryHandler) for every holder that has it another way; a
// serving-port holder of this listener has no subscriber, so its copy is its delivery
// (roleForwardCopyDelivery).
const roleForwardDedupePrefix = contracts.RoleForwardDedupePrefix

const roleReceiptTimeout = 2 * time.Second

// exceptionPublishFailedLine is the line a delivery logs when its delivery exception could not be
// published. The deployed publish-failure metric filter, which the deployment repository's
// listener infrastructure defines, matches it exactly, so every site that logs it uses this constant.
const exceptionPublishFailedLine = "listener exception publish failed"

// ledgerUnavailableLine is the WARN a delivery logs when the delivery ledger could not be read or
// written: the durable lane then retries the message, and the role and live lanes deliver without
// the ledger.
const ledgerUnavailableLine = "delivery ledger unavailable"

// durableRetryDelay is how long the durable waits before it redelivers a message the listener
// NAKed for a retry.
const durableRetryDelay = 30 * time.Second

func shouldNAKFanoutDelivery(sessionLive bool, err error) bool {
	return err != nil && sessionLive
}

type listenerDeliveryHandlerConfig struct {
	client *bus.Client
	// forwardRole publishes a role-lane envelope to the holder's agent subject under msgID, its
	// Nats-Msg-Id when not empty, and returns the wait for its outcome; main.go wires
	// startRoleForward(client). The wait's bus.ErrReceiptTimeout means the forward was flushed to
	// the server and no receipt arrived inside timeout; bus.ErrForwardNotStored that the stream
	// did not acknowledge storing it; any other error — a raw nats.ErrTimeout from the flush
	// included, returned by the publish or the wait — means the forward is not known to have
	// reached the holder.
	forwardRole func(subject string, item contracts.Envelope, msgID string, timeout time.Duration) (wait func() error, err error)
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
	deliveryDuration  *metrics.Histogram
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
	// a shutdown's in-flight grace runs out, which aborts a prompt in flight. Nil on the core lanes.
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

// clearLedgerAttempt gives attempt up so a retry prompts its pair again. A clear the ledger
// cannot write leaves the attempted record behind, which this process's next attempt for the pair
// takes over, and logs that.
func clearLedgerAttempt(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, attempt dedupe.Attempt) {
	if err := cfg.ledger.Clear(attempt); err != nil {
		cfg.logger.Warn(ledgerUnavailableLine, slog.String("operation", "clear"), slog.String("topic", item.Topic), slog.String("event_id", item.EventID), slog.String("error", err.Error()))
	}
}

// attemptOnceReconnected asks ledger for (item, sessionID), and once more once the ledger's own
// connection is back when the first call failed while that connection was reconnecting:
// reconnected waits for it (listenerDeliveryHandlerConfig.ledgerReconnected), so a call that
// failed in a reconnect's moment costs no retry delay, and the second answer, when it comes, is
// logged. It is the durable lane's first ask (ledgerAttempt) and the relay's
// (sessionRelays.followedTopicVerdict) alike. A caller that waits for no reconnect passes nil,
// and a nil ctx waits on the background context.
func attemptOnceReconnected(ctx context.Context, logger *logging.Logger, ledger dedupe.Ledger, reconnected func(context.Context) bool, item contracts.Envelope, sessionID string) (dedupe.Attempt, error) {
	attempt, err := ledger.Attempt(item.DedupeKey, sessionID)
	if err == nil || reconnected == nil {
		return attempt, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !reconnected(ctx) {
		return attempt, err
	}
	first := err
	if attempt, err = ledger.Attempt(item.DedupeKey, sessionID); err == nil {
		logger.Info("delivery ledger answered once its connection was back", slog.String("session_id", sessionID), slog.String("event_id", item.EventID), slog.String("first_error", first.Error()))
	}
	return attempt, err
}

// ledgerAttempt asks the delivery ledger for (item, sessionID) before the listener prompts the
// session over HTTP or forwards the role-lane message to it, and logs and counts any skip. It
// returns the attempt the delivery settles and deliver=true when the listener goes on to deliver;
// otherwise retryIn > 0 asks the durable lane to retry the message after it, and zero to ack it.
//
//   - Delivered (already delivered) and InFlight (this process's attempt whose prompt may still be
//     running, or one a delivery kept) skip the pair, and the message is acked.
//   - Deferred (another pod's attempt, which may still be prompting) is retried on the durable lane
//     once that attempt can be taken over, and skipped on a core lane, which retries nothing.
//   - A ledger that cannot be read retries the message on the durable lane after durableRetryDelay;
//     a core lane delivers without the ledger, under the zero attempt, which settles nothing. The
//     durable lane first waits for the ledger's connection (ledgerReconnected) and asks once more,
//     so a call that failed while that connection was reconnecting costs no 30 s.
//
// role picks the role lane's skip line, `listener role dedupe skip`, for every skip.
func ledgerAttempt(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string, role bool) (attempt dedupe.Attempt, deliver bool, retryIn time.Duration) {
	durable := message.mode == durableDelivery
	// Only the durable lane waits for the ledger's connection and asks again: a core lane retries
	// nothing, so it delivers under the zero attempt rather than waiting.
	reconnected := cfg.ledgerReconnected
	if !durable {
		reconnected = nil
	}
	attempt, err := attemptOnceReconnected(message.ctx, cfg.logger, cfg.ledger, reconnected, item, sessionID)
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
	if role {
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

// laneHolderFresh is role delivery's liveness test of a holder's session entry, which
// resolveRoleHolder applies for the role lane and the publish route's attempts: the entry
// heartbeated within session.ClaimStaleAfter as of now (roles.Fresh, the test Dispatch's outbox
// makes too), read at each test, so the cached entry and the entry read again from the bucket are
// each judged at the moment they are checked. The plugin heartbeats every 90 seconds
// (DEFAULT_HEARTBEAT_MS, packages/envoy-client/src/envoy-settings.ts), so three missed heartbeats
// means the claiming process is very likely gone.
func laneHolderFresh(entry session.SessionEntry) bool {
	return roles.Fresh(entry, time.Now())
}

// resolveCoreRoleHolder resolves the current live holder for a role-lane delivery by role
// delivery's rule (resolveRoleHolderFrom with laneHolderFresh, over roles.ResolveHolderFrom),
// applying the terminal outcome itself and reporting ok=false when there is nobody to deliver to.
// A resolution that found no live holder keeps the message for the role's next holder first
// (keepRoleLaneMessage), in one decision for both give-up branches: roleKeepable says which
// states a copy is kept for - an unclaimed role and a lapsed claim that was released or already
// absent, never a claim the restart grace retained - and a copy that was kept publishes no
// delivery exception, since the message is not lost. Every other case leaves its branch exactly
// as it was before the kept lane: an unclaimed role is no_holder, a lapsed holder
// delivery_failed, and so is a keep that failed. Every failed read is delivery_failed and keeps
// nothing, since a read that did not answer is no evidence the role has no holder. A claim whose
// release finds it superseded is resolved again, bounded, so a claim raced by a live replacement
// is re-resolved to that replacement here exactly as it is for GET /v1/roles/<role>, instead of
// reporting delivery_failed about a session a fresh claim has already superseded. Each branch
// logs a line that names which it was. Role delivery answers no caller, so it resolves under
// context.Background(): the holder's session read keeps roleHolderReadTimeout
// (roleHolderSessions' bound) as its own cap. Its first claim read is the registry's watched copy
// of the role bucket (store.Registry.CachedRoleHolder, passed as the resolution's firstHolder),
// so a role message costs no bucket round trip; a retry after a superseded release reads the
// bucket itself, since the copy can trail the claim that superseded it.
func resolveCoreRoleHolder(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, role string) (string, session.SessionEntry, bool) {
	resolved, err := resolveRoleHolderFrom(context.Background(), cfg.registry, cfg.registry.CachedRoleHolder, cfg.sessions, role, laneHolderFresh)
	sessionID := resolved.holder
	// Every error resolveRoleHolder returns is a *roleHolderFailure naming the read that failed.
	// One that is not would still be a read that failed, so it is logged as the claim read with
	// its own error rather than left without a step to branch on.
	var failure *roleHolderFailure
	if err != nil && !errors.As(err, &failure) {
		failure = &roleHolderFailure{step: roleHolderClaimRead, cause: err}
	}
	if err == nil {
		if resolved.state == roleHolderLive {
			return sessionID, resolved.entry, true
		}
		if roleKeepable(resolved) && keepRoleLaneMessage(cfg, role, sessionID, item, resolved.state) {
			return "", session.SessionEntry{}, false
		}
		if resolved.state == roleHolderUnclaimed {
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
	switch failure.step {
	case roleHolderClaimRead:
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.Error("listener role holder lookup failed", slog.String("role", role), slog.String("error", failure.cause.Error()))
			},
			exceptionReason: "delivery_failed",
		})
	case roleHolderSessionRead:
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.Error("listener role holder session lookup failed", slog.String("role", role), slog.String("session_id", sessionID), slog.String("error", failure.cause.Error()))
			},
			exceptionReason: "delivery_failed",
		})
	case roleHolderRelease:
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.Error("listener expired role claim cleanup failed", slog.String("role", role), slog.String("session_id", sessionID), slog.String("error", failure.cause.Error()))
			},
			exceptionReason: "delivery_failed",
		})
	default:
		// roleHolderChurn, whose cause is errRoleHolderChanged: its own line says so.
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.Error("listener role holder churned past resolution bound", slog.String("role", role))
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
	attempt, deliver, _ := ledgerAttempt(cfg, message, item, sessionID, true)
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
// A delivery this listener answers for asks the delivery ledger before it applies its outcome,
// as main does: the pair is attempted, and a delivery that lands - the session takes the message
// off its own subscription - records it delivered, so a second copy of the same message is a
// dedupe skip rather than a second delivery, on this pod or the one that takes delivery over,
// and a message already reported or given up for this session is skipped rather than reported a
// second time. The two routes this listener hands nothing to are settled before that ask and
// cost no ledger write at all: a message the relay of the session's own consumer delivers, and
// one a session another listener owns. A record of either would say this listener delivered a
// message it handed to nobody, and the relay, which asks the ledger for nothing, would be judged
// by it.
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
	entry, entryErr := liveSessionEntry(context.Background(), cfg.sessions, sessionID, deliverySessionReadTimeout, roles.Registered)
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
	// time.
	attempt, deliver, retryIn := ledgerAttempt(cfg, message, item, sessionID, false)
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

// followedTopicRules is what a message on a topic a session follows is judged against
// (judgeFollowedTopic): the session entries. What each session already took, and what is already
// on its way to it, is the machine's shared delivery ledger's answer rather than a rule here, and
// every pod of the machine reads and writes that one ledger, so a pod that takes delivery over
// neither pushes again what the pod before it delivered nor pushes beside a push that pod still
// has in flight.
type followedTopicRules struct {
	sessions *session.SessionRegistry
	ledger   dedupe.Ledger
}

func (cfg listenerDeliveryHandlerConfig) followedTopicRules() followedTopicRules {
	return followedTopicRules{sessions: cfg.sessions, ledger: cfg.ledger}
}

// followedTopicVerdict is what a delivery path does with a message on a topic a session follows.
type followedTopicVerdict uint8

const (
	// takeFollowedTopic: deliver the message to the session.
	takeFollowedTopic followedTopicVerdict = iota
	// followedTopicEcho: the session published the message itself.
	followedTopicEcho
	// followedTopicModeUnreadable: the envelope's targeted delivery mode cannot be read
	// unambiguously, so no session can be shown to advertise it.
	followedTopicModeUnreadable
	// followedTopicModeNotAdvertised: the session's live entry does not advertise the mode.
	followedTopicModeNotAdvertised
)

// judgeFollowedTopic is the judgement of a message on a topic sessionID follows that the durable
// path's fan-out (fanoutDelivery) and the relay of a serving-port session's consumer
// (sessionRelays.followedTopicVerdict) apply alike, so each refusal holds whichever route a
// session's message takes: an echo of the session's own message, and a delivery mode its entry
// does not advertise or one that cannot be read, are refused; everything else is the session's to
// take.
//
// A copy the session already took, and one already on its way to it, are no part of this
// judgement: both are the machine's shared delivery ledger's answer, which each route asks for
// and settles itself (ledgerAttempt on the fan-out, followedTopicVerdict on the relay). Both are
// keyed on the envelope's dedupe key, so one upstream event published on several of a session's
// topics (contracts/normalize.go) reaches it once, on either route and from either pod of the
// machine.
//
// mode is the delivery mode the envelope targets, "" when it targets none, and modeErr the failure
// of reading it (envelopeDeliveryMode): both are read once per message rather than once per
// recipient.
//
// It returns the session's live entry whenever one was read, for every verdict and not only the
// ones judged against it: a caller that delivers through the entry does not read it again, and
// the fan-out decides from it whether the session's own consumer relays this message, which it
// must know for a refusal as much as for a delivery (fanoutDelivery). It returns the error behind
// a mode refusal too, for the caller to report.
func judgeFollowedTopic(rules followedTopicRules, sessionID string, item contracts.Envelope, mode string, modeErr error) (followedTopicVerdict, *session.SessionEntry, error) {
	var target *session.SessionEntry
	if entry, err := rules.sessions.Get(sessionID); err == nil {
		target = &entry
	}
	if item.SourceSession != "" && item.SourceSession == sessionID {
		return followedTopicEcho, target, nil
	}
	if modeErr != nil {
		return followedTopicModeUnreadable, target, modeErr
	}
	if target == nil {
		// No entry is no refusal: the delivery path decides what to do with a session it cannot
		// read, and a mode nobody advertises cannot be judged against an entry there is none of.
		return takeFollowedTopic, nil, nil
	}
	if mode != "" && !hasCapability(target.Capabilities, mode) {
		return followedTopicModeNotAdvertised, target, errors.New("the session does not advertise delivery mode " + mode)
	}
	return takeFollowedTopic, target, nil
}

// fanoutRefusalOutcome is how the fan-out reports a refusal judgeFollowedTopic returned, each
// under the line and the metric status it has always had. keep says whether the pair's ledger
// attempt is one to leave standing: a mode refusal of a session this path resolved and delivers
// to itself keeps it, so an identical redelivery inside dedupe.AttemptedAge is the attempt-dedupe
// skip ledgerAttempt reports rather than a second exception. A refusal whose resolution failed,
// and one of a session whose own consumer relays the message, gives the attempt up. An attempt is
// kept only together with the exception that reports it, which is what lets the relay of a
// serving-port session's consumer tell the two meanings of a ledger record apart: it acks such a
// message unreported rather than reporting it again (sessionRelays.followedTopicVerdict). An echo
// is refused before the ledger is asked and settles nothing.
func fanoutRefusalOutcome(sessionID string, item contracts.Envelope, verdict followedTopicVerdict, mode string, attempt dedupe.Attempt, keep bool) deliveryOutcome {
	settle := settleClear
	if keep {
		settle = settleKeep
	}
	switch verdict {
	case followedTopicEcho:
		return deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "skipped",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener skip echo", sessionID, item.Topic, item.EventID, "skipped")
			},
		}
	case followedTopicModeUnreadable:
		return deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener delivery mode cannot be read unambiguously", sessionID, item.Topic, item.EventID, "failed")
			},
			exceptionReason:               "delivery_failed",
			fanoutRefusal:                 true,
			settle:                        settle,
			exceptionFailureClearsAttempt: keep,
		}
	default:
		return deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener subscriber does not advertise delivery mode", sessionID, item.Topic, item.EventID, "failed", slog.String("mode", mode))
			},
			exceptionReason:               "delivery_failed",
			fanoutRefusal:                 true,
			settle:                        settle,
			exceptionFailureClearsAttempt: keep,
		}
	}
}

// fanoutDelivery delivers a general-topic envelope to every session that
// registered interest in it (registry.Match), aggregating per-recipient
// failures into a single NAK/ACK decision for the underlying message. Only a
// recipient this path refuses is asked of the delivery ledger, after the echo
// check and before its delivery-mode checks, so a recipient that simply takes
// the message costs no ledger write, and a duplicate of a message already
// reported is skipped before its recipient's current capabilities can refuse
// it a second time. The message is retried after the longest wait any
// recipient asks for: durableRetryDelay for a failure, or until another pod's
// attempt can be taken over.
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
		verdict, target, _ := judgeFollowedTopic(cfg.followedTopicRules(), interest.SessionID, item, mode, modeErr)
		if verdict == followedTopicEcho {
			// The echo is disposed of before the ledger is asked, as the role lane disposes of
			// one: a message the session published itself is no delivery to it.
			applyDeliveryOutcome(cfg, item, fanoutRefusalOutcome(interest.SessionID, item, verdict, mode, dedupe.Attempt{}, false))
			continue
		}
		// One resolution per recipient decides every outcome below: it reads the entry the
		// judgement above returned for every verdict, its refusals among them, and makes no call
		// of its own.
		delivery, err := session.Resolve(item, interest.SessionID, target, message.storedSeq())
		relayed := err == nil && delivery.Relayed
		// The delivery ledger is asked for a recipient this path refuses, and for no other: the
		// exception a refusal reports is deduped by the pair's record, so one `delivery_failed`
		// holds across the copies of an event stored on several of the session's topics and
		// across the pods of the machine. A recipient that simply takes the message - one
		// reading it on its own subscription, one whose own consumer relays it - costs no ledger
		// write: nothing here is handed to it, so there is nothing a record could keep another
		// pod from doing again, and two KV round trips per recipient on the durable's handler
		// would turn a ledger that is down into a 30 s retry of the message for every recipient
		// of it.
		var attempt dedupe.Attempt
		if !relayed && verdict != takeFollowedTopic {
			var deliver bool
			var wait time.Duration
			if attempt, deliver, wait = ledgerAttempt(cfg, message, item, interest.SessionID, false); !deliver {
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
			deliveryStatus := "delivered"
			if delivery.Skipped {
				deliveryStatus = "skipped"
			}
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				metricStatus: deliveryStatus,
				duration:     &deliveryTimer,
			})
		}
	}
	if deadDeliveries > 0 {
		cfg.logger.Info("listener skipped dead session deliveries", slog.String("topic", item.Topic), slog.Int("count", deadDeliveries))
	}
	message.finish(cfg, retryIn)
}
