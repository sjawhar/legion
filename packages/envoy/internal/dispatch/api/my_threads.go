package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// One named query reads membership, actual activity, page, and total. The authored
// arm of graph_thread_participation uses user_threads as its existing index;
// mentioned/addressed-only membership is derived, never written into that index.
const myThreadsQuery = `
 with membership as materialized (
   select thread_kind, thread_id, array_agg(distinct cause order by cause) as causes
   from graph_thread_participation where person = $1
   group by thread_kind, thread_id
 ), items as materialized (
   select p.thread_kind as kind, p.thread_id::text as thread_id, p.causes,
          coalesce(a.question, c.body, m.body, '') as subject,
          dispatch_thread_activity(p.thread_kind, p.thread_id) as activity_at,
          coalesce(a.issue_key, c.issue_key, m.issue_key) as issue_key,
          ar.project_key as project, ar.slug as document,
          case
            when coalesce(a.issue_key, c.issue_key, m.issue_key) is not null then
              '/issues/' || coalesce(a.issue_key, c.issue_key, m.issue_key) || '/' ||
              case p.thread_kind when 'ask' then 'asks' when 'comment' then 'comments' else 'messages' end || '/' || p.thread_id::text
            when ar.id is not null then '/projects/' || ar.project_key || '/documents/' || ar.slug || '?' || p.thread_kind || '=' || p.thread_id::text
            when m.target like 'session:%' then '/agents/' || substr(m.target, length('session:') + 1) || '/live'
          end as href,
          coalesce(u.unread, false) as unread
   from membership p
   left join asks a on p.thread_kind = 'ask' and a.id = p.thread_id
   left join comments c on p.thread_kind = 'comment' and c.id = p.thread_id
   left join messages m on p.thread_kind = 'message' and m.id = p.thread_id
   left join artifacts ar on ar.id = coalesce(a.artifact_id, c.artifact_id)
   left join user_threads u on u.login = $1 and u.thread_id = p.thread_id
   where coalesce(a.id, c.id, m.id) is not null
 ), page as (
   select * from items where href is not null
   order by activity_at desc, kind, thread_id desc limit $2 offset $3
 )
 select (select count(*)::int from items where href is not null),
        coalesce(jsonb_agg(to_jsonb(page) order by activity_at desc, kind, thread_id desc), '[]'::jsonb)
 from page`

type myThread struct {
	Kind       string   `json:"kind"`
	ThreadID   string   `json:"thread_id"`
	Causes     []string `json:"causes"`
	Subject    string   `json:"subject"`
	ActivityAt string   `json:"activity_at"`
	IssueKey   *string  `json:"issue_key"`
	Project    *string  `json:"project"`
	Document   *string  `json:"document"`
	Href       string   `json:"href"`
	Unread     bool     `json:"unread"`
}

type myThreadsPage struct {
	Person string     `json:"person"`
	Rows   []myThread `json:"rows"`
	Total  int        `json:"total"`
	Limit  int        `json:"limit"`
	Offset int        `json:"offset"`
}

func (s *server) listMyThreads(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireOwnThreads(w, r, true)
	if !ok {
		return
	}
	limit, offset, err := parseMyAnswersPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	response := myThreadsPage{Person: model.CanonicalLogin(caller.ID), Rows: []myThread{}, Limit: limit, Offset: offset}
	var raw []byte
	if err := s.deps.Store.Pool.QueryRow(r.Context(), myThreadsQuery, response.Person, limit, offset).Scan(&response.Total, &raw); err != nil {
		s.writeHandlerError(w, fmt.Errorf("list my threads: %w", err))
		return
	}
	if err := json.Unmarshal(raw, &response.Rows); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	for i := range response.Rows {
		response.Rows[i].Subject = contracts.OneLineSummary(response.Rows[i].Subject)
	}
	WriteJSON(w, http.StatusOK, response)
}

func (s *server) requireOwnThreads(w http.ResponseWriter, r *http.Request, page bool) (model.Actor, bool) {
	caller, ok := s.requireHuman(w, r)
	if !ok {
		return caller, false
	}
	for key := range r.URL.Query() {
		if !page || (key != "limit" && key != "offset") {
			writeError(w, "OWN_THREADS_ONLY", http.StatusBadRequest, "this read is the signed-in person's own threads; identity and other filters are not accepted")
			return caller, false
		}
	}
	return caller, true
}
