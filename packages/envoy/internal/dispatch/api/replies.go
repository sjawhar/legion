package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The Replies page lists every thread a person wrote in - an ask's thread, a comment thread by its
// root, an issue message thread by its root - where someone else has written since the person last
// read it. user_threads (migration 0095) keeps one row per person per thread, maintained by the
// triggers 0095-0097 install, so these reads touch only the person's unread rows through the
// partial index user_threads_unread.

// myRepliesUnread is the filter both reads share: the caller's ($1) unread rows, narrowed to one
// issue ($2, empty for any) or one project document ($3, null for any), less an open ask the Inbox
// lists, whose row in Waiting on you already shows its reply (api/inbox.go: an open ask on an issue
// that is not closed, or on a project document). An open ask on a closed issue stays, since the
// Inbox hides it and nothing else would show its reply.
const myRepliesUnread = `
	from user_threads u
	left join asks a on u.kind = 'ask' and a.id = u.thread_id
	left join issues ai on u.kind = 'ask' and ai.key = u.issue_key
	where u.login = $1 and u.unread
	  and ($2::text = '' or u.issue_key = $2::text)
	  and ($3::uuid is null or u.artifact_id = $3::uuid)
	  and not coalesce(a.state = 'open' and (u.issue_key is null or ai.closed_at is null), false)`

// myRepliesCountQuery counts the caller's unread threads: the page's total and the Replies count.
// It reads the unread rows into a materialized set first, so the planner reads them through
// user_threads_unread rather than misjudging the stored flag's selectivity.
const myRepliesCountQuery = `
	with unread as materialized (
		select u.thread_id ` + myRepliesUnread + `
	)
	select count(*) from unread`

// myRepliesPageQuery reads one page of the caller's unread threads, newest reply first, and what a
// row shows: who wrote the newest write someone else made and its text (last_reply_id: a comment
// for an ask or a comment thread, a message for a message thread, or the ask itself when that write
// is its answer, read as the answerer and the answer's options and text), what the thread is about
// (the ask's question, or its root's text) and where it lives (the row's own owner columns). Its
// ref always names the thread's root, never the reply: a reply carries no anchor, so a link to it
// would skip the document redirect an anchored root takes.
const myRepliesPageQuery = `
	with page as (
		select u.thread_id, u.kind, u.replied_at, u.last_reply_id, u.issue_key, u.artifact_id
		` + myRepliesUnread + `
		order by u.replied_at desc, u.thread_id desc
		limit $4 offset $5
	)
	select p.thread_id::text, p.kind,
	       case p.kind
	         when 'ask' then ` + askItemRefExpression + `
	         when 'comment' then case
	           when i.key is not null then '/issues/' || i.key || '/comments/' || p.thread_id::text
	           else '/projects/' || ar.project_key || '/documents/' || ar.slug || '?comment=' || p.thread_id::text
	         end
	         else '/issues/' || i.key || '/messages/' || p.thread_id::text
	       end,
	       i.key, i.title, ar.project_key, ar.slug, ar.name,
	       coalesce(a.question, root.body, mroot.body, ''),
	       p.last_reply_id::text, answered.id is not null,
	       coalesce(rc.author, rm.author,
	         case when answered.id is not null then jsonb_build_object('kind', 'user', 'id', answered.answer->>'user') end),
	       coalesce(rc.body, rm.body, answered.text, ''),
	       coalesce(rc.created_at, rm.created_at, p.replied_at)
	from page p
	left join asks a on p.kind = 'ask' and a.id = p.thread_id
	left join comments root on p.kind = 'comment' and root.id = p.thread_id
	left join messages mroot on p.kind = 'message' and mroot.id = p.thread_id
	left join comments rc on p.kind <> 'message' and rc.id = p.last_reply_id
	left join messages rm on p.kind = 'message' and rm.id = p.last_reply_id
	left join lateral (
		select a.id, a.answer,
		       concat_ws(' - ',
		         nullif(array_to_string(array(select jsonb_array_elements_text(
		           case when jsonb_typeof(a.answer->'selected') = 'array' then a.answer->'selected' else '[]'::jsonb end
		         )), ', '), ''),
		         nullif(a.answer->>'text', '')) as text
		where p.last_reply_id = a.id and a.answer->>'user' is not null
	) answered on true
	left join issues i on i.key = p.issue_key
	left join artifacts ar on ar.id = p.artifact_id
	order by p.replied_at desc, p.thread_id desc`

// parseMyRepliesOwner reads the optional ?issue= or ?artifact= that narrows the Replies rows to one
// thread owner: the read a mounted thread makes to learn whether it is unread. Both together are
// refused, as is an artifact that is not a uuid.
func parseMyRepliesOwner(r *http.Request) (issue string, artifact *string, err error) {
	query := r.URL.Query()
	issue = strings.TrimSpace(query.Get("issue"))
	rawArtifact := strings.TrimSpace(query.Get("artifact"))
	if issue != "" && rawArtifact != "" {
		return "", nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "name issue or artifact, not both")
	}
	if rawArtifact != "" {
		parsed, err := uuid.Parse(rawArtifact)
		if err != nil {
			return "", nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "artifact must be a document id")
		}
		canonical := parsed.String()
		artifact = &canonical
	}
	return issue, artifact, nil
}

// listMyReplies answers GET /api/v1/me/replies: one page of the caller's unread threads and how
// many there are in all.
func (s *server) listMyReplies(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	limit, offset, err := parseMyAnswersPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue, artifact, err := parseMyRepliesOwner(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	login := model.CanonicalLogin(caller.ID)
	response := model.MyRepliesResponse{Rows: []model.MyReplyRow{}, Limit: limit, Offset: offset}
	if err := s.deps.Store.Pool.QueryRow(r.Context(), myRepliesCountQuery, login, issue, artifact).Scan(&response.Total); err != nil {
		s.writeHandlerError(w, fmt.Errorf("count my replies: %w", err))
		return
	}
	rows, err := s.deps.Store.Pool.Query(r.Context(), myRepliesPageQuery, login, issue, artifact, limit, offset)
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("list my replies: %w", err))
		return
	}
	defer rows.Close()
	for rows.Next() {
		var row model.MyReplyRow
		var issueKey, issueTitle, project, slug, name, replyID *string
		var subject, body string
		var author []byte
		var isAnswer bool
		if err := rows.Scan(
			&row.ThreadID, &row.Kind, &row.Ref,
			&issueKey, &issueTitle, &project, &slug, &name,
			&subject, &replyID, &isAnswer, &author, &body, &row.Reply.CreatedAt,
		); err != nil {
			s.writeHandlerError(w, fmt.Errorf("scan my reply: %w", err))
			return
		}
		row.Reply.Kind = "reply"
		if isAnswer {
			row.Reply.Kind = "answer"
		}
		if issueKey != nil {
			row.Issue = &model.OpenAskIssue{Key: *issueKey, Title: *issueTitle}
		} else if project != nil {
			row.Document = &model.OpenAskDocument{Project: *project, Slug: *slug, Name: *name}
		}
		row.Subject = contracts.OneLineSummary(subject)
		if replyID != nil {
			row.Reply.ID = *replyID
		}
		if author != nil {
			if err := json.Unmarshal(author, &row.Reply.Author); err != nil {
				s.writeHandlerError(w, fmt.Errorf("decode my reply author: %w", err))
				return
			}
		}
		row.Reply.FirstLine = contracts.OneLineSummary(body)
		response.Rows = append(response.Rows, row)
	}
	if err := rows.Err(); err != nil {
		s.writeHandlerError(w, fmt.Errorf("list my replies: %w", err))
		return
	}
	WriteJSON(w, http.StatusOK, response)
}

// putThreadRead answers PUT /api/v1/me/threads/{id}/read: it moves the caller's read mark on one
// thread forward to read_through (stored as no later than now, as the Agents page's read mark is),
// so a row a tab read less of a moment ago never comes back. A thread the caller has no row for
// writes nothing.
//
// The transaction locks the thread's owner row `for share` before it touches the caller's row:
// every writer that moves a user_threads row (a comment, an answer, an issue message) holds that
// owner `for no key update` before its trigger fires, and the answer route can hold the events'
// commit-order lock by then too (docs/marks.go's anchor refresh appends an event before the answer's
// update), so a mark that took the row first could wait on a writer waiting on it. Owner first, then
// the project the event names, is the order every multi-owner transaction takes (LockOwners).
func (s *server) putThreadRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	if !requireUUIDPath(w, r, "thread") {
		return
	}
	var input struct {
		ReadThrough *string `json:"read_through"`
	}
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.ReadThrough == nil {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "read_through is required")
		return
	}
	if _, err := parseAgentStateCutoff("read_through", input.ReadThrough); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	login := model.CanonicalLogin(actor.ID)
	threadID := r.PathValue("id")
	tx, err := s.begin(r.Context())
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	defer tx.Rollback(r.Context())
	var project string
	var issueKey, artifactID *string
	err = tx.QueryRow(r.Context(), `
		select project_key, issue_key, artifact_id::text from user_threads where login = $1 and thread_id = $2
	`, login, threadID).Scan(&project, &issueKey, &artifactID)
	if errors.Is(err, pgx.ErrNoRows) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A closed issue is not refused: reading a thread on a closed issue is reading it.
	if issueKey != nil {
		_, err = tx.Exec(r.Context(), `select 1 from issues where key = $1 for share`, *issueKey)
	} else if artifactID != nil {
		_, err = tx.Exec(r.Context(), `select 1 from artifacts where id = $1 for share`, *artifactID)
	}
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// read_through is cast by Postgres from the caller's own text rather than bound as a parsed
	// time: an answer's time carries nanoseconds, which the trigger's cast of the answer rounds to
	// the microsecond while pgx truncates a bound time, so a mark through the answer's own time
	// would otherwise fall a microsecond short of the answer and leave the thread unread.
	moved, err := tx.Exec(r.Context(), `
		update user_threads set read_through = least($3::text::timestamptz, now())
		where login = $1 and thread_id = $2 and read_through < least($3::text::timestamptz, now())
	`, login, threadID, *input.ReadThrough)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// A revisit that moves nothing announces nothing: the thread on screen marks itself on every
	// visit, and each event would refetch the count in every tab the viewer has open.
	if moved.RowsAffected() == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	event, err := s.appendEvent(r.Context(), tx, projectOwner(project).event(
		"user_thread_state.updated", actor, model.UserThreadStateEventPayload{Login: login, ThreadID: threadID},
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
	w.WriteHeader(http.StatusNoContent)
}
