package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
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
// refused under its connection's own publish permissions, so one string matches in a test and in a
// deployed log filter. It is deliberately not busUnansweredPrefix: the bus answered this publish,
// and answered it with a denial, so "the message bus did not answer" would be false of it, and the
// operator reading it would go looking for an outage instead of for the grant that forbids the
// subject.
const publishDeniedPrefix = "envoy: the message bus denied the publish under this listener's own permissions"

// writeBusUnavailable answers a send, a publish, a role lookup or a session or interest read the
// bus did not serve, in the one place that writes that 503, so the body's prefix and the hint never
// drift apart. Retry-After is set only where retrying can help: bus.ErrTooLarge and
// bus.ErrInvalidSubject both satisfy errors.Is(err, bus.ErrRefused) - a refusal NATS repeats
// however often the same bytes are sent - and writeNATSError answers those as the 413 and the 400
// they are, so this one guard keeps the header off exactly those two and off nothing else. The
// status writeNATSError settled on is passed back for the record's sake, as that function's own
// comment explains.
//
// A publish the server denied (bus.ErrPublishDenied) is answered ahead of both, because it is
// neither. bus.ErrRefused has exactly three members (internal/bus/nats.go): bus.ErrTooLarge,
// bus.ErrInvalidSubject and bus.ErrInvalidKey, each of them this client's own refusal of the
// caller's bytes, decided before anything is sent. A denial is the opposite: the publish reached a
// server that answered `-ERR 'Permissions Violation for Publish to "<subject>"'` and dropped it,
// which internal/bus/confirm.go names bus.ErrPublishDenied around nats.ErrPermissionViolation and
// the server's own text. So it is not an outage and not a malformed request, and retrying it can
// never succeed: the grant this listener connects with forbids the subject until an operator
// changes it. 403 says exactly that - the request was understood and the server refuses to
// authorise it - where 400 would blame the caller's bytes, which are fine, and where any 5xx would
// be retried: packages/envoy-client/src/transport.ts retries every status at or past 500 and no
// status below it. No Retry-After rides it for the same reason.
func writeBusUnavailable(w http.ResponseWriter, err error) int {
	if errors.Is(err, bus.ErrPublishDenied) {
		writeJSONError(w, http.StatusForbidden, fmt.Sprintf("%s: %s", publishDeniedPrefix, err))
		return http.StatusForbidden
	}
	if !errors.Is(err, bus.ErrRefused) {
		w.Header().Set("Retry-After", "1")
	}
	return writeNATSError(w, err, http.StatusServiceUnavailable, fmt.Sprintf("%s: %s", busUnansweredPrefix, err))
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

func roleNames(topics []string) []string {
	roles := make([]string, 0)
	for _, topic := range topics {
		if role, ok := strings.CutPrefix(topic, contracts.RoleTopicPrefix); ok && role != "" {
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return roles
}

// senderStamp is the sender a send or a publish carries: the session, the machine and directory it
// runs in and the roles it subscribes to (its interest record), and its title (its session entry).
// It only enriches the envelope, so no read it makes may cost the message. The interest record is
// read from the cache, and a miss - a sender that subscribed to nothing, or any sender while the
// cache refills after a reconnect - is read through to the bucket within senderStampReadTimeout of
// ctx, the route's own (store.Registry.Get bounds that read alone). A read that does not answer
// leaves the stamp with what is already known, the session id and the title the session cache
// holds, and leaves the rest of the budget to the publish that follows, which meets the same silent
// bus itself and is answered as the named 503.
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
		sender.Roles = roleNames(interest.Topics)
	}
	if entry, err := sessions.Get(sourceSession); err == nil {
		sender.Title = entry.Title
	}
	return sender
}

// sendRequestBudget is the bus work a send, a publish or a role lookup does under the request's own
// context: the send route's read of the target session, the publish route's resolution of a role
// lane's holder (resolveLiveRoleHolder, every attempt of it), both routes' read of the sender's
// interest record (senderStamp) and both routes' publish share it, and GET /v1/roles/<role>
// resolves its holder within it too. GET /v1/sessions, GET /v1/registry/<session_id> and GET
// /v1/interests/<session_id> read interest records within it as well. The HTTP client gives each
// attempt 5 s (packages/envoy-client/src/transport.ts) and this listener's HTTP write timeout is
// 10 s (main.go), so answering inside 3.5 s leaves the rest for TLS, scheduling and the body: a
// sender whose bus cannot be reached reads the 503 that names the reason, in time to act on it,
// instead of the timeout it would otherwise retry blind.
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
// not answer earns the 503 that asks the sender to try the same route again. Every one of those
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
		var itemPayload *string
		if item.Payload != "" {
			itemPayload = &item.Payload
		}
		mode, deliveryErr := frameDeliveryMode(itemPayload)
		if deliveryErr != nil {
			record.refuse(w, http.StatusForbidden, outcomeCapabilityRefused, fmt.Sprintf(
				"session %s (%s) sent a delivery mode that cannot be read unambiguously", targetSession, target.Title,
			))
			return
		}
		if mode != "" && !hasCapability(target.Capabilities, mode) {
			record.refuse(w, http.StatusForbidden, outcomeCapabilityRefused, fmt.Sprintf(
				"session %s (%s) does not advertise %s", targetSession, target.Title, mode,
			))
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

// deleteSessionHandler removes a live session registration at shutdown.
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
		if err := sessions.Delete(sessionID); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
			writeNATSError(w, err, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// publishHandler rejects agent-targeted topics (must use /v1/messages/send
// instead) and publishes the envelope to NATS. An explicit dedupe_key is used
// verbatim (a re-send a receiver's own dedupe recognises); it is mutually
// exclusive with idempotency_key, may not begin with roleForwardDedupePrefix,
// the mark the role arbiter drops on sight, and may not ride a dispatch
// envelope. Every host drops a repeat of any dispatch key
// (contracts.DedupeKeyNamesTheUpstreamEvent), and Dispatch's outbox keys are
// sequential (dispatch-<event id>), so a caller that chose one could make a
// host drop the real event it names. Dispatch itself never needs this route's
// key: its outbox publishes to the bus directly and its sends go through
// /v1/messages/send, whose key is the listener's. Like the send route, it answers through record,
// which writes this route's one line per request with the answer.
func publishHandler(d *listenerDeps, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		record := &requestRecord{logger: logger, line: "listener publish", laneKey: "topic"}
		if r.Method != http.MethodPost {
			record.refuse(w, http.StatusMethodNotAllowed, outcomeInvalidRequest, "method not allowed")
			return
		}
		// Every bus call below - the role-holder resolution of a role lane and the publish - takes
		// this one deadline, derived from the caller's own context, so this route too answers a bus
		// it cannot reach rather than spending the caller's attempt: a resolution that runs it out
		// is the 503 below, and one that leaves little of it leaves the publish only that little.
		ctx, cancel := context.WithTimeout(r.Context(), sendRequestBudget)
		defer cancel()
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
		result := roleHolderResult{state: roleHolderLive}
		if role, ok := strings.CutPrefix(request.Topic, contracts.RoleTopicPrefix); ok {
			var err error
			result, err = resolveLiveRoleHolder(ctx, d.registry, d.sessions, role)
			if errors.Is(err, errRoleHolderChanged) {
				// The claim moved while it was being resolved: the role has a holder, this
				// publish simply never got a settled one to forward to. Retrying is right, so
				// the 503 and the Retry-After stand, but the answer says the race rather than
				// claiming the bus went quiet.
				record.refuseRoleHolderChanged(w, role)
				return
			}
			if errors.Is(err, store.ErrMalformedRoleClaim) {
				// The bus answered with a claim record no read can decode, and will answer the same
				// record again, so this is neither the bus going quiet nor a retry's to fix.
				record.refuseRoleClaimUnreadable(w, role, err)
				return
			}
			if err != nil {
				// A role holder read that did not answer - within its own cap or within this
				// request's budget - says nothing about the role: the claim is not resolved, so the
				// caller is asked to try again rather than told the role has no live holder.
				record.refuseBusUnavailable(w, err)
				return
			}
			if result.state != roleHolderLive {
				record.refuseRoleHolder(w, role, result)
				return
			}
			// The lane resolved: the session this role topic reaches is on the record from here
			// on, which nothing else in the line names.
			record.holder = result.holder
		}
		item.Sender = senderStamp(ctx, d.registry, d.sessions, item.SourceSession)
		if err := d.client.PublishWithin(ctx, item); err != nil {
			record.refuseBusUnavailable(w, err)
			return
		}
		record.accept(w, publishResponse{Envelope: item, Holder: result.holder})
	}
}

// sessionInfo is the joined view of an Interest (topics, dir, machine) and a
// SessionEntry (port). Returned by GET /v1/sessions.
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
}

// sessionInterestReadTimeout bounds sessionInfoFor's read of one session's interest record from
// the bucket, which a cache miss makes (store.Registry.Get bounds that read alone): a bucket that
// does not answer is reported a second in, rather than when the route's sendRequestBudget ends,
// and GET /v1/registry/<session_id>, whose session read may already have spent
// roleHolderReadTimeout (2 s), still answers inside that budget.
const sessionInterestReadTimeout = 1 * time.Second

// sessionInfoFor joins one session entry with its interests (its topics, and the roles those topics
// name), so a row of GET /v1/sessions and the single answer of GET /v1/registry/<session_id> are
// one shape built one way. A session the interest registry holds no key for keeps its empty topics
// and roles: a session that registered and subscribed to nothing is not an error here. Any other
// error is a read that did not answer within sessionInterestReadTimeout of ctx, and is returned:
// a caller answers it as the bus-unavailable 503, never as a session with no topics.
func sessionInfoFor(ctx context.Context, registry *store.Registry, sessionID string, entry session.SessionEntry) (sessionInfo, error) {
	info := sessionInfo{
		SessionID:      sessionID,
		MachineID:      entry.MachineID,
		Host:           entry.HostOrMachine(),
		Dir:            entry.Dir,
		Port:           entry.Port,
		Title:          entry.Title,
		Capabilities:   append([]string{}, entry.Capabilities...),
		SelfSubscribed: entry.SelfSubscribed,
		UpdatedAt:      entry.UpdatedAt,
		LastSeen:       entry.UpdatedAt,
	}
	readCtx, cancel := context.WithTimeout(ctx, sessionInterestReadTimeout)
	defer cancel()
	interest, err := registry.Get(readCtx, sessionID)
	switch {
	case errors.Is(err, nats.ErrKeyNotFound):
		return info, nil
	case err != nil:
		return sessionInfo{}, err
	}
	info.Topics = interest.Topics
	info.Roles = roleNames(interest.Topics)
	return info, nil
}

// sessionsHandler returns all live sessions by iterating the session registry
// (envoy_sessions, 5-min TTL — only live sessions) and enriching each with
// topic data from the interests registry (envoy_interests). Every interest read shares one
// sendRequestBudget deadline, so however many rows miss the interest cache the list answers
// inside it, and the first read that does not answer ends the request with the bus-unavailable
// 503.
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
			if err != nil {
				writeBusUnavailable(w, err)
				return
			}
			result = append(result, info)
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// sessionEntryHandler answers GET /v1/registry/<session_id>, the one question a caller holding a
// session id asks before it addresses that session: is this session live, and how is it named? The
// read goes through roleHolderSession, so a session this listener's cache does not hold yet — the
// session bucket was recreated and the watcher is still refilling it, or another listener
// registered the session a moment ago — is read from the bucket itself instead of being reported
// gone. Only the bucket's word that the key is absent is a 404, and its wording is load-bearing:
// Dispatch classifies a delivery failure on the "no live session " prefix, as the send route's own
// 404 writes it. An id NATS refuses as a key is the caller's to fix, and a read that did not answer
// — the session's, or its interest record's within sendRequestBudget (sessionInfoFor) — is a 503 to
// retry: never a session a caller may take for ended, nor one with no topics.
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
		entry, err := roleHolderSession(d.sessions, sessionID, registered)
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
			if err != nil {
				writeBusUnavailable(w, err)
				return
			}
			writeJSON(w, http.StatusOK, info)
		}
	}
}

// adminInterestsHandler lists, reads and removes persisted interests. GET /v1/interests/<session_id>
// reads within sendRequestBudget of the request's own context: a cache miss is read through to the
// bucket, the bucket's word that the key is absent is a 404, and a read that did not answer is the
// bus-unavailable 503 with Retry-After: 1 (a key NATS refuses is still the 413 or 400
// writeBusUnavailable gives it).
func adminInterestsHandler(registry *store.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/interests/")

		if sessionID == "" {
			if r.Method != http.MethodGet {
				writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
				return
			}
			writeJSON(w, http.StatusOK, registry.List())
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
			if err != nil {
				writeBusUnavailable(w, err)
				return
			}
			writeJSON(w, http.StatusOK, item)
		case http.MethodDelete:
			if err := registry.Remove(sessionID, nil); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
				writeNATSError(w, err, http.StatusInternalServerError, err.Error())
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func roleSetHandler(d *listenerDeps, machineID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var body struct {
			SessionID string `json:"session_id"`
			Role      string `json:"role"`
			// A soft claim takes the role only when it is unheld, its holder is
			// no longer live, or its holder is the claimant's declared
			// predecessor; any other live holder answers 409 with its id. This
			// is how a resumed or forked session recovers its own role without
			// ever taking one from a peer. Default false: last-claim-wins.
			Soft bool `json:"soft"`
			// The session id this claimant is continuing (a /fork, /branch, or
			// /handoff mints a new id in the same process). That predecessor is
			// still heartbeating and therefore live, yet it is the same
			// conversation, so a soft claim may take the role from it. Only
			// honoured with soft; ignored otherwise.
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
		entry, err := d.sessions.Get(body.SessionID)
		if errors.Is(err, nats.ErrKeyNotFound) {
			writeJSONError(w, http.StatusNotFound, "session is not registered")
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusServiceUnavailable, "read session registration: "+err.Error())
			return
		}
		entry.MachineID = machineID
		entry.SelfSubscribed = true
		if err := d.sessions.Put(body.SessionID, entry); err != nil {
			writeNATSError(w, err, http.StatusServiceUnavailable, "refresh role claimant registration: "+err.Error())
			return
		}
		previous := ""
		var supersedable []string
		if body.Soft {
			// The registry sees only interest rows; liveness is the session
			// bucket's call (roleHolderSession). A holder whose session entry
			// has aged out (5m TTL) is dead and may be superseded; so may the
			// claimant's own predecessor, live or not; any other live holder is
			// protected.
			holder, err := d.registry.RoleHolder(body.Role)
			if err != nil {
				writeNATSError(w, err, http.StatusServiceUnavailable, "read role holder: "+err.Error())
				return
			}
			previous = strings.TrimSpace(body.PreviousSessionID)
			if holder != "" && holder != body.SessionID {
				if previous != "" && holder == previous {
					supersedable = append(supersedable, holder)
				} else if _, liveErr := roleHolderSession(d.sessions, holder, registered); errors.Is(liveErr, nats.ErrKeyNotFound) {
					supersedable = append(supersedable, holder)
				} else if liveErr != nil {
					writeJSONError(w, http.StatusServiceUnavailable, "read role holder liveness: "+liveErr.Error())
					return
				}
			}
		}
		item, err := d.registry.SetRoleWithPrevious(body.SessionID, machineID, body.Role, previous, body.Soft, supersedable...)
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
			writeNATSError(w, err, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, item)
	}
}

// roleGetHandler answers GET /v1/roles/<role> with the role's live holder. It resolves the holder
// within sendRequestBudget of the request's own context, as a role-lane publish does, so a bus that
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
		result, err := resolveLiveRoleHolder(ctx, d.registry, d.sessions, role)
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
		logger.Info("listener subscribe", slog.String("session_id", body.SessionID), slog.Any("topics", body.Topics), slog.Int("port", body.Port), slog.Bool("self_subscribed", body.SelfSubscribed), slog.String("dir", body.Dir))
		item, err := d.registry.Upsert(store.Interest{
			SessionID: body.SessionID,
			MachineID: machineID,
			Dir:       body.Dir,
		}, append(body.Topics, contracts.AgentSubject(body.SessionID)))
		if err != nil {
			logger.Warn("listener upsert failed",
				slog.String("session_id", body.SessionID),
				slog.Any("topics", body.Topics),
				slog.String("error", err.Error()))
			writeNATSError(w, err, http.StatusServiceUnavailable, err.Error())
			return
		}
		if body.Port > 0 || body.SelfSubscribed {
			if err := d.sessions.Put(body.SessionID, sessionEntryFromSubscribe(body, machineID)); err != nil {
				logger.Error("listener session registry put failed", slog.String("session_id", body.SessionID), slog.String("error", err.Error()))
				writeNATSError(w, err, http.StatusServiceUnavailable, "session registry unavailable")
				return
			}
		}
		warningCtx, cancelWarnings := context.WithTimeout(r.Context(), unwiredRepositoryWarningTimeout)
		defer cancelWarnings()
		warnings := make([]string, 0)
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
		writeJSON(w, http.StatusOK, subscribeResponse{Interest: item, Warnings: warnings})
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
		if err := d.registry.Remove(body.SessionID, body.Topics); err != nil {
			writeNATSError(w, err, http.StatusInternalServerError, err.Error())
			return
		}
		removed := body.Topics
		if removed == nil {
			removed = []string{}
		}
		writeJSON(w, http.StatusOK, map[string][]string{"removed": removed})
	})
	v1.HandleFunc("/v1/roles/set", roleSetHandler(d, machineID))
	v1.HandleFunc("/v1/roles/", roleGetHandler(d))
	v1.HandleFunc("/v1/registry/", sessionEntryHandler(d))

	v1.Handle("/v1/interests/", adminInterestsHandler(d.registry))
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
