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

// inboxDismissNamedThreads reads the Reply threads a request named by id ($2), the owners this
// transaction locks for them, and the version the page saw in each ($3, beside those ids), which
// is what the write checks the row against (inboxDismissMarkThreads). It narrows to no state: a
// thread the write leaves alone is locked with the rest, as the mark-all locks every thread it
// named, and a thread the caller has no row for answers nothing and changes nothing, as
// PUT /api/v1/me/threads/{id}/read does. That is also what makes it one statement for both
// directions, where the state-matched read above is two: a restore checks no version, so the
// array it binds is one null per row and the column it answers is read by nobody.
const inboxDismissNamedThreads = `
	select t.thread_id::text, t.issue_key, t.artifact_id::text, seen.last_reply_id::text
	from user_threads t
	join unnest($2::uuid[], $3::uuid[]) as seen(thread_id, last_reply_id)
	  on seen.thread_id = t.thread_id
	where t.login = $1
	order by t.thread_id`

// What this route's four Agent reads add to the mark-all's own (inboxAgentRootsQuery,
// api/inbox_read.go): a restore marks no thread read, so it selects no reply ids; a request that
// named its rows narrows the state's roots to those ids ($4 as the array) rather than to a mark;
// and such a request answers the version the page saw in each row ($5, beside those ids), where a
// filtered request answers the root's own newest reply (inboxAgentNewestReply), the server being
// the page there. A named restore names no version and binds one null per row.
const (
	inboxDismissNoReplies        = `'{}'::text[]`
	inboxDismissNamedRoots       = `r.root_id = any($4::uuid[])`
	inboxRestoreDismissedThrough = `r.dismissed_at <= least($4::text::timestamptz, now())`
	inboxDismissNamedVersion     = `
		(select named.seen::text from unnest($4::uuid[], $5::uuid[]) as named(root, seen)
		 where named.root = r.root_id)`
)

// The four Agent reads themselves, each over the list route's own roots for the state this request
// writes over (api/replies_agent.go): the Inbox's undismissed roots for a dismissal, the dismissed
// ones for a restore. Reading them through the list's own constants is what keeps this route and
// the Dismissed view from disagreeing about whether a thread is dismissed.
var (
	inboxDismissAgentThreads = inboxAgentRootsQuery(
		myAgentInboxRoots, inboxAgentUnreadReplies, inboxAgentNewestReply, inboxAgentRepliedThrough)
	inboxDismissAgentByID = inboxAgentRootsQuery(
		myAgentInboxRoots, inboxAgentUnreadReplies, inboxDismissNamedVersion, inboxDismissNamedRoots)
	inboxRestoreAgentThreads = inboxAgentRootsQuery(
		myAgentDismissedRoots, inboxDismissNoReplies, inboxAgentNewestReply, inboxRestoreDismissedThrough)
	inboxRestoreAgentThreadsByID = inboxAgentRootsQuery(
		myAgentDismissedRoots, inboxDismissNoReplies, inboxDismissNamedVersion, inboxDismissNamedRoots)
)

// inboxDismissLockAgentRoots takes the root message of every Agent thread a dismissal is about to
// write a mark for, step 2 of the lock order (markInboxReadThreads, api/inbox_read.go), in
// ascending id order so two dismisses over overlapping sets take them in one order. `for update`
// rather than `for share`, because it is two dismisses that must not interleave, while two session
// replies, which take the root `for key share` in the trigger of 20261105093507, need not wait on each
// other at all.
//
// It is what orders this route against that trigger, and neither order then loses a reply: a
// dismiss that takes the root first makes the reply wait, and the reply's trigger then finds the
// committed mark and deletes it; a reply that holds the root first makes this transaction wait,
// and the check below then reads a thread whose replies are not the ones the request named, so no
// mark is written for it.
//
// It must stay a statement of its own, and so must that check. Under read committed each
// statement takes a fresh snapshot when it begins, which is exactly what lets the check see the
// reply this lock waited for: folding the `for update` into a CTE of the check, or the check into
// the insert, would share one snapshot taken before the wait and close nothing.
const inboxDismissLockAgentRoots = `
	select 1 from messages
	where id = any($1::uuid[])
	order by id
	for update`

// inboxDismissCheckAgentRoots answers which of the Agent threads a dismissal read it may still
// write a mark for. It runs as a statement of its own, after the root lock above has been given
// and before the first write, so its snapshot holds every reply that committed while this
// transaction waited at that lock. A root it does not answer is dropped: no mark is written for
// it, it is not counted, and its replies are not marked read (dismissInboxAgentThreads), so the
// thread stays in the Inbox with that reply unread.
//
// A root passes two tests, and both ask one question - whether a reply the request never saw is
// about to be buried:
//
//  1. the thread's newest session reply is the one the request named ($4, beside the roots in
//     $3, null for a row the page showed no reply in). `order by created_at desc, id desc
//     limit 1` is the list route's own `newest` (myAgentRowFacts, api/replies_agent.go), which is
//     the reply the page showed in that row, so the two must go on naming one reply: a reader
//     changing either has to change the other. This catches every reply that lands as the newest
//     in its thread, which is every ordinary one.
//  2. the thread holds no session reply that is unread for this caller and is not among the
//     replies this request is about to mark read ($5, read.replies, which
//     markInboxReadAgentReplies writes). This catches what the first cannot: two replies whose
//     transactions interleave with inverted starts - B begins after A and commits before it -
//     leave A committing last under the older stamp, so the newest reply by stamp does not change
//     and the first test passes while A is unread. "Unread for this caller" is the list route's
//     own test (myAgentUnreadReplyTest against the session's mark, agentMarkOf,
//     api/replies_agent.go), built from those same fragments rather than spelled a second time.
//
// No timestamp decides either test. A write's own timestamp is its transaction's start, taken
// before any other transaction can see the write, so a reply that commits after a dismiss began
// can carry a stamp older than that dismiss's mark: comparing the two cannot tell a reply the
// person had seen from one they could not have.
var inboxDismissCheckAgentRoots = myAgentMarks + `
	named as (
		select n.root, n.seen, ` + agentMarkOf(agentSessionOf("m.target")) + ` as at
		from unnest($3::uuid[], $4::uuid[]) as n(root, seen)
		join messages m on m.id = n.root
	)
	select named.root::text
	from named
	where (` + agentThreadCTE("named.root") + `
		select t.id from thread t
		where t.author->>'kind' = 'session'
		order by t.created_at desc, t.id desc limit 1
	  ) is not distinct from named.seen
	  and not exists (` + agentThreadCTE("named.root") + `
		select 1 from thread t
		where t.author->>'kind' = 'session'
		  and t.` + myAgentUnreadReplyTest("t.id", "named.at") + `
		  and not (t.id = any($5::uuid[]))
	  )`

// inboxDismissMarkThreads writes the dismissal on every Reply thread it was given whose version
// still matches the one the request carries, and answers the project of each row it changed. A
// row already carrying a mark is left alone and not counted: for a Reply thread "dismissed" is
// exactly `dismissed_at is not null`, since user_threads_record clears the mark when somebody
// else writes again (migration 20261105093501), so a second dismissal of a thread nobody has
// written in since changes nothing. The mark is cast by Postgres from the caller's own text
// rather than bound as a parsed time, and stored as no later than now, exactly as the read mark
// is; a request that named threads rather than filters binds no text at all, and least() over a
// null mark is now(), the moment it was dismissed. read_through only moves forward. A timestamp
// as a value is sound; what a dismissal may not do is let one decide which rows it writes.
//
// The version is what decides that: last_reply_id, the newest reply the row held when the page
// showed it ($4, one per thread beside the ids in $3). A row whose last_reply_id has moved since
// is left alone, so a reply nobody has read is never dismissed under it, and one statement checks
// every row. The test is `is not distinct from` because a row nobody has written in holds no
// reply at all, and the page saw that absence as a version like any other.
//
// Why a version rather than a time. A write's timestamp is its transaction's start - replied_at
// is user_threads_record's p_at, which is transaction_timestamp() - taken before any other
// transaction can see the write, so a reply that commits after this request began can carry a
// stamp older than this request's mark. Comparing the two cannot tell a reply the person had seen
// from one they could not have, and a thread would be left dismissed with a reply nobody read.
//
// Why the version closes that window. last_reply_id is moved by user_threads_record in the same
// update that moves replied_at, under the row lock this transaction already holds (step 3 of the
// lock order, markInboxReadThreads, api/inbox_read.go). A reply that commits before this
// statement's own snapshot is in it, so the version differs and the row is skipped. A reply whose
// trigger takes the row first makes this update wait there, and read committed then re-checks
// this `where` against the row the trigger wrote - again a differing version, again skipped.
//
// inboxRestoreThreads is the other direction: it clears the mark on every row that carries one,
// and stores no mark and checks no version, which are the two parameters the two differ by
// (inboxMarkArgs).
const (
	inboxDismissMarkThreads = `
	update user_threads t
	set dismissed_at = least($2::text::timestamptz, now()),
	    read_through = greatest(t.read_through, least($2::text::timestamptz, now()))
	from unnest($3::uuid[], $4::uuid[]) as seen(thread_id, last_reply_id)
	where t.login = $1 and t.thread_id = seen.thread_id and t.dismissed_at is null
	  and t.last_reply_id is not distinct from seen.last_reply_id
	returning t.project_key`
	inboxRestoreThreads = `
	update user_threads set dismissed_at = null
	where login = $1 and thread_id = any($2::uuid[]) and dismissed_at is not null
	returning project_key`
)

// inboxDismissMarkAgentThreads writes one dismissal row per Agent thread it was given, in the
// roots' sorted order so two requests naming the same threads take the new rows in one order, as
// inboxReadMarkAgentReplies sorts the replies it writes. A thread that came back carries no row at
// all - the trigger of migration 20261105093507 deleted it when the session replied - so this
// writes it again; a row already at or after the mark is left alone and not counted, which is what
// a request repeated against an unchanged thread answers 0 for.
//
// It asks nothing about the thread's replies. Which roots it may write was decided before it ran,
// by inboxDismissCheckAgentRoots under the root lock, and a root that check did not answer is
// already gone from the list this statement is given. What stood here instead was a comparison of
// a reply's created_at with the mark, and a reply that commits after this request began can carry
// the older stamp of the two: that is the window the check closes and no comparison of times can.
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

// inboxDismissRequest is the body: either the row ids to change, with the version the page saw in
// each of them (seen_replies), or the list's own narrowing (inboxThreadFilters, api/inbox_read.go,
// which the mark-all takes too) with the mark to change through, never both, and restore, which
// puts the threads back in the Inbox instead of taking them out.
type inboxDismissRequest struct {
	Threads []string `json:"threads"`
	// SeenReplies is one entry per row threads names, keyed by the row id threads names it with:
	// the newest reply the page showed in that row - user_threads.last_reply_id for a `reply:`
	// row and the newest session reply's id for an `agent:` row, which are what
	// GET /api/v1/me/replies answers as that row's reply.id - or null for a row it showed no
	// reply in. A mark is written only where the row still holds what the page saw, so a reply
	// that landed since is never dismissed unread (readInboxDismissSeen).
	SeenReplies    map[string]*string `json:"seen_replies"`
	DismissThrough *string            `json:"dismiss_through"`
	Restore        bool               `json:"restore"`
	inboxThreadFilters
}

// inboxDismissResponse answers how many threads the request changed: the Reply threads whose mark
// moved plus the Agent threads whose dismissal row it wrote or deleted. A request that changes
// nothing answers 0.
type inboxDismissResponse struct {
	Dismissed int `json:"dismissed"`
}

// inboxDismissRows is the rows one request names, read off its body: the Reply threads and the
// Agent roots, each sorted, so the locks and the writes that follow are taken in one order, and
// the version the page saw in each row beside it.
type inboxDismissRows struct {
	threads []string
	roots   []string
	// seenThreads and seenRoots are one version per entry of threads and of roots, in those
	// slices' own order, which is how the reads bind them beside the ids. A restore names none
	// and checks none, so its entries stay null and no statement reads them back.
	seenThreads []*string
	seenRoots   []*string
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

// seenReplies is what this plan's Reply write checks each row against, bound after the rows
// (inboxMarkArgs): the version of every thread the read answered, in the order it answered them.
// A restore checks none, so it binds none and its statement takes no such parameter.
func (plan inboxDismissPlan) seenReplies(read inboxReadThreads) []any {
	if plan.restore {
		return nil
	}
	return []any{inboxDismissSeenOf(read.replyVersions, read.threads)}
}

// inboxDismissSeenOf lines a read's versions up with the rows it answered, which is the array a
// statement unnests beside those ids: one entry per row, in the rows' own order.
func inboxDismissSeenOf(versions map[string]*string, rows []string) []*string {
	seen := make([]*string, 0, len(rows))
	for _, row := range rows {
		seen = append(seen, versions[row])
	}
	return seen
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
	// One version per row, filled by readInboxDismissSeen for a dismissal and left null for a
	// restore, so a read binds an array of its ids' own length either way.
	rows.seenThreads = make([]*string, len(rows.threads))
	rows.seenRoots = make([]*string, len(rows.roots))
	return rows, nil
}

// readInboxDismissSeen reads seen_replies onto the rows a dismissal named: the newest reply the
// page showed in each, which is what the writes check the row against (inboxDismissMarkThreads
// and inboxDismissCheckAgentRoots). It is the page's answer to the question the filtered form
// answers out of the database, the server being the page there (readInboxDismissThreads).
//
// Three refusals of its own, each 400 INVALID_QUERY: a row threads names and seen_replies leaves
// out, since a dismissal with no version for a row is a dismissal nothing can check; a key naming
// a row threads does not, which is a page asking about a row it did not ask to dismiss; and a
// value that is neither null nor a uuid. The other two - seen_replies on a restore, and with the
// filtered form - belong to the handler, which knows the direction and the form (dismissInbox).
func readInboxDismissSeen(rows *inboxDismissRows, seen map[string]*string) error {
	missing := func(row string) error {
		return errorf(http.StatusBadRequest, "INVALID_QUERY",
			"seen_replies leaves %s out: a dismissal carries the newest reply the page showed in"+
				" every row threads names, or null for a row it showed none in", row)
	}
	versions := make(map[string]*string, len(seen))
	for key, value := range seen {
		// The key is read as a row id by the parser threads itself is read by, so a key spelled
		// in another casing names the row it means. A key of no shape at all names no row the
		// request named, which is the one refusal both it and an unnamed row answer.
		named, err := parseInboxDismissRows([]string{key})
		row := ""
		switch {
		case err != nil:
		case len(named.threads) == 1 && slices.Contains(rows.threads, named.threads[0]):
			row = inboxDismissReplyPrefix + named.threads[0]
		case len(named.roots) == 1 && slices.Contains(rows.roots, named.roots[0]):
			row = inboxDismissAgentPrefix + named.roots[0]
		}
		if row == "" {
			return errorf(http.StatusBadRequest, "INVALID_QUERY",
				"seen_replies names %s, which threads does not: it takes one entry per row"+
					" threads names", key)
		}
		if value != nil {
			parsed, err := uuid.Parse(*value)
			if err != nil {
				return errorf(http.StatusBadRequest, "INVALID_QUERY",
					"seen_replies[%s] names no reply: the value is the id of the newest reply that"+
						" row showed, a uuid, or null for a row that showed none", key)
			}
			id := parsed.String()
			value = &id
		}
		versions[row] = value
	}
	for i, thread := range rows.threads {
		version, carried := versions[inboxDismissReplyPrefix+thread]
		if !carried {
			return missing(inboxDismissReplyPrefix + thread)
		}
		rows.seenThreads[i] = version
	}
	for i, root := range rows.roots {
		version, carried := versions[inboxDismissAgentPrefix+root]
		if !carried {
			return missing(inboxDismissAgentPrefix + root)
		}
		rows.seenRoots[i] = version
	}
	return nil
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
	// seen_replies belongs to a dismissal that named its rows and to nothing else: a restore
	// clears the marks and checks nothing, and the filtered form reads each row's version in the
	// read that chose it (readInboxDismissThreads), the server standing in for the page there.
	if input.SeenReplies != nil && input.Restore {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest,
			"a restore clears the marks and takes no seen_replies")
		return
	}
	if input.SeenReplies != nil && !rows.named() {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest,
			"seen_replies goes with threads: under the list's filters the server reads each row's"+
				" newest reply itself")
		return
	}
	if rows.named() && !input.Restore {
		if err := readInboxDismissSeen(&rows, input.SeenReplies); err != nil {
			s.writeHandlerError(w, err)
			return
		}
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
// markInboxReadThreads (api/inbox_read.go) - steps 2 and 4 of that list, the roots of the Agent
// threads it writes marks for and the caller's own user_agent_thread_dismissed rows, are this
// route's alone, since the mark-all writes no dismissal. Taking that same order is what keeps a
// dismiss, a mark-all and a one-thread mark from crossing: all three take the owner `for share`,
// all three move rows of user_threads under one login, and two of them that took the row and the
// project in opposite orders would deadlock on the project row every time they met.
//
// Every write this route makes falls inside that order: the Reply write is one update over rows
// step 3 has already locked, as the mark-all's is and through the mark-all's own statement
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
	if !plan.restore && len(read.agentRoots) > 0 {
		if err := lockInboxDismissAgentRoots(ctx, tx, read.agentRoots); err != nil {
			return 0, nil, err
		}
		// Which of those roots the dismissal may still write, read once the lock has been given
		// and before the first write (inboxDismissCheckAgentRoots). The roots it does not answer
		// are dropped here, so nothing below writes, counts or marks a reply read for them.
		if err := checkInboxDismissAgentRoots(ctx, tx, actor, &read); err != nil {
			return 0, nil, err
		}
	}

	events := []model.Event{}
	changed := 0
	if len(read.threads) > 0 {
		moved, err := writeInboxThreadMarks(
			ctx, tx, login, read.threads, plan.markThreads, plan.writeMark(), plan.seenReplies(read)...)
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
//
// Each read answers the version of every row it chose, which is what the writes check the row
// against: for a request that named its rows the version is the one the page carried, bound
// beside the ids; for a filtered request it is the row's own, read here in the read that chose
// it, the server standing in for the page. One mechanism for both forms.
func readInboxDismissThreads(
	ctx context.Context, tx pgx.Tx, actor model.Actor, login string,
	plan inboxDismissPlan, rows inboxDismissRows, scope inboxReadScope,
) (inboxReadThreads, error) {
	read := newInboxReadThreads()
	if rows.named() {
		if len(rows.threads) > 0 {
			if err := readInboxReplyRows(ctx, tx, &read, inboxDismissNamedThreads,
				login, rows.threads, rows.seenThreads); err != nil {
				return read, err
			}
		}
		if len(rows.roots) > 0 {
			args := append(unreadDirectRepliesArgs(actor.ID, nil), rows.roots, rows.seenRoots)
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

// lockInboxDismissAgentRoots takes the root of every Agent thread this request is about to write a
// mark for, step 2 of the lock order (markInboxReadThreads, api/inbox_read.go): after the owners
// and before the caller's rows and every write, so the check that follows reads each thread in a
// snapshot taken once the root is this transaction's (inboxDismissLockAgentRoots).
//
// A restore takes no root. It deletes marks, and a session reply that commits beside it deletes
// the same mark through the trigger of 20261105093507, so whichever of the two runs first the
// thread ends in the Inbox, and a `for update` held over the rest of the transaction would stop
// that thread's replies for nothing.
func lockInboxDismissAgentRoots(ctx context.Context, tx pgx.Tx, roots []string) error {
	if _, err := tx.Exec(ctx, inboxDismissLockAgentRoots, roots); err != nil {
		return fmt.Errorf("lock the agent threads' roots: %w", err)
	}
	return nil
}

// checkInboxDismissAgentRoots asks which of the Agent threads this dismissal read it may still
// write a mark for (inboxDismissCheckAgentRoots) and drops the rest, so what follows writes,
// counts and marks read only the threads whose replies are the ones the request named.
//
// The marks it reads a reply against are unreadDirectRepliesCTE's own, so its first two
// parameters are that fragment's first two, built by unreadDirectRepliesArgs rather than spelled
// here. It narrows to no session, so it takes none of that fragment's third and numbers its own
// from there.
func checkInboxDismissAgentRoots(
	ctx context.Context, tx pgx.Tx, actor model.Actor, read *inboxReadThreads,
) error {
	marks := unreadDirectRepliesArgs(actor.ID, nil)
	seen := inboxDismissSeenOf(read.rootVersions, read.agentRoots)
	args := append(marks[:2:2], read.agentRoots, seen, read.replies)
	rows, err := tx.Query(ctx, inboxDismissCheckAgentRoots, args...)
	if err != nil {
		return fmt.Errorf("check the agent threads' replies: %w", err)
	}
	defer rows.Close()
	keep := map[string]struct{}{}
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return fmt.Errorf("scan a checked agent thread: %w", err)
		}
		keep[root] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check the agent threads' replies: %w", err)
	}
	read.keepAgentRoots(keep)
	return nil
}

// keepAgentRoots leaves the read holding the Agent threads given and nothing else: the roots in
// their own order, their sessions and their versions, and the replies this request marks read,
// which are what the writes below take as the whole of what they change.
func (read *inboxReadThreads) keepAgentRoots(keep map[string]struct{}) {
	roots := read.agentRoots[:0]
	for _, root := range read.agentRoots {
		if _, kept := keep[root]; !kept {
			delete(read.rootSessions, root)
			delete(read.rootVersions, root)
			continue
		}
		roots = append(roots, root)
	}
	read.agentRoots = roots
	replies, sessions := read.replies[:0], read.sessions[:0]
	for i, reply := range read.replies {
		if _, kept := keep[read.roots[reply]]; !kept {
			delete(read.roots, reply)
			continue
		}
		replies = append(replies, reply)
		sessions = append(sessions, read.sessions[i])
	}
	read.replies, read.sessions = replies, sessions
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
