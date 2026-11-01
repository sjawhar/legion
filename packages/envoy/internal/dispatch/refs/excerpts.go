package refs

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/sjawhar/envoy/internal/dispatch/model"
	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/text"
)

// BlockExcerptRunes bounds the containing block a document mention's excerpt shows.
const BlockExcerptRunes = 480

// Execer is the statement surface shared by a pool and a transaction.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// MentionExcerpts is, for each target the mentions located in markdown name, the innermost of
// blocks holding its first mention that lies inside a block - the block with the tightest byte
// range - as the references route shows it: the block's id and its markdown cut to
// BlockExcerptRunes. A target none of whose mentions lies inside a block has none. The block is
// any of blocks, not only a graph node: PlanBlocks makes no node of a block whose id is past
// text.MaxBlockIDBytes, empty or unstorable, nor of a later holder of a repeated id
// (uniqueBlocks), and writes the mention from the innermost node holding it, if any. Such a block
// still gives the excerpt, so its excerpt_block_id can name a block the graph does not hold.
// located is text.ExtractAt(markdown, …), the one extraction a version's index reads.
func MentionExcerpts(markdown string, blocks []model.ArtifactBlock, located []text.Located) map[Target]model.GraphExcerpt {
	starts := make([]int, len(located))
	for i, mention := range located {
		starts[i] = mention.Offset
	}
	holders := innermostBlocks(blocks, starts)
	excerpts := make(map[Target]model.GraphExcerpt)
	for i, mention := range located {
		if holders[i] < 0 {
			continue
		}
		target, ok := TargetOf(mention.Ref)
		if !ok {
			continue
		}
		if _, found := excerpts[target]; found {
			continue
		}
		block := blocks[holders[i]]
		excerpts[target] = model.GraphExcerpt{
			BlockID: block.ID,
			Text:    text.HeadRunes(markdown[block.From:block.To], BlockExcerptRunes),
		}
	}
	return excerpts
}

// RecordExcerpts stores, on every mention the document fromID writes, the excerpt excerpts holds
// for its target, or none, as of the document's version: the excerpt GET /api/v1/references
// answers that mention with. A row already holding a later version's excerpt is left as it is, so
// the reconciliation, which reads the document outside the version write's transaction, never
// overwrites what a later version stored.
//
// The guard admits a row a version write has just marked owed (ClearExcerpts, excerpt_version
// null) while the reconciliation, holding an older version's number, read the document, so the
// row can take that older number. The invariant that keeps the excerpt right: a version write
// marks its mentions owed only when its transaction recorded no rendering of the tree it wrote,
// which is an upload whose replacement left the document as it was (ReplaceText returns before
// applyLive records one). Every other version write cuts its excerpts from the render its
// markdown came from (docs' indexDocument, which indexes the version's references, excerpts and
// blocks from one extraction), so IndexDocumentReferences' check that the recorded rendering is
// the version's markdown never clears. That unchanged upload's markdown is the
// document the reconciliation read, so the excerpt stored under the older number is the one the
// version would have stored. StoredExcerpts hides an owed row, and the reconciliation's non-null
// version makes that same excerpt current again.
//
// It returns the targets whose excerpt the route answers differently after the write, in kind and
// id order: the route serves a row's stored block and text only once its version is set and its
// text is not null (StoredExcerpts), and the document's name otherwise, so a row moves when what it
// served before differs from what it serves now. A version write names them on its event; one
// that writes in a write phase works them out first (ExcerptsMoved) and queues the statement alone
// (QueueRecordExcerpts).
func RecordExcerpts(ctx context.Context, q Queryer, fromID string, version int, excerpts map[Target]model.GraphExcerpt) ([]Target, error) {
	kinds, ids, blockIDs, texts := excerptColumns(excerpts)
	// The target's excerpt is one row-valued subquery, so a mention whose target excerpts lacks
	// (no block holds it) is still updated, with no excerpt, and leaves the drift index. Every
	// part of the statement reads the one snapshot, so prior holds each row as it was.
	moved, err := scanTargets(q.Query(ctx, `
		with e(kind, id, block_id, text) as (select * from unnest($3::text[], $4::text[], $5::text[], $6::text[])),
		prior as (
			select to_kind, to_id,
			       case when excerpt_version is not null then excerpt_text end as served_text,
			       case when excerpt_version is not null and excerpt_text is not null then coalesce(excerpt_block_id, '') end as served_block
			from refs
			where from_kind = 'artifact' and from_id = $1 and coalesce(excerpt_version, 0) <= $2
		),
		written as (
			update refs r
			set excerpt_version = $2,
			    (excerpt_block_id, excerpt_text) = (select e.block_id, e.text from e where e.kind = r.to_kind and e.id = r.to_id)
			where r.from_kind = 'artifact' and r.from_id = $1
			  and coalesce(r.excerpt_version, 0) <= $2
			returning r.to_kind, r.to_id, r.excerpt_text as served_text,
			          case when r.excerpt_text is not null then coalesce(r.excerpt_block_id, '') end as served_block
		)
		select w.to_kind, w.to_id from written w join prior p on p.to_kind = w.to_kind and p.to_id = w.to_id
		where (p.served_block, p.served_text) is distinct from (w.served_block, w.served_text)
		order by w.to_kind, w.to_id
	`, fromID, version, kinds, ids, blockIDs, texts))
	if err != nil {
		return nil, fmt.Errorf("record reference excerpts of %s: %w", fromID, err)
	}
	return moved, nil
}

// QueueRecordExcerpts queues RecordExcerpts' update on a write phase's batch, without the targets
// it moved, which the writer worked out in its read phase (ExcerptsMoved).
func QueueRecordExcerpts(b *store.WriteBatch, fromID string, version int, excerpts map[Target]model.GraphExcerpt) {
	kinds, ids, blockIDs, texts := excerptColumns(excerpts)
	b.Queue("record reference excerpts of "+fromID, `
		with e(kind, id, block_id, text) as (select * from unnest($3::text[], $4::text[], $5::text[], $6::text[]))
		update refs r
		set excerpt_version = $2,
		    (excerpt_block_id, excerpt_text) = (select e.block_id, e.text from e where e.kind = r.to_kind and e.id = r.to_id)
		where r.from_kind = 'artifact' and r.from_id = $1
		  and coalesce(r.excerpt_version, 0) <= $2
	`, fromID, version, kinds, ids, blockIDs, texts)
}

// ExcerptsMoved is what RecordExcerpts would return for the document fromID's version, worked out
// in a writer's read phase for the update it queues in its write phase right after its
// reconciliation of the document's references: held is the targets the document cites once that
// reconciliation is written (Replacement.Held), so a new edge, which holds no excerpt yet, is
// judged as RecordExcerpts would judge it. It reads the excerpts q holds on the document's edges;
// nothing in the transaction writes them before the update.
func ExcerptsMoved(ctx context.Context, q Queryer, fromID string, version int, held []Target, excerpts map[Target]model.GraphExcerpt) ([]Target, error) {
	type storedExcerpt struct {
		version     *int
		block, text *string
	}
	rows, err := q.Query(ctx, `
		select to_kind, to_id, excerpt_version, excerpt_block_id, excerpt_text
		from refs
		where from_kind = 'artifact' and from_id = $1
	`, fromID)
	if err != nil {
		return nil, fmt.Errorf("read the reference excerpts of %s: %w", fromID, err)
	}
	defer rows.Close()
	stored := make(map[Target]storedExcerpt)
	for rows.Next() {
		var target Target
		var row storedExcerpt
		if err := rows.Scan(&target.Kind, &target.ID, &row.version, &row.block, &row.text); err != nil {
			return nil, fmt.Errorf("scan a reference excerpt of %s: %w", fromID, err)
		}
		stored[target] = row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the reference excerpts of %s: %w", fromID, err)
	}
	moved := []Target{}
	seen := make(map[Target]struct{}, len(held))
	for _, target := range held {
		if _, dup := seen[target]; dup {
			continue
		}
		seen[target] = struct{}{}
		row := stored[target]
		if row.version != nil && *row.version > version {
			continue
		}
		// What the route serves: a stored block and text once the version is set and the text is
		// not null, the document's name otherwise.
		servedBefore := row.version != nil && row.text != nil
		var blockBefore, textBefore string
		if servedBefore {
			textBefore = *row.text
			if row.block != nil {
				blockBefore = *row.block
			}
		}
		after, servedAfter := excerpts[target]
		if servedBefore != servedAfter || (servedAfter && (blockBefore != after.BlockID || textBefore != after.Text)) {
			moved = append(moved, target)
		}
	}
	slices.SortFunc(moved, func(a, b Target) int {
		return cmp.Or(strings.Compare(a.Kind, b.Kind), strings.Compare(a.ID, b.ID))
	})
	return moved, nil
}

// excerptColumns is excerpts as the unnested columns RecordExcerpts' statement reads.
func excerptColumns(excerpts map[Target]model.GraphExcerpt) (kinds, ids, blockIDs, texts []string) {
	kinds = make([]string, 0, len(excerpts))
	ids = make([]string, 0, len(excerpts))
	blockIDs = make([]string, 0, len(excerpts))
	texts = make([]string, 0, len(excerpts))
	for target, excerpt := range excerpts {
		kinds = append(kinds, target.Kind)
		ids = append(ids, target.ID)
		blockIDs = append(blockIDs, excerpt.BlockID)
		texts = append(texts, excerpt.Text)
	}
	return kinds, ids, blockIDs, texts
}

// StoredExcerpts is, by citing document, the excerpt stored on each mention of (toKind, toID) a
// document writes; a mention with none stored, or one the reconciliation owes (excerpt_version
// null), is absent. It cannot tell a set excerpt_version that is behind its document's latest
// version: a version a binary predating migration 0092 writes leaves the previous version's
// excerpt set, and that excerpt is served until MarkStaleExcerpts marks it owed, which the
// reconciliation does once, in the first pass each process runs. Every build since 0092 records
// its excerpts in each version write, so no build a deploy replaces leaves one now.
// It reads refs through refs_to alone.
func StoredExcerpts(ctx context.Context, q Queryer, toKind, toID string) (map[string]model.GraphExcerpt, error) {
	rows, err := q.Query(ctx, `
		select from_id, coalesce(excerpt_block_id, ''), excerpt_text from refs
		where to_kind = $1 and to_id = $2 and from_kind = 'artifact'
		  and excerpt_text is not null and excerpt_version is not null
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
// number when it was listed, and whether a backoff row (reference_excerpt_backoff) stands for it,
// of that version or an older one: the row the reconciliation deletes once it records the
// document.
type ExcerptDrift struct {
	FromID    string
	Version   int
	BackedOff bool
}

// driftedExcerptsQuery lists, in id order, the documents some of whose mentions hold no excerpt
// version (excerpt_version is null), through the partial index refs_excerpt_drift, which holds
// exactly those rows: once the reconciliation has caught up, the index is empty and the query reads
// nothing else. A document whose backoff row names its latest version and is not yet due is left
// out, so the limit is spent on documents a batch can still record; a row naming an older version
// is a failure of a version the document has since replaced, and holds nothing back. The
// exclusion is an anti-join inside the limited distinct, so the plan stays the ordered walk of
// refs_excerpt_drift that stops after limit documents, whatever the visibility map says; the
// backoff table holds only failing documents and is read whole once.
const driftedExcerptsQuery = `
	select d.from_id,
	       (select coalesce(max(v.number), 0) from artifact_versions v where v.artifact_id = d.from_id::uuid),
	       exists (select 1 from reference_excerpt_backoff b where b.artifact_id = d.from_id::uuid)
	from (
		select distinct r.from_id from refs r
		where r.from_kind = 'artifact' and r.excerpt_version is null
		  and not exists (
			select 1 from reference_excerpt_backoff b
			where b.artifact_id = r.from_id::uuid and b.next_attempt_at > now()
			  and b.version = (select coalesce(max(v.number), 0) from artifact_versions v where v.artifact_id = b.artifact_id)
		  )
		order by r.from_id
		limit $1
	) d
	order by d.from_id`

// DriftedExcerpts lists, in id order and at most limit, the documents some of whose mentions are
// owed an excerpt and that are not waiting out a backoff (DeferExcerpts): every mention indexed
// before migration 0092, each one rebuild-refs inserts, one a version write could not cut
// (ClearExcerpts), and one MarkStaleExcerpts found behind.
func DriftedExcerpts(ctx context.Context, q Queryer, limit int) ([]ExcerptDrift, error) {
	rows, err := q.Query(ctx, driftedExcerptsQuery, limit)
	if err != nil {
		return nil, fmt.Errorf("list drifted reference excerpts: %w", err)
	}
	defer rows.Close()
	drifted := []ExcerptDrift{}
	for rows.Next() {
		var document ExcerptDrift
		if err := rows.Scan(&document.FromID, &document.Version, &document.BackedOff); err != nil {
			return nil, fmt.Errorf("scan drifted reference excerpt: %w", err)
		}
		drifted = append(drifted, document)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list drifted reference excerpts: %w", err)
	}
	return drifted, nil
}

// DeferExcerpts records that the reconciliation could not read or record the document fromID at
// version, so DriftedExcerpts leaves it out until its next attempt: base after its first failure
// at that version, doubling with each failure after, never more than ceiling, plus up to a fifth
// again of random jitter so documents that failed together are not retried together. A row of an
// older version is replaced and counts from one again: the document has a version that has not
// failed yet. The exponent is held at 30, past which every delay a caller passes is the ceiling,
// so the doubling never leaves double precision.
func DeferExcerpts(ctx context.Context, q Execer, fromID string, version int, base, ceiling time.Duration) error {
	if _, err := q.Exec(ctx, `
		insert into reference_excerpt_backoff as b (artifact_id, version, attempts, next_attempt_at)
		values ($1::uuid, $2, 1, now() + make_interval(secs => least($3::float8, $4::float8) * (1 + 0.2 * random())))
		on conflict (artifact_id) do update set
			version = excluded.version,
			attempts = case when b.version = excluded.version then b.attempts + 1 else 1 end,
			next_attempt_at = now() + make_interval(secs => least(
				$3::float8 * power(2, case when b.version = excluded.version then least(b.attempts, 30) else 0 end),
				$4::float8
			) * (1 + 0.2 * random()))
	`, fromID, version, base.Seconds(), ceiling.Seconds()); err != nil {
		return fmt.Errorf("defer the reference excerpts of %s: %w", fromID, err)
	}
	return nil
}

// ClearExcerptBackoff deletes the backoff row of the document fromID, once the reconciliation has
// recorded its excerpts.
func ClearExcerptBackoff(ctx context.Context, q Execer, fromID string) error {
	if _, err := q.Exec(ctx, `delete from reference_excerpt_backoff where artifact_id = $1::uuid`, fromID); err != nil {
		return fmt.Errorf("clear the reference excerpt backoff of %s: %w", fromID, err)
	}
	return nil
}

// NextExcerptRetry is when the earliest document the reconciliation deferred (DeferExcerpts) and
// still owes falls due, and the zero time when none does: the one timer the reconciliation holds
// while it waits for no write. A backoff row of an older version than the document's latest does
// not count: that document is listed now, and the batch that just ran either took it or was full.
// It first deletes the rows of documents that owe nothing any more - a later version write cut
// their excerpts, or the document is gone - which no success of the reconciliation's own would
// reach; the table holds only documents that failed, so both statements read a few rows.
func NextExcerptRetry(ctx context.Context, q Queryer) (time.Time, error) {
	var next *time.Time
	if err := q.QueryRow(ctx, `
		with settled as (
			delete from reference_excerpt_backoff b
			where not exists (
				select 1 from refs r
				where r.from_kind = 'artifact' and r.excerpt_version is null and r.from_id = b.artifact_id::text
			)
		)
		select min(b.next_attempt_at) from reference_excerpt_backoff b
		where b.version = (select coalesce(max(v.number), 0) from artifact_versions v where v.artifact_id = b.artifact_id)
		  and exists (
			select 1 from refs r
			where r.from_kind = 'artifact' and r.excerpt_version is null and r.from_id = b.artifact_id::text
		  )
	`).Scan(&next); err != nil {
		return time.Time{}, fmt.Errorf("read the next reference excerpt retry: %w", err)
	}
	if next == nil {
		return time.Time{}, nil
	}
	return *next, nil
}

// ClearExcerpts queues, on a write phase's batch, the mark of every mention the document fromID
// writes as owed an excerpt, keeping the payload each holds for the reconciliation to overwrite.
// StoredExcerpts does not serve it while excerpt_version is null, so the route uses the
// document's name until the reconciliation stores the latest excerpt: what a version write that
// cannot cut its excerpts leaves. Each row it marks announces itself on dispatch_excerpts_owed at
// commit (refs_excerpts_owed_notify, migration 0153), which wakes the reconciliation in every
// process.
func ClearExcerpts(b *store.WriteBatch, fromID string) {
	b.Queue("mark the reference excerpts of "+fromID+" owed", `
		update refs set excerpt_version = null where from_kind = 'artifact' and from_id = $1
	`, fromID)
}

// MarkStaleExcerpts marks owed every mention whose excerpt is older than its document's latest
// version, and returns how many it marked. Every version write stores its own mentions' excerpts,
// so only a version a binary predating migration 0092 wrote leaves one behind, and StoredExcerpts
// serves that older excerpt until this marks it. No index supports it: it reads every artifact
// mention holding an excerpt (7,537 rows on the 2026-10-10 production dump, 26 ms), so the
// reconciliation runs it once per process, in its first pass.
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
