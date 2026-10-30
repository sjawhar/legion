// The listener's record of one inbound send or publish: the fixed outcome vocabulary and the
// requestRecord through which both handlers write each answer and its one log line together.

package main

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/logging"
)

// requestOutcome is what became of one inbound send or publish, in the fixed vocabulary those two
// records are read with: an operator's log filter matches these strings, so a route gets one word
// per answer and never a second spelling of the same one.
type requestOutcome string

const (
	outcomeAccepted       requestOutcome = "accepted"
	outcomeInvalidRequest requestOutcome = "invalid_request"
	outcomeTooLarge       requestOutcome = "too_large"
	outcomeBusUnavailable requestOutcome = "bus_unavailable"
	// outcomeNoLiveSession belongs to the send route alone: the store said the target's key is
	// gone. outcomeCapabilityRefused is the send route's target, or the publish route's role
	// holder (sendToRole), that is live but will not take this delivery mode.
	outcomeNoLiveSession     requestOutcome = "no_live_session"
	outcomeCapabilityRefused requestOutcome = "capability_refused"
	// outcomeRoleUnclaimed and outcomeRoleLapsed belong to the publish route alone. Both answer
	// 404 and neither is outcomeNoLiveSession: the request named a role, not a session, and the
	// two say different things about the claim behind it.
	outcomeRoleUnclaimed requestOutcome = "role_unclaimed"
	outcomeRoleLapsed    requestOutcome = "role_lapsed"
	// outcomeRoleHolderChanged is the publish route's lost race for a role's claim
	// (errRoleHolderChanged): every bus read answered, so it is deliberately not
	// outcomeBusUnavailable, which keeps its meaning of a bus that did not answer.
	outcomeRoleHolderChanged requestOutcome = "role_holder_changed"
	// outcomeRoleClaimUnreadable is the publish route's role claim record that the bus answered
	// and no read can decode (store.ErrMalformedRoleClaim): a 500, not outcomeBusUnavailable,
	// since the bus did answer and retrying reads the same record.
	outcomeRoleClaimUnreadable requestOutcome = "role_claim_unreadable"
	// outcomePublishDenied is a publish the server refused under this listener's own publish
	// permissions (bus.ErrPublishDenied). Only refuseBusUnavailable records it, from the 403 that
	// writeBusUnavailable writes; the replay route answers the same error as 503
	// REPLAY_UNAVAILABLE and records nothing. It is not outcomeBusUnavailable: the bus answered,
	// with a denial no retry can get past.
	outcomePublishDenied requestOutcome = "publish_denied"
	// outcomeReceiptTimeout and outcomeDeliveryFailed are the publish route's role send answered
	// 504 (confirmed.go), each under the reason its body names: a holder's receipt that did not
	// arrive after a forward reached the server, or a send no forward was shown to reach.
	outcomeReceiptTimeout requestOutcome = "receipt_timeout"
	outcomeDeliveryFailed requestOutcome = "delivery_failed"
	// outcomeStreamFull is a send or publish the stream refused because it holds its byte limit
	// (bus.ErrStreamFull). It is not outcomeBusUnavailable: the bus answered, that the stream is
	// full, so an operator reading the record looks at the stream's size, not for an outage.
	outcomeStreamFull requestOutcome = "stream_full"
)

// requestRecord is this listener's record of one inbound send or publish, and the only way either
// handler answers one. Until it existed the log held what arrived from the bus and nothing of what
// was asked of this listener, so a send that timed out left no server-side trace at all and "never
// attempted", "attempted and lost" and "attempted and refused" could not be told apart afterwards.
// A request refused before it reaches the bus is the one message no other system holds -- nothing
// was published, so no "listener received" line (delivery.go) and no stream entry will ever name
// it -- which is why the record carries the same short payload summary that arrival line carries:
// the message stays identifiable from this one line.
//
// Every return path of sendHandler and publishHandler answers through one of the methods below,
// which write the response and the record together. A status answered with no record, or a record
// whose outcome has drifted from the status actually written, is therefore not expressible.
//
// Two refusals are deliberately outside this record, because neither reaches a handler: a request
// the API auth wrapper refuses with 401 before the route is entered (apiauth.go), and a caller the
// starting gate turns away while the routes are still opening, which writes its own "request
// refused while starting" line (main.go).
type requestRecord struct {
	logger *logging.Logger
	// line is the record's message, "listener send" or "listener publish", and laneKey the name
	// of the destination field that route is addressed by, "target_session" or "topic". lane is
	// that destination, empty until the body has been decoded far enough to know it.
	line    string
	laneKey string
	lane    string
	// item is the envelope the handler built, taken by pointer rather than copied, and nil on a
	// request refused before the envelope exists (an undecodable body): that request still gets
	// its line, with the envelope's four fields empty rather than no line at all.
	item   *contracts.Envelope
	holder string
}

// accept writes the handler's 200 and records the request as accepted.
func (rec *requestRecord) accept(w http.ResponseWriter, value interface{}) {
	writeJSON(w, http.StatusOK, value)
	rec.write(http.StatusOK, outcomeAccepted)
}

// refuse writes exactly the named refusal writeJSONError writes, and records it under outcome.
func (rec *requestRecord) refuse(w http.ResponseWriter, status int, outcome requestOutcome, message string, expected ...string) {
	writeJSONError(w, status, message, expected...)
	rec.write(status, outcome)
}

// refuseWith writes a refusal whose body says more than writeJSONError's - a confirmed send's 403
// naming the holder or its 504 naming who did not confirm (confirmed.go) - and records it under
// outcome.
func (rec *requestRecord) refuseWith(w http.ResponseWriter, status int, outcome requestOutcome, body interface{}) {
	writeJSON(w, status, body)
	rec.write(status, outcome)
}

// refuseBusUnavailable answers a bus that did not serve the request and records the outcome the
// status it wrote implies: 413 is the caller's own oversized message, 400 the subject or key NATS
// will not take however often it is sent, 403 the publish this listener's own grant denies, and
// anything else the bus not answering. The status is read back from the write rather than
// classified a second time here, so the pair cannot drift, with one exception: a full stream
// (bus.ErrStreamFull) shares its 503 with the bus not answering, so the error tells it apart.
func (rec *requestRecord) refuseBusUnavailable(w http.ResponseWriter, err error) {
	status := writeBusUnavailable(w, err)
	outcome := outcomeBusUnavailable
	switch {
	case errors.Is(err, bus.ErrStreamFull):
		outcome = outcomeStreamFull
	case status == http.StatusRequestEntityTooLarge:
		outcome = outcomeTooLarge
	case status == http.StatusBadRequest:
		outcome = outcomeInvalidRequest
	case status == http.StatusForbidden:
		outcome = outcomePublishDenied
	}
	rec.write(status, outcome)
}

// refuseRoleHolderChanged answers a publish whose role claim moved under the resolution
// (errRoleHolderChanged) with the 503, Retry-After and body writeRoleHolderChanged writes for both
// routes that resolve a role, and records it under its own outcome rather than bus_unavailable:
// every bus read answered, so naming it bus_unavailable would send an operator after an outage
// that never happened, and the route's record is where "never attempted" is told from "attempted
// and refused" afterwards.
func (rec *requestRecord) refuseRoleHolderChanged(w http.ResponseWriter, role string) {
	rec.write(writeRoleHolderChanged(w, role, "publish"), outcomeRoleHolderChanged)
}

// refuseRoleClaimUnreadable answers a publish whose role claim record cannot be decoded
// (store.ErrMalformedRoleClaim) with the 500 writeRoleClaimUnreadable writes for both routes that
// resolve a role, and records it under its own outcome rather than bus_unavailable: the bus
// answered, so naming it bus_unavailable would send an operator after an outage that never
// happened instead of after the record.
func (rec *requestRecord) refuseRoleClaimUnreadable(w http.ResponseWriter, role string, err error) {
	rec.write(writeRoleClaimUnreadable(w, role, err), outcomeRoleClaimUnreadable)
}

// refuseRoleHolder answers the publish route's two role-holder refusals. A lapsed holder is the
// session the claim named, which the answer's own body names as well, so the record carries it;
// an unclaimed role has no holder to carry.
func (rec *requestRecord) refuseRoleHolder(w http.ResponseWriter, role string, result roleHolderResult) {
	writeRoleHolderError(w, role, result)
	outcome := outcomeRoleUnclaimed
	if result.state == roleHolderLapsed {
		outcome = outcomeRoleLapsed
	}
	rec.holder = result.holder
	rec.write(http.StatusNotFound, outcome)
}

// write emits the one line, INFO for a request this listener took and WARN for one it refused.
func (rec *requestRecord) write(status int, outcome requestOutcome) {
	var eventID, source, sourceSession, payloadSummary string
	if rec.item != nil {
		eventID, source = rec.item.EventID, rec.item.Source
		sourceSession, payloadSummary = rec.item.SourceSession, rec.item.PayloadSummary
	}
	attrs := make([]slog.Attr, 0, 8)
	attrs = append(attrs,
		slog.String(rec.laneKey, rec.lane),
		slog.String("event_id", eventID),
		slog.String("source", source),
		slog.String("source_session", sourceSession),
		slog.String("payload_summary", payloadSummary),
		slog.String("outcome", string(outcome)),
		slog.Int("status", status))
	if rec.holder != "" {
		attrs = append(attrs, slog.String("holder", rec.holder))
	}
	level := slog.LevelWarn
	if outcome == outcomeAccepted {
		level = slog.LevelInfo
	}
	rec.logger.LogEvent(level, rec.line, attrs...)
}
