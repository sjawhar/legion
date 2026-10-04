package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// canonicalLogin is the stored form of a person's email: trimmed and lowercased, the form the
// identity implementations name a person by and the people table holds.
func canonicalLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

// parseIssueAssignee decodes the tri-state `assignee` field of an issue write. Absent →
// (nil, false): leave it alone. JSON null → (nil, true): clear it. A string → its canonical
// email, which must name a person who has signed in (400 ASSIGNEE_NOT_ALLOWED otherwise).
// Anything else → 400 INVALID_ISSUE.
func (s *server) parseIssueAssignee(ctx context.Context, raw json.RawMessage) (assignee *string, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var login string
	if err := json.Unmarshal(raw, &login); err != nil {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_ISSUE", "assignee must be a person's email or null")
	}
	canonical, err := s.allowedLogin(ctx, login)
	if err != nil {
		return nil, true, err
	}
	return &canonical, true, nil
}

// allowedLogin canonicalises login and checks that it names a person who has signed in.
func (s *server) allowedLogin(ctx context.Context, login string) (string, error) {
	canonical := canonicalLogin(login)
	var known bool
	if canonical != "" {
		if err := s.deps.Store.Pool.QueryRow(ctx, `select exists(select 1 from people where email = $1)`, canonical).Scan(&known); err != nil {
			return "", fmt.Errorf("read person %q: %w", canonical, err)
		}
	}
	if !known {
		return "", errorf(http.StatusBadRequest, "ASSIGNEE_NOT_ALLOWED", "%q has not signed in to Dispatch", strings.TrimSpace(login))
	}
	return canonical, nil
}

// defaultAssignee is who a new issue goes to when the creator names nobody: the human who
// created it, the owner of the personal token that created it, or the parent's assignee.
// The shared token creating a root issue leaves it unassigned.
func defaultAssignee(actor model.Actor, parent *string) *string {
	if actor.Kind == "user" {
		return new(canonicalLogin(actor.ID))
	}
	if actor.Owner != nil {
		return new(canonicalLogin(*actor.Owner))
	}
	return parent
}

type dispatchUser struct {
	Login string `json:"login"`
}

// listUsers returns everyone who has signed in, sorted by email: the assignee picker's options.
func (s *server) listUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `select email from people order by email`)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list people: %w", err))
		return
	}
	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (dispatchUser, error) {
		var user dispatchUser
		err := row.Scan(&user.Login)
		return user, err
	})
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list people: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"users": users})
}

// whoami tells any authenticated caller who the server takes it for: a human by login, or an
// agent with the lowercase login of the personal token's owner (null for the shared token) and
// the verified Kubernetes subject of its service-account token (null for every other bearer).
func (s *server) whoami(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	if human {
		WriteJSON(w, http.StatusOK, map[string]any{"kind": "user", "login": actor.ID})
		return
	}
	var owner *string
	if actor.Owner != nil {
		owner = new(canonicalLogin(*actor.Owner))
	}
	WriteJSON(w, http.StatusOK, map[string]any{"kind": "agent", "owner": owner, "service": actor.Service})
}
