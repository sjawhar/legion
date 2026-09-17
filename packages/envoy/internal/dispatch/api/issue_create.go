package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/rank"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
)

const defaultIssueSpecMarkdown = `## Summary

_Three sentences at most, in plain words: the problem, what changes for whom, and how we will know it worked._

## Decisions needed

_List only decisions requiring human authority, taste, or risk appetite: one plain question each, two or three options with what each costs, and a recommendation with its reason._

None: this records what was agreed.

## New since we talked

_One plain sentence per design point the human did not settle in conversation, marked inferred with the reasoning._

## Acceptance

_List numbered outcomes that name what a user will observe and the check that proves each one._

## Requirements

_What must hold, and where each came from: a quoted human sentence, or inferred plus the reasoning._

## Design

_The files, components, routes, and data flow that change._

## Errors

_Use a condition | behaviour table; do not specify silent fallbacks._

## Testing

_Map every acceptance line to the proof that exercises it._

## Rejected

_List each considered alternative and the reason it was rejected._
`

// parseIssuePriority decodes the tri-state `priority` field of an issue write. Absent →
// (nil, false): leave it alone. JSON null → (nil, true): clear it. An integer 0..3 → its
// value. Anything else → 400 INVALID_PRIORITY.
func parseIssuePriority(raw json.RawMessage) (priority *int, provided bool, err error) {
	if len(raw) == 0 {
		return nil, false, nil
	}
	if strings.TrimSpace(string(raw)) == "null" {
		return nil, true, nil
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value < 0 || value > 3 {
		return nil, true, errorf(http.StatusBadRequest, "INVALID_PRIORITY", "priority must be an integer from 0 to 3 or null")
	}
	return &value, true, nil
}

func (s *server) createIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	var input struct {
		Project    string          `json:"project"`
		Title      string          `json:"title"`
		Parent     *string         `json:"parent"`
		External   string          `json:"external"`
		Force      bool            `json:"force"`
		Spec       *string         `json:"spec"`
		Labels     []string        `json:"labels"`
		Priority   json.RawMessage `json:"priority"`
		Assignee   json.RawMessage `json:"assignee"`
		Components json.RawMessage `json:"components"`
		Actor      *model.Actor    `json:"actor"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	actor, ok := s.requireActor(w, r, input.Actor)
	if !ok {
		return
	}
	input.Project = strings.TrimSpace(input.Project)
	input.Title = strings.TrimSpace(input.Title)
	parentKey := ""
	if input.Parent != nil {
		parentKey = strings.TrimSpace(*input.Parent)
	}

	var externalRepo, externalNumber string
	usingDefaultProject := false
	if input.External != "" {
		var err error
		externalRepo, externalNumber, err = externalRef(input.External)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		project, err := s.repoProject(r.Context(), externalRepo)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if project == "" {
			project = s.deps.DefaultProject
			if project == "" {
				writeError(w, "PROJECT_UNMAPPED", http.StatusBadRequest,
					"repository is not mapped in repository settings and DISPATCH_DEFAULT_PROJECT is not configured")
				return
			}
			usingDefaultProject = true
		}
		if input.Project != "" && input.Project != project {
			writeError(w, "EXTERNAL_PROJECT_MISMATCH", http.StatusBadRequest, "external issue project must match repository settings")
			return
		}
		input.Project = project
		if input.Title == "" {
			input.Title = input.External
		}
	}
	if !projectKeyPattern.MatchString(input.Project) || input.Title == "" {
		writeError(w, "INVALID_ISSUE", http.StatusBadRequest, "project and title are required")
		return
	}
	if usingDefaultProject {
		input.Labels = append(input.Labels, repoLabelPrefix+externalRepo)
	}
	labels, err := normalizeIssueLabels(input.Labels)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	priority, _, err := parseIssuePriority(input.Priority)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	assignee, assigneeProvided, err := s.parseIssueAssignee(input.Assignee)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	components, componentsProvided, err := parseIssueComponents(input.Components)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.External == "" && !input.Force {
		candidates, err := s.duplicateCandidates(r.Context(), s.deps.Store.Pool, input.Project, input.Title, parentKey)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if len(candidates) > 0 {
			WriteJSON(w, http.StatusConflict, map[string]any{
				"error":      fmt.Sprintf("possible duplicate of %s: %s", candidates[0].Key, candidates[0].Title),
				"code":       "POSSIBLE_DUPLICATE",
				"candidates": candidates,
			})
			return
		}
	}

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var parentAssignee *string
	if parentKey != "" {
		var parentProject string
		err := tx.QueryRow(r.Context(), `select project_key, assignee from issues where key = $1`, parentKey).Scan(&parentProject, &parentAssignee)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, "PARENT_INPUT", http.StatusBadRequest, fmt.Sprintf("parent issue %s not found", parentKey))
			return
		}
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if parentProject != input.Project {
			writeError(w, "PARENT_INPUT", http.StatusBadRequest, "parent must be in the same project")
			return
		}
	}
	if !assigneeProvided {
		assignee = defaultAssignee(actor, parentAssignee)
	}

	var number int
	if err := tx.QueryRow(r.Context(), `
		update projects
		set next_number = next_number + 1
		where key = $1
		returning next_number - 1
	`, input.Project).Scan(&number); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	key := fmt.Sprintf("%s-%d", input.Project, number)
	actorJSON, err := encodeJSON(actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	var parent any
	if parentKey != "" {
		parent = parentKey
	}
	if err := lockProjectRankAllocation(r.Context(), tx, input.Project); err != nil {
		s.writeHandlerError(w, err)
		return
	}

	lastRank, err := lastProjectRank(r.Context(), tx, input.Project, "")
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issueRank := rank.Between(lastRank, "")

	if _, err := tx.Exec(r.Context(), `
		insert into issues (key, project_key, number, title, parent_key, created_by, labels, priority, rank, assignee)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, key, input.Project, number, input.Title, parent, actorJSON, labels, priority, issueRank, assignee); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if componentsProvided {
		if err := writeIssueComponents(r.Context(), tx, key, input.Project, *components); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	if input.External != "" {
		if _, err := tx.Exec(r.Context(), `
			insert into issue_external_links (issue_key, url, kind) values ($1, $2, 'github_issue')
		`, key, externalURL(externalRepo, externalNumber)); err != nil {
			if !isUniqueViolation(err) {
				s.writeHandlerError(w, err)
				return
			}
			if err := tx.Rollback(r.Context()); err != nil {
				s.writeHandlerError(w, err)
				return
			}
			existingKey, err := s.resolveIssueRef(r.Context(), input.External)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			existing, err := s.loadIssue(r.Context(), s.deps.Store.Pool, existingKey)
			if err != nil {
				s.writeHandlerError(w, err)
				return
			}
			WriteJSON(w, http.StatusOK, existing)
			return
		}
	}

	var artifactID string
	if err := tx.QueryRow(r.Context(), `
		insert into artifacts (issue_key, project_key, slug, name, kind, is_primary, created_by)
		values ($1, $2, 'spec', 'spec.md', 'doc', true, $3)
		returning id::text
	`, key, input.Project, actorJSON).Scan(&artifactID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	markdown := defaultIssueSpecMarkdown
	if input.Spec != nil && strings.TrimSpace(*input.Spec) != "" {
		markdown = *input.Spec
	}
	markdown, err = s.deps.Docs.SeedText(r.Context(), tx, artifactID, markdown, actor)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	authors, err := encodeJSON([]model.Actor{actor})
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if _, err := tx.Exec(r.Context(), `
		insert into artifact_versions (artifact_id, number, markdown, authors)
		values ($1, 1, $2, $3)
	`, artifactID, markdown, authors); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Replace(r.Context(), tx, "artifact", artifactID, markdown, s.deps.ServerURL); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue, err := s.loadIssue(r.Context(), tx, key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue.LastSeq++
	event, err := s.appendEvent(r.Context(), tx, issueOwner(key).event(
		"issue.created",
		actor,
		issue,
	))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := refs.Stamp(r.Context(), tx, "artifact", artifactID, event.ID); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A seeded spec never passed through a live edit, so nothing has queued the document
	// closer that indexes its ask blocks and repairs block ids; queue it now that the
	// transaction is durable.
	s.deps.Docs.ScheduleSettlement(artifactID)
	s.publish(event)
	WriteJSON(w, http.StatusCreated, issue)
}

func isUniqueViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}
