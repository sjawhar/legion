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
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// GET /api/v1/me/replies is the Inbox's thread read, of two kinds of row. A Reply row is a thread
// the person wrote in - an ask's thread, a comment thread by its root, an issue message thread by
// its root - that somebody else has written in; user_threads (migration 0095) keeps one row per
// person per thread, maintained by the triggers 0095-0097 install, so the Reply reads touch only
// the person's own rows, each state through its own partial index: user_threads_unread (0095)
// under state=unread, user_threads_inbox (20261105093502) under state=inbox and
// user_threads_dismissed (20261105093503) under state=dismissed. An Agent row is one of the
// person's one-off messages to an agent session - a BTW, a broadcast's copy, or a Send or Aside
// the session did not take as a turn of its live conversation - holding a reply from that
// session; its dismissal is a row of user_agent_thread_dismissed (20261105093505), since an Agent
// row has no user_threads row to carry one.
//
// A thread stays in the Inbox, read or not, until the person dismisses it (api/inbox_dismiss.go):
// state=inbox answers what is in it, state=dismissed what has been taken out of it, and
// state=unread, the default, answers the unread rows the Inbox's count reads.
//
// The Agent half - the statements each state reads those rows through, the row read and the
// session names the page shows - is api/replies_agent.go's. This file holds the route, the Reply
// half and what the two share.

// myRepliesOwner is the filter every Reply read shares: the caller's rows ($1, their canonical
// login), narrowed to one issue ($2, empty for any) or one project document ($3, null for any),
// less an open ask the Inbox already lists in Waiting on you, whose row there shows its reply
// (api/inbox.go). That is an open ask on an open issue assigned to the caller alone: an open ask on
// a project document, on an unassigned issue or on another person's issue is nobody else's Inbox
// row of the caller's, so it is a Reply row here. A close withdraws its issue's open asks, so an
// open ask on a closed issue is one a close by an older release left during a roll; it stays too.
//
// The ask is excluded by a test keyed on the thread's own id rather than by two left joins over
// the row's kind and issue, so every column this filter reads of user_threads is one the state's
// own index holds and the read above it can answer from that index alone (myRepliesPage). An ask's
// thread id is the ask's own id, which is what makes the id enough to key the test on.
const myRepliesOwner = `
	from user_threads u
	where u.login = $1
	  and ($2::text = '' or u.issue_key = $2::text)
	  and ($3::uuid is null or u.artifact_id = $3::uuid)
	  and not exists (
		select 1 from asks a join issues ai on ai.key = a.issue_key
		where a.id = u.thread_id and a.state = 'open' and ai.closed_at is null
		  and lower(btrim(ai.assignee)) = $1
	  )`

// myRepliesUnread, myRepliesInbox and myRepliesDismissed are the three states of that filter,
// spelled as three constants over the one base above rather than as one bound parameter. A
// parameter defeats every partial index: `case when $n then u.dismissed_at is null else u.unread
// end` leaves the planner nothing to imply any index's predicate from, and on PostgreSQL 18
// `explain (generic_plan)` of that form reads user_threads_pkey with the state as a filter, while
// each constant below reads its own index. A prepared statement reaches a generic plan after five
// executions, so the parameter would cost every Inbox read a scan of every row of the caller's.
//
// "Dismissed" is exactly `dismissed_at is not null`: user_threads_record clears the mark when
// somebody else writes again (migration 20261105093501), so no read compares the two marks.
const (
	myRepliesUnread    = myRepliesOwner + ` and u.unread`
	myRepliesInbox     = myRepliesOwner + ` and u.replied_at is not null and u.dismissed_at is null`
	myRepliesDismissed = myRepliesOwner + ` and u.dismissed_at is not null`
)

// myRepliesDismissedCursor resumes a Dismissed page where the last one ended ($5 the dismissal
// time as the caller's own text, $6 the thread id), written as one row comparison so it is the
// index's own condition rather than a filter over it. It is a constant of its own, beside the
// first page's query, for the reason the states are: `($5 is null or ...)` would leave the
// planner a filter and read every dismissed row of the caller's to answer page two.
const myRepliesDismissedCursor = `
	  and (u.dismissed_at, u.thread_id) < ($5::text::timestamptz, $6::uuid)`

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
	myRepliesCountQuery      = myRepliesCount + myRepliesUnread + myRepliesCountRest
	myRepliesInboxCountQuery = myRepliesCount + myRepliesInbox + myRepliesCountRest
)

// myRepliesPage reads the caller's Reply rows in one state, newest reply first, and what a row
// shows: who wrote the newest write someone else made and its text (last_reply_id: a comment for
// an ask or a comment thread, a message for a message thread, or the ask itself when that write is
// its answer, read as the answerer and the answer's options and text), what the thread is about
// (the ask's question, or its root's text), where it lives (the row's own owner columns, its
// issue's priority and its project), how far the caller has read it, whether it is still unread,
// how many writes in it they have not read, the document mark its root is anchored to, and the
// version a dismissal of the row is held to (user_threads.version, migration 20261105093500).
// Its ref always names the thread's root, never the reply: a reply carries no anchor, so a link
// to it would skip the document redirect an anchored root takes.
//
// $4 is the number of rows to read, null for every row; a Dismissed page resumes from $5 and $6
// (myRepliesDismissedCursor).
//
// The unread count is one lateral over the thread's own writes, so the read stays one statement:
// an ask's replies and its answer, a comment thread's root and replies (every reply is stored
// against its thread's head, so one level reaches them all), or a message thread's root and the
// walk of its replies, which message_replies makes once for every message row on the page rather
// than once per row. "Somebody else" is user_threads_login, the function the triggers key a row
// on, so the count and the row's own unread flag can never disagree about who wrote a write.
//
// It is read in two steps. The first picks the page's thread ids out of the state's own index and
// nothing else: every column that step reads of user_threads - the login it narrows by, the mark it
// orders by and the thread id - is one the state's partial index holds, and its only other test is
// keyed on the thread id too (myRepliesOwner), so the read is an index-only scan of that index in
// the order the state answers in, with no sort and no row of the table touched. The second fetches
// the rows the cut kept, by their primary key. One step would be neither: with the Inbox's own read
// taking every undismissed row, a plan that fetches each row as it walks the index costs more than
// one that scans the table, so the planner scans it - 101,540 rows read to answer 1,540 of them at
// INFRA-450's a14 seed - and sorts what it keeps.
//
// Neither step carries an order over the joins: the route sorts the two kinds of row together
// (listMyReplies) whatever order each half read them in, so an order here would be one sort for
// nothing.
const myRepliesPage = `
	with recursive picked as (
		select u.thread_id `

// myRepliesPageRows is the second step: the rows the cut kept, each read by the thread id the cut
// answered. The `offset 0` is load-bearing: without it the planner flattens the lateral into a
// join, and the cheapest join over a table one login owns is a scan of it - 101,540 rows read to
// answer 1,540 at a14's seed, which is the scan the first step was restructured to avoid. Fenced,
// the fetch is one index read per row the page answers and costs the same time (228 ms against
// 222 ms on that seed), for a plan that reads nothing it does not answer.
const myRepliesPageRows = `
	),
	page as (
		select p.thread_id, kept.kind, kept.replied_at, kept.dismissed_at, kept.last_reply_id,
		       kept.issue_key, kept.artifact_id, kept.read_through, kept.project_key, kept.unread,
		       kept.version
		from picked p
		join lateral (
			select u.kind, u.replied_at, u.dismissed_at, u.last_reply_id, u.issue_key,
			       u.artifact_id, u.read_through, u.project_key, u.unread, u.version
			from user_threads u
			where u.login = $1 and u.thread_id = p.thread_id
			offset 0
		) kept on true
	),`

// myRepliesPageCut and myRepliesDismissedCut are the two orders a state is read in, which are the
// two the states' indexes answer in: newest reply first for the Inbox's own states, newest
// dismissal first for the Dismissed view. Each cuts the read to $4 rows before any row of the
// table is fetched, so a page costs one primary-key read and one set of joins per row it answers.
const myRepliesPageCut = `
		order by u.replied_at desc, u.thread_id desc
		limit $4`
const myRepliesDismissedCut = `
		order by u.dismissed_at desc, u.thread_id desc
		limit $4`

// myRepliesPageRest is what every state selects over the rows the cut kept.
const myRepliesPageRest = `
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
	       coalesce(counted.unread_count, 0), coalesce(root.anchor, a.anchor), p.dismissed_at,
	       p.version
	from page p
	-- The thread's own root and the reply the row shows, each read by its id. They are laterals
	-- with an offset-0 fence rather than joins because a join leaves the planner free to hash the
	-- whole of comments and messages against the page: at a14's seed that is two scans of 203,080
	-- comments and two of 20,120 messages, 100 ms of the Inbox read's 154, to answer 1,540 rows
	-- whose ids it already holds.
	left join lateral (
		select a.id, a.question, a.anchor, a.answer
		from asks a where p.kind = 'ask' and a.id = p.thread_id
		offset 0
	) a on true
	left join lateral (
		select root.body, root.anchor
		from comments root where p.kind = 'comment' and root.id = p.thread_id
		offset 0
	) root on true
	left join lateral (
		select mroot.body
		from messages mroot where p.kind = 'message' and mroot.id = p.thread_id
		offset 0
	) mroot on true
	left join lateral (
		select rc.author, rc.body, rc.created_at
		from comments rc where p.kind <> 'message' and rc.id = p.last_reply_id
		offset 0
	) rc on true
	left join lateral (
		select rm.author, rm.body, rm.created_at
		from messages rm where p.kind = 'message' and rm.id = p.last_reply_id
		offset 0
	) rm on true
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
	left join artifacts ar on ar.id = p.artifact_id`

const (
	myRepliesPageQuery      = myRepliesPage + myRepliesUnread + myRepliesPageCut + myRepliesPageRows + myRepliesPageRest
	myRepliesInboxPageQuery = myRepliesPage + myRepliesInbox + myRepliesPageCut + myRepliesPageRows + myRepliesPageRest
	// The Dismissed view's first page and the page that follows a cursor, two constants for the
	// reason the states are three: a cursor behind a parameter is a filter, not an index
	// condition.
	myRepliesDismissedPageQuery = myRepliesPage + myRepliesDismissed +
		myRepliesDismissedCut + myRepliesPageRows + myRepliesPageRest
	myRepliesDismissedFromQuery = myRepliesPage + myRepliesDismissed + myRepliesDismissedCursor +
		myRepliesDismissedCut + myRepliesPageRows + myRepliesPageRest
)

// parseThreadOwner reads the owner that narrows an Inbox read to one thread owner: an issue key
// or a project document's id, the read a mounted thread makes to learn whether it is unread.
// Both together are refused, as is a document that is not a uuid. The list route reads the two
// off its query (parseMyRepliesOwner) and the mark-all route off its body (parseInboxReadScope),
// and both refuse the same input with the same words.
func parseThreadOwner(rawIssue, rawArtifact string) (issue string, artifact *string, err error) {
	issue = strings.TrimSpace(rawIssue)
	rawArtifact = strings.TrimSpace(rawArtifact)
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

// parseMyRepliesOwner reads the optional ?issue= or ?artifact= of GET /api/v1/me/replies.
func parseMyRepliesOwner(r *http.Request) (issue string, artifact *string, err error) {
	query := r.URL.Query()
	return parseThreadOwner(query.Get("issue"), query.Get("artifact"))
}

// The kinds of row GET /api/v1/me/replies answers, as ?kind= names them, and the states it reads
// them in. kind defaults to reply alone, so a caller that names none reads what it read before the
// Inbox took the Agent rows; state defaults to unread, the state the Inbox's count reads.
const (
	myReplyKindReply = "reply"
	myReplyKindAgent = "agent"
	// myRepliesStateUnread answers the rows somebody else has written in since the caller read
	// them, myRepliesStateInbox every row still in their Inbox, read or unread, with the ones
	// they have dismissed left out, and myRepliesStateDismissed those (api/inbox_dismiss.go).
	myRepliesStateUnread    = "unread"
	myRepliesStateInbox     = "inbox"
	myRepliesStateDismissed = "dismissed"
)

// myRepliesDismissedPage is how many rows one state=dismissed read answers, and the page the
// Dismissed view's Show more reads again from the cursor.
const myRepliesDismissedPage = 200

// parseThreadKinds reads the kinds of row an Inbox read names: reply and agent, a value named
// twice counting once, and any other value refused, the empty one included. Both false is a
// request that named none - an empty list - which each route defaults for itself: reply alone
// for the list, both for the mark-all.
func parseThreadKinds(values []string) (reply, agent bool, err error) {
	for _, value := range values {
		switch strings.TrimSpace(value) {
		case myReplyKindReply:
			reply = true
		case myReplyKindAgent:
			agent = true
		default:
			return false, false, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"kind takes %s and %s", myReplyKindReply, myReplyKindAgent)
		}
	}
	return reply, agent, nil
}

// parseMyRepliesKinds reads ?kind=, a comma-separated list of reply and agent. A request that
// named none reads reply alone, what it read before the Inbox took the Agent rows. An absent
// ?kind= splits into one empty string, which is how this route names none, so the empties are
// dropped here rather than skipped by the parser: the mark-all reads its kinds off a JSON list,
// where "" is a value the caller wrote and the route refuses.
func parseMyRepliesKinds(r *http.Request) (reply, agent bool, err error) {
	named := []string{}
	for _, value := range strings.Split(r.URL.Query().Get("kind"), ",") {
		if strings.TrimSpace(value) != "" {
			named = append(named, value)
		}
	}
	reply, agent, err = parseThreadKinds(named)
	if err != nil {
		return false, false, err
	}
	if !reply && !agent {
		reply = true
	}
	return reply, agent, nil
}

// parseMyRepliesState reads ?state=: unread, the default, inbox or dismissed.
func parseMyRepliesState(r *http.Request) (state string, err error) {
	switch value := strings.TrimSpace(r.URL.Query().Get("state")); value {
	case "":
		return myRepliesStateUnread, nil
	case myRepliesStateUnread, myRepliesStateInbox, myRepliesStateDismissed:
		return value, nil
	default:
		return "", errorf(http.StatusBadRequest, "INVALID_QUERY", "state is %s, %s or %s",
			myRepliesStateUnread, myRepliesStateInbox, myRepliesStateDismissed)
	}
}

// myRepliesCursor is where a Dismissed page resumes: the dismissal mark and thread id of the last
// row the page before it answered, which is the pair both kinds of row are ordered by, so one
// cursor cuts both. The mark is carried on as the caller's own text, which the statements cast, as
// every mark on these routes is.
type myRepliesCursor struct {
	at     string
	thread string
}

// myRepliesCursorOf is the cursor a page answers for the row it ended on.
func myRepliesCursorOf(row model.MyReplyRow) string {
	return row.DismissedAt.UTC().Format(time.RFC3339Nano) + "," + row.ThreadID
}

// parseMyRepliesCursor reads one ?cursor=, which is a cursor this route answered: the dismissal
// mark and the thread id it names, as myRepliesCursorOf writes them.
func parseMyRepliesCursor(raw string) (*myRepliesCursor, error) {
	at, thread, found := strings.Cut(raw, ",")
	refusal := errorf(http.StatusBadRequest, "INVALID_QUERY", "cursor is one this route answered in next_cursor")
	if !found {
		return nil, refusal
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		return nil, refusal
	}
	if _, err := uuid.Parse(thread); err != nil {
		return nil, refusal
	}
	return &myRepliesCursor{at: at, thread: thread}, nil
}

// parseMyRepliesPage reads the paging of one state. unread and inbox page with ?limit= and
// ?offset=: an absent limit is every row, which is what the Inbox reads, since it sorts, groups,
// filters and counts over all of them, and a limit the caller names keeps the rules
// GET /api/v1/me/answers has (1 to 200, so limit=0 is refused). dismissed pages with ?cursor=
// alone, myRepliesDismissedPage rows at a time, because its order is the dismissal mark and an
// offset over a list that moves as rows are dismissed and restored would serve one row twice and
// another never.
//
// Each state refuses the other's parameters rather than ignoring them, as GET /api/v1/issues
// refuses a cursor (parseIssuePage): answering a cursor request with the first page, or a limit
// request with 200 rows, would hand the caller rows it cannot tell from the ones it asked for.
func parseMyRepliesPage(r *http.Request, state string) (limit *int, offset int, cursor *myRepliesCursor, err error) {
	query := r.URL.Query()
	if state == myRepliesStateDismissed {
		for _, name := range []string{"limit", "offset"} {
			if query.Has(name) {
				return nil, 0, nil, errorf(http.StatusBadRequest, "INVALID_QUERY",
					"%s is not a query parameter of state=%s, which pages with cursor, %d rows at a time",
					name, myRepliesStateDismissed, myRepliesDismissedPage)
			}
		}
		page := myRepliesDismissedPage
		if raw := strings.TrimSpace(query.Get("cursor")); raw != "" {
			cursor, err = parseMyRepliesCursor(raw)
			if err != nil {
				return nil, 0, nil, err
			}
		}
		return &page, 0, cursor, nil
	}
	if query.Has("cursor") {
		return nil, 0, nil, errorf(http.StatusBadRequest, "INVALID_QUERY",
			"cursor is a query parameter of state=%s alone; this state pages with limit and offset",
			myRepliesStateDismissed)
	}
	if value, present, valid := parseQueryInt(query, "limit", 1, myAnswersMaxLimit); present {
		if !valid {
			return nil, 0, nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "limit must be one integer from 1 to %d", myAnswersMaxLimit)
		}
		limit = &value
	}
	if value, present, valid := parseQueryInt(query, "offset", 0, math.MaxInt); present {
		if !valid {
			return nil, 0, nil, errorf(http.StatusBadRequest, "INVALID_QUERY", "offset must be one non-negative integer")
		}
		offset = value
	}
	return limit, offset, nil, nil
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
// each read answers in the merge's own order (the state's own - newest reply first, or newest
// dismissal first under state=dismissed - ties by thread id descending), and the page is cut from
// the merged list. Each read is asked for no more rows than the cut can reach, since a row past
// offset+limit of its own order cannot be inside the merged page, and the total is then counted by
// a statement of its own. An unpaged read needs neither: in the same snapshot over the same filter
// it reads every matching row, so what it read is the total. The Agent rows' sessions are named
// after the cut (nameAgentSessions), so a row the page throws away costs no title.
//
// The two kinds' blocks below stay two blocks. They share a shape - count when the read is
// paged, then read the rows - but every line of it differs: a pair of queries each, a login
// with an owner narrowing against a session read's own arguments, a row read of its own, and an
// error of its own to report. One block over a per-kind callback would hide four differences
// behind one parameter list to save six lines. The unpaged total is the one line the two would
// have shared, and it is taken once, after both.
func (s *server) listMyReplies(w http.ResponseWriter, r *http.Request) {
	caller, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	wantReply, wantAgent, err := parseMyRepliesKinds(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	state, err := parseMyRepliesState(r)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	limit, offset, from, err := parseMyRepliesPage(r, state)
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
	// How many rows a paged read asks each kind for: a row past offset+limit of its own order
	// cannot be inside the merged page, so neither half reads one. An unpaged read asks for every
	// row (null).
	var reach *int
	if limit != nil {
		through := offset + *limit
		reach = &through
	}
	// Every state but the Dismissed view counts its rows when the read is paged, since the page it
	// answers is a cut of a list the caller is told the length of. The Dismissed view is counted
	// nowhere: it answers 200 rows at a time from a cursor and says in next_cursor whether another
	// page follows, which is what the view reads, and a count of it is a count of every thread the
	// person has ever dismissed - 110,000 rows at INFRA-450's a14 seed, 8 s of the 9.4 s that read
	// took. Its total is the page it answers (contracts' MyRepliesResponse).
	counted := limit != nil && state != myRepliesStateDismissed
	if wantReply {
		countQuery, pageQuery := myRepliesQueries(state, from)
		countArgs := []any{login, issue, artifact}
		if counted {
			var total int
			if err := tx.QueryRow(r.Context(), countQuery, countArgs...).Scan(&total); err != nil {
				s.writeHandlerError(w, fmt.Errorf("count my replies: %w", err))
				return
			}
			response.Total += total
		}
		read, err := s.readMyReplyRows(r.Context(), tx, pageQuery, myRepliesPageArgs(countArgs, reach, from))
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		rows = append(rows, read...)
	}
	if wantAgent {
		// The Agent half answers its own total beside its rows when the read is counted, rather
		// than in a second statement: it reads every root the state holds either way.
		args := unreadDirectRepliesArgs(caller.ID, nil)
		read, total, err := s.readMyAgentRows(r.Context(), tx,
			myAgentPageQuery(state, from, counted), myRepliesPageArgs(args, reach, from), counted)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
		response.Total += total
		rows = append(rows, read...)
	}
	// An unpaged read of either kind read every matching row, so the rows in hand are the total,
	// taken before the offset cut below throws any away. The Dismissed view's total is the page it
	// answers, so it is taken after that cut, below.
	if limit == nil {
		response.Total = len(rows)
	}

	slices.SortFunc(rows, func(left, right model.MyReplyRow) int {
		if order := myRepliesOrderOf(state, right).Compare(myRepliesOrderOf(state, left)); order != 0 {
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
	// The Dismissed view's own total: the page it answers, counted once the cut has taken it.
	if state == myRepliesStateDismissed {
		response.Total = len(rows)
	}
	// A Dismissed page that filled answers the cursor the next one resumes from. A page that did
	// not fill is the last one, and following a cursor off the end of the list answers no row.
	if state == myRepliesStateDismissed && limit != nil && len(rows) == *limit {
		response.NextCursor = myRepliesCursorOf(rows[len(rows)-1])
	}
	if err := nameAgentSessions(r.Context(), tx, rows); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		s.writeHandlerError(w, fmt.Errorf("read my replies: %w", err))
		return
	}
	response.Rows = append(response.Rows, rows...)
	WriteJSON(w, http.StatusOK, response)
}

// myRepliesQueries is the statements one state reads its Reply rows through: the page itself,
// which is the cursor's own query when a Dismissed page resumes from one, and the count the
// answer's total needs when the read is paged. The Dismissed view has no count - its total is the
// page it answers (listMyReplies) - so it answers an empty count for that state, which that read
// never asks for. The Agent half's own statements are myAgentPageQuery's (api/replies_agent.go).
func myRepliesQueries(state string, from *myRepliesCursor) (count, page string) {
	switch state {
	case myRepliesStateInbox:
		return myRepliesInboxCountQuery, myRepliesInboxPageQuery
	case myRepliesStateDismissed:
		if from != nil {
			return "", myRepliesDismissedFromQuery
		}
		return "", myRepliesDismissedPageQuery
	default:
		return myRepliesCountQuery, myRepliesPageQuery
	}
}

// myRepliesPageArgs is one page read's parameters: the count read's own, then the rows to read
// (null for every row) and the cursor the page resumes from, which only a Dismissed page binds.
func myRepliesPageArgs(countArgs []any, reach *int, from *myRepliesCursor) []any {
	args := append(slices.Clone(countArgs), reach)
	if from != nil {
		args = append(args, from.at, from.thread)
	}
	return args
}

// myRepliesOrderOf is the time a row is ordered by in one state: the newest write somebody else
// made in the thread, or, in the Dismissed view, the moment it was dismissed. A row of either kind
// in that state carries a mark, since each half reads only its dismissed rows.
func myRepliesOrderOf(state string, row model.MyReplyRow) time.Time {
	if state == myRepliesStateDismissed && row.DismissedAt != nil {
		return *row.DismissedAt
	}
	return row.Reply.CreatedAt
}

// readMyReplyRows reads one state's Reply rows under the parameters myRepliesPageArgs built.
func (s *server) readMyReplyRows(
	ctx context.Context, tx pgx.Tx, query string, args []any,
) ([]model.MyReplyRow, error) {
	rows, err := tx.Query(ctx, query, args...)
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
		// The version is a Reply row's alone, so this scan is the one place that sets it: an
		// Agent row carries none and sends none (model.MyReplyRow.Version).
		var version int64
		if err := rows.Scan(
			&row.ThreadID, &row.Kind, &row.Ref,
			&issueKey, &issueTitle, &project, &slug, &name,
			&subject, &replyID, &isAnswer, &author, &body, &row.Reply.CreatedAt,
			&row.Priority, &projectKey, &readThrough, &row.Unread, &row.UnreadCount, &anchor,
			&row.DismissedAt, &version,
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
		row.Version = &version
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

// putThreadRead answers PUT /api/v1/me/threads/{id}/read: it moves the caller's read mark on one
// thread forward to read_through (stored as no later than now, as the Agents page's read mark is),
// so a row a tab read less of a moment ago never comes back. A thread the caller has no row for
// writes nothing.
//
// This transaction takes the Inbox's read-mark lock order over its one thread: the thread's
// owner row `for share`, then the caller's user_threads row, then the project its event names.
// Which locks that order rules out, and why the Inbox's Mark all read has to take the same one,
// is written on markInboxReadThreads (api/inbox_read.go).
func (s *server) putThreadRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
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
