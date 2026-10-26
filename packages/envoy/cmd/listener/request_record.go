// The listener's record of one inbound send or publish: the fixed outcome vocabulary and the
// requestRecord through which both handlers write each answer and its one log line together.

package main

import (
	"fmt"
	"log/slog"
	"net/http"

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
	// outcomeNoLiveSession and outcomeCapabilityRefused belong to the send route alone: the store
	// said the target's key is gone, or the target is live but will not take this delivery mode.
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
	// outcomePublishDenied is a publish the server refused under this listener's own publish
	// permissions (bus.ErrPublishDenied). Only the publish route can record it, and only for a
	// topic the stream does not capture: bus.ErrPublishDenied is made in one place,
	// confirmPublished (internal/bus/confirm.go), which only the core publishes (publishCoreTo,
	// RequestCoreTo) reach, and a publish takes the core path only when usesCoreTransport
	// (internal/bus/nats.go) finds its topic outside streamSubjects (internal/bus/stream.go) - a
	// role lane or an ad-hoc topic. The send route publishes contracts.AgentSubject(target),
	// which notifications.agent.> captures, so it goes through publishJetStream and is never
	// denied this way. The publish route does not record it as outcomeBusUnavailable: the bus
	// answered, with a denial no retry can get past.
	outcomePublishDenied requestOutcome = "publish_denied"
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

// refuseBusUnavailable answers a bus that did not serve the request and records the outcome the
// status it wrote implies: 413 is the caller's own oversized message, 400 the subject or key NATS
// will not take however often it is sent, 403 the publish this listener's own grant denies, and
// anything else the bus not answering. The status is read back from the write rather than
// classified a second time here, so the pair cannot drift.
func (rec *requestRecord) refuseBusUnavailable(w http.ResponseWriter, err error) {
	status := writeBusUnavailable(w, err)
	outcome := outcomeBusUnavailable
	switch status {
	case http.StatusRequestEntityTooLarge:
		outcome = outcomeTooLarge
	case http.StatusBadRequest:
		outcome = outcomeInvalidRequest
	case http.StatusForbidden:
		outcome = outcomePublishDenied
	}
	rec.write(status, outcome)
}

// refuseRoleHolderChanged answers a publish whose role claim moved under the resolution
// (errRoleHolderChanged). The 503 and the Retry-After are the same ask as a bus that did not
// answer - a fresh read wins this race as soon as the replacement claim settles - but the body
// and the record are not: every bus read answered, so naming it bus_unavailable would send an
// operator after an outage that never happened, and the route's record is where "never
// attempted" is told from "attempted and refused" afterwards.
func (rec *requestRecord) refuseRoleHolderChanged(w http.ResponseWriter, role string) {
	w.Header().Set("Retry-After", "1")
	rec.refuse(w, http.StatusServiceUnavailable, outcomeRoleHolderChanged, fmt.Sprintf(
		"envoy: the holder of role %s changed while this publish was resolving it", role,
	))
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
