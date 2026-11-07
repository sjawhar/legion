package api

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// POST /api/v1/me/inbox/read is the Inbox's Mark all read: one request marks every thread the
// list route would show the caller as unread, loaded on the page or not, narrowed as the list is
// narrowed and no further than one read mark. A thread whose newest reply landed after
// read_through stays unread, so a write the person has not seen is never swallowed by the button.
//
// The set it marks is exactly what GET /api/v1/me/replies?state=unread answers under the same
// narrowing, so it is read through that route's own filters - myRepliesUnread for the Reply rows
// (api/replies.go) and the Agent read beside it (api/replies_agent.go) - rather than predicates of
// its own: two spellings of "unread" would let the button mark a row the list never showed, or
// leave one it did.

// inboxReplyMatchQuery builds the Reply read both write routes over these rows make: the caller's
// rows in one state (source, one of the list route's own myRepliesUnread, myRepliesInbox and
// myRepliesDismissed, with that route's parameters $1 login, $2 issue, $3 document) whose mark -
// the column that state is ordered by, named by mark - is at or before $4, of the priorities ($5,
// null for every one, with $6 for a thread whose issue carries none) and the projects ($7, null
// for every one) the request names. It answers each thread's id, the owner this transaction locks
// before it writes - its issue or its document - and the row's own version (user_threads.version,
// migration 20261105093500), which is what a filtered dismissal holds the row to, the server
// standing in for the page there (inboxDismissMarkThreads, api/inbox_dismiss.go); the mark-all
// answers it and ignores it, since one read serves both routes. The project whose event announces
// the write is not read here: the update answers the project of every row it moved, and those are
// the projects this transaction locks, after the rows (inboxReadLockDocuments).
//
// One builder rather than a statement per state and route: the mark-all reads the caller's unread
// rows and the dismiss route the Inbox's own or the dismissed ones (api/inbox_dismiss.go), and the
// three differ in the state they match and the mark they narrow by and in nothing else, so three
// spellings of it would be three places to carry a fix to the one plan below.
//
// The rows are matched first, as myRepliesCount matches them, so the planner reads them through
// the state's own partial index rather than misjudging the stored flag; the narrowing these routes
// add is applied over that set, where the issue join costs one lookup per matched row.
//
// The match reads the index and nothing else - the thread id it answers and the mark it narrows by
// are both columns the state's partial index holds - and each matched row's owner is then read by
// that id, fenced from the planner as the list route's page read is (myRepliesPageRows,
// api/replies.go): a one-step match reads the owner columns too, and the cheapest way to read a
// few columns of a thousand rows out of a table one login owns is a scan of the whole table, which
// at INFRA-450's a14 seed is 101,540 rows read to mark 1,300.
func inboxReplyMatchQuery(source, mark string) string {
	return `
	with matched as materialized (
		select u.thread_id, u.` + mark + `
		` + source + `
	)
	select m.thread_id::text, owner.issue_key, owner.artifact_id::text, owner.version
	from matched m
	join lateral (
		select u.issue_key, u.artifact_id, u.project_key, u.version
		from user_threads u
		where u.login = $1 and u.thread_id = m.thread_id
		offset 0
	) owner on true
	left join issues i on i.key = owner.issue_key
	where m.` + mark + ` <= least($4::text::timestamptz, now())
	  and ($5::int[] is null or i.priority = any($5::int[]) or ($6::boolean and i.priority is null))
	  and ($7::text[] is null or owner.project_key = any($7::text[]))
	order by m.thread_id`
}

// inboxReadReplyThreads reads the Reply threads one request marks: the caller's unread rows
// (myRepliesUnread, read through user_threads_unread) whose newest reply is at or before the read
// mark.
var inboxReadReplyThreads = inboxReplyMatchQuery(myRepliesUnread, "replied_at")

// inboxAgentRootsQuery builds the Agent read both write routes make over one state's roots: each
// root the request changes, its session - which one event names - the replies a write that
// marks the thread read writes by id, which is inboxAgentUnreadReplies for such a write and an
// empty array for one that leaves the read mark alone, and the newest session reply in the
// root's thread (inboxAgentNewestReply), which is what a dismissal holds the thread to. Every
// read answers that column, the request that named its rows included: one read shape for the
// three routes, and a named dismissal writes what the page drew over it afterwards
// (readInboxDismissThreads, api/inbox_dismiss.go). match is what narrows those roots: the mark
// the request carries ($4), or the ids it named ($4 again, as the array). Each root's own facts
// are read as the list route reads them (myAgentRootsWithFacts, api/replies_agent.go), so a
// change to either moves the list and these routes together.
func inboxAgentRootsQuery(roots, replies, match string) string {
	return roots + `
	select r.root_id::text, agent.session_id, ` + replies + `, ` + inboxAgentNewestReply +
		myAgentRootsWithFacts + `
	where ` + match + `
	order by r.root_id`
}

// inboxAgentUnreadReplies is what a write that marks the thread read selects,
// inboxAgentRepliedThrough the narrowing the Inbox's own states take - the roots whose newest
// session reply is at or before the mark ($4) - and inboxAgentNewestReply the column every one
// of these reads answers last: the newest reply the session wrote in that thread, which is what
// the list route shows as the row's reply (agent.reply_id, myAgentRowFacts' `newest`,
// api/replies_agent.go). Only the dismissal's check reads it
// (inboxDismissCheckAgentRoots, api/inbox_dismiss.go); the mark-all answers it and ignores it,
// since one read serves both routes.
const (
	inboxAgentUnreadReplies  = `agent.unread_reply_ids`
	inboxAgentRepliedThrough = `agent.replied_at <= least($4::text::timestamptz, now())`
	inboxAgentNewestReply    = `agent.reply_id::text`
)

// inboxReadAgentThreads is the list route's own unread roots with that read over them: the roots
// holding a reply this login has not read, and each root's own facts, are myAgentUnreadRoots and
// myAgentRootsWithFacts, the constants myAgentRowsQuery is built from (api/replies_agent.go), so a
// change to either moves the list and this button together.
var inboxReadAgentThreads = inboxAgentRootsQuery(
	myAgentUnreadRoots, inboxAgentUnreadReplies, inboxAgentRepliedThrough)

// The lock statements of this transaction, which it takes in the order markInboxReadThreads
// writes down: the owner rows of the threads it marks `for share`, then the caller's own
// user_threads rows `for no key update`. Each list is ordered before it is locked, so the locks
// are taken in the sorted order rather than whatever order the scan returns, and the update
// below re-locks the rows rather than taking them in the order its own plan reaches them.
const (
	inboxReadLockDocuments = `select 1 from artifacts where id = any($1::uuid[]) order by id for share`
	inboxReadLockIssues    = `select 1 from issues where key = any($1::text[]) order by key for share`
	inboxReadLockThreads   = `
	select 1 from user_threads
	where login = $1 and thread_id = any($2::uuid[])
	order by thread_id
	for no key update`
)

// inboxReadMarkThreads moves the caller's mark on every Reply thread it was given, and answers
// the project of each row it moved. Every row it reaches is one inboxReadLockThreads has just
// taken, so the order its own plan scans them in decides nothing. read_through is cast by
// Postgres from the caller's own text rather than bound as a parsed time, and stored as no
// later than now, exactly as putThreadRead stores one mark: a mark never moves back, and a time
// carrying nanoseconds is not truncated a microsecond short of the write it means to cover.
const inboxReadMarkThreads = `
	update user_threads set read_through = least($2::text::timestamptz, now())
	where login = $1 and thread_id = any($3::uuid[])
	  and read_through < least($2::text::timestamptz, now())
	returning project_key`

// inboxReadMarkAgentReplies writes the agent replies this request reads, by id, as
// putUserAgentState writes the ones a view showed: the ids are sorted before the insert so two
// writes lock new rows in one order, a reply the session's own read mark has already passed gets
// no row (the prune below would delete it again), and a row already there is left alone. No
// session's user_agent_read.read_through moves, so a reply in the live conversation that this
// request did not mark stays unread.
const inboxReadMarkAgentReplies = `
	insert into user_agent_reply_read (login, session_id, reply_id)
	select $1, wanted.session_id, wanted.reply_id
	from unnest($2::text[], $3::uuid[]) with ordinality as wanted(session_id, reply_id, position)
	where not exists (
		select 1 from messages
		join user_agent_read on user_agent_read.login = $1 and user_agent_read.session_id = wanted.session_id
		where messages.id = wanted.reply_id and messages.created_at <= user_agent_read.read_through
	)
	order by wanted.position
	on conflict do nothing
	returning session_id, reply_id::text`

// inboxReadStatementTimeout bounds every statement of the mark-all's transaction. One request
// marking the acceptance seed - 600 unread Reply threads and 20 Agent threads for one login -
// takes 30 to 43 ms end to end on the test database, of which the ordered lock pass is 7 to 10
// (the same seed takes 24 to 33 ms without it), so ten seconds leaves better than two orders of
// magnitude of headroom for a busier server and cuts no honest request off. The ceiling
// matters because this transaction holds its owner rows `for share`: every comment, answer and
// issue message on those owners waits behind a statement that hangs, so one that cannot finish
// is better killed, which rolls the transaction back and frees them.
const inboxReadStatementTimeout = "10s"

// inboxReadPriorityLevels are the priorities a request names, as the Inbox's filter writes them,
// against the issues.priority each stands for. inboxReadNone is the value that stands for the
// absence of one: a thread whose issue carries no priority, a thread on a project document, which
// has no issue at all, and every Agent row, which has neither issue nor project.
var inboxReadPriorityLevels = map[string]int32{"P0": 0, "P1": 1, "P2": 2, "P3": 3}

const inboxReadNone = "none"

// inboxThreadFilters is the narrowing the Inbox's filter writes into the list route's query, as
// the two write routes over these rows read it off their body: the mark-all (inboxReadRequest) and
// the dismiss route (inboxDismissRequest, api/inbox_dismiss.go), which read it with one parser
// (parseInboxReadScope). Every list is optional and an absent one means every value. It is one
// embedded struct rather than six fields spelled in each body, so the two cannot drift into
// refusing different requests.
type inboxThreadFilters struct {
	Kind     []string `json:"kind"`
	Priority []string `json:"priority"`
	Urgency  []string `json:"urgency"`
	Project  []string `json:"project"`
	Issue    string   `json:"issue"`
	Artifact string   `json:"artifact"`
}

// narrows is whether a request carries any of them, which is what a request that names its own
// rows may not do (dismissInbox).
func (filters inboxThreadFilters) narrows() bool {
	return len(filters.Kind) > 0 || len(filters.Priority) > 0 || len(filters.Urgency) > 0 ||
		len(filters.Project) > 0 || filters.Issue != "" || filters.Artifact != ""
}

// inboxReadRequest is the body: the read mark, and that narrowing.
type inboxReadRequest struct {
	ReadThrough *string `json:"read_through"`
	inboxThreadFilters
}

// inboxReadResponse answers how many threads the request marked: the Reply threads whose mark
// moved plus the Agent threads that gained a read reply. A request that moves nothing answers 0.
type inboxReadResponse struct {
	Marked int `json:"marked"`
}

// inboxReadScope is one request's narrowing, read off its body. A nil priorities or projects list
// is every value; priorityNone and projectNone say whether a row carrying neither is in the set,
// which is what decides whether an Agent row - no issue, no project, no urgency - can be marked
// at all.
type inboxReadScope struct {
	reply, agent bool
	priorities   []int32
	priorityNone bool
	projects     []string
	projectNone  bool
	// urgencyNone is false only when the request names urgencies and none of them is "none".
	// A Reply row and an Agent row carry no urgency - only an ask the Inbox lists in Waiting on
	// you does - so such a request matches no thread and the route marks nothing.
	urgencyNone bool
	issue       string
	artifact    *string
}

// parseInboxReadScope reads the narrowing. kind defaults to both, unlike the list route's default
// of reply alone: the button sits over one list of both kinds, and a person who has filtered
// nothing means all of it.
func parseInboxReadScope(input inboxThreadFilters) (inboxReadScope, error) {
	scope := inboxReadScope{priorityNone: true, projectNone: true, urgencyNone: true}
	reply, agent, err := parseThreadKinds(input.Kind)
	if err != nil {
		return scope, err
	}
	// A request that named no kind means both of them, which is this route's default alone: the
	// list route's is reply.
	if !reply && !agent {
		reply, agent = true, true
	}
	scope.reply, scope.agent = reply, agent
	if len(input.Priority) > 0 {
		scope.priorities, scope.priorityNone = []int32{}, false
		for _, value := range input.Priority {
			name := strings.TrimSpace(value)
			if name == inboxReadNone {
				scope.priorityNone = true
				continue
			}
			level, known := inboxReadPriorityLevels[name]
			if !known {
				return scope, errorf(http.StatusBadRequest, "INVALID_QUERY",
					"priority takes P0, P1, P2, P3 and %s", inboxReadNone)
			}
			scope.priorities = append(scope.priorities, level)
		}
	}
	if len(input.Urgency) > 0 {
		scope.urgencyNone = false
		for _, value := range input.Urgency {
			name := strings.TrimSpace(value)
			if name == inboxReadNone {
				scope.urgencyNone = true
				continue
			}
			if !validUrgency(name) {
				return scope, errorf(http.StatusBadRequest, "INVALID_QUERY",
					"urgency takes low, med, high, blocking and %s", inboxReadNone)
			}
		}
	}
	if len(input.Project) > 0 {
		scope.projects, scope.projectNone = []string{}, false
		for _, value := range input.Project {
			key := strings.TrimSpace(value)
			if key == inboxReadNone {
				scope.projectNone = true
				continue
			}
			scope.projects = append(scope.projects, key)
		}
	}
	// The owner narrowing is the list route's (parseThreadOwner), read from the body rather than
	// the query.
	issue, artifact, err := parseThreadOwner(input.Issue, input.Artifact)
	if err != nil {
		return scope, err
	}
	scope.issue, scope.artifact = issue, artifact
	return scope, nil
}

// wantsReplyThreads and wantsAgentThreads say which of the two reads this scope can match a row
// in. An Agent row carries no issue, no document, no project, no priority and no urgency, so any
// narrowing to one of those leaves it out, exactly as the list route leaves it out of an
// owner-narrowed page.
func (scope inboxReadScope) wantsReplyThreads() bool {
	return scope.reply && scope.urgencyNone
}

func (scope inboxReadScope) wantsAgentThreads() bool {
	return scope.agent && scope.urgencyNone && scope.priorityNone && scope.projectNone &&
		scope.issue == "" && scope.artifact == nil
}

// inboxReadThreads is what the transaction read before it took a lock or wrote a row: the Reply
// threads to write over with the owners to lock for them, the Agent threads' unread replies, their
// sessions and their roots, and the Agent roots themselves. Both write routes over these rows read
// into it (api/inbox_dismiss.go), which is what lets them share this read, the owner lock and the
// agent-reply write.
type inboxReadThreads struct {
	threads   []string
	documents []string
	issues    []string
	// sessions and replies are one agent reply each, in the replies' sorted order, and roots
	// names each reply's thread, which is what the answer counts.
	sessions []string
	replies  []string
	roots    map[string]string
	// agentRoots are the Agent threads the request changes, sorted, and rootSessions names each
	// root's session, which is one event each. They are the dismiss route's to use: the mark-all
	// writes no row per root, it marks the replies.
	agentRoots   []string
	rootSessions map[string]string
	// replyVersions and rootVersions are what the dismissal holds each row it changes to, keyed
	// by thread id and by root id: a Reply row's user_threads.version and an Agent root's newest
	// session reply. Every read above answers the row's own; a request that named its rows drew
	// them itself, so readInboxDismissThreads (api/inbox_dismiss.go) writes what the page sent
	// over them. They are the dismissal's alone to read: the mark-all's reads fill both and
	// nothing reads them back.
	replyVersions map[string]int64
	rootVersions  map[string]string
	// drawnUnread are the unread replies the page drew in all of the Agent threads at once -
	// reply ids are unique, so one flat set serves every root, which is how the check binds it
	// (inboxDismissCheckAgentRoots, api/inbox_dismiss.go). readInboxDismissThreads fills it, from
	// the request where it named its rows and from replies above under the list's filters, where
	// the server is the page; the mark-all holds no thread to anything and leaves it empty.
	drawnUnread []string
}

// newInboxReadThreads is the empty read both routes fill, so neither has to spell which of its
// maps must exist before the first row lands in it.
func newInboxReadThreads() inboxReadThreads {
	return inboxReadThreads{
		roots: map[string]string{}, rootSessions: map[string]string{},
		replyVersions: map[string]int64{}, rootVersions: map[string]string{},
	}
}

// markInboxRead answers POST /api/v1/me/inbox/read.
func (s *server) markInboxRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.actingPerson(w, r)
	if !ok {
		return
	}
	var input inboxReadRequest
	if err := decodeJSON(r, &input); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	if input.ReadThrough == nil {
		writeError(w, "INVALID_STATE", http.StatusBadRequest, "read_through is required")
		return
	}
	// The cutoff is checked as every read mark here is checked - RFC3339, and no further ahead of
	// the server clock than a minute - and then carried on as the caller's own text, which the
	// statements cast.
	if _, err := parseAgentStateCutoff("read_through", input.ReadThrough); err != nil {
		s.writeHandlerError(w, err)
		return
	}
	scope, err := parseInboxReadScope(input.inboxThreadFilters)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	marked, events, err := s.markInboxReadThreads(r.Context(), actor, *input.ReadThrough, scope)
	if err != nil {
		s.writeHandlerError(w, err)
		return
	}
	s.publish(events...)
	WriteJSON(w, http.StatusOK, inboxReadResponse{Marked: marked})
}

// markInboxReadThreads is the one transaction: it reads the threads, takes their owners, writes
// the three marks, takes the projects it wrote in and appends one event per project and per
// session it touched. It answers how many threads it marked and the events to publish once it
// has committed.
//
// This comment is where the Inbox's read-mark lock order is written down. putThreadRead
// (api/replies.go), the dismiss route (api/inbox_dismiss.go), the lock statements above, the
// race tests and packages/envoy/AGENTS.md point here rather than saying it again.
//
// The order is owner, then the root of every Agent thread a dismissal writes a mark for, then
// the caller's rows, then the agent rows it writes, then the project its events name. Steps 2
// and 4 are the dismiss route's alone - this button writes no dismissal - and they are numbered
// here rather than there because one order written in two places is two orders as soon as one
// of them is edited:
//
//  1. the owner row of every thread it marks, `for share`, documents then issues, in ascending
//     owner-key order (lockInboxReadOwners, inboxReadLockDocuments);
//  2. the root message of every Agent thread a dismissal writes a mark for, `for update`, in
//     ascending id order (inboxDismissLockAgentRoots, api/inbox_dismiss.go), which the version
//     check that follows reads each thread under (inboxDismissCheckAgentRoots) before the insert
//     writes any mark;
//  3. the caller's user_threads rows, `for no key update`, in ascending thread_id order
//     (inboxReadLockThreads), which the update that follows re-locks;
//  4. the caller's own user_agent_thread_dismissed rows, `for no key update`, in ascending
//     message_id order (inboxDismissLockAgentThreads, api/inbox_dismiss.go), which the insert
//     or the delete that follows re-locks;
//  5. the caller's dead user_agent_reply_read rows, `for no key update`, in ascending reply_id
//     order, which the same statement deletes (pruneAgentReplies, api/state.go); the other
//     writer of that table, putUserAgentState, takes them in that order too;
//  6. the project of every row that update moved, through LockOwners, in sorted key order and
//     before the first append.
//
// Step 2 comes before the caller's rows because that is the order a session's reply takes those
// two in. Postgres fires a table's row triggers in name order, so on messages
// user_agent_thread_dismissed_clear (migration 20261105093507) runs before user_threads_message
// (0095, attached by 0097) on an issue message, and on an Agent thread's reply it is the only
// one of the two that runs at all: that other trigger returns at once for a message with no
// issue key, and an Agent thread's messages carry none. So a session's reply takes the thread's
// root - the row the clear trigger locks before its delete - before any row of this caller's it
// reaches. A dismiss that took the caller's rows first and the root second would be the opposite
// order, and the two would meet each holding what the other waits for.
//
// PUT /api/v1/me/threads/{id}/read takes that same order over its one thread, and the two need
// the same one: both take the owner `for share`, two share locks do not conflict, so the owner
// orders nothing between them, and one of them holding the project while the other holds the
// row and waits for the project is a cycle Postgres breaks with a 40P01 that the caller reads
// as a 500. Rows before project rather than project before rows, because that is where the
// one-thread route's project lock already falls: its Append takes it (events.Broker.Append),
// after the row and only when the mark moved. The other order would have every revisit that
// moves nothing queue on the project row - the busiest row the Inbox touches - behind every
// write in that project, for an event it was never going to append.
//
// Two presses of this button by one person - two tabs, two devices, a narrowed press against an
// unfiltered one - are ordered by step 3 and nothing else: they can name the same threads and
// the same projects, and both take their owners `for share`. Step 3 is a pass of its own rather
// than the update's own row locks because `thread_id = any($3)` locks what its plan reaches in
// the order it reaches it, and the plans differ with the size of the array: a press of a
// thousand threads is planned as a bitmap heap scan, which locks in heap order, where a small
// press and the generic plan take an index scan on the primary key, which locks in thread_id
// order. With random thread ids those two orders disagree, so without the pass two presses over
// one set of rows could each hold a row the other waits on (TestInboxReadRacingAnotherMarkAll
// forces the two plans apart and is red without it). The pass costs one index scan of the rows
// the press named, and it takes `for no key update` on every row it named rather than on the
// ones the update moves, so a second press waits for the first over rows it would have skipped.
//
// An ordinary writer is ordered by the owner instead. A comment, an answer or an issue message
// takes its issue or document `for no key update` (owner.go, ownerRowLock) before its trigger
// moves the user_threads rows of everyone in the thread; that level conflicts with the
// `for share` taken here, and every row such a trigger moves belongs to a thread on the owner
// it holds, which is an owner this transaction holds too, since it locks the owner of every
// thread it marks. One of the two therefore waits at the owner while holding nothing the other
// wants, whichever order the writer itself reaches the project in: a block ask's answer appends
// an anchor refresh, and so takes the project, before its own update moves those rows, and it
// is ordered all the same.
func (s *server) markInboxReadThreads(
	ctx context.Context, actor model.Actor, readThrough string, scope inboxReadScope,
) (int, []model.Event, error) {
	login := model.CanonicalLogin(actor.ID)
	tx, err := s.begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "set local statement_timeout = '"+inboxReadStatementTimeout+"'"); err != nil {
		return 0, nil, fmt.Errorf("bound the mark-all's statements: %w", err)
	}
	read, err := readInboxReadThreads(ctx, tx, actor, login,
		inboxReadReplyThreads, inboxReadAgentThreads, readThrough, scope)
	if err != nil {
		return 0, nil, err
	}
	if err := lockInboxReadOwners(ctx, tx, read); err != nil {
		return 0, nil, err
	}

	events := []model.Event{}
	marked := 0
	if len(read.threads) > 0 {
		projects, err := writeInboxThreadMarks(ctx, tx, login, read.threads, inboxReadMarkThreads, []any{readThrough})
		if err != nil {
			return 0, nil, fmt.Errorf("mark the inbox's threads read: %w", err)
		}
		marked += projects.marked
		for _, project := range projects.keys {
			// One event per project, naming no thread: the viewer's other tabs and devices
			// refetch the list, which is what a thread's own mark asks of them too.
			events = append(events, projectOwner(project).event(
				"user_thread_state.updated", actor, model.UserThreadStateEventPayload{Login: login},
			))
		}
	}
	if len(read.replies) > 0 {
		sessions, roots, err := s.markInboxReadAgentReplies(ctx, tx, login, read)
		if err != nil {
			return 0, nil, err
		}
		marked += roots
		for _, session := range sessions {
			events = append(events, userAgentStateEvent(actor, session))
		}
	}
	return s.commitInboxMarks(ctx, tx, marked, events)
}

// commitInboxMarks ends both write routes over these rows. A request that changed nothing
// announces nothing and has nothing to commit: the Inbox marks itself read on every click, and
// each event would refetch the list in every tab the viewer has open. A request that changed
// something takes the project of every event it appends - step 6 of the lock order above, in one
// sorted order after the rows it wrote and before the first append, an agent reply's event naming
// no owner and so taking no lock here - appends them and commits. It answers the count its caller
// answers, 0 when nothing changed, and the events to publish once the transaction has committed.
func (s *server) commitInboxMarks(
	ctx context.Context, tx pgx.Tx, changed int, events []model.Event,
) (int, []model.Event, error) {
	if len(events) == 0 {
		return 0, nil, tx.Rollback(ctx)
	}
	if err := s.deps.Events.LockOwners(ctx, tx, events...); err != nil {
		return 0, nil, fmt.Errorf("lock the changed threads' projects: %w", err)
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

// readInboxReadThreads reads both kinds of thread one filtered request matches, and the owners
// to lock for them, each list sorted so the locks that follow are taken in one order. Both write
// routes over these rows read through it - the mark-all with its own two statements and
// read_through, the dismiss route with its plan's two and dismiss_through
// (readInboxDismissThreads, api/inbox_dismiss.go) - which is why the two statements and the mark
// are given to it rather than named in it. The mark is bound rather than read here, so it is
// whatever the caller binds its own text as: the mark-all's string, the dismissal's *string.
func readInboxReadThreads(
	ctx context.Context, tx pgx.Tx, actor model.Actor, login string,
	replyThreads, agentThreads string, mark any, scope inboxReadScope,
) (inboxReadThreads, error) {
	read := newInboxReadThreads()
	if scope.wantsReplyThreads() {
		if err := readInboxReplyRows(ctx, tx, &read, replyThreads,
			login, scope.issue, scope.artifact, mark,
			scope.priorities, scope.priorityNone, scope.projects); err != nil {
			return read, err
		}
	}
	if scope.wantsAgentThreads() {
		args := append(unreadDirectRepliesArgs(actor.ID, nil), mark)
		if err := readInboxAgentRows(ctx, tx, &read, agentThreads, args); err != nil {
			return read, err
		}
	}
	return read, nil
}

// readInboxReplyRows reads the Reply threads one statement matches, the owners to lock for them
// and the version of each row, each list sorted. Both write routes over these rows read through
// it: their statements differ in the state they match and in the mark they narrow by
// (inboxReplyMatchQuery), and every one of them answers the version (api/inbox_dismiss.go).
func readInboxReplyRows(
	ctx context.Context, tx pgx.Tx, read *inboxReadThreads, query string, args ...any,
) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the inbox's threads: %w", err)
	}
	defer rows.Close()
	documents, issues := map[string]struct{}{}, map[string]struct{}{}
	// The scan's targets are the same for every row, so they are taken once and read out below.
	var thread string
	var issueKey, artifactID *string
	var version int64
	for rows.Next() {
		if err := rows.Scan(&thread, &issueKey, &artifactID, &version); err != nil {
			return fmt.Errorf("scan one of the inbox's threads: %w", err)
		}
		read.threads = append(read.threads, thread)
		read.replyVersions[thread] = version
		if issueKey != nil {
			issues[*issueKey] = struct{}{}
		}
		if artifactID != nil {
			documents[*artifactID] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the inbox's threads: %w", err)
	}
	read.documents = slices.Sorted(maps.Keys(documents))
	read.issues = slices.Sorted(maps.Keys(issues))
	return nil
}

// readInboxAgentRows reads the Agent threads one statement matches: each root, its session, the
// replies a write that marks the thread read writes by id, sorted as those writes take them, and
// the newest session reply in the root's own thread, which every one of these statements answers
// (inboxAgentRootsQuery). The roots are sorted too, for the dismissal that writes one row per
// root (api/inbox_dismiss.go).
func readInboxAgentRows(
	ctx context.Context, tx pgx.Tx, read *inboxReadThreads, query string, args []any,
) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("read the inbox's agent threads: %w", err)
	}
	defer rows.Close()
	sessions := map[string]string{}
	var root, session, newest string
	var replies []string
	for rows.Next() {
		if err := rows.Scan(&root, &session, &replies, &newest); err != nil {
			return fmt.Errorf("scan one of the inbox's agent threads: %w", err)
		}
		read.agentRoots = append(read.agentRoots, root)
		read.rootSessions[root] = session
		read.rootVersions[root] = newest
		for _, reply := range replies {
			read.replies = append(read.replies, reply)
			read.roots[reply] = root
			sessions[reply] = session
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read the inbox's agent threads: %w", err)
	}
	slices.Sort(read.agentRoots)
	// One lock order for the new rows, the ids' own, as putUserAgentState sorts them: two
	// writes naming the same replies in opposite orders would wait on each other.
	slices.Sort(read.replies)
	read.sessions = make([]string, 0, len(read.replies))
	for _, reply := range read.replies {
		read.sessions = append(read.sessions, sessions[reply])
	}
	return nil
}

// lockInboxReadOwners takes the owner row of every thread this request marks, step 1 of the
// lock order markInboxReadThreads writes down. The caller's rows follow, in
// writeInboxThreadMarks, and the projects after them.
func lockInboxReadOwners(ctx context.Context, tx pgx.Tx, read inboxReadThreads) error {
	if len(read.documents) > 0 {
		if _, err := tx.Exec(ctx, inboxReadLockDocuments, read.documents); err != nil {
			return fmt.Errorf("lock the marked threads' documents: %w", err)
		}
	}
	if len(read.issues) > 0 {
		if _, err := tx.Exec(ctx, inboxReadLockIssues, read.issues); err != nil {
			return fmt.Errorf("lock the marked threads' issues: %w", err)
		}
	}
	return nil
}

// inboxReadMarked is what one mark statement moved: how many rows, and the distinct projects they
// belong to, sorted, which is one event each.
type inboxReadMarked struct {
	marked int
	keys   []string
}

// writeInboxThreadMarks takes the caller's rows in thread_id order, runs one mark statement over
// them and reads back the project of every row it changed. A row the statement leaves alone - a
// thread already read past the mark, one already dismissed, one that carries no dismissal under a
// restore - is locked with the rest, since what orders two requests must be the rows they named
// rather than the rows one of them happens to move, and it is not among those answered.
//
// Every statement it runs binds the caller ($1), the mark it stores when it stores one, the rows,
// and last whatever the statement checks those rows against (inboxMarkArgs), so mark carries that
// one parameter or none - a restore stores none - and seen the dismissal's versions or none: the
// mark-all checks no version and binds neither (api/inbox_dismiss.go).
func writeInboxThreadMarks(
	ctx context.Context, tx pgx.Tx, login string, threads []string, query string, mark []any, seen ...any,
) (inboxReadMarked, error) {
	moved := inboxReadMarked{}
	// Step 3 of that order: the update below locks what its plan reaches in the order it
	// reaches it, which is not one order across two presses (markInboxReadThreads).
	if _, err := tx.Exec(ctx, inboxReadLockThreads, login, threads); err != nil {
		return moved, fmt.Errorf("lock the caller's threads: %w", err)
	}
	rows, err := tx.Query(ctx, query, inboxMarkArgs(login, mark, threads, seen...)...)
	if err != nil {
		return moved, err
	}
	defer rows.Close()
	projects := map[string]struct{}{}
	for rows.Next() {
		var project string
		if err := rows.Scan(&project); err != nil {
			return moved, err
		}
		moved.marked++
		projects[project] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return moved, err
	}
	moved.keys = slices.Sorted(maps.Keys(projects))
	return moved, nil
}

// inboxMarkArgs are one mark statement's parameters, the shape every mark on these two routes
// takes: the caller, the mark it stores if it stores one, the rows it writes over, and what it
// checks them against if it checks anything.
func inboxMarkArgs(login string, mark []any, rows []string, seen ...any) []any {
	return append(append(append([]any{login}, mark...), rows), seen...)
}

// markInboxReadAgentReplies writes the agent replies by id and prunes what the sessions' own read
// marks already cover. It answers the sessions whose insert wrote a row, sorted, which is one
// event each, and how many agent threads those rows belong to, which the answer counts.
func (s *server) markInboxReadAgentReplies(
	ctx context.Context, tx pgx.Tx, login string, read inboxReadThreads,
) ([]string, int, error) {
	rows, err := tx.Query(ctx, inboxReadMarkAgentReplies, login, read.sessions, read.replies)
	if err != nil {
		return nil, 0, fmt.Errorf("mark the inbox's agent replies read: %w", err)
	}
	defer rows.Close()
	sessions, roots := map[string]struct{}{}, map[string]struct{}{}
	for rows.Next() {
		var session, reply string
		if err := rows.Scan(&session, &reply); err != nil {
			return nil, 0, fmt.Errorf("scan a marked agent reply: %w", err)
		}
		sessions[session] = struct{}{}
		roots[read.roots[reply]] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("mark the inbox's agent replies read: %w", err)
	}
	touched := slices.Sorted(maps.Keys(sessions))
	if len(touched) > 0 {
		// Step 5 of the lock order above: the dead rows, taken in one order before the delete
		// reaches them, as putUserAgentState takes them. Nothing this request writes is ever
		// dead (the insert above skips what the mark covers), so this clears only what an older
		// write left behind; why the statement locks them in that order is written on
		// pruneAgentReplies (api/state.go).
		if _, err := tx.Exec(ctx, pruneAgentReplies, login, touched); err != nil {
			return nil, 0, fmt.Errorf("prune the agent replies the read marks cover: %w", err)
		}
	}
	return touched, len(roots), nil
}
