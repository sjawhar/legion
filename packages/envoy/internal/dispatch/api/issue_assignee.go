package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// canonicalLogin is the stored form of a GitHub login: trimmed and lowercased, matching how
// parseAllowedLogins and the identity implementations compare against the allowlist.
func canonicalLogin(login string) string {
	return strings.ToLower(strings.TrimSpace(login))
}

// parseIssueAssignee decodes the tri-state `assignee` field of an issue write. Absent →
// (nil, false): leave it alone. JSON null → (nil, true): clear it. A string → its canonical
// login, which must be on the allowlist (400 ASSIGNEE_NOT_ALLOWED otherwise). Anything else →
// 400 INVALID_ISSUE.
func (s *server) parseIssueAssignee(raw json.RawMessage) (assignee *string, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var login string
	if err := json.Unmarshal(raw, &login); err != nil {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_ISSUE", "assignee must be a GitHub login or null")
	}
	canonical, err := s.allowedLogin(login)
	if err != nil {
		return nil, true, err
	}
	return &canonical, true, nil
}

// allowedLogin canonicalises login and checks it against the allowlist.
func (s *server) allowedLogin(login string) (string, error) {
	canonical := canonicalLogin(login)
	if _, allowed := s.deps.AllowedLogins[canonical]; canonical == "" || !allowed {
		return "", errorf(http.StatusBadRequest, "ASSIGNEE_NOT_ALLOWED", "%q is not a login on the sign-in allowlist", strings.TrimSpace(login))
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

// listUsers returns the allowlist, sorted: the assignee picker's options. Pure config; no DB.
func (s *server) listUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	users := make([]dispatchUser, 0, len(s.deps.AllowedLogins))
	for login := range s.deps.AllowedLogins {
		users = append(users, dispatchUser{Login: login})
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Login < users[j].Login })
	WriteJSON(w, http.StatusOK, map[string]any{"users": users})
}

// whoami tells any authenticated caller who the server takes it for: a human by login, or an
// agent with the lowercase login of the personal token's owner (null for the shared token).
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
	WriteJSON(w, http.StatusOK, map[string]any{"kind": "agent", "owner": owner})
}
