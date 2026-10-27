package outbox

import (
	"context"
	"fmt"

	"github.com/google/uuid"
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

// loadCommentRecipients reads, in one query for a whole batch, the sessions a person's reply under
// each comment of commentIDs reaches on their own topics, keyed by that comment's id. They are
// every session that wrote a comment in the comment's thread (its root first, then each reply in
// the order it was written), then every session the thread's document credits (documentCreditsSQL;
// none when the thread is on no document), each once. People are left out, since a person has no
// Envoy topic. A comment that is not a uuid, or that has no row, has no entry. The actor and the
// sessions the reply's mentions reach are skipped by the caller (publishAuthorRoutes).
func loadCommentRecipients(ctx context.Context, deps Deps, commentIDs []string) (map[string][]string, error) {
	ids := make([]string, 0, len(commentIDs))
	for _, commentID := range commentIDs {
		parsed, err := uuid.Parse(commentID)
		if err != nil {
			continue
		}
		ids = append(ids, parsed.String())
	}
	if len(ids) == 0 {
		return map[string][]string{}, nil
	}
	rows, err := deps.Store.Pool.Query(ctx, commentRecipientsQuery, ids, maxCommentThreadDepth)
	if err != nil {
		return nil, fmt.Errorf("load comment recipients: %w", err)
	}
	recipients := map[string][]string{}
	var key, sessionID string
	if _, err := pgx.ForEachRow(rows, []any{&key, &sessionID}, func() error {
		recipients[key] = append(recipients[key], sessionID)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("load comment recipients: %w", err)
	}
	return recipients, nil
}

// threadReplyOf names the comment a person's reply event replies under, the key of its
// loadCommentRecipients entry, or "" when the event takes no thread recipients: it is no
// comment.created or comment.edited, it does not notify (only a person's comment does,
// events.Broker.Notify), it is a new thread rather than a reply, or it is in an ask's thread, whose
// sessions are the ask's followers (publishFollowerRoutes).
func threadReplyOf(event model.Event) string {
	if !event.Notify || (event.Type != "comment.created" && event.Type != "comment.edited") {
		return ""
	}
	if payloadString(event.Payload, "ask_id") != "" {
		return ""
	}
	return payloadString(event.Payload, "reply_to")
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
func loadPrimaryDocumentAuthors(ctx context.Context, deps Deps, issueKeys []string) (map[string][]string, error) {
	if len(issueKeys) == 0 {
		return map[string][]string{}, nil
	}
	rows, err := deps.Store.Pool.Query(ctx, primaryDocumentAuthorsQuery, issueKeys)
	if err != nil {
		return nil, fmt.Errorf("load primary document authors: %w", err)
	}
	authors := map[string][]string{}
	var key, sessionID string
	if _, err := pgx.ForEachRow(rows, []any{&key, &sessionID}, func() error {
		authors[key] = append(authors[key], sessionID)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("load primary document authors: %w", err)
	}
	return authors, nil
}

// resolveCommentRecipients reads, for a whole batch, the sessions a person's comment events reach
// beyond their owner topic and route: the thread recipients of every reply event in one query
// (loadCommentRecipients), for publishAuthorRoutes, and the primary document's sessions of every
// event that reaches them in the claimant's place in one more (loadPrimaryDocumentAuthors), for
// publishClaimant. Each is carried on its event. An event already failed is skipped. When a query
// fails, each event of the batch that needed it fails with it and is backed off, as an event whose
// publish failed is, and the batch's other events go on.
func resolveCommentRecipients(ctx context.Context, deps Deps, pending []pendingEvent) {
	resolveThreadSessions(ctx, deps, pending)
	resolvePrimaryAuthors(ctx, deps, pending)
}

// resolveThreadSessions reads the thread recipients of every reply event of a batch in one query
// (loadCommentRecipients) and carries each on its event as threadSessions.
func resolveThreadSessions(ctx context.Context, deps Deps, pending []pendingEvent) {
	// A reply's key is its comment's canonical uuid, or "" when it is not a uuid: such a reply
	// has no entry, but still fails with the query.
	type reply struct {
		index int
		key   string
	}
	var replies []reply
	var commentIDs []string
	for index, item := range pending {
		if item.failure != nil {
			continue
		}
		commentID := threadReplyOf(item.event)
		if commentID == "" {
			continue
		}
		key := ""
		if parsed, err := uuid.Parse(commentID); err == nil {
			key = parsed.String()
			commentIDs = append(commentIDs, key)
		}
		replies = append(replies, reply{index: index, key: key})
	}
	if len(replies) == 0 {
		return
	}
	recipients, err := loadCommentRecipients(ctx, deps, commentIDs)
	for _, r := range replies {
		item := &pending[r.index]
		if err != nil {
			item.failure = err
			item.failureLog = "dispatch outbox: load comment recipients"
			continue
		}
		if r.key != "" {
			item.threadSessions = recipients[r.key]
		}
	}
}

// primaryAuthorsIssueOf names the issue whose primary document's sessions a person's comment event
// reaches in the claimant's place (publishClaimant), the key of its loadPrimaryDocumentAuthors
// entry, or "" when the event takes no such recipients: it is no comment.created or
// comment.edited, it does not notify (only a person's comment does, events.Broker.Notify), it has
// no issue (a project document's comment), or its issue has a route or a session's claim, whose
// holder hears it in place of the document's sessions. The comment's thread does not matter.
func primaryAuthorsIssueOf(item pendingEvent) string {
	event := item.event
	if !event.Notify || (event.Type != "comment.created" && event.Type != "comment.edited") {
		return ""
	}
	if item.claimant != "" || (item.route != nil && *item.route != "") {
		return ""
	}
	return item.issue
}

// resolvePrimaryAuthors reads the primary document's sessions of every event of a batch that
// takes them (primaryAuthorsIssueOf) in one query (loadPrimaryDocumentAuthors) and carries each on
// its event as primaryAuthors.
func resolvePrimaryAuthors(ctx context.Context, deps Deps, pending []pendingEvent) {
	var indexes []int
	var issueKeys []string
	for index, item := range pending {
		if item.failure != nil {
			continue
		}
		if issueKey := primaryAuthorsIssueOf(item); issueKey != "" {
			indexes = append(indexes, index)
			issueKeys = append(issueKeys, issueKey)
		}
	}
	if len(indexes) == 0 {
		return
	}
	authors, err := loadPrimaryDocumentAuthors(ctx, deps, issueKeys)
	for position, index := range indexes {
		item := &pending[index]
		if err != nil {
			item.failure = err
			item.failureLog = "dispatch outbox: load primary document authors"
			continue
		}
		item.primaryAuthors = authors[issueKeys[position]]
	}
}
