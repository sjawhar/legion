package main

import (
	"context"
	"encoding/json"
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
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// roleForwardDedupePrefix marks an envelope already sent from the role arbiter, or the publish
// route, to its selected agent subject. A role lane that takes one again (an operator re-publishing
// a received frame) acknowledges it without re-entering the arbiter, and a session's dedupe reads
// it as a forward. The durable acknowledges every stored forward copy by its subject instead
// (listenerDeliveryHandler).
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
	// core handler, the live lane's and the durable's, which the receive loop keeps across every
	// resubscribe - draws on the one budget. listenerDeliveryHandler refuses a config without one
	// rather than drawing a private set, since a set per handler would be a second budget beside
	// the first. A test that builds a config sets its own.
	deferredUnplaceable *unplaceableDeferrals
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
// A durable retry waits durableRetryDelay (30 s) before redelivery; a path whose own bound sets the
// pace of its retries asks for its delay through retryAfter.
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

// settle ends the handling of a message nothing more is delivered for: a retry after retryIn when
// a recipient asked for one (counted as a NAK), else an ack.
func (message deliveryMessage) settle(cfg listenerDeliveryHandlerConfig, retryIn time.Duration) {
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

// clearLedgerAttempt gives attempt up so a retry prompts its pair again. A clear the ledger
// cannot write leaves the attempted record behind, which this process's next attempt for the pair
// takes over, and logs that.
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
	attempt, err := cfg.ledger.Attempt(item.DedupeKey, sessionID)
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
// a port, and it is this listener's to deliver, which session.OwnedBy decides for this delivery
// and for session.Deliverer.DeliverWithResultForEntry alike. Only such a pair costs a ledger
// write: a self-subscribed session takes the message from its own subscription and is only
// acked, and another listener's session is that listener's to deliver.
func promptsHere(machineID string, interest *store.Interest, entry *session.SessionEntry) bool {
	return entry != nil && entry.Port > 0 && session.OwnedBy(machineID, interest, *entry)
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
// heartbeated within session.ClaimStaleAfter as of now, read at each test, so the cached entry and
// the entry read again from the bucket are each judged at the moment they are checked. The plugin
// heartbeats every 90 seconds (DEFAULT_HEARTBEAT_MS, packages/envoy-client/src/envoy-settings.ts),
// so three missed heartbeats means the claiming process is very likely gone.
func laneHolderFresh(entry session.SessionEntry) bool {
	now := time.Now().UnixMilli()
	return entry.UpdatedAt > 0 && now-entry.UpdatedAt < int64(session.ClaimStaleAfter/time.Millisecond)
}

// resolveCoreRoleHolder resolves the current live holder for a role-lane delivery by role
// delivery's rule (resolveRoleHolder with laneHolderFresh), applying the terminal
// no_holder/delivery_failed outcome itself and reporting ok=false when there is nobody to deliver
// to. An unclaimed role is no_holder; a lapsed holder and every failed read are delivery_failed,
// each under a log line that names which it was. Role delivery answers no caller, so it resolves
// under context.Background(): the holder's session read keeps roleHolderReadTimeout
// (roleHolderSession's bound) as its own cap.
func resolveCoreRoleHolder(cfg listenerDeliveryHandlerConfig, item contracts.Envelope, role string) (string, session.SessionEntry, bool) {
	resolved, err := resolveRoleHolder(context.Background(), cfg.registry, cfg.sessions, role, laneHolderFresh)
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
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				metricStatus: "skipped",
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelWarn, "listener role has no holder", "", item.Topic, item.EventID, "skipped")
				},
				exceptionReason: "no_holder",
			})
		default:
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
		outcome := deliveryOutcome{
			sessionID:       sessionID,
			attempt:         attempt,
			settle:          settleClear,
			metricStatus:    "failed",
			exceptionReason: "delivery_failed",
		}
		if verdict.kind == forwardModeUnreadable {
			outcome.log = func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role delivery mode cannot be read unambiguously", sessionID, item.Topic, item.EventID, "failed")
			}
		} else {
			outcome.log = func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelWarn, "listener role holder does not advertise delivery mode", sessionID, item.Topic, item.EventID, "failed", slog.String("mode", verdict.mode))
			}
		}
		applyDeliveryOutcome(cfg, item, outcome)
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
				settle:          settleClear,
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
			settle:          settleClear,
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
		settle: settleRecord,
	})
	message.finalize(false)
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
// listener's to deliver, so this one acknowledges the message silently. Only a
// session this listener prompts (promptsHere) is asked of the delivery ledger
// first, so a self-subscribed session or another listener's costs no ledger
// write and is acked as before.
func agentSubjectDelivery(cfg listenerDeliveryHandlerConfig, message deliveryMessage, item contracts.Envelope) {
	sessionID := strings.TrimPrefix(item.Topic, contracts.AgentTopicPrefix)
	// The JetStream metadata carries everything unplaceableAgentDelivery bounds this message
	// by - its delivery count, against the deferrals the window buys and against
	// unplaceableMaxDeliveries - and its stream sequence, the key of the slot a deferral of it
	// holds, so it is read once here for both. A core delivery has none, and a durable message
	// whose metadata will not parse can be bounded by nothing, which unplaceableAgentDelivery
	// gives up on rather than retrying without end.
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
	var attempt dedupe.Attempt
	if promptsHere(cfg.machineID, interestPtr, entryPtr) {
		var deliver bool
		var retryIn time.Duration
		if attempt, deliver, retryIn = ledgerAttempt(cfg, message, item, sessionID, false); !deliver {
			message.settle(cfg, retryIn)
			return
		}
	}
	deliveryTimer := metrics.NewTimer()
	result := session.HandleAgentMessage(item, sessionID, cfg.machineID, interestPtr, entryPtr, message.deliverer(cfg))
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
		// A refused address (session.ErrAddressNotReachable) sent nothing, so its ledger attempt is
		// given up: the pair is not delivered, and a resend after the session registers through this
		// pod is prompted rather than skipped as in flight. It is not retried (ShouldNAK is false).
		refused := errors.Is(result.Err, session.ErrAddressNotReachable)
		applyDeliveryOutcome(cfg, item, deliveryOutcome{
			sessionID:    sessionID,
			attempt:      attempt,
			metricStatus: "failed",
			duration:     &deliveryTimer,
			log: func(logger *logging.Logger) {
				logger.DeliveryLog(slog.LevelError, "listener agent delivery failed", sessionID, item.Topic, item.EventID, "failed", slog.String("error", result.Err.Error()))
			},
			exceptionReason: "delivery_failed",
			settle:          settleClearIf(result.ShouldNAK || refused),
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
			settle: settleRecord,
		})
	} else if result.Err == nil {
		unplaceableAgentDelivery(cfg, message, item, sessionID, metadata, result, attempt)
		return
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
		// No interests is no failure: listenerDeliveryHandler routes every role and agent topic
		// elsewhere, so a fanout topic is never a control topic and owes no delivery exception,
		// and the message is ACKed with nothing to retry.
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
	deliverer := message.deliverer(cfg)
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
				settle:                        settleKeep,
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
				settle:                        settleKeep,
			})
			continue
		}
		deliveryTimer := metrics.NewTimer()
		delivery, err := deliverer.DeliverWithResultForEntry(item, interest, target)
		if err != nil && message.cancelled() {
			// The receiver cut the prompt short on a shutdown: a retry for whichever pod holds the
			// lease next, whatever the session's state, with no failure to report.
			clearLedgerAttempt(cfg, item, attempt)
			cfg.logger.DeliveryLog(slog.LevelWarn, "listener delivery cancelled at shutdown", interest.SessionID, item.Topic, item.EventID, "retry")
			retryIn = max(retryIn, durableRetryDelay)
			continue
		}
		if errors.Is(err, session.ErrAddressNotReachable) {
			// The session's address is local to this pod and another pod accepted its registration:
			// nothing was sent, and no attempt of this pod would reach it. The refusal is reported as
			// a terminal fanout outcome, like the delivery-mode refusals above, its ledger attempt is
			// given up (the pair is not delivered), and the message is not retried for it.
			applyDeliveryOutcome(cfg, item, deliveryOutcome{
				sessionID:    interest.SessionID,
				attempt:      attempt,
				metricStatus: "failed",
				duration:     &deliveryTimer,
				log: func(logger *logging.Logger) {
					logger.DeliveryLog(slog.LevelError, "listener delivery failed", interest.SessionID, item.Topic, item.EventID, "failed", slog.String("error", err.Error()))
				},
				exceptionReason: "delivery_failed",
				fanoutRefusal:   true,
				settle:          settleClear,
			})
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
				settle: settleClearIf(retryable),
				retry:  retryable,
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
				settle:       settleRecord,
			})
		}
	}
	if deadDeliveries > 0 {
		cfg.logger.Info("listener skipped dead session deliveries", slog.String("topic", item.Topic), slog.Int("count", deadDeliveries))
	}
	message.settle(cfg, retryIn)
}
