package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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
	// to defaultUnplaceableRetryWindow. It is spent as whole deferrals of unplaceableRetryDelay
	// each, counted from the message's first delivery, so a window shorter than one delay - a
	// zero one, what a caller that does not set this field asks for - retries nothing: every
	// unplaceable delivery is given up on its first attempt. A push that failed is retried
	// within it at most pushFailedDeferrals times, however long it is.
	unplaceableRetryWindow time.Duration
	// deferredUnplaceable is the set of agent deliveries this listener is holding deferred right
	// now, the budget unplaceableDeferralCap bounds. It is required: main.go makes one and sets
	// it once, in its deliveryConfig, so every handler built from that config - the role lane's
	// core handler, the durable's, and the one the self-health rebind builds after a NATS flap -
	// draws on the one budget. listenerDeliveryHandler refuses a config without one rather than
	// drawing a private set, since a set per handler is a second budget beside the first on the
	// very rebind this retry exists for. A test that builds a config sets its own.
	deferredUnplaceable *unplaceableDeferrals
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
// failure notice never went out must not be silently ACKed. Its one caller is
// the fanout path with no matching interests at all, which has no attempt-cache
// entry to roll back; listenerDeliveryHandler routes every role and agent topic
// elsewhere, so a fanout topic is never a control topic and the guard below
// returns true for it before anything is published. The direct-agent give-up
// does not use it: that give-up is terminal whether or not its notice goes out
// (agentSubjectDelivery), since a NAK there would redeliver a message whose push
// allowance is spent.
func publishNoHolderExceptionOrNak(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) bool {
	if !isControlTopic(item.Topic) || isExceptionsTopic(item.Topic) {
		return true
	}
	if err := publishDeliveryException(cfg.client, item, "no_holder", "", false); err != nil {
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
//
// cfg arrives by value, so every handler built from one config shares the deferral set the
// unplaceable retry bounds itself with only because the field is a pointer; a config without one
// is a programming error, refused here at construction rather than at the first deferral.
func listenerDeliveryHandler(cfg listenerDeliveryHandlerConfig) func(deliveryMessage) {
	if cfg.deferredUnplaceable == nil {
		panic("listener delivery handler built without its deferral set (deferredUnplaceable)")
	}
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

// deliverySessionReadTimeout is delivery's own bound on its read of the target session from the
// bucket. It is the role paths' bound (roleHolderReadTimeout, api.go) today without being tied to
// it, and it can be looser than the send route's (sendSessionReadTimeout) because no HTTP caller
// is waiting on this read: the cost of waiting is a message held, not a request kept open, and
// the alternative - taking the cache's miss for an answer - ACKs the message away.
const deliverySessionReadTimeout = 2 * time.Second

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

// unplaceableDeferrals is the set of agent deliveries this listener holds deferred right now,
// keyed on each message's stream sequence (nats.MsgMetadata.Sequence.Stream), and the budget
// unplaceableDeferralCap bounds. It is keyed rather than counted so that only a slot this
// listener took ever comes back: a redelivery is no evidence that this listener deferred the
// message - the interest path's ShouldNAK and an expired AckWait redeliver too, and they come in
// step with deferrals (a local session whose server is down produces interest-path NAKs and
// direct-path deferrals at once) - so a count released on every redelivery could read zero while
// the cap's worth of messages were still held.
//
// What the set guarantees: it never holds more than unplaceableDeferralCap entries, an entry is
// added only by a deferral this listener made and removed only by that same message's next
// delivery, so at any moment the messages this process is holding deferred number at most the
// cap. What it cannot know, since it is this process's memory and not the stream's:
//   - a listener restart forgets it. The previous process's deferrals stay pending in the durable
//     until each one's NAK delay ends and it is redelivered, when the new process either takes a
//     slot for it or gives it up; until then the durable can hold the old process's cap beside
//     the new one's, which TestUnplaceableRetryWindowFitsTheBoundsItDependsOn keeps under
//     consumerMaxAckPending (twice the cap is below it).
//   - a message deferred and never delivered here again - one the stream drops first, or whose
//     redelivery reaches some other subscriber - keeps its entry for the life of the process.
//     Such an entry holds no slot of the durable's, and the set's own bound caps the cost: at
//     worst every entry is one of these, and this listener gives each unplaceable message up on
//     its first delivery, with its record, until it restarts.
type unplaceableDeferrals struct {
	mu   sync.Mutex
	held map[uint64]struct{}
}

func newUnplaceableDeferrals() *unplaceableDeferrals {
	return &unplaceableDeferrals{held: make(map[uint64]struct{}, unplaceableDeferralCap)}
}

// take records the message at stream sequence seq as deferred and reports whether it fits inside
// unplaceableDeferralCap. Past the cap it records nothing and reports false, and the caller gives
// the message up. A sequence already held reports true without a second slot: one message holds
// one slot however many deliveries of it race. The check and the insert are one critical section,
// so two deliveries racing for the last slot cannot both take it.
func (d *unplaceableDeferrals) take(seq uint64) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.held[seq]; ok {
		return true
	}
	if len(d.held) >= unplaceableDeferralCap {
		return false
	}
	d.held[seq] = struct{}{}
	return true
}

// release gives back the slot the message at stream sequence seq holds, and nothing when it holds
// none: a delivery this listener never deferred has no slot to return.
func (d *unplaceableDeferrals) release(seq uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.held, seq)
}

// size reports how many slots are held, for a test to read the budget back.
func (d *unplaceableDeferrals) size() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.held)
}

// agentSubjectDelivery delivers directly to the session named by the topic
// (notifications.agent.<session>), pushing over HTTP/WebSocket when the
// session is live and falling back to whatever session.HandleAgentMessage
// decides (e.g. queuing) otherwise.
func agentSubjectDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	sessionID := strings.TrimPrefix(item.Topic, contracts.AgentTopicPrefix)
	// The JetStream metadata carries everything the unplaceable branch below bounds this
	// message by - its delivery count, against the deferrals the window buys and against
	// unplaceableMaxDeliveries - and its stream sequence, the key of the slot a deferral of it
	// holds, so it is read once here for both. A core delivery has none, and a durable message
	// whose metadata will not parse can be bounded by nothing, which the branch below gives up
	// on rather than retrying without end.
	var metadata *nats.MsgMetadata
	if message.mode == durableDelivery {
		if md, mdErr := message.msg.Metadata(); mdErr == nil {
			metadata = md
		}
	}
	// A message this listener deferred has left the deferred state by the time it is delivered
	// again, so its slot goes back before any disposition below takes one: the delivery itself is
	// what freed it, not what this attempt decides to do with the message. The release is keyed
	// on the message, so a delivery this listener never deferred - a first one, or a redelivery
	// another branch's NAK or an expired AckWait produced - gives nothing back.
	if metadata != nil {
		cfg.deferredUnplaceable.release(metadata.Sequence.Stream)
	}
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
	// read that did not answer, which is no evidence of absence at all - the bucket was never
	// consulted, so nothing about this session was learned, and an attempt that learned nothing
	// can never be the ground for giving a message up. Neither needs to be told from the other
	// below: both leave this delivery with no entry to push through, and an entry-less attempt
	// reaches the retry decision only as ReasonNoSessionRecord, which already holds the retry
	// open (session.HandleAgentMessage: the registry path reports that reason, and the interest
	// path NAKs with an error, which never reaches that branch).
	entry, entryErr := liveSessionEntry(context.Background(), cfg.sessions, sessionID, deliverySessionReadTimeout, registered)
	var entryPtr *session.SessionEntry
	if entryErr == nil {
		entryPtr = &entry
	}
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
		// ShouldNAK with an Err set and are answered above. A read that did not answer is no
		// evidence of absence and must not end the retries, and it does not: it leaves no
		// entry, and an attempt with no entry reports ReasonNoSessionRecord here, so the reason
		// alone already holds the window open for it.
		placementMayChange := result.Reason != session.ReasonNone
		deferred := false
		if metadata != nil && placementMayChange && metadata.NumDelivered <= windowDeferrals &&
			metadata.NumDelivered < unplaceableMaxDeliveries {
			// The slot is this message's own, keyed on its stream sequence, so only this
			// message's next delivery gives it back. A full set takes nothing, and the delivery
			// falls through to the ordinary give-up below.
			deferred = cfg.deferredUnplaceable.take(metadata.Sequence.Stream)
		}
		if deferred {
			// No exception and no delivery counter: the message is not disposed of yet, and
			// publishing the no_holder notice per attempt would send a dozen of them, each one
			// read back by every listener in the fleet. The attempt-cache entry recorded above
			// must go, or the redelivery this NAK asks for is swallowed as an attempt-dedupe
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
			return
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
		// durable's AckWait and well inside attemptCacheTTL - and the message is ACKed below.
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
		if !publishNoHolderExceptionOrNak(cfg, message, item) {
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
