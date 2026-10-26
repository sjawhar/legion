package refs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// Source is the stored text of one reference source as its live write indexes it, read again so
// the index can be rebuilt from what is stored (RebuildAll) or re-indexed when it is owed
// (refs_owed, migration 0120; docs.Service.RunReferenceReindex).
type Source struct {
	Kind, ID string
	// Exists is false for a source that is gone, or that no write indexes (an issue-less message,
	// a document with no markdown version): it has no edges.
	Exists bool
	Body   string
	// WrittenAt is when the text was written: an ask's or comment's latest edit, else its
	// creation; a message's creation; a document version's creation.
	WrittenAt time.Time
	// Version is, for a document, the number of the version whose markdown Body is.
	Version int
}

// sourceQueries reads, by kind, the text each live write indexes (every refs.ReplaceCounted
// caller): a document's latest version's markdown, as the version write does (docs writeVersionTx,
// IndexDocumentReferences); an ask's question (the ask routes, approval requests, settlement's ask
// blocks, the restoration of a decided wording), never its answer; a comment's body (creation, edit,
// delivery reply); an issue message's body (creation, delivery reply), never a direct message's.
var sourceQueries = map[string]string{
	"artifact": `
		select markdown, created_at, number from artifact_versions
		where artifact_id = $1 and markdown is not null
		order by number desc limit 1`,
	"ask":     `select question, coalesce(edited_at, created_at), 0 from asks where id = $1`,
	"comment": `select body, coalesce(edited_at, created_at), 0 from comments where id = $1`,
	"message": `select body, created_at, 0 from messages where id = $1 and issue_key is not null`,
}

// LoadSource reads the stored text of the source (kind, id) through q, as its live write indexes
// it (sourceQueries). A source that does not exist, or whose id is not a uuid, is answered with
// Exists false.
func LoadSource(ctx context.Context, q Queryer, kind, id string) (Source, error) {
	query, known := sourceQueries[kind]
	if !known {
		return Source{}, fmt.Errorf("no reference source of kind %q", kind)
	}
	source := Source{Kind: kind, ID: id}
	if !isUUID(id) {
		return source, nil
	}
	err := q.QueryRow(ctx, query, id).Scan(&source.Body, &source.WrittenAt, &source.Version)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return source, nil
	case err != nil:
		return Source{}, fmt.Errorf("read %s %s: %w", kind, id, err)
	}
	source.Exists = true
	return source, nil
}

// Reindex reconciles source's edges with its stored text in tx, as its live write would by the
// current rules (replaceAt), resolving a bare key and a `#id` through resolver: the source's
// NewResolver, or, for a source of a scope the drain reads once, its NewScopedResolver. An edge it
// adds takes the source's own write time as created_at and carries no source_seq, since no event
// introduced it, and no event is appended. A source that no longer exists loses its edges. It
// leaves refs_owed to its caller, which claims the source's row before reading it (ClaimOwed,
// RebuildAll).
func Reindex(ctx context.Context, tx pgx.Tx, source Source, serverURL string, resolver text.Resolver) error {
	if !source.Exists {
		if _, err := tx.Exec(ctx, `delete from refs where from_kind = $1 and from_id = $2`, source.Kind, source.ID); err != nil {
			return fmt.Errorf("clear the references of %s %s: %w", source.Kind, source.ID, err)
		}
		return nil
	}
	if _, err := replaceAt(ctx, tx, source.Kind, source.ID, source.Body, serverURL, resolver, &source.WrittenAt); err != nil {
		return fmt.Errorf("re-index %s %s: %w", source.Kind, source.ID, err)
	}
	return nil
}

// Owed is one row of refs_owed: a source, or a document's scope, owed a re-index since enqueuedAt.
type Owed struct {
	Kind, ID   string
	EnqueuedAt time.Time
}

// OwedSources lists at most limit rows of refs_owed, oldest first. It locks nothing: each row is
// claimed in the transaction that re-indexes it (ClaimOwed, SettleScope).
func OwedSources(ctx context.Context, q Queryer, limit int) ([]Owed, error) {
	rows, err := q.Query(ctx, `
		select kind, id, enqueued_at from refs_owed order by enqueued_at, kind, id limit $1
	`, limit)
	if err != nil {
		return nil, fmt.Errorf("list owed re-indexes: %w", err)
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Owed])
}

// ClaimOwed deletes the refs_owed row of the source (kind, id) in tx, the first statement of the
// transaction that re-indexes it, and reports whether it did. A row another transaction holds -
// a write of that source, whose trigger locked it, or one deleting it - is skipped rather than
// waited for: that write is still landing, and the row stays owed for the next tick. Once the row
// is deleted, a later write of the source waits on it at its trigger before it touches the
// source's edges, so the text the claimer reads next is never older than a write the queue then
// forgets.
func ClaimOwed(ctx context.Context, tx pgx.Tx, kind, id string) (bool, error) {
	claimed, err := tx.Exec(ctx, `
		delete from refs_owed
		where (kind, id) in (select kind, id from refs_owed where kind = $1 and id = $2 for update skip locked)
	`, kind, id)
	if err != nil {
		return false, fmt.Errorf("claim the owed re-index of %s %s: %w", kind, id, err)
	}
	return claimed.RowsAffected() == 1, nil
}

// OweSource enqueues the source (kind, id) unless it is already owed, as one statement of its own:
// it may wait on a write landing on that row, so it runs holding nothing else.
func OweSource(ctx context.Context, q Execer, kind, id string) error {
	if _, err := q.Exec(ctx, `
		insert into refs_owed (kind, id) values ($1, $2) on conflict (kind, id) do nothing
	`, kind, id); err != nil {
		return fmt.Errorf("owe a re-index of %s %s: %w", kind, id, err)
	}
	return nil
}

// SettleScope deletes the scope row owed, once every source it names is owed itself (OweSource),
// and reports whether it did. A trigger re-dates a row it finds, so a row whose enqueued_at is no
// longer owed's - a question map that changed again after the scope was read - stays owed, as does
// one a writer holds.
func SettleScope(ctx context.Context, q Execer, owed Owed) (bool, error) {
	settled, err := q.Exec(ctx, `
		delete from refs_owed
		where (kind, id) in (
			select kind, id from refs_owed
			where kind = $1 and id = $2 and enqueued_at = $3
			for update skip locked
		)
	`, owed.Kind, owed.ID, owed.EnqueuedAt)
	if err != nil {
		return false, fmt.Errorf("settle the owed scope %s: %w", owed.ID, err)
	}
	return settled.RowsAffected() == 1, nil
}

// questionCandidate is the Postgres regular expression matching every text text.Mentions may read
// a `#id` in, and fewer others: a `#` and a block-id character, after the start of the text or any
// character text.Mentions lets a mention follow. It refuses only what Mentions never reads - a `#`
// after a letter, a digit, a `/` (`owner/repo#1`, `https://x/#q1`) or `{` (`:::ask{#q1}`, the
// declaration itself) - so a document whose only `#` declares its question blocks is not
// re-indexed for its own scope. Postgres reads `^` as the start of the text alone and lets `[^…]`
// match a newline, as Go's regexp does.
const questionCandidate = `(?:^|[^{/0-9A-Za-z])#[0-9A-Za-z_-]`

// citationQueries reads, kind by kind in ScopeSources' order, which of the sources $1 hold text that
// may cite a question (questionCandidate, $2), in id order: the text sourceQueries reads of each
// kind, so a source no write indexes (a document with no markdown version, an issue-less message)
// is never one.
var citationQueries = []struct{ kind, query string }{
	{"artifact", `
		select d.id::text from unnest($1::uuid[]) as d(id)
		cross join lateral (
			select markdown from artifact_versions
			where artifact_id = d.id and markdown is not null
			order by number desc limit 1
		) latest
		where latest.markdown ~ $2
		order by d.id`},
	{"ask", `select id::text from asks where id = any($1::uuid[]) and question ~ $2 order by id`},
	{"comment", `select id::text from comments where id = any($1::uuid[]) and body ~ $2 order by id`},
	{"message", `
		select id::text from messages
		where id = any($1::uuid[]) and issue_key is not null and body ~ $2
		order by id`},
}

// ScopeCitations is every source in the scope of the document doc (ScopeSources) whose stored text
// may cite a question by `#id` (questionCandidate), in ScopeSources' order: the sources whose
// references change when the document's question map does. The text is matched where it is
// stored, one query per kind, so a scope of thousands of comments sends none of their bodies.
func ScopeCitations(ctx context.Context, q Queryer, doc string) ([]Target, error) {
	sources, err := ScopeSources(ctx, q, doc)
	if err != nil {
		return nil, err
	}
	ids := map[string][]string{}
	for _, source := range sources {
		ids[source.Kind] = append(ids[source.Kind], source.ID)
	}
	citing := []Target{}
	for _, kind := range citationQueries {
		if len(ids[kind.kind]) == 0 {
			continue
		}
		rows, err := q.Query(ctx, kind.query, ids[kind.kind], questionCandidate)
		if err != nil {
			return nil, fmt.Errorf("read the %ss in the scope of document %s that may cite a question: %w", kind.kind, doc, err)
		}
		found, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return nil, fmt.Errorf("read the %ss in the scope of document %s that may cite a question: %w", kind.kind, doc, err)
		}
		for _, id := range found {
			citing = append(citing, Target{Kind: kind.kind, ID: id})
		}
	}
	return citing, nil
}
