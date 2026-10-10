package refs

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// BlockExcerptRunes bounds the containing block a document mention's excerpt shows.
const BlockExcerptRunes = 480

// Execer is the statement surface shared by a pool and a transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// MentionExcerpts is, for each target markdown mentions, the block holding its first mention that
// lies inside a block - blocks index markdown by byte, in document order - as the references route
// shows it: the block's id and its markdown cut to BlockExcerptRunes. A target none of whose
// mentions lies inside a block has none.
func MentionExcerpts(markdown string, blocks []model.ArtifactBlock, serverURL string) map[Target]model.GraphExcerpt {
	excerpts := make(map[Target]model.GraphExcerpt)
	for _, located := range text.ExtractAt(markdown, serverURL) {
		if located.Kind == "url" {
			continue
		}
		target := Target{Kind: located.Kind, ID: ToID(located.Ref)}
		if _, found := excerpts[target]; found {
			continue
		}
		for _, block := range blocks {
			if block.From <= located.Offset && located.Offset < block.To {
				excerpts[target] = model.GraphExcerpt{
					BlockID: block.ID,
					Text:    text.HeadRunes(markdown[block.From:block.To], BlockExcerptRunes),
				}
				break
			}
		}
	}
	return excerpts
}

// RecordExcerpts stores, on every mention the document fromID writes, the excerpt excerpts holds
// for its target, or none, as of the document's version: the excerpt GET /api/v1/references
// answers that mention with. A row already holding a later version's excerpt is left as it is, so
// the reconciliation, which reads the document outside the version write's transaction, never
// overwrites what a later version stored.
func RecordExcerpts(ctx context.Context, q Execer, fromID string, version int, excerpts map[Target]model.GraphExcerpt) error {
	kinds := make([]string, 0, len(excerpts))
	ids := make([]string, 0, len(excerpts))
	blockIDs := make([]string, 0, len(excerpts))
	texts := make([]string, 0, len(excerpts))
	for target, excerpt := range excerpts {
		kinds = append(kinds, target.Kind)
		ids = append(ids, target.ID)
		blockIDs = append(blockIDs, excerpt.BlockID)
		texts = append(texts, excerpt.Text)
	}
	if _, err := q.Exec(ctx, `
		update refs r
		set excerpt_version = $2, excerpt_block_id = e.block_id, excerpt_text = e.text
		from refs s
		left join unnest($3::text[], $4::text[], $5::text[], $6::text[]) as e(kind, id, block_id, text)
		  on e.kind = s.to_kind and e.id = s.to_id
		where r.from_kind = 'artifact' and r.from_id = $1
		  and s.from_kind = r.from_kind and s.from_id = r.from_id
		  and s.to_kind = r.to_kind and s.to_id = r.to_id
		  and coalesce(r.excerpt_version, 0) <= $2
	`, fromID, version, kinds, ids, blockIDs, texts); err != nil {
		return fmt.Errorf("record reference excerpts of %s: %w", fromID, err)
	}
	return nil
}

// StoredExcerpts is, by citing document, the excerpt stored on each mention of (toKind, toID) a
// document writes; a mention with none stored is absent. It reads refs through refs_to alone.
func StoredExcerpts(ctx context.Context, q Queryer, toKind, toID string) (map[string]model.GraphExcerpt, error) {
	rows, err := q.Query(ctx, `
		select from_id, coalesce(excerpt_block_id, ''), excerpt_text from refs
		where to_kind = $1 and to_id = $2 and from_kind = 'artifact' and excerpt_text is not null
	`, toKind, toID)
	if err != nil {
		return nil, fmt.Errorf("read reference excerpts: %w", err)
	}
	defer rows.Close()
	excerpts := make(map[string]model.GraphExcerpt)
	for rows.Next() {
		var fromID string
		var excerpt model.GraphExcerpt
		if err := rows.Scan(&fromID, &excerpt.BlockID, &excerpt.Text); err != nil {
			return nil, fmt.Errorf("scan reference excerpt: %w", err)
		}
		excerpts[fromID] = excerpt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read reference excerpts: %w", err)
	}
	return excerpts, nil
}

// ExcerptDrift is a document some of whose mentions are owed an excerpt, with its latest version's
// number when it was listed.
type ExcerptDrift struct {
	FromID  string
	Version int
}

// driftedExcerptsQuery lists, in id order, the documents some of whose mentions hold no excerpt
// version (excerpt_version is null), through the partial index refs_excerpt_drift, which holds
// exactly those rows: once the reconciliation has caught up, the index is empty and the query reads
// nothing else.
const driftedExcerptsQuery = `
	select d.from_id,
	       (select coalesce(max(v.number), 0) from artifact_versions v where v.artifact_id = d.from_id::uuid)
	from (
		select distinct from_id from refs
		where from_kind = 'artifact' and excerpt_version is null
		order by from_id
		limit $1
	) d
	order by d.from_id`

// DriftedExcerpts lists, in id order and at most limit, the documents some of whose mentions are
// owed an excerpt: every mention indexed before migration 0090, each one rebuild-refs inserts, one
// a version write could not cut (ClearExcerpts), and one MarkStaleExcerpts found behind.
func DriftedExcerpts(ctx context.Context, q Queryer, limit int) ([]ExcerptDrift, error) {
	rows, err := q.Query(ctx, driftedExcerptsQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("list drifted reference excerpts: %w", err)
	}
	defer rows.Close()
	drifted := []ExcerptDrift{}
	for rows.Next() {
		var document ExcerptDrift
		if err := rows.Scan(&document.FromID, &document.Version); err != nil {
			return nil, fmt.Errorf("scan drifted reference excerpt: %w", err)
		}
		drifted = append(drifted, document)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list drifted reference excerpts: %w", err)
	}
	return drifted, nil
}

// ClearExcerpts marks every mention the document fromID writes as owed an excerpt, keeping the
// excerpt each holds for the route to serve until the reconciliation stores the latest: what a
// version write that cannot cut its excerpts leaves.
func ClearExcerpts(ctx context.Context, q Execer, fromID string) error {
	if _, err := q.Exec(ctx, `
		update refs set excerpt_version = null where from_kind = 'artifact' and from_id = $1
	`, fromID); err != nil {
		return fmt.Errorf("mark the reference excerpts of %s owed: %w", fromID, err)
	}
	return nil
}

// MarkStaleExcerpts marks owed every mention whose excerpt is older than its document's latest
// version, and returns how many it marked. Every version write stores its own mentions' excerpts,
// so only a version a binary predating migration 0090 wrote - the task a deploy replaces, while
// both run - leaves one behind; it reads every stored excerpt, so the reconciliation runs it only
// in the hour after its process starts.
func MarkStaleExcerpts(ctx context.Context, q Execer) (int64, error) {
	tag, err := q.Exec(ctx, `
		update refs r set excerpt_version = null
		where r.from_kind = 'artifact' and r.excerpt_version is not null
		  and r.excerpt_version < (
		    select coalesce(max(v.number), 0) from artifact_versions v where v.artifact_id = r.from_id::uuid
		  )
	`)
	if err != nil {
		return 0, fmt.Errorf("mark stale reference excerpts: %w", err)
	}
	return tag.RowsAffected(), nil
}
