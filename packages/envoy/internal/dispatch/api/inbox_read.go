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
// and the Agent read beside it (api/replies.go) - rather than predicates of its own: two
// spellings of "unread" would let the button mark a row the list never showed, or leave one it
// did.

// inboxReadReplyThreads reads the Reply threads one request marks: the caller's unread rows
// (myRepliesUnread, with the list route's own parameters $1 login, $2 issue, $3 document) whose
// newest reply is at or before the read mark ($4), of the priorities ($5, null for every one,
// with $6 for a thread whose issue carries none) and the projects ($7, null for every one) the
// request names. It answers each thread's id and the owner this transaction locks before it
// writes: its issue or its document. The project whose event announces the mark is not read
// here: the update answers the project of every row it moved, and those are the projects this
// transaction locks, after the rows (inboxReadLockDocuments).
//
// The rows are matched first, as myRepliesCount matches them, so the planner reads them through
// user_threads_unread rather than misjudging the stored flag; the narrowing this route adds is
// applied over that set, where the issue join costs one lookup per matched row.
const inboxReadReplyThreads = `
	with matched as materialized (
		select u.thread_id, u.replied_at, u.issue_key, u.artifact_id, u.project_key
		` + myRepliesUnread + `
	)
	select m.thread_id::text, m.issue_key, m.artifact_id::text
	from matched m
	left join issues i on i.key = m.issue_key
	where m.replied_at <= least($4::text::timestamptz, now())
	  and ($5::int[] is null or i.priority = any($5::int[]) or ($6::boolean and i.priority is null))
	  and ($7::text[] is null or m.project_key = any($7::text[]))
	order by m.thread_id`

// inboxReadAgentRest is what this route selects over the Inbox's one-off agent roots: each root
// holding a reply the caller has not read, whose newest reply is at or before the read mark ($4),
// with the unread replies the mark writes by id.
const inboxReadAgentRest = `
	select r.root_id::text, r.session_id, c.unread_reply_ids
	from roots r
	join counted c on c.root_id = r.root_id
	where r.replied_at <= least($4::text::timestamptz, now())
	order by r.root_id`

// inboxReadAgentThreads is the list route's own Agent read with that select over it: the head,
// the "a reply this login has not read" narrowing and the roots are myAgentHead, myAgentUnread
// and myAgentRoots, the constants myAgentRowsQuery is built from (api/replies.go), so a change
// to either moves the list and this button together.
const inboxReadAgentThreads = myAgentHead + myAgentUnread + myAgentRoots + inboxReadAgentRest

// inboxReadLockDocuments and inboxReadLockIssues take the owner rows of the threads this request
// marks - its documents, then its issues - in the ascending owner-key order eventOwnerKey builds
// and LockOwners sorts by ("artifact:" + id before "issue:" + key, as strings).
//
// The order this transaction keeps is the owner, then the caller's user_threads rows, then the
// project its events name, and PUT /api/v1/me/threads/{id}/read keeps that same one
// (api/replies.go, putThreadRead). It has to be the same order: both routes take the owner
// `for share`, two share locks do not conflict, so the owner orders nothing between the two,
// and one of them holding the project while the other holds the row and waits for the project
// is a cycle Postgres breaks with a 40P01 that the caller reads as a 500. Rows before project
// rather than project before rows, because that is where the one-thread route's project lock
// already falls: its Append takes it (events.Broker.Append), after the row and only when the
// mark moved. The other order would have every revisit that moves nothing queue on the project
// row - the busiest row the Inbox touches - behind every write in that project, for an event it
// was never going to append.
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
//
// The rows are ordered before they are locked, so the lock is taken in the sorted order rather
// than whatever order the scan returns.
const (
	inboxReadLockDocuments = `select 1 from artifacts where id = any($1::uuid[]) order by id for share`
	inboxReadLockIssues    = `select 1 from issues where key = any($1::text[]) order by key for share`
)

// inboxReadMarkThreads moves the caller's mark on every Reply thread it was given, and answers
// the project of each row it moved. read_through is cast by Postgres from the caller's own text
// rather than bound as a parsed time, and stored as no later than now, exactly as putThreadRead
// stores one mark: a mark never moves back, and a time carrying nanoseconds is not truncated a
// microsecond short of the write it means to cover.
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

// inboxReadPruneAgentReplies is putUserAgentState's prune over the sessions this request wrote
// for: a reply at or before the session's read mark is read whatever user_agent_reply_read holds,
// so its row is dead and is deleted here. Nothing this request writes is ever dead - the insert
// above skips what the mark covers - so this only clears what an older write left behind, and it
// keeps the two writers to one rule about which rows may exist.
const inboxReadPruneAgentReplies = `
	delete from user_agent_reply_read
	using messages, user_agent_read
	where user_agent_reply_read.login = $1 and user_agent_reply_read.session_id = any($2::text[])
	  and user_agent_read.login = $1 and user_agent_read.session_id = user_agent_reply_read.session_id
	  and messages.id = user_agent_reply_read.reply_id
	  and messages.created_at <= user_agent_read.read_through`

// inboxReadStatementTimeout bounds every statement of the mark-all's transaction. One request
// marking the acceptance seed - 600 unread Reply threads and 20 Agent threads for one login -
// takes 24 to 28 ms end to end on the test database, so ten seconds leaves two and a half orders
// of magnitude of headroom for a busier server and cuts no honest request off. The ceiling
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

// inboxReadRequest is the body: the read mark, and the same narrowing the Inbox's filter writes
// into the list route's query. Every list is optional and an absent one means every value.
type inboxReadRequest struct {
	ReadThrough *string  `json:"read_through"`
	Kind        []string `json:"kind"`
	Priority    []string `json:"priority"`
	Urgency     []string `json:"urgency"`
	Project     []string `json:"project"`
	Issue       string   `json:"issue"`
	Artifact    string   `json:"artifact"`
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
func parseInboxReadScope(input inboxReadRequest) (inboxReadScope, error) {
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
// threads to mark with the owners to lock for them, and the Agent threads' unread replies, their
// sessions and their roots.
type inboxReadThreads struct {
	threads   []string
	documents []string
	issues    []string
	// sessions and replies are one agent reply each, in the replies' sorted order, and roots
	// names each reply's thread, which is what the answer counts.
	sessions []string
	replies  []string
	roots    map[string]string
}

// markInboxRead answers POST /api/v1/me/inbox/read.
func (s *server) markInboxRead(w http.ResponseWriter, r *http.Request) {
	actor, ok := s.requireHuman(w, r)
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
	scope, err := parseInboxReadScope(input)
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
	read, err := s.readInboxReadThreads(ctx, tx, actor, login, readThrough, scope)
	if err != nil {
		return 0, nil, err
	}
	if err := lockInboxReadOwners(ctx, tx, read); err != nil {
		return 0, nil, err
	}

	events := []model.Event{}
	marked := 0
	if len(read.threads) > 0 {
		projects, err := markInboxReadReplyThreads(ctx, tx, login, readThrough, read.threads)
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
	// A request that moved nothing announces nothing and has nothing to commit: the Inbox marks
	// itself read on every click, and each event would refetch the list in every tab the viewer
	// has open.
	if len(events) == 0 {
		return 0, nil, tx.Rollback(ctx)
	}
	// Every project this transaction announces in, taken in one sorted order after the rows it
	// marked and before the first append (inboxReadLockDocuments). An agent reply's event names
	// no owner, so it takes no lock here.
	if err := s.deps.Events.LockOwners(ctx, tx, events...); err != nil {
		return 0, nil, fmt.Errorf("lock the marked threads' projects: %w", err)
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
	return marked, appended, nil
}

// readInboxReadThreads reads both kinds of thread this request marks, and the owners to lock for
// them, each list sorted so the locks that follow are taken in one order.
func (s *server) readInboxReadThreads(
	ctx context.Context, tx pgx.Tx, actor model.Actor, login, readThrough string, scope inboxReadScope,
) (inboxReadThreads, error) {
	read := inboxReadThreads{roots: map[string]string{}}
	if scope.wantsReplyThreads() {
		rows, err := tx.Query(ctx, inboxReadReplyThreads,
			login, scope.issue, scope.artifact, readThrough,
			scope.priorities, scope.priorityNone, scope.projects)
		if err != nil {
			return read, fmt.Errorf("read the inbox's unread threads: %w", err)
		}
		defer rows.Close()
		documents, issues := map[string]struct{}{}, map[string]struct{}{}
		for rows.Next() {
			var thread string
			var issueKey, artifactID *string
			if err := rows.Scan(&thread, &issueKey, &artifactID); err != nil {
				return read, fmt.Errorf("scan an unread thread: %w", err)
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
			return read, fmt.Errorf("read the inbox's unread threads: %w", err)
		}
		read.documents = slices.Sorted(maps.Keys(documents))
		read.issues = slices.Sorted(maps.Keys(issues))
	}
	if scope.wantsAgentThreads() {
		args := append(unreadDirectRepliesArgs(actor.ID, nil), readThrough)
		rows, err := tx.Query(ctx, inboxReadAgentThreads, args...)
		if err != nil {
			return read, fmt.Errorf("read the inbox's unread agent threads: %w", err)
		}
		defer rows.Close()
		sessions := map[string]string{}
		for rows.Next() {
			var root, session string
			var replies []string
			if err := rows.Scan(&root, &session, &replies); err != nil {
				return read, fmt.Errorf("scan an unread agent thread: %w", err)
			}
			for _, reply := range replies {
				read.replies = append(read.replies, reply)
				read.roots[reply] = root
				sessions[reply] = session
			}
		}
		if err := rows.Err(); err != nil {
			return read, fmt.Errorf("read the inbox's unread agent threads: %w", err)
		}
		// One lock order for the new rows, the ids' own, as putUserAgentState sorts them: two
		// writes naming the same replies in opposite orders would wait on each other.
		slices.Sort(read.replies)
		read.sessions = make([]string, 0, len(read.replies))
		for _, reply := range read.replies {
			read.sessions = append(read.sessions, sessions[reply])
		}
	}
	return read, nil
}

// lockInboxReadOwners takes the owner row of every thread this request marks, in the one
// ascending owner-key order (inboxReadLockDocuments). The projects are taken after the rows,
// by markInboxReadThreads, and only for the rows it moved.
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

// markInboxReadReplyThreads runs the Reply threads' one update and reads back the project of
// every row it moved. A thread already read past the mark is not among them: its read_through is
// the later one, the update leaves it alone, and the answer does not count it.
func markInboxReadReplyThreads(ctx context.Context, tx pgx.Tx, login, readThrough string, threads []string) (inboxReadMarked, error) {
	moved := inboxReadMarked{}
	rows, err := tx.Query(ctx, inboxReadMarkThreads, login, readThrough, threads)
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
		if _, err := tx.Exec(ctx, inboxReadPruneAgentReplies, login, touched); err != nil {
			return nil, 0, fmt.Errorf("prune the agent replies the read marks cover: %w", err)
		}
	}
	return touched, len(roots), nil
}
