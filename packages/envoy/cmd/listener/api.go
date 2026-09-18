package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
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

type roleClaimResolver interface {
	RoleClaim(role string) (store.RoleClaim, error)
	ReleaseExpiredRoleClaim(role, sessionID string, sessionTTL time.Duration) (store.ExpiredRoleClaimRelease, error)
}

type apiError struct {
	Error    string   `json:"error"`
	Expected []string `json:"expected,omitempty"`
}

type roleHolderState string

const (
	roleHolderLive      roleHolderState = "live"
	roleHolderUnclaimed roleHolderState = "unclaimed"
	roleHolderLapsed    roleHolderState = "holder_lapsed"
)

type roleHolderResult struct {
	state             roleHolderState
	holder            string
	entry             session.SessionEntry
	lastSeen          int64
	lastSeenAvailable bool
	claimRelease      store.ExpiredRoleClaimRelease
}

type roleHolderError struct {
	Error         string          `json:"error"`
	Reason        roleHolderState `json:"reason"`
	Holder        string          `json:"holder,omitempty"`
	LastSeen      *int64          `json:"last_seen,omitempty"`
	ClaimReleased *bool           `json:"claim_released,omitempty"`
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

type sendResponse struct {
	contracts.Envelope
	Recipient string `json:"recipient"`
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
// into a Go struct, as this used to do, is a *second, more lenient* reader of the same bytes:
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
//   - "delivery" is present and its mode reads unambiguously as a single non-empty string:
//     mode=<that string>, err=nil. An unrecognised (non-enum) mode string is not a case this
//     function decides: it returns that string like any other, and the ordinary capability
//     check refuses it because no session advertises a bogus capability.
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
	return mode, nil
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

func writeRoleHolderError(w http.ResponseWriter, role string, result roleHolderResult) {
	response := roleHolderError{
		Reason: result.state,
	}
	switch result.state {
	case roleHolderUnclaimed:
		response.Error = fmt.Sprintf("no holder for role %s", role)
	case roleHolderLapsed:
		response.Holder = result.holder
		lastSeen := " (last seen unavailable)"
		if result.lastSeenAvailable {
			response.LastSeen = &result.lastSeen
			lastSeen = fmt.Sprintf(" (last seen %d)", result.lastSeen)
		}
		claimReleased := result.claimRelease == store.ExpiredRoleClaimReleased
		response.ClaimReleased = &claimReleased
		switch result.claimRelease {
		case store.ExpiredRoleClaimReleased:
			response.Error = fmt.Sprintf(
				"role %s holder %s lapsed; claim released%s",
				role,
				result.holder,
				lastSeen,
			)
		case store.ExpiredRoleClaimMissing:
			response.Error = fmt.Sprintf(
				"role %s holder %s lapsed; claim was already absent%s",
				role,
				result.holder,
				lastSeen,
			)
		default:
			response.Error = fmt.Sprintf(
				"role %s holder %s lapsed; claim retained%s",
				role,
				result.holder,
				lastSeen,
			)
		}
	}
	writeJSON(w, http.StatusNotFound, response)
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

func senderStamp(registry *store.Registry, sessions *session.SessionRegistry, sourceSession string) *contracts.EnvelopeSender {
	if sourceSession == "" {
		return nil
	}
	sender := &contracts.EnvelopeSender{SessionID: sourceSession}
	if registry != nil {
		if interest, err := registry.Get(sourceSession); err == nil {
			sender.Machine = interest.MachineID
			sender.Cwd = interest.Dir
			sender.Roles = roleNames(interest.Topics)
		}
	}
	if sessions != nil {
		if entry, err := sessions.Get(sourceSession); err == nil {
			sender.Title = entry.Title
		}
	}
	return sender
}

const roleHolderResolutionAttempts = 2

func liveRoleHolder(registry *store.Registry, sessions *session.SessionRegistry, role string) (roleHolderResult, error) {
	if registry == nil || sessions == nil {
		return roleHolderResult{}, fmt.Errorf("service starting")
	}
	return resolveLiveRoleHolder(registry, sessions, role)
}

// releaseExpiredRoleClaim is the single point in the listener that
// interprets store.ExpiredRoleClaimRelease's ExpiredRoleClaimSuperseded
// outcome. Both resolveLiveRoleHolder (HTTP role lookups) and
// resolveCoreRoleHolder (synchronous role-lane delivery, delivery.go) call
// it instead of registry.ReleaseExpiredRoleClaim directly, so a claim that
// raced a fresh SetRole before the conditional delete landed is reported as
// "must re-resolve" in exactly one place. A second, drifted branch on
// ExpiredRoleClaimSuperseded is exactly how a live replacement holder ends
// up reported as this call's stale verdict — the defect this helper exists
// to make structurally impossible.
func releaseExpiredRoleClaim(registry roleClaimResolver, role, sessionID string, sessionTTL time.Duration) (release store.ExpiredRoleClaimRelease, superseded bool, err error) {
	release, err = registry.ReleaseExpiredRoleClaim(role, sessionID, sessionTTL)
	if err != nil {
		return release, false, err
	}
	return release, release == store.ExpiredRoleClaimSuperseded, nil
}

func resolveLiveRoleHolder(registry roleClaimResolver, sessions *session.SessionRegistry, role string) (roleHolderResult, error) {
	if registry == nil || sessions == nil {
		return roleHolderResult{}, fmt.Errorf("service starting")
	}
	for range roleHolderResolutionAttempts {
		claim, err := registry.RoleClaim(role)
		if err != nil {
			return roleHolderResult{}, err
		}
		if claim.HolderSessionID == "" {
			return roleHolderResult{state: roleHolderUnclaimed}, nil
		}
		entry, err := sessions.Get(claim.HolderSessionID)
		if errors.Is(err, nats.ErrKeyNotFound) {
			lastSeen := sessions.LastSeen(claim.HolderSessionID)
			release, superseded, err := releaseExpiredRoleClaim(registry, role, claim.HolderSessionID, sessions.TTL())
			if err != nil {
				return roleHolderResult{}, fmt.Errorf("drop expired role claim: %w", err)
			}
			if superseded {
				continue
			}
			return roleHolderResult{
				state:             roleHolderLapsed,
				holder:            claim.HolderSessionID,
				lastSeen:          lastSeen,
				lastSeenAvailable: lastSeen != 0,
				claimRelease:      release,
			}, nil
		}
		if err != nil {
			return roleHolderResult{}, fmt.Errorf("read role holder session: %w", err)
		}
		return roleHolderResult{
			state:  roleHolderLive,
			holder: claim.HolderSessionID,
			entry:  entry,
		}, nil
	}
	return roleHolderResult{}, fmt.Errorf("role holder changed while resolving")
}

// sendHandler publishes a direct message only while the target session has a
// live registry entry.
func sendHandler(state *atomic.Pointer[listenerDeps]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var body messageBody
		var targetSession string
		var request struct {
			messageBody
			TargetSession string `json:"target_session"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		body = request.messageBody
		targetSession = strings.TrimSpace(request.TargetSession)
		if targetSession == "" {
			writeJSONError(w, http.StatusBadRequest, "target_session is required", "target_session")
			return
		}
		if strings.TrimSpace(body.Message) == "" {
			writeJSONError(w, http.StatusBadRequest, "message is required", "message")
			return
		}
		if message, field := validateMessageBody(body); message != "" {
			writeJSONError(w, http.StatusBadRequest, message, field)
			return
		}
		dedupeKey := "agent." + targetSession + "." + id.New()
		if body.IdempotencyKey != "" {
			dedupeKey = "agent." + targetSession + "." + body.IdempotencyKey
		}
		item := messageEnvelope(body, contracts.AgentSubject(targetSession), dedupeKey)
		if err := item.Validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		d := state.Load()
		if d.sessions == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "service starting")
			return
		}
		target, err := d.sessions.Get(targetSession)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no live session %s", targetSession))
			return
		}
		mode, deliveryErr := frameDeliveryMode(body.Payload)
		if deliveryErr != nil {
			writeJSONError(w, http.StatusForbidden, fmt.Sprintf(
				"session %s (%s) sent a delivery mode that cannot be read unambiguously", targetSession, target.Title,
			))
			return
		}
		if mode != "" && !hasCapability(target.Capabilities, mode) {
			writeJSONError(w, http.StatusForbidden, fmt.Sprintf(
				"session %s (%s) does not advertise %s", targetSession, target.Title, mode,
			))
			return
		}
		item.Sender = senderStamp(d.registry, d.sessions, item.SourceSession)
		if d.client == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "service starting")
			return
		}
		if err := d.client.Publish(item); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, sendResponse{Envelope: item, Recipient: targetSession})
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
		if sessions == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "session registry unavailable")
			return
		}
		if err := sessions.Delete(sessionID); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// publishHandler rejects agent-targeted topics (must use /v1/messages/send
// instead) and publishes the envelope to NATS. An explicit dedupe_key is used
// verbatim (a re-send a receiver's own dedupe recognises); it is mutually
// exclusive with idempotency_key and may not begin with roleForwardDedupePrefix,
// the mark the role arbiter drops on sight.
func publishHandler(state *atomic.Pointer[listenerDeps]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		var request struct {
			messageBody
			Topic string `json:"topic"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
		if strings.TrimSpace(request.Topic) == "" {
			writeJSONError(w, http.StatusBadRequest, "topic is required", "topic")
			return
		}
		if strings.TrimSpace(request.Message) == "" {
			writeJSONError(w, http.StatusBadRequest, "message is required", "message")
			return
		}
		if strings.HasPrefix(request.Topic, contracts.AgentTopicPrefix) {
			writeJSONError(w, http.StatusBadRequest, "cannot publish to agent topics; use /v1/messages/send for direct agent messages")
			return
		}
		if message, field := validateMessageBody(request.messageBody); message != "" {
			writeJSONError(w, http.StatusBadRequest, message, field)
			return
		}
		if request.DedupeKey != "" && request.IdempotencyKey != "" {
			writeJSONError(w, http.StatusBadRequest, "dedupe_key and idempotency_key are mutually exclusive", "dedupe_key", "idempotency_key")
			return
		}
		if strings.HasPrefix(request.DedupeKey, roleForwardDedupePrefix) {
			writeJSONError(w, http.StatusBadRequest, "dedupe_key must not begin with the reserved prefix "+roleForwardDedupePrefix, "dedupe_key")
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
		if err := item.Validate(); err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		d := state.Load()
		if d.client == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "service starting")
			return
		}
		result := roleHolderResult{state: roleHolderLive}
		if role, ok := strings.CutPrefix(request.Topic, contracts.RoleTopicPrefix); ok {
			var err error
			result, err = liveRoleHolder(d.registry, d.sessions, role)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if result.state != roleHolderLive {
				writeRoleHolderError(w, role, result)
				return
			}
		}
		item.Sender = senderStamp(d.registry, d.sessions, item.SourceSession)
		if err := d.client.Publish(item); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, publishResponse{Envelope: item, Holder: result.holder})
	}
}

// sessionInfo is the joined view of an Interest (topics, dir, machine) and a
// SessionEntry (port). Returned by GET /v1/sessions.
type sessionInfo struct {
	SessionID      string   `json:"session_id"`
	MachineID      string   `json:"machine_id"`
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

// sessionsHandler returns all live sessions by iterating the session registry
// (envoy_sessions, 5-min TTL — only live sessions) and enriching each with
// topic data from the interests registry (envoy_interests).
func sessionsHandler(registry *store.Registry, sessions *session.SessionRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if sessions == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "session registry unavailable")
			return
		}
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
			info := sessionInfo{
				SessionID:      entry.SessionID,
				MachineID:      entry.MachineID,
				Dir:            entry.Dir,
				Port:           entry.Port,
				Title:          entry.Title,
				Capabilities:   append([]string{}, entry.Capabilities...),
				SelfSubscribed: entry.SelfSubscribed,
				UpdatedAt:      entry.UpdatedAt,
				LastSeen:       entry.UpdatedAt,
			}
			if registry != nil {
				if interest, err := registry.Get(entry.SessionID); err == nil {
					info.Topics = interest.Topics
					info.Roles = roleNames(interest.Topics)
				}
			}
			result = append(result, info)
		}
		writeJSON(w, http.StatusOK, result)
	}
}

func adminInterestsHandler(registry *store.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "interest registry unavailable")
			return
		}
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
			item, err := registry.Get(sessionID)
			if err != nil {
				writeJSONError(w, http.StatusNotFound, err.Error())
				return
			}
			writeJSON(w, http.StatusOK, item)
		case http.MethodDelete:
			if err := registry.Remove(sessionID, nil); err != nil && !errors.Is(err, nats.ErrKeyNotFound) {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func roleSetHandler(state *atomic.Pointer[listenerDeps], machineID string) http.HandlerFunc {
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
		d := state.Load()
		if d.registry == nil || d.sessions == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "service starting")
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
			writeJSONError(w, http.StatusServiceUnavailable, "refresh role claimant registration: "+err.Error())
			return
		}
		previous := ""
		var supersedable []string
		if body.Soft {
			// The registry sees only interest rows; liveness is this registry's
			// call. A holder whose session entry has aged out (5m TTL) is dead
			// and may be superseded; so may the claimant's own predecessor,
			// live or not; any other live holder is protected.
			holder, err := d.registry.RoleHolder(body.Role)
			if err != nil {
				writeJSONError(w, http.StatusServiceUnavailable, "read role holder: "+err.Error())
				return
			}
			previous = strings.TrimSpace(body.PreviousSessionID)
			if holder != "" && holder != body.SessionID {
				if previous != "" && holder == previous {
					supersedable = append(supersedable, holder)
				} else if _, liveErr := d.sessions.Get(holder); errors.Is(liveErr, nats.ErrKeyNotFound) {
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
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, item)
	}
}

func roleGetHandler(state *atomic.Pointer[listenerDeps]) http.HandlerFunc {
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
		d := state.Load()
		result, err := liveRoleHolder(d.registry, d.sessions, role)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
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
			"capabilities": append([]string{}, result.entry.Capabilities...),
			"last_seen":    result.entry.UpdatedAt,
		})
	}
}

func (d *listenerDeps) streamInspector() streamInfoLookup {
	if d.streamInfo != nil {
		return d.streamInfo
	}
	if d.client != nil {
		return d.client.JS()
	}
	return nil
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

func githubRepositoryTopic(topic string) (owner, repo string, ok bool) {
	const githubTopicPrefix = "notifications.github."
	remainder, ok := strings.CutPrefix(topic, githubTopicPrefix)
	if !ok {
		return "", "", false
	}
	parts := strings.Split(remainder, ".")
	if len(parts) < 3 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func unwiredRepositoryWarning(ctx context.Context, d *listenerDeps, topic string, logger *logging.Logger) string {
	owner, repo, ok := githubRepositoryTopic(topic)
	if !ok {
		return ""
	}
	if strings.ContainsAny(owner, "*>") || strings.ContainsAny(repo, "*>") {
		logger.Warn("listener ignored GitHub repository topic with wildcard segment", slog.String("topic", topic))
		return ""
	}
	inspector := d.streamInspector()
	if inspector == nil {
		return ""
	}
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

func subscribeHandler(state *atomic.Pointer[listenerDeps], machineID string, logger *logging.Logger) http.HandlerFunc {
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
		d := state.Load()
		if d.registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "service starting")
			return
		}
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
			writeJSONError(w, http.StatusServiceUnavailable, err.Error())
			return
		}
		if body.Port > 0 || body.SelfSubscribed {
			if d.sessions == nil {
				writeJSONError(w, http.StatusServiceUnavailable, "session registry unavailable")
				return
			}
			if err := d.sessions.Put(body.SessionID, sessionEntryFromSubscribe(body, machineID)); err != nil {
				logger.Error("listener session registry put failed", slog.String("session_id", body.SessionID), slog.String("error", err.Error()))
				writeJSONError(w, http.StatusServiceUnavailable, "session registry unavailable")
				return
			}
		}
		warningCtx, cancelWarnings := context.WithTimeout(r.Context(), unwiredRepositoryWarningTimeout)
		defer cancelWarnings()
		warnings := make([]string, 0)
		warnedRepositories := make(map[string]struct{})
		for _, topic := range body.Topics {
			owner, repo, ok := githubRepositoryTopic(topic)
			if !ok {
				continue
			}
			key := owner + "\x00" + repo
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

func registerV1Routes(v1 *http.ServeMux, deps *atomic.Pointer[listenerDeps], machineID string, logger *logging.Logger) {
	v1.HandleFunc("/v1/interests/subscribe", subscribeHandler(deps, machineID, logger))
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
		d := deps.Load()
		if d.registry == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "service starting")
			return
		}
		if err := d.registry.Remove(body.SessionID, body.Topics); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		removed := body.Topics
		if removed == nil {
			removed = []string{}
		}
		writeJSON(w, http.StatusOK, map[string][]string{"removed": removed})
	})
	v1.HandleFunc("/v1/roles/set", roleSetHandler(deps, machineID))
	v1.HandleFunc("/v1/roles/", roleGetHandler(deps))
	v1.HandleFunc("/v1/registry/", func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
		if sessionID == "" {
			writeJSONError(w, http.StatusBadRequest, "session_id is required", "session_id")
			return
		}
		d := deps.Load()
		if d.sessions == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "service starting")
			return
		}
		entry, err := d.sessions.Get(sessionID)
		if err != nil {
			writeJSONError(w, http.StatusNotFound, "not found")
			return
		}
		writeJSON(w, http.StatusOK, entry)
	})

	v1.HandleFunc("/v1/interests/", func(w http.ResponseWriter, r *http.Request) {
		d := deps.Load()
		adminInterestsHandler(d.registry).ServeHTTP(w, r)
	})
	v1.HandleFunc("/v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		d := deps.Load()
		sessionsHandler(d.registry, d.sessions).ServeHTTP(w, r)
	})
	v1.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
		d := deps.Load()
		deleteSessionHandler(d.sessions).ServeHTTP(w, r)
	})
	v1.HandleFunc("/v1/messages/send", sendHandler(deps))
	v1.HandleFunc("/v1/messages/publish", publishHandler(deps))
	v1.HandleFunc("/v1", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
	v1.HandleFunc("/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeJSONError(w, http.StatusNotFound, "not found")
	})
}
