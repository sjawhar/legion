package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
// write timeout, and leaves room for two forwards of roleForwardTimeout, so a forward the stream
// did not acknowledge storing is made again.
const publishDeliveryWindow = 4 * time.Second

// publishRetryPause is the wait between one failed attempt of a confirmed send and the next, so a
// recipient whose connection is down is not sent a burst of forwards that reach nobody.
const publishRetryPause = 100 * time.Millisecond

// roleSendFailure is the 504 of a role send whose window ended with no forward the stream stored
// for its holder. Reason is delivery_failed: no live holder to forward to, a read that failed or
// ran out of time, or a forward the stream did not acknowledge storing. Holder is the holder the
// send last resolved, empty when none was; Attempts counts the forwards it made, every one the
// same envelope under one event_id.
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

// sendOutcome is how a confirmed send ended, or how its recipient did. An
// answer that carries a reason carries the outcome itself, which marshals as its string. A send's
// log line names the outcome as its reason attribute and deliveryStatus's label for it as its
// delivery_status attribute, so the reason reads as the answer does and delivery_status reads as
// the role lane's own lines and the messagesDelivered count do.
type sendOutcome string

const (
	// sendDelivered is a send the stream stored for its recipient, which reads it from its own
	// session consumer.
	sendDelivered sendOutcome = "delivered"
	// sendDedupe is a recipient this route had already stored this dedupe key for, so it was not
	// sent the message again.
	sendDedupe sendOutcome = "dedupe"
	// sendSkipped is a role send from the role's own holder, the role lane's echo skip.
	sendSkipped sendOutcome = "skipped"
	// sendUnclaimed and sendHolderLapsed are a role send with no live claim, answered 404.
	sendUnclaimed    sendOutcome = sendOutcome(roleHolderUnclaimed)
	sendHolderLapsed sendOutcome = sendOutcome(roleHolderLapsed)
	// sendDeliveryFailed is a recipient the stream stored no forward for: none live to forward to,
	// a read that failed or ran out of time, or a forward it did not acknowledge storing.
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
)

// deliveryStatus is the label the role lane gives the same outcome, which the route counts under
// messagesDelivered and logs as delivery_status, so the route's counts and lines read as the
// lane's: an unclaimed role is skipped, a kept message reads as the lane's own kept, and every
// outcome that is neither taken, skipped nor kept is failed.
func (o sendOutcome) deliveryStatus() string {
	switch o {
	case sendDelivered, sendDedupe, sendSkipped, sendKept:
		return string(o)
	case sendUnclaimed:
		return string(sendSkipped)
	default:
		return string(sendFailed)
	}
}

// forwardWindow is one forward's wait for the stream's acknowledgement that it stored it:
// roleForwardTimeout, or what is left of the send when that is less.
func forwardWindow(ctx context.Context) time.Duration {
	deadline, _ := ctx.Deadline()
	return min(roleForwardTimeout, time.Until(deadline))
}

// pauseBeforeRetry waits publishRetryPause, or until ctx ends, and reports whether the send has time
// left for another attempt.
func pauseBeforeRetry(ctx context.Context) bool {
	return waitUnlessDone(ctx, publishRetryPause) == nil && ctx.Err() == nil
}

// forwardAndWait forwards item to subject, under msgID as its Nats-Msg-Id when that is not empty,
// through the delivery configuration's forwardRole and waits for its outcome, which is the form a
// caller that answers for the forward needs: the publish route answers its sender with what the
// forward did, and the kept lane's drain moves its cursor past the copy (walkKeptRole), where the
// role lane hands the wait over and reports it later. The outcome is the stream's acknowledgement
// that it stored the forward, nothing more: the holder answers no receipt and reads the stored
// copy from its session consumer (startRoleForward).
func (cfg listenerDeliveryHandlerConfig) forwardAndWait(subject string, item contracts.Envelope, msgID string, window time.Duration) error {
	wait, err := cfg.forwardRole(subject, item, msgID, window)
	if err != nil {
		return err
	}
	return wait()
}

// forwardRefused reports whether a forward failed in a way another attempt repeats: NATS refuses
// the envelope or subject however often it is sent (bus.ErrRefused), or the listener's grant does
// not allow the subject (bus.ErrPublishDenied). A role send answers it at once, as
// requestRecord.refuseBusUnavailable maps it (413 or 400 for a refusal, 403 for a denial).
func forwardRefused(err error) bool {
	return errors.Is(err, bus.ErrRefused) || errors.Is(err, bus.ErrPublishDenied)
}

// confirm records that the stream stored the message under dedupeKey for sessionID through the
// route. The route's own record (listenerDeps.confirmed) is what a later send of the same key
// reads, so envoy-client's retry of a send skips a recipient the first attempt stored it for; the
// delivery handlers' cache is told too, so they do not deliver the same key to it again.
func (d *listenerDeps) confirm(dedupeKey, sessionID string) {
	d.confirmed.Record(dedupeKey, sessionID)
	d.delivery.dedupeCache.Record(dedupeKey, sessionID)
}

// roleSend is one role message through the publish route. The route does not publish on the role
// lane: it forwards the envelope to the holder's agent subject itself and answers 200 once the
// stream stored that forward for the holder's session consumer, or with the failure once
// publishDeliveryWindow from arrived has passed, so its sender learns whether the message was kept
// for its holder. No holder answers a receipt (startRoleForward), so the 200 is the stream's word
// rather than the holder's. It publishes no delivery exception: the sender has the answer. Every
// answer goes through record, the route's one line per request (request_record.go), and is then
// settled: counted, and logged on the send's own delivery line.
type roleSend struct {
	w      http.ResponseWriter
	record *requestRecord
	d      *listenerDeps
	item   contracts.Envelope
	role   string
	// keep is the request's keep field as the route read it, true when it set none: whether
	// this send's give-up keeps the message for the role's next holder (noHolder). It is the
	// send's own rather than the envelope's, so no copy the listener forwards and no answer it
	// writes carries it.
	keep     bool
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

// delivered answers 200 with the envelope and the holder it was stored for.
func (s *roleSend) delivered(outcome sendOutcome) {
	s.record.holder = s.holder
	s.record.accept(s.w, publishResponse{Envelope: s.item, Holder: s.holder})
	s.settle(outcome)
}

// noHolder answers a role with no live claim. For a state the listener keeps for (roleKeepable: an
// unclaimed role, and a lapsed claim released or already absent), and while this send's own keep
// flag asks for it, it keeps the message for the role's next holder and hands the lane to whoever
// holds the role now, both in the one keep both give-up paths take (keepForNextHolder), and
// answers kept's 202. A keep that kept nothing - one this send's keep flag turned off, one under
// a role token no claim can name, a publish that failed, logged by keepRoleMessage - is answered
// as a send that kept nothing is, since the sender's own fallback must run when the message was
// not kept: writeRoleHolderError's 404 (unclaimed or holder_lapsed), which the Legion daemon
// reads as notify.ErrNoHolder, recorded role_unclaimed or role_lapsed. The role lane counts an
// unclaimed role skipped and a lapsed holder failed.
//
// It is also the one place a message to a Legion role of a released issue pod is seen at all, so
// it raises the wake report beside the answer, before either answer and waiting for neither
// (reportRoleWake). Both answers need it: a message kept for the role's next holder waits until
// something claims the role again, and a publish with keep: false keeps nothing and is still
// answered 404, so only the wake brings the released pod back to claim the role, drain what was
// kept for it (drainKeptRole) and take the sender's next attempt. The report names the role and
// nothing else; which daemon owns it, and whether any claim of that role is released, is for the
// daemons to decide.
func (s *roleSend) noHolder(result roleHolderResult) {
	reportRoleWake(s.d, s.d.delivery.logger, s.role)
	if s.keep && roleKeepable(result) && keepForNextHolder(*s.d.delivery, s.role, s.item) {
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

// timedOut answers the 504 of a send whose window ended with no forward the stream stored for its
// holder, recorded delivery_failed.
func (s *roleSend) timedOut() {
	s.record.holder = s.holder
	s.record.refuseWith(s.w, http.StatusGatewayTimeout, outcomeDeliveryFailed, roleSendFailure{
		Error: fmt.Sprintf("role %s: event %s was not stored for its holder %s within %s (%s)",
			s.role, s.item.EventID, cmp.Or(s.holder, "(none)"), publishDeliveryWindow, sendDeliveryFailed),
		Reason:    sendDeliveryFailed,
		EventID:   s.item.EventID,
		DedupeKey: s.item.DedupeKey,
		Holder:    s.holder,
		Attempts:  s.attempts,
	})
	s.settle(sendDeliveryFailed)
}

// settledBeforeForward judges the message to a holder the send has not checked yet, whose session
// entry is entry, by beforeForward, the rules the role lane applies before it forwards, and reports
// whether it answered: an echo (the source session holds the role) or a holder this route already
// stored this dedupe key for, both answered 200 with nothing forwarded; a holder that cannot take
// the payload's delivery mode, answered 403 with the refusal sendHandler gives,
// recorded capability_refused as sendHandler records it.
//
// The dedupe check reads the route's own record (listenerDeps.confirmed), not the delivery
// handlers' cache: agentSubjectDelivery and fanoutDelivery record a key there for every delivery
// they make or hand over, so a role send under a key one of them saw would be answered 200 for a
// holder the route never forwarded it to.
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
// first resolution reads the holder as an HTTP lookup does (resolveRoleHolder with
// roles.Registered), within sendRequestBudget of arrived as GET /v1/roles/<role> reads it, and
// gives its answers: a role with no live claim is kept and answered 202 while keep asks for it -
// the request's own keep field, true when it set none - for the states the listener keeps for
// (roleKeepable), else the lookup's 404 unclaimed or holder_lapsed (noHolder), and a resolution
// that failed the lookup's 503 or 500 (unresolved), so a holder that exists but has not
// heartbeated lately gets attempts rather than a 404. The first attempt forwards to that holder
// when its session entry passes role delivery's freshness test (laneHolderFresh), without reading
// the claim again; every other attempt resolves the holder again by role delivery's rule
// (resolveRoleHolder with laneHolderFresh), so a holder that relaunched under a new session
// between attempts gets the next one. On those attempts an unclaimed role, or a lapsed holder
// whose claim was released or already absent, is answered as the first resolution answers it; a
// lapsed holder whose claim was retained, a read that failed and a claim that churned fail that
// attempt only. Each attempt forwards the same envelope, under the role forward's dedupe key and
// under one Nats-Msg-Id, the key and the holder's subject (bus.ForwardMsgID), and waits what is
// left of the send, at most roleForwardTimeout, for the stream's acknowledgement that it stored
// it; that acknowledgement is the send's 200, since the holder answers nothing and reads the
// stored copy from its session consumer (startRoleForward). The stream, which captures agent
// subjects, therefore keeps one copy of the forward however many attempts there are. Every read
// takes ctx, the send's window, so none outlasts it: a send whose window ends is answered 504.
func sendToRole(ctx context.Context, w http.ResponseWriter, record *requestRecord, d *listenerDeps, item contracts.Envelope, role string, keep bool, arrived time.Time) {
	send := &roleSend{w: w, record: record, d: d, item: item, role: role, keep: keep, arrived: arrived}
	// The sender is stamped before the holder is resolved, so the copy a keep stores carries the
	// sender's title, roles and directory exactly as the forward of a live delivery does: the
	// envelope is kept as it is here (keepRoleMessage), and the holder that is handed it later
	// reads the same stamp this send's own 202 names.
	send.item.Sender = senderStamp(ctx, d.registry, d.sessions, item.SourceSession)
	firstCtx, cancelFirst := context.WithDeadline(ctx, arrived.Add(sendRequestBudget))
	first, err := resolveRoleHolder(firstCtx, d.registry, d.sessions, role, roles.Registered)
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
			window := forwardWindow(ctx)
			if window <= 0 {
				send.timedOut()
				return
			}
			send.attempts++
			subject := contracts.AgentSubject(send.holder)
			// The forward carries the same Nats-Msg-Id on every attempt, so the stream, which
			// captures agent subjects, keeps one copy of it per message and recipient however
			// many attempts this send makes.
			err := d.delivery.forwardAndWait(subject, forwarded, bus.ForwardMsgID(forwarded.DedupeKey, subject), window)
			if err == nil {
				d.confirm(item.DedupeKey, send.holder)
				send.delivered(sendDelivered)
				return
			}
			if forwardRefused(err) {
				send.refused(err)
				return
			}
		}
		// Any resolution not answered above (a holder that has not heartbeated within
		// session.ClaimStaleAfter and keeps its claim, a read that failed, a claim that churned)
		// fails this attempt only.
		if !pauseBeforeRetry(ctx) {
			break
		}
	}
	send.timedOut()
}
