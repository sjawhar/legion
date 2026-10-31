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

// roleForwardDedupePrefix marks an envelope already sent from the role arbiter, or the publish
// route, to its selected agent subject. A role lane that takes one again (an operator re-publishing
// a received frame) does not re-enter the arbiter with it, and a session's dedupe reads it as a
// forward. The durable acknowledges the stored forward copy by its subject (listenerDeliveryHandler)
// for every holder that has it another way; a serving-port holder of this listener has no
// subscriber, so its copy is its delivery (roleForwardCopyDelivery).
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
	// forwardRole publishes a role-lane envelope to the holder's agent subject and returns the wait
	// for its outcome; main.go wires startRoleForward(client). The wait's bus.ErrReceiptTimeout
	// means the forward was flushed to the server and no receipt arrived inside timeout;
	// bus.ErrForwardNotStored that the stream did not acknowledge storing it; any other error — a
	// raw nats.ErrTimeout from the flush included, returned by the publish or the wait — means the
	// forward is not known to have reached the holder. storedOnly asks for a forward that counts as
	// delivered on the stream's acknowledgement alone (roleForwardStoredOnly).
	forwardRole func(subject string, item contracts.Envelope, timeout time.Duration, storedOnly bool) (wait func() error, err error)
	// roleForwards runs the role lane's forwards off its handler (roleForwarder), and servePorts
	// pushes the durable path's messages to serving-port sessions off its handler, one worker per
	// session (servePortWorkers); every delivery path hands its forwards and pushes to them, so
	// both are always set: wireDeliveryWorkers starts them from the configuration they report
	// through, once it is otherwise built, and main.go and every test alike wire them there.
	roleForwards      *roleForwarder
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
	// core handler, the durable's, and the one the self-health rebind builds after a NATS flap -
	// draws on the one budget. listenerDeliveryHandler refuses a config without one rather than
	// drawing a private set, since a set per handler is a second budget beside the first on the
	// very rebind this retry exists for. A test that builds a config sets its own.
	deferredUnplaceable *unplaceableDeferrals
}

// wireDeliveryWorkers starts the workers every delivery path hands its work to and returns cfg
// carrying them, with the stop that ends them. Each worker keeps the configuration it is built
// from by value, so a caller wires them last, once every seam of cfg is set: a seam set after this
// is one the workers do not have. The role forwarder is built from a configuration that already
// carries the serving-port workers, since the stored copy of a forward it publishes is pushed by
// them. Stopping ends the serving-port workers; the forwarder has no loop of its own, and the role
// lane's drain waits for the forwards it holds (roleLane.drain).
func wireDeliveryWorkers(cfg listenerDeliveryHandlerConfig) (listenerDeliveryHandlerConfig, func(context.Context)) {
	cfg.servePorts = startServePortWorkers(cfg)
	cfg.roleForwards = newRoleForwarder(cfg)
	return cfg, cfg.servePorts.stop
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
	// metadata is a durable message's JetStream metadata, read once by its handler: its stream
	// sequence, which says which session consumers hold it (storedSeq) and keys an unplaceable
	// deferral, and its delivery count, which bounds that retry. It is nil for a core or live
	// message, and for a durable one whose metadata will not parse, which can be bounded by
	// nothing (unplaceableAgentDelivery) and is in no consumer's window as far as this listener
	// can tell (storedSeq 0).
	metadata *nats.MsgMetadata
	// settle acknowledges a durable message once its handler and the pushes it handed to
	// serving-port workers have ended.
	settle *durableSettle
}

// storedSeq is the stream sequence of a durable message, which decides whether a session's
// consumer holds it (session.SessionEntry.ConsumerHoldsStored): 0 for a core or live message, and
// for a durable one whose metadata will not parse, so such a message is delivered rather than left
// to a consumer that may not hold it.
func (message deliveryMessage) storedSeq() uint64 {
	if message.metadata == nil {
		return 0
	}
	return message.metadata.Sequence.Stream
}

// finalize makes core delivery's loss-on-failure semantics explicit: only a
// durable JetStream message receives an acknowledgement or retry request. A role-lane or live-lane
// message is core NATS, sent once to whoever was subscribed, so nothing acknowledges or retries it.
// A durable message is settled once the pushes its handler handed over have ended too. A retry
// takes the standard delay; a path that needs its own asks for it through finalizeAfter.
func (message deliveryMessage) finalize(retry bool) {
	if message.mode != durableDelivery {
		return
	}
	message.settle.end(retry)
}

// finalizeAfter asks the stream to redeliver this message after delay, for a path whose own
// bound sets the pace of its retries rather than finalize's standard one. Like finalize, it
// does nothing to a core message: nothing redelivers one, so there is no request to make; and
// it settles a durable message only once the pushes its handler handed over have ended too.
func (message deliveryMessage) finalizeAfter(delay time.Duration) {
	if message.mode != durableDelivery {
		return
	}
	message.settle.retryAfter(delay)
}

// jetStreamDeliveryHandler is the durable's handler. It reads the message's JetStream metadata
// once, for the paths that bound themselves by its delivery count and for the stream sequence that
// says which session consumers hold it. A message no session needs from the durable
// (durableNeedsDelivery) is acknowledged before it is decoded, each session that takes it another
// way counted as a skipped delivery, as the delivery paths count it.
func jetStreamDeliveryHandler(cfg listenerDeliveryHandlerConfig) nats.MsgHandler {
	handler := listenerDeliveryHandler(cfg)
	return func(msg *nats.Msg) {
		message := deliveryMessage{data: msg.Data, mode: durableDelivery, msg: msg, settle: newDurableSettle(msg)}
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
		// The stream keeps one copy however many listeners raise it, each from its own durable.
		if err := client.PublishStoredAs(context.Background(), exception, "exception."+reason+"."+origin); err != nil {
			return fmt.Errorf("publish delivery exception: %w", err)
		}
		return nil
	}
	if err := client.Publish(exception); err != nil {
		return fmt.Errorf("publish delivery exception: %w", err)
	}
	return nil
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
	// exceptionFailureClearsAttempt: an exception-publish failure clears the
	// attempt-cache entry, independent of (and possibly in addition to)
	// clearAttempt below.
	exceptionFailureClearsAttempt bool

	// recordAttempt records the attempt-cache entry, before the exception below so that a failure
	// to publish it can clear the record again (exceptionFailureClearsAttempt).
	recordAttempt bool
	// clearAttempt unconditionally rolls back the attempt-cache record.
	clearAttempt bool
	// recordDedupe marks the dedupe-cache entry as sent.
	recordDedupe bool
	// retry is whether this outcome asks for the attempt to be retried
	// (NAKed).
	retry bool
}

// applyDeliveryOutcome performs the bookkeeping every delivery attempt
// shares — metrics, duration, logging, exception publication, and
// attempt/dedupe cache updates — and reports whether the attempt should be
// retried (NAKed).
func applyDeliveryOutcome(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, outcome deliveryOutcome) bool {
	if outcome.recordAttempt {
		cfg.attemptCache.Record(item.DedupeKey, outcome.sessionID)
	}
	if outcome.metricStatus != "" {
		cfg.messagesDelivered.Inc([2]string{"delivery_status", outcome.metricStatus})
		if outcome.duration != nil {
			outcome.duration.ObserveDuration(cfg.deliveryDuration, [2]string{"delivery_status", outcome.metricStatus})
		}
	}
	if outcome.log != nil {
		outcome.log(cfg.logger)
	}
	if outcome.exceptionReason != "" && !isExceptionsTopic(item.Topic) && (isControlTopic(item.Topic) || outcome.fanoutRefusal) {
		if err := publishDeliveryException(cfg.client, item, outcome.exceptionReason, outcome.sessionID, outcome.fanoutRefusal, outcome.origin); err != nil {
			cfg.logger.Error(exceptionPublishFailedLine, slog.String("error", err.Error()), slog.String("topic", item.Topic))
			if outcome.exceptionFailureClearsAttempt {
				cfg.attemptCache.Clear(item.DedupeKey, outcome.sessionID)
			}
		}
	}
	if outcome.clearAttempt {
		cfg.attemptCache.Clear(item.DedupeKey, outcome.sessionID)
	}
	if outcome.recordDedupe {
		cfg.dedupeCache.Record(item.DedupeKey, outcome.sessionID)
	}
	return outcome.retry
}

// listenerDeliveryHandler decodes and validates the envelope (decodeStoredEnvelope, the rules the
// relay of a session's consumer reads a stored message under too), then routes it to exactly one
// of the three delivery paths based on its topic: a role lane (arbitrated to whichever session
// currently holds the role), a direct agent subject, or the general interest fanout.
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
		if err := decodeStoredEnvelope(message.data, &item); err != nil {
			cfg.logger.Error("listener decode failed", slog.String("error", err.Error()))
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
		// The stream captures every agent subject, so it also stores each forward a sender made to
		// a session's agent subject while keeping the envelope's own topic: a role message the
		// lane or the publish route forwarded to its holder, an ad-hoc topic message the route
		// forwarded to each self-subscribed recipient. The forward reached its session over core
		// NATS, or its sender learned it did not; the stored copy is not a message for this
		// listener to deliver again, to the role's holder or to the topic's serve-port sessions.
		// A serving-port holder of this listener is the one exception: it answers no receipt, so
		// its forward was sent for the stream to store (roleForwardStoredOnly) and this copy is
		// its delivery, which the path below pushes to its port.
		if message.mode == durableDelivery && strings.HasPrefix(message.msg.Subject, contracts.AgentTopicPrefix) && item.Topic != message.msg.Subject &&
			!storedForwardCopyIsDelivery(cfg, strings.TrimPrefix(message.msg.Subject, contracts.AgentTopicPrefix), message.storedSeq()) {
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
// heartbeated within session.ClaimStaleAfter as of now, read at each test, so the cached entry and
// the entry read again from the bucket are each judged at the moment they are checked. The plugin
// heartbeats every 90 seconds (DEFAULT_HEARTBEAT_MS, packages/envoy-client/src/envoy-settings.ts),
// so three missed heartbeats means the claiming process is very likely gone.
func laneHolderFresh(entry session.SessionEntry) bool {
	now := time.Now().UnixMilli()
	return entry.UpdatedAt > 0 && now-entry.UpdatedAt < int64(session.ClaimStaleAfter/time.Millisecond)
}

// resolveCoreRoleHolder resolves the current live holder for a role-lane delivery by role
// delivery's rule (resolveRoleHolder with laneHolderFresh), applying the terminal outcome itself
// and reporting ok=false when there is nobody to deliver to. Its two give-up branches - the
// unclaimed role and the lapsed holder - keep the message for the role's next holder first
// (keepRoleLaneMessage), and a copy that was kept publishes no delivery exception: the message is
// not lost. A keep that failed leaves each branch exactly as it was: an unclaimed role is
// no_holder, a lapsed holder delivery_failed. Every failed read is delivery_failed and keeps
// nothing, since a read that did not answer is no evidence the role has no holder. Each branch
// logs a line that names which it was. Role delivery answers no caller, so it resolves
// under context.Background(): the holder's session read keeps roleHolderReadTimeout
// (roleHolderSession's bound) as its own cap. Its first claim read is the registry's watched copy
// of the role bucket (store.Registry.CachedRoleHolder, passed as the resolution's firstHolder), so
// a role message costs no bucket round trip; a retry after a superseded release reads the bucket
// itself, since the copy can trail the claim that superseded it.
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
		switch resolved.state {
		case roleHolderLive:
			return sessionID, resolved.entry, true
		case roleHolderUnclaimed:
			if keepRoleLaneMessage(cfg, role, "", item, roleHolderUnclaimed) {
				break
			}
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				metricStatus: "skipped",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelWarn, "listener role has no holder", "", item.Topic, item.EventID, "skipped")
				},
				exceptionReason: "no_holder",
			})
		default:
			if keepRoleLaneMessage(cfg, role, sessionID, item, roleHolderLapsed) {
				break
			}
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    sessionID,
				metricStatus: "failed",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelWarn, "listener role holder is not live", sessionID, item.Topic, item.EventID, "failed")
				},
				exceptionReason: "delivery_failed",
			})
		}
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
// the sender learns immediately whether the holder received it. Before
// forwarding it judges the message by beforeForward, the rules the publish
// route applies too, with the dedupe and attempt caches as its record of
// what the holder already took: an echo of the holder's own message and a
// duplicate of an envelope already forwarded (or already being forwarded)
// are disposed of with no forward and no exception, and a holder that does
// not advertise the envelope's own targeted delivery mode, or whose mode
// cannot be read unambiguously, is refused like a stale holder. Role
// forwarding needs that guard of its own because it never goes through
// /v1/messages/send. Every branch ACKs: role lanes have no durable transit
// to retry against, so a failed forward is reported via a delivery exception
// instead of a NAK. The forward itself is handed to the role forwarder
// (roleForwarder.enqueue) and its outcome reported when it ends
// (finishRoleForward), so the role lane's handler returns without waiting for
// the holder.
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
	verdict := beforeForward(item, sessionID, holder, func(key, recipient string) bool {
		return cfg.dedupeCache.Seen(key, recipient) || cfg.attemptCache.Seen(key, recipient)
	})
	if verdict.kind != forwardAllowed {
		outcome := deliveryOutcome{sessionID: sessionID}
		switch verdict.kind {
		case forwardEcho:
			outcome.metricStatus = "skipped"
			outcome.log = func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener skip role echo", sessionID, item.Topic, item.EventID, "skipped")
			}
		case forwardRepeat:
			outcome.metricStatus = "dedupe"
			outcome.log = func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener role dedupe skip", sessionID, item.Topic, item.EventID, "dedupe", slog.String("dedupe_key", item.DedupeKey))
			}
		case forwardModeUnreadable:
			outcome.metricStatus = "failed"
			outcome.log = func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role delivery mode cannot be read unambiguously", sessionID, item.Topic, item.EventID, "failed")
			}
			outcome.exceptionReason = "delivery_failed"
		case forwardModeUnadvertised:
			outcome.metricStatus = "failed"
			outcome.log = func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role holder does not advertise delivery mode", sessionID, item.Topic, item.EventID, "failed", slog.String("mode", verdict.mode))
			}
			outcome.exceptionReason = "delivery_failed"
		}
		applyDeliveryOutcome(cfg, item, outcome)
		message.finalize(false)
		return
	}
	cfg.attemptCache.Record(item.DedupeKey, sessionID)
	cfg.roleForwards.enqueue(roleForward{item: item, sessionID: sessionID, storedOnly: roleForwardStoredOnly(holder)})
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

// handOverOrRetry hands push to its session's serving-port worker (handOverPush), or, when the
// workers cannot take it, gives message back to the durable for a retry.
func handOverOrRetry(cfg listenerDeliveryHandlerConfig, message deliveryMessage, push servePortPush) {
	if handOverPush(cfg, push) {
		message.finalize(false)
		return
	}
	cfg.messagesNAKed.Inc()
	message.finalize(true)
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

// deliverySessionReadTimeout is delivery's own bound on its read of the target session from the
// bucket. It is the role paths' bound (roleHolderReadTimeout, role_holder.go) today without being
// tied to it, and it can be looser than the send route's (sendSessionReadTimeout) because no HTTP
// caller is waiting on this read: the cost of waiting is a message held, not a request kept open,
// and the alternative - taking the cache's miss for an answer - ACKs the message away.
const deliverySessionReadTimeout = 2 * time.Second

// agentSubjectDelivery delivers directly to the session named by the topic
// (notifications.agent.<session>), pushing over HTTP/WebSocket when the
// session is live and falling back to whatever session.HandleAgentMessage
// decides (e.g. queuing) otherwise. A session another listener owns is that
// listener's to deliver, so this one acknowledges the message silently. On the
// durable path a push to a serving port is handed to the session's worker
// (servePortWorkers), which acknowledges the message once the port accepts it.
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
	interest, err := cfg.registry.Get(context.Background(), sessionID)
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
	// The durable path hands a serving-port session's push to its worker (servePortWorkers), and
	// its message's stream sequence says whether that session's consumer already holds it; the
	// core and live paths push the session themselves.
	var result session.HandleAgentResult
	if message.mode == durableDelivery {
		result = session.ResolveStoredAgentMessage(item, sessionID, cfg.machineID, interestPtr, entryPtr, cfg.deliverer, message.storedSeq())
	} else {
		result = session.HandleAgentMessage(item, sessionID, cfg.machineID, interestPtr, entryPtr, cfg.deliverer)
	}
	if result.Push {
		handOverOrRetry(cfg, message, servePortPush{item: item, sessionID: sessionID, settle: message.settle, storedSeq: message.storedSeq(), logLine: "listener agent delivered", timer: deliveryTimer})
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
		unplaceableAgentDelivery(cfg, message, item, sessionID, metadata, result)
		return
	}
	if result.ShouldNAK {
		cfg.messagesNAKed.Inc()
		message.finalize(true)
	} else {
		message.finalize(false)
	}
}

// followedTopicRules is what a message on a topic a session follows is judged against
// (judgeFollowedTopic): the session entries, the dedupe cache of what each session already took,
// and the attempt cache of what is already on its way to it.
type followedTopicRules struct {
	sessions     *session.SessionRegistry
	dedupeCache  *dedupe.Cache
	attemptCache *dedupe.Cache
}

func (cfg listenerDeliveryHandlerConfig) followedTopicRules() followedTopicRules {
	return followedTopicRules{sessions: cfg.sessions, dedupeCache: cfg.dedupeCache, attemptCache: cfg.attemptCache}
}

// followedTopicVerdict is what a delivery path does with a message on a topic a session follows.
type followedTopicVerdict uint8

const (
	// takeFollowedTopic: deliver the message to the session.
	takeFollowedTopic followedTopicVerdict = iota
	// followedTopicDeduped: a copy under this dedupe key already reached the session.
	followedTopicDeduped
	// followedTopicAttempted: a copy under this dedupe key is already on its way to it.
	followedTopicAttempted
	// followedTopicEcho: the session published the message itself.
	followedTopicEcho
	// followedTopicModeUnreadable: the envelope's targeted delivery mode cannot be read
	// unambiguously, so no session can be shown to advertise it.
	followedTopicModeUnreadable
	// followedTopicModeNotAdvertised: the session's live entry does not advertise the mode.
	followedTopicModeNotAdvertised
)

// judgeFollowedTopic is the one judgement of a message on a topic sessionID follows: the durable
// path's fan-out (fanoutDelivery) and the relay of a serving-port session's consumer
// (sessionRelays.pushUntilAccepted), which push the same messages by different routes, both apply
// it, so each refusal holds whichever route a session's message takes. A copy the session already
// took, or one already on its way to it, is refused by the dedupe and attempt caches, which are
// keyed on the envelope's dedupe key: one upstream event published on several of a session's
// topics (contracts/normalize.go) reaches it once. An echo of the session's own message, and a
// delivery mode its entry does not advertise, are refused too.
//
// mode is the delivery mode the envelope targets, "" when it targets none, and modeErr the failure
// of reading it (envelopeDeliveryMode): both are read once per message rather than once per
// recipient.
//
// It returns the session's live entry when one was read, so a caller that delivers through the
// entry does not read it again, and the error behind a mode refusal for the caller to report.
func judgeFollowedTopic(rules followedTopicRules, sessionID string, item contracts.Envelope, mode string, modeErr error) (followedTopicVerdict, *session.SessionEntry, error) {
	if rules.dedupeCache.Seen(item.DedupeKey, sessionID) {
		return followedTopicDeduped, nil, nil
	}
	if rules.attemptCache.Seen(item.DedupeKey, sessionID) {
		return followedTopicAttempted, nil, nil
	}
	if item.SourceSession != "" && item.SourceSession == sessionID {
		return followedTopicEcho, nil, nil
	}
	if modeErr != nil {
		return followedTopicModeUnreadable, nil, modeErr
	}
	entry, err := rules.sessions.Get(sessionID)
	if err != nil {
		// No entry is no refusal: the delivery path decides what to do with a session it cannot
		// read, and a mode nobody advertises cannot be judged against an entry there is none of.
		return takeFollowedTopic, nil, nil
	}
	if mode != "" && !hasCapability(entry.Capabilities, mode) {
		return followedTopicModeNotAdvertised, &entry, errors.New("the session does not advertise delivery mode " + mode)
	}
	return takeFollowedTopic, &entry, nil
}

// fanoutRefusalOutcome is how the fan-out reports a refusal judgeFollowedTopic returned, each
// under the line and the metric status it has always had. A mode refusal records the attempt, so
// an identical redelivery inside the attempt window is the dedupe skip above rather than a second
// exception; a refusal the caches themselves made records nothing.
func fanoutRefusalOutcome(sessionID string, item contracts.Envelope, verdict followedTopicVerdict, mode string) deliveryOutcome {
	switch verdict {
	case followedTopicDeduped:
		return deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "dedupe",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener dedupe skip", sessionID, item.Topic, item.EventID, "dedupe", slog.String("dedupe_key", item.DedupeKey))
			},
		}
	case followedTopicAttempted:
		return deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "dedupe",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelInfo, "listener attempt-dedupe skip", sessionID, item.Topic, item.EventID, "skipped", slog.String("dedupe_key", item.DedupeKey))
			},
		}
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
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener delivery mode cannot be read unambiguously", sessionID, item.Topic, item.EventID, "failed")
			},
			exceptionReason:               "delivery_failed",
			fanoutRefusal:                 true,
			recordAttempt:                 true,
			exceptionFailureClearsAttempt: true,
		}
	default:
		return deliveryOutcome{
			sessionID:    sessionID,
			metricStatus: "failed",
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener subscriber does not advertise delivery mode", sessionID, item.Topic, item.EventID, "failed", slog.String("mode", mode))
			},
			exceptionReason:               "delivery_failed",
			fanoutRefusal:                 true,
			recordAttempt:                 true,
			exceptionFailureClearsAttempt: true,
		}
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
		// No interests is no failure: listenerDeliveryHandler routes every role and agent topic
		// elsewhere, so a fanout topic is never a control topic and owes no delivery exception,
		// and the message is ACKed with nothing to retry.
		cfg.logger.Info("listener no matching interests", slog.String("topic", item.Topic))
		message.finalize(false)
		return
	}
	mode, modeErr := envelopeDeliveryMode(item)

	var failed bool
	var deadDeliveries int
	for _, interest := range items {
		verdict, target, _ := judgeFollowedTopic(cfg.followedTopicRules(), interest.SessionID, item, mode, modeErr)
		if verdict != takeFollowedTopic {
			applyDeliveryOutcome(cfg, item, fanoutRefusalOutcome(interest.SessionID, item, verdict, mode))
			continue
		}
		deliveryTimer := metrics.NewTimer()
		// One resolution per recipient decides every outcome below, this path's push included: it
		// reads the entry the judgement above returned and makes no call of its own.
		delivery, err := cfg.deliverer.Resolve(item, interest, target, item.Topic, message.storedSeq())
		if err == nil && delivery.Relayed {
			// The session's own consumer holds this message and the relay of that consumer pushes
			// it (sessionRelays.pushUntilAccepted), which is also what records its dedupe key, on
			// the port's 204. Nothing is recorded here: an attempt or dedupe record this path
			// wrote for a session it does not push to is one the relay would find and ack a copy
			// the session never received by (session.SessionEntry.ConsumerRelaysStored).
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				metricStatus: "skipped",
				duration:     &deliveryTimer,
			})
			continue
		}
		// The attempt is recorded before the push is made or handed over, so another copy of the
		// same event read while it is in flight is a dedupe skip rather than a second push.
		cfg.attemptCache.Record(item.DedupeKey, interest.SessionID)
		if err == nil && delivery.Push {
			if message.mode == durableDelivery {
				// The durable path's push waits behind the session's earlier messages in its own
				// worker, and the message is acknowledged once that push ends.
				if !handOverPush(cfg, servePortPush{item: item, sessionID: interest.SessionID, settle: message.settle, storedSeq: message.storedSeq(), timer: deliveryTimer}) {
					failed = true
				}
				continue
			}
			// The live and core paths push here, in the handler, as
			// session.Deliverer.DeliverWithResultForEntry pushes for them elsewhere: their
			// message is sent once to whoever is there, with nothing to acknowledge or retry.
			err = cfg.deliverer.Push(context.Background(), item, interest.SessionID, delivery.Entry)
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
				clearAttempt: retryable,
				retry:        retryable,
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
