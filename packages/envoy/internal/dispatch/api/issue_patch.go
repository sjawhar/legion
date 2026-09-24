package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
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
		Assignee      json.RawMessage       `json:"assignee"`
		Parent        json.RawMessage       `json:"parent"`
		Components    json.RawMessage       `json:"components"`
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
	priority, priorityProvided, err := parseIssuePriority(input.Priority)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	assignee, assigneeProvided, err := s.parseIssueAssignee(input.Assignee)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	parent, parentProvided, err := parseIssueParent(input.Parent)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	components, componentsProvided, err := parseIssueComponents(input.Components)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}

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

	if parentProvided && parent != nil {
		if err := lockIssueAndParent(r.Context(), tx, key, *parent); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	} else if err := tx.QueryRow(r.Context(), `select key from issues where key = $1 for no key update`, key).Scan(new(string)); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	before, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if before.ClosedAt != nil {
		// A closed issue takes only its rank (board order) and its component attachment
		// (classified without reopening), alone or beside a reopening status; every other
		// field, and closing it again, waits for a reopen.
		others := input.Title != nil || input.Labels != nil || priorityProvided || input.Route != nil || input.ExternalLinks != nil || assigneeProvided || parentProvided
		reopening := input.Status != nil && status != "done"
		if others || (input.Status != nil && !reopening) || (input.Status == nil && input.Rank == nil && !componentsProvided) {
			writeError(w, "ISSUE_CLOSED", http.StatusConflict, "issue is closed")
			return
		}
	}
	// Closing an issue releases whatever claim it held: the work is finished, so nobody is
	// on it. No other status move touches the claim — a claim is an explicit act
	// (issue_claim.go), and the status is also how humans track work.
	// previousClaim is set only when this write closes the issue, so it alone says both that a
	// claim is being released and whose it was.
	var previousClaim *model.IssueClaim
	if input.Status != nil && status == "done" {
		previousClaim = before.Claim
	}
	// Every provided column lands in one UPDATE, updated_at with it; a components-only PATCH
	// still touches updated_at. The row write runs before the components write so a component
	// validation error surfaces after it, and the transaction rolls both back.
	var sets []string
	args := []any{key}
	set := func(column string, value any) {
		args = append(args, value)
		sets = append(sets, column+" = $"+strconv.Itoa(len(args)))
	}
	if input.Title != nil {
		set("title", strings.TrimSpace(*input.Title))
	}
	if input.Status != nil {
		args = append(args, status)
		placeholder := "$" + strconv.Itoa(len(args))
		sets = append(sets, "status = "+placeholder, "closed_at = case when "+placeholder+" = 'done' then now() else null end")
	}
	if input.Rank != nil {
		issueRank, err := s.rankForInput(r.Context(), tx, before.Project, key, *input.Rank)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		set("rank", issueRank)
	}
	if priorityProvided {
		set("priority", priority)
	}
	if assigneeProvided {
		set("assignee", assignee)
	}
	if parentProvided {
		var parentValue any
		if parent != nil {
			parentValue = *parent
		}
		set("parent_key", parentValue)
	}
	if input.Labels != nil {
		set("labels", labels)
	}
	if input.Route != nil {
		var route any
		if *input.Route != "" {
			route = *input.Route
		}
		set("route", route)
	}
	changed := len(sets) > 0 || componentsProvided
	if changed {
		sets = append(sets, "updated_at = now()")
		if _, err := tx.Exec(r.Context(), `update issues set `+strings.Join(sets, ", ")+` where key = $1`, args...); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if previousClaim != nil {
		// Closing releases the claim through the one writer of those columns.
		if err := writeIssueClaim(r.Context(), tx, key, nil); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if componentsProvided {
		if err := writeIssueComponents(r.Context(), tx, key, before.Project, *components); err != nil {
			s.writeHandlerError(w, err)
			return
		}
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
			// A URL links exactly one issue (issue_external_links_url). This issue's own
			// links are already deleted above, so an owner found here is another issue
			// (or the same URL repeated earlier in this request).
			var owner string
			err := tx.QueryRow(r.Context(), `select issue_key from issue_external_links where url = $1`, link.URL).Scan(&owner)
			if err == nil {
				writeError(w, "EXTERNAL_LINK_TAKEN", http.StatusConflict, fmt.Sprintf("%s is already linked from %s", link.URL, owner))
				return
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				s.writeHandlerError(w, err)
				return
			}
			if _, err := tx.Exec(r.Context(), `insert into issue_external_links (issue_key, url, kind) values ($1, $2, $3)`, key, link.URL, kind); err != nil {
				if isUniqueViolation(err) {
					writeError(w, "EXTERNAL_LINK_TAKEN", http.StatusConflict, fmt.Sprintf("%s is already linked from another issue", link.URL))
					return
				}
				s.writeHandlerError(w, err)
				return
			}
		}
		changed = true
	}
	if !changed {
		advice := s.writeAdvice(
			r.Context(), tx, "PATCH /api/v1/issues/{key}", key, actor, "", before.Status,
		)
		if err := tx.Commit(r.Context()); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		WriteJSON(w, http.StatusOK, withAdvice(before, advice))
		return
	}
	after, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events := []model.Event{}
	statusChanged := input.Status != nil && before.Status != after.Status
	parentChanged := parentProvided && !stringPointersEqual(before.Parent, after.Parent)
	eventType := "issue.updated"
	if statusChanged && after.Status == "done" {
		eventType = "issue.closed"
	}
	// Every owner this transaction appends to must be in one LockOwners call before the
	// first append: the issue itself, the status-change parent, and both ends of a reparent.
	owners := []model.Event{issueOwner(key).event("", actor, nil)}
	ownerKeys := map[string]bool{key: true}
	addOwner := func(parentKey *string) {
		if parentKey == nil || ownerKeys[*parentKey] {
			return
		}
		ownerKeys[*parentKey] = true
		owners = append(owners, issueOwner(*parentKey).event("", actor, nil))
	}
	if statusChanged {
		addOwner(after.Parent)
	}
	if parentChanged {
		addOwner(before.Parent)
		addOwner(after.Parent)
	}
	if len(owners) > 1 {
		if err := s.deps.Events.LockOwners(r.Context(), tx, owners...); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	after.LastSeq++
	event, err := s.appendEvent(r.Context(), tx, issueOwner(key).event(eventType, actor, model.NewIssueEventPayload(after, model.ReferenceChanges{})))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	events = append(events, event)
	if previousClaim != nil {
		released, err := s.appendEvent(r.Context(), tx, claimEvent("issue.released", key, actor, after, previousClaim, "closed"))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		after.LastSeq++
		events = append(events, released)
	}
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
	if parentChanged {
		if before.Parent != nil {
			removedEvent, err := s.appendEvent(r.Context(), tx, issueOwner(*before.Parent).event(
				"child.removed", actor, map[string]any{"child_key": after.Key},
			))
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			events = append(events, removedEvent)
		}
		if after.Parent != nil {
			addedEvent, err := s.appendEvent(r.Context(), tx, issueOwner(*after.Parent).event(
				"child.added", actor, map[string]any{"child_key": after.Key},
			))
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			events = append(events, addedEvent)
		}
	}
	advice := s.writeAdvice(
		r.Context(), tx, "PATCH /api/v1/issues/{key}", key, actor, "", after.Status,
	)
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if (before.ClosedAt == nil) != (after.ClosedAt == nil) {
		s.deps.Docs.SetIssueClosed(r.Context(), key, after.ClosedAt != nil)
	}
	s.publish(events...)
	WriteJSON(w, http.StatusOK, withAdvice(after, advice))
}
