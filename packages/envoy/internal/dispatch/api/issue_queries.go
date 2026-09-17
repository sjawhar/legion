package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// issueStatusOrderSQL derives the SQL ordering from Dispatch's canonical lifecycle.
func issueStatusOrderSQL() string {
	var order strings.Builder
	order.WriteString("case i.status")
	for index, status := range model.IssueStatuses {
		fmt.Fprintf(&order, "\n\t\twhen '%s' then %d", status, index+1)
	}
	order.WriteString("\n\t\tend")
	return order.String()
}

var issueStatusCase = issueStatusOrderSQL()

// listIssuesQuery aggregates each issue's open-ask count with a single
// query. The state filter lives in the join condition, not a WHERE/FILTER
// clause on the joined rows, so it matches the partial asks_open(issue_key)
// where state = 'open' index instead of forcing a sequential scan of asks.
// The components lateral yields one row per issue, so grouping by its columns
// with the key adds no rows. listPinnedIssuesQuery is the same query joined
// to the caller's pinned rows ($7 is the login).
var listIssuesQuery = issueSummaryHead + issueSummaryTail

var listPinnedIssuesQuery = issueSummaryHead + `
	join user_issue_state s on s.issue_key = i.key and s.login = $7 and s.pinned` + issueSummaryTail

const issueSummaryHead = `
	select i.key, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key, i.assignee, i.updated_at, i.last_seq,
	       count(a.id) filter (where i.closed_at is null),
	       ` + issueComponentsColumns + `
	from issues i`

var issueSummaryTail = `
	left join asks a on a.issue_key = i.key and a.state = 'open'
	` + issueComponentsLateral + `
	where ($1 = '' or i.project_key = $1)
	  and ($2 = '' or i.status = $2)
	  and ($3 = '' or i.parent_key = $3)
	  and ($4::timestamptz is null or i.updated_at >= $4)
	  and ($5::text[] = '{}' or (select array_agg(lower(label)) from unnest(i.labels) as label) @> $5)
	  and (not $6::boolean or i.closed_at is null)
	group by i.key, ` + issueComponentsColumns + `
	order by ` + issueStatusCase + `, i.rank asc, i.created_at asc
`

const (
	maxIssueLabels  = 20
	maxIssueLabel16 = 40
)

func normalizeIssueLabels(values []string) ([]string, error) {
	if len(values) > maxIssueLabels {
		return nil, countExceededError("LABELS_INPUT", "labels", len(values), maxIssueLabels)
	}
	labels := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		label := strings.TrimSpace(value)
		if length := len16(label); length == 0 || length > maxIssueLabel16 {
			return nil, errorf(http.StatusBadRequest, "LABELS_INPUT", "each label must be 1 to %d characters", maxIssueLabel16)
		}
		key := strings.ToLower(label)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		labels = append(labels, label)
	}
	return labels, nil
}

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
	labels, err := normalizeIssueLabels(query["label"])
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for index, label := range labels {
		labels[index] = strings.ToLower(label)
	}
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
	open := query.Get("open") == "true"
	arguments := []any{project, status, parent, updatedSince, labels, open}
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
		var components componentsScan
		targets := []any{&issue.Key, &issue.Title, &issue.Status, &issue.Priority, &issue.Rank, &issue.Labels, &issue.Parent, &issue.Assignee, &issue.UpdatedAt, &issue.LastSeq, &issue.OpenAsks}
		if err := rows.Scan(append(targets, components.targets()...)...); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		issue.Components = components.resolve(issue.Key)
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, issues)
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
	WriteJSON(w, http.StatusOK, struct {
		model.Issue
		Artifacts []model.Artifact   `json:"artifacts"`
		OpenAsks  []issueOpenAsk     `json:"open_asks"`
		Children  []model.IssueChild `json:"children"`
	}{Issue: issue, Artifacts: artifacts, OpenAsks: openAsks, Children: children})
}

// issueOpenAsk is an open ask on the issue detail together with the newest
// comment in its thread — the same last_reply the inbox row carries — so the
// issue header can show who spoke last beside the ask's waiting_on.
type issueOpenAsk struct {
	model.Ask
	LastReply *model.AskLastReply `json:"last_reply"`
}

// loadOpenAsks returns the issue's unanswered asks, oldest first, each with the
// newest reply in its thread (null when nobody has replied). The issue listing
// carries only a count; the detail response carries the asks themselves so
// agents, which cannot read the human inbox, can see what is waiting.
func (s *server) loadOpenAsks(ctx context.Context, q queryer, key string) ([]issueOpenAsk, error) {
	asks, replies, err := s.queryOwnerAsks(ctx, q, issueOwner(key), "open")
	if err != nil {
		return nil, err
	}
	openAsks := make([]issueOpenAsk, len(asks))
	for index, ask := range asks {
		openAsks[index] = issueOpenAsk{Ask: ask}
		if reply, ok := replies[ask.ID]; ok {
			openAsks[index].LastReply = &reply
		}
	}
	return openAsks, nil
}

func (s *server) loadIssue(ctx context.Context, q queryer, key string) (model.Issue, error) {
	var issue model.Issue
	var createdBy []byte
	var components componentsScan
	targets := []any{
		&issue.Key, &issue.Project, &issue.Number, &issue.Title, &issue.Status, &issue.Priority, &issue.Rank, &issue.Labels,
		&issue.Parent, &issue.Assignee, &issue.Route, &createdBy, &issue.CreatedAt, &issue.UpdatedAt, &issue.ClosedAt,
		&issue.PrimaryArtifactID, &issue.LastSeq,
	}
	if err := q.QueryRow(ctx, `
		select i.key, i.project_key, i.number, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key, i.assignee, i.route,
		       i.created_by, i.created_at, i.updated_at, i.closed_at,
		       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and a.is_primary), ''),
		       i.last_seq,
		       `+issueComponentsColumns+`
		from issues i
		`+issueComponentsLateral+`
		where i.key = $1
	`, key).Scan(append(targets, components.targets()...)...); err != nil {
		return model.Issue{}, err
	}
	issue.Components = components.resolve(issue.Key)
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

// loadChildren returns the issue's direct children, each with a rollup over its whole
// subtree (the child itself included, every status): done/total counts and the newest
// updated_at. The recursive walk uses `union` with a depth cap like refs.Closure, so it
// terminates even if a raced reparent ever commits a cycle.
func (s *server) loadChildren(ctx context.Context, q queryer, key string) ([]model.IssueChild, error) {
	rows, err := q.Query(ctx, `
		with recursive subtree as (
			select key as root, key, 1 as depth from issues where parent_key = $1
			union
			select s.root, i.key, s.depth + 1
			from issues i join subtree s on i.parent_key = s.key
			where s.depth < 32
		), rollup as (
			select s.root,
			       count(distinct s.key) as total,
			       count(distinct s.key) filter (where i.status = 'done') as done,
			       max(i.updated_at) as active_at
			from subtree s join issues i on i.key = s.key
			group by s.root
		)
		select c.key, c.title, c.status, r.done, r.total, r.active_at,
		       coalesce((select json_agg(json_build_object('url', l.url, 'kind', l.kind) order by l.url)
		                 from issue_external_links l where l.issue_key = c.key), '[]')
		from issues c join rollup r on r.root = c.key
		where c.parent_key = $1 order by c.key
	`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	children := []model.IssueChild{}
	for rows.Next() {
		var child model.IssueChild
		var links []byte
		if err := rows.Scan(&child.Key, &child.Title, &child.Status,
			&child.SubtreeDone, &child.SubtreeTotal, &child.ActiveAt, &links); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(links, &child.ExternalLinks); err != nil {
			return nil, err
		}
		children = append(children, child)
	}
	return children, rows.Err()
}
