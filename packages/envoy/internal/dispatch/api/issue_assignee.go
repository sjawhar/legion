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
	canonical := model.CanonicalLogin(login)
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

// defaultAssignee is who a new issue goes to when the create names nobody, in order: the human
// who created it, the owner of the personal token that created it, the parent's assignee, and
// the project's default assignee (projectDefault, the column a human sets through PUT
// /api/v1/projects/{key}/default-assignee). An explicit assignee on the create, a JSON null
// among them, never reaches here: parseIssueAssignee decides both. The shared token creating a
// root issue in a project with no default leaves it unassigned.
//
// The project's default is used only when it still names a person who has signed in: the create's
// own update of the project row returns the stored default only in that case (see the `returning`
// in createIssue), so a create in a project with no default costs nothing new and no extra round
// trip runs under the project row lock. That check is deliberately not allowedLogin: a default
// left stale by a restore or a direct write must fall through to nobody, not refuse the create.
func defaultAssignee(actor model.Actor, parent, projectDefault *string) *string {
	if actor.Kind == "user" {
		return new(model.CanonicalLogin(actor.ID))
	}
	if actor.Owner != nil {
		return new(model.CanonicalLogin(*actor.Owner))
	}
	if parent != nil {
		return parent
	}
	return projectDefault
}

type dispatchUser struct {
	Login string `json:"login"`
}

// listUsers returns everyone who has signed in, sorted by email: the assignee picker's options.
func (s *server) listUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.actingPerson(w, r); !ok {
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
// agent with the lowercase login of the person it acts for — the personal token's owner, or a
// broker-issued token's person (null for the shared token and for a broker-issued token that
// names a service and no person) — the service it is, which is the verified Kubernetes subject
// of its service-account token or the fleet a broker-issued token naming no person belongs to
// (null for every other credential), the session its credential proved (null for a credential
// that proves none, which every one but a broker-issued token is) and the one project that
// credential lets it write (null where it names none, a person-scoped token included).
//
// Every caller is told the OAuth scopes its credential holds, which is what decides the routes it
// may call (gateRouteScopes): both for a person at a browser, the set its own row names for a
// personal token, and none for the shared token, a pod's service-account token and a
// broker-issued token that names a service and no person. The kind stays `agent` for a personal
// token holding the person scope, since the kind names the credential and not the act; the
// person that token acts for is its `owner`, which the dispatch command reads.
func (s *server) whoami(w http.ResponseWriter, r *http.Request) {
	actor, human, err := s.optionalActor(r)
	if err != nil {
		s.writeAuthenticationError(w, err)
		return
	}
	scopes := whoamiScopes(actor.Credential)
	if human {
		WriteJSON(w, http.StatusOK, map[string]any{"kind": "user", "login": actor.ID, "scopes": scopes})
		return
	}
	var owner *string
	if actor.Owner != nil {
		owner = new(model.CanonicalLogin(*actor.Owner))
	}
	var session *string
	if actor.Kind == "session" && actor.ID != "" {
		session = &actor.ID
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"kind":    "agent",
		"owner":   owner,
		"service": actor.Service,
		"session": session,
		"project": actor.Project,
		"scopes":  scopes,
	})
}

// whoamiScopes is the scopes a credential holds as whoami answers them: an empty array for a
// credential that holds none, never null, so a reader need not tell a credential with no scope
// from one whose scopes it could not read.
func whoamiScopes(credential *model.Credential) []string {
	if credential == nil || len(credential.Scopes) == 0 {
		return []string{}
	}
	return credential.Scopes
}
