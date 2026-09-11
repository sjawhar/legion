package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

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
	LastReply *model.AskLastReply `json:"last_reply"`
}

func (s *server) listInbox(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select a.id::text, a.issue_key, a.artifact_id::text, a.author, a.question, a.options, a.multiple, a.urgency,
		       a.anchor, a.state, a.answer, a.resolution, a.created_at, a.edited_at,
		       i.key, i.title, ar.project_key, ar.slug, ar.name,
		       lr.author, lr.created_at
		from asks a
		left join issues i on i.key = a.issue_key
		left join artifacts ar on ar.id = a.artifact_id
		left join lateral (
			select c.author, c.created_at from comments c
			where c.ask_id = a.id
			order by c.created_at desc, c.id desc
			limit 1
		) lr on true
		where a.state = 'open'
		  and (i.key is null or i.closed_at is null)
		  and ($1 = '' or coalesce(i.project_key, ar.project_key) = $1)
		order by coalesce(lr.author->>'kind' = 'user', false) asc,
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
		var author, options, anchor, answer, resolution []byte
		var issueKey, issueTitle, documentProject, documentSlug, documentName *string
		var editedAt, lastReplyAt *time.Time
		var lastReplyAuthor []byte
		if err := rows.Scan(
			&ask.ID, &ask.IssueKey, &ask.ArtifactID, &author, &ask.Question, &options, &ask.Multiple, &ask.Urgency,
			&anchor, &ask.State, &answer, &resolution, &ask.CreatedAt, &editedAt,
			&issueKey, &issueTitle, &documentProject, &documentSlug, &documentName,
			&lastReplyAuthor, &lastReplyAt,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := decodeInboxAsk(&ask.Ask, author, options, anchor, answer, resolution); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		ask.EditedAt = askTimestampPtr(editedAt)
		if lastReplyAt != nil {
			var reply model.AskLastReply
			if err := json.Unmarshal(lastReplyAuthor, &reply.Author); err != nil {
				s.writeHandlerError(w, fmt.Errorf("decode inbox last reply author: %w", err))
				return
			}
			reply.CreatedAt = *askTimestamp(*lastReplyAt)
			ask.LastReply = &reply
		}
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
	writeJSON(w, http.StatusOK, asks)
}

func decodeInboxAsk(ask *model.Ask, author, options, anchor, answer, resolution []byte) error {
	if err := json.Unmarshal(author, &ask.Author); err != nil {
		return fmt.Errorf("decode inbox ask author: %w", err)
	}
	if err := json.Unmarshal(options, &ask.Options); err != nil {
		return fmt.Errorf("decode inbox ask options: %w", err)
	}
	if ask.Options == nil {
		ask.Options = []model.AskOption{}
	}

	if len(anchor) > 0 {
		var value model.Anchor
		if err := json.Unmarshal(anchor, &value); err != nil {
			return fmt.Errorf("decode inbox ask anchor: %w", err)
		}
		ask.Anchor = &value
	}
	if len(answer) > 0 {
		var value model.AskAnswer
		if err := json.Unmarshal(answer, &value); err != nil {
			return fmt.Errorf("decode inbox ask answer: %w", err)
		}
		ask.Answer = &value
	}
	if len(resolution) > 0 {
		var value model.AskResolution
		if err := json.Unmarshal(resolution, &value); err != nil {
			return fmt.Errorf("decode inbox ask resolution: %w", err)
		}
		ask.Resolution = &value
	}
	return nil
}
