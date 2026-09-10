package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// listIssuesQuery aggregates each issue's open-ask count with a single
// query. The state filter lives in the join condition, not a WHERE/FILTER
// clause on the joined rows, so it matches the partial asks_open(issue_key)
// where state = 'open' index instead of forcing a sequential scan of asks.
const listIssuesQuery = `
	select i.key, i.title, i.status, i.labels, i.parent_key, i.updated_at, i.last_seq,
	       count(a.id) filter (where i.closed_at is null)
	from issues i
	left join asks a on a.issue_key = i.key and a.state = 'open'
	where ($1 = '' or i.project_key = $1)
	  and ($2 = '' or i.status = $2)
	  and ($3 = '' or i.parent_key = $3)
	  and ($4::timestamptz is null or i.updated_at >= $4)
	group by i.key
	order by i.updated_at desc, i.key desc
`

const listPinnedIssuesQuery = `
	select i.key, i.title, i.status, i.labels, i.parent_key, i.updated_at, i.last_seq,
	       count(a.id) filter (where i.closed_at is null)
	from issues i
	join user_issue_state s on s.issue_key = i.key and s.login = $5 and s.pinned
	left join asks a on a.issue_key = i.key and a.state = 'open'
	where ($1 = '' or i.project_key = $1)
	  and ($2 = '' or i.status = $2)
	  and ($3 = '' or i.parent_key = $3)
	  and ($4::timestamptz is null or i.updated_at >= $4)
	group by i.key
	order by i.updated_at desc, i.key desc
`

const defaultIssueSpecMarkdown = `## Decisions needed

_List only decisions requiring human authority, taste, or risk appetite, with options, tradeoffs, and a recommendation._

None.

## Acceptance

_List numbered outcomes that name the check and user-facing surface that verifies each one._

## Requirements

_Use a requirement | provenance table with verbatim human quotes or inferred reasoning._

## Design

_List the files, components, routes, and data flow that change._

## Errors

_Use a condition | behaviour table; do not specify silent fallbacks._

## Testing

_Map every acceptance line to the suite or scenario that proves it._

## Rejected

_List each considered alternative and the reason it was rejected._
`

func (s *server) listIssues(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	pinned := query.Get("pinned") == "true"
	login := ""
	if pinned {
		actor, ok := s.requireHuman(w, r)
		if !ok {
			return
		}
		login = actor.ID
	} else if !s.requireAuthenticated(w, r) {
		return
	}
	project := strings.TrimSpace(query.Get("project"))
	status := strings.TrimSpace(query.Get("status"))
	parent := strings.TrimSpace(query.Get("parent"))
	var updatedSince *time.Time
	if query.Has("updated_since") {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(query.Get("updated_since")))
		if err != nil {
			writeError(w, "INVALID_UPDATED_SINCE", http.StatusBadRequest, "updated_since must be an RFC3339 timestamp")
			return
		}
		updatedSince = &parsed
	}
	listQuery := listIssuesQuery
	arguments := []any{project, status, parent, updatedSince}
	if pinned {
		listQuery = listPinnedIssuesQuery
		arguments = append(arguments, login)
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), listQuery, arguments...)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	issues := []model.IssueSummary{}
	for rows.Next() {
		var issue model.IssueSummary
		if err := rows.Scan(&issue.Key, &issue.Title, &issue.Status, &issue.Labels, &issue.Parent, &issue.UpdatedAt, &issue.LastSeq, &issue.OpenAsks); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, issues)
}

func (s *server) createIssue(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Project  string       `json:"project"`
		Title    string       `json:"title"`
		Parent   *string      `json:"parent"`
		External string       `json:"external"`
		Force    bool         `json:"force"`
		Spec     *string      `json:"spec"`
		Actor    *model.Actor `json:"actor"`
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
	if input.External == "" && !input.Force {
		candidates, err := s.duplicateCandidates(r.Context(), s.deps.Store.Pool, input.Project, input.Title, parentKey)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if len(candidates) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
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
	if parentKey != "" {
		if err := tx.QueryRow(r.Context(), `select key from issues where key = $1`, parentKey).Scan(new(string)); err != nil {
			s.writeHandlerError(w, err)
			return
		}
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
	labels := []string{}
	if usingDefaultProject {
		labels = []string{repoLabelPrefix + externalRepo}
	}
	if _, err := tx.Exec(r.Context(), `
		insert into issues (key, project_key, number, title, parent_key, created_by, labels)
		values ($1, $2, $3, $4, $5, $6, $7)
	`, key, input.Project, number, input.Title, parent, actorJSON, labels); err != nil {
		s.writeHandlerError(w, err)
		return
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
			writeJSON(w, http.StatusOK, existing)
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
	markdown, err = s.deps.Docs.SeedText(r.Context(), tx, artifactID, markdown)
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
	if err := s.replaceRefs(r.Context(), tx, "artifact", artifactID, markdown); err != nil {
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
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(event)
	writeJSON(w, http.StatusCreated, issue)
}

func isUniqueViolation(err error) bool {
	var pgError *pgconn.PgError
	return errors.As(err, &pgError) && pgError.Code == "23505"
}

func (s *server) getIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	issue, err := s.loadIssue(r.Context(), s.deps.Store.Pool, r.PathValue("key"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	artifacts, err := s.loadArtifacts(r.Context(), s.deps.Store.Pool, issue.Key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	openAsks, err := s.loadOpenAsks(r.Context(), s.deps.Store.Pool, issue.Key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	children, err := s.loadChildren(r.Context(), s.deps.Store.Pool, issue.Key)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		model.Issue
		Artifacts []model.Artifact   `json:"artifacts"`
		OpenAsks  []model.Ask        `json:"open_asks"`
		Children  []model.IssueChild `json:"children"`
	}{Issue: issue, Artifacts: artifacts, OpenAsks: openAsks, Children: children})
}

// loadOpenAsks returns the issue's unanswered asks, oldest first. The issue
// listing carries only a count; the detail response carries the asks themselves
// so agents, which cannot read the human inbox, can see what is waiting.
func (s *server) loadOpenAsks(ctx context.Context, q queryer, key string) ([]model.Ask, error) {
	return s.loadIssueAsks(ctx, q, key, "open")
}

func (s *server) patchIssue(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title         *string               `json:"title"`
		Status        *string               `json:"status"`
		Labels        *[]string             `json:"labels"`
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

	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	key := r.PathValue("key")
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
		if status == "" || status == "done" || input.Title != nil || input.Labels != nil || input.Route != nil || input.ExternalLinks != nil {
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
	if input.Labels != nil {
		if _, err := tx.Exec(r.Context(), `update issues set labels = $2, updated_at = now() where key = $1`, key, *input.Labels); err != nil {
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

func (s *server) resolveIssue(w http.ResponseWriter, r *http.Request) {
	if !s.requireAuthenticated(w, r) {
		return
	}
	key, err := s.resolveIssueRef(r.Context(), r.URL.Query().Get("ref"))
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

// resolveIssueRef resolves an existing native key or external GitHub link.
// Creating a missing external issue is the responsibility of POST /issues.
func (s *server) resolveIssueRef(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if issueKeyPattern.MatchString(ref) {
		var key string
		err := s.deps.Store.Pool.QueryRow(ctx, `select key from issues where key = $1`, ref).Scan(&key)
		if err != nil {
			return "", err
		}
		return key, nil
	}
	repo, number, err := externalRef(ref)
	if err != nil {
		return "", err
	}
	var key string
	err = s.deps.Store.Pool.QueryRow(ctx, `
		select issue_key from issue_external_links where url = $1
	`, externalURL(repo, number)).Scan(&key)
	if err != nil {
		return "", err
	}
	return key, nil
}

func (s *server) loadIssue(ctx context.Context, q queryer, key string) (model.Issue, error) {
	var issue model.Issue
	var createdBy []byte
	if err := q.QueryRow(ctx, `
		select i.key, i.project_key, i.number, i.title, i.status, i.labels, i.parent_key, i.route,
		       i.created_by, i.created_at, i.updated_at, i.closed_at,
		       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and a.is_primary), ''),
		       i.last_seq
		from issues i
		where i.key = $1
	`, key).Scan(
		&issue.Key, &issue.Project, &issue.Number, &issue.Title, &issue.Status, &issue.Labels,
		&issue.Parent, &issue.Route, &createdBy, &issue.CreatedAt, &issue.UpdatedAt, &issue.ClosedAt,
		&issue.PrimaryArtifactID, &issue.LastSeq,
	); err != nil {
		return model.Issue{}, err
	}
	if err := json.Unmarshal(createdBy, &issue.CreatedBy); err != nil {
		return model.Issue{}, fmt.Errorf("decode issue actor: %w", err)
	}
	rows, err := q.Query(ctx, `select url, kind from issue_external_links where issue_key = $1 order by url`, key)
	if err != nil {
		return model.Issue{}, err
	}
	defer rows.Close()
	issue.ExternalLinks = []model.ExternalLink{}
	for rows.Next() {
		var link model.ExternalLink
		if err := rows.Scan(&link.URL, &link.Kind); err != nil {
			return model.Issue{}, err
		}
		issue.ExternalLinks = append(issue.ExternalLinks, link)
	}
	if err := rows.Err(); err != nil {
		return model.Issue{}, err
	}
	return issue, nil
}

func (s *server) loadChildren(ctx context.Context, q queryer, key string) ([]model.IssueChild, error) {
	rows, err := q.Query(ctx, `select key, title, status from issues where parent_key = $1 order by key`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := []model.IssueChild{}
	for rows.Next() {
		var child model.IssueChild
		if err := rows.Scan(&child.Key, &child.Title, &child.Status); err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, rows.Err()
}

func validateExternalURL(raw string) error {
	value, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || !value.IsAbs() || value.Host == "" || (value.Scheme != "http" && value.Scheme != "https") {
		return errorf(http.StatusBadRequest, "INVALID_URL", "external link URL must be an absolute HTTP(S) URL")
	}
	return nil
}
