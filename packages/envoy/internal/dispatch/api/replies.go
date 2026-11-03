package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/delivery"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// GET /api/v1/me/replies is the Inbox's thread read, of two kinds of row. A Reply row is a thread
// the person wrote in - an ask's thread, a comment thread by its root, an issue message thread by
// its root - where someone else has written since they last read it; user_threads (migration 0095)
// keeps one row per person per thread, maintained by the triggers 0095-0097 install, so the Reply
// reads touch only the person's own rows, through user_threads_unread under state=unread and
// user_threads_login_replied (migration 0151) under state=all. An Agent row is one of the person's
// one-off messages to an agent session - a BTW, a broadcast's copy, or a Send or Aside the session
// did not take as a turn of its live conversation - holding a reply from that session.

// myRepliesOwner is the filter every Reply read shares: the caller's rows ($1, their canonical
// login), narrowed to one issue ($2, empty for any) or one project document ($3, null for any),
// less an open ask the Inbox already lists in Waiting on you, whose row there shows its reply
// (api/inbox.go). That is an open ask on an open issue assigned to the caller alone: an open ask on
// a project document, on an unassigned issue or on another person's issue is nobody else's Inbox
// row of the caller's, so it is a Reply row here. A close withdraws its issue's open asks, so an
// open ask on a closed issue is one a close by an older release left during a roll; it stays too.
const myRepliesOwner = `
	from user_threads u
	left join asks a on u.kind = 'ask' and a.id = u.thread_id
	left join issues ai on u.kind = 'ask' and ai.key = u.issue_key
	where u.login = $1
	  and ($2::text = '' or u.issue_key = $2::text)
	  and ($3::uuid is null or u.artifact_id = $3::uuid)
	  and not coalesce(a.state = 'open' and ai.closed_at is null and lower(btrim(ai.assignee)) = $1, false)`

// myRepliesUnread and myRepliesEverything are the two states of that filter, spelled as two
// constants over the one base above rather than as one bound parameter. A parameter defeats both
// partial indexes: `case when $n then u.replied_at is not null else u.unread end` leaves the
// planner nothing to imply either index's predicate from, and on PostgreSQL 18
// `explain (generic_plan)` of that form reads user_threads_pkey with the state as a filter, while
// each constant below reads its own index. A prepared statement reaches a generic plan after five
// executions, so the parameter would cost every Inbox read a scan of every row of the caller's.
const (
	myRepliesUnread     = myRepliesOwner + ` and u.unread`
	myRepliesEverything = myRepliesOwner + ` and u.replied_at is not null`
)

// myRepliesCount counts the caller's Reply rows in one state: the answer's total and the Inbox's
// unread-thread count. It reads them into a materialized set first, so the planner reads them
// through the state's partial index rather than misjudging the stored flag's selectivity.
const myRepliesCount = `
	with matched as materialized (
		select u.thread_id `
const myRepliesCountRest = `
	)
	select count(*) from matched`

const (
	myRepliesCountQuery    = myRepliesCount + myRepliesUnread + myRepliesCountRest
	myRepliesAllCountQuery = myRepliesCount + myRepliesEverything + myRepliesCountRest
)

// myRepliesPage reads the caller's Reply rows in one state, newest reply first, and what a row
// shows: who wrote the newest write someone else made and its text (last_reply_id: a comment for
// an ask or a comment thread, a message for a message thread, or the ask itself when that write is
// its answer, read as the answerer and the answer's options and text), what the thread is about
// (the ask's question, or its root's text), where it lives (the row's own owner columns, its
// issue's priority and its project), how far the caller has read it, whether it is still unread,
// how many writes in it they have not read, and the document mark its root is anchored to. Its ref
// always names the thread's root, never the reply: a reply carries no anchor, so a link to it
// would skip the document redirect an anchored root takes.
//
// $4 is the number of rows to read, null for every row, and $5 how many to skip.
//
// The unread count is one lateral over the thread's own writes, so the read stays one statement:
// an ask's replies and its answer, a comment thread's root and replies (every reply is stored
// against its thread's head, so one level reaches them all), or a message thread's root and the
// walk of its replies, which message_replies makes once for every message row on the page rather
// than once per row. "Somebody else" is user_threads_login, the function the triggers key a row
// on, so the count and the row's own unread flag can never disagree about who wrote a write.
const myRepliesPage = `
	with recursive page as (
		select u.thread_id, u.kind, u.replied_at, u.last_reply_id, u.issue_key, u.artifact_id,
		       u.read_through, u.project_key, u.unread
		`
const myRepliesPageRest = `
		order by u.replied_at desc, u.thread_id desc
		limit $4 offset $5
	),
	message_replies as (
		select p.thread_id as root_id, m.id, m.created_at, m.author
		from page p join messages m on m.in_reply_to = p.thread_id
		where p.kind = 'message'
		union all
		select r.root_id, m.id, m.created_at, m.author
		from message_replies r join messages m on m.in_reply_to = r.id
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
	       coalesce(rc.created_at, rm.created_at, p.replied_at),
	       i.priority, p.project_key, p.read_through, p.unread,
	       coalesce(counted.unread_count, 0), coalesce(root.anchor, a.anchor)
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
	left join lateral (
		select count(*)::int as unread_count
		from (
			select c.created_at, c.author from comments c
			where p.kind = 'ask' and c.ask_id = p.thread_id
			union all
			select (ac.answer->>'at')::timestamptz,
			       jsonb_build_object('kind', 'user', 'id', ac.answer->>'user')
			from asks ac
			where p.kind = 'ask' and ac.id = p.thread_id and ac.answer->>'user' is not null
			union all
			select c.created_at, c.author from comments c
			where p.kind = 'comment' and (c.id = p.thread_id or c.reply_to = p.thread_id)
			union all
			select m.created_at, m.author from messages m
			where p.kind = 'message' and m.id = p.thread_id
			union all
			select r.created_at, r.author from message_replies r where r.root_id = p.thread_id
		) w(created_at, author)
		where w.created_at > p.read_through
		  and not (w.author->>'kind' = 'user' and user_threads_login(w.author->>'id') = $1)
	) counted on true
	left join issues i on i.key = p.issue_key
	left join artifacts ar on ar.id = p.artifact_id
	order by p.replied_at desc, p.thread_id desc`

const (
	myRepliesPageQuery    = myRepliesPage + myRepliesUnread + myRepliesPageRest
	myRepliesAllPageQuery = myRepliesPage + myRepliesEverything + myRepliesPageRest
)

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

// myAgentRows reads the caller's one-off agent threads in one state, newest reply first. Its rows
// are the output of unreadDirectRepliesCTE - the one definition of "a reply this login has not
// read in this session" that the Agents page's New replies N badge counts - narrowed here, and
// only here, to the roots no session took as a turn of its live conversation. That narrowing is
// the Inbox's alone: unread_replies.go is untouched, so the badge and the conversation window keep
// counting an accepted Send's reply, which still shows in the live view.
//
// Parameters are unreadDirectRepliesCTE's own (unreadDirectRepliesArgs), so the first free one is
// $4; this read needs none.
const myAgentRows = `
	one_off_replies as (
		select dt.root_id, dt.session_id, dt.id as reply_id, dt.created_at, dt.author
		from direct_thread dt
		where dt.author->>'kind' = 'session'
		  and not exists (
			select 1 from message_deliveries d
			where d.message_id = dt.root_id and d.accepted_as = 'user_turn'
		  )
	),
	one_off_unread as (
		select u.root_id, u.reply_id, u.created_at
		from unread_direct_replies u
		where not exists (
			select 1 from message_deliveries d
			where d.message_id = u.root_id and d.accepted_as = 'user_turn'
		)
	),
	counted as (
		select root_id, count(*)::int as unread_count,
		       array_agg(reply_id::text order by created_at, reply_id) as unread_reply_ids
		from one_off_unread group by root_id
	),
	roots as (
		select o.root_id, o.session_id, max(o.created_at) as replied_at
		from one_off_replies o
		`

// myAgentRowsRest is what both states select over those roots: the root's body as the subject, the
// newest session reply in the thread as the row's reply, and the unread replies the Inbox marks
// read by id.
const myAgentRowsRest = `
		group by o.root_id, o.session_id
	)
	select r.root_id::text, r.session_id, r.replied_at, root.body,
	       newest.reply_id::text, newest.author, newest.body, newest.created_at,
	       coalesce(c.unread_count, 0), coalesce(c.unread_reply_ids, '{}'::text[])
	from roots r
	join messages root on root.id = r.root_id
	join lateral (
		select o.reply_id, o.author, o.created_at, m.body
		from one_off_replies o join messages m on m.id = o.reply_id
		where o.root_id = r.root_id
		order by o.created_at desc, o.reply_id desc
		limit 1
	) newest on true
	left join counted c on c.root_id = r.root_id
	order by r.replied_at desc, r.root_id desc`

const (
	// myAgentRowsQuery reads the one-off roots holding a reply the caller has not read.
	myAgentRowsQuery = `with recursive` + unreadDirectRepliesCTE + `,` + myAgentRows +
		`where exists (select 1 from one_off_unread where one_off_unread.root_id = o.root_id)` + myAgentRowsRest
	// myAgentAllRowsQuery reads every one-off root holding a session reply, read or not.
	myAgentAllRowsQuery = `with recursive` + unreadDirectRepliesCTE + `,` + myAgentRows + myAgentRowsRest
)

// The kinds of row GET /api/v1/me/replies answers, as ?kind= names them, and the states it reads
// them in. kind defaults to reply alone, so a caller that names none reads what it read before the
// Inbox took the Agent rows; state defaults to unread.
const (
	myReplyKindReply  = "reply"
	myReplyKindAgent  = "agent"
	myRepliesStateAll = "all"
)

// parseMyRepliesKinds reads ?kind=, a comma-separated list of reply and agent. An empty list is
// the default, reply alone; any other value is refused.
func parseMyRepliesKinds(r *http.Request) (reply, agent bool, err error) {
	for _, value := range strings.Split(r.URL.Query().Get("kind"), ",") {
		switch strings.TrimSpace(value) {
		case "":
		case myReplyKindReply:
			reply = true
		case myReplyKindAgent:
			agent = true
		default:
			return false, false, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"kind takes %s and %s", myReplyKindReply, myReplyKindAgent)
		}
	}
	if !reply && !agent {
		reply = true
	}
	return reply, agent, nil
}

// parseMyRepliesState reads ?state=: unread, the default, or all.
func parseMyRepliesState(r *http.Request) (all bool, err error) {
	switch strings.TrimSpace(r.URL.Query().Get("state")) {
	case "", "unread":
		return false, nil
	case myRepliesStateAll:
		return true, nil
	default:
		return false, errorf(http.StatusBadRequest, "INVALID_QUERY", "state is unread or all")
	}
}

// parseMyRepliesPage reads ?limit= and ?offset=. An absent limit is every row, which is what the
// Inbox reads: it sorts, groups, filters and counts over all of them. A limit the caller names
// keeps the rules GET /api/v1/me/answers has (1 to 200, so limit=0 is refused).
func parseMyRepliesPage(r *http.Request) (limit *int, offset int, err error) {
	query := r.URL.Query()
	if value, present, valid := parseQueryInt(query, "limit", 1, myAnswersMaxLimit); present {
		if !valid {
			return nil, 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "limit must be one integer from 1 to %d", myAnswersMaxLimit)
		}
		limit = &value
	}
	if value, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			return nil, 0, errorf(http.StatusBadRequest, "INVALID_QUERY", "offset must be one non-negative integer")
		}
		offset = value
	}
	return limit, offset, nil
}

// listMyReplies answers GET /api/v1/me/replies: the caller's Inbox threads of the kinds ?kind=
// names, in the state ?state= names, and how many there are in all.
//
// Every read runs in one read-only repeatable-read transaction, and as_of is that snapshot's
// database time. A mark-all passes as_of back as its read mark, so that time has to be one at
// which every row in the answer was already committed; two reads in two snapshots would leave a
// row committed between them marked read without ever having been shown.
//
// The two kinds come from unrelated tables, so they are merged in Go rather than unioned in SQL:
// each read answers in the merge's own order (newest reply first, ties by thread id descending),
// and the page is cut from the merged list. The Reply read is asked for no more rows than the cut
// can reach, since a row past offset+limit of its own order cannot be inside the merged page.
func (s *server) listMyReplies(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.requireHuman(w, r)
	if !ok {
		return
	}
	wantReply, wantAgent, err := parseMyRepliesKinds(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	all, err := parseMyRepliesState(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	limit, offset, err := parseMyRepliesPage(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	issue, artifact, err := parseMyRepliesOwner(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// An Agent row has no issue and no document, so a read narrowed to one owner holds none.
	wantAgent = wantAgent && issue == "" && artifact == nil

	login := model.CanonicalLogin(caller.ID)
	response := model.MyRepliesResponse{Rows: []model.MyReplyRow{}, Offset: offset}
	if limit != nil {
		response.Limit = *limit
	}
	tx, err := s.deps.Store.Pool.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		s.writeHandlerError(w, fmt.Errorf("read my replies: %w", err))
		return
	}
	defer tx.Rollback(r.Context())
	if err := tx.QueryRow(r.Context(), "select now()").Scan(&response.AsOf); err != nil {
		s.writeHandlerError(w, fmt.Errorf("read my replies: %w", err))
		return
	}

	rows := []model.MyReplyRow{}
	if wantReply {
		countQuery, pageQuery := myRepliesCountQuery, myRepliesPageQuery
		if all {
			countQuery, pageQuery = myRepliesAllCountQuery, myRepliesAllPageQuery
		}
		var total int
		if err := tx.QueryRow(r.Context(), countQuery, login, issue, artifact).Scan(&total); err != nil {
			s.writeHandlerError(w, fmt.Errorf("count my replies: %w", err))
			return
		}
		response.Total += total
		var reach *int
		if limit != nil {
			through := offset + *limit
			reach = &through
		}
		read, err := s.readMyReplyRows(r.Context(), tx, pageQuery, login, issue, artifact, reach)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		rows = append(rows, read...)
	}
	if wantAgent {
		query := myAgentRowsQuery
		if all {
			query = myAgentAllRowsQuery
		}
		read, err := s.readMyAgentRows(r.Context(), tx, query, caller.ID)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		response.Total += len(read)
		rows = append(rows, read...)
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, fmt.Errorf("read my replies: %w", err))
		return
	}

	slices.SortFunc(rows, func(left, right model.MyReplyRow) int {
		if order := right.Reply.CreatedAt.Compare(left.Reply.CreatedAt); order != 0 {
			return order
		}
		return strings.Compare(right.ThreadID, left.ThreadID)
	})
	if offset < len(rows) {
		rows = rows[offset:]
	} else {
		rows = nil
	}
	if limit != nil && *limit < len(rows) {
		rows = rows[:*limit]
	}
	response.Rows = append(response.Rows, rows...)
	WriteJSON(w, http.StatusOK, response)
}

// readMyReplyRows reads one state's Reply rows, at most reach of them (nil for every row).
func (s *server) readMyReplyRows(
	ctx context.Context, tx pgx.Tx, query, login, issue string, artifact *string, reach *int,
) ([]model.MyReplyRow, error) {
	rows, err := tx.Query(ctx, query, login, issue, artifact, reach, 0)
	if err != nil {
		return nil, fmt.Errorf("list my replies: %w", err)
	}
	defer rows.Close()
	read := []model.MyReplyRow{}
	for rows.Next() {
		var row model.MyReplyRow
		var issueKey, issueTitle, project, slug, name, replyID *string
		var subject, body string
		var author, anchor []byte
		var isAnswer bool
		var readThrough time.Time
		var projectKey string
		if err := rows.Scan(
			&row.ThreadID, &row.Kind, &row.Ref,
			&issueKey, &issueTitle, &project, &slug, &name,
			&subject, &replyID, &isAnswer, &author, &body, &row.Reply.CreatedAt,
			&row.Priority, &projectKey, &readThrough, &row.Unread, &row.UnreadCount, &anchor,
		); err != nil {
			return nil, fmt.Errorf("scan my reply: %w", err)
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
		row.Project = &projectKey
		row.ReadThrough = &readThrough
		row.Subject = contracts.OneLineSummary(subject)
		if replyID != nil {
			row.Reply.ID = *replyID
		}
		if author != nil {
			if err := json.Unmarshal(author, &row.Reply.Author); err != nil {
				return nil, fmt.Errorf("decode my reply author: %w", err)
			}
		}
		if len(anchor) > 0 {
			var value model.Anchor
			if err := model.DecodeAnchor(anchor, &value); err != nil {
				return nil, fmt.Errorf("decode my reply anchor: %w", err)
			}
			row.Anchor = &value
		}
		row.Reply.FirstLine = contracts.OneLineSummary(body)
		read = append(read, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list my replies: %w", err)
	}
	return read, nil
}

// readMyAgentRows reads one state's Agent rows and names each one's session. A session's name is
// the newest title it stamped on anything it wrote (delivery.ResolveSessionTitles, the Delivery
// page's own read), which outlives the session; one it never stamped is named by its id.
func (s *server) readMyAgentRows(ctx context.Context, tx pgx.Tx, query, actorID string) ([]model.MyReplyRow, error) {
	rows, err := tx.Query(ctx, query, unreadDirectRepliesArgs(actorID, nil)...)
	if err != nil {
		return nil, fmt.Errorf("list my agent threads: %w", err)
	}
	defer rows.Close()
	read := []model.MyReplyRow{}
	sessions := []string{}
	for rows.Next() {
		row := model.MyReplyRow{Kind: myReplyKindAgent, Reply: model.MyReplyReply{Kind: "reply"}}
		var session, subject, body string
		var author []byte
		var repliedAt time.Time
		if err := rows.Scan(
			&row.ThreadID, &session, &repliedAt, &subject,
			&row.Reply.ID, &author, &body, &row.Reply.CreatedAt,
			&row.UnreadCount, &row.UnreadReplyIDs,
		); err != nil {
			return nil, fmt.Errorf("scan my agent thread: %w", err)
		}
		if err := json.Unmarshal(author, &row.Reply.Author); err != nil {
			return nil, fmt.Errorf("decode my agent reply author: %w", err)
		}
		row.Ref = "/agents/" + session + "/live"
		row.Subject = contracts.OneLineSummary(subject)
		row.Reply.FirstLine = contracts.OneLineSummary(body)
		row.Unread = row.UnreadCount > 0
		row.Session = &model.MyReplySession{ID: session}
		read = append(read, row)
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list my agent threads: %w", err)
	}
	titles, err := delivery.ResolveSessionTitles(ctx, tx, sessions)
	if err != nil {
		return nil, fmt.Errorf("name my agent threads' sessions: %w", err)
	}
	for index := range read {
		read[index].Session.Name = delivery.SessionTitle(titles, read[index].Session.ID)
	}
	return read, nil
}

// putThreadRead answers PUT /api/v1/me/threads/{id}/read: it moves the caller's read mark on one
// thread forward to read_through (stored as no later than now, as the Agents page's read mark is),
// so a row a tab read less of a moment ago never comes back. A thread the caller has no row for
// writes nothing.
//
// The transaction locks the thread's owner row `for share` before it touches the caller's row:
// every writer that moves a user_threads row (a comment, an answer, an issue message) holds that
// owner `for no key update` before its trigger fires, so a mark that took the row first could wait
// on a writer waiting on it. Owner first, then the project the event names, is the order every
// multi-owner transaction takes (LockOwners).
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
