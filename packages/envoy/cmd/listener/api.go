package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

const rolePatternString = `^[a-z0-9][a-z0-9_-]*$`

var rolePattern = regexp.MustCompile(rolePatternString)

type streamInfoLookup interface {
	StreamInfo(stream string, opts ...nats.JSOpt) (*nats.StreamInfo, error)
}

type apiError struct {
	Code     string   `json:"code,omitempty"`
	Error    string   `json:"error"`
	Expected []string `json:"expected,omitempty"`
}

type messageBody struct {
	Source         string  `json:"source"`
	SourceSession  string  `json:"source_session"`
	Message        string  `json:"message"`
	Payload        *string `json:"payload"`
	IdempotencyKey string  `json:"idempotency_key"`
	DedupeKey      string  `json:"dedupe_key"`
	InReplyTo      *string `json:"in_reply_to"`
	Supersedes     *string `json:"supersedes"`
	Urgency        *string `json:"urgency"`
	ExpectsReply   *string `json:"expects_reply"`
	ExpiresAt      *int64  `json:"expires_at"`
}

// sendResponse is the answer to a targeted send. Duplicate reports that JetStream already held
// this message, so the stream stored nothing new; the publish still reached the agent's subject.
// What recognises that repeat, and for how long, is stated on DELIVERY_DUPLICATE_WINDOW_MS in
// @legion/contracts (contracts.DeliveryDuplicateWindow here). It can only be true for a send whose
// dedupe key names the upstream event (contracts.DedupeKeyNamesTheUpstreamEvent), and Dispatch is
// the one sender that uses it. Absent means false, which is what an older listener's answer reads
// as.
type sendResponse struct {
	contracts.Envelope
	Recipient string `json:"recipient"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

type publishResponse struct {
	contracts.Envelope
	Holder string `json:"holder,omitempty"`
}

type subscribeResponse struct {
	store.Interest
	Warnings []string `json:"warnings,omitempty"`
	// Delivery is the session's delivery (deliveryAnswer), present whenever the registration
	// carried delivery or registered a serving port.
	Delivery *deliveryAnswer `json:"delivery,omitempty"`
}

const unwiredRepositoryWarningTimeout = 750 * time.Millisecond

func isValidRole(role string) bool {
	return rolePattern.MatchString(role)
}

// hasCapability reports whether capabilities includes value. The Dispatch
// server (packages/envoy/internal/dispatch/api) has its own copy of this
// same check; the two packages communicate over HTTP, not a shared Go
// import, so there is one definition per side of that boundary rather than
// one shared helper.
func hasCapability(capabilities []string, value string) bool {
	for _, capability := range capabilities {
		if capability == value {
			return true
		}
	}
	return false
}

// frameDeliveryMode reads the canonical delivery mode from a targeted Dispatch frame's
// payload the same way the receiving client reads it: an exact, case-sensitive "delivery" key
// at the top level and an exact, case-sensitive "mode" key inside it. The receiving client
// (packages/envoy-client/src/delivery.ts's parseDispatchFrame) gets there via JSON.parse plus
// plain object property access, which is exact and case-sensitive in JavaScript; the wire key
// is fixed lower-case ("delivery") in packages/contracts/src/dispatch-api.ts. Decoding straight
// into a Go struct would be a *second, more lenient* reader of the same bytes:
// encoding/json matches JSON object keys case-insensitively when no exact match exists, so a
// payload carrying both the receiver's exact "delivery" key and a same-key-different-case
// sibling like "Delivery" could make that lenient decode read the sibling's mode while the
// receiver executes the exact key's -- guard and receiver disagreeing about what is being
// sent. This function never resolves that disagreement; it refuses to guess.
//
// The return is a genuine tri-state, distinguished by type, not by string emptiness (an empty
// mode string can never again mean two different things):
//
//   - No "delivery" key at all (nil payload, unparseable JSON, or a parsed object missing the
//     exact key): mode="", err=nil. This is an ordinary, untagged send -- exactly like an
//     interest-topic publish or role forward -- and stays unguarded, because there is no
//     "delivery" key for the receiver to see either.
//   - "delivery" is present (the exact key exists) but its mode cannot be determined
//     unambiguously as a single non-empty string -- delivery is not an object, has no "mode"
//     key, "mode" is not a JSON string, "mode" is an empty string, or a same-key-different-
//     case sibling exists for "delivery" or "mode": mode="", err!=nil. The caller must refuse
//     the send outright. A malformed or unreadable targeted frame is never treated as if it
//     were untagged: presence of the exact "delivery" key is itself a claim that this send is
//     targeted, and an unreadable claim is refused, not waved through.
//   - "delivery" is present and its mode reads unambiguously as one of the three delivery
//     modes: mode=<that string>, err=nil.
//   - "delivery" is present and its mode reads unambiguously as something else: mode="",
//     err!=nil, refused like any other unreadable claim. Waving it through on the grounds that
//     the capability check behind it would refuse a mode no session advertises does not hold:
//     `capabilities` is an open list from the registry, so a session that advertises a
//     bogus string is exactly what makes that argument fail. The Dispatch server's
//     `validDelivery` already refuses the same set at its own boundary; this is the listener's.
func frameDeliveryMode(payload *string) (mode string, err error) {
	if payload == nil {
		return "", nil
	}
	var top map[string]json.RawMessage
	if unmarshalErr := json.Unmarshal([]byte(*payload), &top); unmarshalErr != nil {
		return "", nil
	}
	deliveryRaw, hasDelivery := top["delivery"]
	if !hasDelivery {
		return "", nil
	}
	if hasCaseVariantSibling(top, "delivery") {
		return "", fmt.Errorf("delivery key has an ambiguous case-variant sibling")
	}
	var delivery map[string]json.RawMessage
	if unmarshalErr := json.Unmarshal(deliveryRaw, &delivery); unmarshalErr != nil {
		return "", fmt.Errorf("delivery is present but not a JSON object: %w", unmarshalErr)
	}
	if hasCaseVariantSibling(delivery, "mode") {
		return "", fmt.Errorf("delivery.mode key has an ambiguous case-variant sibling")
	}
	modeRaw, hasMode := delivery["mode"]
	if !hasMode {
		return "", fmt.Errorf("delivery is present but has no mode")
	}
	if unmarshalErr := json.Unmarshal(modeRaw, &mode); unmarshalErr != nil {
		return "", fmt.Errorf("delivery.mode is present but not a JSON string: %w", unmarshalErr)
	}
	if mode == "" {
		return "", fmt.Errorf("delivery.mode is present but empty")
	}
	if !isDeliveryMode(mode) {
		return "", fmt.Errorf("delivery.mode %q is not a delivery mode", mode)
	}
	return mode, nil
}

// envelopeDeliveryMode is frameDeliveryMode of item's payload, none when it has none.
func envelopeDeliveryMode(item contracts.Envelope) (mode string, err error) {
	var payload *string
	if item.Payload != "" {
		payload = &item.Payload
	}
	return frameDeliveryMode(payload)
}

// isDeliveryMode reports whether value is one of the three targeted-delivery modes. The list is
// DELIVERY_CAPABILITIES in packages/contracts/src/dispatch-api.ts, spelled here as the Dispatch
// server spells it in its own `validDelivery`: the two sides talk over HTTP, not a shared Go
// import.
func isDeliveryMode(value string) bool {
	return value == "aside" || value == "btw" || value == "steer"
}

// hasCaseVariantSibling reports whether keys contains some key that case-insensitively, but
// not exactly, matches exact -- a same-field claim under a different spelling that an
// exact-match reader (this guard, or the JavaScript receiver's plain property access) would
// never see, but a case-insensitive one (Go's struct unmarshal, if used here) could.
func hasCaseVariantSibling(keys map[string]json.RawMessage, exact string) bool {
	for key := range keys {
		if key != exact && strings.EqualFold(key, exact) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string, expected ...string) {
	writeJSON(w, status, apiError{Error: message, Expected: expected})
}

// writeNATSError answers a request NATS did not serve. What NATS refuses however often it is sent
// (bus.ErrRefused) is the caller's own input, so it is answered naming why: a message too large to
// take whole, or a session id or role whose KV key would make a subject too long (bus.ErrTooLarge),
// is a 413, and any other refusal, a topic or key NATS does not accept in a subject
// (bus.ErrInvalidSubject) or a key outside nats.go's key alphabet (bus.ErrInvalidKey: `ses:bad`), a
// 400. Any other failure is status with message. It returns the status it wrote: a caller that
// also records the answer (requestRecord, request_record.go) reads this one classification back
// instead of repeating it, so no record can disagree with the response that was sent.
func writeNATSError(w http.ResponseWriter, err error, status int, message string) int {
	switch {
	case errors.Is(err, bus.ErrTooLarge):
		status, message = http.StatusRequestEntityTooLarge, err.Error()
	case errors.Is(err, bus.ErrRefused):
		status, message = http.StatusBadRequest, err.Error()
	}
	writeJSONError(w, status, message)
	return status
}

// busUnansweredPrefix opens the body of every answer this listener gives a request the message bus
// did not answer, so one string matches in a test and in a deployed log filter. It is deliberately
// distinct from the starting gate's "service starting" 503 (main.go): that one says this listener
// is not ready yet and the same request will be served here in a moment, this one says the bus
// itself did not answer, and the two ask the caller for different actions.
const busUnansweredPrefix = "envoy: the message bus did not answer"

// publishDeniedPrefix opens the body of every answer this listener gives a publish the NATS server
// refused under its connection's own publish permissions, so one string matches in a test, in a
// deployed log filter and in the Legion daemon, whose notify.HTTPPublisher finishes a notice on a
// 403 whose error opens with its copy of this string (publishDeniedPrefix,
// packages/daemon/internal/notify/notify.go). Rewording it turns that denial back into a notice the
// daemon retries without a cap, holding the architect's later notices behind it. It is
// deliberately not busUnansweredPrefix: the bus answered this publish, and answered it with a
// denial, so "the message bus did not answer" would be false of it, and the operator reading it
// would go looking for an outage instead of for the grant that forbids the subject.
const publishDeniedPrefix = "envoy: the message bus denied the publish under this listener's own permissions"

// streamFullPrefix opens the body of every answer this listener gives a send or publish the stream
// refused because it holds its byte limit (bus.ErrStreamFull: ENVOY_STREAM_MAX_BYTES with
// DiscardNew), so one string matches in a test and in a deployed log filter. It is deliberately not
// busUnansweredPrefix: the bus answered, that the stream is full, so "the message bus did not
// answer" would send an operator looking for an outage instead of at the stream's size. Unlike a
// denial it is worth retrying: the stream takes the same message once age or an operator has made
// room.
const streamFullPrefix = "envoy: the message bus refused the message because its stream is full"

// writeBusUnavailable answers a send, a publish, a role lookup or a session or interest read the
// bus did not serve, in the one place that writes that 503, so the body's prefix and the hint never
// drift apart. Retry-After is set only where retrying can help: every member of bus.ErrRefused -
// a refusal NATS repeats however often the same bytes are sent - is answered by writeNATSError as
// what it is, bus.ErrTooLarge as the 413 and bus.ErrInvalidSubject and bus.ErrInvalidKey as the
// 400, so this one guard keeps the header off every bus.ErrRefused and off nothing else. The
// status writeNATSError settled on is passed back for the record's sake, as that function's own
// comment explains.
//
// A publish the server denied (bus.ErrPublishDenied) is answered ahead of both, because it is
// neither. bus.ErrRefused has exactly three members (internal/bus/nats.go): bus.ErrTooLarge,
// bus.ErrInvalidSubject and bus.ErrInvalidKey, each of them this client's own refusal of the
// caller's bytes, decided before anything is sent. A denial is the opposite: the publish reached a
// server that answered `-ERR 'Permissions Violation for Publish to "<subject>"'` and dropped it,
// which internal/bus names bus.ErrPublishDenied around nats.ErrPermissionViolation and the
// server's own text (confirmPublished for a core publish, deniedPublish for a JetStream one). So
// it is not an outage and not a malformed request, and retrying it can never succeed: the grant
// this listener connects with forbids the subject until an operator changes it. 403 says exactly
// that - the request was understood and the server refuses to authorise it - where 400 would
// blame the caller's bytes, which are fine, and where any 5xx would be retried:
// packages/envoy-client/src/transport.ts retries every status at or past 500 and no status below
// it. No Retry-After rides it for the same reason.
//
// A message the stream refused because it is full (bus.ErrStreamFull) is answered ahead of the
// bus-unavailable 503 too, with a 503 and Retry-After of its own under streamFullPrefix: the bus
// answered, so the body must not say it did not, but the same message is taken once the stream
// has room, so the caller's retry is right. A republish that failed because the stream is full
// (bus.ErrNotStored wrapping bus.ErrStreamFull) is answered the same way.
func writeBusUnavailable(w http.ResponseWriter, err error) int {
	if errors.Is(err, bus.ErrPublishDenied) {
		writeJSONError(w, http.StatusForbidden, fmt.Sprintf("%s: %s", publishDeniedPrefix, err))
		return http.StatusForbidden
	}
	if errors.Is(err, bus.ErrStreamFull) {
		w.Header().Set("Retry-After", "1")
		writeJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("%s: %s", streamFullPrefix, err))
		return http.StatusServiceUnavailable
	}
	if !errors.Is(err, bus.ErrRefused) {
		w.Header().Set("Retry-After", "1")
	}
	return writeNATSError(w, err, http.StatusServiceUnavailable, fmt.Sprintf("%s: %s", busUnansweredPrefix, err))
}

// storeAnswered reports whether err, from the store work of a /v1 route that writes, done within
// ctx, is the store's own answer: a key NATS refuses (bus.ErrRefused), no key (nats.ErrKeyNotFound),
// a record this build cannot decode (store.ErrMalformedInterest, store.ErrMalformedRoleClaim), or a
// conditional write or delete another writer's change beat (nats.ErrKeyExists). Every other error
// did not answer, whatever it names, and so does any error once ctx has ended. An errors.Join of
// failures (store.Registry.SetRoleWithPrevious joins a lost claim with its holder read and its
// rollback) answered only when every part of it did.
func storeAnswered(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	return storeAnswer(err)
}

// joinedType is the type errors.Join returns. fmt.Errorf with several %w also unwraps to a list,
// but its parts are one failure and the detail it carries (store.ErrMalformedInterest and the
// decode error), so only a join is split; a join under a single-%w wrapper is found by walking the
// wrapper's chain first.
var joinedType = reflect.TypeOf(errors.Join(errors.New("")))

func storeAnswer(err error) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if joined, ok := e.(interface{ Unwrap() []error }); ok && reflect.TypeOf(e) == joinedType {
			for _, part := range joined.Unwrap() {
				if !storeAnswer(part) {
					return false
				}
			}
			return true
		}
	}
	return errors.Is(err, bus.ErrRefused) || errors.Is(err, nats.ErrKeyNotFound) || errors.Is(err, nats.ErrKeyExists) ||
		errors.Is(err, store.ErrMalformedInterest) || errors.Is(err, store.ErrMalformedRoleClaim)
}

// writeStoreError answers err from a write route's store work done within ctx: a store answer
// (storeAnswered) as status naming err, as the route always answered it (a refused key still the
// 413 or 400 writeNATSError gives it), and anything else as the bus-unavailable 503 with
// Retry-After: 1 (writeBusUnavailable).
func writeStoreError(ctx context.Context, w http.ResponseWriter, err error, status int) {
	if storeAnswered(ctx, err) {
		writeNATSError(w, err, status, err.Error())
		return
	}
	writeBusUnavailable(w, err)
}

func validateMessageBody(body messageBody) (string, string) {
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"in_reply_to", body.InReplyTo},
		{"supersedes", body.Supersedes},
	} {
		if field.value != nil && strings.TrimSpace(*field.value) == "" {
			return field.name + " must not be empty", field.name
		}
	}
	if body.Urgency != nil {
		if *body.Urgency == "" || (*body.Urgency != "low" && *body.Urgency != "med" && *body.Urgency != "high" && *body.Urgency != "blocking") {
			return "urgency must be one of low, med, high, blocking", "urgency"
		}
	}
	if body.ExpectsReply != nil {
		if *body.ExpectsReply == "" || (*body.ExpectsReply != "none" && *body.ExpectsReply != "optional" && *body.ExpectsReply != "required") {
			return "expects_reply must be one of none, optional, required", "expects_reply"
		}
	}
	return "", ""
}

func optionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func messageEnvelope(body messageBody, topic, dedupeKey string) contracts.Envelope {
	summary := contracts.OneLineSummary(body.Message)
	payload := ""
	if body.Payload != nil {
		payload = *body.Payload
	} else if body.Message != summary {
		payload = body.Message
	}
	source := body.Source
	if source == "" {
		source = "agent"
	}
	return contracts.Envelope{
		EventID:        id.New(),
		Source:         source,
		SourceSession:  body.SourceSession,
		SourceEventID:  id.New(),
		Topic:          topic,
		DedupeKey:      dedupeKey,
		IssuedAt:       contracts.NowMillis(),
		ExpiresAt:      body.ExpiresAt,
		PayloadSummary: summary,
		Payload:        payload,
		TraceID:        id.New(),
		InReplyTo:      optionalString(body.InReplyTo),
		Supersedes:     optionalString(body.Supersedes),
		Urgency:        optionalString(body.Urgency),
		ExpectsReply:   optionalString(body.ExpectsReply),
	}
}

// senderStamp is the sender a send or a publish carries: the session, the machine and directory it
// runs in (its interest record), the roles whose claims name it (store.Registry.RolesHeldBy), and
// its title (its session entry). It only enriches the envelope, so no read it makes may cost the
// message. The interest record is read from the cache, and a miss - a sender that subscribed to
// nothing, or any sender while the cache refills after a reconnect - is read through to the bucket
// within senderStampReadTimeout of ctx, the route's own (store.Registry.Get bounds that read
// alone). A read that does not answer leaves the stamp with what is already known, the session id
// and the title the session cache holds, and leaves the rest of the budget to the publish that
// follows, which meets the same silent bus itself and is answered as the named 503. The roles come
// from the registry's watched view of the claims, with no bucket call, and a view that is not
// complete (store.ErrRoleViewNotReady) leaves them out rather than refusing the send.
func senderStamp(ctx context.Context, registry *store.Registry, sessions *session.SessionRegistry, sourceSession string) *contracts.EnvelopeSender {
	if sourceSession == "" {
		return nil
	}
	sender := &contracts.EnvelopeSender{SessionID: sourceSession}
	readCtx, cancel := context.WithTimeout(ctx, senderStampReadTimeout)
	defer cancel()
	if interest, err := registry.Get(readCtx, sourceSession); err == nil {
		sender.Machine = interest.MachineID
		sender.Cwd = interest.Dir
	}
	if roles, err := registry.RolesHeldBy(sourceSession); err == nil {
		sender.Roles = roles
	}
	if entry, err := sessions.Get(sourceSession); err == nil {
		sender.Title = entry.Title
	}
	return sender
}

// forwardVerdictKind is how beforeForward judged a message to a holder.
type forwardVerdictKind uint8

const (
	// forwardAllowed: nothing stops the forward.
	forwardAllowed forwardVerdictKind = iota
	// forwardEcho: the message's source session is the holder itself.
	forwardEcho
	// forwardRepeat: the holder already took a message under this dedupe key.
	forwardRepeat
	// forwardModeUnreadable: the payload's delivery mode cannot be read unambiguously.
	forwardModeUnreadable
	// forwardModeUnadvertised: the holder does not advertise the payload's delivery mode.
	forwardModeUnadvertised
)

// forwardVerdict is beforeForward's judgement. mode is the payload's delivery mode, set for
// forwardModeUnadvertised; refusal is the refusal's text, set for both mode verdicts, and is what
// sendHandler and the publish route answer in their 403.
type forwardVerdict struct {
	kind    forwardVerdictKind
	mode    string
	refusal string
}

// beforeForward applies, to a message about to be forwarded to holder, whose session entry is
// entry, the rules a role delivery applies before it forwards, in this order: the source session
// holds the role (an echo, never forwarded back), seen reports the holder already took this dedupe
// key (a repeat), and the holder cannot take the payload's delivery mode (deliveryModeVerdict). The
// role lane (roleTopicDelivery) and the publish route (roleSend.settledBeforeForward) both judge by
// it, each with the record it trusts as seen, so both refuse and skip the same messages. Guarding
// after the dedupe check keeps a delivered repeat from being judged by the holder's current
// capabilities, which can have changed since the original forward and would misreport a delivered
// message as failed — a false signal the Legion daemon treats as cause to probe and potentially
// resume the worker.
func beforeForward(item contracts.Envelope, holder string, entry session.SessionEntry, seen func(key, sessionID string) bool) forwardVerdict {
	if item.SourceSession != "" && item.SourceSession == holder {
		return forwardVerdict{kind: forwardEcho}
	}
	if seen(item.DedupeKey, holder) {
		return forwardVerdict{kind: forwardRepeat}
	}
	return deliveryModeVerdict(holder, entry, item.Payload)
}

// deliveryModeVerdict judges a message to sessionID, whose session entry is entry, by its
// payload's delivery mode, read as the receiving client reads it (frameDeliveryMode): the mode
// cannot be read unambiguously, or entry does not advertise it (hasCapability); otherwise the
// message may be forwarded. sendHandler answers a refusal 403 with its text, recorded as
// outcomeCapabilityRefused, as the publish route does through beforeForward.
func deliveryModeVerdict(sessionID string, entry session.SessionEntry, payload string) forwardVerdict {
	var framed *string
	if payload != "" {
		framed = &payload
	}
	mode, err := frameDeliveryMode(framed)
	switch {
	case err != nil:
		return forwardVerdict{
			kind:    forwardModeUnreadable,
			refusal: fmt.Sprintf("session %s (%s) sent a delivery mode that cannot be read unambiguously", sessionID, entry.Title),
		}
	case mode != "" && !hasCapability(entry.Capabilities, mode):
		return forwardVerdict{
			kind:    forwardModeUnadvertised,
			mode:    mode,
			refusal: fmt.Sprintf("session %s (%s) does not advertise %s", sessionID, entry.Title, mode),
		}
	}
	return forwardVerdict{kind: forwardAllowed}
}

// sendRequestBudget is the bus work a /v1 route does under the request's own context: the send
// route's read of the target session, its read of the sender's interest record (senderStamp) and
// its publish share it, and GET /v1/roles/<role> resolves its holder within it too. The publish
// route spends it on the publish of a topic the stream captures, and a role send's first
// resolution of the role's holder (sendToRole, every attempt of that resolution) is answered within
// it too; a role send then runs on within publishDeliveryWindow (confirmed.go). GET
// /v1/sessions, GET /v1/registry/<session_id> and GET /v1/interests/<session_id> read within it as
// well, and every route that writes does its store work within it: subscribe, unsubscribe, POST
// /v1/roles/set, and DELETE of /v1/interests/<session_id> and /v1/sessions/<session_id>. The HTTP
// client gives each attempt 5 s (packages/envoy-client/src/transport.ts) and this listener's HTTP
// write timeout is 10 s (main.go), so answering inside 3.5 s leaves the rest for TLS, scheduling
// and the body: a sender whose bus cannot be reached reads the 503 that names the reason, in time
// to act on it, instead of the timeout it would otherwise retry blind.
const sendRequestBudget = 3500 * time.Millisecond

// sendSessionReadTimeout bounds the send route's read of the target session from the bucket, so a
// bucket that does not answer still leaves most of sendRequestBudget (~2.5 s) to the sender stamp's
// read (senderStampReadTimeout) and the publish. The role paths keep roleHolderReadTimeout as their
// per-read cap instead.
const sendSessionReadTimeout = 1 * time.Second

// senderStampReadTimeout bounds senderStamp's read of the sender's interest record from the bucket.
// nats.go's KV read takes no context and waits out the JetStream MaxWait (10 s), and the stamp is
// only enrichment, so on a bus that does not answer it gets no more of sendRequestBudget than the
// send route's own read of its target (sendSessionReadTimeout): a send whose two reads both go
// unanswered still leaves its publish 1.5 s, and the publish is what answers the named 503.
const senderStampReadTimeout = 1 * time.Second

// sendHandler publishes a direct message only while the session store itself, not merely this
// listener's cache of it, holds the target's entry: a 404 here is a sender's cue to route around
// the pane, so only the store's word that the key is gone earns one, and a store or a bus that did
// not answer earns the 503 that asks the sender to try the same route again. A target that cannot
// take the payload's delivery mode (deliveryModeVerdict) is the 403 naming it. Every one of those
// answers is written through record, so this route's one line per request is written with it.
func sendHandler(d *listenerDeps, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		record := &requestRecord{logger: logger, line: "listener send", laneKey: "target_session"}
		if r.Method != http.MethodPost {
			record.refuse(w, http.StatusMethodNotAllowed, outcomeInvalidRequest, "method not allowed")
			return
		}
		// Every bus read and the publish below share one deadline derived from the caller's own, so
		// a bus this listener cannot reach is answered as a named 503 inside the caller's attempt
		// rather than left to that caller's timeout.
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		var body messageBody
		var targetSession string
		var request struct {
			messageBody
			TargetSession string `json:"target_session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "invalid json")
			return
		}
		body = request.messageBody
		targetSession = strings.TrimSpace(request.TargetSession)
		record.lane = targetSession
		if targetSession == "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "target_session is required", "target_session")
			return
		}
		if strings.TrimSpace(body.Message) == "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "message is required", "message")
			return
		}
		if message, field := validateMessageBody(body); message != "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, message, field)
			return
		}
		dedupeKey := "agent." + targetSession + "." + id.New()
		if body.IdempotencyKey != "" {
			dedupeKey = "agent." + targetSession + "." + body.IdempotencyKey
		}
		item := messageEnvelope(body, contracts.AgentSubject(targetSession), dedupeKey)
		record.item = &item
		if err := item.Validate(); err != nil {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, err.Error())
			return
		}
		// The cache alone cannot say a session is gone: it trails the bucket it follows, and is
		// still being refilled after a reconnect, so a miss is read through to the bucket itself.
		// Only the bucket's own word for it (nats.ErrKeyNotFound, which Refresh also returns for a
		// key no session can register under) is the 404 a sender routes around the pane on; a read
		// that did not answer is the 503 below, which asks it to try again instead.
		target, err := liveSessionEntry(ctx, d.sessions, targetSession, sendSessionReadTimeout, registered)
		if errors.Is(err, nats.ErrKeyNotFound) {
			record.refuse(w, http.StatusNotFound, outcomeNoLiveSession, fmt.Sprintf("no live session %s", targetSession))
			return
		}
		if err != nil {
			record.refuseBusUnavailable(w, err)
			return
		}
		if verdict := deliveryModeVerdict(targetSession, target, item.Payload); verdict.refusal != "" {
			record.refuse(w, http.StatusForbidden, outcomeCapabilityRefused, verdict.refusal)
			return
		}
		item.Sender = senderStamp(ctx, d.registry, d.sessions, item.SourceSession)
		duplicate, err := d.client.PublishReportingDuplicateWithin(ctx, item)
		if err != nil {
			record.refuseBusUnavailable(w, err)
			return
		}
		record.accept(w, sendResponse{
			Envelope: item, Recipient: targetSession, Duplicate: duplicate,
		})
	}
}

// deleteSessionHandler removes a live session registration at shutdown, and answers 200 whether or
// not there was one to remove. A client that mints its own instance id sends it as the
// client_instance_id query parameter, and then the registration is removed only while it carries
// that id or carries none (session.SessionRegistry.DeleteIfInstance): a Claude Code /clear handoff
// deregisters the session it hands off from before it claims that session's role, and the entry
// under that id can by then be another process's live registration, which the handoff must leave
// alone instead of clearing the predecessor its claim is then weighed against. Such a request
// answers {"deleted":true} for a registration that is gone and {"deleted":false} with a reason for
// one that stayed, both 200: the deregistering client has nothing to retry either way. Each of
// DeleteIfInstance's two kept outcomes gets its own reason, because only one of them knows that
// another client instance owns the registration: a delete that lost both compare-and-swaps read
// an entry this very instance may own and says only that the registration changed under it. A
// request naming no instance id removes the registration whoever wrote it, and answers 200 with
// no body, as this route always has. The delete runs within sendRequestBudget of the request's
// own context, so one the bus does not answer is the bus-unavailable 503 with Retry-After: 1
// inside the caller's attempt (writeStoreError).
func deleteSessionHandler(sessions *session.SessionRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		if sessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		instanceID := strings.TrimSpace(r.URL.Query().Get("client_instance_id"))
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		if instanceID == "" {
			if err := sessions.Delete(ctx, sessionID); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
				writeStoreError(ctx, w, err, http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		outcome, err := sessions.DeleteIfInstance(ctx, sessionID, instanceID)
		if err != nil {
			writeStoreError(ctx, w, err, http.StatusInternalServerError)
			return
		}
		switch outcome {
		case session.EntryDeleted:
			writeJSON(w, http.StatusOK, deleteSessionResponse{Deleted: true})
		case session.EntryKeptForAnotherInstance:
			writeJSON(w, http.StatusOK, deleteSessionResponse{
				Reason: "the registration belongs to another client instance",
			})
		default:
			// session.EntryKeptAfterLostRace: the entry matched and both conditional deletes
			// lost to a write that landed under them, this client instance's own heartbeat
			// among the likeliest, so the only true thing to say is that it stayed and
			// changed. session.EntryUnanswered never reaches here: it comes with the error
			// answered above.
			writeJSON(w, http.StatusOK, deleteSessionResponse{
				Reason: "the registration changed while it was being removed",
			})
		}
	}
}

// deleteSessionResponse is what DELETE /v1/sessions/<session_id> answers a request that named a
// client instance id: whether the registration is gone, and for one that stayed why it did. A
// request that named none answers 200 with no body at all, as this route always has.
type deleteSessionResponse struct {
	Deleted bool   `json:"deleted"`
	Reason  string `json:"reason,omitempty"`
}

// publishHandler rejects agent-targeted topics (must use /v1/messages/send
// instead) and sends the envelope. A role topic is forwarded to its holder,
// answered once confirmed or when publishDeliveryWindow from the request's
// arrival has passed (sendToRole in confirmed.go); a topic the stream captures
// is answered once the stream stored it, and any other notifications. topic is
// refused by name, since nothing would store it for a session to read. An
// explicit dedupe_key is used verbatim (a re-send a receiver's own dedupe
// recognises); it is mutually exclusive with idempotency_key, may not begin
// with roleForwardDedupePrefix, the mark the role arbiter drops on sight, and
// may not ride a dispatch envelope. Every host drops a repeat of any dispatch key
// (contracts.DedupeKeyNamesTheUpstreamEvent), and Dispatch's outbox keys are
// sequential (dispatch-<event id>), so a caller that chose one could make a
// host drop the real event it names. Dispatch itself never needs this route's
// key: its outbox publishes to the bus directly and its sends go through
// /v1/messages/send, whose key is the listener's. Like the send route, it answers through record,
// which writes this route's one line per request with the answer; a confirmed send answers through
// it too.
func publishHandler(d *listenerDeps, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		arrived := time.Now()
		record := &requestRecord{logger: logger, line: "listener publish", laneKey: "topic"}
		if r.Method != http.MethodPost {
			record.refuse(w, http.StatusMethodNotAllowed, outcomeInvalidRequest, "method not allowed")
			return
		}
		var request struct {
			messageBody
			Topic string `json:"topic"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "invalid json")
			return
		}
		// The topic is recorded as the envelope will carry it, untrimmed, so a request refused for
		// the spelling of its topic records the spelling that was refused.
		record.lane = request.Topic
		if strings.TrimSpace(request.Topic) == "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "topic is required", "topic")
			return
		}
		if strings.TrimSpace(request.Message) == "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "message is required", "message")
			return
		}
		if !strings.HasPrefix(request.Topic, bus.NotificationTopicPrefix) {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "topic must begin with "+bus.NotificationTopicPrefix, "topic")
			return
		}
		if strings.HasPrefix(request.Topic, contracts.AgentTopicPrefix) {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "cannot publish to agent topics; use /v1/messages/send for direct agent messages")
			return
		}
		if message, field := validateMessageBody(request.messageBody); message != "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, message, field)
			return
		}
		if request.DedupeKey != "" && request.IdempotencyKey != "" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "dedupe_key and idempotency_key are mutually exclusive", "dedupe_key", "idempotency_key")
			return
		}
		if strings.HasPrefix(request.DedupeKey, roleForwardDedupePrefix) {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "dedupe_key must not begin with the reserved prefix "+roleForwardDedupePrefix, "dedupe_key")
			return
		}
		if request.DedupeKey != "" && request.Source == "dispatch" {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, "dedupe_key cannot be chosen for a dispatch envelope", "dedupe_key")
			return
		}
		dedupeKey := "publish." + id.New()
		switch {
		case request.DedupeKey != "":
			dedupeKey = request.DedupeKey
		case request.IdempotencyKey != "":
			dedupeKey = "publish." + request.IdempotencyKey
		}
		item := messageEnvelope(request.messageBody, request.Topic, dedupeKey)
		record.item = &item
		if err := item.Validate(); err != nil {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest, err.Error())
			return
		}
		if role, ok := strings.CutPrefix(request.Topic, contracts.RoleTopicPrefix); ok {
			ctx, cancel := context.WithDeadline(r.Context(), arrived.Add(publishDeliveryWindow))
			defer cancel()
			sendToRole(ctx, w, record, d, item, role, arrived)
			return
		}
		// Every other topic that would deliver to no session is refused by name: a session reads
		// only what its own consumer holds, so such a publish would reach nobody, now or later;
		// nothing retries into success, so it is the 400 the refusals above answer rather than a
		// 504. Team topics are the namespace an agent names itself and the stream stores them
		// (bus.StreamSubjects). The role lanes bus.ReachesNoSession excepts are forwarded above.
		if bus.ReachesNoSession(request.Topic) {
			record.refuse(w, http.StatusBadRequest, outcomeInvalidRequest,
				"no stream subject captures "+request.Topic+", so no session would receive it; publish to a notifications.team.<name> topic instead", "topic")
			return
		}
		// The sender stamp's read and the publish take this one deadline, derived from the
		// caller's own context, so this route too answers a bus it cannot reach rather than
		// spending the caller's attempt.
		ctx, cancel := context.WithDeadline(r.Context(), arrived.Add(sendRequestBudget))
		defer cancel()
		item.Sender = senderStamp(ctx, d.registry, d.sessions, item.SourceSession)
		if err := d.client.PublishWithin(ctx, item); err != nil {
			record.refuseBusUnavailable(w, err)
			return
		}
		record.accept(w, publishResponse{Envelope: item})
	}
}

// sessionInfo is the joined view of an Interest (topics, dir, machine) and a
// SessionEntry (port). Returned by GET /v1/sessions. Its host is where the session runs, its
// recorded host or else MachineID (HostOrMachine). Only the session's owner delivers to it
// (session.Deliverer): the listener its stored subscription names, the one store.Registry.Match
// delivers on, else the one MachineID names. The owner reaches a session registered through it at
// its host bridge when one is set, else at the recorded host, else at host.docker.internal; a
// session that re-registered through another listener is reached at its recorded host, else at
// MachineID. Any other listener leaves inbox messages to the owner silently.
type sessionInfo struct {
	SessionID      string   `json:"session_id"`
	MachineID      string   `json:"machine_id"`
	Host           string   `json:"host"`
	Dir            string   `json:"dir"`
	Port           int      `json:"port"`
	Title          string   `json:"title"`
	Capabilities   []string `json:"capabilities"`
	SelfSubscribed bool     `json:"self_subscribed"`
	Topics         []string `json:"topics"`
	Roles          []string `json:"roles"`
	UpdatedAt      int64    `json:"updated_at"`
	LastSeen       int64    `json:"last_seen"`
	// The plugin the session registered through and its build; absent for a session whose plugin
	// predates the fields.
	Plugin        string `json:"plugin,omitempty"`
	PluginVersion string `json:"plugin_version,omitempty"`
	// Delivery is how this listener delivers to the session: session.DeliveryConsumer
	// ("consumer") when its own session consumer holds its stored messages, empty for a session on
	// the core path. Written always, never omitempty: the production reading of "0 sessions on
	// core" has to tell a listener that does not report delivery at all - a build older than this
	// field, which sends no such key - from a session this listener reports as on the core path.
	// With omitempty both are a row with no "delivery" key and the two cannot be told apart, so
	// the count of core sessions would read as zero on a fleet that never reported one.
	Delivery string `json:"delivery"`
}

// sessionInterestReadTimeout bounds sessionInfoFor's read of one session's interest record from
// the bucket, which a cache miss makes (store.Registry.Get bounds that read alone): a bucket that
// does not answer is reported a second in, rather than when the route's sendRequestBudget ends,
// and GET /v1/registry/<session_id>, whose session read may already have spent
// roleHolderReadTimeout (2 s), still answers inside that budget.
const sessionInterestReadTimeout = 1 * time.Second

// sessionInfoFor joins one session entry with its interest record's topics and the roles whose
// claims name it (store.Registry.RolesHeldBy), so a row of GET /v1/sessions and the single answer
// of GET /v1/registry/<session_id> are one shape built one way. A session holds a role exactly
// when the role's claim names it: a role topic its record keeps from a claim it lost gives it no
// role, and a claim it holds is reported whatever its record's topics are, or with no record at
// all. A claims view that is not complete (store.ErrRoleViewNotReady: still scanning, refilling a
// recreated bucket, or its watcher ended) is returned alone, as a read that did not answer, never
// as a session holding no roles. A session the interest registry holds no key for keeps its empty
// topics: a session that registered and subscribed to nothing is not an error here. The other
// answers about the record (interestReadAnswered) are returned with the session's info, its topics
// empty, so the caller can still show the session and name what the read answered. Every other
// error is a read that did not answer and is returned alone: a caller answers it as the
// bus-unavailable 503, never as a session with no topics or roles.
func sessionInfoFor(ctx context.Context, registry *store.Registry, sessionID string, entry session.SessionEntry) (sessionInfo, error) {
	roles, err := registry.RolesHeldBy(sessionID)
	if err != nil {
		return sessionInfo{}, err
	}
	info := sessionInfo{
		SessionID:      sessionID,
		MachineID:      entry.MachineID,
		Host:           entry.HostOrMachine(),
		Dir:            entry.Dir,
		Port:           entry.Port,
		Title:          entry.Title,
		Capabilities:   append([]string{}, entry.Capabilities...),
		SelfSubscribed: entry.SelfSubscribed,
		Roles:          roles,
		UpdatedAt:      entry.UpdatedAt,
		LastSeen:       entry.UpdatedAt,
		Plugin:         entry.Plugin,
		PluginVersion:  entry.PluginVersion,
		Delivery:       entry.Delivery,
	}
	readCtx, cancel := context.WithTimeout(ctx, sessionInterestReadTimeout)
	defer cancel()
	interest, err := registry.Get(readCtx, sessionID)
	switch {
	case errors.Is(err, nats.ErrKeyNotFound):
		return info, nil
	case interestReadAnswered(err):
		return info, err
	case err != nil:
		return sessionInfo{}, err
	}
	info.Topics = interest.Topics
	return info, nil
}

// interestReadAnswered reports whether err, from a read of an interest record other than the
// bucket's word that it holds no key (nats.ErrKeyNotFound), is an answer about that record, which
// a retry reads again: a record this build cannot decode (store.ErrMalformedInterest, the 500
// writeInterestRecordUnreadable writes), or a key the interest bucket refuses (bus.ErrRefused: a
// legacy id past envoy_interests' get bound that envoy_sessions still lists, or one outside
// nats.go's key alphabet, which GET /v1/interests/<session_id> answers as the 413 or 400
// writeBusUnavailable writes and the two session routes as a session with no topics). Any other
// error is a read that did not answer, whatever it names. In nats.go v1.50.0 that includes ctx's
// end (store.Registry.Get's bound, or the JetStream MaxWait context nats.go's direct get always
// runs under, so it always takes requestWithContext), nats.ErrConnectionClosed,
// nats.ErrNoResponders, and whatever nats.go's publish returns, the raw *net.OpError of a write
// buffer flushed onto a broken connection among them; the rule does not rest on that list being
// complete.
func interestReadAnswered(err error) bool {
	return errors.Is(err, store.ErrMalformedInterest) || errors.Is(err, bus.ErrRefused)
}

// writeInterestRecordUnreadable answers a read of one session's interest record that this build
// cannot decode (store.ErrMalformedInterest), for GET /v1/registry/<session_id> and GET
// /v1/interests/<session_id>. The bus answered, and will answer the same record again, so this is
// not the bus-unavailable 503: it is a 500 with no Retry-After, naming the session and the record,
// as writeRoleClaimUnreadable answers an unreadable role claim.
func writeInterestRecordUnreadable(w http.ResponseWriter, sessionID string, err error) {
	writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf(
		"envoy: the interest record of session %s is unreadable: %s", sessionID, err,
	))
}

// sessionsHandler returns all live sessions by iterating the session registry
// (envoy_sessions, 5-min TTL — only live sessions) and enriching each with
// topic data from the interests registry (envoy_interests). Every interest read shares one
// sendRequestBudget deadline, so however many rows miss the interest cache the list answers
// inside it. Each row's roles are those whose claims name the session (sessionInfoFor). A session
// whose interest read answered with no topics to give - a record this build cannot decode or a key
// the bucket refuses (interestReadAnswered) - is listed with no topics rather than refusing the
// list; any other error, a claims view that cannot answer among them, is a read that did not answer
// and ends the request with the bus-unavailable 503. The registry logs a record this build cannot
// decode at WARN itself; a key the bucket refuses is logged here at WARN, once per listed session,
// through the registry's logger, the listener's own.
func sessionsHandler(registry *store.Registry, sessions *session.SessionRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		entries, err := sessions.List()
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		dirFilter := r.URL.Query().Get("dir")
		titleFilter := r.URL.Query().Get("title")
		result := make([]sessionInfo, 0, len(entries))
		for _, entry := range entries {
			if !strings.Contains(entry.Dir, dirFilter) || !strings.Contains(entry.Title, titleFilter) {
				continue
			}
			info, err := sessionInfoFor(ctx, registry, entry.SessionID, entry.SessionEntry)
			if err != nil && !interestReadAnswered(err) {
				writeBusUnavailable(w, err)
				return
			}
			if errors.Is(err, bus.ErrRefused) {
				registry.Logger().Warn("session listed with no topics: its interest record could not be read",
					slog.String("session_id", entry.SessionID),
					slog.String("error", err.Error()),
				)
			}
			result = append(result, info)
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// sessionEntryHandler answers GET /v1/registry/<session_id>, the one question a caller holding a
// session id asks before it addresses that session: is this session live, and how is it named? The
// read goes through liveSessionEntry, so a session this listener's cache does not hold yet — the
// session bucket was recreated and the watcher is still refilling it, or another listener
// registered the session a moment ago — is read from the bucket itself instead of being reported
// gone. Only the bucket's word that the key is absent is a 404, and its wording is load-bearing:
// Dispatch classifies a delivery failure on the "no live session " prefix, as the send route's own
// 404 writes it. An id the session bucket refuses as a key is the caller's to fix, the 413 or 400
// writeBusUnavailable writes. A live session whose id only the interest bucket refuses (a legacy id
// past envoy_interests' get bound that envoy_sessions still serves, or one outside nats.go's key
// alphabet) is answered 200 with no topics, as GET /v1/sessions lists it, and logged at WARN as the
// list logs it: Dispatch reads a 413 or 400 here as a session that cannot exist. A read that did
// not answer — the session's, its interest record's, or the claims view's (sessionInfoFor), all
// within sendRequestBudget — is a 503 to retry: never a session a caller may take for ended, nor
// one with no topics or roles. An interest record this build cannot decode is the 500
// writeInterestRecordUnreadable writes: the bus answered it.
func sessionEntryHandler(d *listenerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
		if sessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		entry, err := liveSessionEntry(ctx, d.sessions, sessionID, roleHolderReadTimeout, registered)
		switch {
		case errors.Is(err, bus.ErrRefused):
			// Refresh reports a refused key as a missing one too, so this test comes first:
			// an id NATS will never hold is answered as the bad input it is, not as a 404.
			// writeBusUnavailable answers bus.ErrTooLarge as the 413 it is and every other
			// refusal as a 400, and withholds Retry-After from both, since NATS repeats a
			// refusal however often the same bytes are sent.
			writeBusUnavailable(w, err)
		case errors.Is(err, nats.ErrKeyNotFound):
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no live session %s", sessionID))
		case err != nil:
			// The 503, its Retry-After: 1 and the busUnansweredPrefix body are written in one
			// place for every route that answers a bus that did not answer, so the string a
			// deployed log filter matches has a single copy (INFRA-141). This route's own
			// literal was that copy's twin until the two changes met here.
			writeBusUnavailable(w, err)
		default:
			info, err := sessionInfoFor(ctx, d.registry, sessionID, entry)
			switch {
			case errors.Is(err, store.ErrMalformedInterest):
				writeInterestRecordUnreadable(w, sessionID, err)
			case errors.Is(err, bus.ErrRefused):
				d.registry.Logger().Warn("session answered with no topics: its interest record could not be read",
					slog.String("session_id", sessionID),
					slog.String("error", err.Error()),
				)
				writeJSON(w, http.StatusOK, info)
			case err != nil:
				writeBusUnavailable(w, err)
			default:
				writeJSON(w, http.StatusOK, info)
			}
		}
	}
}

// interestRow is one persisted interest as GET /v1/interests/ and GET /v1/interests/<session_id>
// answer it: the stored record plus LastSeen, the session's last heartbeat (Unix ms) as this
// listener's session cache knows it, omitted when unknown. The record's own UpdatedAt can lag that
// heartbeat by up to store.InterestRefreshAge, since an unchanged row is rewritten only that often.
type interestRow struct {
	store.Interest
	LastSeen int64 `json:"last_seen,omitempty"`
}

// interestRowFor answers item with its session's last heartbeat: the live entry's UpdatedAt while
// the session is live, else the final heartbeat the cache still retains for it
// (session.SessionRegistry.LastSeen), else none. A listener restart empties those records, so a
// session that died before it reads as unknown.
func interestRowFor(sessions *session.SessionRegistry, item store.Interest) interestRow {
	row := interestRow{Interest: item}
	if entry, err := sessions.Get(item.SessionID); err == nil {
		row.LastSeen = entry.UpdatedAt
	} else {
		row.LastSeen = sessions.LastSeen(item.SessionID)
	}
	return row
}

// adminInterestsHandler lists, reads and removes persisted interests. Each row a GET answers
// carries its session's last heartbeat (interestRowFor). GET /v1/interests/<session_id>
// reads within sendRequestBudget of the request's own context: a cache miss is read through to the
// bucket, the bucket's word that the key is absent is a 404, a record this build cannot decode is
// the 500 writeInterestRecordUnreadable writes, and a read that did not answer is the
// bus-unavailable 503 with Retry-After: 1 (a key NATS refuses is still the 413 or 400
// writeBusUnavailable gives it). DELETE removes within the same budget, and answers store work that
// did not answer with that 503 too (writeStoreError). removed, when not nil, is called after a
// DELETE removed a session's interests.
func adminInterestsHandler(registry *store.Registry, sessions *session.SessionRegistry, removed func(ctx context.Context, sessionID string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/interests/")

		if sessionID == "" {
			if r.Method != http.MethodGet {
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			items := registry.List()
			rows := make([]interestRow, 0, len(items))
			for _, item := range items {
				rows = append(rows, interestRowFor(sessions, item))
			}
			writeJSON(w, http.StatusOK, rows)
			return
		}

		switch r.Method {
		case http.MethodGet:
			ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
			defer cancel()
			item, err := registry.Get(ctx, sessionID)
			// Only a session the registry holds no key for is unknown; a read NATS did not
			// serve is the listener's failure, which a caller must not take for "no topics".
			if errors.Is(err, nats.ErrKeyNotFound) {
				writeJSONError(w, http.StatusNotFound, err.Error())
				return
			}
			if errors.Is(err, store.ErrMalformedInterest) {
				writeInterestRecordUnreadable(w, sessionID, err)
				return
			}
			if err != nil {
				writeBusUnavailable(w, err)
				return
			}
			writeJSON(w, http.StatusOK, interestRowFor(sessions, item))
		case http.MethodDelete:
			ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
			defer cancel()
			if err := registry.Remove(ctx, sessionID, nil); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
				writeStoreError(ctx, w, err, http.StatusInternalServerError)
				return
			}
			if removed != nil {
				removed(r.Context(), sessionID)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

// roleSetHandler answers POST /v1/roles/set. Its store work - the claimant's registration, a soft
// claim's read of the role's holder and of that holder's session, and the claim itself
// (store.Registry.SetRoleWithPrevious) - runs within sendRequestBudget of the request's own
// context, so a bus that does not answer is the bus-unavailable 503 with Retry-After: 1 inside the
// caller's attempt (writeStoreError). The store's own answers keep the answers they had: a held
// role is a 409, a refused key a 413 or 400, and an unreadable claim the 503 or 500 it was.
//
// logger is registerV1Routes's own. One decision of this route is logged with it: a soft claim that
// takes a role from the predecessor it names because the claimant's entry or the predecessor's
// carries no client instance id, which is the one takeover the route cannot tell from a different
// client instance's, and so names both session ids.
func roleSetHandler(d *listenerDeps, machineID string, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var body struct {
			SessionID string `json:"session_id"`
			Role      string `json:"role"`
			// A soft claim takes the role only when it is unheld, when its
			// holder is no longer live, or when its holder is the claimant's
			// declared predecessor and that predecessor's own client instance
			// allows it (PreviousSessionID); any other live holder answers 409
			// with its id. This is how a resumed or forked session recovers its
			// own role without ever taking one from a peer. Default false:
			// last-claim-wins.
			Soft bool `json:"soft"`
			// The session id this claimant is continuing (a /fork, /branch, or
			// /handoff mints a new id in the same process). Naming it is not
			// enough to take the role from it: a soft claim naming a
			// predecessor takes the role only when that predecessor has no live
			// registry entry, when its live entry carries this claimant's own
			// client instance id (the same conversation continuing inside its
			// own Oh My Pi Envoy extension instance or Claude Code
			// channel-server process), or when the claimant's entry or the
			// predecessor's carries no client instance id at all, which is a
			// client predating the field and whose takeover is logged. A
			// predecessor still live in another client instance keeps the role,
			// and the claim answers 409 naming it. Only honoured with soft;
			// ignored otherwise.
			PreviousSessionID string `json:"previous_session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		body.SessionID = strings.TrimSpace(body.SessionID)
		body.Role = strings.TrimSpace(body.Role)
		if body.SessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		if body.Role == "" {
			writeJSONError(w, http.StatusBadRequest, "role is required", "role")
			return
		}
		if !isValidRole(body.Role) {
			writeJSONError(w, http.StatusBadRequest, "role must match "+rolePatternString, "role")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		entry, err := d.sessions.Get(body.SessionID)
		if errors.Is(err, nats.ErrKeyNotFound) {
			writeJSONError(w, http.StatusNotFound, "session is not registered")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "read session registration: "+err.Error())
			return
		}
		// Get answers from the cache alone, and the cache follows the session bucket through a
		// watcher and so trails it. A trailing cache would decide this claim twice over: it would
		// read an empty client instance id from an entry this process has not caught up with,
		// demoting every claim to the "no instance id" case below, and PutWithin would write that
		// stale entry back over a newer one. So the claimant's own entry is read from the bucket
		// itself, and when that read answers, its entry is the one refreshed here and the one the
		// claimant's instance id is taken from. A read that does not answer keeps the cache's
		// entry and goes on, so this claim is never worse off than it was without the bucket read.
		if fresh, freshErr := bucketSessionEntry(ctx, d.sessions, body.SessionID, roleHolderReadTimeout); freshErr == nil {
			entry = fresh
		}
		claimantInstanceID := entry.ClientInstanceID
		entry.MachineID = machineID
		entry.SelfSubscribed = true
		if err := d.sessions.PutWithin(ctx, body.SessionID, entry); err != nil {
			writeStoreError(ctx, w, fmt.Errorf("refresh role claimant registration: %w", err), http.StatusServiceUnavailable)
			return
		}
		previous := ""
		var supersedable []string
		if body.Soft {
			// The registry sees only interest rows; liveness is the session
			// bucket's call. A holder whose session entry has aged out (5m
			// TTL) is dead and may be superseded, and the claimant's own
			// declared predecessor is weighed on the client instance it
			// belongs to as well: roleSupersessionVerdict reads both from the
			// bucket and answers the three cases this switch words — the role
			// is taken from its holder, it stays with it, or a read did not
			// answer and nothing is taken from anyone.
			claim, err := d.registry.RoleClaim(ctx, body.Role)
			if err != nil {
				writeStoreError(ctx, w, fmt.Errorf("read role holder: %w", err), http.StatusServiceUnavailable)
				return
			}
			holder := claim.HolderSessionID
			previous = strings.TrimSpace(body.PreviousSessionID)
			if holder != "" && holder != body.SessionID {
				verdict, readErr := roleSupersessionVerdict(ctx, d.sessions, holder, previous, claimantInstanceID)
				switch verdict {
				case roleHolderUnread:
					// A read that did not answer takes nothing from a holder.
					writeBusUnavailable(w, readErr)
					return
				case roleTakenFromUnidentifiedPredecessor:
					logger.Info("listener role taken from a live predecessor because one side carries no client instance id",
						slog.String("role", body.Role),
						slog.String("session_id", body.SessionID),
						slog.String("previous_session_id", holder))
					supersedable = append(supersedable, holder)
				case roleTakenFromHolder:
					supersedable = append(supersedable, holder)
				case roleStaysWithHolder:
					// A live holder keeps the role, a predecessor still live inside another
					// client instance among them, and store.Registry.SetRoleWithPrevious
					// answers the 409 that names it.
				}
			}
		}
		item, err := d.registry.SetRoleWithPrevious(ctx, body.SessionID, machineID, body.Role, previous, body.Soft, supersedable...)
		var held *store.ErrRoleHeld
		if errors.As(err, &held) {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":  err.Error(),
				"role":   held.Role,
				"holder": held.Holder,
			})
			return
		}
		if err != nil {
			writeStoreError(ctx, w, err, http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, item)
	}
}

// roleGetHandler answers GET /v1/roles/<role> with the role's live holder. It resolves the holder
// (resolveRoleHolder, with registered: any entry) within sendRequestBudget of the request's own
// context, as a role send's first resolution through the publish route does, so a bus that
// does not answer is reported inside the caller's attempt. A resolution that lost the race for the
// role's claim (errRoleHolderChanged) is the publish route's 503 with Retry-After: 1, worded for a
// lookup, since every read answered and a fresh lookup wins the race once the replacement claim
// settles. A claim record that cannot be decoded (store.ErrMalformedRoleClaim) is the publish
// route's 500 with no Retry-After (writeRoleClaimUnreadable): the bus answered it. Any other
// failure is a role store call that did not answer within its cap or that budget - the claim read,
// the holder's session read or the conditional release - and is answered as the publish route
// answers it (writeBusUnavailable): the 503 with Retry-After: 1 whose body begins
// busUnansweredPrefix and names the call, or the 413 and 400 a refused key gets.
func roleGetHandler(d *listenerDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		role := strings.TrimPrefix(r.URL.Path, "/v1/roles/")
		if role == "" {
			writeJSONError(w, http.StatusBadRequest, "role is required", "role")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		result, err := resolveRoleHolder(ctx, d.registry, d.sessions, role, registered)
		if errors.Is(err, errRoleHolderChanged) {
			writeRoleHolderChanged(w, role, "lookup")
			return
		}
		if errors.Is(err, store.ErrMalformedRoleClaim) {
			writeRoleClaimUnreadable(w, role, err)
			return
		}
		if err != nil {
			writeBusUnavailable(w, err)
			return
		}
		if result.state != roleHolderLive {
			writeRoleHolderError(w, role, result)
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"role":         role,
			"holder":       result.holder,
			"title":        result.entry.Title,
			"dir":          result.entry.Dir,
			"machine_id":   result.entry.MachineID,
			"host":         result.entry.HostOrMachine(),
			"capabilities": append([]string{}, result.entry.Capabilities...),
			"last_seen":    result.entry.UpdatedAt,
		})
	}
}

func (d *listenerDeps) streamInspector() streamInfoLookup {
	if d.streamInfo != nil {
		return d.streamInfo
	}
	return d.client.JS()
}

func streamInfoWithin(ctx context.Context, inspector streamInfoLookup, streamName, subjectsFilter string) (*nats.StreamInfo, error) {
	type result struct {
		info *nats.StreamInfo
		err  error
	}
	resultCh := make(chan result, 1)
	go func() {
		info, err := inspector.StreamInfo(
			streamName,
			&nats.StreamInfoRequest{SubjectsFilter: subjectsFilter},
			nats.Context(ctx),
		)
		resultCh <- result{info: info, err: err}
	}()
	select {
	case result := <-resultCh:
		return result.info, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// githubRepositoryTopic is a GitHub topic read as Envoy publishes it. Its repository name runs from
// the token after the owner to the first token that is one of contracts.GithubTopicKinds or a
// wildcard, and Envoy writes each dot in a name as `_`, so a name the topic spreads over several
// tokens receives nothing.
type githubRepositoryTopic struct {
	owner string
	// name holds the name's tokens as the topic wrote them; repo is them joined with `_`.
	name []string
	repo string
	// spelled is the topic with repo in place of name.
	spelled string
	// kindFollows is whether a kind, rather than a wildcard or the topic's end, ended the name.
	kindFollows bool
}

func parseGithubRepositoryTopic(topic string) (githubRepositoryTopic, bool) {
	const githubTopicPrefix = "notifications.github."
	remainder, ok := strings.CutPrefix(topic, githubTopicPrefix)
	if !ok {
		return githubRepositoryTopic{}, false
	}
	parts := strings.Split(remainder, ".")
	if len(parts) < 3 || parts[0] == "" {
		return githubRepositoryTopic{}, false
	}
	end := 2
	for end < len(parts) && parts[end] != "*" && parts[end] != ">" && !slices.Contains(contracts.GithubTopicKinds, parts[end]) {
		end++
	}
	repo := strings.Join(parts[1:end], "_")
	if repo == "" {
		return githubRepositoryTopic{}, false
	}
	return githubRepositoryTopic{
		owner:       parts[0],
		name:        parts[1:end],
		repo:        repo,
		spelled:     githubTopicPrefix + strings.Join(append([]string{parts[0], repo}, parts[end:]...), "."),
		kindFollows: end < len(parts) && slices.Contains(contracts.GithubTopicKinds, parts[end]),
	}, true
}

// warning is what a subscription to the topic is told when its name spans several tokens. A kind
// after them, or an empty token among them (a leading, trailing or doubled dot), means the name
// was spelled with its dot, and the warning names Envoy's spelling. Otherwise the token after the
// name's first is a mistyped kind, and the warning names the kinds instead of a spelling that
// would receive nothing either. A repository whose name is itself a kind (`acme.pr.pr.7.>`) is
// read as that name, and `acme.site.pr.pr.7.>` reads as `acme/site`: the two can't be told apart.
func (g githubRepositoryTopic) warning(topic string) string {
	switch {
	case len(g.name) == 1:
		return ""
	case g.kindFollows || slices.Contains(g.name, ""):
		return fmt.Sprintf(`%s spells a repository name with a dot, and GitHub topics write each dot in a name as "_": subscribe to %s`, topic, g.spelled)
	default:
		return fmt.Sprintf(`%s: %q after %s/%s is not a GitHub topic kind (%s), and a dot in a repository name is written "_"`,
			topic, g.name[1], g.owner, g.name[0], strings.Join(contracts.GithubTopicKinds, ", "))
	}
}

func unwiredRepositoryWarning(ctx context.Context, d *listenerDeps, topic string, logger *logging.Logger) string {
	parsed, ok := parseGithubRepositoryTopic(topic)
	if !ok {
		return ""
	}
	owner, repo := parsed.owner, parsed.repo
	if strings.ContainsAny(owner, "*>") || strings.ContainsAny(repo, "*>") {
		logger.Warn("listener ignored GitHub repository topic with wildcard segment", slog.String("topic", topic))
		return ""
	}
	inspector := d.streamInspector()
	streamName := d.streamName
	if streamName == "" {
		streamName = bus.Stream
	}
	subjectsFilter := "notifications.github." + owner + "." + repo + ".>"
	info, err := streamInfoWithin(ctx, inspector, streamName, subjectsFilter)
	if err != nil {
		logger.Warn("listener stream info failed",
			slog.String("stream", streamName),
			slog.String("subjects_filter", subjectsFilter),
			slog.String("error", err.Error()))
		return fmt.Sprintf("could not verify GitHub wiring for %s/%s: %v", owner, repo, err)
	}
	if info == nil || len(info.State.Subjects) == 0 {
		return fmt.Sprintf("no GitHub event for %s/%s in the stream's retention window; is the App installed there?", owner, repo)
	}
	return ""
}

// subscribeHandler answers POST /v1/interests/subscribe. Its store work - the interest record's
// read and write (store.Registry.PlanInterest, then UpsertPlanned), the session entry's write and
// the unwired-repository checks - runs within sendRequestBudget of the request's own context, so a
// bus that does not answer is the bus-unavailable 503 with Retry-After: 1 inside the caller's
// attempt (writeStoreError); a refused key is still its 413 or 400.
func subscribeHandler(d *listenerDeps, machineID string, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var body subscribeBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		body.SessionID = strings.TrimSpace(body.SessionID)
		if body.SessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		// A session on a serving port is reached at the host it records (session.Deliverer), so a
		// host that could change the delivery URL's authority or path is refused before anything is
		// written. An absent host is an older plugin's, and a registration with no port is never
		// dialled, so neither is checked.
		if body.Port > 0 && body.Host != "" && !session.ValidHost(body.Host) {
			writeJSONError(w, http.StatusBadRequest, "host is not a host name or address", "host")
			return
		}
		logger.Info("listener subscribe", slog.String("session_id", body.SessionID), slog.Any("topics", body.Topics), slog.Int("port", body.Port), slog.Bool("self_subscribed", body.SelfSubscribed), slog.String("dir", body.Dir), slog.String("plugin", body.Plugin), slog.String("plugin_version", body.PluginVersion))
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		// One read and one merge decide this registration's topics, which are both the row it
		// stores and the filter of the session's own consumer (store.Registry.PlanInterest). The
		// roles the session holds are read for the topics the body names and for the row it
		// already holds together, since either may name its own project's controller notice lane
		// (notifications.legion.<project>.controller, the daemon's notify.ControllerTopic),
		// which its holder registers because a Legion controller receives that lane from its own
		// consumer now and from no core subscription of its own (INFRA-771): a heartbeat whose
		// body leaves the lane out therefore keeps it in the filter and in the row alike, and
		// the write below stores this plan rather than deciding it a second time
		// (store.Registry.UpsertPlanned).
		//
		// The roles are read only when one of the two names such a lane, so no other
		// registration is refused while that view cannot answer (store.ErrRoleViewNotReady), and
		// one that does is refused the bus-unavailable 503 rather than stored without the lane:
		// the stored topics are the consumer's filter, and a notice published while the lane is
		// out of it reaches the controller on no later path. The row's own read is the registry
		// cache's on every heartbeat, and one that fails refuses the registration as the write
		// it precedes would.
		requested := append(slices.Clone(body.Topics), contracts.AgentSubject(body.SessionID))
		plan, err := d.registry.PlanInterest(ctx, body.SessionID, requested)
		if err != nil {
			logger.Warn("listener could not plan the session's interest row",
				slog.String("session_id", body.SessionID), slog.String("error", err.Error()))
			writeStoreError(ctx, w, err, http.StatusServiceUnavailable)
			return
		}
		// A topic reaching a private stream subject that is not this session's (another session's
		// inbox, a failed-delivery lane, another project's controller notice lane;
		// bus.ReachesPrivateStreamSubject) is out of the plan (store.MergedTopics) and named in a
		// warning here, the rest of the registration stored: the topics stored drive the
		// listener's fan-out to serve-port sessions (`store.Registry.Match`) and the filters of
		// the session's own consumer (`sessionConsumerFilter`), and such a topic would hand this
		// session messages meant for another session or for another project's controller. A
		// registration naming one is not refused whole, since a released pi-envoy registers every
		// subject it has open, a fork parent's agent subject and any topic its envoy_subscribe
		// took included. The plan drops such a topic from the stored row too, so a row an earlier
		// build stored with one, and the lane of a session that has since lost the controller
		// role, are cleaned by this registration.
		warnings := make([]string, 0)
		for _, topic := range requested {
			if bus.ReachesPrivateStreamSubject(body.SessionID, topic, plan.Roles) {
				warnings = append(warnings, fmt.Sprintf("%s was not registered: it reaches a private stream subject (another session's inbox, a failed-delivery lane, or a Legion controller notice lane), and of those a session registers only its own agent subject, %s, and the notice lane of a project whose controller role it holds",
					topic, contracts.AgentSubject(body.SessionID)))
				continue
			}
			// A topic that would deliver to no session (bus.ReachesNoSession) is refused by name
			// with the whole registration: such an interest would be stored and deliver nothing,
			// and the plugin that asked for it would never learn why. It is the registration and
			// not the topic alone, unlike a private-subject topic above, because no live caller
			// names one: a census of production on 2026-10-30 read all 596 interest rows and
			// found every uncaptured non-role topic in use to be a notifications.team.* topic,
			// which this build stores.
			if bus.ReachesNoSession(topic) {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("no stream subject captures %s, so nothing would be delivered to %s on it; follow a notifications.team.<name> topic instead",
					topic, body.SessionID), "topics")
				return
			}
		}
		if len(warnings) > 0 {
			logger.Warn("listener subscribe refused topics reaching private stream subjects",
				slog.String("session_id", body.SessionID), slog.Any("refused", warnings))
		}
		// The delivery decision comes before both writes: a session is delivered to through its
		// own consumer alone, so a registration this listener cannot give one is refused and must
		// leave no interest row and no session entry behind - the row would make the listener
		// deliver to a session nothing reaches, and the entry would say a session is here that
		// receives nothing. The consumer is created, or has its filter set to the topics about to
		// be stored, before the registration answers, so a message published on a newly followed
		// stored topic after the answer is the consumer's to deliver.
		delivery, refusal := d.consumers.register(r.Context(), body, plan.Topics)
		if refusal != nil {
			logger.Warn("listener refused a registration it can give no session consumer",
				slog.String("session_id", body.SessionID), slog.String("reason", refusal.Reason), slog.Int("status", refusal.status))
			writeJSON(w, refusal.status, refusal)
			return
		}
		// The write stores the plan at the revision the plan read, and plans again when the
		// bucket refuses it (store.Registry.UpsertPlanned): the consumer work above takes up to
		// 5 s per bus call, and an unsubscribe landing in that window would otherwise have every
		// topic it removed written back by this plan.
		item, err := d.registry.UpsertPlanned(ctx, store.Interest{
			SessionID: body.SessionID,
			MachineID: machineID,
			Dir:       body.Dir,
		}, plan)
		if err != nil {
			logger.Warn("listener upsert failed",
				slog.String("session_id", body.SessionID),
				slog.Any("topics", body.Topics),
				slog.String("error", err.Error()))
			writeStoreError(ctx, w, err, http.StatusServiceUnavailable)
			return
		}
		// Every registration answered writes the session's entry. All but one holds the session's
		// consumer and is relayed (sessionDelivery.register); the exception is a serving-port
		// session this listener could give none, which its port is pushed to instead.
		epochChanged := false
		if delivery.relayed() {
			previous, err := d.sessions.Get(body.SessionID)
			epochChanged = err != nil || previous.Delivery != session.DeliveryConsumer || previous.Epoch != delivery.epoch
		}
		if err := d.sessions.PutWithin(ctx, body.SessionID, sessionEntryFromSubscribe(body, machineID, delivery)); err != nil {
			logger.Error("listener session registry put failed", slog.String("session_id", body.SessionID), slog.String("error", err.Error()))
			writeStoreError(ctx, w, fmt.Errorf("session registry unavailable: %w", err), http.StatusServiceUnavailable)
			return
		}
		if delivery.relayed() {
			d.consumers.relay.registered(body.SessionID, epochChanged)
		}
		warningCtx, cancelWarnings := context.WithTimeout(ctx, unwiredRepositoryWarningTimeout)
		defer cancelWarnings()
		warnedRepositories := make(map[string]struct{})
		for _, topic := range body.Topics {
			parsed, ok := parseGithubRepositoryTopic(topic)
			if !ok {
				continue
			}
			if warning := parsed.warning(topic); warning != "" {
				warnings = append(warnings, warning)
				continue
			}
			key := parsed.owner + "\x00" + parsed.repo
			if _, seen := warnedRepositories[key]; seen {
				continue
			}
			warnedRepositories[key] = struct{}{}
			if warning := unwiredRepositoryWarning(warningCtx, d, topic, logger); warning != "" {
				warnings = append(warnings, warning)
			}
		}
		writeJSON(w, http.StatusOK, subscribeResponse{Interest: item, Warnings: warnings, Delivery: delivery.answer})
	}
}

// registerV1Routes builds the /v1 routes over the listener's dependencies, which main hands over
// only once every store is open, so no handler can see a store that is not.
func registerV1Routes(v1 *http.ServeMux, d *listenerDeps, machineID string, logger *logging.Logger) {
	v1.HandleFunc("/v1/interests/subscribe", subscribeHandler(d, machineID, logger))
	v1.HandleFunc("/v1/interests/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var body struct {
			SessionID string   `json:"session_id"`
			Topics    []string `json:"topics"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		body.SessionID = strings.TrimSpace(body.SessionID)
		if body.SessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		logger.Info("listener unsubscribe", slog.String("session_id", body.SessionID), slog.Any("topics", body.Topics))
		// The removal runs within sendRequestBudget of the request's own context, as subscribe's
		// write does, and store work that did not answer is the bus-unavailable 503.
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
		if err := d.registry.Remove(ctx, body.SessionID, body.Topics); err != nil {
			writeStoreError(ctx, w, err, http.StatusInternalServerError)
			return
		}
		// A consumer-delivery session's consumer stops filtering on the topics just removed.
		d.consumers.refollow(r.Context(), d, body.SessionID)
		removed := body.Topics
		if removed == nil {
			removed = []string{}
		}
		writeJSON(w, http.StatusOK, map[string][]string{"removed": removed})
	})
	v1.HandleFunc("/v1/roles/set", roleSetHandler(d, machineID, logger))
	v1.HandleFunc("/v1/roles/", roleGetHandler(d))
	v1.HandleFunc("/v1/registry/", sessionEntryHandler(d))

	v1.Handle("/v1/interests/", adminInterestsHandler(d.registry, d.sessions, func(ctx context.Context, sessionID string) {
		d.consumers.refollow(ctx, d, sessionID)
	}))
	v1.Handle("/v1/sessions", sessionsHandler(d.registry, d.sessions))
	v1.Handle("/v1/sessions/", deleteSessionHandler(d.sessions))
	v1.HandleFunc("/v1/messages/send", sendHandler(d, logger))
	v1.HandleFunc("/v1/messages/publish", publishHandler(d, logger))
	v1.HandleFunc("/v1", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
	v1.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
}
