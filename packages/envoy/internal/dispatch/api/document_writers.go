package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/docs"
	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// Every handler that writes a live document takes that document's writer gate
// (docs.Service.AcquireDocumentWriter) before it opens its transaction, and ends it once that
// transaction has committed and published: writers queued for a busy document then wait in
// memory holding no connection, instead of in Postgres and the writer slot each holding one. So
// the handler names the documents it will write with no transaction open - read through the pool,
// or known from its path - and, where what it read could have moved before its transaction locked
// it, names them again under its locks and compares (gatedWrite).
//
// The release runs right after the commit and the publish, before the handler's post-commit work
// - a related-item check, a comment's mention deliveries - so the next writer of the document
// waits on this write's transaction alone. Only work that writes the same
// documents keeps the gate past the commit: the close's final ask sweep (sweepClosedIssueAsks)
// runs under the gates its PATCH took, since joinLiveWrite refuses a document whose gate the
// context does not carry.

// gateDocuments takes the writer gates of artifactIDs for r's write and returns r carrying them,
// with their release. The caller releases them once its transaction has committed and published,
// and defers the release before its transaction's rollback for the paths that answer before the
// commit; the release is idempotent, so both may run.
func (s *server) gateDocuments(r *http.Request, artifactIDs ...string) (*http.Request, func(), error) {
	ctx, release, err := s.deps.Docs.AcquireDocumentWriter(r.Context(), artifactIDs...)
	if err != nil {
		return r, nil, err
	}
	return r.WithContext(ctx), release, nil
}

// gateComment takes, for r's write to comment id, the writer gate of the document the comment is
// anchored to (commentDocuments).
func (s *server) gateComment(r *http.Request, id string) (*http.Request, func(), error) {
	documents, err := s.commentDocuments(r.Context(), id)
	if err != nil {
		return r, nil, err
	}
	return s.gateDocuments(r, documents...)
}

// errDocumentsMoved is how a gated write's open reports that, under its locks, it writes
// documents other than the ones whose gates it took. It never reaches a client: gatedWrite redoes
// the pair once, then answers documentsMoved.
var errDocumentsMoved = errors.New("the documents the write changes are not the ones it gated")

// gatedWriteAttempts is how many times gatedWrite names and gates the documents before it gives
// up: the first, and one redo for documents that moved underneath it.
const gatedWriteAttempts = 2

// documentsMoved answers a write whose documents moved under it twice while it gated them.
var documentsMoved = errorf(http.StatusConflict, "DOCUMENT_CHANGED",
	"the documents this write changes changed while it waited to write them; send it again")

// gatedWrite takes the writer gates of the documents resolve names, with no transaction open, and
// runs open under them, with the context carrying them and the documents it gated. open begins the
// write's transaction, takes its locks, and names again, under them, the documents the write
// changes; when they are not the ones gated it rolls back and returns errDocumentsMoved, and
// gatedWrite releases the gates and runs the pair again - at most gatedWriteAttempts times, after
// which documentsMoved is the answer. On success the caller holds the gates through the returned
// context until it runs the returned release, once its transaction has committed and published or
// rolled back; on an error they are released already.
func gatedWrite[T any](
	ctx context.Context,
	documents docs.API,
	resolve func(context.Context) ([]string, error),
	open func(context.Context, []string) (T, error),
) (T, context.Context, func(), error) {
	var zero T
	for range gatedWriteAttempts {
		artifactIDs, err := resolve(ctx)
		if err != nil {
			return zero, nil, nil, err
		}
		artifactIDs = slices.Compact(slices.Sorted(slices.Values(artifactIDs)))
		gated, release, err := documents.AcquireDocumentWriter(ctx, artifactIDs...)
		if err != nil {
			return zero, nil, nil, err
		}
		result, err := open(gated, artifactIDs)
		if err == nil {
			return result, gated, release, nil
		}
		release()
		if !errors.Is(err, errDocumentsMoved) {
			return zero, nil, nil, err
		}
	}
	return zero, nil, nil, documentsMoved
}

// sameDocuments reports whether named, which a write found under its locks, are the documents
// gated, which gatedWrite gated: named may repeat a document and comes in any order.
func sameDocuments(gated, named []string) bool {
	return slices.Equal(gated, slices.Compact(slices.Sorted(slices.Values(named))))
}

// documentQueryer is the pool for a read before the transaction opens, nil when the server has no
// database (begin then answers DATABASE_UNAVAILABLE).
func (s *server) documentQueryer() queryer {
	if s.deps.Store == nil || s.deps.Store.Pool == nil {
		return nil
	}
	return s.deps.Store.Pool
}

// optionalDocument is the one document id row reads, or none for no row or a null one.
func optionalDocument(row pgx.Row) ([]string, error) {
	var artifactID *string
	if err := row.Scan(&artifactID); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if artifactID == nil || *artifactID == "" {
		return nil, nil
	}
	return []string{*artifactID}, nil
}

// commentDocuments is the document comment id is anchored to, whose marks projection its
// mutations rewrite, or none for an unanchored comment or an id no comment has (the write answers
// that under its lock). A comment's anchor never moves to another document.
func (s *server) commentDocuments(ctx context.Context, id string) ([]string, error) {
	q := s.documentQueryer()
	if q == nil || uuid.Validate(id) != nil {
		return nil, nil
	}
	return optionalDocument(q.QueryRow(ctx, `select anchor->>'artifact_id' from comments where id = $1`, id))
}

// askDocuments is the document a block ask's block lives in, which its edits and transitions
// write, or none for an ask with no block or an id no ask has.
func (s *server) askDocuments(ctx context.Context, id string) ([]string, error) {
	q := s.documentQueryer()
	if q == nil || uuid.Validate(id) != nil {
		return nil, nil
	}
	return optionalDocument(q.QueryRow(ctx,
		`select block_artifact_id::text from asks where id = $1 and block_id is not null`, id))
}

// anchorDocuments is the document input anchors to on owner, which an anchored comment or ask
// marks, or none for no anchor or a reference to no document of owner (resolveAnchor answers it).
func anchorDocuments(ctx context.Context, q queryer, owner owner, input *model.AnchorInput) ([]string, error) {
	if q == nil || input == nil {
		return nil, nil
	}
	if owner.IssueKey == nil {
		if owner.ArtifactID == nil {
			return nil, nil
		}
		return []string{*owner.ArtifactID}, nil
	}
	artifactRef := strings.TrimSpace(input.Artifact)
	if artifactRef == "" {
		return nil, nil
	}
	return optionalDocument(q.QueryRow(ctx, `
		select id::text from artifacts
		where issue_key = $1 and (id::text = $2 or slug = $2) and kind = 'doc'
	`, *owner.IssueKey, artifactRef))
}

// replyRootDocuments is the document the root of the thread replyTo hangs in is anchored to: a
// reply rewrites its root's marks projection there. None for no reply, a thread under an ask, or
// an unanchored root. The walk stops at parentDepthCap, as threadHeadOf's does.
func replyRootDocuments(ctx context.Context, q queryer, replyTo *string) ([]string, error) {
	if q == nil || replyTo == nil || uuid.Validate(*replyTo) != nil {
		return nil, nil
	}
	return optionalDocument(q.QueryRow(ctx, `
		with recursive thread (id, reply_to, ask_id, anchor, depth) as (
			select id, reply_to, ask_id, anchor, 1 from comments where id = $1
			union all
			select c.id, c.reply_to, c.ask_id, c.anchor, thread.depth + 1
			from comments c join thread on c.id = thread.reply_to
			where thread.ask_id is null and thread.depth < $2
		)
		select anchor->>'artifact_id' from thread where reply_to is null and ask_id is null
	`, *replyTo, parentDepthCap))
}

// newCommentDocuments is every document a new comment writes: the one its anchor marks, and the
// one its reply's thread root is anchored to.
func newCommentDocuments(ctx context.Context, q queryer, owner owner, input commentInput) ([]string, error) {
	anchored, err := anchorDocuments(ctx, q, owner, input.Anchor)
	if err != nil {
		return nil, err
	}
	root, err := replyRootDocuments(ctx, q, input.ReplyTo)
	if err != nil {
		return nil, err
	}
	return append(anchored, root...), nil
}

// issueDocuments is every document of issue key, in id order: the documents its close and its
// reopen write (withdrawIssueAsksTx, restoreIssueAsksTx, sweepClosedIssueAsks), and the ask repair
// with them (repairIssueAsksOf). Read in a transaction holding the issue's row, it cannot move
// before that transaction ends: a document is created only under its issue's row.
func issueDocuments(ctx context.Context, q queryer, key string) ([]string, error) {
	if q == nil {
		return nil, nil
	}
	rows, err := q.Query(ctx, `select id::text from artifacts where issue_key = $1 and kind = 'doc' order by id`, key)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// issueStatusDocuments is, read with no transaction open, every document a PATCH moving issue
// key's status writes: the issue's documents when the move closes the open issue (closes) or
// reopens the closed one, none otherwise. The PATCH decides the move again under the issue's row,
// and names the documents again there.
func (s *server) issueStatusDocuments(ctx context.Context, key string, closes bool) ([]string, error) {
	q := s.documentQueryer()
	if q == nil {
		return nil, nil
	}
	var closed bool
	if err := q.QueryRow(ctx, `select closed_at is not null from issues where key = $1`, key).Scan(&closed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if closes == closed {
		return nil, nil
	}
	return issueDocuments(ctx, q, key)
}
