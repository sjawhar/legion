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

func writeJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeJSONError(w http.ResponseWriter, status int, message string, expected ...string) {
	writeJSON(w, status, apiError{Error: message, Expected: expected})
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

func liveRoleHolder(registry *store.Registry, sessions *session.SessionRegistry, role string) (string, session.SessionEntry, error) {
	if registry == nil || sessions == nil {
		return "", session.SessionEntry{}, fmt.Errorf("service starting")
	}
	holder, err := registry.RoleHolder(role)
	if err != nil {
		return "", session.SessionEntry{}, err
	}
	if holder == "" {
		return "", session.SessionEntry{}, nil
	}
	entry, err := sessions.Get(holder)
	if err != nil {
		return "", session.SessionEntry{}, nil
	}
	return holder, entry, nil
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
		if !isSessionLive(d.sessions, targetSession) {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no live session %s", targetSession))
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
// instead) and publishes the envelope to NATS.
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
		dedupeKey := "publish." + id.New()
		if request.IdempotencyKey != "" {
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
		holder := ""
		if role, ok := strings.CutPrefix(request.Topic, contracts.RoleTopicPrefix); ok {
			var err error
			holder, _, err = liveRoleHolder(d.registry, d.sessions, role)
			if err != nil {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
				return
			}
			if holder == "" {
				writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no holder for role %s", role))
				return
			}
		}
		item.Sender = senderStamp(d.registry, d.sessions, item.SourceSession)
		if err := d.client.Publish(item); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, publishResponse{Envelope: item, Holder: holder})
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
		item, err := d.registry.SetRole(body.SessionID, machineID, body.Role)
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
		holder, entry, err := liveRoleHolder(d.registry, d.sessions, role)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if holder == "" {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no holder for role %s", role))
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"role":      role,
			"holder":    holder,
			"last_seen": entry.UpdatedAt,
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
