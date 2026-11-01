package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// A project's default assignee is the last step of an issue create's assignee chain
// (defaultAssignee, issue_assignee.go): it names a person by lowercase email, so both routes
// below are human-only, the gate GET /api/v1/users uses. That is deliberate and not an
// oversight to be tidied away: no agent command, flag or route gains the power to set or clear
// a project's default, however the architecture-source routes beside it are shaped. An agent
// session acts for a machine; choosing which person a project's unowned work lands on is a
// human's call, and the only agent-visible effect is the assignee a create comes back with.

type projectDefaultAssignee struct {
	Project  string  `json:"project"`
	Assignee *string `json:"assignee"`
}

// listProjectDefaultAssignees answers every project that names a default, by project key: the
// Settings page's one read. A project with no default is left out, and no default anywhere is
// an empty list, never null.
func (s *server) listProjectDefaultAssignees(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select key, default_assignee
		from projects
		where default_assignee is not null
		order by key
	`)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defaults, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (projectDefaultAssignee, error) {
		var entry projectDefaultAssignee
		err := row.Scan(&entry.Project, &entry.Assignee)
		return entry, err
	})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, defaults)
}

// putProjectDefaultAssignee sets a project's default assignee, or clears it with an explicit
// null. Human-only, as the file's head says. A body without an assignee is refused (400
// INVALID_PROJECT), never read as a null, so a PUT of `{}` cannot wipe a default. A login that
// is not a person who has signed in is refused here (400 ASSIGNEE_NOT_ALLOWED) so a typo never
// becomes a stored default; a default that goes stale later is not refused but ignored, by the
// create chain.
func (s *server) putProjectDefaultAssignee(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	key := r.PathValue("key")
	var input struct {
		Assignee json.RawMessage `json:"assignee"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// encoding/json leaves the RawMessage empty when the key is absent and hands it the literal
	// null for an explicit null: only the latter clears.
	if len(input.Assignee) == 0 {
		writeError(w, "INVALID_PROJECT", http.StatusBadRequest, "assignee is required: a person's email, or null to clear the default")
		return
	}
	var assignee *string
	if raw := strings.TrimSpace(string(input.Assignee)); raw != "null" {
		var login string
		if err := json.Unmarshal(input.Assignee, &login); err != nil {
			writeError(w, "INVALID_PROJECT", http.StatusBadRequest, "assignee must be a person's email or null")
			return
		}
		canonical, err := s.allowedLogin(r.Context(), login)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		assignee = &canonical
	}
	if err := s.deps.Store.Pool.QueryRow(r.Context(), "select key from projects where key = $1", key).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `update projects set default_assignee = $2 where key = $1`, key, assignee); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	stored := projectDefaultAssignee{Project: key, Assignee: assignee}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(key).event(
		"settings.project_default_assignee.updated", actor, stored,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	WriteJSON(w, http.StatusOK, stored)
}
