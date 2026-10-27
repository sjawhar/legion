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

// topicSendFailure is the 504 of an ad-hoc topic send that published the message and that some
// recipient did not confirm inside the window. Delivered names every recipient that did; an empty
// list is [] rather than null.
type topicSendFailure struct {
	Error       string                 `json:"error"`
	Reason      string                 `json:"reason"`
	EventID     string                 `json:"event_id"`
	DedupeKey   string                 `json:"dedupe_key"`
	Delivered   []string               `json:"delivered"`
	Undelivered []undeliveredRecipient `json:"undelivered"`
}

// topicPublishFailure is the 504 of an ad-hoc topic send whose own publish on the topic did not
// reach NATS, reason publish_failed: no recipient was forwarded the message, so the answer lists
// none, and its error says the topic was not published, which is all a client that shows only the
// error (envoy-client's EnvoyApiError) tells its agent.
type topicPublishFailure struct {
	Error     string `json:"error"`
	Reason    string `json:"reason"`
	EventID   string `json:"event_id"`
	DedupeKey string `json:"dedupe_key"`
}

// sendOutcome is how a confirmed send ended, or how one recipient of an ad-hoc topic send did. Its
// value is the outcome the send's log line names and, where the answer carries a reason, that
// reason, so the log, the answer and the role lane's own lines read alike.
type sendOutcome string

const (
	// sendDelivered is a send whose recipient's receipt arrived through this route.
	sendDelivered sendOutcome = "delivered"
	// sendDedupe is a recipient that had confirmed this dedupe key through the route already, so
	// it was not sent the message again.
	sendDedupe sendOutcome = "dedupe"
	// sendSkipped is a role send from the role's own holder, the role lane's echo skip.
	sendSkipped sendOutcome = "skipped"
	// sendUnclaimed and sendHolderLapsed are a role send with no live claim, answered 404.
	sendUnclaimed    sendOutcome = sendOutcome(roleHolderUnclaimed)
	sendHolderLapsed sendOutcome = sendOutcome(roleHolderLapsed)
	// sendReceiptTimeout is a recipient whose last forward reached the server and drew no receipt.
	sendReceiptTimeout sendOutcome = "receipt_timeout"
	// sendDeliveryFailed is a recipient that no forward was shown to reach: none live to forward
	// to, a read that failed or ran out of time, or a forward not shown to have reached the server.
	sendDeliveryFailed sendOutcome = "delivery_failed"
	// sendCapability is a role holder that cannot take the payload's delivery mode, answered 403.
	sendCapability sendOutcome = "capability"
	// sendFailed is a send refused as writeNATSError maps it, or a role-claim read that failed.
	sendFailed sendOutcome = "failed"
	// sendNotConfirmed is an ad-hoc topic send that published and that some recipient did not
	// confirm.
	sendNotConfirmed sendOutcome = "not_confirmed"
	// sendPublishFailed is an ad-hoc topic send whose own publish did not reach NATS.
	sendPublishFailed sendOutcome = "publish_failed"
)

// reason is the outcome as an answer's JSON reason names it.
func (o sendOutcome) reason() string { return string(o) }

// deliveryStatus is the messagesDelivered label the role lane gives the same outcome, so the
// route's counts read as the lane's: an unclaimed role is skipped, and every outcome that is
// neither taken, skipped nor a receipt timeout is failed.
func (o sendOutcome) deliveryStatus() string {
	switch o {
	case sendDelivered, sendDedupe, sendSkipped, sendReceiptTimeout:
		return string(o)
	case sendUnclaimed:
		return string(sendSkipped)
	default:
		return string(sendFailed)
	}
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

// unconfirmedReason is the outcome a send gives a recipient that never sent its receipt, from the
// last forward made to it: receipt_timeout when that forward reached the server and drew no
// receipt, delivery_failed for anything else, no forward at all among them.
func unconfirmedReason(lastForward error) sendOutcome {
	if errors.Is(lastForward, bus.ErrReceiptTimeout) {
		return sendReceiptTimeout
	}
	return sendDeliveryFailed
}

// confirm records that sessionID's receipt for the message under dedupeKey arrived through the
// route. The route's own record (listenerDeps.confirmed) is what a later send of the same key
// reads, so envoy-client's retry of a send skips a recipient the first attempt confirmed; the
// delivery handlers' cache is told too, so they do not push the same key to it again.
func (d *listenerDeps) confirm(dedupeKey, sessionID string) {
	d.confirmed.Record(dedupeKey, sessionID)
	d.dedupeCache.Record(dedupeKey, sessionID)
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
// It counts the label the role lane gives the same outcome.
func (s *roleSend) settle(outcome sendOutcome) {
	metricStatus := outcome.deliveryStatus()
	s.d.messagesDelivered.Inc([2]string{"delivery_status", metricStatus})
	level := slog.LevelWarn
	switch metricStatus {
	case string(sendDelivered), string(sendSkipped), string(sendDedupe):
		level = slog.LevelInfo
	}
	s.d.logger.DeliveryLog(level, "listener publish role send", s.holder, s.item.Topic, s.item.EventID, string(outcome),
		slog.String("dedupe_key", s.item.DedupeKey),
		slog.Int("attempts", s.attempts),
		slog.String("elapsed", time.Since(s.arrived).String()))
}

// delivered answers 200 with the envelope and the holder that took it.
func (s *roleSend) delivered(outcome sendOutcome) {
	writeJSON(s.w, http.StatusOK, publishResponse{Envelope: s.item, Holder: s.holder})
	s.settle(outcome)
}

// noHolder answers writeRoleHolderError's 404 (unclaimed or holder_lapsed) for a role with no live
// claim, which the Legion daemon reads as notify.ErrNoHolder. The role lane counts an unclaimed
// role skipped and a lapsed holder failed.
func (s *roleSend) noHolder(result roleHolderResult) {
	writeRoleHolderError(s.w, s.role, result)
	outcome := sendHolderLapsed
	if result.state == roleHolderUnclaimed {
		outcome = sendUnclaimed
	}
	s.settle(outcome)
}

// failed answers a failure that is not the window's end, as writeNATSError maps it.
func (s *roleSend) failed(err error) {
	writeNATSError(s.w, err, http.StatusInternalServerError, err.Error())
	s.settle(sendFailed)
}

// timedOut answers the 504 of a send whose window ended without a receipt.
func (s *roleSend) timedOut(lastForward error) {
	outcome := unconfirmedReason(lastForward)
	writeJSON(s.w, http.StatusGatewayTimeout, roleSendFailure{
		Error: fmt.Sprintf("role %s: event %s was not confirmed by its holder %s within %s (%s)",
			s.role, s.item.EventID, cmp.Or(s.holder, "(none)"), publishDeliveryWindow, outcome.reason()),
		Reason:    outcome.reason(),
		EventID:   s.item.EventID,
		DedupeKey: s.item.DedupeKey,
		Holder:    s.holder,
		Attempts:  s.attempts,
	})
	s.settle(outcome)
}

// settledBeforeForward applies, to a holder the send has not checked yet, whose session entry is
// entry, the rules the role lane applies before it forwards, in the lane's order, and reports
// whether it answered: the source session holds the role (the lane's echo skip) or the holder
// already confirmed this dedupe key through the route, both answered 200 with nothing forwarded;
// the holder cannot take the payload's delivery mode (deliveryModeRefusal, sendHandler's refusal,
// read as roleTopicDelivery reads it), answered 403. Guarding after the dedupe check keeps a
// delivered repeat from being judged by the holder's current capabilities.
//
// The dedupe check reads the route's own record (listenerDeps.confirmed), not the delivery
// handlers' cache: agentSubjectDelivery and fanoutDelivery record a key there for a self-subscribed
// session whose push they skipped, having heard nothing from it, so a role send under a key one of
// them saw would be answered 200 for a holder that never confirmed it.
func (s *roleSend) settledBeforeForward(entry session.SessionEntry) bool {
	if s.item.SourceSession != "" && s.item.SourceSession == s.holder {
		s.delivered(sendSkipped)
		return true
	}
	if s.d.confirmed.Seen(s.item.DedupeKey, s.holder) {
		s.delivered(sendDedupe)
		return true
	}
	refusal := deliveryModeRefusal(s.holder, entry, s.item.Payload)
	if refusal == "" {
		return false
	}
	writeJSON(s.w, http.StatusForbidden, capabilityRefusal{
		Error:  refusal,
		Reason: sendCapability.reason(),
		Holder: s.holder,
	})
	s.settle(sendCapability)
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
				d.confirm(item.DedupeKey, send.holder)
				send.delivered(sendDelivered)
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
// dedupe (it had confirmed this key through the route already), receipt_timeout or delivery_failed.
// The recipient is checked live and self-subscribed before each attempt, and one that confirmed the
// key already is not sent it again. A forward refused however often it is sent (forwardRefused)
// ends this recipient alone as delivery_failed: the envelope itself already passed the topic's
// publish, so the refusal is the recipient's subject or the grant's, not the sender's input.
//
// Only a receipt that arrived through the route counts, which is why it reads the route's own
// record (listenerDeps.confirmed) and not the delivery handlers' cache: with the live topics lane
// on, the lane receives this send's own publish on the topic, and its fanout records there every
// self-subscribed session on this machine whose push it skipped, having heard nothing from it.
func confirmTopicRecipient(ctx context.Context, d *listenerDeps, item contracts.Envelope, sessionID string) sendOutcome {
	var lastForward error
	for {
		if d.confirmed.Seen(item.DedupeKey, sessionID) {
			return sendDedupe
		}
		if entry, err := d.sessions.Get(sessionID); err == nil && entry.SelfSubscribed {
			window := receiptWindow(ctx)
			if window <= 0 {
				break
			}
			err := d.forward(contracts.AgentSubject(sessionID), item, window)
			if err == nil {
				d.confirm(item.DedupeKey, sessionID)
				return sendDelivered
			}
			if forwardRefused(err) {
				d.logger.DeliveryLog(slog.LevelWarn, "listener publish topic forward refused", sessionID, item.Topic, item.EventID, string(sendFailed), slog.String("error", err.Error()))
				return sendDeliveryFailed
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
// (topicRecipients) in parallel and answers once each confirmed, or 504 not_confirmed naming each
// that did not once publishDeliveryWindow from arrived has passed. A recipient that took both
// copies drops the second by its event_id.
//
// A send whose own publish on the topic did not reach NATS answers 504 publish_failed, whether the
// window ended first (the sender-stamp read or the publish's reconnect wait used it up) or the
// publish failed at once on a connection that is down, so its status does not depend on how soon
// the failure came; nothing was forwarded, and with no recipient the send is answered 200 only once
// the topic was published. A publish NATS refuses however often it is sent (forwardRefused) is the
// sender's input or the grant's, answered as writeNATSError maps it.
func sendToTopic(ctx context.Context, w http.ResponseWriter, d *listenerDeps, item contracts.Envelope, arrived time.Time) {
	recipients := topicRecipients(d, item)
	settle := func(outcome sendOutcome, delivered []string, undelivered []undeliveredRecipient, attrs ...slog.Attr) {
		level := slog.LevelInfo
		if outcome != sendDelivered {
			level = slog.LevelWarn
		}
		attrs = append([]slog.Attr{
			slog.String("dedupe_key", item.DedupeKey),
			slog.Any("delivered", delivered),
			slog.Any("undelivered", undelivered),
			slog.String("elapsed", time.Since(arrived).String()),
		}, attrs...)
		d.logger.DeliveryLog(level, "listener publish topic send", "", item.Topic, item.EventID, string(outcome), attrs...)
	}
	notPublished := func(cause error) {
		writeJSON(w, http.StatusGatewayTimeout, topicPublishFailure{
			Error:     fmt.Sprintf("%s was not published within %s: %v; no session was sent it", item.Topic, publishDeliveryWindow, cause),
			Reason:    sendPublishFailed.reason(),
			EventID:   item.EventID,
			DedupeKey: item.DedupeKey,
		})
		settle(sendPublishFailed, nil, nil, slog.String("error", cause.Error()))
	}
	sender, err := readBefore(ctx, func() *contracts.EnvelopeSender {
		return senderStamp(d.registry, d.sessions, item.SourceSession)
	})
	if err != nil {
		notPublished(fmt.Errorf("reading the sender's session: %w", err))
		return
	}
	item.Sender = sender
	// The topic's own core publish waits for a reconnecting connection as long as the send's window
	// lasts and no longer, so a hub that is down is answered inside the window, never after the
	// bus's own reconnect wait, which outlasts envoy-client's 5 s attempt.
	if err := d.client.PublishCoreContext(ctx, item.Topic, item); err != nil {
		if forwardRefused(err) {
			writeNATSError(w, err, http.StatusInternalServerError, err.Error())
			settle(sendFailed, nil, nil, slog.String("error", err.Error()))
			return
		}
		notPublished(err)
		return
	}
	outcomes := make([]sendOutcome, len(recipients))
	var wg sync.WaitGroup
	for i, sessionID := range recipients {
		wg.Go(func() { outcomes[i] = confirmTopicRecipient(ctx, d, item, sessionID) })
	}
	wg.Wait()
	delivered := []string{}
	undelivered := []undeliveredRecipient{}
	for i, outcome := range outcomes {
		d.messagesDelivered.Inc([2]string{"delivery_status", outcome.deliveryStatus()})
		switch outcome {
		case sendDelivered, sendDedupe:
			delivered = append(delivered, recipients[i])
		default:
			undelivered = append(undelivered, undeliveredRecipient{SessionID: recipients[i], Reason: outcome.reason()})
		}
	}
	if len(undelivered) > 0 {
		// The error names each session, since a client that shows only the error (envoy-client's
		// EnvoyApiError) must still tell its agent which sessions did not take the message.
		named := make([]string, len(undelivered))
		for i, recipient := range undelivered {
			named[i] = fmt.Sprintf("%s (%s)", recipient.SessionID, recipient.Reason)
		}
		writeJSON(w, http.StatusGatewayTimeout, topicSendFailure{
			Error: fmt.Sprintf("%d of %d sessions subscribed to %s did not confirm event %s within %s: %s",
				len(undelivered), len(recipients), item.Topic, item.EventID, publishDeliveryWindow, strings.Join(named, ", ")),
			Reason:      sendNotConfirmed.reason(),
			EventID:     item.EventID,
			DedupeKey:   item.DedupeKey,
			Delivered:   delivered,
			Undelivered: undelivered,
		})
		settle(sendNotConfirmed, delivered, undelivered)
		return
	}
	writeJSON(w, http.StatusOK, publishResponse{Envelope: item})
	settle(sendDelivered, delivered, undelivered)
}
