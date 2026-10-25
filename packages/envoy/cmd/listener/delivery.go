package main

import (
	"context"
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
	forwardRole       func(subject string, item contracts.Envelope, timeout time.Duration) error
	registry          *store.Registry
	sessions          *session.SessionRegistry
	machineID         string
	deliverer         *session.Deliverer
	dedupeCache       *dedupe.Cache
	attemptCache      *dedupe.Cache
	logger            *logging.Logger
	messagesReceived  *metrics.Counter
	messagesDelivered *metrics.Counter
	messagesNAKed     *metrics.Counter
	deliveryDuration  *metrics.Histogram
	// unplaceableRetryWindow is how long an agent delivery this listener cannot place is held
	// in the stream and redelivered before it is given up with a record of its own
	// (agentSubjectDelivery). main.go reads it from ENVOY_UNPLACEABLE_RETRY_WINDOW, defaulting
	// to defaultUnplaceableRetryWindow. A zero window - what a caller that does not set this
	// field asks for - retries nothing: every unplaceable delivery is given up on its first
	// attempt.
	unplaceableRetryWindow time.Duration
}

type deliveryMode uint8

const (
	coreDelivery deliveryMode = iota
	durableDelivery
)

type deliveryMessage struct {
	data []byte
	mode deliveryMode
	msg  *nats.Msg
}

// finalize makes core delivery's loss-on-failure semantics explicit: only a
// durable JetStream message receives an acknowledgement or retry request.
// A retry takes the standard delay; a path that needs its own asks for it
// through finalizeAfter.
func (message deliveryMessage) finalize(retry bool) {
	if retry {
		message.finalizeAfter(30 * time.Second)
		return
	}
	if message.mode != durableDelivery {
		return
	}
	_ = message.msg.Ack()
}

// finalizeAfter asks the stream to redeliver this message after delay, for a path whose own
// bound sets the pace of its retries rather than finalize's standard one. Like finalize, it
// does nothing to a core message: nothing redelivers one, so there is no request to make.
func (message deliveryMessage) finalizeAfter(delay time.Duration) {
	if message.mode != durableDelivery {
		return
	}
	_ = message.msg.NakWithDelay(delay)
}

func jetStreamDeliveryHandler(cfg listenerDeliveryHandlerConfig) nats.MsgHandler {
	handler := listenerDeliveryHandler(cfg)
	return func(msg *nats.Msg) {
		handler(deliveryMessage{data: msg.Data, mode: durableDelivery, msg: msg})
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
	exceptionID := id.New()
	exception := contracts.Envelope{
		EventID:        exceptionID,
		Source:         "envoy",
		SourceEventID:  item.EventID,
		Topic:          bus.ExceptionTopicPrefix + item.Topic,
		DedupeKey:      "envoy.exception." + exceptionID,
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
// which cache entries to update. Each branch of the three delivery paths
// below produces one of these; applyDeliveryOutcome performs the mechanical
// side effects so they happen in exactly one place.
type deliveryOutcome struct {
	// sessionID keys the attempt/dedupe cache updates below; "" when this
	// outcome has none (e.g. a role that was never assigned a holder).
	sessionID string
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
	// attempt-cache entry, independent of (and possibly in addition to)
	// clearAttempt below.
	exceptionFailureClearsAttempt bool

	// clearAttempt unconditionally rolls back the attempt-cache record.
	clearAttempt bool
	// recordDedupe marks the dedupe-cache entry as sent.
	recordDedupe bool
	// retry is this outcome's baseline retry request, before any exception-
	// publish escalation above is folded in.
	retry bool
}

// applyDeliveryOutcome performs the bookkeeping every delivery attempt
// shares — metrics, duration, logging, exception publication, and
// attempt/dedupe cache updates — and reports whether the attempt should be
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
	if outcome.exceptionReason != "" && !isExceptionsTopic(item.Topic) && (isControlTopic(item.Topic) || outcome.fanoutRefusal) {
		if err := publishDeliveryException(cfg.client, item, outcome.exceptionReason, outcome.sessionID, outcome.fanoutRefusal); err != nil {
			cfg.logger.Error(exceptionPublishFailedLine, slog.String("error", err.Error()), slog.String("topic", item.Topic))
			if outcome.exceptionFailureClearsAttempt {
				cfg.attemptCache.Clear(item.DedupeKey, outcome.sessionID)
			}
			if outcome.exceptionFailureRetries {
				retry = true
			}
		}
	}
	if outcome.clearAttempt {
		cfg.attemptCache.Clear(item.DedupeKey, outcome.sessionID)
	}
	if outcome.recordDedupe {
		cfg.dedupeCache.Record(item.DedupeKey, outcome.sessionID)
	}
	return retry
}

// publishNoHolderExceptionOrNak publishes a "no_holder" delivery exception
// for control topics and, if publishing itself fails, escalates to an
// immediate NAK by finalizing message and reporting false — a message whose
// failure notice never went out must not be silently ACKed. Shared by the
// direct-agent path (no live session for the target) and the fanout path (no
// matching interests at all). sessionID is "" when this outcome has no
// attempt-cache entry to roll back (the fanout no-interests case).
func publishNoHolderExceptionOrNak(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope, sessionID string) bool {
	if !isControlTopic(item.Topic) || isExceptionsTopic(item.Topic) {
		return true
	}
	if err := publishDeliveryException(cfg.client, item, "no_holder", sessionID, false); err != nil {
		if sessionID != "" {
			cfg.attemptCache.Clear(item.DedupeKey, sessionID)
		}
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
// expired (the plugin heartbeats every two minutes, so two missed
// heartbeats means the claiming process is very likely gone) — stricter
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
// dedupe/attempt-cache skip -- a duplicate of an envelope already forwarded
// (or already being forwarded) is disposed of first, with no forward and no
// exception -- and before forwarding, it applies the same capability guard
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
	if cfg.dedupeCache.Seen(item.DedupeKey, sessionID) || cfg.attemptCache.Seen(item.DedupeKey, sessionID) {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "dedupe",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener role dedupe skip", sessionID, item.Topic, item.EventID, "dedupe", slog.String("dedupe_key", item.DedupeKey))
			},
		})
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
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role holder does not advertise delivery mode", sessionID, item.Topic, item.EventID, "failed", slog.String("mode", mode))
			},
			exceptionReason: "delivery_failed",
		})
		message.finalize(false)
		return
	}
	cfg.attemptCache.Record(item.DedupeKey, sessionID)
	forwarded := item
	forwarded.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
	if err := cfg.forwardRole(contracts.AgentSubject(sessionID), forwarded, roleReceiptTimeout); err != nil {
		if errors.Is(err, bus.ErrReceiptTimeout) {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    sessionID,
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
		metricStatus: "delivered",
		log: func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelInfo, "listener role forwarded", sessionID, item.Topic, item.EventID, "delivered")
		},
		recordDedupe: true,
	})
	message.finalize(false)
}

// deliverySessionReadTimeout bounds the delivery path's read of the target session from the
// bucket. Delivery answers no HTTP caller of its own, so it carries the role paths' bound rather
// than the send route's tighter one: the cost of waiting is a message held, not a request kept
// open, and the alternative - taking the cache's miss for an answer - ACKs the message away.
const deliverySessionReadTimeout = 2 * time.Second

// unplaceableRetryDelay is the delay an agent delivery this listener could not place asks for
// before the stream redelivers it. It is short enough that a window's worth of them is a dozen
// attempts rather than two - at the 30 s finalize(true) takes, a minute is one retry - and long
// enough that a session whose record is still propagating, or whose server is still binding its
// serve port, has time to finish between two of them.
const unplaceableRetryDelay = 5 * time.Second

// unplaceableMaxDeliveries bounds those retries by delivery count as well as by age, two short
// of the durable's own MaxDeliver (consumerMaxDeliver, durable.go: 20, so 18 here). JetStream
// stops redelivering at max_deliver silently - nothing in this repository consumes its advisory
// - so this listener must give up, and write its give-up record, before the stream does. The
// two-delivery margin covers the deliveries this handler does not ask for: one whose process
// died before it finalized and whose AckWait therefore expired still counts against
// max_deliver.
const unplaceableMaxDeliveries = consumerMaxDeliver - 2

// defaultUnplaceableRetryWindow is the window main.go uses when ENVOY_UNPLACEABLE_RETRY_WINDOW
// is unset or unreadable: a minute of retries, which at unplaceableRetryDelay is 12 deliveries,
// 14 with the margin above and so inside consumerMaxDeliver, and well inside the attempt
// cache's TTL (attemptCacheTTL, main.go), the entry whose clearing keeps each redelivery from
// being swallowed as an attempt-dedupe skip. TestUnplaceableRetryWindowFitsTheBoundsItDependsOn
// pins both.
const defaultUnplaceableRetryWindow = 60 * time.Second

// agentSubjectDelivery delivers directly to the session named by the topic
// (notifications.agent.<session>), pushing over HTTP/WebSocket when the
// session is live and falling back to whatever session.HandleAgentMessage
// decides (e.g. queuing) otherwise.
func agentSubjectDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	sessionID := strings.TrimPrefix(item.Topic, contracts.AgentTopicPrefix)
	if cfg.dedupeCache.Seen(item.DedupeKey, sessionID) {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "dedupe",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener dedupe skip", sessionID, item.Topic, item.EventID, "dedupe", slog.String("dedupe_key", item.DedupeKey))
			},
		})
		message.finalize(false)
		return
	}
	if cfg.attemptCache.Seen(item.DedupeKey, sessionID) {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "dedupe",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener attempt-dedupe skip", sessionID, item.Topic, item.EventID, "skipped", slog.String("dedupe_key", item.DedupeKey))
			},
		})
		message.finalize(false)
		return
	}
	interest, err := cfg.registry.Get(sessionID)
	var interestPtr *store.Interest
	if err == nil {
		interestPtr = &interest
	}
	// The session record delivery pushes through is read from the session bucket itself, as the
	// send route reads it: this listener's cache trails the bucket it follows, and is still being
	// refilled after its bus connection came back, so a session registered a moment ago, or one
	// this listener has not re-read since the reconnect, is in the bucket before it is in the
	// cache - and a message dropped on that miss is gone, since it is already ACKed away.
	// nats.ErrKeyNotFound is the bucket's own word that no record exists; any other error is a
	// read that did not answer. Both leave this delivery with no entry to push through, so
	// session.HandleAgentMessage reports the same ReasonNoSessionRecord for the two, and only
	// sessionReadUnanswered tells them apart below: the first is evidence the session is gone,
	// the second is no evidence at all - the bucket was never consulted, so nothing about this
	// session was learned, and an attempt that learned nothing can never be the ground for
	// giving a message up.
	entry, entryErr := liveSessionEntry(context.Background(), cfg.sessions, sessionID, deliverySessionReadTimeout, registered)
	var entryPtr *session.SessionEntry
	if entryErr == nil {
		entryPtr = &entry
	}
	sessionReadUnanswered := entryErr != nil && !errors.Is(entryErr, nats.ErrKeyNotFound)
	cfg.attemptCache.Record(item.DedupeKey, sessionID)
	deliveryTimer := metrics.NewTimer()
	result := session.HandleAgentMessage(item, sessionID, cfg.machineID, interestPtr, entryPtr, cfg.deliverer)
	if result.Err != nil {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
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
			metricStatus: deliveryStatus,
			duration:     &deliveryTimer,
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, deliveryLog, sessionID, item.Topic, item.EventID, deliveryStatus)
			},
			recordDedupe: true,
		})
	} else if result.Err == nil {
		// Nothing placed this message and nothing failed: the session has no record this
		// listener could find, or its record names nothing to push to. Either can be true of a
		// session that is about to be placeable - a record still propagating from the bucket, a
		// server still binding its serve port - so the message is kept in the stream and
		// redelivered for as long as cfg.unplaceableRetryWindow, and only then given up, with a
		// record and a counter of its own rather than the ACKed skip that lost it.
		//
		// reason names which of the two causes this is, for the operator and for the counter
		// label. session.ReasonNone shares no_push_target's: reaching here without an error
		// means an entry was resolved (a nil one is ReasonNoSessionRecord) and the push to it
		// did not land, which is the same fact an operator acts on - a record exists and
		// nothing could be pushed to it.
		reason := "no_push_target"
		giveUpStatus := "unplaceable_no_push_target"
		if result.Reason == session.ReasonNoSessionRecord {
			reason = "no_session_record"
			giveUpStatus = "unplaceable_no_record"
		}
		// Only a durable message is retried: the role lane is this listener's one core
		// subscription (main.go), and nothing redelivers a core message, so a NAK there would
		// drop it with no record at all. The JetStream metadata carries the two bounds - the
		// message's age against the window, and its delivery count against
		// unplaceableMaxDeliveries - and a message that does not carry it cannot be bounded by
		// either, so it is given up now rather than retried without end.
		var metadata *nats.MsgMetadata
		var age time.Duration
		if message.mode == durableDelivery {
			if md, mdErr := message.msg.Metadata(); mdErr == nil {
				metadata = md
				age = time.Since(md.Timestamp)
			}
		}
		// A read that did not answer holds the retry open on its own for whatever is left of
		// the window: it is no evidence of absence, whatever reason this attempt reports. What
		// does not hold it open is session.ReasonNone, the one outcome here another delivery
		// cannot change by itself - a record this listener did resolve, with a serve port,
		// whose push failed on the wire. The interest path NAKs that failure itself
		// (internal/session/handler.go), so repeating it here would only repeat a refused HTTP
		// request.
		placementMayChange := sessionReadUnanswered || result.Reason != session.ReasonNone
		if metadata != nil && placementMayChange && age < cfg.unplaceableRetryWindow &&
			metadata.NumDelivered < unplaceableMaxDeliveries {
			// No exception and no delivery counter: the message is not disposed of yet, and
			// publishing the no_holder notice per attempt would send a dozen of them, each one
			// read back by every listener in the fleet. The attempt-cache entry recorded above
			// must go, or the redelivery this NAK asks for is swallowed as an attempt-dedupe
			// skip before it reaches the session at all.
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID: sessionID,
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelInfo, "listener agent delivery deferred", sessionID, item.Topic, item.EventID, "deferred",
						slog.String("reason", reason),
						slog.Uint64("attempt", metadata.NumDelivered),
						slog.Int64("age_ms", age.Milliseconds()),
					)
				},
				clearAttempt: true,
			})
			cfg.messagesNAKed.Inc()
			message.finalizeAfter(unplaceableRetryDelay)
			return
		}
		// Given up: the window is spent, the delivery count is reached, the metadata could not
		// be read, or this is a core delivery nothing would redeliver. The no_holder exception
		// goes out here, once per message rather than once per attempt.
		if !publishNoHolderExceptionOrNak(cfg, message, item, sessionID) {
			return
		}
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: giveUpStatus,
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener agent delivery given up", sessionID, item.Topic, item.EventID, giveUpStatus, slog.String("reason", reason))
			},
			clearAttempt: true,
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
// failures into a single NAK/ACK decision for the underlying message.
func fanoutDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	items := cfg.registry.Match(cfg.machineID, item.Topic)
	if len(items) == 0 {
		if !publishNoHolderExceptionOrNak(cfg, message, item, "") {
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

	var failed bool
	var deadDeliveries int
	for _, interest := range items {
		if cfg.dedupeCache.Seen(item.DedupeKey, interest.SessionID) {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				metricStatus: "dedupe",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelInfo, "listener dedupe skip", interest.SessionID, item.Topic, item.EventID, "dedupe", slog.String("dedupe_key", item.DedupeKey))
				},
			})
			continue
		}
		if cfg.attemptCache.Seen(item.DedupeKey, interest.SessionID) {
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				metricStatus: "dedupe",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelInfo, "listener attempt-dedupe skip", interest.SessionID, item.Topic, item.EventID, "skipped", slog.String("dedupe_key", item.DedupeKey))
				},
			})
			continue
		}
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
		if deliveryErr != nil {
			cfg.attemptCache.Record(item.DedupeKey, interest.SessionID)
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
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
		targetEntry, targetErr := cfg.sessions.Get(interest.SessionID)
		targetLive := targetErr == nil
		var target *session.SessionEntry
		if targetLive {
			target = &targetEntry
		}
		if mode != "" && targetLive && !hasCapability(targetEntry.Capabilities, mode) {
			cfg.attemptCache.Record(item.DedupeKey, interest.SessionID)
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
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
		cfg.attemptCache.Record(item.DedupeKey, interest.SessionID)
		deliveryTimer := metrics.NewTimer()
		delivery, err := cfg.deliverer.DeliverWithResultForEntry(item, interest, target)
		if err != nil {
			retryable := shouldNAKFanoutDelivery(targetLive, err)
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
				exceptionReason:               "delivery_failed",
				exceptionFailureClearsAttempt: true,
				exceptionFailureRetries:       true,
				clearAttempt:                  retryable,
				retry:                         retryable,
			}) {
				failed = true
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
				recordDedupe: true,
			})
		}
	}
	if deadDeliveries > 0 {
		cfg.logger.Info("listener skipped dead session deliveries", slog.String("topic", item.Topic), slog.Int("count", deadDeliveries))
	}
	if failed {
		cfg.messagesNAKed.Inc()
		message.finalize(true)
	} else {
		message.finalize(false)
	}
}
