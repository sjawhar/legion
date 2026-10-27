package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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
// to its selected agent subject. The durable agent-stream replay must not
// re-enter the arbiter with it: roleForwardCopyDelivery handles the stored copy.
const roleForwardDedupePrefix = "envoy.role.forward."

const roleReceiptTimeout = 2 * time.Second

// exceptionPublishTimeout bounds the wait for the stream's acknowledgement of a stored exception
// (publishStoredException), as the bus bounds every JetStream publish.
const exceptionPublishTimeout = 5 * time.Second

// exceptionPublishFailedLine is the line a delivery logs when its delivery exception could not be
// published. The deployed publish-failure metric filter, which the deployment repository's
// listener infrastructure defines, matches it exactly, so every site that logs it uses this constant.
const exceptionPublishFailedLine = "listener exception publish failed"

func shouldNAKFanoutDelivery(sessionLive bool, err error) bool {
	return err != nil && sessionLive
}

type listenerDeliveryHandlerConfig struct {
	client *bus.Client
	// forwardRole publishes a role-lane envelope to the holder's agent subject and returns the wait
	// for its outcome; main.go wires startRoleForward(client). The wait's bus.ErrReceiptTimeout
	// means the forward was flushed to the server and no receipt arrived inside timeout;
	// bus.ErrForwardNotStored that the stream did not acknowledge storing it; any other error — a
	// raw nats.ErrTimeout from the flush included, returned by the publish or the wait — means the
	// forward is not known to have reached the holder. storedOnly asks for a forward that counts as
	// delivered on the stream's acknowledgement alone (roleForwardStoredOnly).
	forwardRole func(subject string, item contracts.Envelope, timeout time.Duration, storedOnly bool) (wait func() error, err error)
	// roleForwards runs the role lane's forwards off its handler (roleForwarder); nil runs each one
	// inline, so a caller reads its outcome as soon as the handler returns.
	roleForwards *roleForwarder
	// servePorts pushes the durable path's messages to serving-port sessions off its handler, one
	// worker per session (servePortWorkers); nil pushes each one inline, so a caller reads its
	// outcome as soon as the handler returns.
	servePorts        *servePortWorkers
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
	// settle acknowledges a durable message once its handler and the pushes it handed to
	// serving-port workers have ended.
	settle *durableSettle
}

// finalize makes core delivery's loss-on-failure semantics explicit: only a
// durable JetStream message receives an acknowledgement or retry request. A role-lane or live-lane
// message is core NATS, sent once to whoever was subscribed, so nothing acknowledges or retries it.
// A durable message is settled once the pushes its handler handed over have ended too.
func (message deliveryMessage) finalize(retry bool) {
	if message.mode != durableDelivery {
		return
	}
	message.settle.end(retry)
}

// jetStreamDeliveryHandler is the durable's handler. A message no session needs from the durable
// (durableNeedsDelivery) is acknowledged before it is decoded, each session that takes it another
// way counted as a skipped delivery, as the delivery paths count it.
func jetStreamDeliveryHandler(cfg listenerDeliveryHandlerConfig) nats.MsgHandler {
	handler := listenerDeliveryHandler(cfg)
	return func(msg *nats.Msg) {
		needed, skipped := durableNeedsDelivery(cfg, msg.Subject)
		if !needed {
			for range skipped {
				cfg.messagesDelivered.Inc([2]string{"delivery_status", "skipped"})
			}
			_ = msg.Ack()
			return
		}
		handler(deliveryMessage{data: msg.Data, mode: durableDelivery, msg: msg, settle: newDurableSettle(msg)})
	}
}

// durableNeedsDelivery reports whether the stored message on subject may need this listener's
// durable path, read from the subject alone, and when it does not, how many sessions of this
// machine take it another way (takesStoredMessagesItself). A role lane's subject always needs it. A
// session's agent subject does unless the session takes it another way or is another listener's
// (agentSubjectNeedsDelivery). Any other subject does when it matches a topic of an interest of
// this machine (the registry's subject index) whose session does not take it another way; with
// none, the fan-out would deliver it to nobody and report nothing.
func durableNeedsDelivery(cfg listenerDeliveryHandlerConfig, subject string) (needed bool, skipped int) {
	if strings.HasPrefix(subject, contracts.RoleTopicPrefix) {
		return true, 0
	}
	if sessionID, ok := strings.CutPrefix(subject, contracts.AgentTopicPrefix); ok {
		return agentSubjectNeedsDelivery(cfg, sessionID)
	}
	for _, interest := range cfg.registry.Match(cfg.machineID, subject) {
		entry, err := cfg.sessions.Get(interest.SessionID)
		if err != nil || !takesStoredMessagesItself(entry) {
			return true, 0
		}
		skipped++
	}
	return false, skipped
}

// takesStoredMessagesItself reports whether the session of entry takes a stored message without the
// durable path: on consumer delivery its consumer holds every stored message for it, and with no
// serving port a self-subscribed session's own subscription took it.
func takesStoredMessagesItself(entry session.SessionEntry) bool {
	return entry.Delivery == session.DeliveryConsumer || (entry.SelfSubscribed && entry.Port == 0)
}

// agentSubjectNeedsDelivery is durableNeedsDelivery for sessionID's agent subject: not needed when
// the session's live entry takes it another way (takesStoredMessagesItself, one skipped delivery),
// nor when its cached interest names another listener, which delivers it
// (session.HandleAgentMessage's OwnedElsewhere). Everything else is decoded and delivered: a
// serving-port or core-path session of this machine, and a session nobody has, for which the path
// raises its no_holder exception.
func agentSubjectNeedsDelivery(cfg listenerDeliveryHandlerConfig, sessionID string) (needed bool, skipped int) {
	if entry, err := cfg.sessions.Get(sessionID); err == nil && takesStoredMessagesItself(entry) {
		return false, 1
	}
	interest, cached := cfg.registry.CachedInterest(sessionID)
	return !cached || interest.MachineID == "" || interest.MachineID == cfg.machineID, 0
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

// publishDeliveryException publishes item's delivery exception for reason. origin, when not empty,
// identifies the stored message the exception is about (exceptionOrigin): an exception the stream
// stores then carries Nats-Msg-Id exception.<reason>.<origin>, so the stream keeps one copy
// however many listeners raise it for that message, each from its own durable.
func publishDeliveryException(client *bus.Client, item contracts.Envelope, reason, recipientSession string, fanoutRefusal bool, origin string) error {
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
	if origin != "" && bus.StreamCaptures(exception.Topic) {
		if err := publishStoredException(client, exception, "exception."+reason+"."+origin); err != nil {
			return fmt.Errorf("publish delivery exception: %w", err)
		}
		return nil
	}
	if err := client.Publish(exception); err != nil {
		return fmt.Errorf("publish delivery exception: %w", err)
	}
	return nil
}

// publishStoredException publishes exception to the stream under Nats-Msg-Id msgID, waiting up to
// exceptionPublishTimeout for the stream's acknowledgement; a duplicate the stream drops counts as
// published.
func publishStoredException(client *bus.Client, exception contracts.Envelope, msgID string) error {
	data, err := json.Marshal(exception)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), exceptionPublishTimeout)
	defer cancel()
	_, err = client.JS().Publish(exception.Topic, data, nats.MsgId(msgID), nats.Context(ctx))
	return err
}

// exceptionOrigin identifies the durable message message for an exception's Nats-Msg-Id: its own
// Nats-Msg-Id, else its stream sequence; "" for a message that is not durable.
func exceptionOrigin(message deliveryMessage) string {
	if message.mode != durableDelivery || message.msg == nil {
		return ""
	}
	if msgID := message.msg.Header.Get(nats.MsgIdHdr); msgID != "" {
		return msgID
	}
	if meta, err := message.msg.Metadata(); err == nil {
		return strconv.FormatUint(meta.Sequence.Stream, 10)
	}
	return ""
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
	// origin identifies the durable message an exception is about (exceptionOrigin), "" for none.
	origin string
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
		if err := publishDeliveryException(cfg.client, item, outcome.exceptionReason, outcome.sessionID, outcome.fanoutRefusal, outcome.origin); err != nil {
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
	if err := publishDeliveryException(cfg.client, item, "no_holder", sessionID, false, exceptionOrigin(message)); err != nil {
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
// session a fresh claim has already superseded. The first lookup reads the
// registry's watched copy of the role bucket (store.Registry.CachedRoleHolder);
// a retry after a superseded release reads the bucket itself, since the copy
// can trail the claim that superseded it.
func resolveCoreRoleHolder(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, role string) (string, session.SessionEntry, bool) {
	lookup := cfg.registry.CachedRoleHolder
	for range roleHolderResolutionAttempts {
		sessionID, err := lookup(role)
		lookup = cfg.registry.RoleHolder
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
// reported via a delivery exception instead of a NAK. The forward itself is
// handed over (forwardRoleMessage) and its outcome reported when it ends
// (finishRoleForward), so the role lane's handler returns without waiting
// for the holder.
func roleTopicDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	if strings.HasPrefix(item.DedupeKey, roleForwardDedupePrefix) {
		roleForwardCopyDelivery(cfg, message, item)
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
	forwardRoleMessage(cfg, roleForward{item: item, sessionID: sessionID, storedOnly: roleForwardStoredOnly(holder)})
	message.finalize(false)
}

// roleForwardStoredOnly reports whether holder answers no receipt for a role forward: a session on a
// serving port has no NATS subscriber. The stream's copy of the forward reaches its port with
// acknowledgement and redelivery: from its session consumer, which the listener's relay pushes,
// when its entry records consumer delivery, else from the owning listener's durable
// (roleForwardCopyDelivery). Its forward counts as delivered once the stream acknowledges storing it.
func roleForwardStoredOnly(holder session.SessionEntry) bool {
	return holder.Port > 0
}

// roleForwardCopyDelivery handles the stored copy of a role forward (roleForward.forwarded), which
// the durable reads on its holder's agent subject. A holder on NATS took the forward over its own
// subscription, a holder on consumer delivery takes the copy from its consumer, and a holder another
// listener owns is that listener's, so the copy is acknowledged. A serving-port holder this listener
// owns has no subscriber: the copy is pushed to its port and acknowledged only on the port's 204,
// by the holder's serving-port worker on the durable path, so its order behind the holder's earlier
// messages holds; a copy the workers cannot take goes back to the durable.
func roleForwardCopyDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	holder, ok := "", false
	if message.mode == durableDelivery {
		holder, ok = strings.CutPrefix(message.msg.Subject, contracts.AgentTopicPrefix)
	}
	if !ok {
		message.finalize(false)
		return
	}
	entry, err := cfg.sessions.Get(holder)
	if err != nil || entry.Port <= 0 || entry.Delivery == session.DeliveryConsumer {
		message.finalize(false)
		return
	}
	interest, err := cfg.registry.Get(holder)
	if err != nil {
		interest = store.Interest{SessionID: holder}
	}
	if !cfg.deliverer.OwnsServingPort(interest, entry) || cfg.dedupeCache.Seen(item.DedupeKey, holder) || cfg.attemptCache.Seen(item.DedupeKey, holder) {
		message.finalize(false)
		return
	}
	cfg.attemptCache.Record(item.DedupeKey, holder)
	if cfg.servePorts != nil {
		if handOverPush(cfg, servePortPush{item: item, sessionID: holder, settle: message.settle, forward: true, logLine: "listener role forward pushed"}) {
			message.finalize(false)
			return
		}
		cfg.messagesNAKed.Inc()
		message.finalize(true)
		return
	}
	if err := cfg.deliverer.Push(context.Background(), item, holder, entry); err != nil {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID: holder,
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role forward push refused", holder, item.Topic, item.EventID, "retrying", slog.String("error", err.Error()))
			},
			clearAttempt: true,
		})
		cfg.messagesNAKed.Inc()
		message.finalize(true)
		return
	}
	applyDeliveryOutcome(cfg, item, deliveryOutcome{
		sessionID: holder,
		log: func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelInfo, "listener role forward pushed", holder, item.Topic, item.EventID, "delivered")
		},
		recordDedupe: true,
	})
	message.finalize(false)
}

// handOverPush hands push to its session's serving-port worker (servePortWorkers) and reports
// true; the push's outcome is reported when it ends. When the workers cannot take it (they stopped,
// or are full) it clears push's attempt and reports false: the caller gives the message back to the
// durable for a retry.
func handOverPush(cfg listenerDeliveryHandlerConfig, push servePortPush) bool {
	if cfg.servePorts.enqueue(push) {
		return true
	}
	applyDeliveryOutcome(cfg, push.item, deliveryOutcome{
		sessionID: push.sessionID,
		log: func(logger *logging.Logger) {
			logger.DeliveryLog(slog.LevelWarn, "listener serving-port workers cannot take the push; the message goes back to the durable", push.sessionID, push.item.Topic, push.item.EventID, "retrying",
				slog.Int("worker_limit", servePortWorkerLimit), slog.Int("queue_limit", servePortQueueLimit))
		},
		clearAttempt: true,
	})
	return false
}

// finishRoleForward reports a role forward's outcome, err from its publish or its wait. A forward
// that reached the server and drew no receipt from the live holder inside roleReceiptTimeout
// (bus.ErrReceiptTimeout, the one error keyed on) is receipt_timeout; one the stream did not
// acknowledge storing (bus.ErrForwardNotStored), or not known to have left this process -- the flush
// timed out or failed, the publish failed -- is delivery_failed like a stale holder or an
// unadvertised/unreadable delivery mode; no claim is no_holder. Either failure clears the attempt, so
// a re-send is forwarded again; a delivered forward records the dedupe entry.
func finishRoleForward(cfg listenerDeliveryHandlerConfig, forward roleForward, err error) {
	item, sessionID := forward.item, forward.sessionID
	switch {
	case err == nil:
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "delivered",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener role forwarded", sessionID, item.Topic, item.EventID, "delivered")
			},
			recordDedupe: true,
		})
	case errors.Is(err, bus.ErrReceiptTimeout):
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "receipt_timeout",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role receipt timed out", sessionID, item.Topic, item.EventID, "receipt_timeout", slog.String("timeout", roleReceiptTimeout.String()))
			},
			exceptionReason: "receipt_timeout",
			clearAttempt:    true,
		})
	default:
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelError, "listener role forward failed", sessionID, item.Topic, item.EventID, "failed", slog.String("error", err.Error()))
			},
			exceptionReason: "delivery_failed",
			clearAttempt:    true,
		})
	}
}

// agentSubjectDelivery delivers directly to the session named by the topic
// (notifications.agent.<session>), pushing over HTTP/WebSocket when the
// session is live and falling back to whatever session.HandleAgentMessage
// decides (e.g. queuing) otherwise. A session another listener owns is that
// listener's to deliver, so this one acknowledges the message silently. On the
// durable path a push to a serving port is handed to the session's worker
// (servePortWorkers), which acknowledges the message once the port accepts it.
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
	handle := session.HandleAgentMessage
	if message.mode == durableDelivery && cfg.servePorts != nil {
		handle = session.ResolveAgentMessage
	}
	result := handle(item, sessionID, cfg.machineID, interestPtr, cfg.deliverer)
	if result.Push {
		if handOverPush(cfg, servePortPush{item: item, sessionID: sessionID, settle: message.settle, logLine: "listener agent delivered", timer: deliveryTimer}) {
			message.finalize(false)
			return
		}
		cfg.messagesNAKed.Inc()
		message.finalize(true)
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
	if result.Err != nil {
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "failed",
			duration:     &deliveryTimer,
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelError, "listener agent delivery failed", sessionID, item.Topic, item.EventID, "failed", slog.String("error", result.Err.Error()))
			},
			exceptionReason: "delivery_failed",
			origin:          exceptionOrigin(message),
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
// failures into a single NAK/ACK decision for the underlying message. On the
// durable path a push to a serving port is handed to the session's worker
// (servePortWorkers), and the message is acknowledged once every push ends.
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
	resolve := cfg.deliverer.DeliverWithResultForEntry
	if message.mode == durableDelivery && cfg.servePorts != nil {
		resolve = cfg.deliverer.Resolve
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
		delivery, err := resolve(item, interest, target)
		if err == nil && delivery.Push {
			if !handOverPush(cfg, servePortPush{item: item, sessionID: interest.SessionID, settle: message.settle, timer: deliveryTimer}) {
				failed = true
			}
			continue
		}
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
