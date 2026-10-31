package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dedupe"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/metrics"
)

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

// ledgerSettle is what a delivery outcome does with the pair's delivery-ledger attempt. settleNone
// is its zero value, so an outcome that settles nothing names nothing.
type ledgerSettle uint8

const (
	// settleNone settles nothing. It is the outcome with no attempt of this process to settle -
	// a skip taken before the ledger was asked, a recipient this listener does not prompt
	// (promptsHere), a role whose holder never resolved - and the failure that deliberately
	// leaves this process's attempt in flight, held for the pair until it reaches the ledger's
	// takeover age (an agent delivery that failed and is neither retried nor a refused address).
	settleNone ledgerSettle = iota
	// settleClear gives the attempt up (dedupe.Ledger.Clear), so a retry prompts the pair again.
	settleClear
	// settleRecord records the pair delivered in the ledger (dedupe.Ledger.Record).
	settleRecord
	// settleKeep keeps the attempt (dedupe.Ledger.Keep): a prompt whose outcome is unknown, or a
	// pair this delivery disposed of for good, so a further delivery of the message within
	// dedupe.AttemptedAge is an attempt-dedupe skip.
	settleKeep
)

// settleClearIf is settleClear when clear and settleNone otherwise, for the two outcomes whose
// settlement the failure they carry decides.
func settleClearIf(clear bool) ledgerSettle {
	if clear {
		return settleClear
	}
	return settleNone
}

// deliveryOutcome bundles one delivery attempt's bookkeeping: the metrics
// label to record, the log line to emit, whether to publish a delivery
// exception (and how an exception-publish failure should be treated), and
// how to settle the pair's delivery-ledger attempt. Each branch of the three
// delivery paths in delivery.go (roleTopicDelivery, agentSubjectDelivery and
// fanoutDelivery) produces one of these; applyDeliveryOutcome performs
// the mechanical side effects so they happen in exactly one place.
type deliveryOutcome struct {
	// sessionID is the recipient the outcome is about, named in its exception; "" when there is
	// none (e.g. a role that was never assigned a holder).
	sessionID string
	// attempt is the pair's delivery-ledger attempt that settle and exceptionFailureClearsAttempt
	// settle; the zero Attempt (a pair delivered without the ledger, or one the listener does not
	// prompt) settles nothing.
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
	// exceptionFailureClearsAttempt makes an exception-publish failure settle the ledger attempt
	// settleClear, whatever settle below names.
	exceptionFailureClearsAttempt bool

	// settle is how this outcome settles the pair's ledger attempt; settling nothing
	// (settleNone) is the zero value, which an outcome with no attempt of its own leaves unset.
	settle ledgerSettle
	// retry is whether this outcome asks for the attempt to be retried
	// (NAKed).
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
	settle := outcome.settle
	if outcome.exceptionReason != "" && !isExceptionsTopic(item.Topic) && (isControlTopic(item.Topic) || outcome.fanoutRefusal) {
		if err := publishDeliveryException(cfg.client, item, outcome.exceptionReason, outcome.sessionID, outcome.fanoutRefusal); err != nil {
			cfg.logger.Error(exceptionPublishFailedLine, slog.String("error", err.Error()), slog.String("topic", item.Topic))
			if outcome.exceptionFailureClearsAttempt {
				settle = settleClear
			}
		}
	}
	switch settle {
	case settleClear:
		clearLedgerAttempt(cfg, item, outcome.attempt)
	case settleRecord:
		if err := cfg.ledger.Record(outcome.attempt); err != nil {
			cfg.logger.Warn(ledgerUnavailableLine, slog.String("operation", "record"), slog.String("session_id", outcome.sessionID), slog.String("event_id", item.EventID), slog.String("error", err.Error()))
		}
	case settleKeep:
		cfg.ledger.Keep(outcome.attempt)
	}
	return outcome.retry
}
