package api

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/envoy"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// collaboratorReason is one way a person or an agent worked an issue. The values are bits so a
// row's set is one word; collaboratorReasons lists them in the order every answer spells them.
type collaboratorReason uint8

const (
	reasonAssignee collaboratorReason = 1 << iota
	reasonClaimed
	reasonPosted
	reasonAnswered
)

var collaboratorReasons = []struct {
	reason collaboratorReason
	name   string
}{
	{reasonAssignee, "assignee"},
	{reasonClaimed, "claimed"},
	{reasonPosted, "posted"},
	{reasonAnswered, "answered"},
}

// collaboratorEventReasons is every event type whose actor worked the issue, with what it did:
// claimed it, wrote on it or one of its documents (a comment, a message, an ask, a document
// version, an agent's reply to a comment or message delivery), or answered one of its asks. An
// issue document's events carry the issue's key (ownerForArtifact), so one read by issue_key
// covers them.
var collaboratorEventReasons = map[string]collaboratorReason{
	"issue.claimed":    reasonClaimed,
	"comment.created":  reasonPosted,
	"message.created":  reasonPosted,
	"ask.opened":       reasonPosted,
	"artifact.version": reasonPosted,
	"comment.answered": reasonPosted,
	"message.answered": reasonPosted,
	"ask.answered":     reasonAnswered,
}

// collaboratorEventTypes is collaboratorEventReasons' keys, the type list the one events read
// binds.
var collaboratorEventTypes = func() []string {
	types := make([]string, 0, len(collaboratorEventReasons))
	for eventType := range collaboratorEventReasons {
		types = append(types, eventType)
	}
	slices.Sort(types)
	return types
}()

// legionDaemonSessionPrefix starts the session id the Legion daemon writes an issue's status and
// lifecycle messages under (legion-daemon:<PROJECT>, packages/daemon's DaemonSession). It holds no
// conversation, so it is nobody to ask and never a collaborator.
const legionDaemonSessionPrefix = "legion-daemon:"

// Registry states the collaborators answer names.
const (
	legionRegistryOK          = "ok"
	legionRegistryUnavailable = "unavailable"
)

// issueCollaborators is GET /api/v1/issues/{key}/collaborators.
type issueCollaborators struct {
	IssueKey string               `json:"issue_key"`
	People   []collaboratorPerson `json:"people"`
	Agents   []collaboratorAgent  `json:"agents"`
	// LegionRegistry is "unavailable" when the listener's Legion agent registry did not answer
	// (no listener configured, an error, a timeout): agents then holds only the sessions the
	// issue's events name.
	LegionRegistry string `json:"legion_registry"`
}

// collaboratorPerson is one person, by canonical login.
type collaboratorPerson struct {
	Kind    string   `json:"kind"`
	Login   string   `json:"login"`
	Reasons []string `json:"reasons"`
	// LastAt is the newest event that made them a collaborator; null for an assignee who did
	// nothing else on the issue.
	LastAt *string `json:"last_at"`
}

// collaboratorAgent is one agent session, by id. It carries no running state: a reader takes
// that from the live agents registry, so one agent never shows two states on one page.
type collaboratorAgent struct {
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
	// Title is the registry entry's title, else the session's newest stamped title; null when
	// neither names one.
	Title   *string  `json:"title"`
	Reasons []string `json:"reasons"`
	// LastAt is the newest event that made the session a collaborator; null for a registry
	// entry with no such event on the issue.
	LastAt *string `json:"last_at"`
	// Legion is the session's Legion agent registry entry; null for a session the registry does
	// not record for this issue, or when the registry did not answer.
	Legion *collaboratorLegion `json:"legion"`
}

// collaboratorLegion is the part of a Legion agent registry entry the answer carries.
// ReplacedBy and ReplacedAt name the role token's next session, null for its newest.
type collaboratorLegion struct {
	Role       string  `json:"role"`
	RoleToken  string  `json:"role_token"`
	StartedAt  string  `json:"started_at"`
	ReplacedBy *string `json:"replaced_by"`
	ReplacedAt *string `json:"replaced_at"`
}

// collaboratorTally is what the events say about one person or session.
type collaboratorTally struct {
	reasons collaboratorReason
	lastAt  *time.Time
}

func (t *collaboratorTally) add(reason collaboratorReason, at *time.Time) {
	t.reasons |= reason
	if at != nil && (t.lastAt == nil || at.After(*t.lastAt)) {
		t.lastAt = at
	}
}

func (t *collaboratorTally) reasonNames() []string {
	names := []string{}
	for _, known := range collaboratorReasons {
		if t.reasons&known.reason != 0 {
			names = append(names, known.name)
		}
	}
	return names
}

func tallyOf(tallies map[string]*collaboratorTally, id string) *collaboratorTally {
	tally, ok := tallies[id]
	if !ok {
		tally = &collaboratorTally{}
		tallies[id] = tally
	}
	return tally
}

// getIssueCollaborators answers everyone who worked an issue: the assignee, and every actor of
// the issue's claim, writing and answer events, people by login and sessions by id, merged with
// the sessions the listener's Legion agent registry records for the issue. The cost is fixed: the
// assignee, one events read by issue_key and type (the (issue_key, seq) index), one listener call
// and, for sessions the registry names no title for, one title read.
func (s *server) getIssueCollaborators(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	ctx := r.Context()
	key := r.PathValue("key")
	var assignee *string
	if err := s.deps.Store.Pool.QueryRow(ctx, `select assignee from issues where key = $1`, key).Scan(&assignee); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	people := map[string]*collaboratorTally{}
	sessions := map[string]*collaboratorTally{}
	if assignee != nil {
		if login := model.CanonicalLogin(*assignee); login != "" {
			tallyOf(people, login).add(reasonAssignee, nil)
		}
	}
	if err := s.tallyCollaboratorEvents(ctx, key, people, sessions); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	registry, registryState := s.issueLegionAgents(ctx, key)
	agents, err := s.collaboratorAgents(ctx, sessions, registry)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, issueCollaborators{
		IssueKey:       key,
		People:         collaboratorPeople(people),
		Agents:         agents,
		LegionRegistry: registryState,
	})
}

// tallyCollaboratorEvents reads the issue's claim, writing and answer events, one row per actor
// and type, into people (by canonical login) and sessions (by id). A session acting for a
// person's token is an agent; the daemon's lifecycle writer and system writers are nobody.
func (s *server) tallyCollaboratorEvents(
	ctx context.Context, key string, people, sessions map[string]*collaboratorTally,
) error {
	rows, err := s.deps.Store.Pool.Query(ctx, `
		select coalesce(actor->>'kind', ''), coalesce(actor->>'id', ''), type, max(created_at)
		from events
		where issue_key = $1 and type = any($2)
		group by 1, 2, 3
	`, key, collaboratorEventTypes)
	if err != nil {
		return fmt.Errorf("query issue collaborators: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind, id, eventType string
		var at time.Time
		if err := rows.Scan(&kind, &id, &eventType, &at); err != nil {
			return fmt.Errorf("scan issue collaborator: %w", err)
		}
		reason := collaboratorEventReasons[eventType]
		switch kind {
		case "user":
			if login := model.CanonicalLogin(id); login != "" {
				tallyOf(people, login).add(reason, &at)
			}
		case "session":
			if id != "" && !strings.HasPrefix(id, legionDaemonSessionPrefix) {
				tallyOf(sessions, id).add(reason, &at)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate issue collaborators: %w", err)
	}
	return nil
}

// issueLegionAgents is the listener's Legion agent registry entries for the issue, or
// "unavailable" when there is no listener or it did not answer within the client's window.
func (s *server) issueLegionAgents(ctx context.Context, key string) ([]envoy.LegionAgent, string) {
	if s.deps.Envoy == nil {
		return nil, legionRegistryUnavailable
	}
	entries, err := s.deps.Envoy.IssueAgents(ctx, key)
	if err != nil {
		return nil, legionRegistryUnavailable
	}
	return entries, legionRegistryOK
}

// collaboratorAgents merges the event-derived sessions with the registry's entries by session id:
// the registry's sessions first, in its order (by role in workflow order, then newest first), then
// the other sessions newest first.
func (s *server) collaboratorAgents(
	ctx context.Context, sessions map[string]*collaboratorTally, registry []envoy.LegionAgent,
) ([]collaboratorAgent, error) {
	agents := make([]collaboratorAgent, 0, len(sessions)+len(registry))
	listed := make(map[string]struct{}, len(sessions)+len(registry))
	for _, entry := range registry {
		if _, seen := listed[entry.SessionID]; seen {
			continue
		}
		listed[entry.SessionID] = struct{}{}
		agent := newCollaboratorAgent(entry.SessionID, sessions[entry.SessionID])
		if entry.Title != "" {
			agent.Title = &entry.Title
		}
		agent.Legion = &collaboratorLegion{
			Role:       entry.Role,
			RoleToken:  entry.RoleToken,
			StartedAt:  entry.StartedAt,
			ReplacedBy: entry.ReplacedBy,
			ReplacedAt: entry.ReplacedAt,
		}
		agents = append(agents, agent)
	}
	others := make([]string, 0, len(sessions))
	for id := range sessions {
		if _, seen := listed[id]; !seen {
			others = append(others, id)
		}
	}
	slices.SortFunc(others, func(left, right string) int {
		return cmp.Or(sessions[right].lastAt.Compare(*sessions[left].lastAt), strings.Compare(left, right))
	})
	for _, id := range others {
		agents = append(agents, newCollaboratorAgent(id, sessions[id]))
	}

	untitled := []string{}
	for _, agent := range agents {
		if agent.Title == nil {
			untitled = append(untitled, agent.SessionID)
		}
	}
	if len(untitled) == 0 {
		return agents, nil
	}
	titles, err := delivery.ResolveSessionTitles(ctx, s.deps.Store.Pool, untitled)
	if err != nil {
		return nil, fmt.Errorf("resolve collaborator titles: %w", err)
	}
	for index := range agents {
		if title, ok := titles[agents[index].SessionID]; ok && agents[index].Title == nil {
			agents[index].Title = &title
		}
	}
	return agents, nil
}

func newCollaboratorAgent(id string, tally *collaboratorTally) collaboratorAgent {
	if tally == nil {
		tally = &collaboratorTally{}
	}
	return collaboratorAgent{
		Kind:      "session",
		SessionID: id,
		Reasons:   tally.reasonNames(),
		LastAt:    timestampPtr(tally.lastAt),
	}
}

// collaboratorPeople is one row per login, newest last_at first, an assignee who did nothing else
// last, ties by login.
func collaboratorPeople(people map[string]*collaboratorTally) []collaboratorPerson {
	logins := make([]string, 0, len(people))
	for login := range people {
		logins = append(logins, login)
	}
	slices.SortFunc(logins, func(left, right string) int {
		leftAt, rightAt := people[left].lastAt, people[right].lastAt
		switch {
		case leftAt == nil && rightAt != nil:
			return 1
		case leftAt != nil && rightAt == nil:
			return -1
		case leftAt != nil && rightAt != nil:
			if order := rightAt.Compare(*leftAt); order != 0 {
				return order
			}
		}
		return strings.Compare(left, right)
	})
	rows := make([]collaboratorPerson, 0, len(logins))
	for _, login := range logins {
		rows = append(rows, collaboratorPerson{
			Kind:    "user",
			Login:   login,
			Reasons: people[login].reasonNames(),
			LastAt:  timestampPtr(people[login].lastAt),
		})
	}
	return rows
}
