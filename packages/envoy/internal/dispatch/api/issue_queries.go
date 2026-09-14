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
var listIssuesQuery = `
	select i.key, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key, i.updated_at, i.last_seq,
	       count(a.id) filter (where i.closed_at is null)
	from issues i
	left join asks a on a.issue_key = i.key and a.state = 'open'
	where ($1 = '' or i.project_key = $1)
	  and ($2 = '' or i.status = $2)
	  and ($3 = '' or i.parent_key = $3)
	  and ($4::timestamptz is null or i.updated_at >= $4)
	  and ($5::text[] = '{}' or (select array_agg(lower(label)) from unnest(i.labels) as label) @> $5)
	  and (not $6::boolean or i.closed_at is null)
	group by i.key
	order by ` + issueStatusCase + `, i.priority asc nulls last, i.rank asc, i.created_at asc
`

var listPinnedIssuesQuery = `
	select i.key, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key, i.updated_at, i.last_seq,
	       count(a.id) filter (where i.closed_at is null)
	from issues i
	join user_issue_state s on s.issue_key = i.key and s.login = $7 and s.pinned
	left join asks a on a.issue_key = i.key and a.state = 'open'
	where ($1 = '' or i.project_key = $1)
	  and ($2 = '' or i.status = $2)
	  and ($3 = '' or i.parent_key = $3)
	  and ($4::timestamptz is null or i.updated_at >= $4)
	  and ($5::text[] = '{}' or (select array_agg(lower(label)) from unnest(i.labels) as label) @> $5)
	  and (not $6::boolean or i.closed_at is null)
	group by i.key
	order by ` + issueStatusCase + `, i.priority asc nulls last, i.rank asc, i.created_at asc
`

const (
	maxIssueLabels  = 20
	maxIssueLabel16 = 40
)

func normalizeIssueLabels(values []string) ([]string, error) {
	if len(values) > maxIssueLabels {
		return nil, errorf(http.StatusBadRequest, "LABELS_INPUT", "labels length %d exceeds limit %d", len(values), maxIssueLabels)
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
		if err := rows.Scan(&issue.Key, &issue.Title, &issue.Status, &issue.Priority, &issue.Rank, &issue.Labels, &issue.Parent, &issue.UpdatedAt, &issue.LastSeq, &issue.OpenAsks); err != nil {
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
		OpenAsks  []issueOpenAsk     `json:"open_asks"`
		Children  []model.IssueChild `json:"children"`
	}{Issue: issue, Artifacts: artifacts, OpenAsks: openAsks, Children: children})
}

// issueOpenAsk is an open ask on the issue detail together with the newest
// comment in its thread — the same last_reply the inbox row carries — so the
// issue header can tell whose turn it is without the human-only inbox.
type issueOpenAsk struct {
	model.Ask
	LastReply *model.AskLastReply `json:"last_reply"`
}

// loadOpenAsks returns the issue's unanswered asks, oldest first, each with the
// newest reply in its thread (null when nobody has replied). The issue listing
// carries only a count; the detail response carries the asks themselves so
// agents, which cannot read the human inbox, can see what is waiting.
func (s *server) loadOpenAsks(ctx context.Context, q queryer, key string) ([]issueOpenAsk, error) {
	asks, err := s.loadIssueAsks(ctx, q, key, "open")
	if err != nil {
		return nil, err
	}
	replies, err := s.loadAskLastReplies(ctx, q, asks)
	if err != nil {
		return nil, err
	}
	openAsks := make([]issueOpenAsk, len(asks))
	for index, ask := range asks {
		openAsks[index] = issueOpenAsk{Ask: ask, LastReply: replies[ask.ID]}
	}
	return openAsks, nil
}

// loadAskLastReplies returns the newest comment in each ask's thread keyed by
// ask id; an ask nobody has replied to has no entry. It is the reading the
// inbox query makes inline in its lateral join.
func (s *server) loadAskLastReplies(ctx context.Context, q queryer, asks []model.Ask) (map[string]*model.AskLastReply, error) {
	if len(asks) == 0 {
		return nil, nil
	}
	ids := make([]string, len(asks))
	for index, ask := range asks {
		ids[index] = ask.ID
	}
	rows, err := q.Query(ctx, `
		select distinct on (ask_id) ask_id::text, author, created_at
		from comments where ask_id = any($1::uuid[])
		order by ask_id, created_at desc, id desc
	`, ids)
	if err != nil {
		return nil, fmt.Errorf("load ask last replies: %w", err)
	}
	defer rows.Close()
	replies := make(map[string]*model.AskLastReply, len(ids))
	for rows.Next() {
		var askID string
		var author []byte
		var createdAt time.Time
		if err := rows.Scan(&askID, &author, &createdAt); err != nil {
			return nil, err
		}
		var reply model.AskLastReply
		if err := json.Unmarshal(author, &reply.Author); err != nil {
			return nil, fmt.Errorf("decode ask last reply author: %w", err)
		}
		reply.CreatedAt = *askTimestamp(createdAt)
		replies[askID] = &reply
	}
	return replies, rows.Err()
}
func (s *server) loadIssue(ctx context.Context, q queryer, key string) (model.Issue, error) {
	var issue model.Issue
	var createdBy []byte
	if err := q.QueryRow(ctx, `
		select i.key, i.project_key, i.number, i.title, i.status, i.priority, i.rank, i.labels, i.parent_key, i.route,
		       i.created_by, i.created_at, i.updated_at, i.closed_at,
		       coalesce((select a.id::text from artifacts a where a.issue_key = i.key and a.is_primary), ''),
		       i.last_seq
		from issues i
		where i.key = $1
	`, key).Scan(
		&issue.Key, &issue.Project, &issue.Number, &issue.Title, &issue.Status, &issue.Priority, &issue.Rank, &issue.Labels,
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
