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
// list is [] rather than null.
type topicSendFailure struct {
	Error       string                 `json:"error"`
	Reason      sendOutcome            `json:"reason"`
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
	sendUnclaimed    sendOutcome = sendOutcome(roleHolderUnclaimed)
	sendHolderLapsed sendOutcome = sendOutcome(roleHolderLapsed)
	// sendReceiptTimeout is a recipient whose last forward reached the server and drew no receipt.
	sendReceiptTimeout sendOutcome = "receipt_timeout"
	// sendDeliveryFailed is a recipient that no forward was shown to reach: none live to forward
	// to, a read that failed or ran out of time, or a forward not shown to have reached the server.
	sendDeliveryFailed sendOutcome = "delivery_failed"
	// sendCapability is a role holder that cannot take the payload's delivery mode, answered 403.
	sendCapability sendOutcome = "capability"
	// sendKept is a role send with no live holder whose message the listener kept on the role's
	// kept lane instead of refusing it, answered 202 (roleSend.kept).
	sendKept sendOutcome = sendOutcome(roleKeptDeliveryStatus)
	// sendFailed is a send refused as requestRecord.refuseBusUnavailable maps it (a forward or
	// publish NATS refuses or the grant denies), or a role send whose first resolution of the
	// holder failed, answered as GET /v1/roles/<role> answers it.
	sendFailed sendOutcome = "failed"
	// sendNotConfirmed is an ad-hoc topic send that published and that some recipient did not
	// confirm.
	sendNotConfirmed sendOutcome = "not_confirmed"
	// sendPublishFailed is an ad-hoc topic send whose own publish did not reach NATS.
	sendPublishFailed sendOutcome = "publish_failed"
)

// deliveryStatus is the label the role lane gives the same outcome, which the route counts under
// messagesDelivered and logs as delivery_status, so the route's counts and lines read as the
// lane's: an unclaimed role is skipped, a kept message reads as the lane's own kept, and every
// outcome that is neither taken, skipped, kept nor a receipt timeout is failed.
func (o sendOutcome) deliveryStatus() string {
	switch o {
	case sendDelivered, sendDedupe, sendSkipped, sendReceiptTimeout, sendKept:
		return string(o)
	case sendUnclaimed:
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

// forwardAndWait forwards item to subject through the delivery configuration's forwardRole and
// waits for its outcome, which is the form a caller that answers for the forward needs: the
// publish route answers its sender with what the forward did, and the kept lane's drain moves its
// cursor past the copy (walkKeptRole), where the role lane hands the wait over and
// reports it later. storedOnly asks for a forward that counts as delivered once the stream
// acknowledges storing it, with no receipt (roleForwardStoredOnly): a holder served on a port has
// no NATS subscriber to answer one, and takes the stored copy through its listener instead.
func (cfg listenerDeliveryHandlerConfig) forwardAndWait(subject string, item contracts.Envelope, window time.Duration, storedOnly bool) error {
	wait, err := cfg.forwardRole(subject, item, window, storedOnly)
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
// delivery handlers' cache is told too, so they do not push the same key to it again.
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
	w        http.ResponseWriter
	record   *requestRecord
	d        *listenerDeps
	item     contracts.Envelope
	role     string
	arrived  time.Time
	holder   string
	attempts int
}

// settle counts and logs the send's outcome, the one delivery line a send writes, once its answer
// is written. It counts and logs as delivery_status the label the role lane gives the same outcome,
// and logs the outcome itself as reason.
func (s *roleSend) settle(outcome sendOutcome) {
	s.d.delivery.messagesDelivered.Inc([2]string{"delivery_status", outcome.deliveryStatus()})
	level := slog.LevelWarn
	switch outcome {
	case sendDelivered, sendSkipped, sendDedupe, sendUnclaimed, sendKept:
		level = slog.LevelInfo
	}
	s.d.delivery.logger.DeliveryLog(level, "listener publish role send", s.holder, s.item.Topic, s.item.EventID, outcome.deliveryStatus(),
		slog.String("reason", string(outcome)),
		slog.String("dedupe_key", s.item.DedupeKey),
		slog.Int("attempts", s.attempts),
		slog.String("elapsed", time.Since(s.arrived).String()))
}

// delivered answers 200 with the envelope and the holder that took it.
func (s *roleSend) delivered(outcome sendOutcome) {
	s.record.holder = s.holder
	s.record.accept(s.w, publishResponse{Envelope: s.item, Holder: s.holder})
	s.settle(outcome)
}

// noHolder answers a role with no live claim. For a state the listener keeps for (roleKeepable: an
// unclaimed role, and a lapsed claim released or already absent), it keeps the message for the
// role's next holder and hands the lane to whoever holds the role now, both in the one keep both
// give-up paths take (keepForNextHolder), and answers kept's 202. A keep that kept nothing - one
// the envelope's own keep flag turned off, one under a role token no claim can name, a publish
// that failed, logged by keepRoleMessage - is answered as a send that kept nothing is, since the
// sender's own fallback must run when the message was not kept: writeRoleHolderError's 404
// (unclaimed or holder_lapsed), which the Legion daemon reads as notify.ErrNoHolder, recorded
// role_unclaimed or role_lapsed. The role lane counts an unclaimed role skipped and a lapsed
// holder failed.
func (s *roleSend) noHolder(result roleHolderResult) {
	if roleKeepable(result) && keepForNextHolder(*s.d.delivery, s.role, s.item) {
		s.kept(result)
		return
	}
	s.record.refuseRoleHolder(s.w, s.role, result)
	outcome := sendHolderLapsed
	if result.state == roleHolderUnclaimed {
		outcome = sendUnclaimed
	}
	s.settle(outcome)
}

// kept answers 202 with the envelope the listener kept on the role's kept lane: kept is true,
// kept_reason names which case it was (unclaimed or holder_lapsed) and holder names the session
// the lapsed claim pointed at, when one was read. The message reaches the role's next holder when
// it claims the role (drainKeptRole), so this is an answer rather than a refusal, and it settles
// under its own outcome so the send still writes exactly one delivery line.
//
// 202 rather than 200, so a sender tells the three answers of a role send apart by status alone:
// 200, the message is with a live holder; 202, nobody holds the role and the message waits for
// whoever claims it within roleKeepWindow; 404, nothing was kept and the sender's own fallback
// must run. INFRA-514's architect decided it on 2026-10-31.
func (s *roleSend) kept(result roleHolderResult) {
	if result.holder != "" {
		s.holder = result.holder
	}
	s.record.holder = s.holder
	s.record.acceptWith(s.w, http.StatusAccepted, publishResponse{Envelope: s.item, Holder: s.holder, Kept: true, KeptReason: result.state})
	s.settle(sendKept)
}

// unresolved answers a first resolution of the holder that did not resolve, as GET
// /v1/roles/<role> answers it: a claim that moved under the resolution (errRoleHolderChanged) is
// the 503 that names the race, a claim record no read can decode (store.ErrMalformedRoleClaim) the
// 500 that names it, and any other failure - a read that did not answer within its cap or
// sendRequestBudget among them - the bus-unavailable 503, or the 413 or 400 a refused key gets.
// Each is recorded under its own outcome.
func (s *roleSend) unresolved(err error) {
	switch {
	case errors.Is(err, errRoleHolderChanged):
		s.record.refuseRoleHolderChanged(s.w, s.role)
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
// session whose push they skipped, having heard nothing from it, so a role send under a key one of
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
// first resolution reads the holder as an HTTP lookup does (resolveRoleHolder with registered),
// within sendRequestBudget of arrived as GET /v1/roles/<role> reads it, and gives its answers: a
// role with no live claim is kept and answered 202 while the envelope asks to be kept, for the
// states the listener keeps for (roleKeepable), else the lookup's 404 unclaimed or holder_lapsed
// (noHolder), and a resolution that failed the lookup's 503 or 500 (unresolved), so a holder that
// exists but has not heartbeated lately gets attempts rather than a 404. The first
// attempt forwards to that holder when its session entry passes role delivery's freshness test
// (laneHolderFresh), without reading the claim again; every other attempt resolves the holder
// again by role delivery's rule (resolveRoleHolder with laneHolderFresh), so a holder that
// relaunched under a new session between attempts gets the next one. On those attempts an
// unclaimed role, or a lapsed holder whose claim was released or already absent, is answered as
// the first resolution answers it; a lapsed holder whose claim was retained, a read that failed and
// a claim that churned fail that attempt only. Each attempt forwards the same envelope, under the
// role forward's dedupe key, with a receipt window of what is left of the send, at most
// roleReceiptTimeout. Every forward carries the same Nats-Msg-Id (bus.Client.RequestCoreTo), so the
// stream, which captures agent subjects, keeps one copy however many attempts there are. Every read
// takes ctx, the send's window, so none outlasts it: a send whose window ends is answered 504.
func sendToRole(ctx context.Context, w http.ResponseWriter, record *requestRecord, d *listenerDeps, item contracts.Envelope, role string, arrived time.Time) {
	send := &roleSend{w: w, record: record, d: d, item: item, role: role, arrived: arrived}
	// The sender is stamped before the holder is resolved, so the copy a keep stores carries the
	// sender's title, roles and directory exactly as the forward of a live delivery does: the
	// envelope is kept as it is here (keepRoleMessage), and the holder that is handed it later
	// reads the same stamp this send's own 202 names.
	send.item.Sender = senderStamp(ctx, d.registry, d.sessions, item.SourceSession)
	firstCtx, cancelFirst := context.WithDeadline(ctx, arrived.Add(sendRequestBudget))
	first, err := resolveRoleHolder(firstCtx, d.registry, d.sessions, role, registered)
	cancelFirst()
	if err != nil {
		send.unresolved(err)
		return
	}
	if first.state != roleHolderLive {
		send.noHolder(first)
		return
	}
	send.holder = first.holder
	if send.settledBeforeForward(first.entry) {
		return
	}
	checked := send.holder
	forwarded := send.item
	forwarded.DedupeKey = roleForwardDedupePrefix + item.DedupeKey
	var lastForward error
	resolved := first
	for pass := 0; ; pass++ {
		if pass > 0 || !laneHolderFresh(first.entry) {
			resolved, err = resolveRoleHolder(ctx, d.registry, d.sessions, role, laneHolderFresh)
			if err != nil && ctx.Err() != nil {
				// The window ended under the resolution: what it read says nothing more.
				break
			}
		}
		if resolved.holder != "" {
			send.holder = resolved.holder
		}
		switch {
		case err != nil:
			// Retried below, as every resolution not answered here is.
		case resolved.state == roleHolderUnclaimed:
			send.noHolder(resolved)
			return
		case resolved.state == roleHolderLapsed && resolved.claimRelease != store.ExpiredRoleClaimRetained:
			send.noHolder(resolved)
			return
		case resolved.state == roleHolderLive:
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
			err := d.delivery.forwardAndWait(contracts.AgentSubject(send.holder), forwarded, window, roleForwardStoredOnly(resolved.entry))
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
		// Any resolution not answered above (a holder that has not heartbeated within
		// session.ClaimStaleAfter and keeps its claim, a read that failed, a claim that churned)
		// fails this attempt only.
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
			err := d.delivery.forwardAndWait(contracts.AgentSubject(sessionID), item, window, false)
			if err == nil {
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
// moment. It still publishes the envelope on the topic, for the serve-port sessions the live lane
// delivers to and any subscriber the registry does not know; then it forwards the same envelope,
// topic, event_id and dedupe_key unchanged, to the agent subject of every recipient
// (topicRecipients) in parallel and answers once each confirmed, or 504 not_confirmed naming each
// that did not once publishDeliveryWindow from arrived has passed. A recipient that took both
// copies drops the second by its event_id.
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
	for i, outcome := range outcomes {
		d.delivery.messagesDelivered.Inc([2]string{"delivery_status", outcome.deliveryStatus()})
		switch outcome {
		case sendDelivered, sendDedupe:
			delivered = append(delivered, recipients[i])
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
			Reason:      sendNotConfirmed,
			EventID:     item.EventID,
			DedupeKey:   item.DedupeKey,
			Delivered:   delivered,
			Undelivered: undelivered,
		})
		settle(sendNotConfirmed, delivered, undelivered)
		return
	}
	record.accept(w, publishResponse{Envelope: item})
	settle(sendDelivered, delivered, undelivered)
}
