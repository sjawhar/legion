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
	"github.com/sjawhar/envoy/internal/roles"
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
	Error     string      `json:"error"`
	Reason    sendOutcome `json:"reason"`
	EventID   string      `json:"event_id"`
	DedupeKey string      `json:"dedupe_key"`
	Holder    string      `json:"holder"`
	Attempts  int         `json:"attempts"`
}

// capabilityRefusal is the 403 of a role send whose holder cannot take the payload's delivery
// mode: it does not advertise the mode, or the mode cannot be read unambiguously. Its error reads
// as /v1/messages/send's answer to the same refusal.
type capabilityRefusal struct {
	Error  string      `json:"error"`
	Reason sendOutcome `json:"reason"`
	Holder string      `json:"holder"`
}

// undeliveredRecipient names one session of an ad-hoc topic send that did not confirm the message,
// with the reason a role send would give.
type undeliveredRecipient struct {
	SessionID string      `json:"session_id"`
	Reason    sendOutcome `json:"reason"`
}

// topicSendFailure is the 504 of an ad-hoc topic send that published the message and that some
// recipient did not confirm inside the window. Delivered names every recipient that did; an empty
// list is [] rather than null. Unresponsive names every recipient past the live window, which the
// message was stored for and no receipt asked of (confirmTopicRecipient): it is neither delivered
// nor undelivered, and it is left out when there is none, so an answer with no such recipient
// reads exactly as it did before.
type topicSendFailure struct {
	Error        string                 `json:"error"`
	Reason       sendOutcome            `json:"reason"`
	EventID      string                 `json:"event_id"`
	DedupeKey    string                 `json:"dedupe_key"`
	Delivered    []string               `json:"delivered"`
	Undelivered  []undeliveredRecipient `json:"undelivered"`
	Unresponsive []string               `json:"unresponsive,omitempty"`
}

// topicPublishFailure is the 504 of an ad-hoc topic send whose own publish on the topic did not
// reach NATS, reason publish_failed: no recipient was forwarded the message, so the answer lists
// none, and its error says the topic was not published, which is all a client that shows only the
// error (envoy-client's EnvoyApiError) tells its agent.
type topicPublishFailure struct {
	Error     string      `json:"error"`
	Reason    sendOutcome `json:"reason"`
	EventID   string      `json:"event_id"`
	DedupeKey string      `json:"dedupe_key"`
}

// sendOutcome is how a confirmed send ended, or how one recipient of an ad-hoc topic send did. An
// answer that carries a reason carries the outcome itself, which marshals as its string. A send's
// log line names the outcome as its reason attribute and deliveryStatus's label for it as its
// delivery_status attribute, so the reason reads as the answer does and delivery_status reads as
// the role lane's own lines and the messagesDelivered count do.
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
	sendUnclaimed    sendOutcome = sendOutcome(roles.Unclaimed)
	sendHolderLapsed sendOutcome = sendOutcome(roles.Lapsed)
	// sendReceiptTimeout is a recipient whose last forward reached the server and drew no receipt.
	sendReceiptTimeout sendOutcome = "receipt_timeout"
	// sendDeliveryFailed is a recipient that no forward was shown to reach: none live to forward
	// to, a read that failed or ran out of time, or a forward not shown to have reached the server.
	sendDeliveryFailed sendOutcome = "delivery_failed"
	// sendCapability is a role holder that cannot take the payload's delivery mode, answered 403.
	sendCapability sendOutcome = "capability"
	// sendFailed is a send refused as requestRecord.refuseBusUnavailable maps it (a forward or
	// publish NATS refuses or the grant denies), or a role send whose first resolution of the
	// holder failed, answered as GET /v1/roles/<role> answers it.
	sendFailed sendOutcome = "failed"
	// sendNotConfirmed is an ad-hoc topic send that published and that some recipient did not
	// confirm.
	sendNotConfirmed sendOutcome = "not_confirmed"
	// sendPublishFailed is an ad-hoc topic send whose own publish did not reach NATS.
	sendPublishFailed sendOutcome = "publish_failed"
	// sendUnresponsive is a recipient of an ad-hoc topic send that is past the live window: the
	// message was forwarded to its agent subject and the stream stored it there, and no receipt
	// was waited for, since a session that answers nothing answers no receipt either. It is a
	// skip of the wait and not a failure, so it neither makes the send a 504 nor is named
	// undelivered; the answer names it under unresponsive, as the session the message is kept
	// for.
	sendUnresponsive sendOutcome = "unresponsive"
)

// deliveryStatus is the label the role lane gives the same outcome, which the route counts under
// messagesDelivered and logs as delivery_status, so the route's counts and lines read as the
// lane's: an unclaimed role and an unresponsive recipient are skipped, and every outcome that is
// neither taken, skipped nor a receipt timeout is failed.
func (o sendOutcome) deliveryStatus() string {
	switch o {
	case sendDelivered, sendDedupe, sendSkipped, sendReceiptTimeout:
		return string(o)
	case sendUnclaimed, sendUnresponsive:
		return string(sendSkipped)
	default:
		return string(sendFailed)
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

// forwardAndWait forwards item to subject, under msgID as its Nats-Msg-Id when that is not empty,
// through the delivery configuration's forwardRole and waits for its outcome, which is the form a
// route needs: it answers its sender with what the forward did, where the role lane hands the wait
// over and reports it later.
func (d *listenerDeps) forwardAndWait(subject string, item contracts.Envelope, msgID string, window time.Duration, storedOnly bool) error {
	wait, err := d.delivery.forwardRole(subject, item, msgID, window, storedOnly)
	if err != nil {
		return err
	}
	return wait()
}

// forwardRefused reports whether a forward failed in a way another attempt repeats: NATS refuses
// the envelope or subject however often it is sent (bus.ErrRefused), or the listener's grant does
// not allow the subject (bus.ErrPublishDenied). A role send answers it at once, as
// requestRecord.refuseBusUnavailable maps it (413 or 400 for a refusal, 403 for a denial); an
// ad-hoc topic send stops sending to that recipient (confirmTopicRecipient).
func forwardRefused(err error) bool {
	return errors.Is(err, bus.ErrRefused) || errors.Is(err, bus.ErrPublishDenied)
}

// unconfirmedReason is the outcome a send gives a recipient that never sent its receipt, from the
// last forward made to it: receipt_timeout when that forward reached the server and drew no
// receipt, delivery_failed for anything else — a forward the stream did not store
// (bus.ErrForwardNotStored), and no forward at all, among them.
func unconfirmedReason(lastForward error) sendOutcome {
	if errors.Is(lastForward, bus.ErrReceiptTimeout) {
		return sendReceiptTimeout
	}
	return sendDeliveryFailed
}

// confirm records that sessionID's receipt for the message under dedupeKey arrived through the
// route. The route's own record (listenerDeps.confirmed) is what a later send of the same key
// reads, so envoy-client's retry of a send skips a recipient the first attempt confirmed; the
// delivery handlers' cache is told too, so they do not deliver the same key to it again.
func (d *listenerDeps) confirm(dedupeKey, sessionID string) {
	d.confirmed.Record(dedupeKey, sessionID)
	d.delivery.dedupeCache.Record(dedupeKey, sessionID)
}

// roleSend is one role message through the publish route. The route does not publish on the role
// lane: it forwards the envelope to the holder's agent subject itself and answers once the holder's
// receipt arrived, or with the failure once publishDeliveryWindow from arrived has passed, so its
// sender learns whether the message was taken. It publishes no delivery exception: the sender has
// the answer. Every answer goes through record, the route's one line per request
// (request_record.go), and is then settled: counted, and logged on the send's own delivery line.
type roleSend struct {
	w       http.ResponseWriter
	record  *requestRecord
	d       *listenerDeps
	item    contracts.Envelope
	role    string
	arrived time.Time
	holder  string
	// holderState is the state the holder's own record put it in at the resolution that named
	// it, carried so the 200 can say it (GET /v1/roles/<role> says it the same way). A 200 is
	// answered only for a holder a resolution read live, so that is what it carries there.
	holderState session.State
	attempts    int
}

// settle counts and logs the send's outcome, the one delivery line a send writes, once its answer
// is written. It counts and logs as delivery_status the label the role lane gives the same outcome,
// and logs the outcome itself as reason.
func (s *roleSend) settle(outcome sendOutcome) {
	s.d.delivery.messagesDelivered.Inc([2]string{"delivery_status", outcome.deliveryStatus()})
	level := slog.LevelWarn
	switch outcome {
	case sendDelivered, sendSkipped, sendDedupe, sendUnclaimed:
		level = slog.LevelInfo
	}
	s.d.delivery.logger.DeliveryLog(level, "listener publish role send", s.holder, s.item.Topic, s.item.EventID, outcome.deliveryStatus(),
		slog.String("reason", string(outcome)),
		slog.String("dedupe_key", s.item.DedupeKey),
		slog.Int("attempts", s.attempts),
		slog.String("elapsed", time.Since(s.arrived).String()))
}

// delivered answers 200 with the envelope, the holder that took it and the state that holder's
// record put it in, which GET /v1/roles/<role> names the same way.
func (s *roleSend) delivered(outcome sendOutcome) {
	s.record.holder = s.holder
	s.record.accept(s.w, publishResponse{Envelope: s.item, Holder: s.holder, HolderState: s.holderState})
	s.settle(outcome)
}

// noHolder answers writeRoleHolderError's 404 (unclaimed or holder_lapsed) for a role with no live
// claim, which the Legion daemon reads as notify.ErrNoHolder, recorded role_unclaimed or
// role_lapsed. The role lane counts an unclaimed role skipped and a lapsed holder failed.
func (s *roleSend) noHolder(result roles.Resolution) {
	s.record.refuseRoleHolder(s.w, s.role, result)
	outcome := sendHolderLapsed
	if result.State == roles.Unclaimed {
		outcome = sendUnclaimed
	}
	s.settle(outcome)
}

// unresolved answers a first resolution of the holder that did not resolve, as GET
// /v1/roles/<role> answers it: a claim record no read can decode (store.ErrMalformedRoleClaim) is
// the 500 that names it, and any other failure - a read that did not answer within its cap or
// sendRequestBudget among them - the bus-unavailable 503, or the 413 or 400 a refused key gets.
// Each is recorded under its own outcome.
func (s *roleSend) unresolved(err error) {
	switch {
	case errors.Is(err, store.ErrMalformedRoleClaim):
		s.record.refuseRoleClaimUnreadable(s.w, s.role, err)
	default:
		s.record.refuseBusUnavailable(s.w, err)
	}
	s.settle(sendFailed)
}

// refused answers a forward NATS refuses however often it is sent, or one the listener's grant
// denies (forwardRefused), as requestRecord.refuseBusUnavailable maps it.
func (s *roleSend) refused(err error) {
	s.record.holder = s.holder
	s.record.refuseBusUnavailable(s.w, err)
	s.settle(sendFailed)
}

// timedOut answers the 504 of a send whose window ended without a receipt, recorded under its
// reason.
func (s *roleSend) timedOut(lastForward error) {
	outcome := unconfirmedReason(lastForward)
	recorded := outcomeDeliveryFailed
	if outcome == sendReceiptTimeout {
		recorded = outcomeReceiptTimeout
	}
	s.record.holder = s.holder
	s.record.refuseWith(s.w, http.StatusGatewayTimeout, recorded, roleSendFailure{
		Error: fmt.Sprintf("role %s: event %s was not confirmed by its holder %s within %s (%s)",
			s.role, s.item.EventID, cmp.Or(s.holder, "(none)"), publishDeliveryWindow, outcome),
		Reason:    outcome,
		EventID:   s.item.EventID,
		DedupeKey: s.item.DedupeKey,
		Holder:    s.holder,
		Attempts:  s.attempts,
	})
	s.settle(outcome)
}

// settledBeforeForward judges the message to a holder the send has not checked yet, whose session
// entry is entry, by beforeForward, the rules the role lane applies before it forwards, and reports
// whether it answered: an echo (the source session holds the role) or a holder that already
// confirmed this dedupe key through the route, both answered 200 with nothing forwarded; a holder
// that cannot take the payload's delivery mode, answered 403 with the refusal sendHandler gives,
// recorded capability_refused as sendHandler records it.
//
// The dedupe check reads the route's own record (listenerDeps.confirmed), not the delivery
// handlers' cache: agentSubjectDelivery and fanoutDelivery record a key there for a self-subscribed
// session whose delivery they skipped, having heard nothing from it, so a role send under a key one of
// them saw would be answered 200 for a holder that never confirmed it.
func (s *roleSend) settledBeforeForward(entry session.SessionEntry) bool {
	verdict := beforeForward(s.item, s.holder, entry, s.d.confirmed.Seen)
	switch verdict.kind {
	case forwardAllowed:
		return false
	case forwardEcho:
		s.delivered(sendSkipped)
	case forwardRepeat:
		s.delivered(sendDedupe)
	default:
		s.record.holder = s.holder
		s.record.refuseWith(s.w, http.StatusForbidden, outcomeCapabilityRefused, capabilityRefusal{
			Error:  verdict.refusal,
			Reason: sendCapability,
			Holder: s.holder,
		})
		s.settle(sendCapability)
	}
	return true
}

// sendToRole delivers a role message through the publish route, answering through record. The
// first resolution reads the holder as an HTTP lookup does (roles.ResolveHolder),
// within sendRequestBudget of arrived as GET /v1/roles/<role> reads it, and gives that route's
// answers: 404 unclaimed or holder_lapsed, and for a resolution that failed the lookup's 503 or
// 500 (unresolved). The first attempt forwards to that holder without reading the claim again;
// every other attempt resolves the holder again, so a holder that relaunched under a new session
// between attempts gets the next one. On those attempts an unclaimed role is answered 404 as the
// first resolution answers it, since the claim itself is gone; a holder that stopped answering
// keeps its claim and fails that attempt only, as a read that failed does, so a holder that comes
// back inside the window still gets the message. Each attempt forwards the same envelope, under
// the role forward's dedupe key, with a receipt window of what is left of the send, at most
// roleReceiptTimeout. Every forward goes out under the same Nats-Msg-Id, the forward's dedupe key
// and the holder's subject (bus.ForwardMsgID), so the stream, which captures agent subjects,
// keeps one copy however many attempts there are. Every read takes ctx, the send's window, so
// none outlasts it: a send whose window ends is answered 504.
func sendToRole(ctx context.Context, w http.ResponseWriter, record *requestRecord, d *listenerDeps, item contracts.Envelope, role string, arrived time.Time) {
	send := &roleSend{w: w, record: record, d: d, item: item, role: role, arrived: arrived}
	firstCtx, cancelFirst := context.WithDeadline(ctx, arrived.Add(sendRequestBudget))
	first, err := roles.ResolveHolder(firstCtx, d.registry, roleHolderSessions{d.sessions}, role)
	cancelFirst()
	if err != nil {
		send.unresolved(err)
		return
	}
	if first.State != roles.Live {
		send.noHolder(first)
		return
	}
	send.holder = first.Holder
	send.holderState = first.HolderState
	send.item.Sender = senderStamp(ctx, d.registry, d.sessions, item.SourceSession)
	if send.settledBeforeForward(first.Entry) {
		return
	}
	checked := send.holder
	forwarded := send.item
	forwarded.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
	var lastForward error
	resolved := first
	for pass := 0; ; pass++ {
		if pass > 0 || !sessionIsLive(first.HolderState) {
			resolved, err = roles.ResolveHolder(ctx, d.registry, roleHolderSessions{d.sessions}, role)
			if err != nil && ctx.Err() != nil {
				// The window ended under the resolution: what it read says nothing more.
				break
			}
		}
		if resolved.Holder != "" {
			send.holder = resolved.Holder
			send.holderState = resolved.HolderState
		}
		switch {
		case err != nil:
			// Retried below, as every resolution not answered here is.
		case resolved.State == roles.Unclaimed:
			send.noHolder(resolved)
			return
		case resolved.State == roles.Live:
			if send.holder != checked {
				checked = send.holder
				if send.settledBeforeForward(resolved.Entry) {
					return
				}
			}
			window := receiptWindow(ctx)
			if window <= 0 {
				send.timedOut(lastForward)
				return
			}
			send.attempts++
			subject := contracts.AgentSubject(send.holder)
			// The forward carries the same Nats-Msg-Id on every attempt, so the stream, which
			// captures agent subjects, keeps one copy of it per message and recipient however
			// many attempts this send makes.
			err := d.forwardAndWait(subject, forwarded, bus.ForwardMsgID(forwarded.DedupeKey, subject), window, roleForwardStoredOnly(resolved.HolderState))
			if err == nil {
				d.confirm(item.DedupeKey, send.holder)
				send.delivered(sendDelivered)
				return
			}
			if forwardRefused(err) {
				send.refused(err)
				return
			}
			lastForward = err
		}
		// Any resolution not answered above (a holder past the live window, which keeps its
		// claim, or a read that failed) fails this attempt only.
		if !pauseBeforeRetry(ctx) {
			break
		}
	}
	send.timedOut(lastForward)
}

// topicRecipients lists the sessions an ad-hoc topic send sends the message to: every session
// whose registered interests match the topic on any machine (store.Registry.MatchAll), whose
// session record is not gone and that is self-subscribed, other than the sender. A self-subscribed
// session holds its own NATS connection and answers a receipt on its agent subject; a session that
// holds none answers nothing, so the route confirms nothing with it. A session past the live window
// is listed here and forwarded to by confirmTopicRecipient, which asks it for no receipt, so the
// message is kept for it and the skip of the wait is counted, logged and answered once per send
// rather than hidden in this filter.
func topicRecipients(d *listenerDeps, item contracts.Envelope) []string {
	var recipients []string
	for _, interest := range d.registry.MatchAll(item.Topic) {
		if interest.SessionID == item.SourceSession {
			continue
		}
		if entry, state := d.sessions.Lookup(interest.SessionID); notGone(state) && entry.SelfSubscribed {
			recipients = append(recipients, interest.SessionID)
		}
	}
	return recipients
}

// confirmTopicRecipient forwards item, unchanged, to sessionID's agent subject until its receipt
// arrives or ctx ends, by a role send's retry rule, and reports how the recipient ended: delivered,
// dedupe (it had confirmed this key through the route already), unresponsive, receipt_timeout or
// delivery_failed. The recipient's state and self-subscription are read before each attempt, and
// one that confirmed the key already is not sent it again. A forward refused however often it is
// sent (forwardRefused) ends this recipient alone as delivery_failed: the envelope itself already
// passed the topic's publish, so the refusal is the recipient's subject or the grant's, not the
// sender's input.
//
// A recipient past the live window is forwarded to exactly as a live one is, because that forward
// is the only copy kept for it: the topic itself is a core one, which no stream subject captures
// and nothing stores, while the agent subject the forward goes to is captured, so the stream
// holds the message until the session reads it back on its return (POST /v1/replay, or its
// session consumer). What such a recipient is not asked for is a receipt - a session that answers
// nothing answers no receipt either, and waiting for one would spend the rest of the send's
// window - so its forward counts as made on the stream's acknowledgement alone (the storedOnly
// forward of bus.Client.StartRequestStoredTo),
// and it ends unresponsive: named in the answer as a recipient the message is kept for, and not
// a delivery failure. A forward the stream did not store is no copy kept, so it is retried and
// reported like any other, rather than passed off as a skip.
//
// Only a receipt that arrived through the route counts, which is why it reads the route's own
// record (listenerDeps.confirmed) and not the delivery handlers' dedupe cache: that cache holds
// the session a delivery handed the message to and the session whose delivery it skipped alike,
// and neither entry is a recipient's word that it has the message. An ad-hoc topic's own publish
// is core NATS, which the stream does not store (sendToTopic below), so no delivery handler reads
// this send back at all: the receipts this loop records are the only account of who took it.
func confirmTopicRecipient(ctx context.Context, d *listenerDeps, item contracts.Envelope, sessionID string) sendOutcome {
	var lastForward error
	for {
		if d.confirmed.Seen(item.DedupeKey, sessionID) {
			return sendDedupe
		}
		entry, state := d.sessions.Lookup(sessionID)
		if notGone(state) && entry.SelfSubscribed {
			window := receiptWindow(ctx)
			if window <= 0 {
				break
			}
			subject := contracts.AgentSubject(sessionID)
			storedOnly := state == session.Unresponsive
			// As a role send's forward does: one stored copy per message and recipient,
			// however many attempts this recipient takes.
			err := d.forwardAndWait(subject, item, bus.ForwardMsgID(item.DedupeKey, subject), window, storedOnly)
			if err == nil {
				if storedOnly {
					// Stored, not confirmed: the route's confirmation record is not written,
					// so a later send of the same key stores its own copy for this session
					// rather than reading a receipt that never came.
					d.delivery.fanoutUnresponsiveSkips.Inc()
					d.delivery.logger.DeliveryLog(slog.LevelInfo, "listener publish topic stored for an unresponsive session", sessionID, item.Topic, item.EventID, sendUnresponsive.deliveryStatus(),
						slog.String("reason", string(sendUnresponsive)), slog.Int64("last_seen", entry.UpdatedAt))
					return sendUnresponsive
				}
				d.confirm(item.DedupeKey, sessionID)
				return sendDelivered
			}
			if forwardRefused(err) {
				outcome := sendDeliveryFailed
				d.delivery.logger.DeliveryLog(slog.LevelWarn, "listener publish topic forward refused", sessionID, item.Topic, item.EventID, outcome.deliveryStatus(),
					slog.String("reason", string(outcome)), slog.String("error", err.Error()))
				return outcome
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
// moment. It still publishes the envelope on the topic, for any subscriber the registry does not
// know; then it forwards the same envelope, topic, event_id and dedupe_key unchanged, to the
// agent subject of every recipient (topicRecipients) in parallel and answers once each confirmed,
// or 504 not_confirmed naming each that did not once publishDeliveryWindow from arrived has
// passed. A recipient that took both copies drops the second by its event_id. A recipient past
// the live window is forwarded to like the rest, so the stream keeps the message for it, and is
// asked for no receipt: it holds no answer open and fails none, and both the 200 and the 504 name
// it under unresponsive, so the sender learns the message was kept rather than taken.
//
// A send whose own publish on the topic did not reach NATS answers 504 publish_failed, whether the
// window ended first (the publish's reconnect wait used it up) or the publish failed at once on a
// connection that is down, so its status does not depend on how soon the failure came; nothing was
// forwarded, and with no recipient the send is answered 200 only once the topic was published. A
// publish NATS refuses however often it is sent (forwardRefused) is the sender's input or the
// grant's, answered as requestRecord.refuseBusUnavailable maps it. Every answer goes through
// record, the route's one line per request, under the outcome it answers.
func sendToTopic(ctx context.Context, w http.ResponseWriter, record *requestRecord, d *listenerDeps, item contracts.Envelope, arrived time.Time) {
	recipients := topicRecipients(d, item)
	settle := func(outcome sendOutcome, delivered []string, undelivered []undeliveredRecipient, attrs ...slog.Attr) {
		switch outcome {
		case sendPublishFailed, sendFailed:
			// The send forwarded nothing, so each recipient it did not reach counts failed, as
			// not_confirmed counts each recipient that did not confirm; a send with no recipient
			// counts nothing. Every other outcome's recipients are counted as each one ended.
			for range recipients {
				d.delivery.messagesDelivered.Inc([2]string{"delivery_status", outcome.deliveryStatus()})
			}
		}
		level := slog.LevelInfo
		if outcome != sendDelivered {
			level = slog.LevelWarn
		}
		attrs = append([]slog.Attr{
			slog.String("reason", string(outcome)),
			slog.String("dedupe_key", item.DedupeKey),
			slog.Any("delivered", delivered),
			slog.Any("undelivered", undelivered),
			slog.String("elapsed", time.Since(arrived).String()),
		}, attrs...)
		d.delivery.logger.DeliveryLog(level, "listener publish topic send", "", item.Topic, item.EventID, outcome.deliveryStatus(), attrs...)
	}
	// The sender stamp's read takes no more of the window than senderStampReadTimeout, and a read
	// that does not answer only leaves the stamp short: the publish below meets the same bus.
	item.Sender = senderStamp(ctx, d.registry, d.sessions, item.SourceSession)
	// The topic's own core publish waits for a reconnecting connection as long as the send's window
	// lasts and no longer, so a hub that is down is answered inside the window, never after the
	// bus's own reconnect wait, which outlasts envoy-client's 5 s attempt.
	if err := d.client.PublishCoreContext(ctx, item.Topic, item); err != nil {
		if forwardRefused(err) {
			record.refuseBusUnavailable(w, err)
			settle(sendFailed, nil, nil, slog.String("error", err.Error()))
			return
		}
		record.refuseWith(w, http.StatusGatewayTimeout, outcomePublishFailed, topicPublishFailure{
			Error:     fmt.Sprintf("%s was not published within %s: %v; no session was sent it", item.Topic, publishDeliveryWindow, err),
			Reason:    sendPublishFailed,
			EventID:   item.EventID,
			DedupeKey: item.DedupeKey,
		})
		settle(sendPublishFailed, nil, nil, slog.String("error", err.Error()))
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
	var unresponsive []string
	for i, outcome := range outcomes {
		d.delivery.messagesDelivered.Inc([2]string{"delivery_status", outcome.deliveryStatus()})
		switch outcome {
		case sendDelivered, sendDedupe:
			delivered = append(delivered, recipients[i])
		case sendUnresponsive:
			// Stored for, not confirmed by: neither delivered nor undelivered, so the send is
			// not held open and not answered 504 for a session that stopped answering while
			// its message waits in the stream for it. The answer names it all the same, so the
			// sender knows the message is kept for that session rather than taken by it.
			unresponsive = append(unresponsive, recipients[i])
		default:
			undelivered = append(undelivered, undeliveredRecipient{SessionID: recipients[i], Reason: outcome})
		}
	}
	if len(undelivered) > 0 {
		// The error names each session, since a client that shows only the error (envoy-client's
		// EnvoyApiError) must still tell its agent which sessions did not take the message.
		named := make([]string, len(undelivered))
		for i, recipient := range undelivered {
			named[i] = fmt.Sprintf("%s (%s)", recipient.SessionID, recipient.Reason)
		}
		record.refuseWith(w, http.StatusGatewayTimeout, outcomeNotConfirmed, topicSendFailure{
			Error: fmt.Sprintf("%d of %d sessions subscribed to %s did not confirm event %s within %s: %s",
				len(undelivered), len(recipients), item.Topic, item.EventID, publishDeliveryWindow, strings.Join(named, ", ")),
			Reason:       sendNotConfirmed,
			EventID:      item.EventID,
			DedupeKey:    item.DedupeKey,
			Delivered:    delivered,
			Undelivered:  undelivered,
			Unresponsive: unresponsive,
		})
		settle(sendNotConfirmed, delivered, undelivered, slog.Any("unresponsive", unresponsive))
		return
	}
	record.accept(w, publishResponse{Envelope: item, Unresponsive: unresponsive})
	settle(sendDelivered, delivered, undelivered, slog.Any("unresponsive", unresponsive))
}
