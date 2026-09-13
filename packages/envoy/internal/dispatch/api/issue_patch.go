package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

func (s *server) patchIssue(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title         *string               `json:"title"`
		Status        *string               `json:"status"`
		Rank          *rankInput            `json:"rank"`
		Labels        *[]string             `json:"labels"`
		Priority      json.RawMessage       `json:"priority"`
		Route         *string               `json:"route"`
		ExternalLinks *[]model.ExternalLink `json:"external_links"`
		Actor         *model.Actor          `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	if input.Title != nil && strings.TrimSpace(*input.Title) == "" {
		writeError(w, "INVALID_ISSUE", http.StatusBadRequest, "title must not be blank")
		return
	}
	status := ""
	if input.Status != nil {
		status = strings.TrimSpace(*input.Status)
		if status == "" {
			writeError(w, "INVALID_ISSUE", http.StatusBadRequest, "status must not be blank")
			return
		}
		if !model.IsIssueStatus(status) {
			writeError(w, "INVALID_STATUS", http.StatusBadRequest, "status is not in the Legion lifecycle")
			return
		}
	}
	if input.Route != nil && *input.Route != "" {
		if _, err := model.ParseRoute(*input.Route); err != nil {
			writeError(w, "ROUTE_INVALID", http.StatusBadRequest, "invalid route")
			return
		}
	}
	var labels []string
	if input.Labels != nil {
		var err error
		labels, err = normalizeIssueLabels(*input.Labels)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	priority, err := parseIssuePriority(input.Priority)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	priorityProvided := len(input.Priority) > 0

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())

	key := r.PathValue("key")
	if input.Rank != nil {
		var project string
		if err := tx.QueryRow(r.Context(), `select project_key from issues where key = $1`, key).Scan(&project); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := lockProjectRankAllocation(r.Context(), tx, project); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}

	if err := tx.QueryRow(r.Context(), `select key from issues where key = $1 for update`, key).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	before, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if before.ClosedAt != nil {
		rankOnly := input.Rank != nil && input.Status == nil && input.Title == nil && input.Labels == nil && !priorityProvided && input.Route == nil && input.ExternalLinks == nil
		if !rankOnly && (status == "" || status == "done" || input.Title != nil || input.Labels != nil || priorityProvided || input.Route != nil || input.ExternalLinks != nil) {
			writeError(w, "ISSUE_CLOSED", http.StatusConflict, "issue is closed")
			return
		}
	}
	changed := false
	if input.Title != nil {
		if _, err := tx.Exec(r.Context(), `update issues set title = $2, updated_at = now() where key = $1`, key, strings.TrimSpace(*input.Title)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		changed = true
	}
	if input.Status != nil {
		if _, err := tx.Exec(r.Context(), `
			update issues
			set status = $2, closed_at = case when $2 = 'done' then now() else null end, updated_at = now()
			where key = $1
		`, key, status); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		changed = true
	}
	if input.Rank != nil {
		issueRank, err := s.rankForInput(r.Context(), tx, before.Project, key, *input.Rank)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if _, err := tx.Exec(r.Context(), `update issues set rank = $2, updated_at = now() where key = $1`, key, issueRank); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		changed = true
	}
	if priorityProvided {
		if _, err := tx.Exec(r.Context(), `update issues set priority = $2, updated_at = now() where key = $1`, key, priority); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		changed = true
	}

	if input.Labels != nil {
		if _, err := tx.Exec(r.Context(), `update issues set labels = $2, updated_at = now() where key = $1`, key, labels); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		changed = true
	}
	if input.Route != nil {
		var route any
		if *input.Route != "" {
			route = *input.Route
		}
		if _, err := tx.Exec(r.Context(), `update issues set route = $2, updated_at = now() where key = $1`, key, route); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		changed = true
	}
	if input.ExternalLinks != nil {
		if _, err := tx.Exec(r.Context(), `delete from issue_external_links where issue_key = $1`, key); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		for _, link := range *input.ExternalLinks {
			if err := validateExternalURL(link.URL); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			kind := link.Kind
			if kind == "" {
				kind = "url"
			}
			if _, err := tx.Exec(r.Context(), `insert into issue_external_links (issue_key, url, kind) values ($1, $2, $3)`, key, link.URL, kind); err != nil {
				s.writeHandlerError(w, err)
				return
			}
		}
		changed = true
	}
	if !changed {
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, before)
		return
	}
	after, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events := []model.Event{}
	statusChanged := input.Status != nil && before.Status != after.Status
	eventType := "issue.updated"
	if statusChanged && after.Status == "done" {
		eventType = "issue.closed"
	}
	if statusChanged && after.Parent != nil {
		if err := s.deps.Events.LockOwners(r.Context(), tx,
			issueOwner(key).event("", actor, nil),
			issueOwner(*after.Parent).event("", actor, nil),
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	after.LastSeq++
	event, err := s.appendEvent(r.Context(), tx, issueOwner(key).event(eventType, actor, after))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	if statusChanged && after.Parent != nil {
		childEvent, err := s.appendEvent(r.Context(), tx, issueOwner(*after.Parent).event(
			"child.status",
			actor,
			map[string]any{"child_key": after.Key, "from": before.Status, "to": after.Status},
		))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		events = append(events, childEvent)
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if (before.ClosedAt == nil) != (after.ClosedAt == nil) {
		s.deps.Docs.SetIssueClosed(key, after.ClosedAt != nil)
	}
	s.publish(events...)
	writeJSON(w, http.StatusOK, after)
}
