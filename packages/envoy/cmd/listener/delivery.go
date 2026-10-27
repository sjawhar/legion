package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// roleForwardDedupePrefix marks an envelope already sent from the role arbiter
// to its selected agent subject. The durable agent-stream replay must ACK it
// without re-entering the arbiter.
const roleForwardDedupePrefix = "envoy.role.forward."

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
	// forwardRole delivers a role-lane envelope to the holder's agent subject and
	// waits for its empty receipt. main.go wires client.RequestCoreTo.
	// bus.ErrReceiptTimeout means the forward was flushed to the server and no
	// receipt arrived inside timeout; any other error — a raw nats.ErrTimeout
	// from the flush included — means the forward is not known to have reached
	// the holder.
	forwardRole func(subject string, item contracts.Envelope, timeout time.Duration) error
	registry    *store.Registry
	sessions    *session.SessionRegistry
	machineID   string
	deliverer   *session.Deliverer
	// ledger is the delivery ledger every lane asks before it prompts a session or forwards a
	// role-lane message to it: the machine's shared one (dedupe.KV), or, without the listener
	// state bucket, this process's own (dedupe.Memory).
	ledger            dedupe.Ledger
	logger            *logging.Logger
	messagesReceived  *metrics.Counter
	messagesDelivered *metrics.Counter
	messagesNAKed     *metrics.Counter
	deliveryDuration  *metrics.Histogram
}

type deliveryMode uint8

const (
	coreDelivery deliveryMode = iota
	durableDelivery
	// liveDelivery is a message the live lane took off core NATS (liveDeliveryHandler). Like
	// coreDelivery it is delivered to whoever is there and never retried; unlike it, its envelope
	// must name the subject it arrived on as its topic.
	liveDelivery
)

type deliveryMessage struct {
	data []byte
	mode deliveryMode
	// msg is the NATS message: set for durableDelivery, which acknowledges it, and liveDelivery,
	// which reads its subject.
	msg *nats.Msg
	// ctx is a durable message's handling context (durableReceiver): the receiver cancels it when
	// a shutdown's in-flight grace runs out, which aborts a prompt in flight. Nil on the core lanes.
	ctx context.Context
}

// cancelled reports whether the receiver cut this message's handling short. Whatever the
// handling had reached is then a retryable failure: the next holder of the lease delivers it.
func (message deliveryMessage) cancelled() bool {
	return message.ctx != nil && message.ctx.Err() != nil
}

// deliverer is the deliverer for this message: cfg's, with its prompts under the message's
// handling context when it has one, so cancelling that context aborts a prompt in flight.
func (message deliveryMessage) deliverer(cfg listenerDeliveryHandlerConfig) *session.Deliverer {
	if message.ctx == nil {
		return cfg.deliverer
	}
	return cfg.deliverer.WithContext(message.ctx)
}

// finalize makes core delivery's loss-on-failure semantics explicit: only a
// durable JetStream message receives an acknowledgement or retry request. A role-lane or live-lane
// message is core NATS, sent once to whoever was subscribed, so nothing acknowledges or retries it.
// A durable retry waits durableRetryDelay (30 s) before redelivery (retryAfter).
func (message deliveryMessage) finalize(retry bool) {
	if retry {
		message.retryAfter(durableRetryDelay)
		return
	}
	if message.mode == durableDelivery {
		_ = message.msg.Ack()
	}
}

// retryAfter NAKs a durable message for redelivery after delay, unless the receiver cancelled the
// message's handling on a shutdown: that one is NAKed for redelivery at once, so the pod that takes
// the lease next delivers it first rather than after the messages behind it. A core-lane message
// is never retried.
func (message deliveryMessage) retryAfter(delay time.Duration) {
	if message.mode != durableDelivery {
		return
	}
	if message.cancelled() {
		_ = message.msg.Nak()
		return
	}
	_ = message.msg.NakWithDelay(delay)
}

// durableHandler handles one message of the listener's durable under ctx, the message's handling
// context, and settles it: an ack, or a NAK for a retry.
type durableHandler func(ctx context.Context, msg *nats.Msg)

func jetStreamDeliveryHandler(cfg listenerDeliveryHandlerConfig) durableHandler {
	handler := listenerDeliveryHandler(cfg)
	return func(ctx context.Context, msg *nats.Msg) {
		handler(deliveryMessage{data: msg.Data, mode: durableDelivery, msg: msg, ctx: ctx})
	}
}

func coreNATSDeliveryHandler(cfg listenerDeliveryHandlerConfig) nats.MsgHandler {
	handler := listenerDeliveryHandler(cfg)
	return func(msg *nats.Msg) {
		handler(deliveryMessage{data: msg.Data, mode: coreDelivery})
	}
}

type deliveryExceptionPayload struct {
	OriginalTopic    string `json:"original_topic"`
	EventID          string `json:"event_id"`
	Reason           string `json:"reason"`
	RecipientSession string `json:"recipient_session,omitempty"`
	PayloadSummary   string `json:"payload_summary"`
	Payload          string `json:"payload"`
	DedupeKey        string `json:"dedupe_key"`
	Source           string `json:"source"`
	SourceSession    string `json:"source_session"`
}

func isControlTopic(topic string) bool {
	return strings.HasPrefix(topic, contracts.RoleTopicPrefix) || strings.HasPrefix(topic, contracts.AgentTopicPrefix)
}

func isExceptionsTopic(topic string) bool {
	return strings.HasPrefix(topic, bus.ExceptionTopicPrefix)
}

// deliveryExceptionID is the id of the exception reporting that the delivery of the event eventID
// to recipientSession ended in reason: 32 hex of the sha256 of the three, the shape of a minted
// id. A redelivery that ends the same way publishes the same exception, so every repeat carries
// one id and one dedupe key (bus.DeliveryExceptionDedupePrefix and the id), and the stream, which
// publishes a delivery exception under a MsgId of that key, stores it once.
func deliveryExceptionID(eventID, recipientSession, reason string) string {
	fields, _ := json.Marshal([3]string{eventID, recipientSession, reason})
	sum := sha256.Sum256(fields)
	return hex.EncodeToString(sum[:16])
}

func publishDeliveryException(client *bus.Client, item contracts.Envelope, reason, recipientSession string, fanoutRefusal bool) error {
	payload, err := json.Marshal(deliveryExceptionPayload{
		OriginalTopic:    item.Topic,
		EventID:          item.EventID,
		Reason:           reason,
		RecipientSession: recipientSession,
		PayloadSummary:   item.PayloadSummary,
		Payload:          item.Payload,
		DedupeKey:        item.DedupeKey,
		Source:           item.Source,
		SourceSession:    item.SourceSession,
	})
	if err != nil {
		return fmt.Errorf("marshal delivery exception: %w", err)
	}
	exceptionID := deliveryExceptionID(item.EventID, recipientSession, reason)
	exception := contracts.Envelope{
		EventID:        exceptionID,
		Source:         "envoy",
		SourceEventID:  item.EventID,
		Topic:          bus.ExceptionTopicPrefix + item.Topic,
		DedupeKey:      bus.DeliveryExceptionDedupePrefix + exceptionID,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "delivery exception: " + reason,
		Payload:        string(payload),
		TraceID:        id.New(),
	}
	if err := exception.Validate(); err != nil {
		return fmt.Errorf("validate delivery exception: %w", err)
	}
	if fanoutRefusal {
		if err := client.PublishCore(exception); err != nil {
			return fmt.Errorf("publish fanout delivery exception: %w", err)
		}
		return nil
	}
	if err := client.Publish(exception); err != nil {
		return fmt.Errorf("publish delivery exception: %w", err)
	}
	return nil
}

// deliveryOutcome bundles one delivery attempt's bookkeeping: the metrics
// label to record, the log line to emit, whether to publish a delivery
// exception (and how an exception-publish failure should be treated), and
// how to settle the pair's delivery-ledger attempt. Each branch of the three
// delivery paths below produces one of these; applyDeliveryOutcome performs
// the mechanical side effects so they happen in exactly one place.
type deliveryOutcome struct {
	// sessionID is the recipient the outcome is about, named in its exception; "" when there is
	// none (e.g. a role that was never assigned a holder).
	sessionID string
	// attempt is the pair's delivery-ledger attempt that clearAttempt, recordDedupe and
	// exceptionFailureClearsAttempt settle; the zero Attempt (a pair delivered without the
	// ledger, or one the listener does not prompt) settles nothing.
	attempt dedupe.Attempt
	// metricStatus is the messagesDelivered "delivery_status" label; ""
	// skips the increment entirely.
	metricStatus string
	// duration, when non-nil, is observed against cfg.deliveryDuration under
	// metricStatus (only the direct-agent and fanout paths time attempts).
	duration *metrics.Timer
	// log renders this outcome's operator-facing line; nil emits nothing.
	log func(logger *logging.Logger)

	// exceptionReason, when non-empty, publishes a delivery exception via
	// publishDeliveryException for control topics. fanoutRefusal also emits a
	// core-NATS exception for this terminal fanout outcome.
	exceptionReason string
	fanoutRefusal   bool
	// exceptionFailureRetries: an exception-publish failure escalates this
	// outcome's retry result to true.
	exceptionFailureRetries bool
	// exceptionFailureClearsAttempt: an exception-publish failure clears the
	// ledger attempt, independent of (and possibly in addition to)
	// clearAttempt below.
	exceptionFailureClearsAttempt bool

	// clearAttempt unconditionally gives the ledger attempt up (dedupe.Ledger.Clear).
	clearAttempt bool
	// recordDedupe records the pair delivered in the ledger (dedupe.Ledger.Record).
	recordDedupe bool
	// retry is this outcome's baseline retry request, before any exception-
	// publish escalation above is folded in.
	retry bool
}

// applyDeliveryOutcome performs the bookkeeping every delivery attempt
// shares — metrics, duration, logging, exception publication, and
// settling the ledger attempt — and reports whether the attempt should be
// retried (NAKed).
func applyDeliveryOutcome(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, outcome deliveryOutcome) bool {
	if outcome.metricStatus != "" {
		cfg.messagesDelivered.Inc([2]string{"delivery_status", outcome.metricStatus})
		if outcome.duration != nil {
			outcome.duration.ObserveDuration(cfg.deliveryDuration, [2]string{"delivery_status", outcome.metricStatus})
		}
	}
	if outcome.log != nil {
		outcome.log(cfg.logger)
	}
	retry := outcome.retry
	clearAttempt := outcome.clearAttempt
	if outcome.exceptionReason != "" && !isExceptionsTopic(item.Topic) && (isControlTopic(item.Topic) || outcome.fanoutRefusal) {
		if err := publishDeliveryException(cfg.client, item, outcome.exceptionReason, outcome.sessionID, outcome.fanoutRefusal); err != nil {
			cfg.logger.Error(exceptionPublishFailedLine, slog.String("error", err.Error()), slog.String("topic", item.Topic))
			clearAttempt = clearAttempt || outcome.exceptionFailureClearsAttempt
			if outcome.exceptionFailureRetries {
				retry = true
			}
		}
	}
	if clearAttempt {
		clearLedgerAttempt(cfg, item, outcome.attempt)
	}
	if outcome.recordDedupe {
		if err := cfg.ledger.Record(outcome.attempt); err != nil {
			cfg.logger.Warn(ledgerUnavailableLine, slog.String("operation", "record"), slog.String("session_id", outcome.sessionID), slog.String("event_id", item.EventID), slog.String("error", err.Error()))
		}
	}
	return retry
}

// clearLedgerAttempt gives attempt up so a retry prompts its pair again. A clear the ledger
// cannot write leaves the attempt to age out (dedupe.AttemptedAge), and logs that.
func clearLedgerAttempt(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, attempt dedupe.Attempt) {
	if err := cfg.ledger.Clear(attempt); err != nil {
		cfg.logger.Warn(ledgerUnavailableLine, slog.String("operation", "clear"), slog.String("topic", item.Topic), slog.String("event_id", item.EventID), slog.String("error", err.Error()))
	}
}

// ledgerAttempt asks the delivery ledger for (item, sessionID) before the listener prompts the
// session over HTTP or forwards the role-lane message to it, and logs and counts any skip. It
// returns the attempt the delivery settles and deliver=true when the listener goes on to deliver;
// otherwise retryIn > 0 asks the durable lane to retry the message after it, and zero to ack it.
//
//   - Delivered (already delivered) and InFlight (this process's unresolved attempt) skip the pair,
//     and the message is acked.
//   - Deferred (another pod's attempt, which may still be prompting) is retried on the durable lane
//     once that attempt can be taken over, and skipped on a core lane, which retries nothing.
//   - A ledger that cannot be read retries the message on the durable lane after durableRetryDelay;
//     a core lane delivers without the ledger, under the zero attempt, which settles nothing.
//
// role picks the role lane's skip line, `listener role dedupe skip`, for every skip.
func ledgerAttempt(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string, role bool) (attempt dedupe.Attempt, deliver bool, retryIn time.Duration) {
	attempt, err := cfg.ledger.Attempt(item.DedupeKey, sessionID)
	if err != nil {
		durable := message.mode == durableDelivery
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
		if message.mode == durableDelivery {
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

// promptsHere reports whether this listener, machineID, prompts the session over HTTP: it serves
// a port, and it is this listener's to deliver, as session.Deliverer.DeliverWithResultForEntry
// decides. Its owner is the listener its stored subscription names, else the one it registered
// through; a session neither names is every listener's. Only such a pair costs a ledger write: a
// self-subscribed session takes the message from its own subscription and is only acked, and
// another listener's session is that listener's to deliver.
func promptsHere(machineID string, interest *store.Interest, entry *session.SessionEntry) bool {
	if entry == nil || entry.Port <= 0 {
		return false
	}
	owner := entry.MachineID
	if interest != nil && interest.MachineID != "" {
		owner = interest.MachineID
	}
	return owner == "" || owner == machineID
}

// publishNoHolderExceptionOrNak publishes a "no_holder" delivery exception
// for control topics and, if publishing itself fails, escalates to an
// immediate NAK by finalizing message and reporting false — a message whose
// failure notice never went out must not be silently ACKed. Shared by the
// direct-agent path (no live session for the target) and the fanout path (no
// matching interests at all). sessionID is "" and attempt the zero Attempt when
// this outcome has no recipient (the fanout no-interests case).
func publishNoHolderExceptionOrNak(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string, attempt dedupe.Attempt) bool {
	if !isControlTopic(item.Topic) || isExceptionsTopic(item.Topic) {
		return true
	}
	if err := publishDeliveryException(cfg.client, item, "no_holder", sessionID, false); err != nil {
		clearLedgerAttempt(cfg, item, attempt)
		cfg.logger.Error(exceptionPublishFailedLine, slog.String("error", err.Error()), slog.String("topic", item.Topic))
		cfg.messagesNAKed.Inc()
		message.finalize(true)
		return false
	}
	return true
}

// listenerDeliveryHandler decodes and validates the envelope, then routes it
// to exactly one of the three delivery paths based on its topic: a role
// lane (arbitrated to whichever session currently holds the role), a direct
// agent subject, or the general interest fanout.
func listenerDeliveryHandler(cfg listenerDeliveryHandlerConfig) func(deliveryMessage) {
	return func(message deliveryMessage) {
		var item contracts.Envelope
		if err := json.Unmarshal(message.data, &item); err != nil {
			cfg.logger.Error("listener decode failed", slog.String("error", err.Error()))
			message.finalize(false)
			return
		}
		if err := contracts.ValidateWire(message.data); err != nil {
			cfg.logger.Error("listener invalid envelope", slog.String("error", err.Error()))
			message.finalize(false)
			return
		}
		if err := item.Validate(); err != nil {
			cfg.logger.Error("listener invalid envelope", slog.String("error", err.Error()))
			message.finalize(false)
			return
		}
		// The live lane chose this message by its subject, so an envelope naming another topic would
		// be routed past that choice: to a role lane or an agent's inbox, or to sessions subscribed
		// to a topic nobody published on.
		if message.mode == liveDelivery && item.Topic != message.msg.Subject {
			cfg.logger.Warn("listener live envelope names a topic other than its subject", slog.String("subject", message.msg.Subject), slog.String("topic", item.Topic), slog.String("event_id", item.EventID))
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

// resolveCoreRoleHolder resolves the current live holder for a role-lane
// delivery, applying the terminal no_holder/delivery_failed outcome itself
// and reporting ok=false when there is nobody to deliver to. Delivery
// distrusts a claim whose holder has not heartbeated within
// session.ClaimStaleAfter even when its registry cache entry has not yet
// expired (the plugin heartbeats every 90 seconds — DEFAULT_HEARTBEAT_MS,
// packages/envoy-client/src/envoy-settings.ts — so three missed heartbeats
// means the claiming process is very likely gone) — stricter
// than resolveLiveRoleHolder's plain ErrKeyNotFound check, since a dropped
// delivery is costlier here than an HTTP 404. Both that stricter check and
// resolveLiveRoleHolder release an expired claim through the shared
// releaseExpiredRoleClaim helper and retry, bounded to
// roleHolderResolutionAttempts, whenever the release reports the claim it
// tried to drop was already superseded by a fresh one — so a claim raced by
// a live replacement is re-resolved to that replacement here exactly as it
// is for GET /v1/roles/<role>, instead of reporting delivery_failed about a
// session a fresh claim has already superseded.
func resolveCoreRoleHolder(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, role string) (string, session.SessionEntry, bool) {
	for range roleHolderResolutionAttempts {
		sessionID, err := cfg.registry.RoleHolder(role)
		if err != nil {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				metricStatus: "failed",
				log: func(logger *logging.Logger) {
					logger.Error("listener role holder lookup failed", slog.String("role", role), slog.String("error", err.Error()))
				},
				exceptionReason: "delivery_failed",
			})
			return "", session.SessionEntry{}, false
		}
		if sessionID == "" {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				metricStatus: "skipped",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelWarn, "listener role has no holder", "", item.Topic, item.EventID, "skipped")
				},
				exceptionReason: "no_holder",
			})
			return "", session.SessionEntry{}, false
		}
		now := time.Now().UnixMilli()
		holder, holderErr := roleHolderSession(cfg.sessions, sessionID, func(holder session.SessionEntry) bool {
			return holder.UpdatedAt > 0 && now-holder.UpdatedAt < int64(session.ClaimStaleAfter/time.Millisecond)
		})
		if holderErr == nil {
			return sessionID, holder, true
		}
		if !errors.Is(holderErr, nats.ErrKeyNotFound) {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    sessionID,
				metricStatus: "failed",
				log: func(logger *logging.Logger) {
					logger.Error("listener role holder session lookup failed", slog.String("role", role), slog.String("session_id", sessionID), slog.String("error", holderErr.Error()))
				},
				exceptionReason: "delivery_failed",
			})
			return "", session.SessionEntry{}, false
		}
		_, superseded, err := releaseExpiredRoleClaim(cfg.registry, role, sessionID, cfg.sessions.TTL())
		if err != nil {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    sessionID,
				metricStatus: "failed",
				log: func(logger *logging.Logger) {
					logger.Error("listener expired role claim cleanup failed", slog.String("role", role), slog.String("session_id", sessionID), slog.String("error", err.Error()))
				},
				exceptionReason: "delivery_failed",
			})
			return "", session.SessionEntry{}, false
		}
		if superseded {
			continue
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
	applyDeliveryOutcome(cfg, item, deliveryOutcome{
		metricStatus: "failed",
		log: func(logger *logging.Logger) {
			logger.Error("listener role holder churned past resolution bound", slog.String("role", role))
		},
		exceptionReason: "delivery_failed",
	})
	return "", session.SessionEntry{}, false
}

// roleTopicDelivery arbitrates a role-lane envelope to whichever session
// currently holds the role, forwarding it over core NATS request-reply so
// the sender learns immediately whether the holder received it. After the
// delivery-ledger skip (ledgerAttempt) -- a duplicate of an envelope already
// forwarded (or already being forwarded, by this pod or another of the
// machine's) is disposed of first, with no forward and no exception -- and
// before forwarding, it applies the same capability guard
// as sendHandler (frameDeliveryMode + hasCapability, api.go): a holder that
// does not advertise the envelope's own targeted delivery mode is refused
// like a stale holder. Role forwarding needs its own copy of the guard
// because it never goes through /v1/messages/send. The ordering matters:
// guarding before the dedupe check would re-evaluate an already-delivered
// duplicate against the holder's *current* capabilities, which can have
// changed since the original successful forward, and misreport a delivered
// message as delivery_failed -- a false signal the Legion daemon treats as
// cause to probe and potentially resume the worker. Every branch ACKs: role
// lanes have no durable transit to retry against, so a failed forward is
// reported via a delivery exception instead of a NAK. A forward that reached
// the server and drew no receipt from the live holder inside
// roleReceiptTimeout (bus.ErrReceiptTimeout, the one error keyed on) is
// receipt_timeout; a forward not known to have left this process -- the
// flush timed out or failed, the publish failed -- is delivery_failed like
// a stale holder or an unadvertised/unreadable delivery mode; no claim is
// no_holder.
func roleTopicDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
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
	if item.SourceSession != "" && item.SourceSession == sessionID {
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
	var itemPayload *string
	if item.Payload != "" {
		itemPayload = &item.Payload
	}
	mode, deliveryErr := frameDeliveryMode(itemPayload)
	if deliveryErr != nil {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			clearAttempt: true,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role delivery mode cannot be read unambiguously", sessionID, item.Topic, item.EventID, "failed")
			},
			exceptionReason: "delivery_failed",
		})
		message.finalize(false)
		return
	}
	if mode != "" && !hasCapability(holder.Capabilities, mode) {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			clearAttempt: true,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role holder does not advertise delivery mode", sessionID, item.Topic, item.EventID, "failed", slog.String("mode", mode))
			},
			exceptionReason: "delivery_failed",
		})
		message.finalize(false)
		return
	}
	forwarded := item
	forwarded.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
	if err := cfg.forwardRole(contracts.AgentSubject(sessionID), forwarded, roleReceiptTimeout); err != nil {
		if errors.Is(err, bus.ErrReceiptTimeout) {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    sessionID,
				attempt:      attempt,
				metricStatus: "receipt_timeout",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelWarn, "listener role receipt timed out", sessionID, item.Topic, item.EventID, "receipt_timeout", slog.String("timeout", roleReceiptTimeout.String()))
				},
				exceptionReason: "receipt_timeout",
				clearAttempt:    true,
			})
			message.finalize(false)
			return
		}
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelError, "listener role forward failed", sessionID, item.Topic, item.EventID, "failed", slog.String("error", err.Error()))
			},
			exceptionReason: "delivery_failed",
			clearAttempt:    true,
		})
		message.finalize(false)
		return
	}
	applyDeliveryOutcome(cfg, item, deliveryOutcome{
		sessionID:    sessionID,
		attempt:      attempt,
		metricStatus: "delivered",
		log: func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelInfo, "listener role forwarded", sessionID, item.Topic, item.EventID, "delivered")
		},
		recordDedupe: true,
	})
	message.finalize(false)
}

// agentSubjectDelivery delivers directly to the session named by the topic
// (notifications.agent.<session>), pushing over HTTP/WebSocket when the
// session is live and falling back to whatever session.HandleAgentMessage
// decides (e.g. queuing) otherwise. A session another listener owns is that
// listener's to deliver, so this one acknowledges the message silently. Only a
// session this listener prompts (promptsHere) is asked of the delivery ledger
// first, so a self-subscribed session or another listener's costs no ledger
// write and is acked as before.
func agentSubjectDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	sessionID := strings.TrimPrefix(item.Topic, contracts.AgentTopicPrefix)
	interest, err := cfg.registry.Get(sessionID)
	var interestPtr *store.Interest
	if err == nil {
		interestPtr = &interest
	}
	var attempt dedupe.Attempt
	if entry, err := cfg.sessions.Get(sessionID); err == nil && promptsHere(cfg.machineID, interestPtr, &entry) {
		var deliver bool
		var retryIn time.Duration
		if attempt, deliver, retryIn = ledgerAttempt(cfg, message, item, sessionID, false); !deliver {
			if retryIn > 0 {
				cfg.messagesNAKed.Inc()
				message.retryAfter(retryIn)
			} else {
				message.finalize(false)
			}
			return
		}
	}
	deliveryTimer := metrics.NewTimer()
	result := session.HandleAgentMessage(item, sessionID, cfg.machineID, interestPtr, message.deliverer(cfg))
	if result.OwnedElsewhere {
		// Every listener on the bus reads each inbox message from its own durable, and the owner
		// delivers it from its copy: a no_holder exception here would report a delivered message
		// as undeliverable, once per listener that does not own the session.
		clearLedgerAttempt(cfg, item, attempt)
		cfg.logger.DeliveryLog(slog.LevelDebug, "listener agent session owned by another listener", sessionID, item.Topic, item.EventID, "owned_elsewhere")
		message.finalize(false)
		return
	}
	if !result.Delivered && message.cancelled() {
		// The receiver cut the prompt short on a shutdown: neither a failed delivery to report nor
		// a session found nowhere (the path without a stored subscription reports any failed prompt
		// as that), but a retry for whichever pod holds the lease next.
		clearLedgerAttempt(cfg, item, attempt)
		cfg.logger.DeliveryLog(slog.LevelWarn, "listener agent delivery cancelled at shutdown", sessionID, item.Topic, item.EventID, "retry")
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
			clearAttempt:    result.ShouldNAK,
		})
	}
	if result.Delivered {
		deliveryStatus := "delivered"
		deliveryLog := "listener agent delivered"
		if result.Skipped {
			deliveryStatus = "skipped"
			deliveryLog = "listener agent skipped push"
		}
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			metricStatus: deliveryStatus,
			duration:     &deliveryTimer,
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, deliveryLog, sessionID, item.Topic, item.EventID, deliveryStatus)
			},
			recordDedupe: true,
		})
	} else if result.Err == nil {
		if !publishNoHolderExceptionOrNak(cfg, message, item, sessionID, attempt) {
			return
		}
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			metricStatus: "skipped",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener agent session not found anywhere", sessionID, item.Topic, item.EventID, "skipped")
			},
		})
	}
	if result.ShouldNAK {
		cfg.messagesNAKed.Inc()
		message.finalize(true)
	} else {
		message.finalize(false)
	}
}

// fanoutDelivery delivers a general-topic envelope to every session that
// registered interest in it (registry.Match), aggregating per-recipient
// failures into a single NAK/ACK decision for the underlying message. Only a
// recipient this listener prompts (promptsHere) is asked of the delivery
// ledger, after the echo check and before its delivery-mode checks, so a
// self-subscribed recipient costs no ledger write, and a duplicate of a
// delivered message is skipped before its recipient's current capabilities
// can refuse it. The message is retried after the longest wait any recipient
// asks for: durableRetryDelay for a failure, or until another pod's attempt
// can be taken over.
func fanoutDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	items := cfg.registry.Match(cfg.machineID, item.Topic)
	if len(items) == 0 {
		if !publishNoHolderExceptionOrNak(cfg, message, item, "", dedupe.Attempt{}) {
			return
		}
		cfg.logger.Info("listener no matching interests", slog.String("topic", item.Topic))
		message.finalize(false)
		return
	}
	var itemPayload *string
	if item.Payload != "" {
		itemPayload = &item.Payload
	}
	mode, deliveryErr := frameDeliveryMode(itemPayload)

	var retryIn time.Duration
	var deadDeliveries int
	for _, interest := range items {
		if item.SourceSession != "" && item.SourceSession == interest.SessionID {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				metricStatus: "skipped",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelInfo, "listener skip echo", interest.SessionID, item.Topic, item.EventID, "skipped")
				},
			})
			continue
		}
		targetEntry, targetErr := cfg.sessions.Get(interest.SessionID)
		targetLive := targetErr == nil
		var target *session.SessionEntry
		if targetLive {
			target = &targetEntry
		}
		var attempt dedupe.Attempt
		if promptsHere(cfg.machineID, &interest, target) {
			var deliver bool
			var wait time.Duration
			if attempt, deliver, wait = ledgerAttempt(cfg, message, item, interest.SessionID, false); !deliver {
				retryIn = max(retryIn, wait)
				continue
			}
		}
		if deliveryErr != nil {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				attempt:      attempt,
				metricStatus: "failed",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelWarn, "listener delivery mode cannot be read unambiguously", interest.SessionID, item.Topic, item.EventID, "failed")
				},
				exceptionReason:               "delivery_failed",
				fanoutRefusal:                 true,
				exceptionFailureClearsAttempt: true,
			})
			continue
		}
		if mode != "" && targetLive && !hasCapability(targetEntry.Capabilities, mode) {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				attempt:      attempt,
				metricStatus: "failed",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelWarn, "listener subscriber does not advertise delivery mode", interest.SessionID, item.Topic, item.EventID, "failed", slog.String("mode", mode))
				},
				exceptionReason:               "delivery_failed",
				fanoutRefusal:                 true,
				exceptionFailureClearsAttempt: true,
			})
			continue
		}
		deliveryTimer := metrics.NewTimer()
		delivery, err := message.deliverer(cfg).DeliverWithResultForEntry(item, interest, target)
		if err != nil && message.cancelled() {
			// The receiver cut the prompt short on a shutdown: a retry for whichever pod holds the
			// lease next, whatever the session's state, with no failure to report.
			clearLedgerAttempt(cfg, item, attempt)
			cfg.logger.DeliveryLog(slog.LevelWarn, "listener delivery cancelled at shutdown", interest.SessionID, item.Topic, item.EventID, "retry")
			retryIn = max(retryIn, durableRetryDelay)
			continue
		}
		if err != nil {
			retryable := shouldNAKFanoutDelivery(targetLive, err)
			if !retryable {
				deadDeliveries++
			}
			if applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				attempt:      attempt,
				metricStatus: "failed",
				duration:     &deliveryTimer,
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelError, "listener delivery failed", interest.SessionID, item.Topic, item.EventID, "failed", slog.String("error", err.Error()))
				},
				exceptionReason:               "delivery_failed",
				exceptionFailureClearsAttempt: true,
				exceptionFailureRetries:       true,
				clearAttempt:                  retryable,
				retry:                         retryable,
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
				attempt:      attempt,
				metricStatus: deliveryStatus,
				duration:     &deliveryTimer,
				recordDedupe: true,
			})
		}
	}
	if deadDeliveries > 0 {
		cfg.logger.Info("listener skipped dead session deliveries", slog.String("topic", item.Topic), slog.Int("count", deadDeliveries))
	}
	if retryIn > 0 {
		cfg.messagesNAKed.Inc()
		message.retryAfter(retryIn)
	} else {
		message.finalize(false)
	}
}
