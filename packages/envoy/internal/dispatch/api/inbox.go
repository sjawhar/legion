package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

type inboxAsk struct {
	model.Ask
	Issue struct {
		Key   string `json:"key"`
		Title string `json:"title"`
	} `json:"issue"`
}

func (s *server) listInbox(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireActor(w, r, nil); !ok {
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	rows, err := s.deps.Store.Pool.Query(r.Context(), `
		select a.id::text, a.issue_key, a.author, a.question, a.options, a.multiple, a.custom, a.urgency,
		       a.anchor, a.state, a.answer, a.created_at, i.key, i.title
		from asks a
		join issues i on i.key = a.issue_key
		where a.state = 'open' and i.closed_at is null and ($1 = '' or i.project_key = $1)
		order by a.created_at desc, a.id desc
	`, project)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer rows.Close()
	asks := []inboxAsk{}
	for rows.Next() {
		var ask inboxAsk
		var author, options, anchor, answer []byte
		if err := rows.Scan(
			&ask.ID, &ask.IssueKey, &author, &ask.Question, &options, &ask.Multiple, &ask.Custom, &ask.Urgency,
			&anchor, &ask.State, &answer, &ask.CreatedAt, &ask.Issue.Key, &ask.Issue.Title,
		); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if err := decodeInboxAsk(&ask.Ask, author, options, anchor, answer); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		asks = append(asks, ask)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, asks)
}

func decodeInboxAsk(ask *model.Ask, author, options, anchor, answer []byte) error {
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
	return nil
}
