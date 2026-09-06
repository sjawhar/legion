package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/nats-io/nats.go"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/logging"
	"github.com/sjawhar/envoy/internal/session"
	"github.com/sjawhar/envoy/internal/store"
)

const rolePatternString = `^[a-z0-9][a-z0-9_-]*$`

var rolePattern = regexp.MustCompile(rolePatternString)

func isValidRole(role string) bool {
	return rolePattern.MatchString(role)
}

func messageSource(value string) (string, error) {
	if value == "" {
		return "agent", nil
	}
	if value == "agent" || value == "human" {
		return value, nil
	}
	return "", fmt.Errorf("source must be one of: agent, human")
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// sendHandler publishes a direct message only while the target session has a
// live registry entry.
func sendHandler(state *atomic.Pointer[listenerDeps]) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Source         string `json:"source"`
			SourceSession  string `json:"source_session"`
			TargetSession  string `json:"target_session"`
			Message        string `json:"message"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		source, err := messageSource(body.Source)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := state.Load()
		if !isSessionLive(d.sessions, body.TargetSession) {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("no live session %s", body.TargetSession))
			return
		}
		dedupeKey := "agent." + body.TargetSession + "." + id.New()
		if body.IdempotencyKey != "" {
			dedupeKey = "agent." + body.TargetSession + "." + body.IdempotencyKey
		}
		item := contracts.Envelope{
			EventID:        id.New(),
			Source:         source,
			SourceSession:  body.SourceSession,
			SourceEventID:  id.New(),
			Topic:          contracts.AgentSubject(body.TargetSession),
			DedupeKey:      dedupeKey,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: body.Message,
			TraceID:        id.New(),
		}
		if err := item.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := d.client.Publish(item); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(item)
	}
}

// deleteSessionHandler removes a live session registration at shutdown.
func deleteSessionHandler(sessions *session.SessionRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
		if sessionID == "" {
			http.Error(w, "session_id required", http.StatusBadRequest)
			return
		}
		if sessions == nil {
			http.Error(w, "session registry unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := sessions.Delete(sessionID); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
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
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Source         string `json:"source"`
			SourceSession  string `json:"source_session"`
			Topic          string `json:"topic"`
			Message        string `json:"message"`
			Payload        string `json:"payload"`
			IdempotencyKey string `json:"idempotency_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		if body.Topic == "" || body.Message == "" {
			http.Error(w, "topic and message are required", http.StatusBadRequest)
			return
		}
		if strings.HasPrefix(body.Topic, contracts.AgentTopicPrefix) {
			http.Error(w, "cannot publish to agent topics; use /v1/messages/send for direct agent messages", http.StatusBadRequest)
			return
		}
		source, err := messageSource(body.Source)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		dedupeKey := "publish." + id.New()
		if body.IdempotencyKey != "" {
			dedupeKey = "publish." + body.IdempotencyKey
		}
		item := contracts.Envelope{
			EventID:        id.New(),
			Source:         source,
			SourceSession:  body.SourceSession,
			SourceEventID:  id.New(),
			Topic:          body.Topic,
			DedupeKey:      dedupeKey,
			IssuedAt:       contracts.NowMillis(),
			PayloadSummary: body.Message,
			Payload:        body.Payload,
			TraceID:        id.New(),
		}
		if err := item.Validate(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		d := state.Load()
		if err := d.client.Publish(item); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(item)
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
	UpdatedAt      int64    `json:"updated_at"`
}

// sessionsHandler returns all live sessions by iterating the session registry
// (envoy_sessions, 5-min TTL — only live sessions) and enriching each with
// topic data from the interests registry (envoy_interests).
func sessionsHandler(registry *store.Registry, sessions *session.SessionRegistry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if sessions == nil {
			http.Error(w, "session registry unavailable", http.StatusServiceUnavailable)
			return
		}
		entries, err := sessions.List()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		result := make([]sessionInfo, 0, len(entries))
		for _, entry := range entries {
			info := sessionInfo{
				SessionID:      entry.SessionID,
				MachineID:      entry.MachineID,
				Dir:            entry.Dir,
				Port:           entry.Port,
				Title:          entry.Title,
				SelfSubscribed: entry.SelfSubscribed,
				UpdatedAt:      entry.UpdatedAt,
			}
			if registry != nil {
				if interest, err := registry.Get(entry.SessionID); err == nil {
					info.Topics = interest.Topics
				}
			}
			result = append(result, info)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}

func adminInterestsHandler(registry *store.Registry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/interests/")

		if sessionID == "" {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			items := registry.List()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(items)
			return
		}

		switch r.Method {
		case http.MethodGet:
			item, err := registry.Get(sessionID)
			if err != nil {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(item)
		case http.MethodDelete:
			if err := registry.Remove(sessionID, nil); err != nil {
				if !errors.Is(err, nats.ErrKeyNotFound) {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func roleSetHandler(state *atomic.Pointer[listenerDeps], machineID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID string `json:"session_id"`
			Role      string `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		body.SessionID = strings.TrimSpace(body.SessionID)
		body.Role = strings.TrimSpace(body.Role)
		if body.SessionID == "" {
			http.Error(w, "session_id is required", http.StatusBadRequest)
			return
		}
		if body.Role == "" {
			http.Error(w, "role is required", http.StatusBadRequest)
			return
		}
		if !isValidRole(body.Role) {
			http.Error(w, "role must match "+rolePatternString, http.StatusBadRequest)
			return
		}
		d := state.Load()
		if d == nil || d.registry == nil || d.sessions == nil {
			http.Error(w, "service starting", http.StatusServiceUnavailable)
			return
		}
		entry, err := d.sessions.Get(body.SessionID)
		if errors.Is(err, nats.ErrKeyNotFound) {
			http.Error(w, "session is not registered", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "read session registration: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		entry.MachineID = machineID
		entry.SelfSubscribed = true
		if err := d.sessions.Put(body.SessionID, entry); err != nil {
			http.Error(w, "refresh role claimant registration: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		item, err := d.registry.SetRole(body.SessionID, machineID, body.Role)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(item)
	}
}

func subscribeHandler(state *atomic.Pointer[listenerDeps], machineID string, logger *logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body subscribeBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		logger.Info("listener subscribe", slog.String("session_id", body.SessionID), slog.Any("topics", body.Topics), slog.Int("port", body.Port), slog.Bool("self_subscribed", body.SelfSubscribed), slog.String("dir", body.Dir))
		d := state.Load()
		item, err := d.registry.Upsert(store.Interest{
			SessionID: body.SessionID,
			MachineID: machineID,
			Dir:       body.Dir,
		}, append(body.Topics, contracts.AgentSubject(body.SessionID)))
		if err != nil {
			// WARN, not Error: this is a transient condition (typically a NATS/KV
			// hiccup during heartbeat) and the plugin's next 2-min heartbeat will
			// retry. Logged at WARN so subscription-drop incidents are greppable
			// in one shot instead of requiring 2-hour log forensics.
			logger.Warn("listener upsert failed",
				slog.String("session_id", body.SessionID),
				slog.Any("topics", body.Topics),
				slog.String("error", err.Error()))
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		if body.Port > 0 || body.SelfSubscribed {
			if err := d.sessions.Put(body.SessionID, sessionEntryFromSubscribe(body, machineID)); err != nil {
				logger.Error("listener session registry put failed", slog.String("session_id", body.SessionID), slog.String("error", err.Error()))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(item)
	}
}

func registerV1Routes(v1 *http.ServeMux, deps *atomic.Pointer[listenerDeps], machineID string, logger *logging.Logger) {
	v1.HandleFunc("/v1/interests/subscribe", subscribeHandler(deps, machineID, logger))
	v1.HandleFunc("/v1/interests/unsubscribe", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			SessionID string   `json:"session_id"`
			Topics    []string `json:"topics"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		logger.Info("listener unsubscribe", slog.String("session_id", body.SessionID), slog.Any("topics", body.Topics))
		d := deps.Load()
		if err := d.registry.Remove(body.SessionID, body.Topics); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	v1.HandleFunc("/v1/roles/set", roleSetHandler(deps, machineID))
	v1.HandleFunc("/v1/registry/", func(w http.ResponseWriter, r *http.Request) {
		sessionID := strings.TrimPrefix(r.URL.Path, "/v1/registry/")
		if sessionID == "" {
			http.Error(w, "session_id required", http.StatusBadRequest)
			return
		}
		d := deps.Load()
		entry, err := d.sessions.Get(sessionID)
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(entry)
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
		deleteSessionHandler(deps.Load().sessions).ServeHTTP(w, r)
	})
	v1.HandleFunc("/v1/messages/send", sendHandler(deps))
	v1.HandleFunc("/v1/messages/publish", publishHandler(deps))
}
