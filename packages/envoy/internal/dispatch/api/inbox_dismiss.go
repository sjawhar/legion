package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// POST /api/v1/me/inbox/dismiss takes a thread out of the person's Inbox, and puts it back. A
// thread stays in the Inbox, read or not, until they dismiss it, as an email stays until it is
// archived: reading it and pressing Mark all read leave it there, and this route alone takes it
// out. A new write by somebody else brings a dismissed thread back, unread; the person's own reply
// does not, so a reply they send from the Dismissed view leaves the thread there.
//
// It is a file of its own rather than a third act in api/inbox_read.go: that file's subject is the
// read mark and the lock order written on markInboxReadThreads, which this route takes without
// restating, and holding both routes in one file would put two bodies, two refusal tables and two
// pairs of reads around one comment that belongs to neither alone. What the two share - the
// narrowing a request names, the statements' own shapes, the reads, the owner lock, the mark
// write, the agent-reply write, the prune and the commit - this file calls rather than copies, and
// what it adds is the direction (inboxDismissPlan) and the statements that write a dismissal.
//
// The dismissal itself is one mark per thread, beside the read mark for a Reply thread
// (user_threads.dismissed_at, migration 20261105093500) and a row of its own for an Agent thread
// (user_agent_thread_dismissed, migration 20261105093505), and dismissing marks the thread read
// too, so a dismissed thread never counts as unread.

// inboxDismissReplyPrefix and inboxDismissAgentPrefix spell a row id the Inbox draws: a Reply row
// is `reply:<thread id>` and an Agent row `agent:<root message id>`. inboxDismissAskPrefix is the
// Inbox's third kind of row, an open ask the page lists in Waiting on you, which has no Dismiss at
// all: it is named here so naming one is refused with its own words rather than read as a row id of
// no shape. The Reply row of an open ask's thread is an ordinary `reply:` id and dismisses like any
// other.
const (
	inboxDismissReplyPrefix = "reply:"
	inboxDismissAgentPrefix = "agent:"
	inboxDismissAskPrefix   = "ask:"
)

// inboxDismissReplyThreads reads the Reply threads one filtered request dismisses: the caller's
// undismissed rows (myRepliesInbox) whose newest reply is at or before the mark the request
// carries. inboxRestoreReplyThreads is the same read over the dismissed rows (myRepliesDismissed),
// whose dismissal is at or before that mark.
//
// Both are the mark-all's own read of the state they write over (inboxReplyMatchQuery,
// api/inbox_read.go), which is what keeps this route and the list route from disagreeing about
// which rows a state holds, and leaves one plan to tune for the three.
var (
	inboxDismissReplyThreads = inboxReplyMatchQuery(myRepliesInbox, "replied_at")
	inboxRestoreReplyThreads = inboxReplyMatchQuery(myRepliesDismissed, "dismissed_at")
)

// inboxDismissNamedThreads reads the owners of the Reply threads a request named by id ($2), which
// is what this transaction locks for them. It narrows to no state: a thread the write leaves alone
// is locked with the rest, as the mark-all locks every thread it named, and a thread the caller has
// no row for answers nothing and changes nothing, as PUT /api/v1/me/threads/{id}/read does. That
// is also what makes it one statement for both directions, where the state-matched read above is
// two.
const inboxDismissNamedThreads = `
	select thread_id::text, issue_key, artifact_id::text
	from user_threads
	where login = $1 and thread_id = any($2::uuid[])
	order by thread_id`

// What this route's four Agent reads add to the mark-all's own (inboxAgentRootsQuery,
// api/inbox_read.go): a restore marks no thread read, so it selects no reply ids, and a request
// that named its rows narrows the state's roots to those ids ($4 as the array) rather than to a
// mark.
const (
	inboxDismissNoReplies        = `'{}'::text[]`
	inboxDismissNamedRoots       = `r.root_id = any($4::uuid[])`
	inboxRestoreDismissedThrough = `r.dismissed_at <= least($4::text::timestamptz, now())`
)

// The four Agent reads themselves, each over the list route's own roots for the state this request
// writes over (api/replies_agent.go): the Inbox's undismissed roots for a dismissal, the dismissed
// ones for a restore. Reading them through the list's own constants is what keeps this route and
// the Dismissed view from disagreeing about whether a thread is dismissed.
var (
	inboxDismissAgentThreads = inboxAgentRootsQuery(
		myAgentInboxRoots, inboxAgentUnreadReplies, inboxAgentRepliedThrough)
	inboxDismissAgentByID = inboxAgentRootsQuery(
		myAgentInboxRoots, inboxAgentUnreadReplies, inboxDismissNamedRoots)
	inboxRestoreAgentThreads = inboxAgentRootsQuery(
		myAgentDismissedRoots, inboxDismissNoReplies, inboxRestoreDismissedThrough)
	inboxRestoreAgentThreadsByID = inboxAgentRootsQuery(
		myAgentDismissedRoots, inboxDismissNoReplies, inboxDismissNamedRoots)
)

// inboxDismissMarkThreads writes the dismissal on every Reply thread it was given and answers the
// project of each row it changed. A row already carrying a mark is left alone and not counted: for
// a Reply thread "dismissed" is exactly `dismissed_at is not null`, since user_threads_record clears
// the mark when somebody else writes again (migration 20261105093501), so a second dismissal of a
// thread nobody has written in since changes nothing. The mark is cast by Postgres from the
// caller's own text rather than bound as a parsed time, and stored as no later than now, exactly as
// the read mark is; a request that named threads rather than filters binds no text at all, and
// least() over a null mark is now(), the moment it was dismissed. read_through only moves forward.
//
// A row whose replied_at has passed the mark is left alone too, which is what keeps a reply that
// commits while this request runs from being dismissed unread. now() is this transaction's own
// timestamp, so the mark a request without dismiss_through writes is the moment it began - before
// it read the threads, and before every lock it waited for - and a filtered request's mark is
// older still. This statement reads replied_at in a snapshot of its own, taken after the last of
// those waits, so a write by somebody else that committed in between is seen here and the thread
// stays in the Inbox, unread. A write that commits after it is the trigger's: it waits on the row
// this transaction holds (step 2 of the lock order) and then clears the mark, as
// user_threads_record clears any mark older than the write. A thread nobody else has ever written
// in carries no replied_at and so has no write to be older than: it is dismissed as before.
//
// inboxRestoreThreads is the other direction: it clears the mark on every row that carries one,
// and takes no mark of its own, which is the one parameter the two differ by (inboxMarkArgs).
const (
	inboxDismissMarkThreads = `
	update user_threads
	set dismissed_at = least($2::text::timestamptz, now()),
	    read_through = greatest(read_through, least($2::text::timestamptz, now()))
	where login = $1 and thread_id = any($3::uuid[]) and dismissed_at is null
	  and (replied_at is null or replied_at <= least($2::text::timestamptz, now()))
	returning project_key`
	inboxRestoreThreads = `
	update user_threads set dismissed_at = null
	where login = $1 and thread_id = any($2::uuid[]) and dismissed_at is not null
	returning project_key`
)

// inboxDismissMarkAgentThreads writes one dismissal row per Agent thread, in the roots' sorted
// order so two requests naming the same threads take the new rows in one order, as
// inboxReadMarkAgentReplies sorts the replies it writes. A thread that came back carries no row at
// all - the trigger of migration 20261105093507 deleted it when the session replied - so this
// writes it again; a row already at or after the mark is left alone and not counted, which is what
// a request repeated against an unchanged thread answers 0 for.
//
// A root whose thread holds a session reply newer than the mark is left alone, which is the Agent
// half of the comparison the update above makes on replied_at and closes the same window: the mark
// is this transaction's own now(), or text the request carried that is older again, so both are
// older than the read that chose this root. Without the test a reply that committed between that
// read and this insert would be buried under a mark it postdates, and nothing would dig it out -
// the trigger of 20261105093507 deletes a mark that was already there when the reply committed,
// and this one was not. The insert's own snapshot is taken after the last lock this transaction
// waited for, so that reply is visible to it. The comparison is `>`, the trigger's inverted, so
// the two agree about a reply written at the mark exactly: the person had seen it, and it leaves
// the thread dismissed.
//
// It closes that window and not the statement's own: a reply whose transaction commits after
// this snapshot is taken finds no mark to delete, since the row this statement is writing is
// not committed either, and leaves it behind. The Reply half has no such gap - the trigger's
// update waits on the row step 2 locked - and here there is no row to wait on until this
// insert commits. What that leaves is one statement wide, against a wait on every lock the
// request takes, and closing it would need a lock on the thread's root that the reply route
// does not take.
//
// The walk is the list route's own (agentThreadCTE, api/replies_agent.go), so this statement and
// the Inbox read one thread the same way; being built from it is what makes these var rather than
// const. A `with recursive` inside a subquery is a CTE of that subquery, as myAgentUnreadRoots
// already asks of the planner.
//
// inboxRestoreAgentThreadRows is the other direction, and inboxDismissLockAgentThreads takes the
// rows it deletes in that same sorted order first, since a delete locks what its own plan reaches
// in the order it reaches it.
var (
	inboxDismissMarkAgentThreads = `
	insert into user_agent_thread_dismissed (login, message_id, dismissed_at)
	select $1, named.root, least($2::text::timestamptz, now())
	from unnest($3::uuid[]) with ordinality as named(root, position)
	where not exists (` + agentThreadCTE("named.root") + `
		select 1 from thread t
		where t.author->>'kind' = 'session'
		  and t.created_at > least($2::text::timestamptz, now())
	)
	order by named.position
	on conflict (login, message_id) do update
	  set dismissed_at = excluded.dismissed_at
	  where user_agent_thread_dismissed.dismissed_at < excluded.dismissed_at
	returning message_id::text`
	inboxDismissLockAgentThreads = `
	select 1 from user_agent_thread_dismissed
	where login = $1 and message_id = any($2::uuid[])
	order by message_id
	for no key update`
	inboxRestoreAgentThreadRows = `
	delete from user_agent_thread_dismissed
	where login = $1 and message_id = any($2::uuid[])
	returning message_id::text`
)

// inboxDismissRequest is the body: either the row ids to change or the list's own narrowing
// (inboxThreadFilters, api/inbox_read.go, which the mark-all takes too) with the mark to change
// through, never both, and restore, which puts the threads back in the Inbox instead of taking
// them out.
type inboxDismissRequest struct {
	Threads        []string `json:"threads"`
	DismissThrough *string  `json:"dismiss_through"`
	Restore        bool     `json:"restore"`
	inboxThreadFilters
}

// inboxDismissResponse answers how many threads the request changed: the Reply threads whose mark
// moved plus the Agent threads whose dismissal row it wrote or deleted. A request that changes
// nothing answers 0.
type inboxDismissResponse struct {
	Dismissed int `json:"dismissed"`
}

// inboxDismissRows is the rows one request names, read off its body: the Reply threads and the
// Agent roots, each sorted, so the locks and the writes that follow are taken in one order.
type inboxDismissRows struct {
	threads []string
	roots   []string
}

// named is whether the request named its rows rather than the filters to match them by.
func (rows inboxDismissRows) named() bool {
	return len(rows.threads) > 0 || len(rows.roots) > 0
}

// inboxDismissPlan is the direction one request takes, chosen once by the handler
// (inboxDismissPlanOf) rather than at each statement: a dismissal writes the marks and marks the
// threads read, a restore clears them and moves no read mark, and everything between those two
// ends is one transaction. The direction is one decision, so it is read once: spelled at each of
// the six statements it picks, it was six spellings to keep in step with each other.
type inboxDismissPlan struct {
	// The reads: the Reply and Agent rows a filtered request changes, and the Agent roots a
	// request that named its rows changes. The Reply rows it named are read by
	// inboxDismissNamedThreads, which narrows to no state and so is one statement for both.
	replyThreads string
	agentThreads string
	agentByID    string
	// The writes themselves, over the rows those reads answered.
	markThreads      string
	markAgentThreads string
	// through is the mark the request carries: what every filtered read matches its rows against
	// ($4) and what a dismissal's writes store. restore is the direction, which decides whether
	// those writes store it at all and whether the Agent half marks the thread read.
	through *string
	restore bool
}

// inboxDismissPlanOf reads the direction off the body.
func inboxDismissPlanOf(input inboxDismissRequest) inboxDismissPlan {
	plan := inboxDismissPlan{
		replyThreads:     inboxDismissReplyThreads,
		agentThreads:     inboxDismissAgentThreads,
		agentByID:        inboxDismissAgentByID,
		markThreads:      inboxDismissMarkThreads,
		markAgentThreads: inboxDismissMarkAgentThreads,
		through:          input.DismissThrough,
	}
	if input.Restore {
		plan.replyThreads = inboxRestoreReplyThreads
		plan.agentThreads = inboxRestoreAgentThreads
		plan.agentByID = inboxRestoreAgentThreadsByID
		plan.markThreads = inboxRestoreThreads
		plan.markAgentThreads = inboxRestoreAgentThreadRows
		plan.restore = true
	}
	return plan
}

// writeMark is the mark this plan's writes store, as the one parameter they bind between the
// caller and the rows (inboxMarkArgs): a restore stores none, so it binds none.
func (plan inboxDismissPlan) writeMark() []any {
	if plan.restore {
		return nil
	}
	return []any{plan.through}
}

// parseInboxDismissRows reads the threads list: `reply:<thread id>` and `agent:<message id>`, each
// id a uuid, sorted so two requests over one set of rows write them in one order. An Ask row's id is
// refused with its own words, since the Inbox draws no Dismiss on one, and any other spelling is
// refused as the shape it is not.
func parseInboxDismissRows(values []string) (inboxDismissRows, error) {
	rows := inboxDismissRows{}
	for _, value := range values {
		row := strings.TrimSpace(value)
		if strings.HasPrefix(row, inboxDismissAskPrefix) {
			return rows, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"an ask is answered or snoozed, not dismissed; threads takes %s<id> and %s<id>",
				inboxDismissReplyPrefix, inboxDismissAgentPrefix)
		}
		kind, id, found := strings.Cut(row, ":")
		if !found || (kind+":" != inboxDismissReplyPrefix && kind+":" != inboxDismissAgentPrefix) {
			return rows, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"threads takes %s<id> and %s<id>", inboxDismissReplyPrefix, inboxDismissAgentPrefix)
		}
		parsed, err := uuid.Parse(id)
		if err != nil {
			return rows, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"%s%s names no thread: the id must be a uuid", kind, ":"+id)
		}
		if kind+":" == inboxDismissReplyPrefix {
			rows.threads = append(rows.threads, parsed.String())
			continue
		}
		rows.roots = append(rows.roots, parsed.String())
	}
	slices.Sort(rows.threads)
	rows.threads = slices.Compact(rows.threads)
	slices.Sort(rows.roots)
	rows.roots = slices.Compact(rows.roots)
	return rows, nil
}

// dismissInbox answers POST /api/v1/me/inbox/dismiss.
func (s *server) dismissInbox(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input inboxDismissRequest
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	rows, err := parseInboxDismissRows(input.Threads)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	// The two forms are exclusive: a request carrying both would leave it to the server to decide
	// whether the filters narrow the named rows or name rows of their own.
	if rows.named() && (input.DismissThrough != nil || input.narrows()) {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest,
			"name threads or the list's filters with dismiss_through, not both")
		return
	}
	// The narrowing a filtered request matches its rows by. A request that named its rows is read
	// by those ids alone (readInboxDismissThreads), so this stands unread for it.
	scope := inboxReadScope{reply: true, agent: true, priorityNone: true, projectNone: true, urgencyNone: true}
	if !rows.named() {
		// Without named rows the request is the list's own filters, and it needs the mark they are
		// matched against, read as every mark on these routes is read (RFC3339, no further ahead of
		// the server clock than a minute) and carried on as the caller's own text.
		if input.DismissThrough == nil {
			writeError(w, "INVALID_STATE", http.StatusBadRequest, "dismiss_through is required")
			return
		}
		if _, err := parseAgentStateCutoff("dismiss_through", input.DismissThrough); err != nil {
			s.writeHandlerError(w, err)
			return
		}
		scope, err = parseInboxReadScope(input.inboxThreadFilters)
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	dismissed, events, err := s.dismissInboxThreads(r.Context(), actor, inboxDismissPlanOf(input), rows, scope)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(events...)
	WriteJSON(w, http.StatusOK, inboxDismissResponse{Dismissed: dismissed})
}

// dismissInboxThreads is the one transaction: it reads the rows it changes, takes their owners,
// writes the marks, takes the projects it wrote in and appends one event per project and per
// session it touched (commitInboxMarks, api/inbox_read.go, which the mark-all ends with too). It
// answers how many threads it changed and the events to publish once it has committed.
//
// It takes the Inbox's read-mark lock order, every step of it, which is written down once, on
// markInboxReadThreads (api/inbox_read.go) - step 3 of that list, the caller's own
// user_agent_thread_dismissed rows, is this route's alone, since the mark-all writes no
// dismissal. Taking that same order is what keeps a dismiss, a mark-all and a one-thread mark
// from crossing: all three take the owner `for share`, all three move rows of user_threads under
// one login, and two of them that took the row and the project in opposite orders would deadlock
// on the project row every time they met.
//
// Every write this route makes falls inside that order: the Reply write is one update over rows
// step 2 has already locked, as the mark-all's is and through the mark-all's own statement
// (writeInboxThreadMarks), and the agent-reply write and its prune are the mark-all's own too
// (markInboxReadAgentReplies), taken here for the same reason, so a dismissed Agent thread stops
// counting as unread.
//
// Every statement runs under the mark-all's statement_timeout, because this transaction holds the
// same owner rows `for share`: every comment, answer and issue message on those owners waits behind
// a statement that hangs, so one that cannot finish is better killed.
func (s *server) dismissInboxThreads(
	ctx context.Context, actor model.Actor, plan inboxDismissPlan, rows inboxDismissRows, scope inboxReadScope,
) (int, []model.Event, error) {
	login := model.CanonicalLogin(actor.ID)
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local statement_timeout = '"+inboxReadStatementTimeout+"'"); err != nil {
		return 0, nil, fmt.Errorf("bound the dismissal's statements: %w", err)
	}
	read, err := readInboxDismissThreads(ctx, tx, actor, login, plan, rows, scope)
	if err != nil {
		return 0, nil, err
	}
	if err := lockInboxReadOwners(ctx, tx, read); err != nil {
		return 0, nil, err
	}

	events := []model.Event{}
	changed := 0
	if len(read.threads) > 0 {
		moved, err := writeInboxThreadMarks(ctx, tx, login, read.threads, plan.markThreads, plan.writeMark())
		if err != nil {
			return 0, nil, fmt.Errorf("dismiss the inbox's threads: %w", err)
		}
		changed += moved.marked
		for _, project := range moved.keys {
			// One event per project, naming no thread, as the mark-all appends them: the viewer's
			// other tabs and devices refetch the list, which is what a thread's own mark asks of
			// them too.
			events = append(events, projectOwner(project).event(
				"user_thread_state.updated", actor, model.UserThreadStateEventPayload{Login: login},
			))
		}
	}
	if len(read.agentRoots) > 0 {
		sessions, roots, err := s.dismissInboxAgentThreads(ctx, tx, login, plan, read)
		if err != nil {
			return 0, nil, err
		}
		changed += roots
		for _, session := range sessions {
			events = append(events, userAgentStateEvent(actor, session))
		}
	}
	return s.commitInboxMarks(ctx, tx, changed, events)
}

// readInboxDismissThreads reads both kinds of row this request changes, through the mark-all's own
// reads (readInboxReplyRows and readInboxAgentRows, api/inbox_read.go), each list sorted so the
// locks and writes that follow are taken in one order. A request that named its rows is read by
// those ids and a request that named the filters by the state they match, which is the list route's
// own read of that state.
func readInboxDismissThreads(
	ctx context.Context, tx pgx.Tx, actor model.Actor, login string,
	plan inboxDismissPlan, rows inboxDismissRows, scope inboxReadScope,
) (inboxReadThreads, error) {
	read := inboxReadThreads{roots: map[string]string{}, rootSessions: map[string]string{}}
	if rows.named() {
		if len(rows.threads) > 0 {
			if err := readInboxReplyRows(ctx, tx, &read, inboxDismissNamedThreads, login, rows.threads); err != nil {
				return read, err
			}
		}
		if len(rows.roots) > 0 {
			args := append(unreadDirectRepliesArgs(actor.ID, nil), rows.roots)
			if err := readInboxAgentRows(ctx, tx, &read, plan.agentByID, args); err != nil {
				return read, err
			}
		}
		return read, nil
	}
	if scope.wantsReplyThreads() {
		if err := readInboxReplyRows(ctx, tx, &read, plan.replyThreads,
			login, scope.issue, scope.artifact, plan.through,
			scope.priorities, scope.priorityNone, scope.projects); err != nil {
			return read, err
		}
	}
	if scope.wantsAgentThreads() {
		args := append(unreadDirectRepliesArgs(actor.ID, nil), plan.through)
		if err := readInboxAgentRows(ctx, tx, &read, plan.agentThreads, args); err != nil {
			return read, err
		}
	}
	return read, nil
}

// dismissInboxAgentThreads writes or clears the dismissal row of every Agent thread it was given
// and, on a dismissal, marks that thread's unread replies read by id, as the mark-all writes them.
// It answers the sessions of the threads it changed, sorted, which is one event each, and how many
// threads those are, which the answer counts.
func (s *server) dismissInboxAgentThreads(
	ctx context.Context, tx pgx.Tx, login string, plan inboxDismissPlan, read inboxReadThreads,
) ([]string, int, error) {
	if _, err := tx.Exec(ctx, inboxDismissLockAgentThreads, login, read.agentRoots); err != nil {
		return nil, 0, fmt.Errorf("lock the caller's agent threads: %w", err)
	}
	args := inboxMarkArgs(login, plan.writeMark(), read.agentRoots)
	rows, err := tx.Query(ctx, plan.markAgentThreads, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("dismiss the inbox's agent threads: %w", err)
	}
	defer rows.Close()
	changed := map[string]struct{}{}
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, 0, fmt.Errorf("scan a dismissed agent thread: %w", err)
		}
		changed[root] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("dismiss the inbox's agent threads: %w", err)
	}
	sessions := map[string]struct{}{}
	for root := range changed {
		sessions[read.rootSessions[root]] = struct{}{}
	}
	if len(read.replies) > 0 && !plan.restore {
		// Dismissing marks the thread read, so its unread replies are written by id exactly as the
		// mark-all writes them, and the dead rows the sessions' own marks cover are pruned with
		// them. A restore moves no read mark: a thread comes back as it was read, and a new write
		// by somebody else is what brings it back unread.
		marked, _, err := s.markInboxReadAgentReplies(ctx, tx, login, read)
		if err != nil {
			return nil, 0, err
		}
		for _, session := range marked {
			sessions[session] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(sessions)), len(changed), nil
}
