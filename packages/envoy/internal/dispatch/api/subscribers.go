package api

import (
	"errors"
	"net/http"
	"sort"

	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/routing"
)

// subscriber is a session whose persisted Envoy interests include an issue's
// or unlinked document's topic family, enriched with its live registry state
// when the session is currently connected. Removable is false when the only
// matching topic is broader than this owner (e.g. "notifications.dispatch.>"):
// removing that topic would silently unsubscribe the session from every
// other issue and document too, so Unsubscribe refuses it; Via then names
// one such topic for the header to explain why.
type subscriber struct {
	SessionID string   `json:"session_id"`
	Title     string   `json:"title"`
	Live      bool     `json:"live"`
	LastSeen  int64    `json:"last_seen"`
	Topics    []string `json:"topics"`
	Removable bool     `json:"removable"`
	Via       string   `json:"via,omitempty"`
}

// issueSubscriberBase is the bare NATS subject family root a session must be
// interested in (with or without a trailing ".>") to receive an issue's events.
func issueSubscriberBase(key string) string {
	return "notifications.dispatch.issue." + key
}

// documentSubscriberBase is the bare NATS subject family root for an unlinked
// project document's own topic (see model.Artifact: an issue-attached
// artifact has no topic of its own — its events route through the issue).
func documentSubscriberBase(project, slug string) string {
	return "notifications.dispatch.document." + project + "." + slug
}

// matchingTopics returns the subset of topics that would actually deliver an
// event published under base, using the listener's own wildcard semantics
// (routing.Match): a bare subject alone (no ".>") never matches, since NATS
// only delivers a typed event like "<base>.issue.created" to a subscriber of
// "<base>.>" or a broader wildcard, never to a subscriber of "<base>" alone.
func matchingTopics(topics []string, base string) []string {
	candidate := base + ".marker"
	matched := make([]string, 0, len(topics))
	for _, topic := range topics {
		if routing.Match(topic, candidate) {
			matched = append(matched, topic)
		}
	}
	return matched
}

// exactTopics returns the subset of topics scoped exactly to base (base
// itself, or base+".>") — the only forms safe to remove without silently
// unsubscribing the session from every other issue or document a broader
// wildcard among its topics also happens to cover.
func exactTopics(topics []string, base string) []string {
	wildcard := base + ".>"
	exact := make([]string, 0, len(topics))
	for _, topic := range topics {
		if topic == base || topic == wildcard {
			exact = append(exact, topic)
		}
	}
	return exact
}

func (s *server) listIssueSubscribers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	s.writeSubscribers(w, r, issueSubscriberBase(r.PathValue("key")))
}

func (s *server) listArtifactSubscribers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	artifact, _, err := s.documentOwnerFromRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.writeSubscribers(w, r, documentSubscriberBase(artifact.Project, artifact.Slug))
}

func (s *server) writeSubscribers(w http.ResponseWriter, r *http.Request, base string) {
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}
	interests, err := s.deps.Envoy.ListInterests(r.Context())
	if err != nil {
		s.writeEnvoyError(w, err)
		return
	}
	sessions, err := s.deps.Envoy.Sessions(r.Context())
	if err != nil {
		s.writeEnvoyError(w, err)
		return
	}
	live := make(map[string]envoy.Session, len(sessions))
	for _, session := range sessions {
		live[session.SessionID] = session
	}

	result := make([]subscriber, 0)
	for _, interest := range interests {
		matched := matchingTopics(interest.Topics, base)
		if len(matched) == 0 {
			continue
		}
		entry := subscriber{SessionID: interest.SessionID, Topics: matched, LastSeen: interest.UpdatedAt}
		if exact := exactTopics(matched, base); len(exact) > 0 {
			entry.Removable = true
		} else {
			entry.Via = matched[0]
		}
		if session, ok := live[interest.SessionID]; ok {
			entry.Title = session.Title
			entry.Live = true
			entry.LastSeen = session.LastSeen
		}
		result = append(result, entry)
	}
	sort.SliceStable(result, func(left, right int) bool {
		if result[left].LastSeen != result[right].LastSeen {
			return result[left].LastSeen > result[right].LastSeen
		}
		return result[left].SessionID < result[right].SessionID
	})
	writeJSON(w, http.StatusOK, result)
}

func (s *server) unsubscribeIssueSession(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	s.unsubscribeSession(w, r, actor, issueOwner(key), issueSubscriberBase(key))
}

func (s *server) unsubscribeArtifactSession(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	artifact, own, err := s.documentOwnerFromRequest(r.Context(), s.deps.Store.Pool, r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.unsubscribeSession(w, r, actor, own, documentSubscriberBase(artifact.Project, artifact.Slug))
}

// subscriptionRemovedPayload is the wire payload of subscription.removed: a
// human unsubscribed SessionID from Topics on this issue or document. It is
// both the audit trail (recorded on the owner's event log) and, once the
// outbox publishes it, the direct notice routed to the unsubscribed session's
// own agent topic regardless of the issue's route (see
// outbox.publishAuthorRoutes).
type subscriptionRemovedPayload struct {
	SessionID string      `json:"session_id"`
	By        model.Actor `json:"by"`
	Topics    []string    `json:"topics"`
}

func (s *server) unsubscribeSession(
	w http.ResponseWriter,
	r *http.Request,
	actor model.Actor,
	own owner,
	base string,
) {
	if s.deps.Envoy == nil {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, "ENVOY_URL is not configured")
		return
	}
	sessionID := r.PathValue("session_id")
	interest, err := s.deps.Envoy.Interest(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, envoy.ErrNotFound) {
			writeError(w, "SUBSCRIBER_NOT_FOUND", http.StatusNotFound, "session is not subscribed")
			return
		}
		s.writeEnvoyError(w, err)
		return
	}
	matched := matchingTopics(interest.Topics, base)
	if len(matched) == 0 {
		writeError(w, "SUBSCRIBER_NOT_FOUND", http.StatusNotFound, "session is not subscribed")
		return
	}
	// Only a topic scoped exactly to this owner is safe to remove: a broader
	// wildcard among the session's topics (e.g. "notifications.dispatch.>")
	// also covers every other issue and document, so removing it here would
	// silently unsubscribe the session everywhere else too.
	exact := exactTopics(matched, base)
	if len(exact) == 0 {
		writeError(
			w, "NOT_REMOVABLE", http.StatusConflict,
			"session is subscribed only via a broader topic ("+matched[0]+"); removing it would affect every other issue and document",
		)
		return
	}

	// Record the event durably before ever touching the listener: if the
	// listener call below fails, the transaction rolls back (via the deferred
	// Rollback, a no-op after a successful Commit) so a retry sees the
	// interest still intact and no orphaned event with nothing removed.
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	event, err := s.appendEvent(r.Context(), tx, own.event(
		"subscription.removed",
		actor,
		subscriptionRemovedPayload{SessionID: sessionID, By: actor, Topics: exact},
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := s.deps.Envoy.Unsubscribe(r.Context(), sessionID, exact); err != nil {
		writeError(w, "ENVOY_UNSUBSCRIBE_FAILED", http.StatusBadGateway, err.Error())
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) writeEnvoyError(w http.ResponseWriter, err error) {
	if errors.Is(err, envoy.ErrUnavailable) {
		writeError(w, "ENVOY_UNAVAILABLE", http.StatusServiceUnavailable, err.Error())
		return
	}
	s.writeHandlerError(w, err)
}
