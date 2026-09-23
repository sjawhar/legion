package api

import (
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type inboxIssue struct {
	Key      string  `json:"key"`
	Title    string  `json:"title"`
	Assignee *string `json:"assignee"`
}

type inboxDocument struct {
	Project string `json:"project"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
}

type inboxAskThread struct {
	Replies   []model.Comment     `json:"replies"`
	Edits     []model.AskEdit     `json:"edits"`
	Followers []model.AskFollower `json:"followers"`
}

type inboxAsk struct {
	model.Ask
	Issue     *inboxIssue         `json:"issue,omitempty"`
	Document  *inboxDocument      `json:"document,omitempty"`
	Priority  *int                `json:"priority"`
	LastReply *model.AskLastReply `json:"last_reply"`
	Thread    inboxAskThread      `json:"thread"`
}

// inboxAssigneeFilter turns ?assignee= into the SQL mode and login the inbox query binds:
// "" (no filter), "unassigned", or "login" with the canonical allowlisted login. `me` is the
// caller; a named login is canonicalised first so a shared `?assignee=Alice` link never 400s.
func (s *server) inboxAssigneeFilter(raw string, caller model.Actor) (mode, login string, err error) {
	switch value := strings.TrimSpace(raw); value {
	case "":
		return "", "", nil
	case "unassigned":
		return "unassigned", "", nil
	case "me":
		return "login", canonicalLogin(caller.ID), nil
	default:
		login, err := s.allowedLogin(value)
		if err != nil {
			return "", "", err
		}
		return "login", login, nil
	}
}

func (s *server) listInbox(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	assigneeMode, assigneeLogin, err := s.inboxAssigneeFilter(r.URL.Query().Get("assignee"), caller)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+askReadColumns+`,
		       i.key, i.title, i.assignee, i.priority, ar.project_key, ar.slug, ar.name
		`+askReadFrom+`
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		where a.state = 'open'
		  and (i.key is null or i.closed_at is null)
		  and ($1 = '' or coalesce(i.project_key, ar.project_key) = $1)
		  and ($2 = ''
		       or ($2 = 'unassigned' and (i.key is null or i.assignee is null))
		       or ($2 = 'login' and i.assignee = $3))
		order by i.priority asc nulls last, coalesce(lr.turn, 'human') = 'agent' asc,
		         coalesce(lr.created_at, a.created_at) desc, a.id desc
	`, project, assigneeMode, assigneeLogin)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	asks := []inboxAsk{}
	for rows.Next() {
		var ask inboxAsk
		var issueKey, issueTitle, issueAssignee, documentProject, documentSlug, documentName *string
		row, reply, err := scanAskRead(rows, &issueKey, &issueTitle, &issueAssignee, &ask.Priority, &documentProject, &documentSlug, &documentName)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		ask.Ask = row
		ask.LastReply = reply
		if issueKey != nil {
			ask.Issue = &inboxIssue{Key: *issueKey, Title: *issueTitle, Assignee: issueAssignee}
		}
		if documentProject != nil {
			ask.Document = &inboxDocument{Project: *documentProject, Slug: *documentSlug, Name: *documentName}
		}
		asks = append(asks, ask)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	askPointers := make([]*model.Ask, len(asks))
	askIDs := make([]string, len(asks))
	for index := range asks {
		askPointers[index] = &asks[index].Ask
		askIDs[index] = asks[index].ID
	}
	if err := s.attachOpenedEventIDs(r.Context(), s.deps.Store.Pool, askPointers); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := attachAskBacklinkCounts(r.Context(), s.deps.Store.Pool, askPointers); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	threads, err := s.loadInboxAskThreads(r.Context(), s.deps.Store.Pool, askIDs)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for index := range asks {
		asks[index].Thread = threads[asks[index].ID]
	}
	WriteJSON(w, http.StatusOK, asks)
}
