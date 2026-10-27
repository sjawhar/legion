package outbox

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// lookup answers the reads an event's destinations depend on: an ask's followers in the order they
// joined, a comment's author and its thread root's author, a message's author, and the sessions a
// document credits. A comment or message that does not exist answers no author, found false.
type lookup interface {
	followers(askID string) ([]string, error)
	commentAuthor(commentID string) (model.Actor, bool, error)
	rootCommentAuthor(commentID string) (model.Actor, bool, error)
	messageAuthor(messageID string) (model.Actor, bool, error)
	documentAuthors(artifactID string) ([]string, error)
}

// wanted is the lookup a batch's first pass over its events runs: it answers nothing and records
// every id asked for, so that the batch reads each kind once (loadLookups). An event's destinations
// ask for the same ids whatever the answers are, so a second pass over resolved lookups asks for
// exactly these.
type wanted struct {
	asks, comments, messages, documents map[string]struct{}
}

func newWanted() *wanted {
	return &wanted{
		asks: map[string]struct{}{}, comments: map[string]struct{}{},
		messages: map[string]struct{}{}, documents: map[string]struct{}{},
	}
}

func (w *wanted) followers(askID string) ([]string, error) {
	w.asks[askID] = struct{}{}
	return nil, nil
}

func (w *wanted) commentAuthor(commentID string) (model.Actor, bool, error) {
	w.comments[commentID] = struct{}{}
	return model.Actor{}, false, nil
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

// lookups is what a batch read for the ids its events asked for (loadLookups). failed holds each
// id that no read can answer - an id that is no uuid, which the column it is read from refuses, or
// an author whose JSON does not decode - keyed by its kind and id, with the error the event's
// destinations then report.
type lookups struct {
	asks      map[string][]string
	comments  map[string]commentAuthors
	messages  map[string]json.RawMessage
	documents map[string][]string
	failed    map[string]error
}

// commentAuthors is a comment's own author and, when its reply_to walk found one, its thread root's.
type commentAuthors struct {
	author json.RawMessage
	root   json.RawMessage
}

func (l *lookups) followers(askID string) ([]string, error) {
	if err := l.failed["ask:"+askID]; err != nil {
		return nil, err
	}
	return l.asks[askID], nil
}

func (l *lookups) commentAuthor(commentID string) (model.Actor, bool, error) {
	if err := l.failed["comment:"+commentID]; err != nil {
		return model.Actor{}, false, err
	}
	authors, ok := l.comments[commentID]
	if !ok {
		return model.Actor{}, false, nil
	}
	return decodeAuthor(authors.author, "comment", commentID)
}

func (l *lookups) rootCommentAuthor(commentID string) (model.Actor, bool, error) {
	if err := l.failed["comment:"+commentID]; err != nil {
		return model.Actor{}, false, err
	}
	authors, ok := l.comments[commentID]
	if !ok || authors.root == nil {
		return model.Actor{}, false, nil
	}
	return decodeAuthor(authors.root, "comment", commentID)
}

func (l *lookups) messageAuthor(messageID string) (model.Actor, bool, error) {
	if err := l.failed["message:"+messageID]; err != nil {
		return model.Actor{}, false, err
	}
	author, ok := l.messages[messageID]
	if !ok {
		return model.Actor{}, false, nil
	}
	return decodeAuthor(author, "message", messageID)
}

func (l *lookups) documentAuthors(artifactID string) ([]string, error) {
	if err := l.failed["document:"+artifactID]; err != nil {
		return nil, err
	}
	return l.documents[artifactID], nil
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
// at most four reads of followers and authors however many events it holds. Each read is the
// per-id read the outbox made before, over a set of ids: the same rows, in the same order for each
// id. An id that is no uuid is never sent to Postgres, which would refuse the whole statement over
// it; it is recorded failed, as the per-id read failed on it before.
func loadLookups(ctx context.Context, db queryer, want *wanted) (*lookups, error) {
	found := &lookups{
		asks: map[string][]string{}, comments: map[string]commentAuthors{},
		messages: map[string]json.RawMessage{}, documents: map[string][]string{}, failed: map[string]error{},
	}
	asks := validIDs(want.asks, "ask", "load followers of ask %q", found.failed)
	comments := validIDs(want.comments, "comment", "load comment author %q", found.failed)
	messages := validIDs(want.messages, "message", "load message author %q", found.failed)
	documents := validIDs(want.documents, "document", "load document %q session authors", found.failed)
	if err := loadFollowers(ctx, db, asks, found); err != nil {
		return nil, err
	}
	if err := loadCommentAuthors(ctx, db, comments, found); err != nil {
		return nil, err
	}
	if err := loadMessageAuthors(ctx, db, messages, found); err != nil {
		return nil, err
	}
	if err := loadDocumentSessionAuthors(ctx, db, documents, found); err != nil {
		return nil, err
	}
	return found, nil
}

// validIDs returns the ids of set that are uuids, sorted, and records each other one in failed
// under kind, with the error format names it by.
func validIDs(set map[string]struct{}, kind, format string, failed map[string]error) []string {
	ids := make([]string, 0, len(set))
	for _, id := range slices.Sorted(maps.Keys(set)) {
		if _, err := uuid.Parse(id); err != nil {
			failed[kind+":"+id] = fmt.Errorf(format+": not a uuid: %w", id, err)
			continue
		}
		ids = append(ids, id)
	}
	return ids
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

// loadCommentAuthors reads, for every comment in ids, its own author and its thread root's: the
// walk up its reply_to chain to the comment humans reply under. comments.reply_to has no
// acyclicity constraint, so the walk unions on the visited row and stops at
// maxCommentThreadDepth: a cycle then finds no reply_to-is-null row and reports no root rather
// than spinning.
func loadCommentAuthors(ctx context.Context, db queryer, ids []string, found *lookups) error {
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
		select start::text, depth = 0, reply_to is null, author
		from chain
		where depth = 0 or reply_to is null
		order by start, depth
	`, ids, maxCommentThreadDepth)
	if err != nil {
		return fmt.Errorf("load the batch's comment authors: %w", err)
	}
	var start string
	var own, root bool
	var author []byte
	_, err = pgx.ForEachRow(rows, []any{&start, &own, &root, &author}, func() error {
		authors := found.comments[start]
		if own {
			authors.author = json.RawMessage(slices.Clone(author))
		}
		if root && authors.root == nil {
			authors.root = json.RawMessage(slices.Clone(author))
		}
		found.comments[start] = authors
		return nil
	})
	if err != nil {
		return fmt.Errorf("load the batch's comment authors: %w", err)
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

// loadDocumentSessionAuthors lists, for every artifact in ids that is a document, the sessions it
// credits: the one that created it (artifacts.created_by) and every one a version lists
// (artifact_versions.authors), each once and in the order the document first credits it, its
// creator first. A person has no Envoy topic, so people are left out, and so is a session credit
// naming no id, which has none either. A version's authors are a JSON array, or JSON null where a
// writer encoded a nil list, which every Go reader of the column decodes as no authors; the query
// reads null the same way rather than failing the event's every publish on jsonb_array_elements'
// refusal of a scalar.
func loadDocumentSessionAuthors(ctx context.Context, db queryer, ids []string, found *lookups) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := db.Query(ctx, `
		with credited as (
			select a.id as document, a.created_by as author, 0 as number, 0::bigint as position
			from artifacts a
			where a.id = any($1::text[]::uuid[]) and a.kind = 'doc'
			union all
			select v.artifact_id, credit.author, v.number, credit.position
			from artifact_versions v
			join artifacts a on a.id = v.artifact_id and a.kind = 'doc'
			cross join lateral jsonb_array_elements(nullif(v.authors, 'null'::jsonb))
				with ordinality as credit(author, position)
			where v.artifact_id = any($1::text[]::uuid[])
		)
		select document::text, author->>'id'
		from credited
		where author->>'kind' = 'session' and coalesce(author->>'id', '') <> ''
		group by document, author->>'id'
		order by document, min(array[number, position])
	`, ids)
	if err != nil {
		return fmt.Errorf("load the batch's document session authors: %w", err)
	}
	var document, author string
	_, err = pgx.ForEachRow(rows, []any{&document, &author}, func() error {
		found.documents[document] = append(found.documents[document], author)
		return nil
	})
	if err != nil {
		return fmt.Errorf("load the batch's document session authors: %w", err)
	}
	return nil
}
