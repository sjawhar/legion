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
// narrowing a request names, the priority names, the owner parse, the agent-reply write and the
// prune - this file calls rather than copies.
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
// undismissed rows (myRepliesInbox, with the list route's own parameters $1 login, $2 issue, $3
// document) whose newest reply is at or before the mark ($4), of the priorities ($5, null for every
// one, with $6 for a thread whose issue carries none) and the projects ($7, null for every one) the
// request names. inboxRestoreReplyThreads is the same read over the dismissed rows
// (myRepliesDismissed), whose mark is at or before the one the request carries.
//
// Each answers the thread's id and the owner this transaction locks before it writes, exactly as
// inboxReadReplyThreads does, and for its reason: the rows are matched first, so the planner reads
// them through the state's own partial index rather than misjudging the join, and the project each
// event names is read back from the update rather than here.
const inboxDismissReplyThreads = `
	with matched as materialized (
		select u.thread_id, u.replied_at
		` + myRepliesInbox + `
	)
	select m.thread_id::text, owner.issue_key, owner.artifact_id::text
	from matched m
	join lateral (
		select u.issue_key, u.artifact_id, u.project_key
		from user_threads u
		where u.login = $1 and u.thread_id = m.thread_id
		offset 0
	) owner on true
	left join issues i on i.key = owner.issue_key
	where m.replied_at <= least($4::text::timestamptz, now())
	  and ($5::int[] is null or i.priority = any($5::int[]) or ($6::boolean and i.priority is null))
	  and ($7::text[] is null or owner.project_key = any($7::text[]))
	order by m.thread_id`

const inboxRestoreReplyThreads = `
	with matched as materialized (
		select u.thread_id, u.dismissed_at
		` + myRepliesDismissed + `
	)
	select m.thread_id::text, owner.issue_key, owner.artifact_id::text
	from matched m
	join lateral (
		select u.issue_key, u.artifact_id, u.project_key
		from user_threads u
		where u.login = $1 and u.thread_id = m.thread_id
		offset 0
	) owner on true
	left join issues i on i.key = owner.issue_key
	where m.dismissed_at <= least($4::text::timestamptz, now())
	  and ($5::int[] is null or i.priority = any($5::int[]) or ($6::boolean and i.priority is null))
	  and ($7::text[] is null or owner.project_key = any($7::text[]))
	order by m.thread_id`

// inboxDismissNamedThreads reads the owners of the Reply threads a request named by id ($2), which
// is what this transaction locks for them. It narrows to no state: a thread the write leaves alone
// is locked with the rest, as the mark-all locks every thread it named, and a thread the caller has
// no row for answers nothing and changes nothing, as PUT /api/v1/me/threads/{id}/read does.
const inboxDismissNamedThreads = `
	select thread_id::text, issue_key, artifact_id::text
	from user_threads
	where login = $1 and thread_id = any($2::uuid[])
	order by thread_id`

// inboxDismissAgentRest and inboxRestoreAgentRest are what this route selects over the Inbox's
// one-off agent roots: each root this request changes, its session - which one event names - and,
// for a dismissal, the unread replies it marks read by id, since dismissing marks the thread read
// too. The by-mark forms take the roots at or before the mark ($4); the by-id forms take the roots
// the request named ($4 again, as the array). Each reads the root's own facts as the list route
// reads them (myAgentRootsWithFacts, api/replies.go).
var (
	inboxDismissAgentRest = `
	select r.root_id::text, agent.session_id, agent.unread_reply_ids` + myAgentRootsWithFacts + `
	where agent.replied_at <= least($4::text::timestamptz, now())
	order by r.root_id`
	inboxDismissAgentByIDRest = `
	select r.root_id::text, agent.session_id, agent.unread_reply_ids` + myAgentRootsWithFacts + `
	where r.root_id = any($4::uuid[])
	order by r.root_id`
	inboxRestoreAgentRest = `
	select r.root_id::text, agent.session_id, '{}'::text[]` + myAgentRootsWithFacts + `
	where r.dismissed_at <= least($4::text::timestamptz, now())
	order by r.root_id`
	inboxRestoreAgentByIDRest = `
	select r.root_id::text, agent.session_id, '{}'::text[]` + myAgentRootsWithFacts + `
	where r.root_id = any($4::uuid[])
	order by r.root_id`
)

// The four Agent reads this route makes, each the list route's own roots for the state it writes
// over (api/replies.go): the Inbox's undismissed roots for a dismissal, the dismissed ones for a
// restore. Reading them through the list's own constants is what keeps this route and the Dismissed
// view from disagreeing about whether a thread is dismissed.
var (
	inboxDismissAgentThreads     = myAgentInboxRoots + inboxDismissAgentRest
	inboxDismissAgentByID        = myAgentInboxRoots + inboxDismissAgentByIDRest
	inboxRestoreAgentThreads     = myAgentDismissedRoots + inboxRestoreAgentRest
	inboxRestoreAgentThreadsByID = myAgentDismissedRoots + inboxRestoreAgentByIDRest
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
// inboxRestoreThreads is the other direction: it clears the mark on every row that carries one.
const (
	inboxDismissMarkThreads = `
	update user_threads
	set dismissed_at = least($2::text::timestamptz, now()),
	    read_through = greatest(read_through, least($2::text::timestamptz, now()))
	where login = $1 and thread_id = any($3::uuid[]) and dismissed_at is null
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
// inboxRestoreAgentThreadRows is the other direction, and inboxDismissLockAgentThreads takes the
// rows it deletes in that same sorted order first, since a delete locks what its own plan reaches
// in the order it reaches it.
const (
	inboxDismissMarkAgentThreads = `
	insert into user_agent_thread_dismissed (login, message_id, dismissed_at)
	select $1, named.root, least($2::text::timestamptz, now())
	from unnest($3::uuid[]) with ordinality as named(root, position)
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

// inboxDismissRequest is the body: either the row ids to change or the list's own narrowing with
// the mark to change through, never both, and restore, which puts the threads back in the Inbox
// instead of taking them out. Every narrowing list is optional and an absent one means every value,
// as the mark-all reads them.
type inboxDismissRequest struct {
	Threads        []string `json:"threads"`
	DismissThrough *string  `json:"dismiss_through"`
	Restore        bool     `json:"restore"`
	Kind           []string `json:"kind"`
	Priority       []string `json:"priority"`
	Urgency        []string `json:"urgency"`
	Project        []string `json:"project"`
	Issue          string   `json:"issue"`
	Artifact       string   `json:"artifact"`
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
	if rows.named() && inboxDismissNamesFilters(input) {
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
		scope, err = parseInboxReadScope(inboxReadRequest{
			Kind: input.Kind, Priority: input.Priority, Urgency: input.Urgency,
			Project: input.Project, Issue: input.Issue, Artifact: input.Artifact,
		})
		if err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	dismissed, events, err := s.dismissInboxThreads(r.Context(), actor, input, rows, scope)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(events...)
	WriteJSON(w, http.StatusOK, inboxDismissResponse{Dismissed: dismissed})
}

// inboxDismissNamesFilters is whether a request carries any of the list's filters, which is what a
// request naming its rows may not carry.
func inboxDismissNamesFilters(input inboxDismissRequest) bool {
	return input.DismissThrough != nil || len(input.Kind) > 0 || len(input.Priority) > 0 ||
		len(input.Urgency) > 0 || len(input.Project) > 0 || input.Issue != "" || input.Artifact != ""
}

// dismissInboxThreads is the one transaction: it reads the rows it changes, takes their owners,
// writes the marks, takes the projects it wrote in and appends one event per project and per
// session it touched. It answers how many threads it changed and the events to publish once it has
// committed.
//
// It takes the Inbox's read-mark lock order, which is written down once, on markInboxReadThreads
// (api/inbox_read.go): the owner row of every thread it changes `for share`, then the caller's own
// user_threads rows `for no key update` in thread_id order, then the agent rows, then the project
// of every row it changed. Taking that same order is what keeps a dismiss, a mark-all and a
// one-thread mark from crossing: all three take the owner `for share`, all three move rows of
// user_threads under one login, and two of them that took the row and the project in opposite
// orders would deadlock on the project row every time they met.
//
// The two writes this route adds to that order are the caller's own user_agent_thread_dismissed
// rows, taken in message_id order, and nothing else: the Reply write is one update over rows the
// pass above has already locked, as the mark-all's is, and the agent-reply write and its prune are
// the mark-all's own statements (markInboxReadAgentReplies), taken here for the same reason, so a
// dismissed Agent thread stops counting as unread.
//
// Every statement runs under the mark-all's statement_timeout, because this transaction holds the
// same owner rows `for share`: every comment, answer and issue message on those owners waits behind
// a statement that hangs, so one that cannot finish is better killed.
func (s *server) dismissInboxThreads(
	ctx context.Context, actor model.Actor, input inboxDismissRequest, rows inboxDismissRows, scope inboxReadScope,
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
	read, err := s.readInboxDismissThreads(ctx, tx, actor, login, input, rows, scope)
	if err != nil {
		return 0, nil, err
	}
	if err := lockInboxReadOwners(ctx, tx, read.inboxReadThreads); err != nil {
		return 0, nil, err
	}

	events := []model.Event{}
	changed := 0
	if len(read.threads) > 0 {
		moved, err := dismissInboxReplyThreads(ctx, tx, login, input, read.threads)
		if err != nil {
			return 0, nil, err
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
		sessions, roots, err := s.dismissInboxAgentThreads(ctx, tx, login, input, read)
		if err != nil {
			return 0, nil, err
		}
		changed += roots
		for _, session := range sessions {
			events = append(events, userAgentStateEvent(actor, session))
		}
	}
	// A request that changed nothing announces nothing and has nothing to commit, as a mark-all
	// that moved nothing announces nothing: each event refetches the list in every tab the viewer
	// has open.
	if len(events) == 0 {
		return 0, nil, tx.Rollback(ctx)
	}
	if err := s.deps.Events.LockOwners(ctx, tx, events...); err != nil {
		return 0, nil, fmt.Errorf("lock the dismissed threads' projects: %w", err)
	}
	appended := make([]model.Event, 0, len(events))
	for _, event := range events {
		written, err := s.appendEvent(ctx, tx, event)
		if err != nil {
			return 0, nil, err
		}
		appended = append(appended, written)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return changed, appended, nil
}

// inboxDismissRead is what the transaction read before it took a lock or wrote a row: the Reply
// threads to change with the owners to lock for them, and the Agent roots with their sessions and,
// for a dismissal, the unread replies it marks read. It is an inboxReadThreads so the mark-all's
// owner lock (lockInboxReadOwners) and agent-reply write (markInboxReadAgentReplies) serve this
// route unchanged.
type inboxDismissRead struct {
	inboxReadThreads
	// agentRoots are the Agent threads this request changes, sorted, and rootSessions names each
	// root's session, which is one event each.
	agentRoots   []string
	rootSessions map[string]string
}

// readInboxDismissThreads reads both kinds of row this request changes, each list sorted so the
// locks and writes that follow are taken in one order. A request that named its rows is read by
// those ids and a request that named the filters by the state they match, which is the list route's
// own read of that state.
func (s *server) readInboxDismissThreads(
	ctx context.Context, tx pgx.Tx, actor model.Actor, login string,
	input inboxDismissRequest, rows inboxDismissRows, scope inboxReadScope,
) (inboxDismissRead, error) {
	read := inboxDismissRead{
		inboxReadThreads: inboxReadThreads{roots: map[string]string{}},
		rootSessions:     map[string]string{},
	}
	if rows.named() {
		if len(rows.threads) > 0 {
			if err := s.readInboxDismissOwners(ctx, tx, &read, inboxDismissNamedThreads, login, rows.threads); err != nil {
				return read, err
			}
		}
		if len(rows.roots) > 0 {
			query := inboxDismissAgentByID
			if input.Restore {
				query = inboxRestoreAgentThreadsByID
			}
			args := append(unreadDirectRepliesArgs(actor.ID, nil), rows.roots)
			if err := s.readInboxDismissRoots(ctx, tx, &read, query, args); err != nil {
				return read, err
			}
		}
		return read, nil
	}
	if scope.wantsReplyThreads() {
		query := inboxDismissReplyThreads
		if input.Restore {
			query = inboxRestoreReplyThreads
		}
		if err := s.readInboxDismissOwners(ctx, tx, &read, query,
			login, scope.issue, scope.artifact, input.DismissThrough,
			scope.priorities, scope.priorityNone, scope.projects); err != nil {
			return read, err
		}
	}
	if scope.wantsAgentThreads() {
		query := inboxDismissAgentThreads
		if input.Restore {
			query = inboxRestoreAgentThreads
		}
		args := append(unreadDirectRepliesArgs(actor.ID, nil), input.DismissThrough)
		if err := s.readInboxDismissRoots(ctx, tx, &read, query, args); err != nil {
			return read, err
		}
	}
	return read, nil
}

// readInboxDismissOwners reads the Reply threads one statement matches and the owners to lock for
// them, each sorted.
func (s *server) readInboxDismissOwners(
	ctx context.Context, tx pgx.Tx, read *inboxDismissRead, query string, args ...any,
) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the inbox's threads to dismiss: %w", err)
	}
	defer rows.Close()
	documents, issues := map[string]struct{}{}, map[string]struct{}{}
	for rows.Next() {
		var thread string
		var issueKey, artifactID *string
		if err := rows.Scan(&thread, &issueKey, &artifactID); err != nil {
			return fmt.Errorf("scan a thread to dismiss: %w", err)
		}
		read.threads = append(read.threads, thread)
		if issueKey != nil {
			issues[*issueKey] = struct{}{}
		}
		if artifactID != nil {
			documents[*artifactID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the inbox's threads to dismiss: %w", err)
	}
	read.documents = slices.Sorted(maps.Keys(documents))
	read.issues = slices.Sorted(maps.Keys(issues))
	return nil
}

// readInboxDismissRoots reads the Agent threads one statement matches: each root, its session and
// the unread replies a dismissal marks read, sorted as the writes take them.
func (s *server) readInboxDismissRoots(
	ctx context.Context, tx pgx.Tx, read *inboxDismissRead, query string, args []any,
) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the inbox's agent threads to dismiss: %w", err)
	}
	defer rows.Close()
	sessions := map[string]string{}
	for rows.Next() {
		var root, session string
		var replies []string
		if err := rows.Scan(&root, &session, &replies); err != nil {
			return fmt.Errorf("scan an agent thread to dismiss: %w", err)
		}
		read.agentRoots = append(read.agentRoots, root)
		read.rootSessions[root] = session
		for _, reply := range replies {
			read.replies = append(read.replies, reply)
			read.inboxReadThreads.roots[reply] = root
			sessions[reply] = session
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the inbox's agent threads to dismiss: %w", err)
	}
	slices.Sort(read.agentRoots)
	// One lock order for the reply rows a dismissal writes, the ids' own, as putUserAgentState and
	// the mark-all sort them.
	slices.Sort(read.replies)
	read.sessions = make([]string, 0, len(read.replies))
	for _, reply := range read.replies {
		read.sessions = append(read.sessions, sessions[reply])
	}
	return nil
}

// dismissInboxReplyThreads takes the caller's rows in thread_id order, writes or clears the mark
// over them and reads back the project of every row it changed. A thread the write leaves alone -
// one already dismissed, or one that was never dismissed under a restore - is locked with the rest,
// since what orders two requests must be the rows they named rather than the rows one of them
// happens to change, and it is not among those answered.
func dismissInboxReplyThreads(
	ctx context.Context, tx pgx.Tx, login string, input inboxDismissRequest, threads []string,
) (inboxReadMarked, error) {
	moved := inboxReadMarked{}
	if _, err := tx.Exec(ctx, inboxReadLockThreads, login, threads); err != nil {
		return moved, fmt.Errorf("lock the caller's threads: %w", err)
	}
	query, args := inboxDismissMarkThreads, []any{login, input.DismissThrough, threads}
	if input.Restore {
		query, args = inboxRestoreThreads, []any{login, threads}
	}
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return moved, fmt.Errorf("dismiss the inbox's threads: %w", err)
	}
	defer rows.Close()
	projects := map[string]struct{}{}
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return moved, fmt.Errorf("scan a dismissed thread: %w", err)
		}
		moved.marked++
		projects[project] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return moved, fmt.Errorf("dismiss the inbox's threads: %w", err)
	}
	moved.keys = slices.Sorted(maps.Keys(projects))
	return moved, nil
}

// dismissInboxAgentThreads writes or clears the dismissal row of every Agent thread it was given
// and, on a dismissal, marks that thread's unread replies read by id, as the mark-all writes them.
// It answers the sessions of the threads it changed, sorted, which is one event each, and how many
// threads those are, which the answer counts.
func (s *server) dismissInboxAgentThreads(
	ctx context.Context, tx pgx.Tx, login string, input inboxDismissRequest, read inboxDismissRead,
) ([]string, int, error) {
	if _, err := tx.Exec(ctx, inboxDismissLockAgentThreads, login, read.agentRoots); err != nil {
		return nil, 0, fmt.Errorf("lock the caller's agent threads: %w", err)
	}
	query, args := inboxDismissMarkAgentThreads, []any{login, input.DismissThrough, read.agentRoots}
	if input.Restore {
		query, args = inboxRestoreAgentThreadRows, []any{login, read.agentRoots}
	}
	rows, err := tx.Query(ctx, query, args...)
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
	if len(read.replies) > 0 && !input.Restore {
		// Dismissing marks the thread read, so its unread replies are written by id exactly as the
		// mark-all writes them, and the dead rows the sessions' own marks cover are pruned with
		// them. A restore moves no read mark: a thread comes back as it was read, and a new write
		// by somebody else is what brings it back unread.
		marked, _, err := s.markInboxReadAgentReplies(ctx, tx, login, read.inboxReadThreads)
		if err != nil {
			return nil, 0, err
		}
		for _, session := range marked {
			sessions[session] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(sessions)), len(changed), nil
}
