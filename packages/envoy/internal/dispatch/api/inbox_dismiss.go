package api

import (
	"bytes"
	"context"
	"encoding/json"
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
// transaction locks for them and each row's own version, which every read of these rows answers
// (readInboxReplyRows, api/inbox_read.go). What the write holds a named row to is the version
// the page drew, which the request carries and readInboxDismissThreads writes over this one. It
// narrows to no state: a thread the write leaves alone is locked with the rest, as the mark-all
// locks every thread it named, and a thread the caller has no row for answers nothing and
// changes nothing, as PUT /api/v1/me/threads/{id}/read does. That is also what makes it one
// statement for both directions, where the state-matched read above is two.
//
// The ids are matched against the row's own columns, so the read stays on the caller's own key
// (user_threads_pkey, which the plan test pins).
const inboxDismissNamedThreads = `
	select t.thread_id::text, t.issue_key, t.artifact_id::text, t.version
	from user_threads t
	where t.login = $1 and t.thread_id = any($2::uuid[])
	order by t.thread_id`

// What this route's four Agent reads add to the mark-all's own (inboxAgentRootsQuery,
// api/inbox_read.go): a restore marks no thread read, so it selects no reply ids; and a request
// that named its rows narrows the state's roots to those ids ($4 as the array) rather than to a
// mark. All four answer each root's own newest reply, as every one of these reads does, and a
// request that named its rows has the reply the page drew written over it
// (readInboxDismissThreads).
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

// inboxDismissLockAgentRoots takes the root message of every Agent thread a dismissal is about to
// write a mark for, step 2 of the lock order (markInboxReadThreads, api/inbox_read.go), in
// ascending id order so two dismisses over overlapping sets take them in one order.
//
// This comment is where `for update` is accounted for, and the route's own steps, the race tests
// below and packages/envoy/AGENTS.md point here rather than saying it again. It is the one level
// that conflicts with the `for key share` the clearing trigger of 20261105093507 takes on this
// same row before its delete - that migration's header says why the trigger takes that level -
// and that conflict is the whole of what orders this route against the trigger. Two dismisses
// need nothing from it: they are ordered by the caller's own rows, steps 3 and 4, whatever level
// this one is taken at.
//
// Neither order then loses a reply: a dismiss that takes the root first makes the reply wait, and
// the reply's trigger then finds the committed mark and deletes it; a reply that holds the root
// first makes this transaction wait, and the check below then reads a thread whose replies are
// not the ones the request named, so no mark is written for it.
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
// It reads each root's thread through the list route's own lateral (myAgentRootsWithFacts,
// api/replies_agent.go), so the thread is walked once for both tests below and the reply the
// page drew and the reply this check reads are one reply by construction, not by two spellings
// of `order by created_at desc, id desc limit 1` kept in step.
//
// A root passes two tests, and both ask one question - whether a write the request never drew is
// about to be buried:
//
//  1. the thread's newest session reply is the one the request drew ($4, beside the roots in
//     $3), which is agent.reply_id, what the list answered as that row's reply. This catches
//     every reply that lands as the newest in its thread, which is every ordinary one.
//  2. every session reply that is unread for this caller is one the request drew as unread ($5,
//     the ids of every root's drawn unread replies in one array, reply ids being unique). This
//     catches what the first cannot: two replies whose transactions interleave with inverted
//     starts - B begins after A and commits before it - leave A committing last under the older
//     stamp, so the newest reply by stamp does not change and the first test passes while A is
//     unread. The set it is held to is the page's own, not this transaction's read of what is
//     unread now, which would already hold a reply the page never drew.
//
// No timestamp decides either test, for the reason stated on migration 20261105093501's
// increment.
var inboxDismissCheckAgentRoots = myAgentMarks + `
	roots as (
		select drawn.root as root_id, drawn.newest
		from unnest($3::uuid[], $4::uuid[]) as drawn(root, newest)
	)
	select r.root_id::text` + myAgentRootsWithFacts + `
	where agent.reply_id = r.newest
	  and agent.unread_reply_ids::uuid[] <@ $5::uuid[]`

// inboxDismissMarkThreads writes the dismissal on every Reply thread it was given whose version
// is still the one the request carries, and answers the project of each row it changed. A row
// already carrying a mark is left alone and not counted: for a Reply thread "dismissed" is
// exactly `dismissed_at is not null`, since user_threads_record clears the mark when somebody
// else writes again (migration 20261105093501), so a second dismissal of a thread nobody has
// written in since changes nothing. The mark is cast by Postgres from the caller's own text
// rather than bound as a parsed time, and stored as no later than now, exactly as the read mark
// is; a request that named threads rather than filters binds no text at all, and least() over a
// null mark is now(), the moment it was dismissed. read_through only moves forward. A timestamp
// as a value is sound, and a filtered dismissal is matched by one: it writes the rows whose
// replied_at is at or before dismiss_through, the as_of rule the mark-all takes too (INFRA-450's
// spec, v32). What decides a named dismissal's rows is the version.
//
// That version is user_threads.version ($4, one per thread beside the ids in $3), which the list
// answered for the row and which user_threads_record increments on every write by somebody else,
// in the same update that clears the mark (migration 20261105093501). A row whose version has
// moved since is left alone, so a write nobody has read is never dismissed under it, and one
// statement checks every row.
//
// Why a version rather than a time at all is written once, on the header of migration
// 20261105093501, which is where every write's stamp - and what a mark may therefore be
// compared with - is accounted for; this statement points there rather than restating it.
//
// Why the version closes the window that header names. The increment runs under the row lock
// this transaction already holds (step 3 of the lock order, markInboxReadThreads,
// api/inbox_read.go). A write that committed before this statement's own snapshot is in it, so
// the version differs and the row is skipped. A write whose trigger takes the row first makes
// this update wait there, and read committed then re-checks this `where` against the row the
// trigger wrote - again a differing version, again skipped.
//
// inboxRestoreThreads is the other direction: it clears the mark on every row that carries one,
// and stores no mark and checks no version, which are the two parameters the two differ by
// (inboxMarkArgs).
const (
	inboxDismissMarkThreads = `
	update user_threads t
	set dismissed_at = least($2::text::timestamptz, now()),
	    read_through = greatest(t.read_through, least($2::text::timestamptz, now()))
	from unnest($3::uuid[], $4::bigint[]) as seen(thread_id, version)
	where t.login = $1 and t.thread_id = seen.thread_id and t.dismissed_at is null
	  and t.version = seen.version
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
// It asks nothing about the thread's replies. Which roots it may write was decided before it
// ran, by inboxDismissCheckAgentRoots under the root lock, and a root that check did not answer
// is already gone from the list this statement is given.
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

// inboxDismissRequest is the body: either the row ids to change, with what the page drew in each
// of them (seen_replies), or the list's own narrowing (inboxThreadFilters, api/inbox_read.go,
// which the mark-all takes too) with the mark to change through, never both, and restore, which
// puts the threads back in the Inbox instead of taking them out.
type inboxDismissRequest struct {
	Threads []string `json:"threads"`
	// SeenReplies is one object per row threads names, keyed by the row id threads names it
	// with, holding what the page drew in that row: inboxDismissSeenReply for a reply: row and
	// inboxDismissSeenAgent for an agent: one. A mark is written only where the thread still
	// holds what the page drew, so a write that landed since is never dismissed unread
	// (readInboxDismissSeen). The entries stay raw here so each is read in the shape its own
	// row's kind takes, and refused in this route's words rather than the decoder's.
	SeenReplies    map[string]json.RawMessage `json:"seen_replies"`
	DismissThrough *string                    `json:"dismiss_through"`
	Restore        bool                       `json:"restore"`
	inboxThreadFilters
}

// inboxDismissSeenReply is one of those objects for a Reply row and inboxDismissSeenAgent one
// for an Agent row, the two shapes the contract spells (InboxDismissSeenReply and
// InboxDismissSeenAgent, packages/contracts/src/dispatch-api.ts). A Reply row carries version
// alone: user_threads.version as GET /api/v1/me/replies answered it, which is what the mark is
// written under (inboxDismissMarkThreads). An Agent row has no user_threads row, so it carries
// the two things the page drew of its thread instead: newest_reply, the newest session reply it
// showed (the row's reply.id, which every Agent row has), and unread_replies, the ids it drew as
// unread (unread_reply_ids), which may be empty - the two inboxDismissCheckAgentRoots holds the
// thread to.
//
// A row's entry is read in its own kind's shape and no other, so a field of the other kind is an
// unknown one and refused there. Each field is a pointer, so an entry that leaves one out and
// one that carries it zero are different requests (readInboxDismissSeen).
type inboxDismissSeenReply struct {
	Version *int64 `json:"version"`
}

type inboxDismissSeenAgent struct {
	NewestReply   *string   `json:"newest_reply"`
	UnreadReplies *[]string `json:"unread_replies"`
}

// inboxDismissResponse answers how many threads the request changed: the Reply threads whose mark
// moved plus the Agent threads whose dismissal row it wrote or deleted. A request that changes
// nothing answers 0.
type inboxDismissResponse struct {
	Dismissed int `json:"dismissed"`
}

// inboxDismissRows is the rows one request names, read off its body: the Reply threads and the
// Agent roots, each sorted, so the locks and the writes that follow are taken in one order, and
// what the page drew in them.
type inboxDismissRows struct {
	threads []string
	roots   []string
	// What the page drew, read off seen_replies (readInboxDismissSeen) and carried into the read
	// as it stands (readInboxDismissThreads): the version of each thread named and the newest
	// reply of each root, keyed by that row's id as the read keys them, and seenUnread the
	// unread replies it drew in all of the roots at once - reply ids are unique, so one flat set
	// serves every root, which is how the check binds it. A restore drew nothing and is held to
	// nothing, so all three stay empty.
	seenVersions map[string]int64
	seenNewest   map[string]string
	seenUnread   []string
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

// seenReplies is what this plan's Reply write holds each row to, bound after the rows
// (inboxMarkArgs): the version of every thread the read answered, in the order it answered them.
// A restore holds a row to nothing, so it binds none and its statement takes no such parameter.
func (plan inboxDismissPlan) seenReplies(read inboxReadThreads) []any {
	if plan.restore {
		return nil
	}
	return []any{inboxDismissSeenOf(read.replyVersions, read.threads)}
}

// inboxDismissSeenOf lines what the page drew up with the rows a read answered, which is the
// array a statement unnests beside those ids: one entry per row, in the rows' own order.
func inboxDismissSeenOf[Drawn any](drawn map[string]Drawn, rows []string) []Drawn {
	seen := make([]Drawn, 0, len(rows))
	for _, row := range rows {
		seen = append(seen, drawn[row])
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
	return rows, nil
}

// inboxDismissSeenTakesReply and inboxDismissSeenTakesAgent are what each kind of row carries,
// and the words an entry of any other shape is refused in: what a row's kind takes is one fact,
// so a page that sent something else - a value of the wrong type, a field of the other kind, or
// one of its own left out - is told that whole fact once.
const (
	inboxDismissSeenTakesReply = "takes version alone: a " + inboxDismissReplyPrefix +
		"<id> row is one user_threads row, and what the page drew in it is that row's version," +
		" a non-negative integer"
	inboxDismissSeenTakesAgent = "takes newest_reply and unread_replies: an " +
		inboxDismissAgentPrefix + "<id> row has no user_threads row and so no version, and what" +
		" the page drew in it is the newest session reply it showed and the ids it drew as unread"
)

// readInboxDismissSeen reads seen_replies onto the rows a dismissal named: what the page drew in
// each of them, which is what the writes hold the row to (inboxDismissMarkThreads and
// inboxDismissCheckAgentRoots). It is the page's answer to the question the filtered form
// answers out of the database, the server being the page there (readInboxDismissThreads).
//
// Five refusals of its own, each 400 INVALID_QUERY: a row threads names and seen_replies leaves
// out, since a dismissal with nothing to hold a row to is a dismissal nothing can check; a key
// naming a row threads does not, which is a page asking about a row it did not ask to dismiss;
// a reply: entry that is not that row's own version; an agent: entry that is not the two things
// the page drew of its thread; and an id that is not a uuid. The other two - seen_replies on a
// restore, and with the filtered form - belong to the handler, which knows the direction and the
// form (dismissInbox). Those seven are the seven cmd/dispatch/README.md lists.
func readInboxDismissSeen(rows *inboxDismissRows, seen map[string]json.RawMessage) error {
	drawn, err := inboxDismissSeenByRow(rows, seen)
	if err != nil {
		return err
	}
	rows.seenVersions = make(map[string]int64, len(rows.threads))
	for _, thread := range rows.threads {
		row := inboxDismissReplyPrefix + thread
		entry := inboxDismissSeenReply{}
		if err := readInboxDismissSeenEntry(drawn, row, inboxDismissSeenTakesReply, &entry); err != nil {
			return err
		}
		if entry.Version == nil || *entry.Version < 0 {
			return inboxDismissSeenRefused(row, inboxDismissSeenTakesReply)
		}
		rows.seenVersions[thread] = *entry.Version
	}
	rows.seenNewest = make(map[string]string, len(rows.roots))
	for _, root := range rows.roots {
		row := inboxDismissAgentPrefix + root
		entry := inboxDismissSeenAgent{}
		if err := readInboxDismissSeenEntry(drawn, row, inboxDismissSeenTakesAgent, &entry); err != nil {
			return err
		}
		if entry.NewestReply == nil || entry.UnreadReplies == nil {
			return inboxDismissSeenRefused(row, inboxDismissSeenTakesAgent)
		}
		newest, err := uuid.Parse(*entry.NewestReply)
		if err != nil {
			return inboxDismissSeenNoReply(row, "newest_reply")
		}
		rows.seenNewest[root] = newest.String()
		for _, unread := range *entry.UnreadReplies {
			parsed, err := uuid.Parse(unread)
			if err != nil {
				return inboxDismissSeenNoReply(row, "unread_replies")
			}
			rows.seenUnread = append(rows.seenUnread, parsed.String())
		}
	}
	// The check binds every root's drawn unread replies as one set (inboxDismissCheckAgentRoots),
	// so an id two rows drew is bound once.
	slices.Sort(rows.seenUnread)
	rows.seenUnread = slices.Compact(rows.seenUnread)
	return nil
}

// inboxDismissSeenByRow keys the entries by the row each is about, so the two loops above read
// every row the request named rather than whatever the body happened to key its entries by.
func inboxDismissSeenByRow(
	rows *inboxDismissRows, seen map[string]json.RawMessage,
) (map[string]json.RawMessage, error) {
	drawn := make(map[string]json.RawMessage, len(seen))
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
			return nil, errorf(http.StatusBadRequest, "INVALID_QUERY",
				"seen_replies names %s, which threads does not: it takes one entry per row"+
					" threads names", key)
		}
		drawn[row] = value
	}
	return drawn, nil
}

// readInboxDismissSeenEntry reads one row's entry into the shape its kind takes, which is what
// refuses an entry carrying the other kind's fields: they are unknown ones there. takes is what
// that kind carries, and the words a row left out or an entry of another shape is refused in.
func readInboxDismissSeenEntry(
	drawn map[string]json.RawMessage, row, takes string, entry any,
) error {
	value, carried := drawn[row]
	if !carried {
		return errorf(http.StatusBadRequest, "INVALID_QUERY",
			"seen_replies leaves %s out: a dismissal carries what the page drew in every row"+
				" threads names", row)
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(entry); err != nil {
		return inboxDismissSeenRefused(row, takes)
	}
	return nil
}

// inboxDismissSeenRefused is that refusal, and inboxDismissSeenNoReply the one an id that is not
// a uuid answers, whichever field of whichever kind carried it.
func inboxDismissSeenRefused(row, takes string) error {
	return errorf(http.StatusBadRequest, "INVALID_QUERY", "seen_replies[%s] %s", row, takes)
}

func inboxDismissSeenNoReply(row, field string) error {
	return errorf(http.StatusBadRequest, "INVALID_QUERY",
		"seen_replies[%s].%s names no reply: a reply the page drew is named by its id, a uuid",
		row, field)
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
	// Which form the body is, read once here and once in the transaction's own read
	// (readInboxDismissThreads). The two are exclusive: a request carrying both would leave it to
	// the server to decide whether the filters narrow the named rows or name rows of their own.
	named := rows.named()
	if named && (input.DismissThrough != nil || input.narrows()) {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest,
			"name threads or the list's filters with dismiss_through, not both")
		return
	}
	// seen_replies belongs to a dismissal that named its rows and to nothing else: a restore
	// holds a row to nothing, and under the list's filters the read that chooses the rows draws
	// them (readInboxDismissThreads), the server standing in for the page there.
	if input.SeenReplies != nil && input.Restore {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest,
			"a restore clears the marks and takes no seen_replies")
		return
	}
	if input.SeenReplies != nil && !named {
		writeError(w, "INVALID_QUERY", http.StatusBadRequest,
			"seen_replies goes with threads: under the list's filters the server draws the rows"+
				" itself and holds each of them to what it drew")
		return
	}
	if named && !input.Restore {
		if err := readInboxDismissSeen(&rows, input.SeenReplies); err != nil {
			s.writeHandlerError(w, err)
			return
		}
	}
	// The narrowing a filtered request matches its rows by. A request that named its rows is read
	// by those ids alone (readInboxDismissThreads), so this stands unread for it.
	scope := inboxReadScope{reply: true, agent: true, priorityNone: true, projectNone: true, urgencyNone: true}
	if !named {
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
		// Step 2 of the lock order, after the owners and before the caller's rows and every
		// write, so the check that follows reads each thread in a snapshot taken once the root is
		// this transaction's (inboxDismissLockAgentRoots). A restore takes no root: it deletes
		// marks, and a session reply that commits beside it deletes the same mark through the
		// trigger of 20261105093507, so whichever of the two runs first the thread ends in the
		// Inbox, and a `for update` held over the rest of the transaction would stop that
		// thread's replies for nothing.
		if _, err := tx.Exec(ctx, inboxDismissLockAgentRoots, read.agentRoots); err != nil {
			return 0, nil, fmt.Errorf("lock the agent threads' roots: %w", err)
		}
		// Which of those roots the dismissal may still write, read once the lock has been given
		// and before the first write (inboxDismissCheckAgentRoots). The roots it does not answer
		// are dropped here, so nothing below writes, counts or marks a reply read for them. The
		// unread replies it holds each thread to are the ones the page drew, which the read
		// above settled for either form (drawnUnread, api/inbox_read.go).
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

// readInboxDismissThreads reads both kinds of row this request changes, each list sorted so the
// locks and writes that follow are taken in one order. A request that named its rows is read by
// those ids, through the reads both write routes over these rows share (readInboxReplyRows and
// readInboxAgentRows, api/inbox_read.go); a filtered request is the mark-all's own read of the
// state it writes over (readInboxReadThreads), given this plan's two statements and its mark, so
// that read lives once.
//
// What the writes hold each row to comes from the request where it named its rows: every read
// answers each row's own version and newest reply, and what the page drew is written over them
// here rather than sent to Postgres and back. A filtered request drew nothing, so what its
// reads answered stands, the server standing in for the page. One mechanism for both forms, and
// the one place that decides which of the two this request is.
func readInboxDismissThreads(
	ctx context.Context, tx pgx.Tx, actor model.Actor, login string,
	plan inboxDismissPlan, rows inboxDismissRows, scope inboxReadScope,
) (inboxReadThreads, error) {
	if !rows.named() {
		read, err := readInboxReadThreads(ctx, tx, actor, login,
			plan.replyThreads, plan.agentThreads, plan.through, scope)
		// The unread replies the read itself drew are the set the check holds each thread to.
		// They share one backing array, which nothing writes to before the check binds it
		// (keepAgentRoots rewrites replies after it).
		read.drawnUnread = read.replies
		return read, err
	}
	read := newInboxReadThreads()
	if len(rows.threads) > 0 {
		if err := readInboxReplyRows(ctx, tx, &read, inboxDismissNamedThreads,
			login, rows.threads); err != nil {
			return read, err
		}
	}
	if len(rows.roots) > 0 {
		args := append(unreadDirectRepliesArgs(actor.ID, nil), rows.roots)
		if err := readInboxAgentRows(ctx, tx, &read, plan.agentByID, args); err != nil {
			return read, err
		}
	}
	// What the page drew, over what the reads answered. A restore is held to nothing and drew
	// nothing, so all three are empty for one and the reads' own answers stand unread.
	maps.Copy(read.replyVersions, rows.seenVersions)
	maps.Copy(read.rootVersions, rows.seenNewest)
	read.drawnUnread = rows.seenUnread
	return read, nil
}

// checkInboxDismissAgentRoots asks which of the Agent threads this dismissal read it may still
// write a mark for (inboxDismissCheckAgentRoots) and drops the rest, so what follows writes,
// counts and marks read only the threads that still hold what the page drew: the newest reply
// of each root and the unread replies drawn in all of them, both as the read holds them
// (readInboxDismissThreads).
//
// The marks it reads a reply against are unreadDirectRepliesCTE's own, so its first two
// parameters are that fragment's first two, built by unreadDirectRepliesArgs rather than spelled
// here. It narrows to no session, so it takes none of that fragment's third and numbers its own
// from there.
func checkInboxDismissAgentRoots(
	ctx context.Context, tx pgx.Tx, actor model.Actor, read *inboxReadThreads,
) error {
	marks := unreadDirectRepliesArgs(actor.ID, nil)
	newest := inboxDismissSeenOf(read.rootVersions, read.agentRoots)
	drawn := read.drawnUnread
	if drawn == nil {
		// A page that drew no unread reply in any of these rows is still a page that drew them:
		// pgx binds a nil slice as null, and every root is contained by an empty set but none by
		// a null one.
		drawn = []string{}
	}
	args := append(marks[:2:2], read.agentRoots, newest, drawn)
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
	if len(read.replies) > 0 {
		// Dismissing marks the thread read, so its unread replies are written by id exactly as the
		// mark-all writes them, and the dead rows the sessions' own marks cover are pruned with
		// them. A restore arrives here with no reply at all: it moves no read mark, so the reads
		// that chose its roots select no reply ids (inboxDismissNoReplies).
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
