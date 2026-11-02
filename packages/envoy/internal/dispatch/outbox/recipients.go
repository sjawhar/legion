package outbox

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// documentCreditsSQL is the body of a CTE that lists, for each row of a relation
// documents(key, artifact_id) in scope, every actor that document credits: the one that created it
// (artifacts.created_by) and every one a version lists (artifact_versions.authors). Each row has
// the key, the author's actor JSON and a rank: the creator first, then the versions in number
// order, and each version's authors in its list's order. An artifact that is not a document
// credits no one. A version's authors are a JSON array, or JSON null where a writer encoded a nil
// list, which every Go reader of the column decodes as no authors; the CTE reads null the same way
// rather than failing on jsonb_array_elements' refusal of a scalar. loadDocumentSessionAuthors,
// loadCommentRecipients and loadPrimaryDocumentAuthors all read it, so a document credits the same
// sessions for each.
const documentCreditsSQL = `
	select d.key, a.created_by as author, array[1, 0, 0]::bigint[] as rank
	from documents d
	join artifacts a on a.id = d.artifact_id and a.kind = 'doc'
	union all
	select d.key, credit.author, array[1, v.number, credit.position]::bigint[]
	from documents d
	join artifacts a on a.id = d.artifact_id and a.kind = 'doc'
	join artifact_versions v on v.artifact_id = a.id
	cross join lateral jsonb_array_elements(nullif(v.authors, 'null'::jsonb))
		with ordinality as credit(author, position)
`

// sessionCreditSQL keeps the credits that name a session by its id. A person has no Envoy topic,
// so people are left out, and so is a session credit with no id, which has no topic either.
const sessionCreditSQL = `author->>'kind' = 'session' and coalesce(author->>'id', '') <> ''`

// commentRecipientsQuery is loadCommentRecipients' one statement. $1 is the comment ids, $2
// maxCommentThreadDepth. The walk up (up) finds each comment's thread root. comments.reply_to has
// no acyclicity constraint, so the walk unions on the visited row and stops at the depth bound: a
// cycle then has no reply_to-is-null row and gives no root, rather than spinning. The walk down
// (thread) reads every comment under that root, to the same depth, for legacy nested rows. The
// thread's members are both walks' comments, so a cycle still reaches the authors of the comments
// the walk up visited, the replied-to comment's among them. The thread's document is the root's
// anchor's artifact, or the root's own artifact for a comment on a whole project artifact, since
// a reply carries no anchor.
const commentRecipientsQuery = `
	with recursive
	up as (
		select c.id as key, c.id, c.reply_to, c.anchor, c.artifact_id, 0 as depth
		from comments c
		where c.id = any($1::uuid[])
		union
		select u.key, c.id, c.reply_to, c.anchor, c.artifact_id, u.depth + 1
		from up u
		join comments c on c.id = u.reply_to
		where u.depth < $2
	),
	roots as (
		select key, id, coalesce((anchor->>'artifact_id')::uuid, artifact_id) as artifact_id
		from up
		where reply_to is null
	),
	thread as (
		select key, id, 0 as depth from roots
		union
		select t.key, c.id, t.depth + 1
		from thread t
		join comments c on c.reply_to = t.id
		where t.depth < $2
	),
	members as (
		select key, id from up
		union
		select key, id from thread
	),
	documents as (
		select key, artifact_id from roots where artifact_id is not null
	),
	credited as (
		select m.key, c.author,
			array[0, row_number() over (partition by m.key order by c.reply_to is null desc, c.created_at, c.id), 0]::bigint[] as rank
		from members m
		join comments c on c.id = m.id
		union all
		` + documentCreditsSQL + `
	)
	select key::text, author->>'id'
	from credited
	where ` + sessionCreditSQL + `
	group by key, author->>'id'
	order by key, min(rank)
`

// loadCommentRecipients reads, in one query for a whole batch (or for half of a refused one's ids,
// when loadRecipients runs it again in halves), the sessions a person's reply under each comment
// of commentIDs reaches on their own topics, keyed by that comment's id. commentIDs are canonical
// uuids (validIDs): a reply_to that is no uuid names no comment and is never read. They are every
// session that wrote a comment in the comment's thread (its root first, then each reply in the
// order it was written), then every session the thread's document credits (documentCreditsSQL;
// none when the thread is on no document), each once. People are left out, since a person has no
// Envoy topic. A comment that has no row has no entry. The actor and the sessions the reply's
// mentions reach are skipped by the caller (addAuthorRoutes).
func loadCommentRecipients(ctx context.Context, db queryer, commentIDs []string) (map[string][]string, error) {
	if len(commentIDs) == 0 {
		return map[string][]string{}, nil
	}
	return loadKeyedSessions(ctx, db, "load comment recipients", commentRecipientsQuery, commentIDs, maxCommentThreadDepth)
}

// primaryDocumentAuthorsQuery is loadPrimaryDocumentAuthors' one statement. $1 is the issue keys.
// The documents are the issues' primary artifacts (artifacts.is_primary, one per issue at most).
const primaryDocumentAuthorsQuery = `
	with documents as (
		select issue_key as key, id as artifact_id
		from artifacts
		where issue_key = any($1::text[]) and is_primary
	),
	credited as (` + documentCreditsSQL + `)
	select key, author->>'id'
	from credited
	where ` + sessionCreditSQL + `
	group by key, author->>'id'
	order by key, min(rank)
`

// loadPrimaryDocumentAuthors reads, in one query for a whole batch, the sessions the primary
// document of each issue of issueKeys credits (documentCreditsSQL), keyed by the issue's key: its
// creator first, then each version's authors, each once. People, and session credits with no id,
// are left out (sessionCreditSQL). An issue with no primary document, or with no row, has no entry.
func loadPrimaryDocumentAuthors(ctx context.Context, db queryer, issueKeys []string) (map[string][]string, error) {
	if len(issueKeys) == 0 {
		return map[string][]string{}, nil
	}
	return loadKeyedSessions(ctx, db, "load primary document authors", primaryDocumentAuthorsQuery, issueKeys)
}

// loadKeyedSessions runs query, one statement for a whole batch whose every row is a key and a
// session id, and returns each key's sessions in the order of its rows. what names the load in its
// error.
func loadKeyedSessions(ctx context.Context, db queryer, what, query string, args ...any) (map[string][]string, error) {
	rows, err := db.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	sessions := map[string][]string{}
	var key, sessionID string
	if _, err := pgx.ForEachRow(rows, []any{&key, &sessionID}, func() error {
		sessions[key] = append(sessions[key], sessionID)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return sessions, nil
}

// takesPrimaryAuthors reports whether a person's comment event reaches the sessions of its issue's
// primary document (addPrimaryDocumentAuthors): it is a comment.created or comment.edited, it
// notifies (only a person's comment does, events.Broker.Notify), it names an issue, and that issue
// has no route and no session claimant (routing), either of which hears it in their place. The
// comment's thread does not matter.
func takesPrimaryAuthors(event model.Event, routing routing) bool {
	if !event.Notify || (event.Type != "comment.created" && event.Type != "comment.edited") {
		return false
	}
	if event.IssueKey == nil || routing.claimant != "" {
		return false
	}
	return routing.route == nil || *routing.route == ""
}

// recipientFilter keeps, of the sessions one route of a notifying event reaches on their own
// topics, each one once, and none that is the event's actor. A session the comment's mentions
// resolved to (payloadMentionedSessions, which the claimant and follower routes read too) is left
// to the mention by the rule every session route of a new comment applies
// (destinations.leftToMention): skipped once its mention delivered, held back while the send is
// undecided, and kept when it failed. addAuthorRoutes and addPrimaryDocumentAuthors both filter
// through it.
type recipientFilter struct {
	actor        model.Actor
	suppressed   map[string]struct{}
	seen         map[string]bool
	destinations *destinations
}

func newRecipientFilter(event model.Event, d *destinations) *recipientFilter {
	return &recipientFilter{
		actor:        event.Actor,
		suppressed:   payloadMentionedSessions(event.Payload),
		seen:         map[string]bool{},
		destinations: d,
	}
}

// keep reports whether sessionID is a recipient, the first time the filter is asked about it.
func (f *recipientFilter) keep(sessionID string) bool {
	if f.seen[sessionID] || f.actor.SameAs(model.Actor{Kind: "session", ID: sessionID}) {
		return false
	}
	f.seen[sessionID] = true
	_, suppressed := f.suppressed[sessionID]
	return !suppressed || !f.destinations.leftToMention(sessionID)
}
