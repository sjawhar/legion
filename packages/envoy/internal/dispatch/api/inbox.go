package api

import (
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type inboxIssue struct {
	Key   string `json:"key"`
	Title string `json:"title"`
}

type inboxDocument struct {
	Project string `json:"project"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
}

type inboxAsk struct {
	model.Ask
	Issue     *inboxIssue         `json:"issue,omitempty"`
	Document  *inboxDocument      `json:"document,omitempty"`
	Priority  *int                `json:"priority"`
	LastReply *model.AskLastReply `json:"last_reply"`
}

func (s *server) listInbox(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select `+askReadColumns+`,
		       i.key, i.title, i.priority, ar.project_key, ar.slug, ar.name
		`+askReadFrom+`
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		where a.state = 'open'
		  and (i.key is null or i.closed_at is null)
		  and ($1 = '' or coalesce(i.project_key, ar.project_key) = $1)
		order by coalesce(lr.turn, 'human') = 'agent' asc, i.priority asc nulls last,
		         coalesce(lr.created_at, a.created_at) desc, a.id desc
	`, project)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	asks := []inboxAsk{}
	for rows.Next() {
		var ask inboxAsk
		var issueKey, issueTitle, documentProject, documentSlug, documentName *string
		row, reply, err := scanAskRead(rows, &issueKey, &issueTitle, &ask.Priority, &documentProject, &documentSlug, &documentName)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		ask.Ask = row
		ask.LastReply = reply
		if issueKey != nil {
			ask.Issue = &inboxIssue{Key: *issueKey, Title: *issueTitle}
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
	for index := range asks {
		askPointers[index] = &asks[index].Ask
	}
	if err := s.attachOpenedEventIDs(r.Context(), s.deps.Store.Pool, askPointers); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, asks)
}
