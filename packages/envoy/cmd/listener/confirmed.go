package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

// publishDeliveryWindow bounds a confirmed send through POST /v1/messages/publish, counted from the
// request's arrival: every read, forward and pause of the send happens inside it. It keeps the
// answer inside envoy-client's 5 s attempt, the Legion daemon's 10 s client and the listener's 10 s
// write timeout, and leaves room for two forwards of roleReceiptTimeout, which covers a recipient
// whose connection is moving to another server.
const publishDeliveryWindow = 4 * time.Second

// publishRetryPause is the wait between one failed attempt of a confirmed send and the next, so a
// recipient whose connection is down is not sent a burst of forwards that reach nobody.
const publishRetryPause = 100 * time.Millisecond

// roleSendFailure is the 504 of a role send whose window ended without its holder's receipt.
// Reason is receipt_timeout when the last forward reached the server and drew no receipt
// (bus.ErrReceiptTimeout), else delivery_failed: no live holder to forward to, a read that failed
// or ran out of time, or a forward not shown to have reached the server. Holder is the holder the
// send last resolved, empty when none was; Attempts counts the forwards it made, every one the same
// envelope under one event_id.
type roleSendFailure struct {
	Error     string `json:"error"`
	Reason    string `json:"reason"`
	EventID   string `json:"event_id"`
	DedupeKey string `json:"dedupe_key"`
	Holder    string `json:"holder"`
	Attempts  int    `json:"attempts"`
}

// capabilityRefusal is the 403 of a role send whose holder cannot take the payload's delivery
// mode: it does not advertise the mode, or the mode cannot be read unambiguously. Its error reads
// as /v1/messages/send's answer to the same refusal.
type capabilityRefusal struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
	Holder string `json:"holder"`
}

// undeliveredRecipient names one session of an ad-hoc topic send that did not confirm the message,
// with the reason a role send would give.
type undeliveredRecipient struct {
	SessionID string `json:"session_id"`
	Reason    string `json:"reason"`
}

// topicSendFailure is the 504 of an ad-hoc topic send that some recipient did not confirm inside
// the window. Delivered names every recipient that did; an empty list is [] rather than null.
type topicSendFailure struct {
	Error       string                 `json:"error"`
	Reason      string                 `json:"reason"`
	EventID     string                 `json:"event_id"`
	DedupeKey   string                 `json:"dedupe_key"`
	Delivered   []string               `json:"delivered"`
	Undelivered []undeliveredRecipient `json:"undelivered"`
}

// readBefore runs read on its own goroutine and waits for its answer until ctx ends. The registry's
// reads take no context (nats.go's KV read waits out the JetStream MaxWait, and roleHolderSession
// gives its bucket read roleHolderReadTimeout), so a read a send's deadline cuts short is left to
// finish on its own and its answer is dropped; what it changes meanwhile, such as a lapsed claim it
// releases, is what the same read changes for any other caller.
func readBefore[T any](ctx context.Context, read func() T) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	answered := make(chan T, 1)
	go func() { answered <- read() }()
	select {
	case got := <-answered:
		return got, nil
	case <-ctx.Done():
		return zero, ctx.Err()
	}
}

// receiptWindow is the receipt wait of one forward: roleReceiptTimeout, or what is left of the send
// when that is less.
func receiptWindow(ctx context.Context) time.Duration {
	deadline, _ := ctx.Deadline()
	return min(roleReceiptTimeout, time.Until(deadline))
}

// pauseBeforeRetry waits publishRetryPause, or until ctx ends, and reports whether the send has time
// left for another attempt.
func pauseBeforeRetry(ctx context.Context) bool {
	return waitUnlessDone(ctx, publishRetryPause) == nil && ctx.Err() == nil
}

// forwardRefused reports whether a forward failed in a way another attempt repeats: NATS refuses
// the envelope or subject however often it is sent (bus.ErrRefused), or the listener's grant does
// not allow the subject (bus.ErrPublishDenied). A role send answers it at once, as writeNATSError
// maps it; an ad-hoc topic send stops sending to that recipient (confirmTopicRecipient).
func forwardRefused(err error) bool {
	return errors.Is(err, bus.ErrRefused) || errors.Is(err, bus.ErrPublishDenied)
}

// unconfirmedReason is the reason a send gives for a recipient that never sent its receipt, from
// the last forward made to it: receipt_timeout when that forward reached the server and drew no
// receipt, delivery_failed for anything else, no forward at all among them.
func unconfirmedReason(lastForward error) string {
	if errors.Is(lastForward, bus.ErrReceiptTimeout) {
		return "receipt_timeout"
	}
	return "delivery_failed"
}

// deliveryStatus is the messagesDelivered label of a reason, as the role lane labels the same
// outcome.
func deliveryStatus(reason string) string {
	if reason == "receipt_timeout" {
		return reason
	}
	return "failed"
}

// roleSend is one role message through the publish route. The route does not publish on the role
// lane: it forwards the envelope to the holder's agent subject itself and answers once the holder's
// receipt arrived, or with the failure once publishDeliveryWindow from arrived has passed, so its
// sender learns whether the message was taken. It publishes no delivery exception: the sender has
// the answer.
type roleSend struct {
	w        http.ResponseWriter
	d        *listenerDeps
	item     contracts.Envelope
	role     string
	arrived  time.Time
	holder   string
	attempts int
}

// settle counts and logs the send's outcome, the one line a send writes, once its answer is written.
// metricStatus is the label the role lane gives the same outcome.
func (s *roleSend) settle(outcome, metricStatus string) {
	s.d.messagesDelivered.Inc([2]string{"delivery_status", metricStatus})
	level := slog.LevelInfo
	if metricStatus != "delivered" && metricStatus != "skipped" && metricStatus != "dedupe" {
		level = slog.LevelWarn
	}
	s.d.logger.DeliveryLog(level, "listener publish role send", s.holder, s.item.Topic, s.item.EventID, outcome,
		slog.String("dedupe_key", s.item.DedupeKey),
		slog.Int("attempts", s.attempts),
		slog.String("elapsed", time.Since(s.arrived).String()))
}

// delivered answers 200 with the envelope and the holder that took it; outcome is also the label
// the role lane gives it.
func (s *roleSend) delivered(outcome string) {
	writeJSON(s.w, http.StatusOK, publishResponse{Envelope: s.item, Holder: s.holder})
	s.settle(outcome, outcome)
}

// noHolder answers writeRoleHolderError's 404 (unclaimed or holder_lapsed) for a role with no live
// claim, which the Legion daemon reads as notify.ErrNoHolder. The role lane counts an unclaimed
// role skipped and a lapsed holder failed.
func (s *roleSend) noHolder(result roleHolderResult) {
	writeRoleHolderError(s.w, s.role, result)
	metricStatus := "failed"
	if result.state == roleHolderUnclaimed {
		metricStatus = "skipped"
	}
	s.settle(string(result.state), metricStatus)
}

// failed answers a failure that is not the window's end, as writeNATSError maps it.
func (s *roleSend) failed(err error) {
	writeNATSError(s.w, err, http.StatusInternalServerError, err.Error())
	s.settle("failed", "failed")
}

// timedOut answers the 504 of a send whose window ended without a receipt.
func (s *roleSend) timedOut(lastForward error) {
	reason := unconfirmedReason(lastForward)
	writeJSON(s.w, http.StatusGatewayTimeout, roleSendFailure{
		Error: fmt.Sprintf("role %s: event %s was not confirmed by its holder %s within %s (%s)",
			s.role, s.item.EventID, cmp.Or(s.holder, "(none)"), publishDeliveryWindow, reason),
		Reason:    reason,
		EventID:   s.item.EventID,
		DedupeKey: s.item.DedupeKey,
		Holder:    s.holder,
		Attempts:  s.attempts,
	})
	s.settle(reason, deliveryStatus(reason))
}

// settledBeforeForward applies, to a holder the send has not checked yet, whose session entry is
// entry, the rules the role lane applies before it forwards, in the lane's order, and reports
// whether it answered: the source session holds the role (the lane's echo skip) or the holder
// already took this dedupe key, both answered 200 with nothing forwarded; the holder cannot take
// the payload's delivery mode (deliveryModeRefusal, sendHandler's refusal, read as
// roleTopicDelivery reads it), answered 403. Guarding after the dedupe check keeps a delivered
// repeat from being judged by the holder's current capabilities.
func (s *roleSend) settledBeforeForward(entry session.SessionEntry) bool {
	if s.item.SourceSession != "" && s.item.SourceSession == s.holder {
		s.delivered("skipped")
		return true
	}
	if s.d.dedupeCache.Seen(s.item.DedupeKey, s.holder) {
		s.delivered("dedupe")
		return true
	}
	refusal := deliveryModeRefusal(s.holder, entry, s.item.Payload)
	if refusal == "" {
		return false
	}
	writeJSON(s.w, http.StatusForbidden, capabilityRefusal{
		Error:  refusal,
		Reason: "capability",
		Holder: s.holder,
	})
	s.settle("capability", "failed")
	return true
}

// sendToRole delivers a role message through the publish route. The first resolution keeps
// resolveLiveRoleHolder's rule and its answers (404 unclaimed or holder_lapsed, a read error's
// 500). The first attempt forwards to that holder when its session entry passes the role lane's
// freshness test (laneHolderFresh), without reading the claim again; every other attempt resolves
// the holder again by the role lane's rule (resolveLaneRoleHolder), so a holder that relaunched
// under a new session between attempts gets the next one. Each attempt forwards the same envelope,
// under the role forward's dedupe key, with a receipt window of what is left of the send, at most
// roleReceiptTimeout. Every forward carries the same Nats-Msg-Id (bus.Client.RequestCoreTo), so the
// stream, which captures agent subjects, keeps one copy however many attempts there are.
func sendToRole(ctx context.Context, w http.ResponseWriter, d *listenerDeps, item contracts.Envelope, role string, arrived time.Time) {
	send := &roleSend{w: w, d: d, item: item, role: role, arrived: arrived}
	type liveResolution struct {
		result roleHolderResult
		err    error
	}
	first, err := readBefore(ctx, func() liveResolution {
		result, err := resolveLiveRoleHolder(d.registry, d.sessions, role)
		return liveResolution{result: result, err: err}
	})
	if err != nil {
		send.timedOut(nil)
		return
	}
	if first.err != nil {
		send.failed(first.err)
		return
	}
	if first.result.state != roleHolderLive {
		send.noHolder(first.result)
		return
	}
	send.holder = first.result.holder
	sender, err := readBefore(ctx, func() *contracts.EnvelopeSender {
		return senderStamp(d.registry, d.sessions, item.SourceSession)
	})
	if err != nil {
		send.timedOut(nil)
		return
	}
	send.item.Sender = sender
	if send.settledBeforeForward(first.result.entry) {
		return
	}
	checked := send.holder
	forwarded := send.item
	forwarded.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
	var lastForward error
	for pass := 0; ; pass++ {
		var resolved laneHolderResolution
		if pass == 0 && laneHolderFresh(first.result.entry, time.Now().UnixMilli()) {
			resolved = laneHolderResolution{outcome: laneHolderLive, holder: first.result.holder, entry: first.result.entry}
		} else {
			resolved, err = readBefore(ctx, func() laneHolderResolution {
				return resolveLaneRoleHolder(d.registry, d.sessions, role)
			})
			if err != nil {
				break
			}
		}
		if resolved.holder != "" {
			send.holder = resolved.holder
		}
		switch {
		case resolved.outcome == laneHolderUnclaimed:
			send.noHolder(roleHolderResult{state: roleHolderUnclaimed})
			return
		case resolved.outcome == laneHolderNotLive && resolved.release != store.ExpiredRoleClaimRetained:
			send.noHolder(roleHolderResult{
				state:             roleHolderLapsed,
				holder:            resolved.holder,
				lastSeen:          resolved.lastSeen,
				lastSeenAvailable: resolved.lastSeen != 0,
				claimRelease:      resolved.release,
			})
			return
		case resolved.outcome == laneHolderLive:
			if send.holder != checked {
				checked = send.holder
				if send.settledBeforeForward(resolved.entry) {
					return
				}
			}
			window := receiptWindow(ctx)
			if window <= 0 {
				send.timedOut(lastForward)
				return
			}
			send.attempts++
			err := d.forward(contracts.AgentSubject(send.holder), forwarded, window)
			if err == nil {
				d.dedupeCache.Record(item.DedupeKey, send.holder)
				send.delivered("delivered")
				return
			}
			if forwardRefused(err) {
				send.failed(err)
				return
			}
			lastForward = err
		}
		// Any other resolution (a holder that has not heartbeated within session.ClaimStaleAfter
		// and keeps its claim, a read that failed, a claim that churned) fails this attempt only.
		if !pauseBeforeRetry(ctx) {
			break
		}
	}
	send.timedOut(lastForward)
}

// topicRecipients lists the sessions an ad-hoc topic send confirms the message with: every session
// whose registered interests match the topic on any machine (store.Registry.MatchAll), whose
// session entry is live and self-subscribed, other than the sender. A self-subscribed session holds
// its own NATS connection and answers a receipt on its agent subject; a session served on a port
// does not, and keeps the live lane's delivery.
func topicRecipients(d *listenerDeps, item contracts.Envelope) []string {
	var recipients []string
	for _, interest := range d.registry.MatchAll(item.Topic) {
		if interest.SessionID == item.SourceSession {
			continue
		}
		if entry, err := d.sessions.Get(interest.SessionID); err == nil && entry.SelfSubscribed {
			recipients = append(recipients, interest.SessionID)
		}
	}
	return recipients
}

// confirmTopicRecipient forwards item, unchanged, to sessionID's agent subject until its receipt
// arrives or ctx ends, by a role send's retry rule, and reports how the recipient ended: delivered,
// dedupe (it had taken this key already), receipt_timeout or delivery_failed. The recipient is
// checked live and self-subscribed before each attempt, and one that took the key already is not
// sent it again. A forward refused however often it is sent (forwardRefused) ends this recipient
// alone as delivery_failed: the envelope itself already passed the topic's publish, so the refusal
// is the recipient's subject or the grant's, not the sender's input.
func confirmTopicRecipient(ctx context.Context, d *listenerDeps, item contracts.Envelope, sessionID string) string {
	var lastForward error
	for {
		if d.dedupeCache.Seen(item.DedupeKey, sessionID) {
			return "dedupe"
		}
		if entry, err := d.sessions.Get(sessionID); err == nil && entry.SelfSubscribed {
			window := receiptWindow(ctx)
			if window <= 0 {
				break
			}
			err := d.forward(contracts.AgentSubject(sessionID), item, window)
			if err == nil {
				d.dedupeCache.Record(item.DedupeKey, sessionID)
				return "delivered"
			}
			if forwardRefused(err) {
				d.logger.DeliveryLog(slog.LevelWarn, "listener publish topic forward refused", sessionID, item.Topic, item.EventID, "failed", slog.String("error", err.Error()))
				return "delivery_failed"
			}
			lastForward = err
		}
		if !pauseBeforeRetry(ctx) {
			break
		}
	}
	return unconfirmedReason(lastForward)
}

// sendToTopic delivers an ad-hoc topic message through the publish route: a topic no stream subject
// captures (bus.UsesCoreTransport), which core NATS hands once to whoever is subscribed at that
// moment. It still publishes the envelope on the topic, for the serve-port sessions the live lane
// delivers to and any subscriber the registry does not know; then it forwards the same envelope,
// topic, event_id and dedupe_key unchanged, to the agent subject of every recipient
// (topicRecipients) in parallel and answers once each confirmed, or 504 naming each that did not
// once publishDeliveryWindow from arrived has passed. A recipient that took both copies drops the
// second by its event_id.
func sendToTopic(ctx context.Context, w http.ResponseWriter, d *listenerDeps, item contracts.Envelope, arrived time.Time) {
	recipients := topicRecipients(d, item)
	settle := func(outcome string, delivered []string, undelivered []undeliveredRecipient) {
		level := slog.LevelInfo
		if outcome != "delivered" {
			level = slog.LevelWarn
		}
		d.logger.DeliveryLog(level, "listener publish topic send", "", item.Topic, item.EventID, outcome,
			slog.String("dedupe_key", item.DedupeKey),
			slog.Any("delivered", delivered),
			slog.Any("undelivered", undelivered),
			slog.String("elapsed", time.Since(arrived).String()))
	}
	notConfirmed := func(delivered []string, undelivered []undeliveredRecipient) {
		// The error names each session, since a client that shows only the error (envoy-client's
		// EnvoyApiError) must still tell its agent which sessions did not take the message.
		named := make([]string, len(undelivered))
		for i, recipient := range undelivered {
			named[i] = fmt.Sprintf("%s (%s)", recipient.SessionID, recipient.Reason)
		}
		writeJSON(w, http.StatusGatewayTimeout, topicSendFailure{
			Error: fmt.Sprintf("%d of %d sessions subscribed to %s did not confirm event %s within %s: %s",
				len(undelivered), len(recipients), item.Topic, item.EventID, publishDeliveryWindow, strings.Join(named, ", ")),
			Reason:      "not_confirmed",
			EventID:     item.EventID,
			DedupeKey:   item.DedupeKey,
			Delivered:   delivered,
			Undelivered: undelivered,
		})
		settle("not_confirmed", delivered, undelivered)
	}
	sender, err := readBefore(ctx, func() *contracts.EnvelopeSender {
		return senderStamp(d.registry, d.sessions, item.SourceSession)
	})
	if err != nil {
		// Nothing was published: every recipient is undelivered, and so is a send that had none.
		undelivered := make([]undeliveredRecipient, 0, len(recipients))
		for _, sessionID := range recipients {
			undelivered = append(undelivered, undeliveredRecipient{SessionID: sessionID, Reason: "delivery_failed"})
		}
		notConfirmed([]string{}, undelivered)
		return
	}
	item.Sender = sender
	if err := d.client.Publish(item); err != nil {
		writeNATSError(w, err, http.StatusInternalServerError, err.Error())
		settle("failed", nil, nil)
		return
	}
	statuses := make([]string, len(recipients))
	var wg sync.WaitGroup
	for i, sessionID := range recipients {
		wg.Go(func() { statuses[i] = confirmTopicRecipient(ctx, d, item, sessionID) })
	}
	wg.Wait()
	delivered := []string{}
	undelivered := []undeliveredRecipient{}
	for i, status := range statuses {
		switch status {
		case "delivered", "dedupe":
			delivered = append(delivered, recipients[i])
			d.messagesDelivered.Inc([2]string{"delivery_status", status})
		default:
			undelivered = append(undelivered, undeliveredRecipient{SessionID: recipients[i], Reason: status})
			d.messagesDelivered.Inc([2]string{"delivery_status", deliveryStatus(status)})
		}
	}
	if len(undelivered) > 0 {
		notConfirmed(delivered, undelivered)
		return
	}
	writeJSON(w, http.StatusOK, publishResponse{Envelope: item})
	settle("delivered", delivered, undelivered)
}
