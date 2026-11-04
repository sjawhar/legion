package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/refs"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

type personNode struct {
	Kind      string    `json:"kind"`
	ID        string    `json:"id"`
	Relations []string  `json:"relations"`
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	Ref       *string   `json:"ref"`
	Href      *string   `json:"href"`
}

type personNodes struct {
	Person string       `json:"person"`
	Rows   []personNode `json:"rows"`
	Total  int          `json:"total"`
	Limit  int          `json:"limit"`
	Offset int          `json:"offset"`
}

const personNodesQuery = `
	with related as (
		select from_kind, from_id, array_agg(distinct kind order by kind) as relations from (
			select g.from_kind, g.from_id, g.kind from ` + refs.AllEdgesSQL + ` g
			where g.kind = 'authored_by' and g.to_kind = 'person' and g.to_id = $1
			  and g.from_kind in ('comment', 'message', 'ask')
			union all
			select g.from_kind, g.from_id, g.kind from person_aliases a
			join ` + refs.AllEdgesSQL + ` g on g.to_kind = 'session' and g.to_id = a.alias and g.kind = 'delivered_to'
			where a.person = $1 and a.alias_kind = 'session' and g.from_kind in ('comment', 'message', 'ask')
		) edges group by from_kind, from_id
	), items as (
		select 'comment' as kind, c.id::text as id, c.body as title, c.created_at, r.relations,
		       c.issue_key, a.project_key as project, a.slug, null::text as target
		from related r join comments c on r.from_kind = 'comment' and r.from_id = c.id::text
		left join artifacts a on a.id = c.artifact_id
		union all
		select 'message', m.id::text, m.body, m.created_at, r.relations, m.issue_key, null, null, m.target
		from related r join messages m on r.from_kind = 'message' and r.from_id = m.id::text
		union all
		select 'ask', k.id::text, k.question, k.created_at, r.relations, k.issue_key, a.project_key, a.slug, null
		from related r join asks k on r.from_kind = 'ask' and r.from_id = k.id::text
		left join artifacts a on a.id = k.artifact_id
	), page as (
		select * from items order by created_at desc, kind, id desc limit $2 offset $3
	)
	select totals.total, p.kind, p.id, p.title, p.created_at, p.relations, p.issue_key, p.project, p.slug, p.target
	from (select count(*)::int as total from items) totals left join page p on true
	order by p.created_at desc, p.kind, p.id desc`

// listPersonNodes is a named graph read, not a graph traversal API. Like listAgentMessages it
// is human-only: people may read direct conversations, while a bearer cannot use a person's
// alias to reach another session's conversation. Session ownership affects receipt, not who
// authored an item; an agent's own words remain authored by that session.
func (s *server) listPersonNodes(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireHuman(w, r); !ok {
		return
	}
	limit, offset, err := parseMyAnswersPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	login := model.CanonicalLogin(r.PathValue("login"))
	var exists bool
	if err := s.deps.Store.Pool.QueryRow(r.Context(), `select exists (select 1 from person_aliases where person = $1 and alias_kind = 'login')`, login).Scan(&exists); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if !exists {
		writeError(w, "PERSON_NOT_FOUND", http.StatusNotFound, "person not found")
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), personNodesQuery, login, limit, offset)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("read person nodes: %w", err))
		return
	}
	defer rows.Close()
	response := personNodes{Person: login, Rows: []personNode{}, Limit: limit, Offset: offset}
	for rows.Next() {
		var kind, id, body, issue, project, slug, target *string
		var createdAt *time.Time
		var relations []string
		if err := rows.Scan(&response.Total, &kind, &id, &body, &createdAt, &relations, &issue, &project, &slug, &target); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		if id == nil {
			continue
		}
		first, _, _ := strings.Cut(*body, "\n")
		item := personNode{Kind: *kind, ID: *id, Relations: relations, Title: text.HeadRunes(first, 240), CreatedAt: createdAt.UTC()}
		switch {
		case issue != nil:
			item.Ref = new("dispatch://" + *issue + "/" + *kind + "/" + *id)
			item.Href = new("/issues/" + *issue + "/" + *kind + "s/" + *id)
		case project != nil && slug != nil:
			item.Ref = new("dispatch://" + *project + "/artifact/" + *slug + "/" + *kind + "/" + *id)
			item.Href = new("/projects/" + *project + "/documents/" + url.PathEscape(*slug) + "?" + *kind + "=" + *id)
		case *kind == "message" && target != nil && strings.HasPrefix(*target, "session:"):
			item.Href = new("/agents/" + url.PathEscape(strings.TrimPrefix(*target, "session:")) + "/live")
		}
		response.Rows = append(response.Rows, item)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, response)
}
