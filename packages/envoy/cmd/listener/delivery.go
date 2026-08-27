package main

import (
	"encoding/json"
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

func shouldNAKFanoutDelivery(sessions *session.SessionRegistry, sessionID string, err error) bool {
	if err == nil {
		return false
	}
	return isSessionLive(sessions, sessionID)
}

type listenerDeliveryHandlerConfig struct {
	client            *bus.Client
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
func (message deliveryMessage) finalize(retry bool) {
	if message.mode != durableDelivery {
		return
	}
	if retry {
		_ = message.msg.NakWithDelay(30 * time.Second)
		return
	}
	_ = message.msg.Ack()
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
	OriginalTopic  string `json:"original_topic"`
	EventID        string `json:"event_id"`
	Reason         string `json:"reason"`
	PayloadSummary string `json:"payload_summary"`
	Payload        string `json:"payload"`
	DedupeKey      string `json:"dedupe_key"`
	Source         string `json:"source"`
	SourceSession  string `json:"source_session"`
}

func isControlTopic(topic string) bool {
	return strings.HasPrefix(topic, contracts.RoleTopicPrefix) || strings.HasPrefix(topic, contracts.AgentTopicPrefix)
}

func isExceptionsTopic(topic string) bool {
	return strings.HasPrefix(topic, "notifications.envoy.exceptions.")
}

func publishDeliveryException(client *bus.Client, item contracts.Envelope, reason string) error {
	payload, err := json.Marshal(deliveryExceptionPayload{
		OriginalTopic:  item.Topic,
		EventID:        item.EventID,
		Reason:         reason,
		PayloadSummary: item.PayloadSummary,
		Payload:        item.Payload,
		DedupeKey:      item.DedupeKey,
		Source:         item.Source,
		SourceSession:  item.SourceSession,
	})
	if err != nil {
		return fmt.Errorf("marshal delivery exception: %w", err)
	}
	exceptionID := id.New()
	exception := contracts.Envelope{
		EventID:        exceptionID,
		Source:         "envoy",
		SourceEventID:  item.EventID,
		Topic:          "notifications.envoy.exceptions." + item.Topic,
		DedupeKey:      "envoy.exception." + exceptionID,
		IssuedAt:       contracts.NowMillis(),
		PayloadSummary: "delivery exception: " + reason,
		Payload:        string(payload),
		TraceID:        id.New(),
	}
	if err := exception.Validate(); err != nil {
		return fmt.Errorf("validate delivery exception: %w", err)
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
	// publishDeliveryException for control topics (gated the same way for
	// every path: isControlTopic && !isExceptionsTopic).
	exceptionReason string
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
	if outcome.exceptionReason != "" && isControlTopic(item.Topic) && !isExceptionsTopic(item.Topic) {
		if err := publishDeliveryException(cfg.client, item, outcome.exceptionReason); err != nil {
			cfg.logger.Error("listener exception publish failed", slog.String("error", err.Error()), slog.String("topic", item.Topic))
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
	if err := publishDeliveryException(cfg.client, item, "no_holder"); err != nil {
		if sessionID != "" {
			cfg.attemptCache.Clear(item.DedupeKey, sessionID)
		}
		cfg.logger.Error("listener exception publish failed", slog.String("error", err.Error()), slog.String("topic", item.Topic))
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

// roleTopicDelivery arbitrates a role-lane envelope to whichever session
// currently holds the role, forwarding it over core NATS request-reply so
// the sender learns immediately whether the holder received it. Every
// branch ACKs: role lanes have no durable transit to retry against, so a
// failed forward is reported via a delivery exception instead of a NAK.
func roleTopicDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	if strings.HasPrefix(item.DedupeKey, roleForwardDedupePrefix) {
		message.finalize(false)
		return
	}
	role := strings.TrimPrefix(item.Topic, contracts.RoleTopicPrefix)
	sessionID, err := cfg.registry.RoleHolder(role)
	if err != nil {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.Error("listener role holder lookup failed", slog.String("role", role), slog.String("error", err.Error()))
			},
			exceptionReason: "delivery_failed",
		})
		message.finalize(false)
		return
	}
	if sessionID == "" {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			metricStatus: "skipped",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role has no holder", "", item.Topic, item.EventID, "skipped")
			},
			exceptionReason: "no_holder",
		})
		message.finalize(false)
		return
	}
	now := time.Now().UnixMilli()
	holder, holderErr := cfg.sessions.Get(sessionID)
	if holderErr != nil || holder.UpdatedAt <= 0 || now-holder.UpdatedAt >= int64(session.ClaimStaleAfter/time.Millisecond) {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role holder is not live", sessionID, item.Topic, item.EventID, "failed")
			},
			exceptionReason: "delivery_failed",
		})
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
	cfg.attemptCache.Record(item.DedupeKey, sessionID)
	forwarded := item
	forwarded.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
	if err := cfg.client.RequestCoreTo(contracts.AgentSubject(sessionID), forwarded, roleReceiptTimeout); err != nil {
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
	cfg.attemptCache.Record(item.DedupeKey, sessionID)
	deliveryTimer := metrics.NewTimer()
	result := session.HandleAgentMessage(item, sessionID, cfg.machineID, interestPtr, cfg.deliverer)
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
		if !publishNoHolderExceptionOrNak(cfg, message, item, sessionID) {
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
		cfg.attemptCache.Record(item.DedupeKey, interest.SessionID)
		deliveryTimer := metrics.NewTimer()
		delivery, err := cfg.deliverer.DeliverWithResult(item, interest)
		if err != nil {
			retryable := shouldNAKFanoutDelivery(cfg.sessions, interest.SessionID, err)
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
