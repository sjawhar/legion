package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// lookup answers the reads an event's destinations depend on: an ask's followers in the order they
// joined, a comment's thread root's author, a message's author, the sessions a document credits,
// the sessions a person's reply under a comment reaches (loadCommentRecipients), the sessions an
// issue's primary document credits (loadPrimaryDocumentAuthors), and what a new comment's mentions
// delivered. A comment or message that does not exist answers no author, found false.
type lookup interface {
	followers(askID string) ([]string, error)
	rootCommentAuthor(commentID string) (model.Actor, bool, error)
	messageAuthor(messageID string) (model.Actor, bool, error)
	documentAuthors(artifactID string) ([]string, error)
	commentRecipients(commentID string) ([]string, error)
	primaryDocumentAuthors(issueKey string) ([]string, error)
	mentionDeliveries(commentID string) (mentionDeliveries, error)
}

// mentionDeliveries is what a new comment's mentions did for the sessions they resolved to, read
// from comment_deliveries when the event is published (loadMentionDeliveries). A mention resolved
// at write time names only a candidate: its send runs after the comment commits and can fail - the
// listener does not know the session, the receipt times out, the handler is cancelled, the session
// lacks the delivery mode - and a session whose mention failed has no copy but the outbox's.
type mentionDeliveries struct {
	// sent holds each session some attempt of a mention reached.
	sent map[string]struct{}
	// pending holds each session no attempt reached yet whose send is still live: an attempt is
	// pending and its claim, or its creation when no sender has claimed it yet, is younger than
	// model.DeliveryClaimLapse.
	pending map[string]struct{}
}

// wanted is the lookup a batch's first pass over its events runs: it answers nothing and records
// every id asked for, so that the batch reads each kind once (loadLookups). An event's destinations
// ask for the same ids whatever the answers are, so a second pass over resolved lookups asks for
// exactly these. A not-due event asks through untaken, after every due event has asked, so its
// recipient reads are the same taken or untaken ones in both passes (resolveDestinations).
type wanted struct {
	asks, comments, messages, documents, threads, issues, mentions map[string]struct{}
}

func newWanted() *wanted {
	return &wanted{
		asks: map[string]struct{}{}, comments: map[string]struct{}{},
		messages: map[string]struct{}{}, documents: map[string]struct{}{},
		threads: map[string]struct{}{}, issues: map[string]struct{}{}, mentions: map[string]struct{}{},
	}
}

func (w *wanted) followers(askID string) ([]string, error) {
	w.asks[askID] = struct{}{}
	return nil, nil
}

func (w *wanted) rootCommentAuthor(commentID string) (model.Actor, bool, error) {
	w.comments[commentID] = struct{}{}
	return model.Actor{}, false, nil
}

func (w *wanted) messageAuthor(messageID string) (model.Actor, bool, error) {
	w.messages[messageID] = struct{}{}
	return model.Actor{}, false, nil
}

func (w *wanted) documentAuthors(artifactID string) ([]string, error) {
	w.documents[artifactID] = struct{}{}
	return nil, nil
}

func (w *wanted) commentRecipients(commentID string) ([]string, error) {
	w.threads[commentID] = struct{}{}
	return nil, nil
}

func (w *wanted) primaryDocumentAuthors(issueKey string) ([]string, error) {
	w.issues[issueKey] = struct{}{}
	return nil, nil
}

func (w *wanted) mentionDeliveries(commentID string) (mentionDeliveries, error) {
	w.mentions[commentID] = struct{}{}
	return mentionDeliveries{}, nil
}

// unansweredRecipients is a recipient read - the sessions a reply under a comment reaches, the
// sessions an issue's primary document credits, the sessions a document credits - that an event's
// destinations needed and the batch has no answer for: the read of its id failed
// (loadRecipients), or the event is not due and the batch did not take it (untaken). The event's
// list ends there (eventDestinations), so it names none of the sessions the read would have added,
// and planChains holds its owner's later events back from every session the list does not name
// (batchEvent.incomplete).
type unansweredRecipients struct{ err error }

func (e *unansweredRecipients) Error() string { return e.err.Error() }

func (e *unansweredRecipients) Unwrap() error { return e.err }

// errNotDue is what untaken answers a recipient read the batch did not take.
var errNotDue = errors.New("the batch reads no recipients for an event that is not due")

// untaken is the lookup a not-due event's destinations read (resolveDestinations). The batch takes
// no recipient read for an event it does not send: readBatch reads an owner's whole window, so the
// read of an event backed off on a row no read can take - a credit list jsonb_array_elements
// refuses - would otherwise cost every batch a statement that fails and a run of each of its ids
// alone (loadRecipients). Its comment recipients, primary document authors and document credits
// answer from lookup only for an id a due event of the batch asked for (taken), and otherwise
// unanswered (errNotDue) - except a reply_to that is no uuid, which no read takes for any event
// (validIDs) and which answers from lookup, with no session. Every other read answers from lookup.
type untaken struct {
	lookup
	taken *wanted
}

func (u untaken) documentAuthors(artifactID string) ([]string, error) {
	if _, ok := u.taken.documents[artifactID]; !ok {
		return nil, &unansweredRecipients{err: errNotDue}
	}
	return u.lookup.documentAuthors(artifactID)
}

func (u untaken) commentRecipients(commentID string) ([]string, error) {
	_, taken := u.taken.threads[commentID]
	if _, err := uuid.Parse(commentID); err == nil && !taken {
		return nil, &unansweredRecipients{err: errNotDue}
	}
	return u.lookup.commentRecipients(commentID)
}

func (u untaken) primaryDocumentAuthors(issueKey string) ([]string, error) {
	if _, ok := u.taken.issues[issueKey]; !ok {
		return nil, &unansweredRecipients{err: errNotDue}
	}
	return u.lookup.primaryDocumentAuthors(issueKey)
}

// lookups is what a batch read for the ids its events asked for (loadLookups). Each map of a uuid
// kind is keyed by the id's canonical form (canonicalID), as every read keys its rows, and
// primaries by the issue's key. failed holds each id no read answers - an id that is no uuid,
// which the column it is read from refuses (validIDs; a thread's is not failed but has no entry),
// or an id whose recipient read failed (loadRecipients) - keyed by its kind and that same key, with
// the error the event's destinations then report.
type lookups struct {
	asks      map[string][]string
	roots     map[string]json.RawMessage
	messages  map[string]json.RawMessage
	documents map[string][]string
	threads   map[string][]string
	primaries map[string][]string
	mentions  map[string]mentionDeliveries
	failed    map[string]error
}

func (l *lookups) followers(askID string) ([]string, error) {
	id := canonicalID(askID)
	if err := l.failed["ask:"+id]; err != nil {
		return nil, err
	}
	return l.asks[id], nil
}

func (l *lookups) rootCommentAuthor(commentID string) (model.Actor, bool, error) {
	id := canonicalID(commentID)
	if err := l.failed["comment:"+id]; err != nil {
		return model.Actor{}, false, err
	}
	root, ok := l.roots[id]
	if !ok {
		return model.Actor{}, false, nil
	}
	return decodeAuthor(root, "comment", commentID)
}

func (l *lookups) messageAuthor(messageID string) (model.Actor, bool, error) {
	id := canonicalID(messageID)
	if err := l.failed["message:"+id]; err != nil {
		return model.Actor{}, false, err
	}
	author, ok := l.messages[id]
	if !ok {
		return model.Actor{}, false, nil
	}
	return decodeAuthor(author, "message", messageID)
}

func (l *lookups) documentAuthors(artifactID string) ([]string, error) {
	id := canonicalID(artifactID)
	if err := l.failed["document:"+id]; err != nil {
		return nil, err
	}
	return l.documents[id], nil
}

func (l *lookups) commentRecipients(commentID string) ([]string, error) {
	id := canonicalID(commentID)
	if err := l.failed["thread:"+id]; err != nil {
		return nil, err
	}
	return l.threads[id], nil
}

func (l *lookups) primaryDocumentAuthors(issueKey string) ([]string, error) {
	if err := l.failed["issue:"+issueKey]; err != nil {
		return nil, err
	}
	return l.primaries[issueKey], nil
}

func (l *lookups) mentionDeliveries(commentID string) (mentionDeliveries, error) {
	id := canonicalID(commentID)
	if err := l.failed["mention:"+id]; err != nil {
		return mentionDeliveries{}, err
	}
	return l.mentions[id], nil
}

func decodeAuthor(raw []byte, kind, id string) (model.Actor, bool, error) {
	var author model.Actor
	if err := json.Unmarshal(raw, &author); err != nil {
		return model.Actor{}, false, fmt.Errorf("decode %s author %q: %w", kind, id, err)
	}
	return author, true, nil
}

// queryer is the read surface loadLookups needs of the outbox's pool.
type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// loadLookups reads what want asks for, one statement per kind that asks anything, so a batch costs
// at most seven reads however many events it holds: ask followers, thread root authors, message
// authors, document credits (loadDocumentSessionAuthors), mention deliveries, comment recipients
// (loadCommentRecipients) and primary document authors (loadPrimaryDocumentAuthors). Each read is
// the per-id read the outbox made before, over a set of ids: the same rows, in the same order for
// each id. Every kind but the issues' is a uuid, read and answered in its canonical form
// (validIDs, canonicalID); an id that is no uuid is never sent to Postgres, which would refuse the
// whole statement over it, and is recorded failed, as the per-id read failed on it before - but a
// reply's reply_to, which then names no thread, has no recipients and fails nothing.
//
// A read of ask followers, thread root authors, message authors or mention deliveries that fails
// fails the batch, which is then left unrecorded and read again. A recipient read - document
// credits, comment recipients or primary document authors - that Postgres refuses is run again for
// each of its ids alone (loadRecipients), and an id whose own run fails fails only the events that
// asked it: each such event's destinations end at the step that needed it, after its owner topic,
// route and claimant (eventDestinations), the event's owner's later events are held back from
// every session its list does not name (planChains), and the batch's other events, those that
// asked the same statement for other ids among them, are untouched. So a row no recipient read
// can take - a credit list jsonb_array_elements refuses - backs off the events that read it and
// holds back their owners' later session copies, and nothing else. A recipient read that fails
// for the stop or a lost connection fails every event that asked it, as each run alone would.
func loadLookups(ctx context.Context, db queryer, want *wanted) (*lookups, error) {
	found := &lookups{
		asks: map[string][]string{}, roots: map[string]json.RawMessage{},
		messages: map[string]json.RawMessage{}, documents: map[string][]string{},
		threads: map[string][]string{}, primaries: map[string][]string{},
		mentions: map[string]mentionDeliveries{}, failed: map[string]error{},
	}
	asks := validIDs(want.asks, "ask", "load followers of ask %q", found.failed)
	comments := validIDs(want.comments, "comment", "load root comment author %q", found.failed)
	messages := validIDs(want.messages, "message", "load message author %q", found.failed)
	documents := validIDs(want.documents, "document", "load document %q session authors", found.failed)
	mentions := validIDs(want.mentions, "mention", "load mention deliveries of comment %q", found.failed)
	threads := validIDs(want.threads, "thread", "", found.failed)
	issues := slices.Sorted(maps.Keys(want.issues))
	if err := loadFollowers(ctx, db, asks, found); err != nil {
		return nil, err
	}
	if err := loadRootCommentAuthors(ctx, db, comments, found); err != nil {
		return nil, err
	}
	if err := loadMessageAuthors(ctx, db, messages, found); err != nil {
		return nil, err
	}
	loadRecipients(ctx, db, loadDocumentSessionAuthors, documents, "document", found.documents, found.failed)
	if err := loadMentionDeliveries(ctx, db, mentions, found); err != nil {
		return nil, err
	}
	loadRecipients(ctx, db, loadCommentRecipients, threads, "thread", found.threads, found.failed)
	loadRecipients(ctx, db, loadPrimaryDocumentAuthors, issues, "issue", found.primaries, found.failed)
	return found, nil
}

// validIDs returns the canonical form (canonicalID) of every id of set that is a uuid, sorted and
// each once. Every other id is recorded in failed under kind, with the error format names it by -
// except a thread's, the reply_to of a reply: one that is no uuid replies under no comment, so its
// thread reaches no session, and it is neither read nor failed (commentRecipients answers it no
// session and no error, and the reply holds nothing back). format is unused for a thread's.
func validIDs(set map[string]struct{}, kind, format string, failed map[string]error) []string {
	canonical := map[string]struct{}{}
	for id := range set {
		parsed, err := uuid.Parse(id)
		if err != nil {
			if kind != "thread" {
				failed[kind+":"+id] = fmt.Errorf(format+": not a uuid: %w", id, err)
			}
			continue
		}
		canonical[parsed.String()] = struct{}{}
	}
	return slices.Sorted(maps.Keys(canonical))
}

// canonicalID is the key a lookup of a uuid kind answers id under: the canonical text of a uuid,
// which is what uuid::text writes and so what every read keys its rows by, or id itself when it is
// no uuid, which validIDs records failed under, or, for a thread, leaves with no entry.
func canonicalID(id string) string {
	if parsed, err := uuid.Parse(id); err == nil {
		return parsed.String()
	}
	return id
}

// recipientRead is a recipient read - loadDocumentSessionAuthors, loadCommentRecipients or
// loadPrimaryDocumentAuthors: one statement over ids, answering each id's sessions keyed by it.
type recipientRead func(ctx context.Context, db queryer, ids []string) (map[string][]string, error)

// loadRecipients runs read over ids in one statement and adds each id's sessions to into. A
// statement Postgres refuses (refusedStatement) - over a row of one id no read can take, a credit
// list jsonb_array_elements refuses - names no id, so it is run again once for each id alone: an
// id whose own run answers gets its sessions, and one whose own run Postgres refuses is recorded
// failed (failRecipients) with that run's error. So one id's row fails only the events that asked
// that id, whichever owners asked the statement's other ids. A statement that fails otherwise -
// ctx ended, or the connection was lost or never made - would fail every run alike, so every id it
// read is recorded failed with its error, which releases the events when it is the stop
// (batchEvent.resolveLost); a run of one id that fails so records its id and every id not yet run.
func loadRecipients(ctx context.Context, db queryer, read recipientRead, ids []string, kind string, into map[string][]string, failed map[string]error) {
	sessions, err := read(ctx, db, ids)
	if err == nil {
		maps.Copy(into, sessions)
		return
	}
	if len(ids) == 1 || !refusedStatement(ctx, err) {
		failRecipients(ids, kind, err, failed)
		return
	}
	for i := range ids {
		sessions, err := read(ctx, db, ids[i:i+1])
		switch {
		case err == nil:
			maps.Copy(into, sessions)
		case refusedStatement(ctx, err):
			failRecipients(ids[i:i+1], kind, err, failed)
		default:
			failRecipients(ids[i:], kind, err, failed)
			return
		}
	}
}

// refusedStatement reports whether err is Postgres refusing a statement it ran (a
// *pgconn.PgError) while ctx is still live: a failure of the rows the statement read, which a run
// over fewer ids may not meet. ctx ending, and a connection lost or never made, are not.
func refusedStatement(ctx context.Context, err error) bool {
	var refused *pgconn.PgError
	return ctx.Err() == nil && errors.As(err, &refused)
}

// failRecipients records err, a recipient read of ids that failed - document credits, comment
// recipients or primary document authors - against every id of ids under kind, as unanswered
// recipients: every event that asked one of those ids carries the failure, and no other event
// does.
func failRecipients(ids []string, kind string, err error, failed map[string]error) {
	unanswered := &unansweredRecipients{err: err}
	for _, id := range ids {
		failed[kind+":"+id] = unanswered
	}
}

// loadFollowers reads the followers of every ask in ids, each ask's in the order they joined (the
// order asks.Followers reads them in).
func loadFollowers(ctx context.Context, db queryer, ids []string, found *lookups) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `
		select ask_id::text, session_id from ask_followers
		where ask_id = any($1::text[]::uuid[])
		order by ask_id, since, session_id
	`, ids)
	if err != nil {
		return fmt.Errorf("load the batch's ask followers: %w", err)
	}
	var askID, sessionID string
	_, err = pgx.ForEachRow(rows, []any{&askID, &sessionID}, func() error {
		found.asks[askID] = append(found.asks[askID], sessionID)
		return nil
	})
	if err != nil {
		return fmt.Errorf("load the batch's ask followers: %w", err)
	}
	return nil
}

// loadRootCommentAuthors reads, for every comment in ids, its thread root's author: the walk up its
// reply_to chain to the comment humans reply under. comments.reply_to has no acyclicity
// constraint, so the walk unions on the visited row and stops at maxCommentThreadDepth: a cycle
// then finds no reply_to-is-null row and reports no root rather than spinning.
func loadRootCommentAuthors(ctx context.Context, db queryer, ids []string, found *lookups) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `
		with recursive chain as (
			select id as start, id, reply_to, author, 0 as depth
			from comments where id = any($1::text[]::uuid[])
			union
			select h.start, c.id, c.reply_to, c.author, h.depth + 1
			from comments c join chain h on c.id = h.reply_to
			where h.depth < $2
		)
		select start::text, author
		from chain
		where reply_to is null
		order by start, depth
	`, ids, maxCommentThreadDepth)
	if err != nil {
		return fmt.Errorf("load the batch's root comment authors: %w", err)
	}
	var start string
	var author []byte
	_, err = pgx.ForEachRow(rows, []any{&start, &author}, func() error {
		if _, ok := found.roots[start]; !ok {
			found.roots[start] = json.RawMessage(slices.Clone(author))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("load the batch's root comment authors: %w", err)
	}
	return nil
}

// loadMessageAuthors reads the author of every message in ids.
func loadMessageAuthors(ctx context.Context, db queryer, ids []string, found *lookups) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `select id::text, author from messages where id = any($1::text[]::uuid[])`, ids)
	if err != nil {
		return fmt.Errorf("load the batch's message authors: %w", err)
	}
	var id string
	var author []byte
	_, err = pgx.ForEachRow(rows, []any{&id, &author}, func() error {
		found.messages[id] = json.RawMessage(slices.Clone(author))
		return nil
	})
	if err != nil {
		return fmt.Errorf("load the batch's message authors: %w", err)
	}
	return nil
}

// documentSessionAuthorsQuery is loadDocumentSessionAuthors' one statement. $1 is the artifact ids.
const documentSessionAuthorsQuery = `
	with documents as (
		select id as key, id as artifact_id
		from unnest($1::text[]::uuid[]) as id
	),
	credited as (` + documentCreditsSQL + `)
	select key::text, author->>'id'
	from credited
	where ` + sessionCreditSQL + `
	group by key, author->>'id'
	order by key, min(rank)
`

// loadDocumentSessionAuthors lists, for every artifact in ids that is a document, the sessions it
// credits (documentCreditsSQL), keyed by the artifact's id: the one that created it and every one
// a version lists, each once and in the order the document first credits it, its creator first.
// People, and session credits with no id, are left out (sessionCreditSQL). An artifact that is no
// document, or has no row, has no entry.
func loadDocumentSessionAuthors(ctx context.Context, db queryer, ids []string) (map[string][]string, error) {
	if len(ids) == 0 {
		return map[string][]string{}, nil
	}
	return loadKeyedSessions(ctx, db, "load document session authors", documentSessionAuthorsQuery, ids)
}

// mentionDeliveryQuery groups every comment's mention attempts by the comment and the session each
// was sent to. An attempt pending for longer than model.DeliveryClaimLapse - by its claim, or by
// its creation when no sender ever claimed it - is abandoned, the same lease the API's claimLapsed
// gives a sender (internal/dispatch/api), so it no longer holds the outbox's copy back.
var mentionDeliveryQuery = `
	select comment_id::text, session_id,
	       bool_or(state = 'sent'),
	       bool_or(state = 'pending' and coalesce(claimed_at, created_at) >= now() - ` + model.DeliveryClaimLapseInterval + `)
	from comment_deliveries
	where comment_id = any($1::text[]::uuid[]) and session_id is not null
	group by comment_id, session_id
`

// loadMentionDeliveries reads what the mentions of every comment in ids delivered
// (mentionDeliveries). Only a comment.created whose mentions resolved to a session asks
// (eventDestinations): every other event, edits, resolutions and reopenings included, suppresses
// nothing. A comment no attempt was recorded for answers empty sets, so it suppresses nothing
// either.
func loadMentionDeliveries(ctx context.Context, db queryer, ids []string, found *lookups) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, mentionDeliveryQuery, ids)
	if err != nil {
		return fmt.Errorf("load the batch's mention deliveries: %w", err)
	}
	var commentID, sessionID string
	var sent, pending bool
	_, err = pgx.ForEachRow(rows, []any{&commentID, &sessionID, &sent, &pending}, func() error {
		deliveries, ok := found.mentions[commentID]
		if !ok {
			deliveries = mentionDeliveries{sent: map[string]struct{}{}, pending: map[string]struct{}{}}
			found.mentions[commentID] = deliveries
		}
		switch {
		case sent:
			deliveries.sent[sessionID] = struct{}{}
		case pending:
			deliveries.pending[sessionID] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("load the batch's mention deliveries: %w", err)
	}
	return nil
}
